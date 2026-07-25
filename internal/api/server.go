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

	"github.com/localhub/localhub/internal/cluster"
	"github.com/localhub/localhub/internal/core"
	"github.com/localhub/localhub/internal/jsonrpc"
)

// Server wraps the http.Server plus its listener and the service core.
type Server struct {
	svc        *core.Service
	logger     *slog.Logger
	http       *http.Server
	ln         net.Listener
	addr       string
	adminToken string
	rpc        *jsonrpc.Dispatcher // shared JSON-RPC 2.0 dispatcher (P6.3), used by POST /api/rpc + /api/ws

	// Cluster plane (P7.1, ADR-008). All three are the zero value for cluster.role=off, in which
	// case NO cluster routes are registered and the daemon is byte-identical to 1.0 (cluster-plan
	// §1.3). clusterRole=="coordinator" (with a non-nil clusterRegistry) gates /api/cluster/*.
	// clusterToken gates the join endpoint independently of the admin-token middleware.
	clusterRole     string
	clusterToken    string
	clusterRegistry *cluster.Registry
	// clusterAgg is the P7.2 fan-out aggregator (coordinator only; nil otherwise). It backs
	// /api/cluster/models, /api/cluster/hardware, /api/cluster/v1/models and the cluster.* RPC mirrors.
	clusterAgg *cluster.Aggregator
}

// Option customizes a Server at construction. Variadic options keep New backward-compatible with
// the existing 5-arg callers (no cluster wiring => role off).
type Option func(*Server)

// WithCluster enables the cluster plane on this Server (cluster-plan §2.2). role selects the
// behavior ("coordinator" registers /api/cluster/* backed by reg; "node" carries the token so the
// coordinator-callback path in the auth middleware accepts it). token is the shared cluster bearer,
// distinct from admin_token. nodeID/nodeName identify THIS daemon's own node entry in the aggregated
// views (the coordinator is also a node, cluster-plan §2.2) — its local inventory/hardware is
// included WITHOUT an HTTP round-trip (P7.2). Passing role "off" (or an empty role) is a no-op.
func WithCluster(role, token, nodeID, nodeName string, reg *cluster.Registry) Option {
	return func(s *Server) {
		if role == "" || role == "off" {
			return
		}
		s.clusterRole = role
		s.clusterToken = token
		s.clusterRegistry = reg
		if role == "coordinator" && reg != nil {
			s.clusterAgg = s.newClusterAggregator(nodeID, nodeName, token, reg)
		}
	}
}

