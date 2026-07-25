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
	if err := m.queue.Enqueue(req); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertWaiting(context.Background(), req, blocked, req.Cost.EstimateMiB); err != nil {
		t.Fatal(err)
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
