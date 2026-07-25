package inventory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// DBHandle is the minimal database surface the repository needs. *sql.DB and *sql.Tx both
// satisfy it, and it is injected (rather than importing internal/db) so the repository is
// testable with any modernc.org/sqlite handle — the wiring agent passes the real *db.DB
// (which embeds *sql.DB). This keeps the coordination boundary clean: inventory owns no
// connection/migration logic.
type DBHandle interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Repository upserts inventory records into the `models` table and reconciles vanished
// rows. It is idempotent: a re-scan of unchanged artifacts touches only updated_at, and a
// re-scan overall produces the same row COUNT (no duplicates), because upsert keys on the
// deterministic natural key models.id.
type Repository struct {
	db DBHandle
}

// NewRepository builds a Repository over an injected DB handle.
func NewRepository(db DBHandle) *Repository {
	return &Repository{db: db}
}

// scannerSources are the source values this package populates. Reconcile only ever marks
// rows it could own as missing — it must never touch rows created by the download/import
// path (Phase 3) or by a source it didn't scan (e.g. Ollama rows when Ollama was skipped).
// The caller passes the set of sources actually scanned to Reconcile.

// Upsert inserts or updates the given records. It preserves columns the scanner does not
// own — recommended_engine, compat_status, manifest_id, license (when the scanner has none)
// — so re-scans never clobber values written by the compat engine or supply-chain phases.
// created_at is set once on insert; updated_at is bumped on every write.
//
// The natural key models.id makes this an UPSERT via ON CONFLICT(id). All writes for one
// call run in a single transaction so a partial scan does not leave the catalog half-updated.
func (r *Repository) Upsert(ctx context.Context, models []Model) (UpsertStats, error) {
	var stats UpsertStats
	if len(models) == 0 {
		return stats, nil
	}

	// We need a transaction; DBHandle doesn't expose Begin (it may be a *sql.Tx already),
	// so we detect a *sql.DB to open one, else run inline on the provided handle.
	tx, commit, rollback, err := beginIfPossible(ctx, r.db)
	if err != nil {
		return stats, err
	}
	defer rollback()

	for _, m := range models {
		if err := validateForPersist(m); err != nil {
			return stats, err
		}
		inserted, updateErr := upsertOne(ctx, tx, m)
		if updateErr != nil {
			return stats, fmt.Errorf("upsert model %q: %w", m.ID, updateErr)
		}
		if inserted {
			stats.Inserted++
		} else {
			stats.Updated++
		}
	}

	if err := commit(); err != nil {
		return stats, fmt.Errorf("commit inventory upsert: %w", err)
	}
	return stats, nil
}

// UpsertStats reports how a scan changed the catalog.
type UpsertStats struct {
	Inserted int
	Updated  int
}

// validateForPersist enforces the NOT NULL columns before we attempt SQL, producing a
// readable reason (CLAUDE.md) rather than a raw constraint error.
func validateForPersist(m Model) error {
	if strings.TrimSpace(m.ID) == "" {
		return fmt.Errorf("model has empty id (natural key); refusing to persist: %+v", m)
	}
	if strings.TrimSpace(m.DisplayName) == "" {
		return fmt.Errorf("model %q has empty display_name (NOT NULL)", m.ID)
	}
	if m.Source == "" {
		return fmt.Errorf("model %q has empty source (NOT NULL)", m.ID)
	}
	if m.Format == "" {
		return fmt.Errorf("model %q has empty format (NOT NULL)", m.ID)
	}
	return nil
}

