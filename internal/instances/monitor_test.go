package instances

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// M5 gate tests for the POST-HEALTHY monitor. The behaviours pinned here are exactly the ones the
// plan records as UNVERIFIED: a post-HEALTHY death, an unresponsive-but-alive engine, and the
// guarantee that neither a pinned model nor one with an active stream is ever killed.
//
// Helpers reused from reconcile_test.go: fakeHealth, startInstance, mustCreate, openTestStore, itoa.

// newMonitor builds a Monitor with injected alive/kill seams (the real ones touch OS processes) and
// a frozen clock so last_health_at is deterministic.
func newMonitor(t *testing.T, s *Store, pp *PortPool, health HealthChecker,
	alive AliveChecker, kill TreeKiller, activeCount func(string) int, reapAfter int) *Monitor {
	t.Helper()
	m := NewMonitor(s, pp, health, activeCount, MonitorConfig{ReapAfterFailures: reapAfter})
	m.alive = alive
	m.kill = kill
	m.now = func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) }
	return m
}

// startPinnedInstance drives a PINNED instance to HEALTHY. Pinning is set at Create time (there is
// no setter), so this cannot reuse startInstance/mustCreate.
func startPinnedInstance(t *testing.T, s *Store, pp *PortPool, id string, pid int) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Create(ctx, CreateParams{
		ID: id, ModelID: "m1", Engine: EngineLlamaCPP, ModelPath: `K:\models\m1.gguf`,
		BinaryPath: `K:\llama-server.exe`, Ctx: 4096, VRAMEstMiB: 900,
		Pinned: true, Reason: "test create (pinned)",
	}); err != nil {
		t.Fatalf("Create pinned: %v", err)
	}
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
	if _, err := s.Transition(ctx, id, TransitionParams{To: StateHealthy, Reason: "healthy"}); err != nil {
		t.Fatalf("HEALTHY: %v", err)
	}
}

// TestMonitor_PostHealthyDeath_IsFailedNotStopped is THE M5.1/M5.2 gate. An engine that vanishes
// after reaching HEALTHY must become FAILED — never a clean STOPPED, which would claim a graceful
// shutdown and a verified VRAM reclaim that never happened.
func TestMonitor_PostHealthyDeath_IsFailedNotStopped(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(int) (func() error, error) { return func() error { return nil }, nil }
	startInstance(t, s, pp, "i1", 4242)

	m := newMonitor(t, s, pp,
		fakeHealth{healthy: true, identityOK: true},
		func(pid int) bool { return false }, // PID gone
		func(pid int) error { t.Fatalf("kill must NOT be called: the process is already gone"); return nil },
		nil, DefaultReapAfterFailures)

	res, err := m.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(res.Died) != 1 || res.Died[0] != "i1" {
		t.Fatalf("expected i1 in Died, got %+v", res)
	}
	got, _ := s.Get(ctx, "i1")
	if got.State != StateFailed {
		t.Fatalf("state = %s, want FAILED (a vanished engine is a failure, not a clean stop)", got.State)
	}
	// The reason must say it was NOT a requested stop and that reclaim was not verified.
	low := strings.ToLower(got.Reason)
	for _, want := range []string{"exited", "after reaching healthy", "not a requested stop", "not verified"} {
		if !strings.Contains(low, want) {
			t.Errorf("reason must contain %q; got %q", want, got.Reason)
		}
	}
	// M5.5: machine-readable — a caller must not have to parse prose to learn reclaim was unproven.
	if got.ReclaimVerified == nil || *got.ReclaimVerified {
		t.Errorf("reclaim_verified must be explicitly FALSE after an unobserved death; got %v", got.ReclaimVerified)
	}
}

