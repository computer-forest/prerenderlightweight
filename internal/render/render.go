// Package render drives headless Chromium and decides when a page is finished.
//
// Without a cooperating flag in the page (window.prerenderReady or similar),
// "finished" has to be inferred. This renderer infers it from two independent
// signals and falls back through a ladder:
//
//	tier 1  network_idle  document complete, zero in-flight requests, quiet for NETWORK_IDLE_MS
//	tier 2  dom_stable    document complete, outerHTML length unchanged over STABILITY_CHECKS polls
//	tier 3  max_wait      MAX_WAIT_MS ceiling reached; return what we have
//	tier 4  degraded      render timed out or errored; return what we can, never cache it
//
// Tier 1 handles the common Angular case: the bundle loads, the app bootstraps,
// XHRs resolve, and the network goes quiet. Tier 2 catches pages that never go
// quiet because of polling, analytics beacons, or an open websocket.
package render

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"prerender/internal/config"
)

// Sentinel errors, mapped to HTTP status codes by the handler.
var (
	// ErrNavigation means Chromium could not reach the page at all: DNS
	// failure, connection refused, TLS error. Maps to 502.
	ErrNavigation = errors.New("navigation failed")
	// ErrBrowserUnavailable means no tab could be opened. Maps to 503.
	ErrBrowserUnavailable = errors.New("browser unavailable")
	// ErrEmptyRender means the render produced no usable HTML. Maps to 504.
	ErrEmptyRender = errors.New("render produced no usable html")
)

// Readiness tiers, reported in logs, metadata and the X-Prerender-Tier header.
const (
	TierNetworkIdle = "network_idle"
	TierDOMStable   = "dom_stable"
	TierMaxWait     = "max_wait"
	TierDegraded    = "degraded"
)

// Result is a completed render.
type Result struct {
	HTML      string
	Status    int           // upstream HTTP status of the main document
	Tier      string        // which readiness tier fired
	Degraded  bool          // true when the page never reached a clean state
	Duration  time.Duration // wall clock for the render itself
	Bytes     int
	PageErrs  int // uncaught JS exceptions observed
	FailedReq int // subresource requests that failed (excluding blocked ones)
}

// Cacheable reports whether a result is safe to store. Partial HTML served once
// is unfortunate; partial HTML cached for hours is a real SEO problem, so
// degraded renders and non-2xx responses are never written to Redis.
func (r *Result) Cacheable(minBytes int) bool {
	return !r.Degraded &&
		r.Status >= 200 && r.Status < 300 &&
		r.Bytes >= minBytes
}

// Renderer turns URLs into HTML.
type Renderer struct {
	cfg     *config.Config
	log     *slog.Logger
	browser *Browser
}

// NewRenderer wires a renderer to a running browser.
func NewRenderer(cfg *config.Config, log *slog.Logger, b *Browser) *Renderer {
	return &Renderer{cfg: cfg, log: log, browser: b}
}

