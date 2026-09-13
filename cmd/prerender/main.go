// Command prerender is a small server-side rendering service for
// JavaScript-heavy sites.
//
//	GET https://prerender.sbisecurities.in/https://www.sbisecurities.in/about-us
//
// The path after the service host is the URL to render. Rendered HTML is cached
// in Redis as a plain, readable string.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"prerender/internal/cache"
	"prerender/internal/config"
	"prerender/internal/render"
	"prerender/internal/server"
)

func main() {
	if err := run(); err != nil {
		// Startup failures go to stderr as plain text: at this point the
		// structured logger may not exist yet.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)
	log.Info("startup", cfg.Redacted()...)

	// appCtx is cancelled on SIGINT/SIGTERM. Renders are rooted on it, so
	// shutdown eventually stops them even if a page is hanging.
	appCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := cache.New(cfg)
	defer func() {
		if err := c.Close(); err != nil {
			log.Warn("redis.close_failed", "error", err.Error())
		}
	}()

	// Redis being unreachable at boot is a warning, not a fatal error: the
	// service can still render, it just cannot cache yet.
	pingCtx, cancelPing := context.WithTimeout(appCtx, 5*time.Second)
	if err := c.Ping(pingCtx); err != nil {
		log.Warn("redis.unreachable_at_startup",
			"error", err.Error(),
			"action", "starting anyway; every request will render until Redis returns")
	} else {
		log.Info("redis.connected", "addr", cfg.RedisAddr)
	}
	cancelPing()

	browser := render.NewBrowser(cfg, log)
	if err := browser.Start(appCtx); err != nil {
		// Without Chromium there is nothing this service can do, so this one
		// is fatal.
		return fmt.Errorf("browser startup: %w", err)
	}
	defer browser.Close()

	renderer := render.NewRenderer(cfg, log, browser)
	srv := server.New(appCtx, cfg, log, c, browser, renderer)
	httpServer := srv.HTTPServer()

	errCh := make(chan error, 1)
	go func() {
		log.Info("http.listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http listen on %s: %w", httpServer.Addr, err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return err
		}
	case <-appCtx.Done():
		log.Info("shutdown.signal_received", "grace_period", cfg.ShutdownGrace.String())
	}

	// Stop accepting new connections and let in-flight renders finish. The
	// grace period is validated at startup to exceed RENDER_TIMEOUT, so a
	// render in progress is not cut off.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown.forced",
			"error", err.Error(),
			"note", "some requests were still in flight when the grace period expired")
	}

	// Browser and Redis are closed by the deferred calls above, in reverse
	// order: browser first (it waits for in-flight renders), then Redis.
	log.Info("shutdown.complete")
	return nil
}

// newLogger builds the structured JSON logger used everywhere.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// RFC3339 with milliseconds sorts lexically and is easy to grep.
			if a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					a.Value = slog.StringValue(t.UTC().Format("2006-01-02T15:04:05.000Z"))
				}
			}
			return a
		},
	})
	return slog.New(handler).With("service", "prerender")
}
