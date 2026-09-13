// Package server exposes the HTTP surface of the prerender service.
//
// Routing is done by hand rather than with http.ServeMux. ServeMux cleans the
// request path and issues a 301 redirect for anything containing "//", which
// would break every request this service exists to answer:
//
//	/https://www.sbisecurities.in  ->  301  ->  /https:/www.sbisecurities.in
//
// A plain http.Handler sees the untouched RequestURI, so the target URL
// survives intact.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"prerender/internal/cache"
	"prerender/internal/config"
	"prerender/internal/limit"
	"prerender/internal/render"
)

// Server holds every dependency the handlers need.
type Server struct {
	cfg      *config.Config
	log      *slog.Logger
	cache    *cache.Cache
	renderer *render.Renderer
	browser  *render.Browser
	sem      *limit.Semaphore
	group    *limit.Group

	// appCtx outlives individual requests. Renders are rooted on it so they
	// survive client disconnects but still stop at shutdown.
	appCtx    context.Context
	startedAt time.Time
}

// New assembles the server.
func New(
	appCtx context.Context,
	cfg *config.Config,
	log *slog.Logger,
	c *cache.Cache,
	b *render.Browser,
	rd *render.Renderer,
) *Server {
	return &Server{
		cfg:       cfg,
		log:       log,
		cache:     c,
		browser:   b,
		renderer:  rd,
		sem:       limit.NewSemaphore(cfg.MaxConcurrentRenders),
		group:     &limit.Group{},
		appCtx:    appCtx,
		startedAt: time.Now(),
	}
}

// ServeHTTP routes on the cleaned path for operational endpoints and treats
// everything else as a prerender request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		s.handleHealthz(w, r)
	case r.URL.Path == "/readyz":
		s.handleReadyz(w, r)
	case r.URL.Path == "/_metrics":
		s.handleMetrics(w, r)
	case r.URL.Path == "/_purge":
		s.handlePurge(w, r)
	case r.URL.Path == "/favicon.ico":
		w.WriteHeader(http.StatusNoContent)
	case r.RequestURI == "/" || strings.TrimSpace(r.RequestURI) == "":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("usage: GET /https://example.com/path\n"))
	default:
		s.handlePrerender(w, r)
	}
}

// Handler returns the fully wrapped handler chain. Recovery is outermost so it
// also covers panics raised inside the logging middleware.
func (s *Server) Handler() http.Handler {
	return s.withRecovery(s.withRequestContext(http.HandlerFunc(s.ServeHTTP)))
}

// HTTPServer builds the configured net/http server.
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Addr:              ":" + s.cfg.Port,
		Handler:           s.Handler(),
		ReadHeaderTimeout: s.cfg.ReadTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		// Must comfortably exceed RENDER_TIMEOUT, or slow renders are cut off
		// mid-response.
		WriteTimeout: s.cfg.WriteTimeout,
		IdleTimeout:  s.cfg.IdleTimeout,
		ErrorLog:     slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
}
