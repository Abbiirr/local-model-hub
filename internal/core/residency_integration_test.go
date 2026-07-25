package core

import (
	"context"
	"errors"
	"testing"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/hardware"
	"github.com/localhub/localhub/internal/instances"
	"github.com/localhub/localhub/internal/residency"
)

// startResidencyService builds a Service with a REAL residency.Manager wired in (unlike
// startTestService, which leaves Deps.Residency nil → phase-4 fallback). It seeds a GGUF model +
// llamacpp engine row, a FakeProbe with the given free VRAM, a fake compat verdict, and a fake
// runner. This exercises the P5.1 admission path end-to-end through StartInstance.
func startResidencyService(t *testing.T, verdict compat.Verdict, freeVRAM int, seedInstances []instances.Instance) (*Service, *fakeRunner, *hardware.FakeProbe) {
	t.Helper()
	d := coreTestDB(t)
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, quantization, local_path)
		VALUES ('m1','Model One','local_path','gguf','q4_k_m','/tmp/m1.gguf')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO engines (name, installed) VALUES ('llamacpp',1)`); err != nil {
		t.Fatal(err)
	}
	// Seed any pre-existing instances (eviction candidates / warm hits).
	store := instances.NewStore(d.DB)
	for _, inst := range seedInstances {
		if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format) VALUES (?,?, 'local_path','gguf')
			ON CONFLICT(id) DO NOTHING`, inst.ModelID, inst.ModelID); err != nil {
			t.Fatal(err)
		}
		created, err := store.Create(context.Background(), instances.CreateParams{
			ID: inst.ID, ModelID: inst.ModelID, Engine: inst.Engine, VRAMEstMiB: inst.VRAMEstMiB,
			Pinned: inst.Pinned, Priority: inst.Priority, TTLSeconds: inst.TTLSeconds, Reason: "seed",
		})
		if err != nil {
			t.Fatalf("seed instance %s: %v", inst.ID, err)
		}
		// Drive it to HEALTHY so it is a real resident/candidate.
		if _, err := store.Transition(context.Background(), created.ID, instances.TransitionParams{
			To: instances.StateStarting, Reason: "seed",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Transition(context.Background(), created.ID, instances.TransitionParams{
			To: instances.StateHealthy, Reason: "seed",
		}); err != nil {
			t.Fatal(err)
		}
	}

	snap := hardware.DefaultFakeSnapshot()
	snap.GPUs[0].VRAMFreeMiB = freeVRAM
	probe := &hardware.FakeProbe{Snap: snap}

	inflight := residency.NewInFlightTracker()
	clock := residency.RealClock()
	mgr := residency.NewManager(residency.ManagerConfig{
		Queue:           residency.NewQueue(residency.QueueConfig{}, clock),
		QueueStore:      residency.NewQueueStore(d.DB),
		Probe:           fakeResidencyProbe{probe: probe},
		Lister:          residency.StoreLister{Store: store},
		Tracker:         inflight,
		Clock:           clock,
		SafetyMarginMiB: 1024,
	})

	fr := &fakeRunner{}
	svc := New(Deps{
		DB:            d,
		Hardware:      probe,
		Engines:       EnginesRegistry{},
		Compat:        &fakeCompat{verdict: verdict},
		InstanceStore: store,
		LlamaRunner:   fr,
		Events:        NewEventBus(),
		Cfg:           config.Default(),
		Residency:     mgr,
		InFlight:      inflight,
	})
	return svc, fr, probe
}

type fakeResidencyProbe struct{ probe *hardware.FakeProbe }

func (p fakeResidencyProbe) FreeVRAMMiB(ctx context.Context) (int, error) {
	snap, err := p.probe.Snapshot(ctx)
	if err != nil {
		return 0, err
	}
	return snap.GPUs[0].VRAMFreeMiB, nil
}