// Render loads targetURL in a fresh tab and returns its serialized DOM.
//
// ctx bounds the whole operation. It is deliberately not the HTTP request
// context: a render that has already started should finish and populate the
// cache even if the client that triggered it disconnects.
func (r *Renderer) Render(ctx context.Context, targetURL string, log *slog.Logger) (*Result, error) {
	start := time.Now()

	sess, err := r.browser.Acquire()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrBrowserUnavailable, err)
	}
	defer sess.Close()

	// Every tab is bounded. If the page hangs, the deadline fires, the tab is
	// cancelled, and the deferred Close above reclaims it.
	renderCtx, cancelRender := context.WithTimeout(sess.Ctx, r.cfg.RenderTimeout)
	defer cancelRender()

	tr := newTracker()
	chromedp.ListenTarget(sess.Ctx, tr.handle)

	setup := []chromedp.Action{network.Enable()}
	if r.cfg.BlockResources && len(r.cfg.BlockedURLPatterns) > 0 {
		setup = append(setup, network.SetBlockedURLS(r.cfg.BlockedURLPatterns))
	}
	if err := chromedp.Run(renderCtx, setup...); err != nil {
		return nil, fmt.Errorf("enable cdp network domain: %w", err)
	}

	// Navigation gets its own, shorter deadline. Cancelling a context derived
	// from the tab context aborts the action without destroying the tab, so a
	// slow navigation degrades into the readiness loop instead of failing.
	navCtx, cancelNav := context.WithTimeout(renderCtx, r.cfg.NavigationTimeout)
	navErr := chromedp.Run(navCtx, chromedp.Navigate(targetURL))
	cancelNav()

	navTimedOut := false
	if navErr != nil {
		switch {
		case errors.Is(navErr, context.DeadlineExceeded) && renderCtx.Err() == nil:
			navTimedOut = true
			log.Warn("render.navigation_timeout",
				"navigation_timeout", r.cfg.NavigationTimeout.String(),
				"pending_requests", tr.pendingURLs(),
				"note", "continuing to readiness polling; page may still settle")
		default:
			return nil, fmt.Errorf("%w: %s: %s", ErrNavigation, targetURL, navErr)
		}
	}

	// A main-document request that never produced a response is a hard
	// navigation failure even if chromedp itself returned nil.
	if msg := tr.mainFailure(); msg != "" {
		return nil, fmt.Errorf("%w: %s: %s", ErrNavigation, targetURL, msg)
	}

	tier, degraded := r.waitForReady(renderCtx, tr, log)

	html, err := r.extract(renderCtx, sess, degraded)
	if err != nil {
		return nil, fmt.Errorf("extract html for %s (tier=%s): %w", targetURL, tier, err)
	}
	if html == "" {
		return nil, fmt.Errorf("%w: %s (tier=%s)", ErrEmptyRender, targetURL, tier)
	}

	status := tr.status()
	if status == 0 {
		// No document response was observed. This happens for pages served
		// entirely from the back/forward cache or for data: URLs; treat as 200
		// since we do have HTML.
		status = 200
	}
	if navTimedOut {
		degraded = true
		tier = TierDegraded
	}

	return &Result{
		HTML:      html,
		Status:    status,
		Tier:      tier,
		Degraded:  degraded,
		Duration:  time.Since(start),
		Bytes:     len(html),
		PageErrs:  tr.pageErrors(),
		FailedReq: tr.failedRequests(),
	}, nil
}

// readyState is the payload of the polling expression. Both signals are fetched
// in one CDP round trip to keep polling cheap.
type readyState struct {
	Ready string `json:"ready"`
	Len   int    `json:"len"`
}

const readyExpr = `(function(){
  var el = document.documentElement;
  return {ready: document.readyState, len: el ? el.outerHTML.length : 0};
})()`

// waitForReady polls the page until one of the readiness tiers fires.
func (r *Renderer) waitForReady(ctx context.Context, tr *tracker, log *slog.Logger) (tier string, degraded bool) {
	var (
		start       = time.Now()
		minWait     = time.Duration(r.cfg.MinWaitMS) * time.Millisecond
		maxWait     = time.Duration(r.cfg.MaxWaitMS) * time.Millisecond
		idleWindow  = time.Duration(r.cfg.NetworkIdleMS) * time.Millisecond
		pollEvery   = time.Duration(r.cfg.PollIntervalMS) * time.Millisecond
		stableCount int
		lastLen     = -1
		polls       int
	)

	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Warn("render.readiness_aborted",
				"reason", ctx.Err().Error(),
				"elapsed_ms", time.Since(start).Milliseconds(),
				"polls", polls,
				"pending_requests", tr.pendingURLs())
			return TierDegraded, true
		case <-ticker.C:
		}

		polls++
		elapsed := time.Since(start)

		var st readyState
		pollCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := chromedp.Run(pollCtx, chromedp.Evaluate(readyExpr, &st))
		cancel()
		if err != nil {
			// Evaluations fail transiently while a navigation is committing.
			// Only give up when the render context itself is finished.
			if ctx.Err() != nil {
				return TierDegraded, true
			}
			log.Debug("render.poll_failed", "error", err.Error(), "poll", polls)
			if elapsed >= maxWait {
				return TierMaxWait, false
			}
			continue
		}

		if st.Ready == "complete" && elapsed >= minWait {
			if inflight, quiet := tr.idle(); inflight == 0 && quiet >= idleWindow {
				log.Debug("render.ready", "tier", TierNetworkIdle,
					"elapsed_ms", elapsed.Milliseconds(), "html_bytes", st.Len)
				return TierNetworkIdle, false
			}
			if st.Len == lastLen {
				stableCount++
				if stableCount >= r.cfg.StabilityChecks {
					log.Debug("render.ready", "tier", TierDOMStable,
						"elapsed_ms", elapsed.Milliseconds(), "html_bytes", st.Len)
					return TierDOMStable, false
				}
			} else {
				stableCount = 0
			}
		}
		lastLen = st.Len

		if elapsed >= maxWait {
			log.Warn("render.max_wait_reached",
				"elapsed_ms", elapsed.Milliseconds(),
				"ready_state", st.Ready,
				"html_bytes", st.Len,
				"pending_requests", tr.pendingURLs(),
				"note", "page never settled; consider raising MAX_WAIT_MS")
			return TierMaxWait, false
		}
	}
}

