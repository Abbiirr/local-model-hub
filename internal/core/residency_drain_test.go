package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/instances"
	"github.com/localhub/localhub/internal/residency"
)

// residencyPickVictimsForTest returns the eligible eviction victim IDs for the given active set,
// using the real residency policy + the in-flight tracker (so an in-flight instance is excluded).
func residencyPickVictimsForTest(active []instances.Instance, tracker *residency.InFlightTracker) ([]string, []residency.Victim) {
	ordered, all := residency.PickVictims(active, tracker, residency.EvictionPolicy{Now: time.Now()}, "")
	ids := make([]string, 0, len(ordered))
	for _, v := range ordered {
		ids = append(ids, v.Instance.ID)
	}
	return ids, all
}

// TestDrainQueue_AutoStartsQueuedRequest is the END-TO-END fix-1 gate: a start that queues (no
// room) is auto-started by DrainQueue after capacity frees — WITHOUT any manual per-request API
// call. This is the never-implemented path the first storm exposed. To reach a successful start
// without a real llama-server binary (which PlanLlamaLaunch needs), the drained re-Admit resolves
// to route_loaded: after freeing room a warm m1 instance is present, so StartQueued succeeds via
// the warm route — exercising the full pop → re-Admit → start-path → row-ADMITTED wiring.
func TestDrainQueue_AutoStartsQueuedRequest(t *testing.T) {
	// A pinned resident occupies room so m1 (footprint 9000 + 1024) cannot fit in 3000 free → queue.
	pinned := instances.Instance{ID: "pin", ModelID: "resident", Engine: instances.EngineLlamaCPP, VRAMEstMiB: 4000, Pinned: true}
	svc, fr, probe := startResidencyService(t, compat.Verdict{
		Status: compat.StatusRuns, Reason: "fits at 4k", EstimatedVRAMMiB: 9000,
	}, 3000, []instances.Instance{pinned})

	// 1. Start m1 → QUEUED (nothing spawned).
	_, err := svc.StartInstance(context.Background(), StartInstanceRequest{ModelID: "m1", Ctx: 4096, Pin: true})
	var qErr *QueuedError
	if !errors.As(err, &qErr) {
		t.Fatalf("expected a QueuedError, got %v", err)
	}
	if fr.startCalled {
		t.Fatal("GATE VIOLATION: runner.Start called on a queued admission")
	}

	// 2. Free VRAM AND make m1 warm-resident (models come warm) so the drained re-Admit routes to
	//    it without needing a real spawn/binary.
	probe.Snap.GPUs[0].VRAMFreeMiB = 15000
	seedHealthy(t, svc, "warm-m1", "m1")

	// 3. Drain — no manual per-request API call. The queued m1 must auto-resolve (route_loaded).
	res, derr := svc.DrainQueue(context.Background())
	if derr != nil {
		t.Fatalf("drain: %v", derr)
	}
	if len(res.Started) != 1 || res.Started[0] != qErr.QueueID {
		t.Fatalf("drain Started = %v, want [%s]", res.Started, qErr.QueueID)
	}
	// The durable row is now ADMITTED (auto-drained, no manual API call).
	q, _ := svc.Queue(context.Background())
	for _, e := range q {
		if e.ID == qErr.QueueID && e.Status == "WAITING" {
			t.Errorf("queue entry %s still WAITING after auto-drain", qErr.QueueID)
		}
	}
}

// seedHealthy drives a fresh instance to HEALTHY in the store (a warm-route target for tests).
func seedHealthy(t *testing.T, svc *Service, id, modelID string) {
	t.Helper()
	store := svc.deps.InstanceStore
	if _, err := store.Create(context.Background(), instances.CreateParams{
		ID: id, ModelID: modelID, Engine: instances.EngineLlamaCPP, Reason: "seed",
	}); err != nil {
		t.Fatal(err)
	}
	port := 40190
	url := "http://127.0.0.1:40190"
	if _, err := store.Transition(context.Background(), id, instances.TransitionParams{
		To: instances.StateStarting, Reason: "seed", Port: &port, BaseURL: &url,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), id, instances.TransitionParams{
		To: instances.StateHealthy, Reason: "seed",
	}); err != nil {
		t.Fatal(err)
	}
}

