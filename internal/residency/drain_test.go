package residency

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/localhub/localhub/internal/db"
	"github.com/localhub/localhub/internal/instances"
)

// drainTestStore opens a migrated SQLite DB and returns a QueueStore over it (drain needs the
// durable rows for status transitions + the context fallback).
func drainTestStore(t *testing.T) (*QueueStore, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "drain.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	// The queue table FKs model_id → models; seed a model so InsertWaiting succeeds.
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format) VALUES ('m1','M1','local_path','gguf')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return NewQueueStore(d.DB), d
}

// recordingStarter implements QueueStarter, scripting per-call outcomes and recording the requests.
type recordingStarter struct {
	mu       sync.Mutex
	seen     []DrainRequest
	outcomes []starterOutcome // consumed in order; last repeats
}

type starterOutcome struct {
	started  bool
	requeued bool
	err      error
}

func (s *recordingStarter) StartQueued(ctx context.Context, req DrainRequest) (bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, req)
	i := len(s.seen) - 1
	if i >= len(s.outcomes) {
		i = len(s.outcomes) - 1
	}
	o := s.outcomes[i]
	return o.started, o.requeued, o.err
}

// mgrForDrain builds a manager wired for draining: a real QueueStore, a static lister/probe, and a
// scripted starter.
func mgrForDrain(t *testing.T, store *QueueStore, probeFree int, active []instances.Instance, starter QueueStarter) *Manager {
	t.Helper()
	m := NewManager(ManagerConfig{
		Queue:           NewQueue(QueueConfig{}, newFakeClock()),
		QueueStore:      store,
		Probe:           &countingProbe{vals: []int{probeFree}},
		Lister:          staticLister{active},
		Evictor:         &recordingEvictor{},
		Estimator:       fixedEstimator{mib: 4000},
		Clock:           newFakeClock(),
		SafetyMarginMiB: 1024,
	})
	m.SetStarter(starter)
	return m
}

// enqueue directly enqueues a WAITING request (in-memory + durable) so the drain loop has work.
func enqueueWaiting(t *testing.T, m *Manager, store *QueueStore, req Request, blocked string) {
	t.Helper()
	stored, err := m.queue.Enqueue(req)
	if err != nil {
		t.Fatal(err)
	}
	// Persist the STORED copy (with the assigned seq) — mirrors the production fix so durable rows
	// carry the FIFO seq the in-memory queue uses (fix 3).
	if err := store.InsertWaiting(context.Background(), stored, blocked, req.Cost.EstimateMiB); err != nil {
		t.Fatal(err)
	}
}

// fakeAutoLoadGate implements AutoLoadGate over a static map (migration 0014 test double).
type fakeAutoLoadGate struct{ blocked map[string]string }

func (g fakeAutoLoadGate) AutoLoadRefusalReason(_ context.Context, modelID string) string {
	return g.blocked[modelID]
}

// TestDrainQueue_DropsDecommissionedModel: a WAITING entry for a model the AutoLoadGate refuses is
// DROPPED (durable row marked DONE with the reason) rather than handed to the QueueStarter — the
// drain loop must never attempt to PROMOTE (auto-start) a decommissioned model.
func TestDrainQueue_DropsDecommissionedModel(t *testing.T) {
	store, _ := drainTestStore(t)
	starter := &recordingStarter{outcomes: []starterOutcome{{started: true}}}
	m := mgrForDrain(t, store, 8000, nil, starter)
	reason := `model "m1" (M1) is decommissioned from automatic loading`
	m.SetAutoLoadGate(fakeAutoLoadGate{blocked: map[string]string{"m1": reason}})

	enqueueWaiting(t, m, store, Request{ID: "q1", ModelID: "m1", Class: ClassNormal}, "waiting for VRAM")

	res, err := m.DrainQueue(context.Background())
	if err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}
	if len(res.Dropped) != 1 || res.Dropped[0] != "q1" {
		t.Fatalf("expected q1 in Dropped, got %+v", res)
	}
	if len(starter.seen) != 0 {
		t.Errorf("the QueueStarter must never be called for a decommissioned model, got %d call(s)", len(starter.seen))
	}
	rows, err := store.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != QueueDone || rows[0].BlockedReason != reason {
		t.Fatalf("expected the durable row DONE with the decommission reason, got %+v", rows)
	}
}

