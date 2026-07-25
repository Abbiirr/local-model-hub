// Package api hosts the HTTP transport (REST + SSE, with WS/JSON-RPC to follow) over the
// single service core. Handlers are THIN: they parse input, call one core method, and
// marshal the result or a standard error envelope. No business logic lives here
// (PLAN.md decision 7).
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/localhub/localhub/internal/core"
)

// Server wraps the http.Server plus its listener and the service core.
type Server struct {
	svc    *core.Service
	logger *slog.Logger
	http   *http.Server
	ln     net.Listener
	addr   string
}

// New builds a Server bound to bindAddr:port. Binding happens here (net.Listen) so that a
// bind failure — including a refused non-loopback bind at the OS level — surfaces before
// the daemon claims to be up. Config validation already rejects non-loopback binds without
// the allow_nonlocal override; this is defense in depth.
func New(svc *core.Service, logger *slog.Logger, bindAddr string, port int) (*Server, error) {
	addr := net.JoinHostPort(bindAddr, fmt.Sprintf("%d", port))

	mux := http.NewServeMux()
	s := &Server{svc: svc, logger: logger, addr: addr}
	s.routes(mux)

	// Middleware chain: recover(outermost) -> log -> mux. Recover is outermost so a panic
	// in logging or a handler still produces a JSON envelope.
	handler := recoverMiddleware(logger, logMiddleware(logger, mux))

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind %s: %w", addr, err)
	}
	s.ln = ln
	s.http = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// Addr returns the actual listen address (host:port), useful when port 0 was requested and
// the OS chose a free port (tests do this).
func (s *Server) Addr() string {
	if s.ln != nil {
		return s.ln.Addr().String()
	}
	return s.addr
}

// routes registers all HTTP routes. Handlers are thin (parse → one core call → marshal).
func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", s.handleHealth)

	// Hardware (P1.2).
	mux.HandleFunc("GET /api/hardware", s.handleHardware)
	mux.HandleFunc("GET /api/hardware/live", s.handleHardwareLive) // SSE

	// Engines (P1.3) — detection is read-only; /detect POST re-probes, mutates nothing.
	mux.HandleFunc("GET /api/engines", s.handleEngines)
	mux.HandleFunc("POST /api/engines/detect", s.handleEnginesDetect)

	// Models inventory (P1.4).
	mux.HandleFunc("GET /api/models", s.handleModels)
	mux.HandleFunc("POST /api/models/rescan", s.handleModelsRescan)

	// Compatibility (P2.4). preview computes a verdict WITHOUT downloading weights (remote
	// metadata read); the per-model route caches the verdict onto the models row. Both are
	// read-only w.r.t. engines (no model start/stop); the DB write is a cached-verdict summary.
	mux.HandleFunc("POST /api/compat/preview", s.handleCompatPreview)
	mux.HandleFunc("GET /api/models/{id}/compat", s.handleModelCompat)

	// Supply chain (Phase 3): downloads (two-step HF, gguf_url, ollama pull, local import),
	// immutable manifests, and the reproduce export. download.progress events stream over WS.
	mux.HandleFunc("POST /api/downloads", s.handleDownloadCreate)
	mux.HandleFunc("GET /api/downloads", s.handleDownloadList)
	mux.HandleFunc("GET /api/downloads/{id}", s.handleDownloadStatus)
	mux.HandleFunc("GET /api/manifests/{id}", s.handleManifestGet)
	mux.HandleFunc("GET /api/models/{id}/manifest", s.handleModelManifest)
	mux.HandleFunc("GET /api/models/{id}/reproduce", s.handleReproduce)

	// Instances (P4.1/P4.2): start/stop/list spawned model instances (llama.cpp). ADMIN
	// actions — they spawn/kill GPU processes; localhost-bound + token-gated by middleware.
	mux.HandleFunc("GET /api/instances", s.handleInstancesList)
	mux.HandleFunc("POST /api/instances", s.handleInstanceCreate)
	mux.HandleFunc("GET /api/instances/{id}", s.handleInstanceGet)
	mux.HandleFunc("DELETE /api/instances/{id}", s.handleInstanceStop)
	// Completion pass-through (P5 fix 3): relays to the engine, counting in-flight requests so
	// eviction never selects a streaming instance. Also Phase 6's routing substrate. Streaming intact.
	mux.HandleFunc("POST /api/instances/{id}/completions", s.handleInstanceCompletion)

	// Residency queue (P5.1): the admission queue backing WAITING→ADMITTED. GET lists entries
	// (with blocked_reason + position); DELETE cancels a WAITING entry. Read/cancel only —
	// admission itself happens through POST /api/instances (a queued start returns 202 + position).
	mux.HandleFunc("GET /api/queue", s.handleQueueList)
	mux.HandleFunc("DELETE /api/queue/{id}", s.handleQueueCancel)

	// WebSocket live channel (P1.5, PLAN.md decision 7c): server-push hardware snapshots +
	// models/engines change signals + heartbeat. Read-only; no client frames consumed.
	mux.HandleFunc("GET /api/ws", s.handleWS)

	// Unmatched /api/* → JSON 404 envelope (NOT plain-text "404 page not found"). Go 1.22
	// ServeMux gives the specific method+path patterns above precedence over this subtree
	// catch-all, so this only fires for genuinely unregistered API paths.
	mux.HandleFunc("/api/", s.handleAPINotFound)

	// Static dashboard (P1.5): the embedded web/dist with SPA fallback. Mounted LAST on the
	// bare "/" so every API route wins; this catches only non-API, non-WS paths. If the
	// embed is somehow unavailable, log and skip so the API still serves.
	if static, err := newStaticHandler(); err != nil {
		s.logger.Error("dashboard embed unavailable; UI will not be served", "err", err)
	} else {
		mux.Handle("/", static)
	}
}

// handleAPINotFound is the JSON 404 for unmatched /api/* paths (registered on the "/api/"
// subtree so specific routes take precedence).
func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeAPINotFound(w, r)
}

// Serve runs the server until the context is cancelled, then shuts down gracefully,
// draining in-flight requests up to a timeout.
func (s *Server) Serve(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("http server listening", "addr", s.Addr())
		if err := s.http.Serve(s.ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.logger.Info("shutdown requested, draining connections")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	}
}

// handleHealth is the GET /api/health placeholder: reports version + db reachability via
// the core. Thin transport: parse nothing, call core.Health, marshal.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	res := s.svc.Health(r.Context())
	status := http.StatusOK
	if res.Status != "ok" {
		// Health degraded is still a 200-with-body for probes that read the payload, but
		// we signal it with 503 so naive HTTP checks also see failure.
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, res)
}
