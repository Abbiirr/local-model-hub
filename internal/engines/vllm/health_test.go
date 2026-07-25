package vllm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/localhub/localhub/internal/instances"
)

// seedVLLMInstance creates a STARTING docker-mode vLLM instance pointing at baseURL/port.
func seedVLLMInstance(t *testing.T, store *instances.Store, id, baseURL string, port int) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.Create(ctx, instances.CreateParams{
		ID: id, ModelID: "vm", Engine: instances.EngineVLLM, BinaryPath: DefaultImage,
		Reason: "seed",
	}); err != nil {
		t.Fatal(err)
	}
	zero := 0
	if _, err := store.Transition(ctx, id, instances.TransitionParams{
		To: instances.StateStarting, Reason: "seed starting", PID: &zero, Port: &port, BaseURL: &baseURL,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDockerReconcile_ReattachesRunningHealthy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	store := vllmTestStore(t)
	seedVLLMInstance(t, store, "i1", srv.URL, portFromURL(srv.URL))
	fd := &fakeDocker{psOutput: "localhub-vllm-i1"} // container up

	rec := NewDockerReconciler(store, fd)
	res, err := rec.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Reattached) != 1 || res.Reattached[0] != "i1" {
		t.Fatalf("expected i1 reattached, got %+v", res)
	}
	inst, _ := store.Get(context.Background(), "i1")
	if inst.State != instances.StateHealthy {
		t.Errorf("state: got %q want HEALTHY", inst.State)
	}
}

func TestDockerReconcile_StopsWhenContainerGone(t *testing.T) {
	store := vllmTestStore(t)
	seedVLLMInstance(t, store, "i1", "http://127.0.0.1:40201", 40201)
	fd := &fakeDocker{psOutput: ""} // container NOT running

	rec := NewDockerReconciler(store, fd)
	res, err := rec.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Stopped) != 1 || res.Stopped[0] != "i1" {
		t.Fatalf("expected i1 stopped, got %+v", res)
	}
	inst, _ := store.Get(context.Background(), "i1")
	if inst.State != instances.StateStopped {
		t.Errorf("state: got %q want STOPPED", inst.State)
	}
}
