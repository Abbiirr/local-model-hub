package instances

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/localhub/localhub/internal/db"
)

// openTestStore opens a migrated temp DB, seeds the FK parents (a model + engine row), and
// returns a Store over it.
func openTestStore(t *testing.T) (*db.DB, *Store) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "inst.db")
	d, err := db.Open(p)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// FK parents: instances.model_id → models(id), instances.engine → engines(name).
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format) VALUES ('m1','Model 1','local_path','gguf')`); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO engines (name, installed) VALUES ('llamacpp', 1)`); err != nil {
		t.Fatalf("seed engine: %v", err)
	}
	return d, NewStore(d.DB)
}

func mustCreate(t *testing.T, s *Store, id string) Instance {
	t.Helper()
	inst, err := s.Create(context.Background(), CreateParams{
		ID: id, ModelID: "m1", Engine: EngineLlamaCPP, ModelPath: `K:\models\m1.gguf`,
		BinaryPath: `K:\llama-server.exe`, Ctx: 4096, VRAMEstMiB: 900,
		Reason: "test create",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return inst
}

func TestStore_CreateRecordsRequested(t *testing.T) {
	_, s := openTestStore(t)
	ctx := context.Background()
	inst := mustCreate(t, s, "i1")
	if inst.State != StateRequested {
		t.Fatalf("state = %s, want REQUESTED", inst.State)
	}
	trans, err := s.Transitions(ctx, "i1")
	if err != nil {
		t.Fatalf("Transitions: %v", err)
	}
	if len(trans) != 1 || trans[0].To != StateRequested || trans[0].From != "" {
		t.Fatalf("expected one transition ''→REQUESTED, got %+v", trans)
	}
	if trans[0].Reason == "" {
		t.Error("transition must carry a reason")
	}
}

func TestStore_LegalTransitionPersists(t *testing.T) {
	_, s := openTestStore(t)
	ctx := context.Background()
	mustCreate(t, s, "i1")

	pid, port := 4242, 40100
	base := "http://127.0.0.1:40100"
	if _, err := s.Transition(ctx, "i1", TransitionParams{
		To: StateStarting, Reason: "spawned", PID: &pid, Port: &port, BaseURL: &base,
		LaunchFlags: []string{"llama-server", "-m", "x.gguf"},
	}); err != nil {
		t.Fatalf("STARTING: %v", err)
	}
	got, _ := s.Get(ctx, "i1")
	if got.State != StateStarting || got.PID != pid || got.Port != port || got.BaseURL != base {
		t.Fatalf("STARTING row wrong: %+v", got)
	}
	if len(got.LaunchFlags) != 3 {
		t.Fatalf("launch_flags_json not persisted: %v", got.LaunchFlags)
	}

	if _, err := s.Transition(ctx, "i1", TransitionParams{To: StateHealthy, Reason: "health ok"}); err != nil {
		t.Fatalf("HEALTHY: %v", err)
	}
	got, _ = s.Get(ctx, "i1")
	if got.StartedAt.IsZero() {
		t.Error("HEALTHY must stamp started_at")
	}

	freed := 850
	if _, err := s.Transition(ctx, "i1", TransitionParams{To: StateStopped, Reason: "verified", VRAMFreedMiB: &freed}); err != nil {
		t.Fatalf("STOPPED: %v", err)
	}
	got, _ = s.Get(ctx, "i1")
	if got.State != StateStopped || got.VRAMFreedMiB != 850 || got.StoppedAt.IsZero() {
		t.Fatalf("STOPPED row wrong: %+v", got)
	}

	// Full audit chain persisted.
	trans, _ := s.Transitions(ctx, "i1")
	if len(trans) != 4 {
		t.Fatalf("expected 4 transitions, got %d: %+v", len(trans), trans)
	}
}

func TestStore_IllegalTransitionRejectedAndNotPersisted(t *testing.T) {
	_, s := openTestStore(t)
	ctx := context.Background()
	mustCreate(t, s, "i1") // REQUESTED

	// REQUESTED → HEALTHY is illegal (must go via STARTING).
	_, err := s.Transition(ctx, "i1", TransitionParams{To: StateHealthy, Reason: "illegal skip"})
	var ill *IllegalTransitionError
	if !errors.As(err, &ill) {
		t.Fatalf("expected *IllegalTransitionError, got %v", err)
	}
	if ill.From != StateRequested || ill.To != StateHealthy {
		t.Fatalf("wrong error detail: %+v", ill)
	}
	// State unchanged; no extra transition recorded.
	got, _ := s.Get(ctx, "i1")
	if got.State != StateRequested {
		t.Fatalf("state changed on illegal transition: %s", got.State)
	}
	trans, _ := s.Transitions(ctx, "i1")
	if len(trans) != 1 {
		t.Fatalf("illegal transition must persist nothing; got %d transitions", len(trans))
	}
}

func TestStore_TerminalIsFinal(t *testing.T) {
	_, s := openTestStore(t)
	ctx := context.Background()
	mustCreate(t, s, "i1")
	_, _ = s.Transition(ctx, "i1", TransitionParams{To: StateFailed, Reason: "boom"})
	// FAILED → anything is illegal.
	for _, to := range []State{StateStarting, StateHealthy, StateStopped} {
		if _, err := s.Transition(ctx, "i1", TransitionParams{To: to, Reason: "x"}); err == nil {
			t.Errorf("FAILED → %s should be illegal", to)
		}
	}
}

func TestStore_ReasonRequired(t *testing.T) {
	_, s := openTestStore(t)
	ctx := context.Background()
	mustCreate(t, s, "i1")
	if _, err := s.Transition(ctx, "i1", TransitionParams{To: StateStarting, Reason: ""}); err == nil {
		t.Error("empty reason must be rejected")
	}
}

func TestStore_ListActiveExcludesTerminal(t *testing.T) {
	_, s := openTestStore(t)
	ctx := context.Background()
	mustCreate(t, s, "i1")
	mustCreate(t, s, "i2")
	_, _ = s.Transition(ctx, "i2", TransitionParams{To: StateFailed, Reason: "done"})

	active, _ := s.List(ctx, true)
	if len(active) != 1 || active[0].ID != "i1" {
		t.Fatalf("active list wrong: %+v", active)
	}
	all, _ := s.List(ctx, false)
	if len(all) != 2 {
		t.Fatalf("full list should have 2, got %d", len(all))
	}
}