// TestDrainQueue_AutoStartsWhenRoom is THE fix-1 gate: a queued request auto-starts when the drain
// loop runs and the starter reports it fit — WITHOUT any manual per-request API call. The queue row
// becomes ADMITTED and the request is removed from the in-memory queue.
func TestDrainQueue_AutoStartsWhenRoom(t *testing.T) {
	store, _ := drainTestStore(t)
	starter := &recordingStarter{outcomes: []starterOutcome{{started: true}}}
	m := mgrForDrain(t, store, 15000, nil, starter)
	req := Request{ID: "q1", ModelID: "m1", Engine: "llamacpp", Class: ClassNormal,
		Ctx: 4096, Pin: true, Priority: 0, Cost: CostHint{EstimateMiB: 4000}}
	enqueueWaiting(t, m, store, req, "waiting for VRAM: need 5024 …")

	res, err := m.DrainQueue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Started) != 1 || res.Started[0] != "q1" {
		t.Fatalf("Started = %v, want [q1]", res.Started)
	}
	// The starter received the FULL persisted context (ctx/pin restored — the drain-context fix).
	if len(starter.seen) != 1 {
		t.Fatalf("starter saw %d requests, want 1", len(starter.seen))
	}
	got := starter.seen[0]
	if got.Ctx != 4096 || !got.Pin {
		t.Errorf("drained request lost context: ctx=%d pin=%v, want 4096/true", got.Ctx, got.Pin)
	}
	// The in-memory queue is now empty; the durable row is ADMITTED.
	if m.queue.Len() != 0 {
		t.Errorf("queue not drained: %d remain", m.queue.Len())
	}
	rows, _ := store.List(context.Background(), 10)
	if len(rows) != 1 || rows[0].Status != QueueAdmitted {
		t.Fatalf("durable row not ADMITTED: %+v", rows)
	}
}

// TestDrainQueue_LeavesQueuedWhenNoRoom: when the starter re-queues (still no room), the drain loop
// leaves the request queued (does not double-start) and stops the pass. No manual trigger admits it.
func TestDrainQueue_LeavesQueuedWhenNoRoom(t *testing.T) {
	store, _ := drainTestStore(t)
	starter := &recordingStarter{outcomes: []starterOutcome{{requeued: true}}}
	m := mgrForDrain(t, store, 1000, nil, starter)
	req := Request{ID: "q1", ModelID: "m1", Engine: "llamacpp", Class: ClassNormal,
		Ctx: 4096, Cost: CostHint{EstimateMiB: 9000}}
	enqueueWaiting(t, m, store, req, "waiting for VRAM …")

	res, err := m.DrainQueue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Started) != 0 {
		t.Errorf("nothing should start when no room, got Started=%v", res.Started)
	}
	if len(res.Requeued) != 1 {
		t.Errorf("Requeued = %v, want [q1]", res.Requeued)
	}
}

