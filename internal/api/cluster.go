package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/localhub/localhub/internal/cluster"
	"github.com/localhub/localhub/internal/core"
	"github.com/localhub/localhub/internal/jsonrpc"
)

// cluster.go hosts the THIN P7.1 cluster-plane handlers (docs/cluster-plan.md §5b, ADR-008).
// Same house style as the rest of internal/api: parse → one core/cluster call → marshal, with a
// mandatory human-readable reason on every error. All routes here are registered ONLY when
// cluster.role=coordinator (server.go routes()), so a role=off or node daemon has none of them —
// the /api/ catch-all returns the standard 404 envelope, which the regression test asserts.
//
// Owner directive that scoped this feature (verbatim, 2026-07-08):
//   "cluster means not necessarily k8s or anything like that simple over the network gpu clusters"

// handleClusterJoin serves POST /api/cluster/nodes/join — the idempotent join+heartbeat upsert
// (cluster-plan §2.4, §5b). It is cluster-token-gated ALWAYS when a token is configured, INDEPENDENT
// of the admin-token middleware and INDEPENDENT of the bind: even on loopback a configured cluster
// token is enforced (cluster-plan §4.2/§4.4). A wrong/missing token is a 401 with a reason naming
// the cluster token (token typos are the #1 setup failure — the reason must say which side rejected).
func (s *Server) handleClusterJoin(w http.ResponseWriter, r *http.Request) {
	if !s.clusterTokenOK(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized",
			"missing or invalid cluster token",
			"POST /api/cluster/nodes/join requires Authorization: Bearer <cluster.cluster_token>; "+
				"the coordinator rejected the presented token (check cluster_token matches on both boxes)")
		return
	}
	if !requireJSONBody(w, r) { // CSRF guard; the node join client (internal/cluster) declares application/json
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap — a heartbeat payload is tiny
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not read request body", err.Error())
		return
	}
	var req cluster.JoinRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"could not parse join request",
			"the body must be a JSON object with node_id, name, base_url (+ optional version, hw_summary). "+err.Error())
		return
	}
	node, err := s.clusterRegistry.Upsert(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"could not register node", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, node)
}

// handleClusterNodesList serves GET /api/cluster/nodes — the coordinator's registry with each
// node's status derived live from heartbeat age (cluster-plan §5b). Registered coordinator-only;
// the guard is defense-in-depth in case of a wiring mistake.
func (s *Server) handleClusterNodesList(w http.ResponseWriter, r *http.Request) {
	if s.clusterRegistry == nil {
		writeClusterNotCoordinator(w)
		return
	}
	nodes, err := s.clusterRegistry.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not list nodes", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, nodes)
}

// handleClusterNodeGet serves GET /api/cluster/nodes/{id} — a single registry entry (cluster-plan §5b).
func (s *Server) handleClusterNodeGet(w http.ResponseWriter, r *http.Request) {
	if s.clusterRegistry == nil {
		writeClusterNotCoordinator(w)
		return
	}
	node, err := s.clusterRegistry.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, cluster.ErrNodeNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such node",
			"no node with id "+r.PathValue("id")+" is registered")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not read node", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, node)
}

// handleClusterNodeRemove serves DELETE /api/cluster/nodes/{id} — forget a node from the registry
// (cluster-plan §5b). Does NOT touch the node itself; a live node simply rejoins on its next beat.
func (s *Server) handleClusterNodeRemove(w http.ResponseWriter, r *http.Request) {
	if s.clusterRegistry == nil {
		writeClusterNotCoordinator(w)
		return
	}
	err := s.clusterRegistry.Remove(r.Context(), r.PathValue("id"))
	if errors.Is(err, cluster.ErrNodeNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such node",
			"no node with id "+r.PathValue("id")+" is registered")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not remove node", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clusterTokenOK verifies the cluster bearer for the join endpoint. When no cluster token is
// configured (the loopback two-daemon dev rig may run tokenless, cluster-plan §4.2) the endpoint is
// open; when a token IS configured it is required and compared constant-time — enforced regardless
// of bind (the "gated ALWAYS" rule).
func (s *Server) clusterTokenOK(r *http.Request) bool {
	if s.clusterToken == "" {
		return true
	}
	got := []byte(r.Header.Get("Authorization"))
	return subtle.ConstantTimeCompare(got, []byte("Bearer "+s.clusterToken)) == 1
}

// writeClusterNotCoordinator is the response when a cluster route is hit on a non-coordinator
// daemon (defensive; these routes are not registered off-coordinator so this is rarely reached).
func writeClusterNotCoordinator(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found",
		"cluster coordinator role not enabled",
		"this daemon is not running as a cluster coordinator (set cluster.role=coordinator)")
}

