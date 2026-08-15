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
	// Assign seq via the queue; Enqueue returns the stored copy with Seq() set (fix 3).
	q := NewQueue(QueueConfig{}, newFakeClock())
	stored, err := q.Enqueue(req)
	if err != nil {
		t.Fatal(err)
	}

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

// TestQueueStore_PersistsLaunchMode is the migration-0013 regression gate: a queue row's
// Embedding/Pooling/Reranking server-mode fields must round-trip through SQLite exactly like the
// 0009 sizing fields, so a restart re-seeds the SAME launch profile the request was admitted with
// (the 2026-08-06 bge-m3 crash-loop defect: replaying without --embedding).
func TestQueueStore_PersistsLaunchMode(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format) VALUES ('bge-m3','bge-m3','local_path','gguf')`); err != nil {
		t.Fatal(err)
	}
	store := NewQueueStore(d.DB)
	q := NewQueue(QueueConfig{}, newFakeClock())

	req := Request{
		ID: "q-embed", ModelID: "bge-m3", Engine: "llamacpp", Class: ClassNormal,
		Ctx: 8192, Profile: ProfileSpec{Embedding: true, Pooling: "mean"},
	}
	stored, err := q.Enqueue(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertWaiting(context.Background(), stored, "ram_blocked: test", 1000); err != nil {
		t.Fatal(err)
	}

	got, err := store.get(context.Background(), "q-embed")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Profile.Embedding {
		t.Error("REGRESSION: req_embedding did not round-trip through SQLite — a restart would restore a plain generation start")
	}
	if got.Profile.Pooling != "mean" {
		t.Errorf("req_pooling = %q, want %q", got.Profile.Pooling, "mean")
	}
	if got.Profile.Reranking {
		t.Error("req_reranking must be false when not requested")
	}

	// Reranking: true persists independently and survives ListWaiting too (the restart re-seed path).
	rerank := Request{ID: "q-rerank", ModelID: "bge-m3", Engine: "llamacpp", Class: ClassNormal, Profile: ProfileSpec{Reranking: true}}
	stored2, err := q.Enqueue(rerank)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertWaiting(context.Background(), stored2, "ram_blocked: test", 1000); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListWaiting(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var foundRerank bool
	for _, r := range rows {
		if r.ID == "q-rerank" {
			foundRerank = true
			if !r.Profile.Reranking {
				t.Error("REGRESSION: req_reranking did not round-trip through ListWaiting (the restart re-seed path)")
			}
		}
	}
	if !foundRerank {
		t.Fatal("q-rerank row not found in ListWaiting")
	}
}

// TestQueueStore_PersistsAssignedSeq is the fix-3 regression gate: durable WAITING rows must carry
// the SAME monotonic seq the in-memory queue assigned (not 0), so read/restart paths that ORDER BY
// seq keep FIFO determinism. Before the fix, Enqueue assigned the seq on its internal copy while the
// store received the seq=0 original, so every persisted row had seq=0 and lost ordering.
func TestQueueStore_PersistsAssignedSeq(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := d.Exec(
			`INSERT INTO models (id, display_name, source, format) VALUES (?,?,'local_path','gguf')`,
			id, id); err != nil {
			t.Fatal(err)
		}
	}
	store := NewQueueStore(d.DB)
	q := NewQueue(QueueConfig{}, newFakeClock())

	// Enqueue three in a known arrival order and persist the STORED copy each time (as production's
	// blockOrReject now does).
	inOrder := []string{"a", "b", "c"}
	models := map[string]string{"a": "m1", "b": "m2", "c": "m3"}
	var wantSeq []int64
	for _, id := range inOrder {
		stored, err := q.Enqueue(Request{ID: id, ModelID: models[id], Class: ClassNormal})
		if err != nil {
			t.Fatal(err)
		}
		if stored.Seq() == 0 {
			t.Fatalf("Enqueue returned seq=0 for %q (assigned seq not returned; seq is 1-based)", id)
		}
		wantSeq = append(wantSeq, stored.Seq())
		if err := store.InsertWaiting(context.Background(), stored, "blocked "+id, 100); err != nil {
			t.Fatal(err)
		}
	}

	// seqs must be strictly increasing in arrival order.
	for i := 1; i < len(wantSeq); i++ {
		if wantSeq[i] <= wantSeq[i-1] {
			t.Fatalf("assigned seqs not strictly increasing: %v", wantSeq)
		}
	}

	// Read the rows back from SQLite in seq order (the restart/drain re-seed path) — order must
	// match the in-memory arrival order, and each persisted seq must equal the assigned one.
	rows, err := store.ListWaiting(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("ListWaiting returned %d rows, want 3", len(rows))
	}
	for i, r := range rows {
		if r.ID != inOrder[i] {
			t.Fatalf("restart order[%d] = %q, want %q (FIFO lost — seq not persisted)", i, r.ID, inOrder[i])
		}
		if r.Seq != wantSeq[i] {
			t.Fatalf("row %q persisted seq = %d, want %d", r.ID, r.Seq, wantSeq[i])
		}
		if r.Seq == 0 {
			t.Fatalf("row %q persisted seq = 0 (the fix-3 bug)", r.ID)
		}
	}
}