// TestMonitor_HealthyProbe_StampsLastHealth: a successful probe records WHEN, so "HEALTHY" can be
// distinguished from "HEALTHY, last confirmed an hour ago", and clears any failure count.
func TestMonitor_HealthyProbe_StampsLastHealth(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(int) (func() error, error) { return func() error { return nil }, nil }
	startInstance(t, s, pp, "i1", 4242)

	// Pre-load a failure so we can prove a success RESETS it.
	if _, err := s.RecordHealthFailure(ctx, "i1"); err != nil {
		t.Fatalf("seed failure: %v", err)
	}

	m := newMonitor(t, s, pp,
		fakeHealth{healthy: true, identityOK: true},
		func(pid int) bool { return true },
		func(pid int) error { t.Fatalf("kill must NOT be called for a healthy instance"); return nil },
		nil, DefaultReapAfterFailures)

	if _, err := m.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}
	got, _ := s.Get(ctx, "i1")
	if got.State != StateHealthy {
		t.Fatalf("state = %s, want HEALTHY", got.State)
	}
	if got.LastHealthAt.IsZero() {
		t.Error("last_health_at must be stamped on a successful probe")
	}
	if got.HealthFailures != 0 {
		t.Errorf("health_failures = %d, want 0 after a success", got.HealthFailures)
	}
}

// TestMonitor_UnresponsiveBelowThreshold_NotKilled: llama-server's /health can fail transiently
// under load, so a single miss must NOT kill anything. The failure is recorded and surfaced.
func TestMonitor_UnresponsiveBelowThreshold_NotKilled(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(int) (func() error, error) { return func() error { return nil }, nil }
	startInstance(t, s, pp, "i1", 4242)

	m := newMonitor(t, s, pp,
		fakeHealth{healthy: false, err: errors.New("connection refused")},
		func(pid int) bool { return true },
		func(pid int) error { t.Fatalf("kill must NOT be called below the reap threshold"); return nil },
		nil, 3)

	// Two passes: still below the threshold of 3.
	for i := 0; i < 2; i++ {
		res, err := m.Check(ctx)
		if err != nil {
			t.Fatalf("Check %d: %v", i, err)
		}
		if len(res.Degraded) != 1 {
			t.Fatalf("pass %d: expected i1 degraded, got %+v", i, res)
		}
	}
	got, _ := s.Get(ctx, "i1")
	if got.State != StateHealthy {
		t.Fatalf("state = %s, want HEALTHY (below threshold: recorded, not reaped)", got.State)
	}
	if got.HealthFailures != 2 {
		t.Errorf("health_failures = %d, want 2 (the count must persist across passes)", got.HealthFailures)
	}
}

// TestMonitor_UnresponsiveAtThreshold_Reaped: after ReapAfterFailures CONSECUTIVE failures the tree
// is killed and the row becomes FAILED with the count in the reason.
func TestMonitor_UnresponsiveAtThreshold_Reaped(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(int) (func() error, error) { return func() error { return nil }, nil }
	startInstance(t, s, pp, "i1", 4242)

	killed := 0
	m := newMonitor(t, s, pp,
		fakeHealth{healthy: false},
		func(pid int) bool { return true },
		func(pid int) error { killed++; return nil },
		nil, 3)

	var res MonitorResult
	for i := 0; i < 3; i++ {
		var err error
		if res, err = m.Check(ctx); err != nil {
			t.Fatalf("Check %d: %v", i, err)
		}
	}
	if len(res.Reaped) != 1 || res.Reaped[0] != "i1" {
		t.Fatalf("expected i1 reaped on the third failure, got %+v", res)
	}
	if killed != 1 {
		t.Errorf("kill called %d times, want exactly 1", killed)
	}
	got, _ := s.Get(ctx, "i1")
	if got.State != StateFailed {
		t.Fatalf("state = %s, want FAILED", got.State)
	}
	if !strings.Contains(got.Reason, "3 consecutive") {
		t.Errorf("reason must state the consecutive-failure count; got %q", got.Reason)
	}
	if got.ReclaimVerified == nil || *got.ReclaimVerified {
		t.Errorf("reclaim_verified must be false after a reap (no NVML delta was measured); got %v", got.ReclaimVerified)
	}
}