// upsertOne writes a single record. It returns inserted=true when a new row was created.
//
// Column mapping to the `models` DDL (spec-digest §2):
//
//	id, display_name, source, format          -> record (NOT NULL)
//	quantization, repo, revision, local_path,
//	size_bytes, sha256, license                -> record (NULL when empty/absent)
//	recommended_engine, compat_status,
//	manifest_id                                -> NOT owned by scanner; preserved on update,
//	                                              left NULL on insert.
//	created_at                                 -> DB default on insert; untouched on update.
//	updated_at                                 -> bumped to now() on every write.
//
// Architectures, ModelType, Companions, Parts, Warnings, ModTime have NO column in the DDL
// (reported as a gap) and are NOT persisted here.
func upsertOne(ctx context.Context, db DBHandle, m Model) (bool, error) {
	// Detect prior existence so we can report inserted vs updated and drive
	// COALESCE-preservation of scanner-unowned columns on update.
	var existed bool
	err := db.QueryRowContext(ctx, `SELECT 1 FROM models WHERE id = ?`, m.ID).Scan(new(int))
	switch {
	case err == sql.ErrNoRows:
		existed = false
	case err != nil:
		return false, fmt.Errorf("probe existing row: %w", err)
	default:
		existed = true
	}

	// ON CONFLICT preserves scanner-unowned columns (recommended_engine, compat_status,
	// manifest_id) and created_at by NOT listing them in the UPDATE SET. license is
	// preserved when the scanner has none (COALESCE(new, existing)) so a compat/manifest
	// phase's richer license isn't wiped by a later bare re-scan.
	//
	// presence/missing_since (migration 0002): being seen in a scan means the artifact is
	// present. On insert presence defaults to 'present' (DDL default) and missing_since is
	// NULL. On update we FORCE presence back to 'present' and CLEAR missing_since so a model
	// that vanished and reappeared is un-marked automatically. files_json (0002) captures
	// companions/parts/architectures; it is COALESCE-preserved when the scanner produced none
	// (so a Phase-3 manifest write isn't clobbered by a bare re-scan).
	files := filesJSON(m)
	const q = `
INSERT INTO models
  (id, display_name, source, format, quantization, repo, revision, local_path,
   size_bytes, sha256, license, files_json, presence, missing_since, updated_at)
VALUES
  (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'present', NULL, datetime('now'))
ON CONFLICT(id) DO UPDATE SET
  display_name  = excluded.display_name,
  source        = excluded.source,
  format        = excluded.format,
  quantization  = excluded.quantization,
  repo          = excluded.repo,
  revision      = excluded.revision,
  local_path    = excluded.local_path,
  size_bytes    = excluded.size_bytes,
  sha256        = COALESCE(excluded.sha256, models.sha256),
  license       = COALESCE(NULLIF(excluded.license, ''), models.license),
  files_json    = COALESCE(excluded.files_json, models.files_json),
  presence      = 'present',
  missing_since = NULL,
  updated_at    = datetime('now')
`
	_, err = db.ExecContext(ctx, q,
		m.ID,
		m.DisplayName,
		string(m.Source),
		string(m.Format),
		nullStr(m.Quantization),
		nullStr(m.Repo),
		nullStr(m.Revision),
		nullStr(m.LocalPath),
		m.SizeBytes, // *int64: nil -> NULL
		nullStrPtr(m.SHA256),
		nullStr(m.License),
		files, // NULL when the model has no companions/parts/arch metadata
	)
	if err != nil {
		return false, err
	}
	return !existed, nil
}

// filesJSON serializes the scanner-discovered companions, multi-part shard list, and
// safetensors architecture metadata into the `models.files_json` column (migration 0002).
// It returns nil (SQL NULL) when the model has nothing to carry, so bare Ollama/single-file
// models leave the column NULL and COALESCE-preservation never resurrects a stale blob.
//
// Shape:
//
//	{"companions":[{"kind","path","size_bytes"}], "parts":[...],
//	 "architectures":[...], "model_type":"..."}
//
// This is load-bearing for the llama.cpp launch flags in Phase 4 (--mmproj + shard-0 path)
// and for the compat engine (architectures/model_type). See migration 0002 header.
func filesJSON(m Model) any {
	if len(m.Companions) == 0 && len(m.Parts) == 0 && len(m.Architectures) == 0 && m.ModelType == "" {
		return nil
	}
	type companionJSON struct {
		Kind      string `json:"kind"`
		Path      string `json:"path"`
		SizeBytes int64  `json:"size_bytes"`
	}
	payload := struct {
		Companions    []companionJSON `json:"companions,omitempty"`
		Parts         []string        `json:"parts,omitempty"`
		Architectures []string        `json:"architectures,omitempty"`
		ModelType     string          `json:"model_type,omitempty"`
	}{
		Parts:         m.Parts,
		Architectures: m.Architectures,
		ModelType:     m.ModelType,
	}
	for _, c := range m.Companions {
		payload.Companions = append(payload.Companions, companionJSON{
			Kind: c.Kind, Path: c.Path, SizeBytes: c.SizeBytes,
		})
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil // never fail a persist over metadata serialization
	}
	return string(b)
}

