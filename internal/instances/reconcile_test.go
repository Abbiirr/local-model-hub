package instances

import (
	"context"
	"errors"
	"testing"
)

// fakeHealth scripts the health-checker result.
type fakeHealth struct {
	healthy    bool
	identityOK bool
	err        error
}

func (f fakeHealth) Check(ctx context.Context, inst Instance) (bool, bool, error) {
	return f.healthy, f.identityOK, f.err
}

// startInstance drives an instance to STARTING with a PID+port so the reconciler considers it.
func startInstance(t *testing.T, s *Store, pp *PortPool, id string, pid int) {
	t.Helper()
	ctx := context.Background()
	mustCreate(t, s, id)
	port, err := pp.Allocate(ctx, id)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	base := "http://127.0.0.1:" + itoa(port)
	if _, err := s.Transition(ctx, id, TransitionParams{
		To: StateStarting, Reason: "spawned", PID: &pid, Port: &port, BaseURL: &base,
	}); err != nil {
		t.Fatalf("STARTING: %v", err)
	}
	// Advance to HEALTHY (the pre-restart steady state) so reconcile decides re-attach vs reap.
	if _, err := s.Transition(ctx, id, TransitionParams{To: StateHealthy, Reason: "healthy"}); err != nil {
		t.Fatalf("HEALTHY: %v", err)
	}
}

// TestReconcile_AliveHealthyIdentity_Reattaches: PID alive + healthy + identity match → reattach.
func TestReconcile_AliveHealthyIdentity_Reattaches(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(int) (func() error, error) { return func() error { return nil }, nil }
	startInstance(t, s, pp, "i1", 4242)

	rec := NewReconciler(s, pp,
		fakeHealth{healthy: true, identityOK: true, err: nil},
		func(pid int) bool { return true }, // alive
		func(pid int) error { t.Fatalf("kill must NOT be called for a healthy reattach"); return nil },
	)
	res, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Reattached) != 1 || res.Reattached[0] != "i1" {
		t.Fatalf("expected i1 reattached, got %+v", res)
	}
	got, _ := s.Get(ctx, "i1")
	if got.State != StateHealthy {
		t.Fatalf("state = %s, want HEALTHY", got.State)
	}
}

// TestReconcile_AliveUnhealthy_Reaps: PID alive but /health fails → kill tree + FAILED.
func TestReconcile_AliveUnhealthy_Reaps(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(int) (func() error, error) { return func() error { return nil }, nil }
	startInstance(t, s, pp, "i1", 4242)

	killed := false
	rec := NewReconciler(s, pp,
		fakeHealth{healthy: false, identityOK: false, err: nil},
		func(pid int) bool { return true },
		func(pid int) error { killed = true; return nil },
	)
	res, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !killed {
		t.Error("expected kill-tree to be called for an unhealthy orphan")
	}
	if len(res.Reaped) != 1 {
		t.Fatalf("expected 1 reaped, got %+v", res)
	}
	got, _ := s.Get(ctx, "i1")
	if got.State != StateFailed {
		t.Fatalf("state = %s, want FAILED", got.State)
	}
	if got.Reason == "" || !contains(got.Reason, "orphan reaped at reconcile") {
		t.Errorf("FAILED reason should mention orphan reaping: %q", got.Reason)
	}
}

// TestReconcile_AliveWrongModel_Reaps: PID alive + healthy but identity mismatch → reap.
func TestReconcile_AliveWrongModel_Reaps(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(int) (func() error, error) { return func() error { return nil }, nil }
	startInstance(t, s, pp, "i1", 4242)

	killed := false
	rec := NewReconciler(s, pp,
		fakeHealth{healthy: true, identityOK: false, err: nil}, // healthy but wrong model
		func(pid int) bool { return true },
		func(pid int) error { killed = true; return nil },
	)
	res, _ := rec.Reconcile(ctx)
	if !killed || len(res.Reaped) != 1 {
		t.Fatalf("wrong-model process must be reaped: killed=%v res=%+v", killed, res)
	}
}

// TestReconcile_Dead_MarksStopped: PID dead → STOPPED, no kill.
func TestReconcile_Dead_MarksStopped(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(int) (func() error, error) { return func() error { return nil }, nil }
	startInstance(t, s, pp, "i1", 4242)

	rec := NewReconciler(s, pp,
		fakeHealth{err: errors.New("connection refused")},
		func(pid int) bool { return false }, // dead
		func(pid int) error { t.Fatalf("kill must NOT run for a dead process"); return nil },
	)
	res, _ := rec.Reconcile(ctx)
	if len(res.Stopped) != 1 {
		t.Fatalf("expected 1 stopped, got %+v", res)
	}
	got, _ := s.Get(ctx, "i1")
	if got.State != StateStopped {
		t.Fatalf("state = %s, want STOPPED", got.State)
	}
	if !contains(got.Reason, "process gone at reconcile") {
		t.Errorf("STOPPED reason should mention process gone: %q", got.Reason)
	}
	// Port freed → reusable.
	next, _ := pp.Allocate(ctx, "i2")
	if next != 40100 {
		t.Errorf("reaped port should be freed and reused; got %d", next)
	}
}

// TestReconcile_SkipsOllama: Ollama instances (PID 0) are not process-reconciled.
func TestReconcile_SkipsOllama(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	// Seed an ollama engine + instance.
	if _, err := d.Exec(`INSERT OR IGNORE INTO engines (name, installed) VALUES ('ollama', 1)`); err != nil {
		t.Fatalf("seed ollama engine: %v", err)
	}
	if _, err := s.Create(ctx, CreateParams{ID: "o1", ModelID: "m1", Engine: EngineOllama, Reason: "warm"}); err != nil {
		t.Fatalf("create ollama inst: %v", err)
	}
	_, _ = s.Transition(ctx, "o1", TransitionParams{To: StateStarting, Reason: "warming"})
	_, _ = s.Transition(ctx, "o1", TransitionParams{To: StateHealthy, Reason: "warm"})

	pp := NewPortPool(d.DB, 40100, 40105)
	rec := NewReconciler(s, pp,
		fakeHealth{},
		func(pid int) bool { t.Fatalf("alive-check must not run for ollama"); return false },
		func(pid int) error { return nil },
	)
	res, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Reattached)+len(res.Reaped)+len(res.Stopped) != 0 {
		t.Fatalf("ollama instance must be skipped, got %+v", res)
	}
	got, _ := s.Get(ctx, "o1")
	if got.State != StateHealthy {
		t.Errorf("ollama instance state should be untouched, got %s", got.State)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