// TestMonitor_HealthySuccessResetsCounter: the threshold counts CONSECUTIVE failures, so a recovery
// in the middle must reset it — otherwise an instance that blips once an hour would eventually be
// killed while perfectly healthy.
func TestMonitor_HealthySuccessResetsCounter(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(int) (func() error, error) { return func() error { return nil }, nil }
	startInstance(t, s, pp, "i1", 4242)

	failing := fakeHealth{healthy: false}
	ok := fakeHealth{healthy: true, identityOK: true}
	alive := func(pid int) bool { return true }
	noKill := func(pid int) error { t.Fatalf("kill must NOT be called: failures were not consecutive"); return nil }

	// fail, fail, RECOVER, fail, fail → never 3 in a row, so never reaped.
	for _, h := range []HealthChecker{failing, failing, ok, failing, failing} {
		m := newMonitor(t, s, pp, h, alive, noKill, nil, 3)
		if _, err := m.Check(ctx); err != nil {
			t.Fatalf("Check: %v", err)
		}
	}
	got, _ := s.Get(ctx, "i1")
	if got.State != StateHealthy {
		t.Fatalf("state = %s, want HEALTHY", got.State)
	}
	if got.HealthFailures != 2 {
		t.Errorf("health_failures = %d, want 2 (reset by the success, then two more)", got.HealthFailures)
	}
}

// TestMonitor_NeverReapsPinned: pinning means "this must stay up". The live Qwen3.6-27B on the
// reference box is pinned, so this is the direct guarantee that a probe blip cannot kill it.
func TestMonitor_NeverReapsPinned(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(int) (func() error, error) { return func() error { return nil }, nil }
	startPinnedInstance(t, s, pp, "i1", 4242)

	m := newMonitor(t, s, pp,
		fakeHealth{healthy: false},
		func(pid int) bool { return true },
		func(pid int) error { t.Fatalf("a PINNED instance must NEVER be reaped"); return nil },
		nil, 1) // threshold 1: would reap immediately if pinning were not honoured

	for i := 0; i < 3; i++ {
		res, err := m.Check(ctx)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if len(res.Skipped) != 1 {
			t.Fatalf("pinned instance must be reported as skipped, got %+v", res)
		}
	}
	got, _ := s.Get(ctx, "i1")
	if got.State != StateHealthy {
		t.Fatalf("state = %s, want HEALTHY (pinned is never reaped)", got.State)
	}
	// The failures are still COUNTED, so the problem stays visible even though we refuse to act.
	if got.HealthFailures != 3 {
		t.Errorf("health_failures = %d, want 3 (a protected instance is still reported)", got.HealthFailures)
	}
}

// TestMonitor_NeverReapsActiveStream is the never-kill-active rule (CLAUDE.md) applied to the
// monitor: a busy llama-server may be slow to answer /health precisely BECAUSE it is generating.
// Killing it would destroy the very work the rule protects. This is the "stop/monitor during an
// active stream" case the plan lists as untested.
func TestMonitor_NeverReapsActiveStream(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(int) (func() error, error) { return func() error { return nil }, nil }
	startInstance(t, s, pp, "i1", 4242)

	inFlight := 1 // one active stream
	m := newMonitor(t, s, pp,
		fakeHealth{healthy: false, err: errors.New("timeout: busy generating")},
		func(pid int) bool { return true },
		func(pid int) error { t.Fatalf("must NEVER kill an instance with in-flight requests"); return nil },
		func(string) int { return inFlight }, 1)

	for i := 0; i < 3; i++ {
		res, err := m.Check(ctx)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if len(res.Skipped) != 1 {
			t.Fatalf("busy instance must be skipped, got %+v", res)
		}
	}
	if got, _ := s.Get(ctx, "i1"); got.State != StateHealthy {
		t.Fatalf("state = %s, want HEALTHY while a stream is active", got.State)
	}

	// Once the stream finishes, the SAME failing health DOES lead to a reap — the protection is
	// scoped to actual activity, not a permanent exemption.
	inFlight = 0
	killed := false
	m2 := newMonitor(t, s, pp,
		fakeHealth{healthy: false},
		func(pid int) bool { return true },
		func(pid int) error { killed = true; return nil },
		func(string) int { return inFlight }, 1)
	if _, err := m2.Check(ctx); err != nil {
		t.Fatalf("Check after stream: %v", err)
	}
	if !killed {
		t.Error("once idle, a persistently unresponsive instance must be reaped")
	}
	if got, _ := s.Get(ctx, "i1"); got.State != StateFailed {
		t.Errorf("state = %s, want FAILED once idle and still unresponsive", got.State)
	}
}