// Reconcile marks rows that were NOT seen in the latest scan as missing, WITHOUT deleting
// them (spec: "mark vanished rows as missing rather than delete"). It only considers rows
// whose source is in scannedSources, so it never touches rows from a source that was not
// scanned this pass (e.g. Ollama rows when the Ollama daemon was unreachable — passing that
// source is the caller's responsibility, see RescanModels).
//
// Presence marking (migration 0002, orchestrator-approved): a catalog row in a scanned
// source that this scan did NOT find is set presence='missing' and, if it was previously
// present, gets missing_since=datetime('now'). Rows already 'missing' keep their original
// missing_since (we don't churn the timestamp). Upsert handles the reverse transition
// (present + missing_since=NULL) for rows that reappear, so Reconcile only ever moves rows
// present -> missing. Nothing is deleted. Returns the vanished ids for logging/summary.
func (r *Repository) Reconcile(ctx context.Context, seen []Model, scannedSources []Source) (Reconciliation, error) {
	var rec Reconciliation
	if len(scannedSources) == 0 {
		return rec, nil // nothing scanned → nothing to reconcile.
	}

	seenIDs := make(map[string]bool, len(seen))
	for _, m := range seen {
		seenIDs[m.ID] = true
	}

	// Fetch all catalog ids for the scanned sources (only rows currently present are
	// candidates for marking; already-missing rows keep their existing missing_since).
	placeholders := make([]string, len(scannedSources))
	args := make([]any, len(scannedSources))
	for i, s := range scannedSources {
		placeholders[i] = "?"
		args[i] = string(s)
	}
	q := fmt.Sprintf(`SELECT id, presence FROM models WHERE source IN (%s)`, strings.Join(placeholders, ","))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return rec, fmt.Errorf("list catalog ids: %w", err)
	}
	var vanished []string
	var newlyMissing []string
	for rows.Next() {
		var id, presence string
		if err := rows.Scan(&id, &presence); err != nil {
			rows.Close()
			return rec, err
		}
		if seenIDs[id] {
			continue
		}
		vanished = append(vanished, id)
		if presence != "missing" {
			newlyMissing = append(newlyMissing, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return rec, err
	}
	rows.Close()

	rec.VanishedIDs = vanished
	if len(vanished) == 0 {
		return rec, nil
	}

	// Rows transitioning present -> missing get a fresh missing_since; already-missing rows
	// are left untouched (their timestamp is preserved).
	if len(newlyMissing) > 0 {
		ph := make([]string, len(newlyMissing))
		uargs := make([]any, len(newlyMissing))
		for i, id := range newlyMissing {
			ph[i] = "?"
			uargs[i] = id
		}
		uq := fmt.Sprintf(
			`UPDATE models SET presence='missing', missing_since=datetime('now'), updated_at=datetime('now') WHERE id IN (%s)`,
			strings.Join(ph, ","),
		)
		if _, err := r.db.ExecContext(ctx, uq, uargs...); err != nil {
			return rec, fmt.Errorf("mark vanished rows missing: %w", err)
		}
	}
	rec.NewlyMissing = newlyMissing
	return rec, nil
}

// Reconciliation reports the result of a Reconcile pass.
type Reconciliation struct {
	// VanishedIDs are catalog rows (in a scanned source) that the latest scan did not find.
	// They are NOT deleted; they are marked presence='missing'.
	VanishedIDs []string
	// NewlyMissing is the subset of VanishedIDs that transitioned present -> missing on this
	// pass (i.e. got a fresh missing_since). Rows that were already missing are excluded.
	NewlyMissing []string
}

// MarkMissingViaCompatStatus is an OPT-IN interim that records a vanished row's absence by
// setting compat_status='missing'. It is NOT called by default because it overloads the
// compat_status column (a documented DDL gap). Provided so the orchestrator can choose this
// behavior until a proper presence column exists. Every affected row also gets updated_at
// bumped so the change is visible.
func (r *Repository) MarkMissingViaCompatStatus(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	q := fmt.Sprintf(
		`UPDATE models SET compat_status='missing', updated_at=datetime('now') WHERE id IN (%s)`,
		strings.Join(placeholders, ","),
	)
	res, err := r.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("mark missing: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Count returns the number of rows in the models table (test/diagnostic helper).
func (r *Repository) Count(ctx context.Context) (int, error) {
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM models`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// nullStr turns "" into a NULL (nil) and any other string into itself, for SQL binding.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullStrPtr turns a *string into a NULL-able SQL arg.
func nullStrPtr(p *string) any {
	if p == nil || *p == "" {
		return nil
	}
	return *p
}

// txBeginner is anything that can start a transaction. *sql.DB satisfies it, and so does
// the real *db.DB (which embeds *sql.DB, promoting BeginTx). We detect this interface
// rather than a concrete *sql.DB so wrappers like *db.DB are handled too, without importing
// internal/db.
type txBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// beginIfPossible starts a transaction when the handle can begin one (a *sql.DB or a wrapper
// embedding it). When it cannot (e.g. it is already a *sql.Tx), it runs inline (commit/
// rollback become no-ops) so callers can compose the repository inside their own
// transaction. Returns the handle to use for the writes.
func beginIfPossible(ctx context.Context, h DBHandle) (DBHandle, func() error, func(), error) {
	if b, ok := h.(txBeginner); ok {
		tx, err := b.BeginTx(ctx, nil)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("begin tx: %w", err)
		}
		commit := tx.Commit
		rollback := func() { _ = tx.Rollback() }
		return tx, commit, rollback, nil
	}
	// Cannot begin a transaction (already inside one): run directly, no-op commit/rollback.
	return h, func() error { return nil }, func() {}, nil
}
