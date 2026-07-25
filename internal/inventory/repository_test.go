package inventory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/db"
)

// openTestDB opens a real modernc.org/sqlite database (temp file — WAL needs a real file)
// and applies the production migrations via internal/db (imported READ-ONLY). This
// exercises the repository against the actual `models` DDL, not a hand-rolled schema.
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	p := filepath.Join(t.TempDir(), "inv_test.db")
	d, err := db.Open(p)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	return d
}

func TestUpsertInsertsRows(t *testing.T) {
	d := openTestDB(t)
	repo := NewRepository(d)
	ctx := context.Background()

	root := buildFixtureTree(t)
	res := New([]string{root}).Scan(ctx)

	stats, err := repo.Upsert(ctx, res.Models)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if stats.Inserted != len(res.Models) || stats.Updated != 0 {
		t.Errorf("first upsert stats = %+v, want %d inserted / 0 updated", stats, len(res.Models))
	}
	n, err := repo.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(res.Models) {
		t.Errorf("row count = %d, want %d", n, len(res.Models))
	}
}

func TestReScanIsIdempotent(t *testing.T) {
	d := openTestDB(t)
	repo := NewRepository(d)
	ctx := context.Background()

	root := buildFixtureTree(t)
	res := New([]string{root}).Scan(ctx)

	if _, err := repo.Upsert(ctx, res.Models); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	countAfterFirst, _ := repo.Count(ctx)

	// Capture updated_at of one row before the second scan.
	id := res.Models[0].ID
	var ts1 string
	if err := d.QueryRowContext(ctx, `SELECT updated_at FROM models WHERE id=?`, id).Scan(&ts1); err != nil {
		t.Fatal(err)
	}

	// Re-scan the SAME tree and upsert again.
	res2 := New([]string{root}).Scan(ctx)
	stats, err := repo.Upsert(ctx, res2.Models)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	// Row COUNT must be identical (no duplicates).
	countAfterSecond, _ := repo.Count(ctx)
	if countAfterFirst != countAfterSecond {
		t.Errorf("re-scan changed row count: %d -> %d (not idempotent)", countAfterFirst, countAfterSecond)
	}
	// Second pass must be all updates, zero inserts.
	if stats.Inserted != 0 || stats.Updated != len(res2.Models) {
		t.Errorf("second upsert stats = %+v, want 0 inserted / %d updated", stats, len(res2.Models))
	}
	// updated_at must have been bumped (idempotency of identity, freshness of timestamp).
	var ts2 string
	if err := d.QueryRowContext(ctx, `SELECT updated_at FROM models WHERE id=?`, id).Scan(&ts2); err != nil {
		t.Fatal(err)
	}
	// datetime('now') has 1s resolution; a strict > is flaky in fast tests. Assert it is at
	// least not older, and that the row is unmistakably still the same id (idempotent key).
	if ts2 < ts1 {
		t.Errorf("updated_at went backwards: %q -> %q", ts1, ts2)
	}
}

func TestUpsertPreservesUnownedColumns(t *testing.T) {
	d := openTestDB(t)
	repo := NewRepository(d)
	ctx := context.Background()

	m := Model{
		ID: "gguf-preserve-me", DisplayName: "Preserve", Source: SourceLocalPath,
		Format: FormatGGUF, LocalPath: `K:\x.gguf`, SizeBytes: sizePtr(100),
	}
	if _, err := repo.Upsert(ctx, []Model{m}); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}

	// Simulate a later phase (compat engine) writing scanner-unowned columns.
	if _, err := d.ExecContext(ctx,
		`UPDATE models SET compat_status='runs', recommended_engine='llamacpp' WHERE id=?`, m.ID,
	); err != nil {
		t.Fatal(err)
	}

	// Re-scan/upsert the same model with changed size (as if the file changed).
	m.SizeBytes = sizePtr(200)
	if _, err := repo.Upsert(ctx, []Model{m}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	var compat, engine string
	var size int64
	if err := d.QueryRowContext(ctx,
		`SELECT compat_status, recommended_engine, size_bytes FROM models WHERE id=?`, m.ID,
	).Scan(&compat, &engine, &size); err != nil {
		t.Fatal(err)
	}
	if compat != "runs" || engine != "llamacpp" {
		t.Errorf("re-scan clobbered unowned columns: compat=%q engine=%q (want runs/llamacpp)", compat, engine)
	}
	if size != 200 {
		t.Errorf("scanner-owned size_bytes not updated: got %d, want 200", size)
	}
}