// --- P7.2 aggregated views (cluster-plan §5b, §8 P7.2) ------------------------------------------
//
// The coordinator fans out concurrent authed GETs to each ONLINE node's local endpoints and returns
// node-attributed, partial-on-failure aggregates. The fan-out mechanics + the self-entry (the
// coordinator is also a node, cluster-plan §2.2) live in cluster.Aggregator; these handlers stay thin
// (call one aggregator method → marshal). A failed node NEVER fails the whole request — it becomes an
// explicit error entry (cluster-plan §7 failure-modes).

// newClusterAggregator builds the coordinator's fan-out aggregator, wiring the coordinator's OWN local
// data (Models/Hardware/V1Models) as self closures over the service core so self is aggregated without
// an HTTP round-trip. Kept here (not in cluster) so the cluster package stays free of core/api types —
// the same discipline as the JSON-RPC coreJobService shim (rpc_jobs.go).
func (s *Server) newClusterAggregator(nodeID, nodeName, token string, reg *cluster.Registry) *cluster.Aggregator {
	self := cluster.LocalProvider{
		NodeID:   nodeID,
		NodeName: nodeName,
		Models:   func(ctx context.Context) (any, error) { return s.svc.Models(ctx) },
		Hardware: func(ctx context.Context) (any, error) { return s.svc.Hardware(ctx) },
		V1Models: func(ctx context.Context) (any, error) {
			m, err := s.svc.OpenAIModels(ctx)
			if err != nil {
				return nil, err
			}
			if m == nil {
				m = []core.OpenAIModel{}
			}
			return openAIListResponse{Object: "list", Data: m}, nil
		},
	}
	return cluster.NewAggregator(reg, self, token, 0, s.logger)
}

// handleClusterModels serves GET /api/cluster/models → each node's inventory, node-attributed, with an
// explicit per-node error entry for any unreachable node (partial result, never a hang; cluster-plan §7).
func (s *Server) handleClusterModels(w http.ResponseWriter, r *http.Request) {
	if s.clusterAgg == nil {
		writeClusterNotCoordinator(w)
		return
	}
	writeJSON(w, http.StatusOK, s.clusterAgg.Models(r.Context()))
}

// handleClusterHardware serves GET /api/cluster/hardware → each node's hardware snapshot, attributed,
// same partial-result contract as /api/cluster/models.
func (s *Server) handleClusterHardware(w http.ResponseWriter, r *http.Request) {
	if s.clusterAgg == nil {
		writeClusterNotCoordinator(w)
		return
	}
	writeJSON(w, http.StatusOK, s.clusterAgg.Hardware(r.Context()))
}

// handleClusterV1Models serves GET /api/cluster/v1/models → the union-deduped OpenAI model list across
// all nodes (dedup by id; OpenAI shape stays pure — per-node detail lives in /api/cluster/models).
//
// DELIBERATE PATH CHOICE (task decision, and forced by the P7.2 territory that keeps proxy.go
// untouched): the coordinator's OWN /v1/models stays LOCAL-ONLY in P7.2. The union is served on this
// NEW path instead of promoting it to the bare /v1/models because P7.2 ships NO routing — a unioned
// /v1/models would advertise models the coordinator cannot yet serve (a request for a node-B-only
// model would hit the coordinator's local resolver and 404). P7.3 wires routed completions; the union
// is promoted to /v1/models THEN, when the coordinator can actually serve every advertised model.
// This differs from cluster-plan §5b (which lists /v1/models itself as the union) — recorded as a
// deviation in the P7.2 gate evidence; the plan's INTENT (a union OpenAI list at the coordinator) is
// met here without the premature-advertisement correctness bug.
func (s *Server) handleClusterV1Models(w http.ResponseWriter, r *http.Request) {
	if s.clusterAgg == nil {
		writeClusterNotCoordinator(w)
		return
	}
	writeJSON(w, http.StatusOK, s.clusterAgg.V1Models(r.Context()))
}

// clusterRPCService adapts the coordinator's registry + aggregator to jsonrpc.ClusterService so the
// cluster.* JSON-RPC mirrors (cluster.nodes.list / cluster.models.list / cluster.hardware.get) are
// thin one-liners over the SAME core as the REST routes (cluster-plan §5b). Returns nil (untyped, a
// genuine nil interface) off-coordinator so the mirrors are not registered — the RPC role-gating.
func (s *Server) clusterRPCService() jsonrpc.ClusterService {
	if s.clusterRole == "coordinator" && s.clusterAgg != nil && s.clusterRegistry != nil {
		return clusterRPC{reg: s.clusterRegistry, agg: s.clusterAgg}
	}
	return nil
}

// clusterRPC is the concrete jsonrpc.ClusterService shim (see clusterRPCService).
type clusterRPC struct {
	reg *cluster.Registry
	agg *cluster.Aggregator
}

func (c clusterRPC) NodesList(ctx context.Context) (any, error)   { return c.reg.List(ctx) }
func (c clusterRPC) ModelsList(ctx context.Context) (any, error)  { return c.agg.Models(ctx), nil }
func (c clusterRPC) HardwareGet(ctx context.Context) (any, error) { return c.agg.Hardware(ctx), nil }
