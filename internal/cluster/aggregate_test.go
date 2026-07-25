package cluster

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeNode is an httptest server standing in for a real node's local API (cluster-plan §8: the
// two-daemon rig, here reduced to in-process fakes). It serves /api/models, /api/hardware and
// /v1/models, and asserts the coordinator sent the shared cluster bearer.
func fakeNode(t *testing.T, token string, models, hardware, v1 string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r, token)
		_, _ = io.WriteString(w, models)
	})
	mux.HandleFunc("/api/hardware", func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r, token)
		_, _ = io.WriteString(w, hardware)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r, token)
		_, _ = io.WriteString(w, v1)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func assertBearer(t *testing.T, r *http.Request, token string) {
	t.Helper()
	if token == "" {
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("%s %s: Authorization = %q, want the cluster bearer", r.Method, r.URL.Path, got)
	}
}

// aggTestRegistry returns a registry whose clock is fixed to now, so any node we Upsert reads back
// ONLINE (fresh last_seen) unless we deliberately backdate it.
func aggTestRegistry(t *testing.T, interval time.Duration) *Registry {
	t.Helper()
	reg, _ := testRegistry(t, interval)
	return reg
}

// TestAggregatorFanoutPartial is the P7.2 gate in miniature (TESTING.md#P72): two live fake nodes +
// one dead node produce a partial result — every live node's inventory attributed, the dead node an
// EXPLICIT error entry — with NO hang and NO whole-request failure. Includes the coordinator's own
// self entry (no HTTP). The dead node is a real listener we close, so its fan-out fails fast
// (connection refused) rather than waiting the full timeout; the timeout PATH itself is asserted
// separately (TestAggregatorTimeoutPath).
func TestAggregatorFanoutPartial(t *testing.T) {
	token := "shared-cluster-token"
	nodeA := fakeNode(t, token,
		`[{"id":"alpha","display_name":"alpha","presence":"present"}]`,
		`{"gpus":[{"name":"RTX 4060 Ti","vram_total":16380,"vram_free":15000}]}`,
		`{"object":"list","data":[{"id":"alpha","object":"model","owned_by":"localhub"}]}`)

	// A "dead" node: start a server to capture its URL, then close it so GETs are refused.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	reg := aggTestRegistry(t, time.Hour) // huge interval → every fresh heartbeat stays ONLINE
	ctx := context.Background()
	mustUpsert(t, reg, JoinRequest{NodeID: "a", Name: "node-a", BaseURL: nodeA.URL})
	mustUpsert(t, reg, JoinRequest{NodeID: "d", Name: "node-dead", BaseURL: deadURL})

	agg := NewAggregator(reg, LocalProvider{
		NodeID:   "self",
		NodeName: "coord",
		Models:   func(context.Context) (any, error) { return json.RawMessage(`[]`), nil },
	}, token, 2*time.Second, nil)

	done := make(chan []NodeModels, 1)
	go func() { done <- agg.Models(ctx) }()
	var got []NodeModels
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("aggregate hung — partial-result contract violated (a dead node must never block)")
	}

	by := map[string]NodeModels{}
	for _, e := range got {
		by[e.NodeName] = e
	}
	if len(got) != 3 {
		t.Fatalf("entries = %d (%v), want 3 (self + node-a + node-dead)", len(got), by)
	}
	if e := by["node-a"]; e.Error != "" || len(e.Models) == 0 {
		t.Errorf("node-a should carry inventory, no error; got err=%q models=%s", e.Error, e.Models)
	}
	if e := by["node-dead"]; e.Error == "" {
		t.Errorf("node-dead must carry an explicit per-node error entry, got none (models=%s)", e.Models)
	}
	if e := by["coord"]; e.Error != "" || string(e.Models) != "[]" {
		t.Errorf("self entry should be the local body with no HTTP; got err=%q models=%s", e.Error, e.Models)
	}
}

// TestAggregatorTimeoutPath asserts the per-node TIMEOUT path explicitly: a node that accepts the
// connection but never responds must yield an error entry within ~the per-node timeout, not hang the
// aggregate (cluster-plan §7). We use a 200ms timeout against a node that sleeps 5s.
func TestAggregatorTimeoutPath(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	t.Cleanup(slow.Close)

	reg := aggTestRegistry(t, time.Hour)
	mustUpsert(t, reg, JoinRequest{NodeID: "s", Name: "node-slow", BaseURL: slow.URL})
	agg := NewAggregator(reg, LocalProvider{}, "", 200*time.Millisecond, nil)

	start := time.Now()
	got := agg.Hardware(context.Background())
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("aggregate took %s — per-node timeout (200ms) did not bound the slow node", elapsed)
	}
	if len(got) != 1 || got[0].NodeName != "node-slow" || got[0].Error == "" {
		t.Fatalf("want one node-slow entry with a timeout error, got %+v", got)
	}
}

