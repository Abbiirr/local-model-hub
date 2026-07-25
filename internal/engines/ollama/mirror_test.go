package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/localhub/localhub/internal/db"
	"github.com/localhub/localhub/internal/instances"
)

// mirrorTestStore opens a migrated temp DB and seeds two Ollama catalog models so the
// instances.model_id FK holds when the mirror adopts them.
func mirrorTestStore(t *testing.T) *instances.Store {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mirror.db")
	d, err := db.Open(p)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Ollama catalog rows keyed "ollama-<slug>" (inventory natkey scheme).
	for _, id := range []string{"ollama-mistral-latest", "ollama-llama3-2"} {
		if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format) VALUES (?, ?, 'ollama', 'gguf')`, id, id); err != nil {
			t.Fatalf("seed model %s: %v", id, err)
		}
	}
	return instances.NewStore(d.DB)
}

// resolveTag maps an Ollama tag to the "ollama-<slug>" catalog id, mimicking the inventory natkey.
func resolveTag(_ context.Context, tag string) (string, bool) {
	switch tag {
	case "mistral:latest":
		return "ollama-mistral-latest", true
	case "llama3.2", "llama3.2:latest":
		return "ollama-llama3-2", true
	}
	return "", false
}

func TestMirror_AdoptsResidentModel(t *testing.T) {
	f := newFakeDaemon()
	f.resident["mistral:latest"] = 5137025024 // ~4899 MiB
	srv := f.server(t)
	defer srv.Close()

	store := mirrorTestStore(t)
	m := NewMirror(New(srv.URL, ""), store, resolveTag)

	res, err := m.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Added) != 1 {
		t.Fatalf("expected 1 adopted instance, got %d (%v)", len(res.Added), res.Added)
	}
	// The adopted row is HEALTHY, engine=ollama, pid=0, with the model's VRAM recorded.
	inst, err := store.Get(context.Background(), res.Added[0])
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != instances.StateHealthy {
		t.Errorf("state: got %q want HEALTHY", inst.State)
	}
	if inst.Engine != instances.EngineOllama {
		t.Errorf("engine: got %q want ollama", inst.Engine)
	}
	if inst.PID != 0 || inst.Port != 0 {
		t.Errorf("ollama instance must have pid=0 port=0, got pid=%d port=%d", inst.PID, inst.Port)
	}
	if inst.ModelID != "ollama-mistral-latest" {
		t.Errorf("model_id: got %q want ollama-mistral-latest", inst.ModelID)
	}
	wantMiB := 5137025024 / (1024 * 1024)
	if inst.VRAMObsMiB != wantMiB {
		t.Errorf("vram_obs: got %d want %d", inst.VRAMObsMiB, wantMiB)
	}
}

func TestMirror_MarksStoppedWhenModelGone(t *testing.T) {
	f := newFakeDaemon()
	f.resident["mistral:latest"] = 5137025024
	srv := f.server(t)
	defer srv.Close()

	store := mirrorTestStore(t)
	m := NewMirror(New(srv.URL, ""), store, resolveTag)

	// First pass adopts it HEALTHY.
	if _, err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Model leaves the daemon (idle unload).
	f.mu.Lock()
	delete(f.resident, "mistral:latest")
	f.mu.Unlock()

	res, err := m.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync 2: %v", err)
	}
	if len(res.Stopped) != 1 {
		t.Fatalf("expected 1 stopped instance, got %d (%v)", len(res.Stopped), res.Stopped)
	}
	inst, err := store.Get(context.Background(), res.Stopped[0])
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != instances.StateStopped {
		t.Errorf("state: got %q want STOPPED", inst.State)
	}
}

func TestMirror_PromotesStartingToHealthy(t *testing.T) {
	f := newFakeDaemon()
	srv := f.server(t)
	defer srv.Close()
	store := mirrorTestStore(t)
	m := NewMirror(New(srv.URL, ""), store, resolveTag)
	ctx := context.Background()

	// Simulate a LocalHub-initiated warm: a STARTING row exists before /api/ps shows the model.
	id := "warm-1"
	if _, err := store.Create(ctx, instances.CreateParams{
		ID: id, ModelID: "ollama-llama3-2", Engine: instances.EngineOllama,
		Reason: "warm requested",
	}); err != nil {
		t.Fatal(err)
	}
	zero := 0
	base := srv.URL
	if _, err := store.Transition(ctx, id, instances.TransitionParams{
		To: instances.StateStarting, Reason: "warm in flight", PID: &zero, Port: &zero, BaseURL: &base,
	}); err != nil {
		t.Fatal(err)
	}

	// Now the model becomes resident.
	f.mu.Lock()
	f.resident["llama3.2"] = 800 * 1024 * 1024
	f.mu.Unlock()

	res, err := m.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Promoted) != 1 || res.Promoted[0] != id {
		t.Fatalf("expected instance %q promoted, got %v", id, res.Promoted)
	}
	inst, _ := store.Get(ctx, id)
	if inst.State != instances.StateHealthy {
		t.Errorf("state: got %q want HEALTHY", inst.State)
	}
}

func TestMirror_UnmappedResidentIsSkipped(t *testing.T) {
	f := newFakeDaemon()
	f.resident["unknown:tag"] = 1024 * 1024 * 1024
	srv := f.server(t)
	defer srv.Close()
	store := mirrorTestStore(t)
	m := NewMirror(New(srv.URL, ""), store, resolveTag)

	res, err := m.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Added) != 0 {
		t.Errorf("unmapped model must not be adopted (FK safety), got Added=%v", res.Added)
	}
}

func TestMirror_DaemonDown_NoChanges(t *testing.T) {
	closed := httptest.NewServer(http.NewServeMux())
	url := closed.URL
	closed.Close() // now unreachable
	store := mirrorTestStore(t)
	m := NewMirror(New(url, ""), store, resolveTag)
	_, err := m.Sync(context.Background())
	if err == nil {
		t.Fatal("expected an error when the daemon is unreachable")
	}
	var de *DaemonUnreachableError
	if !asDaemon(err, &de) {
		t.Fatalf("expected *DaemonUnreachableError, got %T", err)
	}
}
