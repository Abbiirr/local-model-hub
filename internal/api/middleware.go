package api

import (
	"bufio"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/localhub/localhub/internal/proxy"
)

// dashboardLiveCookie is accepted only on read-only live endpoints. Browser
// EventSource and WebSocket constructors cannot attach Authorization headers.
const dashboardLiveCookie = "localhub_dashboard_live"

func dashboardLiveCookieValue(adminToken string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(adminToken))
}

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

// authMiddleware enforces the optional single admin bearer token (security.md item 3:
// "for LAN/headless setups, a single admin token in config"). When adminToken is empty the
// chain is untouched (the default localhost-only bind needs no token) — a cluster token alone
// NEVER turns global auth on, so enabling clustering on a loopback dev box does not lock down the
// local admin API. When adminToken is set, every /api/* and /v1/* request must carry
// `Authorization: Bearer <token>`; GET /api/health stays open so liveness probes work, and static
// UI assets stay open (they are useless without API access, and a browser cannot inject the
// header — headless/API clients are the audience for this option). Comparison is constant-time.
//
// extraTokens (P7.1, cluster-plan §4.2), when non-empty, are accepted as ADDITIONAL valid bearers
// alongside adminToken so coordinator→node cluster-plane callbacks pass the same middleware. They do
// not, by themselves, gate anything (the cluster join endpoint enforces the cluster token separately
// in its handler). Variadic so the existing 2-arg callers stay source-compatible.
func authMiddleware(adminToken string, next http.Handler, extraTokens ...string) http.Handler {
	if adminToken == "" {
		return next
	}
	accepted := [][]byte{[]byte("Bearer " + adminToken)}
	for _, tok := range extraTokens {
		if tok != "" {
			accepted = append(accepted, []byte("Bearer "+tok))
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		protected := strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/v1/")
		if protected && !(r.Method == http.MethodGet && (path == "/api/health" || path == "/api/auth/dashboard")) {
			got := []byte(r.Header.Get("Authorization"))
			ok := false
			for _, exp := range accepted {
				if subtle.ConstantTimeCompare(got, exp) == 1 {
					ok = true
					break
				}
			}
			// Anthropic SDKs normally use x-api-key while Claude Code can use
			// Authorization: Bearer. Both authenticate to the same single
			// LocalHub admin token; neither credential is forwarded upstream.
			anthropicPath := path == "/v1/messages" || path == "/v1/messages/count_tokens"
			bothCredentialsMismatch := false
			if anthropicPath && r.Header.Get("Authorization") != "" && r.Header.Get("x-api-key") != "" {
				wantBearer := []byte("Bearer " + adminToken)
				if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), wantBearer) != 1 ||
					subtle.ConstantTimeCompare([]byte(r.Header.Get("x-api-key")), []byte(adminToken)) != 1 {
					ok = false
					bothCredentialsMismatch = true
				}
			}
			if !ok && anthropicPath && !bothCredentialsMismatch {
				ok = subtle.ConstantTimeCompare([]byte(r.Header.Get("x-api-key")), []byte(adminToken)) == 1
			}
			if !ok && r.Method == http.MethodGet && (path == "/api/hardware/live" || path == "/api/ws") {
				if c, err := r.Cookie(dashboardLiveCookie); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(dashboardLiveCookieValue(adminToken))) == 1 {
					ok = true
				}
			}
			if !ok {
				if anthropicPath {
					proxy.WriteAnthropicError(w, http.StatusUnauthorized, "authentication_error",
						"missing or invalid LocalHub credential; send Authorization: Bearer <admin_token> or x-api-key: <admin_token>")
					return
				}
				writeError(w, http.StatusUnauthorized, "unauthorized",
					"missing or invalid bearer token",
					"this daemon requires Authorization: Bearer <admin_token> (server.admin_token is set)")
				return
			}
		}
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