// TestAggregatorV1ModelsUnionDedupe proves the union-deduped OpenAI list: a model present on two
// nodes appears ONCE; the OpenAI shape stays pure (no node annotation); a failed node is silently
// omitted from the union (its detail lives in /api/cluster/models).
func TestAggregatorV1ModelsUnionDedupe(t *testing.T) {
	token := ""
	nodeA := fakeNode(t, token, `[]`, `{}`,
		`{"object":"list","data":[{"id":"shared","object":"model"},{"id":"alpha","object":"model"}]}`)
	nodeB := fakeNode(t, token, `[]`, `{}`,
		`{"object":"list","data":[{"id":"shared","object":"model"},{"id":"beta","object":"model"}]}`)

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	reg := aggTestRegistry(t, time.Hour)
	mustUpsert(t, reg, JoinRequest{NodeID: "a", Name: "node-a", BaseURL: nodeA.URL})
	mustUpsert(t, reg, JoinRequest{NodeID: "b", Name: "node-b", BaseURL: nodeB.URL})
	mustUpsert(t, reg, JoinRequest{NodeID: "d", Name: "node-dead", BaseURL: deadURL})

	agg := NewAggregator(reg, LocalProvider{
		NodeID:   "self",
		NodeName: "coord",
		V1Models: func(context.Context) (any, error) {
			return json.RawMessage(`{"object":"list","data":[{"id":"selfmodel","object":"model"}]}`), nil
		},
	}, token, 2*time.Second, nil)

	list := agg.V1Models(context.Background())
	if list.Object != "list" {
		t.Errorf("object = %q, want list", list.Object)
	}
	ids := map[string]int{}
	for _, m := range list.Data {
		ids[m.ID]++
	}
	// shared appears once (dedupe), alpha/beta/selfmodel each once, dead node omitted (no error id).
	want := []string{"alpha", "beta", "selfmodel", "shared"}
	if len(list.Data) != len(want) {
		t.Fatalf("union size = %d (%v), want %d (%v)", len(list.Data), list.Data, len(want), want)
	}
	for _, id := range want {
		if ids[id] != 1 {
			t.Errorf("id %q appears %d times, want exactly once (union-dedupe)", id, ids[id])
		}
	}
	// Data sorted by id for a stable response.
	for i := 1; i < len(list.Data); i++ {
		if list.Data[i-1].ID > list.Data[i].ID {
			t.Errorf("union not sorted by id: %v", list.Data)
			break
		}
	}
}

// TestHWSummaryRoundTrip proves the hw_summary contract: a node's compact HWSummary marshals, rides
// in JoinRequest.HWSummary, is stored VERBATIM by the coordinator, and reads back byte-identical and
// re-parseable (the coordinator never interprets it — ADR-008, one-core-per-node).
func TestHWSummaryRoundTrip(t *testing.T) {
	reg := aggTestRegistry(t, time.Hour)
	ctx := context.Background()

	hs := HWSummary{
		GPUs:        []HWGPU{{Name: "RTX 4060 Ti", VRAMFreeMiB: 15000, VRAMTotalMiB: 16380}},
		RAMFreeMiB:  20000,
		RAMTotalMiB: 65536,
		CapturedAt:  "2026-07-08T04:00:00Z",
	}
	raw := hs.JSON()
	if raw == "" {
		t.Fatal("HWSummary.JSON() returned empty")
	}

	n, err := reg.Upsert(ctx, JoinRequest{NodeID: "n1", Name: "box-b", BaseURL: "http://127.0.0.1:39922", HWSummary: raw})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if n.HWSummary != raw {
		t.Fatalf("stored hw_summary not verbatim:\n got %q\nwant %q", n.HWSummary, raw)
	}
	// And the coordinator's List returns it identically, still re-parseable to the same struct.
	nodes, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nodes) != 1 || nodes[0].HWSummary != raw {
		t.Fatalf("list hw_summary mismatch: %+v", nodes)
	}
	var back HWSummary
	if err := json.Unmarshal([]byte(nodes[0].HWSummary), &back); err != nil {
		t.Fatalf("stored hw_summary not valid JSON: %v", err)
	}
	if len(back.GPUs) != 1 || back.GPUs[0].Name != "RTX 4060 Ti" || back.GPUs[0].VRAMFreeMiB != 15000 || back.RAMFreeMiB != 20000 {
		t.Errorf("round-tripped HWSummary lost fields: %+v", back)
	}
}

func mustUpsert(t *testing.T, reg *Registry, req JoinRequest) {
	t.Helper()
	if _, err := reg.Upsert(context.Background(), req); err != nil {
		t.Fatalf("upsert %s: %v", req.NodeID, err)
	}
}