// TestStartInstance_Admission_QueuedWhenNoRoom: a runs verdict whose footprint + margin exceeds
// free VRAM, with only a PINNED instance resident (nothing evictable) → the start is QUEUED (a
// *QueuedError), nothing is spawned, and a durable queue row exists with a numeric blocked_reason.
func TestStartInstance_Admission_QueuedWhenNoRoom(t *testing.T) {
	pinned := instances.Instance{ID: "pin", ModelID: "resident", Engine: instances.EngineLlamaCPP, VRAMEstMiB: 4000, Pinned: true}
	// free 3000; footprint 9000 + 1024 margin = 10024 need; pinned is not evictable → queue.
	svc, fr, _ := startResidencyService(t, compat.Verdict{
		Status: compat.StatusRuns, Reason: "fits at 4k", EstimatedVRAMMiB: 9000,
	}, 3000, []instances.Instance{pinned})

	_, err := svc.StartInstance(context.Background(), StartInstanceRequest{ModelID: "m1", Ctx: 4096})
	if err == nil {
		t.Fatal("expected a QueuedError when no room and nothing evictable")
	}
	var qErr *QueuedError
	if !errors.As(err, &qErr) {
		t.Fatalf("expected *QueuedError, got %T: %v", err, err)
	}
	if fr.startCalled {
		t.Error("GATE VIOLATION: runner.Start called despite a queued admission (nothing should spawn)")
	}
	for _, tok := range []string{"need", "free", "reclaimable"} {
		if !contains(qErr.Reason(), tok) {
			t.Errorf("blocked_reason %q missing token %q", qErr.Reason(), tok)
		}
	}
	// The durable queue row exists and is WAITING.
	q, err := svc.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 1 || q[0].Status != "WAITING" || q[0].ModelID != "m1" {
		t.Fatalf("expected one WAITING queue row for m1, got %+v", q)
	}
	if q[0].Position != 1 {
		t.Errorf("queue position = %d, want 1", q[0].Position)
	}
}

// TestStartInstance_Admission_RouteLoadedWhenWarm: the model is already HEALTHY-resident → the
// start returns the warm instance WITHOUT spawning.
func TestStartInstance_Admission_RouteLoadedWhenWarm(t *testing.T) {
	warm := instances.Instance{ID: "warm", ModelID: "m1", Engine: instances.EngineLlamaCPP, VRAMEstMiB: 4000}
	svc, fr, _ := startResidencyService(t, compat.Verdict{
		Status: compat.StatusRuns, Reason: "ok", EstimatedVRAMMiB: 4000,
	}, 3000, []instances.Instance{warm})

	view, err := svc.StartInstance(context.Background(), StartInstanceRequest{ModelID: "m1", Ctx: 4096})
	if err != nil {
		t.Fatalf("warm route should not error: %v", err)
	}
	if view.ID != "warm" {
		t.Errorf("route_loaded should return the warm instance, got id %q", view.ID)
	}
	if fr.startCalled {
		t.Error("GATE VIOLATION: runner.Start called for an already-warm model")
	}
}

// TestCancelQueueEntry: a WAITING entry can be cancelled.
func TestCancelQueueEntry(t *testing.T) {
	pinned := instances.Instance{ID: "pin", ModelID: "resident", Engine: instances.EngineLlamaCPP, VRAMEstMiB: 4000, Pinned: true}
	svc, _, _ := startResidencyService(t, compat.Verdict{
		Status: compat.StatusRuns, Reason: "ok", EstimatedVRAMMiB: 9000,
	}, 3000, []instances.Instance{pinned})

	_, err := svc.StartInstance(context.Background(), StartInstanceRequest{ModelID: "m1", Ctx: 4096})
	var qErr *QueuedError
	if !errors.As(err, &qErr) {
		t.Fatalf("expected queued, got %v", err)
	}
	if cerr := svc.CancelQueueEntry(context.Background(), qErr.QueueID); cerr != nil {
		t.Fatalf("cancel: %v", cerr)
	}
	// After cancel it is no longer WAITING in the in-memory queue (position 0).
	q, _ := svc.Queue(context.Background())
	for _, e := range q {
		if e.ID == qErr.QueueID && e.Status == "WAITING" {
			t.Errorf("entry %s still WAITING after cancel", qErr.QueueID)
		}
	}
}

// contains is a tiny substring helper (mirrors the residency test helper) for the reason asserts.
func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && index(s, sub) >= 0)
}

func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