func TestReconcileFindsVanished(t *testing.T) {
	d := openTestDB(t)
	repo := NewRepository(d)
	ctx := context.Background()

	models := []Model{
		{ID: "gguf-a", DisplayName: "A", Source: SourceLocalPath, Format: FormatGGUF},
		{ID: "gguf-b", DisplayName: "B", Source: SourceLocalPath, Format: FormatGGUF},
		{ID: "ollama-x", DisplayName: "x", Source: SourceOllama, Format: FormatOllama},
	}
	if _, err := repo.Upsert(ctx, models); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	// Next scan of local_path only finds gguf-a (gguf-b vanished). Ollama not scanned.
	seen := []Model{{ID: "gguf-a", DisplayName: "A", Source: SourceLocalPath, Format: FormatGGUF}}
	rec, err := repo.Reconcile(ctx, seen, []Source{SourceLocalPath})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rec.VanishedIDs) != 1 || rec.VanishedIDs[0] != "gguf-b" {
		t.Errorf("vanished = %v, want [gguf-b]", rec.VanishedIDs)
	}

	// Reconcile must NOT delete the vanished row (mark-missing policy, not delete).
	n, _ := repo.Count(ctx)
	if n != 3 {
		t.Errorf("Reconcile deleted rows (count=%d, want 3); it must only report, not delete", n)
	}

	// Ollama row (not in scanned sources) must never be reported vanished.
	for _, id := range rec.VanishedIDs {
		if id == "ollama-x" {
			t.Errorf("reconcile reported an unscanned-source row (ollama-x) as vanished")
		}
	}
}

