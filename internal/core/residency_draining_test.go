package core

import (
	"context"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/instances"
)

// TestStopInstanceAsync_DetachesAndCompletes is the fix-4 gate (caller-disconnect robustness): the
// async stop returns immediately with a DRAINING view, and the actual stop completes in the
// background — the instance reaches STOPPED even after the returning call — so a client that
// disconnects mid-stop never leaves the row stuck DRAINING.
func TestStopInstanceAsync_DetachesAndCompletes(t *testing.T) {
	svc, fr, _ := startResidencyService(t, compat.Verdict{Status: compat.StatusRuns, Reason: "ok", EstimatedVRAMMiB: 4000}, 15000, nil)
	store := svc.deps.InstanceStore
	// Seed a HEALTHY llama.cpp instance with a PID (owned process).
	if _, err := store.Create(context.Background(), instances.CreateParams{
		ID: "s1", ModelID: "m1", Engine: instances.EngineLlamaCPP, Reason: "seed",
	}); err != nil {
		t.Fatal(err)
	}
	pid := 4242
	if _, err := store.Transition(context.Background(), "s1", instances.TransitionParams{
		To: instances.StateStarting, Reason: "seed", PID: &pid,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), "s1", instances.TransitionParams{
		To: instances.StateHealthy, Reason: "seed",
	}); err != nil {
		t.Fatal(err)
	}

	// Async stop returns immediately with the DRAINING view (the API returns 202 on this).
	view, err := svc.StopInstanceAsync(context.Background(), "s1")
	if err != nil {
		t.Fatalf("async stop: %v", err)
	}
	if view.State != string(instances.StateDraining) {
		t.Fatalf("async stop should return a DRAINING view immediately, got %s", view.State)
	}

	// The background stop runs DETACHED from the returning call: the runner's Stop is invoked in the
	// goroutine (fr.stopped is set) even though StopInstanceAsync already returned. This is the
	// caller-disconnect robustness: the stop proceeds regardless of the HTTP caller. (The real
	// llama.cpp runner writes the final STOPPED row here; the fake only records the call, so we
	// assert on the invocation, which is what proves detachment.)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if fr.stopped == "s1" {
			break // fix-4 satisfied: the stop ran in the background, detached from the returning call
		}
		if time.Now().After(deadline) {
			t.Fatalf("background stop never invoked the runner (detachment failed); fr.stopped=%q", fr.stopped)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A forced operator stop of an ADOPTED instance must carry the force bit into the detached
// goroutine. The old implementation validated force before DRAINING, then called StopInstance
// (which always re-guarded with force=false), leaving the row stuck DRAINING forever.
func TestStopInstanceAsyncForce_AdoptedCarriesForceIntoDetachedStop(t *testing.T) {
	svc, fr, _ := startResidencyService(t, compat.Verdict{Status: compat.StatusRuns, Reason: "ok", EstimatedVRAMMiB: 4000}, 15000, nil)
	store := svc.deps.InstanceStore
	if _, err := store.Create(context.Background(), instances.CreateParams{
		ID: "adopted-1", ModelID: "m1", Engine: instances.EngineLlamaCPP, Reason: "seed adopted",
		Adopted: true, AdoptedSource: "test attach", PID: 4243,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), "adopted-1", instances.TransitionParams{
		To: instances.StateStarting, Reason: "seed",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), "adopted-1", instances.TransitionParams{
		To: instances.StateHealthy, Reason: "seed",
	}); err != nil {
		t.Fatal(err)
	}

	view, err := svc.StopInstanceAsyncForce(context.Background(), "adopted-1", true)
	if err != nil {
		t.Fatalf("forced async stop: %v", err)
	}
	if view.State != string(instances.StateDraining) {
		t.Fatalf("forced async stop should return DRAINING, got %s", view.State)
	}

	deadline := time.Now().Add(3 * time.Second)
	for fr.stopped != "adopted-1" {
		if time.Now().After(deadline) {
			t.Fatalf("forced adopted stop never reached runner; row would remain DRAINING")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestReconcile_DrainingDeadPidBecomesStopped is the fix-4 reconciler gate: a DRAINING row whose
// PID is dead (the stop killed the process but the final STOPPED write was lost — e.g. a crash
// mid-drain) is reaped to STOPPED on the reconcile pass. Uses the reconciler with a dead-PID alive
// checker.
func TestReconcile_DrainingDeadPidBecomesStopped(t *testing.T) {
	d := coreTestDB(t)
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format) VALUES ('m1','M1','local_path','gguf')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO engines (name, installed) VALUES ('llamacpp',1)`); err != nil {
		t.Fatal(err)
	}
	store := instances.NewStore(d.DB)
	// Seed a DRAINING instance with a PID.
	if _, err := store.Create(context.Background(), instances.CreateParams{
		ID: "d1", ModelID: "m1", Engine: instances.EngineLlamaCPP, Reason: "seed",
	}); err != nil {
		t.Fatal(err)
	}
	pid := 9999
	port := 40150
	for _, to := range []instances.State{instances.StateStarting, instances.StateHealthy, instances.StateDraining} {
		p := &instances.TransitionParams{To: to, Reason: "seed"}
		if to == instances.StateStarting {
			p.PID = &pid
			p.Port = &port
		}
		if _, err := store.Transition(context.Background(), "d1", *p); err != nil {
			t.Fatalf("to %s: %v", to, err)
		}
	}

	// Reconcile with alive=false (PID dead) → the DRAINING row must become STOPPED.
	pool := instances.NewPortPool(d.DB, 40150, 40160)
	rec := instances.NewReconciler(store, pool,
		instances.HealthCheckFunc(func(ctx context.Context, inst instances.Instance) (bool, bool, error) {
			return false, false, nil
		}),
		func(pid int) bool { return false }, // dead
		func(pid int) error { return nil },
	)
	res, err := rec.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(context.Background(), "d1")
	if got.State != instances.StateStopped {
		t.Fatalf("DRAINING-with-dead-pid should reconcile to STOPPED, got %s (result %+v)", got.State, res)
	}
	found := false
	for _, id := range res.Stopped {
		if id == "d1" {
			found = true
		}
	}
	if !found {
		t.Errorf("d1 not reported in reconcile Stopped set: %+v", res.Stopped)
	}
}