// TestDrainQueue_SkipsBlockedHeadForSmallerCandidate is the head-of-line regression gate: a
// large request may remain queued while a later, smaller request starts in the same pass. The
// scheduler must not infer that "head did not fit" means no later request can fit.
func TestDrainQueue_SkipsBlockedHeadForSmallerCandidate(t *testing.T) {
	store, _ := drainTestStore(t)
	starter := &recordingStarter{outcomes: []starterOutcome{{requeued: true}, {started: true}}}
	m := mgrForDrain(t, store, 5000, nil, starter)
	enqueueWaiting(t, m, store, Request{ID: "large", ModelID: "m1", Engine: "llamacpp", Class: ClassInteractive,
		Cost: CostHint{EstimateMiB: 9000}}, "waiting for VRAM")
	enqueueWaiting(t, m, store, Request{ID: "small", ModelID: "m1", Engine: "llamacpp", Class: ClassNormal,
		Cost: CostHint{EstimateMiB: 1000}}, "waiting for VRAM")

	res, err := m.DrainQueue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requeued) != 1 || res.Requeued[0] != "large" {
		t.Fatalf("Requeued = %v, want [large]", res.Requeued)
	}
	if len(res.Started) != 1 || res.Started[0] != "small" {
		t.Fatalf("Started = %v, want [small]", res.Started)
	}
}

// TestRestoreWaiting_RebuildsQueueFromDurableRows proves a daemon restart preserves ordering and
// the start context required by the drain loop.
func TestRestoreWaiting_RebuildsQueueFromDurableRows(t *testing.T) {
	store, _ := drainTestStore(t)
	before := mgrForDrain(t, store, 15000, nil, &recordingStarter{outcomes: []starterOutcome{{started: true}}})
	enqueueWaiting(t, before, store, Request{ID: "first", ModelID: "m1", Engine: "llamacpp", Class: ClassNormal,
		Ctx: 2048, Batch: 64, Pin: true, Priority: 1, Cost: CostHint{EstimateMiB: 3000}}, "waiting")
	enqueueWaiting(t, before, store, Request{ID: "second", ModelID: "m1", Engine: "llamacpp", Class: ClassNormal,
		Ctx: 4096, Batch: 128, Priority: 1, Cost: CostHint{EstimateMiB: 4000}}, "waiting")

	after := mgrForDrain(t, store, 15000, nil, &recordingStarter{outcomes: []starterOutcome{{started: true}}})
	n, err := after.RestoreWaiting(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("restored %d rows, want 2", n)
	}
	snap := after.Queue().Snapshot(NextInput{})
	if len(snap) != 2 || snap[0].ID != "first" || snap[1].ID != "second" {
		t.Fatalf("restored order = %#v, want first, second", snap)
	}
	if snap[0].Ctx != 2048 || snap[0].Batch != 64 || !snap[0].Pin || snap[0].Cost.EstimateMiB != 3000 {
		t.Fatalf("restored request lost context: %+v", snap[0])
	}
}

// TestDrainQueue_SingleFlight: a re-entrant DrainQueue during an in-progress pass returns empty
// (single-flight) so overlapping triggers do not stack or double-start.
func TestDrainQueue_SingleFlight(t *testing.T) {
	store, _ := drainTestStore(t)
	// The starter blocks on a channel so we can observe a concurrent second call while the first is
	// mid-pass.
	release := make(chan struct{})
	entered := make(chan struct{})
	blocking := &blockingStarter{entered: entered, release: release}
	m := mgrForDrain(t, store, 15000, nil, blocking)
	req := Request{ID: "q1", ModelID: "m1", Engine: "llamacpp", Class: ClassNormal, Cost: CostHint{EstimateMiB: 4000}}
	enqueueWaiting(t, m, store, req, "waiting …")

	var firstRes DrainResult
	done := make(chan struct{})
	go func() {
		firstRes, _ = m.DrainQueue(context.Background())
		close(done)
	}()

	<-entered // the first pass is inside StartQueued now
	// A concurrent second drain must be a no-op (single-flight).
	secondRes, err := m.DrainQueue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(secondRes.Started) != 0 || len(secondRes.Requeued) != 0 {
		t.Errorf("concurrent drain should be a no-op, got %+v", secondRes)
	}
	close(release)
	<-done
	if len(firstRes.Started) != 1 {
		t.Errorf("first drain should have started q1, got %+v", firstRes)
	}
}

type blockingStarter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingStarter) StartQueued(ctx context.Context, req DrainRequest) (bool, bool, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return true, false, nil
}