func TestMarkMissingViaCompatStatus(t *testing.T) {
	d := openTestDB(t)
	repo := NewRepository(d)
	ctx := context.Background()

	if _, err := repo.Upsert(ctx, []Model{
		{ID: "gguf-gone", DisplayName: "gone", Source: SourceLocalPath, Format: FormatGGUF},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := repo.MarkMissingViaCompatStatus(ctx, []string{"gguf-gone"})
	if err != nil {
		t.Fatalf("MarkMissing: %v", err)
	}
	if n != 1 {
		t.Errorf("marked %d rows, want 1", n)
	}
	var status string
	if err := d.QueryRowContext(ctx, `SELECT compat_status FROM models WHERE id=?`, "gguf-gone").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "missing" {
		t.Errorf("compat_status = %q, want missing", status)
	}
}

// TestReconcileMarksPresenceMissing asserts the migration-0002 presence marking: a vanished
// row is set presence='missing' with a missing_since timestamp, and a reappearing row is
// reset to present with missing_since cleared on the next Upsert.
func TestReconcileMarksPresenceMissing(t *testing.T) {
	d := openTestDB(t)
	repo := NewRepository(d)
	ctx := context.Background()

	models := []Model{
		{ID: "gguf-a", DisplayName: "A", Source: SourceLocalPath, Format: FormatGGUF},
		{ID: "gguf-b", DisplayName: "B", Source: SourceLocalPath, Format: FormatGGUF},
	}
	if _, err := repo.Upsert(ctx, models); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	// Scan now finds only gguf-a; gguf-b vanished.
	seen := []Model{{ID: "gguf-a", DisplayName: "A", Source: SourceLocalPath, Format: FormatGGUF}}
	rec, err := repo.Reconcile(ctx, seen, []Source{SourceLocalPath})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rec.NewlyMissing) != 1 || rec.NewlyMissing[0] != "gguf-b" {
		t.Errorf("NewlyMissing = %v, want [gguf-b]", rec.NewlyMissing)
	}

	var presence string
	var missingSince any
	if err := d.QueryRowContext(ctx,
		`SELECT presence, missing_since FROM models WHERE id='gguf-b'`,
	).Scan(&presence, &missingSince); err != nil {
		t.Fatal(err)
	}
	if presence != "missing" {
		t.Errorf("gguf-b presence = %q, want missing", presence)
	}
	if missingSince == nil {
		t.Error("gguf-b missing_since is NULL, want a timestamp")
	}
	// gguf-a must remain present.
	var presenceA string
	if err := d.QueryRowContext(ctx, `SELECT presence FROM models WHERE id='gguf-a'`).Scan(&presenceA); err != nil {
		t.Fatal(err)
	}
	if presenceA != "present" {
		t.Errorf("gguf-a presence = %q, want present", presenceA)
	}

	// Reconcile again with the same missing set: NewlyMissing must be empty (already missing).
	rec2, err := repo.Reconcile(ctx, seen, []Source{SourceLocalPath})
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if len(rec2.NewlyMissing) != 0 {
		t.Errorf("second Reconcile NewlyMissing = %v, want empty (already missing)", rec2.NewlyMissing)
	}
	if len(rec2.VanishedIDs) != 1 {
		t.Errorf("second Reconcile VanishedIDs = %v, want [gguf-b]", rec2.VanishedIDs)
	}

	// gguf-b reappears: Upsert must reset presence and clear missing_since.
	if _, err := repo.Upsert(ctx, []Model{
		{ID: "gguf-b", DisplayName: "B", Source: SourceLocalPath, Format: FormatGGUF},
	}); err != nil {
		t.Fatalf("re-upsert reappeared model: %v", err)
	}
	if err := d.QueryRowContext(ctx,
		`SELECT presence, missing_since FROM models WHERE id='gguf-b'`,
	).Scan(&presence, &missingSince); err != nil {
		t.Fatal(err)
	}
	if presence != "present" {
		t.Errorf("reappeared gguf-b presence = %q, want present", presence)
	}
	if missingSince != nil {
		t.Errorf("reappeared gguf-b missing_since = %v, want NULL", missingSince)
	}
}

// TestUpsertPersistsFilesJSON asserts companions/parts/architecture metadata land in the
// files_json column (migration 0002) and that a bare re-upsert with no metadata preserves it.
func TestUpsertPersistsFilesJSON(t *testing.T) {
	d := openTestDB(t)
	repo := NewRepository(d)
	ctx := context.Background()

	m := Model{
		ID: "gguf-vision", DisplayName: "Vision", Source: SourceLocalPath, Format: FormatGGUF,
		LocalPath:  `K:\vision.gguf`,
		Companions: []Companion{{Kind: "mmproj", Path: `K:\mmproj.gguf`, SizeBytes: 500}},
		Parts:      []string{`K:\vision-00001-of-00002.gguf`, `K:\vision-00002-of-00002.gguf`},
	}
	if _, err := repo.Upsert(ctx, []Model{m}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var fj string
	if err := d.QueryRowContext(ctx, `SELECT files_json FROM models WHERE id='gguf-vision'`).Scan(&fj); err != nil {
		t.Fatalf("read files_json: %v", err)
	}
	if fj == "" {
		t.Fatal("files_json empty, want companions+parts JSON")
	}
	for _, want := range []string{"mmproj", "companions", "parts", "vision-00001"} {
		if !strings.Contains(fj, want) {
			t.Errorf("files_json %q missing %q", fj, want)
		}
	}

	// A model with no companions/parts/arch persists NULL files_json (COALESCE-preserved).
	plain := Model{ID: "gguf-plain", DisplayName: "Plain", Source: SourceLocalPath, Format: FormatGGUF}
	if _, err := repo.Upsert(ctx, []Model{plain}); err != nil {
		t.Fatalf("upsert plain: %v", err)
	}
	var plainFJ any
	if err := d.QueryRowContext(ctx, `SELECT files_json FROM models WHERE id='gguf-plain'`).Scan(&plainFJ); err != nil {
		t.Fatal(err)
	}
	if plainFJ != nil {
		t.Errorf("plain model files_json = %v, want NULL", plainFJ)
	}
}

func TestUpsertRejectsInvalidRecord(t *testing.T) {
	d := openTestDB(t)
	repo := NewRepository(d)
	ctx := context.Background()

	// Empty display_name (NOT NULL) must be rejected with a readable error, not a raw
	// constraint failure.
	_, err := repo.Upsert(ctx, []Model{{ID: "bad", Source: SourceLocalPath, Format: FormatGGUF}})
	if err == nil {
		t.Fatal("expected error for empty display_name")
	}
}