// extract serializes the DOM. When the render context is already dead (the
// degraded path) a short-lived context on the still-open tab is used so that we
// can salvage partial HTML rather than returning nothing.
func (r *Renderer) extract(ctx context.Context, sess *Session, degraded bool) (string, error) {
	extractCtx := ctx
	var cancel context.CancelFunc
	if degraded || ctx.Err() != nil {
		extractCtx, cancel = context.WithTimeout(sess.Ctx, 5*time.Second)
	} else {
		extractCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
	}
	defer cancel()

	var html string
	if err := chromedp.Run(extractCtx,
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
	); err != nil {
		return "", err
	}
	if html != "" {
		// OuterHTML returns the <html> element only; the doctype is needed for
		// crawlers to parse the document in standards mode.
		html = "<!DOCTYPE html>\n" + html
	}
	return html, nil
}

// tracker accumulates CDP events for one tab: in-flight request count, the main
// document's status, navigation failures and JS exceptions.
type tracker struct {
	mu sync.Mutex

	pending      map[network.RequestID]string // request ID -> URL, for diagnosing stuck renders
	lastActivity time.Time

	mainRequestID network.RequestID
	mainStatus    int
	mainFailedMsg string

	failedReq int
	jsErrors  int
}

func newTracker() *tracker {
	return &tracker{
		pending:      make(map[network.RequestID]string),
		lastActivity: time.Now(),
	}
}

// isMain reports whether a request is the main-frame document navigation.
// Chromium assigns the navigation request an id equal to its loader id, which
// is the standard way to distinguish it from iframe documents.
func isMain(reqID network.RequestID, loaderID string, t network.ResourceType) bool {
	return t == network.ResourceTypeDocument && string(reqID) == loaderID
}

func (t *tracker) handle(ev any) {
	switch e := ev.(type) {
	case *network.EventRequestWillBeSent:
		t.mu.Lock()
		if _, seen := t.pending[e.RequestID]; !seen && e.Request != nil {
			t.pending[e.RequestID] = e.Request.URL
		}
		t.lastActivity = time.Now()
		if isMain(e.RequestID, e.LoaderID.String(), e.Type) && t.mainRequestID == "" {
			t.mainRequestID = e.RequestID
		}
		t.mu.Unlock()

	case *network.EventResponseReceived:
		t.mu.Lock()
		t.lastActivity = time.Now()
		if e.Response != nil && isMain(e.RequestID, e.LoaderID.String(), e.Type) {
			t.mainStatus = int(e.Response.Status)
		}
		t.mu.Unlock()

	case *network.EventLoadingFinished:
		t.mu.Lock()
		delete(t.pending, e.RequestID)
		t.lastActivity = time.Now()
		t.mu.Unlock()

	case *network.EventLoadingFailed:
		t.mu.Lock()
		delete(t.pending, e.RequestID)
		t.lastActivity = time.Now()
		if e.BlockedReason == "" {
			// Blocked resources are intentional, so they are not failures.
			t.failedReq++
		}
		if e.RequestID == t.mainRequestID && t.mainStatus == 0 && !e.Canceled {
			t.mainFailedMsg = e.ErrorText
		}
		t.mu.Unlock()

	case *runtime.EventExceptionThrown:
		t.mu.Lock()
		t.jsErrors++
		t.mu.Unlock()
	}
}

// idle returns the in-flight request count and how long the network has been
// quiet.
func (t *tracker) idle() (inflight int, quietFor time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending), time.Since(t.lastActivity)
}

// pendingURLs returns up to 5 URLs still in flight, for diagnosing exactly
// what a stalled render is waiting on.
func (t *tracker) pendingURLs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	urls := make([]string, 0, len(t.pending))
	for _, u := range t.pending {
		urls = append(urls, u)
		if len(urls) == 5 {
			break
		}
	}
	return urls
}

func (t *tracker) status() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mainStatus
}

func (t *tracker) mainFailure() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mainFailedMsg
}

func (t *tracker) failedRequests() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failedReq
}

func (t *tracker) pageErrors() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.jsErrors
}
