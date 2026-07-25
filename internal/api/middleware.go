package api

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"
)

// statusRecorder captures the status code written by a handler for request logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush forwards to the underlying ResponseWriter so SSE handlers (GET /api/hardware/live)
// can stream through the middleware chain. Without this the wrapped writer would not satisfy
// http.Flusher and the SSE handler would (correctly) refuse to stream. It is a no-op if the
// underlying writer is not a Flusher.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying ResponseWriter so the WebSocket handler (GET /api/ws)
// can take over the connection through the middleware chain. Without this the wrapped
// writer would not satisfy http.Hijacker and websocket.Accept would fail the upgrade with
// 501. Errors if the underlying writer is not a Hijacker (e.g. httptest recorder).
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
}

// recoverMiddleware converts a panic in any handler into a JSON error envelope with a
// reason, so a bug never leaks a stack trace or an opaque 500 to the client (spec-digest §3
// "no opaque 500s"). The stack is logged server-side only.
func recoverMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic recovered",
					"method", r.Method, "path", r.URL.Path,
					"panic", rec, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError,
					"internal", "internal server error",
					"an unexpected error occurred while handling the request")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// logMiddleware emits one structured line per request (method, path, status, duration,
// remote addr). Per observability rules it does NOT log request/response bodies.
func logMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}
