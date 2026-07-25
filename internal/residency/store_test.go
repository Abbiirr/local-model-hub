package residency

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/localhub/localhub/internal/db"
)

// TestQueueStore_PersistsStartContext is the fix-1 persistence gate: the queue row round-trips the
// FULL start context (ctx/batch/pin/priority) so the drain loop can re-Admit + start faithfully.
func TestQueueStore_PersistsStartContext(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format) VALUES ('m1','M1','local_path','gguf')`); err != nil {
		t.Fatal(err)
	}
	store := NewQueueStore(d.DB)

	req := Request{
		ID: "q1", ModelID: "m1", Engine: "llamacpp", Class: ClassInteractive,
		Ctx: 8192, Batch: 512, Pin: true, Priority: 0,
		Cost: CostHint{EstimateMiB: 4000},
	}
	// Assign seq via the queue so Seq() is set.
	q := NewQueue(QueueConfig{}, newFakeClock())
	if err := q.Enqueue(req); err != nil {
		t.Fatal(err)
	}
	stored := q.Snapshot(NextInput{})[0]

	if err := store.InsertWaiting(context.Background(), stored, "waiting: 5024 short", 4000); err != nil {
		t.Fatal(err)
	}

	got, err := store.get(context.Background(), "q1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ReqCtx != 8192 || got.ReqBatch != 512 || !got.ReqPin {
		t.Errorf("context not round-tripped: ctx=%d batch=%d pin=%v, want 8192/512/true", got.ReqCtx, got.ReqBatch, got.ReqPin)
	}
	if got.Priority != 0 || got.PriorityClass != int(ClassInteractive) {
		t.Errorf("priority not round-tripped: priority=%d class=%d", got.Priority, got.PriorityClass)
	}
	if got.FootprintMiB != 4000 {
		t.Errorf("footprint = %d, want 4000", got.FootprintMiB)
	}
	if got.BlockedReason != "waiting: 5024 short" {
		t.Errorf("blocked_reason not stored: %q", got.BlockedReason)
	}
}
