package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/cluster"
	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/db"
)

// coordinatorServer builds a Server wired as a cluster coordinator over a scratch DB, with the
// given cluster token. Mirrors the role=coordinator path in main.go without a real listener.
func coordinatorServer(t *testing.T, token string) *Server {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := newTestService(t)
	s := &Server{svc: svc, logger: quietLogger()}
	WithCluster("coordinator", token, "coord-self-id", "coord-a", cluster.NewRegistry(d.DB, 10*time.Second))(s)
	return s
}

// TestClusterRoleOff_NoRoutes is the role-off regression gate (TESTING.md#p71): a daemon with
// clustering off must not register any /api/cluster/* route — the request falls through to the
// /api/ catch-all and returns the standard JSON 404 envelope, byte-identical to 1.0.
func TestClusterRoleOff_NoRoutes(t *testing.T) {
	s := &Server{svc: newTestService(t), logger: quietLogger()} // zero cluster fields => role off
	mux := http.NewServeMux()
	s.routes(mux)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/cluster/nodes"},
		{http.MethodPost, "/api/cluster/nodes/join"},
		{http.MethodGet, "/api/cluster/nodes/abc"},
		{http.MethodDelete, "/api/cluster/nodes/abc"},
		// P7.2 aggregated views must also be absent when role=off (no new routes leak).
		{http.MethodGet, "/api/cluster/models"},
		{http.MethodGet, "/api/cluster/hardware"},
		{http.MethodGet, "/api/cluster/v1/models"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404 (no cluster routes when role=off)", tc.method, tc.path, rec.Code)
		}
		var env ErrorEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Errorf("%s %s: body is not a JSON error envelope: %v", tc.method, tc.path, err)
			continue
		}
		if env.Error.Code != "not_found" || env.Error.Reason == "" {
			t.Errorf("%s %s: envelope = %+v, want code=not_found with a reason", tc.method, tc.path, env.Error)
		}
	}
}

// TestClusterRoleNode_NoCoordinatorRoutes proves a NODE-role daemon registers no coordinator routes
// (P7.2 extends the P7.1 regression pattern): only the coordinator serves /api/cluster/* aggregates.
// A node carries the cluster token (for middleware acceptance) but has no registry/aggregator, so
// routes() registers nothing under /api/cluster/ and the catch-all 404s.
func TestClusterRoleNode_NoCoordinatorRoutes(t *testing.T) {
	s := &Server{svc: newTestService(t), logger: quietLogger()}
	WithCluster("node", "shared-token", "node-self-id", "node-b", nil)(s)
	if s.clusterRole != "node" {
		t.Fatalf("clusterRole = %q, want node", s.clusterRole)
	}
	mux := http.NewServeMux()
	s.routes(mux)
	for _, path := range []string{
		"/api/cluster/nodes", "/api/cluster/models", "/api/cluster/hardware", "/api/cluster/v1/models",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s on node role: status = %d, want 404 (coordinator routes absent)", path, rec.Code)
		}
	}
	// The RPC surface must likewise expose no cluster.* mirrors on a node.
	if svc := s.clusterRPCService(); svc != nil {
		t.Errorf("clusterRPCService() = %v on node role, want nil (no cluster.* RPC mirrors)", svc)
	}
}

// TestClusterAggregatedModelsSelfEntry exercises the coordinator aggregate happy path with NO
// registered nodes: the response still contains exactly the coordinator's OWN node entry (it is also
// a node, cluster-plan §2.2), attributed and error-free — proving the self-without-HTTP path.
func TestClusterAggregatedModelsSelfEntry(t *testing.T) {
	s := coordinatorServer(t, "")
	mux := http.NewServeMux()
	s.routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/cluster/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cluster/models status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var entries []cluster.NodeModels
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("cluster/models body: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want exactly the self entry", len(entries))
	}
	if entries[0].NodeID != "coord-self-id" || entries[0].NodeName != "coord-a" {
		t.Errorf("self entry = {%s,%s}, want {coord-self-id,coord-a}", entries[0].NodeID, entries[0].NodeName)
	}
	if entries[0].Error != "" {
		t.Errorf("self entry carries an error %q; local read must not fail", entries[0].Error)
	}
	if entries[0].Models == nil {
		t.Error("self entry has no models payload (want the local /api/models body, even if empty [])")
	}
}

// TestClusterConfigDefaultOff pins the default so the regression above is meaningful.
func TestClusterConfigDefaultOff(t *testing.T) {
	if role := config.Default().Cluster.Role; role != "off" {
		t.Fatalf("default cluster.role = %q, want off", role)
	}
}

// TestClusterJoinAndList exercises the coordinator happy path: a join upsert appears in the list
// as ONLINE with the reported base_url/version.
func TestClusterJoinAndList(t *testing.T) {
	s := coordinatorServer(t, "") // tokenless (loopback dev rig) — join is open
	mux := http.NewServeMux()
	s.routes(mux)

	body, _ := json.Marshal(cluster.JoinRequest{NodeID: "n1", Name: "box-b", BaseURL: "http://127.0.0.1:39922", Version: "0.1.0-dev"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, jsonReq(http.MethodPost, "/api/cluster/nodes/join", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("join status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/cluster/nodes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}
	var nodes []cluster.Node
	if err := json.Unmarshal(rec.Body.Bytes(), &nodes); err != nil {
		t.Fatalf("list body: %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != "n1" || nodes[0].Status != cluster.StatusOnline {
		t.Fatalf("list = %+v, want one ONLINE node n1", nodes)
	}
	if nodes[0].BaseURL != "http://127.0.0.1:39922" {
		t.Errorf("base_url = %q, want the reported value", nodes[0].BaseURL)
	}
}

// TestClusterJoinTokenGating proves the join endpoint is cluster-token-gated ALWAYS when a token is
// configured (even on loopback, no admin middleware): wrong/missing token => 401 with a reason;
// correct token => 200.
func TestClusterJoinTokenGating(t *testing.T) {
	s := coordinatorServer(t, "secret-cluster-token")
	mux := http.NewServeMux()
	s.routes(mux)
	body, _ := json.Marshal(cluster.JoinRequest{NodeID: "n1", Name: "b", BaseURL: "http://b"})

	// Missing token.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cluster/nodes/join", bytes.NewReader(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-token join = %d, want 401", rec.Code)
	}
	var env ErrorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "unauthorized" || env.Error.Reason == "" {
		t.Errorf("401 envelope = %+v, want unauthorized with reason", env.Error)
	}

	// Wrong token.
	req := httptest.NewRequest(http.MethodPost, "/api/cluster/nodes/join", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-token join = %d, want 401", rec.Code)
	}

	// Correct token.
	req = jsonReq(http.MethodPost, "/api/cluster/nodes/join", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-cluster-token")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct-token join = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