// TestInFlight_StreamingInstanceNotEvictable is the fix-3 gate: an instance with an in-flight
// request (counted via the InFlightTracker, as BeginInstanceRequest does around a stream) is NEVER
// selected as an eviction victim. A start that needs room QUEUES rather than evicting the busy
// instance — the never-kill-active gate now has real signal. When the request completes the
// instance is evictable again (asserted via the eviction policy so we don't depend on probe-reclaim
// simulation in the fake).
func TestInFlight_StreamingInstanceNotEvictable(t *testing.T) {
	// One resident "busy" (model r-busy). m1 needs room; the only candidate is busy.
	busy := instances.Instance{ID: "busy", ModelID: "r-busy", Engine: instances.EngineLlamaCPP, VRAMEstMiB: 8000}
	svc, fr, _ := startResidencyService(t, compat.Verdict{
		Status: compat.StatusRuns, Reason: "needs room", EstimatedVRAMMiB: 9000,
	}, 3000, []instances.Instance{busy})

	// Mark "busy" as serving an in-flight request (what BeginInstanceRequest does around a stream).
	svc.deps.InFlight.IncrementActive("busy")

	// A start of m1 (footprint 9000 + 1024 = 10024, free 3000) can only fit by evicting busy — but
	// busy is in-flight → never a victim → QUEUE, nothing evicted/spawned.
	_, err := svc.StartInstance(context.Background(), StartInstanceRequest{ModelID: "m1", Ctx: 4096})
	var qErr *QueuedError
	if !errors.As(err, &qErr) {
		t.Fatalf("expected QUEUED (busy instance is in-flight, not evictable), got %v", err)
	}
	if fr.startCalled {
		t.Error("GATE VIOLATION: spawned despite no evictable room (busy is in-flight)")
	}
	// busy must still be HEALTHY (never evicted).
	b, _ := svc.deps.InstanceStore.Get(context.Background(), "busy")
	if b.State != instances.StateHealthy {
		t.Errorf("in-flight instance was evicted (state %s), never-kill-active violated", b.State)
	}

	// Complete the in-flight request → busy is now idle and would be an eligible victim. Assert via
	// the eviction policy directly (probe-reclaim simulation is out of scope for the fake).
	svc.deps.InFlight.DecrementActive("busy")
	active, _ := svc.deps.InstanceStore.List(context.Background(), true)
	ordered, _ := residencyPickVictimsForTest(active, svc.deps.InFlight)
	found := false
	for _, id := range ordered {
		if id == "busy" {
			found = true
		}
	}
	if !found {
		t.Error("after the in-flight request completed, busy should be an eligible eviction victim")
	}
}

// TestBeginInstanceRequest_CountsInFlight: BeginInstanceRequest increments the counter and Done
// decrements it, and a non-HEALTHY / no-base-url instance is rejected.
func TestBeginInstanceRequest_CountsInFlight(t *testing.T) {
	// Seed a HEALTHY instance WITH a base_url (drive it through the store).
	svc, _, _ := startResidencyService(t, compat.Verdict{Status: compat.StatusRuns, Reason: "ok", EstimatedVRAMMiB: 4000}, 15000, nil)
	store := svc.deps.InstanceStore
	if _, err := store.Create(context.Background(), instances.CreateParams{
		ID: "i1", ModelID: "m1", Engine: instances.EngineLlamaCPP, Reason: "seed",
	}); err != nil {
		t.Fatal(err)
	}
	port := 40100
	url := "http://127.0.0.1:40100"
	if _, err := store.Transition(context.Background(), "i1", instances.TransitionParams{
		To: instances.StateStarting, Reason: "seed", Port: &port, BaseURL: &url,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), "i1", instances.TransitionParams{
		To: instances.StateHealthy, Reason: "seed",
	}); err != nil {
		t.Fatal(err)
	}

	h, err := svc.BeginInstanceRequest(context.Background(), "i1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if svc.deps.InFlight.ActiveCount("i1") != 1 {
		t.Errorf("in-flight = %d, want 1 after BeginInstanceRequest", svc.deps.InFlight.ActiveCount("i1"))
	}
	if h.TargetBaseURL != url {
		t.Errorf("target = %q, want %q", h.TargetBaseURL, url)
	}
	h.Done()
	if svc.deps.InFlight.ActiveCount("i1") != 0 {
		t.Errorf("in-flight = %d, want 0 after Done", svc.deps.InFlight.ActiveCount("i1"))
	}
	// Done is idempotent.
	h.Done()
	if svc.deps.InFlight.ActiveCount("i1") != 0 {
		t.Errorf("Done not idempotent")
	}

	// An unknown / not-HEALTHY instance is rejected.
	if _, err := svc.BeginInstanceRequest(context.Background(), "nope"); err == nil {
		t.Error("expected an error for an unknown instance")
	}
}