// TestMonitor_ReapDisabled: ReapAfterFailures=0 turns off killing entirely, but the PID-gone path
// must STILL mark FAILED — a dead process is a fact, not a policy decision.
func TestMonitor_ReapDisabled(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(int) (func() error, error) { return func() error { return nil }, nil }
	startInstance(t, s, pp, "alive1", 4242)
	startInstance(t, s, pp, "dead1", 4343)

	m := newMonitor(t, s, pp,
		fakeHealth{healthy: false},
		func(pid int) bool { return pid == 4242 }, // only alive1 is alive
		func(pid int) error { t.Fatalf("reaping is disabled; kill must not be called"); return nil },
		nil, 0)

	res, err := m.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(res.Reaped) != 0 {
		t.Errorf("reaping disabled, got Reaped=%v", res.Reaped)
	}
	if len(res.Died) != 1 || res.Died[0] != "dead1" {
		t.Errorf("the PID-gone path must still fire with reaping disabled; got %+v", res)
	}
	if got, _ := s.Get(ctx, "alive1"); got.State != StateHealthy {
		t.Errorf("alive1 state = %s, want HEALTHY (unresponsive but reaping disabled)", got.State)
	}
	if got, _ := s.Get(ctx, "dead1"); got.State != StateFailed {
		t.Errorf("dead1 state = %s, want FAILED", got.State)
	}
}

// TestMonitor_SkipsNonHealthyStates: STARTING is the start path's business (a 13.7 GB model takes
// ~43 s to load) and DRAINING means a stop is already in flight. Touching either would race.
func TestMonitor_SkipsNonHealthyStates(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(int) (func() error, error) { return func() error { return nil }, nil }

	// STARTING only (never advanced to HEALTHY).
	mustCreate(t, s, "starting1")
	port, err := pp.Allocate(ctx, "starting1")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	pid, base := 5555, "http://127.0.0.1:"+itoa(port)
	if _, err := s.Transition(ctx, "starting1", TransitionParams{
		To: StateStarting, Reason: "spawned", PID: &pid, Port: &port, BaseURL: &base,
	}); err != nil {
		t.Fatalf("STARTING: %v", err)
	}

	// A DRAINING instance (stop already in flight).
	startInstance(t, s, pp, "draining1", 6666)
	if _, err := s.Transition(ctx, "draining1", TransitionParams{To: StateDraining, Reason: "stop requested"}); err != nil {
		t.Fatalf("DRAINING: %v", err)
	}

	m := newMonitor(t, s, pp,
		fakeHealth{healthy: false},
		func(pid int) bool { return false }, // both look dead
		func(pid int) error { t.Fatalf("kill must not be called for non-HEALTHY states"); return nil },
		nil, 1)

	res, err := m.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(res.Checked) != 0 {
		t.Errorf("monitor must only consider HEALTHY instances; checked %v", res.Checked)
	}
	if got, _ := s.Get(ctx, "starting1"); got.State != StateStarting {
		t.Errorf("starting1 = %s, want STARTING untouched", got.State)
	}
	if got, _ := s.Get(ctx, "draining1"); got.State != StateDraining {
		t.Errorf("draining1 = %s, want DRAINING untouched", got.State)
	}
}

// TestMonitor_WrongModelIdentity: a healthy answer from a DIFFERENT model on our port is a
// reused-port collision, and the reason must say so rather than blaming /health.
func TestMonitor_WrongModelIdentity(t *testing.T) {
	d, s := openTestStore(t)
	ctx := context.Background()
	pp := NewPortPool(d.DB, 40100, 40105)
	pp.tryListen = func(int) (func() error, error) { return func() error { return nil }, nil }
	startInstance(t, s, pp, "i1", 4242)

	m := newMonitor(t, s, pp,
		fakeHealth{healthy: true, identityOK: false}, // answers, but it is not our model
		func(pid int) bool { return true },
		func(pid int) error { return nil },
		nil, 1)

	if _, err := m.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}
	got, _ := s.Get(ctx, "i1")
	if got.State != StateFailed {
		t.Fatalf("state = %s, want FAILED", got.State)
	}
	if !strings.Contains(got.Reason, "different/unknown model") {
		t.Errorf("reason must identify the collision, not a generic health failure; got %q", got.Reason)
	}
}
