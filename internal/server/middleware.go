package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"
)

type ctxKey int

const (
	ctxKeyLogger ctxKey = iota
	ctxKeyRequestID
)

// newRequestID returns a short random identifier. Short enough to paste into a
// log query, random enough not to collide.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Failing to read randomness must never fail a request.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// loggerFrom returns the request-scoped logger, which already carries the
// request ID and target URL.
func loggerFrom(ctx context.Context) *slog.Logger {
	if lg, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok {
		return lg
	}
	return slog.Default()
}

func requestIDFrom(ctx context.Context) string {
	if id, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return id
	}
	return ""
}

// statusWriter records the status code and byte count for the access log.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written int
	wrote   bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += n
	return n, err
}

// withRequestContext assigns a request ID, builds the request-scoped logger and
// emits the start/end access log pair.
func (s *Server) withRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := newRequestID()

		lg := s.log.With(
			"request_id", id,
			"method", r.Method,
			"request_uri", r.RequestURI,
		)

		ctx := context.WithValue(r.Context(), ctxKeyLogger, lg)
		ctx = context.WithValue(ctx, ctxKeyRequestID, id)
		r = r.WithContext(ctx)

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		w.Header().Set("X-Request-ID", id)

		if !isProbe(r.URL.Path) {
			lg.Info("request.start",
				"client_ip", clientIP(r),
				"user_agent", r.UserAgent())
		}

		next.ServeHTTP(sw, r)

		if !isProbe(r.URL.Path) {
			lg.Info("request.end",
				"status", sw.status,
				"bytes", sw.written,
				"total_duration_ms", time.Since(start).Milliseconds())
		}
	})
}

// withRecovery converts a panic into a 500 and keeps the process alive. A panic
// in one request must never take down the service.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				lg := loggerFrom(r.Context())
				lg.Error("panic",
					"error", panicMessage(rec),
					"stack", string(debug.Stack()))

				// The client gets the request ID only; the detail stays in the
				// logs where it belongs.
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("internal server error (request " + requestIDFrom(r.Context()) + ")\n"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func panicMessage(rec any) string {
	switch v := rec.(type) {
	case error:
		return v.Error()
	case string:
		return v
	default:
		return http.StatusText(http.StatusInternalServerError)
	}
}

func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz"
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := indexByte(xff, ','); i > 0 {
			return trimSpace(xff[:i])
		}
		return trimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// withLogger replaces the request-scoped logger, used once the target URL is
// known so every later line carries it.
func withLogger(ctx context.Context, lg *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, lg)
}