// New builds a Server bound to bindAddr:port. Binding happens here (net.Listen) so that a
// bind failure — including a refused non-loopback bind at the OS level — surfaces before
// the daemon claims to be up. Config validation already rejects non-loopback binds without
// the allow_nonlocal override; this is defense in depth. adminToken, when non-empty,
// enforces the optional bearer token from server.admin_token (security.md item 3).
func New(svc *core.Service, logger *slog.Logger, bindAddr string, port int, adminToken string, opts ...Option) (*Server, error) {
	addr := net.JoinHostPort(bindAddr, fmt.Sprintf("%d", port))

	mux := http.NewServeMux()
	s := &Server{svc: svc, logger: logger, addr: addr, adminToken: adminToken}
	// Apply cluster (and future) options BEFORE routes() so role-gated route registration and the
	// auth middleware both see the resolved cluster fields (P7.1).
	for _, opt := range opts {
		opt(s)
	}
	// One JSON-RPC dispatcher shared by HTTP + WS so a method behaves identically on both
	// transports (P6.3, design §3.1). Jobs.* are wired to core via the coreJobService shim
	// (rpc_jobs.go) rather than the unwired stub — P6.2 core methods now exist.
	// clusterRPCService is nil unless this is a coordinator (opts applied above set clusterAgg), so the
	// cluster.* RPC mirrors are registered only in coordinator role (P7.2), mirroring the REST gating.
	s.rpc = jsonrpc.NewDispatcherWithServices(svc, coreJobService{svc: svc}, s.clusterRPCService())
	s.routes(mux)

	// Middleware chain: recover(outermost) -> log -> auth -> mux. Recover is outermost so
	// a panic in logging or a handler still produces a JSON envelope; auth sits inside log
	// so rejected (401) requests are still logged. The cluster token is accepted as a second
	// valid bearer alongside admin_token (cluster-plan §4.2) so coordinator→node callbacks pass.
	handler := recoverMiddleware(logger, logMiddleware(logger, authMiddleware(adminToken, mux, s.clusterToken)))

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
	mux.HandleFunc("GET /api/auth/dashboard", s.handleDashboardAuthStatus)
	mux.HandleFunc("POST /api/auth/dashboard", s.handleDashboardAuthLogin)
	mux.HandleFunc("DELETE /api/auth/dashboard", s.handleDashboardAuthLogout)

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

	// P6.1 thin OpenAI proxy (internal/api/proxy.go). Inference endpoints — the same auth
	// middleware + loopback bind cover them; vLLM dev endpoints are NEVER forwarded (security.md).
	// Go 1.22 ServeMux gives these literal patterns precedence over the "/" static catch-all.
	mux.HandleFunc("GET /v1/models", s.handleV1Models)
	mux.HandleFunc("POST /v1/chat/completions", s.handleV1ChatCompletions)
	mux.HandleFunc("POST /v1/completions", s.handleV1Completions)

	// P6.2 async inference jobs (internal/api/jobs.go). Submit → id immediately; poll or watch
	// jobs.updated over /api/ws; cancel; survive a daemon restart. The /api/ catch-all below still
	// fires for genuinely unregistered /api/* paths.
	mux.HandleFunc("POST /api/jobs", s.handleJobSubmit)
	mux.HandleFunc("GET /api/jobs", s.handleJobList)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleJobGet)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.handleJobCancel)

	// P7.1 cluster plane (ADR-008, cluster-plan §5b). Registered ONLY in coordinator role, so a
	// role=off or node daemon has NONE of these routes and the /api/ catch-all below returns the
	// standard 404 envelope for them (the role-off regression gate). join is the idempotent
	// node→coordinator upsert (cluster-token-gated in the handler); list/detail/remove are the
	// registry read/manage surface (admin-token-gated by the middleware like every other /api/*).
	if s.clusterRole == "coordinator" && s.clusterRegistry != nil {
		mux.HandleFunc("POST /api/cluster/nodes/join", s.handleClusterJoin)
		mux.HandleFunc("GET /api/cluster/nodes", s.handleClusterNodesList)
		mux.HandleFunc("GET /api/cluster/nodes/{id}", s.handleClusterNodeGet)
		mux.HandleFunc("DELETE /api/cluster/nodes/{id}", s.handleClusterNodeRemove)
		// P7.2 aggregated views (cluster-plan §5b, §8 P7.2): concurrent authed fan-out to each node,
		// node-attributed, partial-on-failure. /api/cluster/v1/models is the union-deduped OpenAI list
		// (a NEW path — the coordinator's own /v1/models stays local-only until routing lands in P7.3;
		// see handleClusterV1Models).
		mux.HandleFunc("GET /api/cluster/models", s.handleClusterModels)
		mux.HandleFunc("GET /api/cluster/hardware", s.handleClusterHardware)
		mux.HandleFunc("GET /api/cluster/v1/models", s.handleClusterV1Models)
	}

	// P6.3 JSON-RPC 2.0 control surface (PLAN.md decision 7d): a thin layer over the same core as
	// REST. Single request object OR batch array; JSON-RPC errors ride in the body (200) except a
	// non-parseable body (400 + -32700). Loopback-bound + token-gated by the same middleware.
	mux.HandleFunc("POST /api/rpc", rpcHTTPHandler(s.rpc, s.logger))

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
