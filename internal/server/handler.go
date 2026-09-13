package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"prerender/internal/cache"
	"prerender/internal/limit"
	"prerender/internal/render"
	"prerender/internal/urlutil"
)

// handlePrerender implements the whole request lifecycle for
//
//	GET /https://www.sbisecurities.in/about-us?page=2
func (s *Server) handlePrerender(w http.ResponseWriter, r *http.Request) {
	lg := loggerFrom(r.Context())

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		s.fail(w, r, http.StatusMethodNotAllowed, "method not allowed",
			fmt.Errorf("method %s not supported", r.Method))
		return
	}

	// The raw request URI is used on purpose. http.Request.URL.Path has already
	// collapsed "//" into "/", which would turn the target into "https:/host".
	rawTarget, err := urlutil.Extract(r.RequestURI)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid target URL", err)
		return
	}

	normalized, err := urlutil.Normalize(rawTarget)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid target URL",
			fmt.Errorf("normalize %q: %w", rawTarget, err))
		return
	}

	lg = lg.With("url", normalized)
	r = r.WithContext(withLogger(r.Context(), lg))

	host := urlutil.Host(normalized)
	if !s.cfg.AllowedHosts[host] {
		s.fail(w, r, http.StatusForbidden, "host not allowed",
			fmt.Errorf("host %q is not in ALLOWED_HOSTS", host))
		return
	}

	if urlutil.MatchesAny(s.cfg.BlockedPathPatterns, normalized) {
		s.fail(w, r, http.StatusForbidden, "path blocked",
			fmt.Errorf("url matches BLOCKED_PATH_PATTERNS"))
		return
	}

	key := s.cache.Key(normalized)

	// --- cache lookup -------------------------------------------------------
	if s.cache.Enabled() {
		html, err := s.cache.Get(r.Context(), normalized)
		switch {
		case err == nil:
			lg.Info("cache.lookup", "result", "hit", "key", key, "bytes", len(html))
			s.writeHTML(w, r, http.StatusOK, html, "HIT", "", 0)
			return
		case errors.Is(err, cache.ErrMiss):
			lg.Info("cache.lookup", "result", "miss", "key", key)
		default:
			// Redis being down degrades the service to "slow but correct".
			// It must never turn into a failed request.
			lg.Error("cache.read_failed", "key", key, "error", err.Error(),
				"action", "treating as miss")
		}
	}

	// --- render (coalesced) -------------------------------------------------
	val, shared, err := s.group.Do(key, func() (any, error) {
		return s.renderAndStore(r, normalized, lg)
	})
	if err != nil {
		s.failRender(w, r, normalized, err)
		return
	}

	res, ok := val.(*render.Result)
	if !ok || res == nil {
		s.fail(w, r, http.StatusInternalServerError, "render returned no result",
			fmt.Errorf("coalesced call for %s returned %T", normalized, val))
		return
	}

	cacheState := "MISS"
	if shared {
		cacheState = "COALESCED"
	}
	s.writeHTML(w, r, res.Status, res.HTML, cacheState, res.Tier, res.Duration)
}

// renderAndStore acquires a render slot, renders, and writes the result to
// Redis. It runs exactly once per URL even under a burst of identical requests.
func (s *Server) renderAndStore(r *http.Request, normalized string, lg *slog.Logger) (*render.Result, error) {
	queueStart := time.Now()

	// The queue wait honours the client's context: if the crawler hangs up
	// while queued, the slot is released for someone who is still waiting.
	release, err := s.sem.Acquire(r.Context(), s.cfg.QueueWaitTimeout)
	if err != nil {
		return nil, err
	}
	defer release()

	queueWait := time.Since(queueStart)
	lg.Info("render.start",
		"queue_wait_ms", queueWait.Milliseconds(),
		"in_flight", s.sem.InFlight(),
		"waiting", s.sem.Waiting())

	// The render itself is rooted on the application context, not the request
	// context. Once a render has started it should finish and populate the
	// cache even if the client disconnects, so that queued waiters and the next
	// crawler both benefit.
	res, err := s.renderer.Render(s.appCtx, normalized, lg)
	if err != nil {
		lg.Error("render.failed",
			"error", err.Error(),
			"queue_wait_ms", queueWait.Milliseconds(),
			"elapsed_ms", time.Since(queueStart).Milliseconds())
		return nil, err
	}

	lg.Info("render.end",
		"duration_ms", res.Duration.Milliseconds(),
		"html_bytes", res.Bytes,
		"upstream_status", res.Status,
		"ready_tier", res.Tier,
		"degraded", res.Degraded,
		"js_errors", res.PageErrs,
		"failed_requests", res.FailedReq)

	// --- store --------------------------------------------------------------
	if !s.cache.Enabled() {
		return res, nil
	}
	if !res.Cacheable(s.cfg.MinHTMLBytes) {
		lg.Warn("cache.skipped",
			"reason", cacheSkipReason(res, s.cfg.MinHTMLBytes),
			"upstream_status", res.Status,
			"html_bytes", res.Bytes,
			"degraded", res.Degraded)
		return res, nil
	}

	meta := cache.Meta{
		RenderedAt: time.Now().UTC().Format(time.RFC3339),
		DurationMS: res.Duration.Milliseconds(),
		Status:     res.Status,
		Bytes:      res.Bytes,
		Tier:       res.Tier,
	}
	if err := s.cache.Set(s.appCtx, normalized, res.HTML, meta); err != nil {
		// A failed write costs a future cache hit, nothing more. The caller
		// already has valid HTML, so the request still succeeds.
		lg.Error("cache.store_failed", "error", err.Error(), "action", "serving uncached result")
	} else {
		lg.Info("cache.store", "ttl_s", int(s.cache.TTL().Seconds()), "bytes", res.Bytes)
	}
	return res, nil
}

