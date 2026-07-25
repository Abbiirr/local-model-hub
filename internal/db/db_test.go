package db

import (
	"path/filepath"
	"sort"
	"testing"
)

// openTemp opens a fresh temp-file DB (WAL requires a real file, not :memory:).
func openTemp(t *testing.T) *DB {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestOpenSetsWALMode(t *testing.T) {
	d := openTemp(t)
	mode, err := d.JournalMode()
	if err != nil {
		t.Fatalf("JournalMode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

func TestMigrateUpFromZero(t *testing.T) {
	d := openTemp(t)
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// All tables from the DDL must exist.
	wantTables := []string{
		"models", "manifests", "engines", "instances",
		"queue", "metrics", "inst_metrics", "schema_migrations",
	}
	for _, tbl := range wantTables {
		var name string
		err := d.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?;", tbl,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing after migrate: %v", tbl, err)
		}
	}
	// Indexes from the DDL must exist.
	for _, idx := range []string{"idx_metrics_ts", "idx_inst_metrics_ts"} {
		var name string
		err := d.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='index' AND name=?;", idx,
		).Scan(&name)
		if err != nil {
			t.Errorf("index %q missing after migrate: %v", idx, err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	d := openTemp(t)
	if err := d.Migrate(); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	v1, err := d.AppliedVersions()
	if err != nil {
		t.Fatal(err)
	}
	// Running again must not error and must not re-apply (CREATE TABLE would fail if it did).
	if err := d.Migrate(); err != nil {
		t.Fatalf("second Migrate must be idempotent, got: %v", err)
	}
	v2, err := d.AppliedVersions()
	if err != nil {
		t.Fatal(err)
	}
	if len(v1) != len(v2) {
		t.Errorf("applied versions changed on re-run: %v -> %v", keys(v1), keys(v2))
	}
	if !v2[1] {
		t.Errorf("migration version 1 not recorded, applied=%v", keys(v2))
	}
	if !v2[2] {
		t.Errorf("migration version 2 not recorded, applied=%v", keys(v2))
	}
}

// TestMigrate0002AddsPresenceAndFilesColumns asserts the additive 0002 migration lands the
// presence / missing_since / files_json columns on `models` when migrating up from zero.
func TestMigrate0002AddsPresenceAndFilesColumns(t *testing.T) {
	d := openTemp(t)
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	got := tableColumns(t, d, "models")
	for _, c := range []string{"presence", "missing_since", "files_json"} {
		if !got[c] {
			t.Errorf("models missing column %q after 0002 (have %v)", c, sortedKeys(got))
		}
	}
	// presence must default to 'present' so a legacy row inserted with 0001 columns only
	// still reads back as present.
	if _, err := d.Exec(
		`INSERT INTO models (id, display_name, source, format) VALUES ('t','T','local_path','gguf')`,
	); err != nil {
		t.Fatalf("insert minimal row: %v", err)
	}
	var presence string
	var missingSince, filesJSON any
	if err := d.QueryRow(
		`SELECT presence, missing_since, files_json FROM models WHERE id='t'`,
	).Scan(&presence, &missingSince, &filesJSON); err != nil {
		t.Fatalf("read back row: %v", err)
	}
	if presence != "present" {
		t.Errorf("presence default = %q, want present", presence)
	}
	if missingSince != nil {
		t.Errorf("missing_since = %v, want NULL", missingSince)
	}
	if filesJSON != nil {
		t.Errorf("files_json = %v, want NULL", filesJSON)
	}
}

// TestMigrate0001Then0002Stepwise applies only 0001 first (simulating an existing DB created
// before 0002 shipped), inserts a row, then applies 0002 and confirms the new columns appear
// and the pre-existing row backfills to presence='present' (the additive-upgrade path).
func TestMigrate0001Then0002Stepwise(t *testing.T) {
	d := openTemp(t)
	if err := d.ensureMigrationsTable(); err != nil {
		t.Fatalf("ensureMigrationsTable: %v", err)
	}
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migs) < 2 || migs[0].version != 1 || migs[1].version != 2 {
		t.Fatalf("expected migrations 1 then 2, got %v", migs)
	}
	// Apply ONLY 0001.
	if err := d.applyOne(migs[0]); err != nil {
		t.Fatalf("apply 0001: %v", err)
	}
	got := tableColumns(t, d, "models")
	if got["presence"] {
		t.Fatal("presence column should NOT exist before 0002 is applied")
	}
	// A row that predates 0002.
	if _, err := d.Exec(
		`INSERT INTO models (id, display_name, source, format) VALUES ('old','Old','ollama','ollama')`,
	); err != nil {
		t.Fatalf("insert pre-0002 row: %v", err)
	}
	// Now apply 0002.
	if err := d.applyOne(migs[1]); err != nil {
		t.Fatalf("apply 0002: %v", err)
	}
	got = tableColumns(t, d, "models")
	for _, c := range []string{"presence", "missing_since", "files_json"} {
		if !got[c] {
			t.Errorf("models missing column %q after stepwise 0002 (have %v)", c, sortedKeys(got))
		}
	}
	var presence string
	if err := d.QueryRow(`SELECT presence FROM models WHERE id='old'`).Scan(&presence); err != nil {
		t.Fatalf("read back pre-existing row: %v", err)
	}
	if presence != "present" {
		t.Errorf("pre-existing row presence = %q, want present (additive default backfill)", presence)
	}
	// Applying the full Migrate() now must be idempotent (0001+0002 already recorded).
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate after stepwise must be idempotent: %v", err)
	}
}

func TestSchemaColumnsMatchDDL(t *testing.T) {
	d := openTemp(t)
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Spot-check a few load-bearing columns from spec-digest §2 exist with exact names.
	checks := map[string][]string{
		"models":    {"id", "display_name", "source", "format", "quantization", "compat_status", "manifest_id"},
		"instances": {"id", "model_id", "engine", "state", "pid", "port", "launch_flags_json"},
		"queue":     {"id", "model_id", "status", "blocked_reason", "vram_bytes_est"},
		"manifests": {"id", "files_json", "security_warnings_json"},
	}
	for tbl, cols := range checks {
		got := tableColumns(t, d, tbl)
		for _, c := range cols {
			if !got[c] {
				t.Errorf("table %q missing column %q (have %v)", tbl, c, sortedKeys(got))
			}
		}
	}
}

func TestForeignKeysEnabled(t *testing.T) {
	d := openTemp(t)
	var on int
	if err := d.QueryRow("PRAGMA foreign_keys;").Scan(&on); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if on != 1 {
		t.Errorf("foreign_keys = %d, want 1 (ON)", on)
	}
}

func tableColumns(t *testing.T, d *DB, table string) map[string]bool {
	t.Helper()
	rows, err := d.Query("PRAGMA table_info(" + table + ");")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols[name] = true
	}
	return cols
}

func keys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