func cacheSkipReason(res *render.Result, minBytes int) string {
	switch {
	case res.Degraded:
		return "render degraded; refusing to cache partial html"
	case res.Status < 200 || res.Status >= 300:
		return "upstream returned non-2xx"
	case res.Bytes < minBytes:
		return "html smaller than MIN_HTML_BYTES"
	default:
		return "unknown"
	}
}

// writeHTML sends a rendered (or cached) page.
func (s *Server) writeHTML(w http.ResponseWriter, r *http.Request, status int, html, cacheState, tier string, dur time.Duration) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(html)))
	h.Set("X-Prerender-Cache", cacheState)
	if tier != "" {
		h.Set("X-Prerender-Tier", tier)
	}
	if dur > 0 {
		h.Set("X-Prerender-Render-Time-Ms", strconv.FormatInt(dur.Milliseconds(), 10))
	}

	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write([]byte(html)); err != nil {
		loggerFrom(r.Context()).Warn("response.write_failed", "error", err.Error())
	}
}

// failRender maps a render error to a status code. Every branch logs the real
// underlying error; the client only ever receives a generic message plus the
// request ID.
func (s *Server) failRender(w http.ResponseWriter, r *http.Request, normalized string, err error) {
	switch {
	case errors.Is(err, limit.ErrQueueTimeout):
		retry := int(math.Ceil(s.cfg.QueueWaitTimeout.Seconds()))
		if retry < 1 {
			retry = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		s.fail(w, r, http.StatusServiceUnavailable, "render queue is full", err)

	case errors.Is(err, render.ErrBrowserUnavailable):
		w.Header().Set("Retry-After", "5")
		s.fail(w, r, http.StatusServiceUnavailable, "renderer unavailable", err)
		// A tab that cannot be opened usually means the process is gone.
		go s.browser.Recycle("tab acquisition failed")

	case errors.Is(err, render.ErrNavigation):
		s.fail(w, r, http.StatusBadGateway, "could not load the target URL", err)

	case errors.Is(err, render.ErrEmptyRender):
		s.fail(w, r, http.StatusGatewayTimeout, "render produced no html", err)

	case errors.Is(err, context.DeadlineExceeded):
		s.fail(w, r, http.StatusGatewayTimeout, "render timed out", err)

	default:
		s.fail(w, r, http.StatusInternalServerError, "render failed", err)
	}
}

// fail logs the real error with full context and writes a safe response body.
// The underlying error is never sent to the client, and never omitted from the
// logs.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, public string, err error) {
	lg := loggerFrom(r.Context())
	attrs := []any{
		"status", status,
		"error", err.Error(),
		"public_message", public,
	}
	if status >= 500 {
		lg.Error("request.failed", attrs...)
	} else {
		lg.Warn("request.rejected", attrs...)
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "%s (request %s)\n", public, requestIDFrom(r.Context()))
}

// --- operational endpoints --------------------------------------------------

// handleHealthz is liveness only: if the process can answer, it is alive.
// Never make this depend on Redis or Chromium, or a transient dependency blip
// will get the process killed by whatever is supervising it.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     "ok",
		"uptime_s":   int(time.Since(s.startedAt).Seconds()),
		"started_at": s.startedAt.UTC().Format(time.RFC3339),
	})
}

// handleReadyz reports whether the service can actually serve traffic, and
// names the subsystem that is broken when it cannot.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	type check struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	body := map[string]any{}
	ready := true

	if err := s.cache.Ping(r.Context()); err != nil {
		body["redis"] = check{OK: false, Error: err.Error()}
		ready = false
	} else {
		body["redis"] = check{OK: true}
	}

	if err := s.browser.Health(r.Context()); err != nil {
		body["browser"] = check{OK: false, Error: err.Error()}
		ready = false
	} else {
		body["browser"] = check{OK: true}
	}

	body["ready"] = ready
	w.Header().Set("Content-Type", "application/json")
	if ready {
		w.WriteHeader(http.StatusOK)
	} else {
		loggerFrom(r.Context()).Warn("readiness.failed", "detail", fmt.Sprintf("%v", body))
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(body)
}

// handleMetrics exposes the few counters worth watching, as JSON.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	gen, rendersThisProc, rendersTotal, running := s.browser.Stats()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"uptime_s":                int(time.Since(s.startedAt).Seconds()),
		"renders_in_flight":       s.sem.InFlight(),
		"renders_queued":          s.sem.Waiting(),
		"render_capacity":         s.sem.Capacity(),
		"coalesced_keys_in_fligh": s.group.InFlight(),
		"browser_running":         running,
		"browser_generation":      gen,
		"renders_this_process":    rendersThisProc,
		"renders_total":           rendersTotal,
	})
}

// handlePurge removes one URL from the cache: POST /_purge?url=<target>
func (s *Server) handlePurge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "POST, DELETE")
		s.fail(w, r, http.StatusMethodNotAllowed, "method not allowed",
			fmt.Errorf("purge via %s", r.Method))
		return
	}
	target := r.URL.Query().Get("url")
	if target == "" {
		s.fail(w, r, http.StatusBadRequest, "missing url parameter",
			errors.New("purge called without ?url="))
		return
	}
	normalized, err := urlutil.Normalize(target)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid url parameter", err)
		return
	}
	n, err := s.cache.Delete(r.Context(), normalized)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "purge failed", err)
		return
	}
	loggerFrom(r.Context()).Info("cache.purge", "url", normalized, "keys_deleted", n)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"purged": normalized, "keys_deleted": n})
}
