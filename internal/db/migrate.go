package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// migrationFiles holds the embedded, sequentially-numbered .sql migrations. Files are
// named NNNN_description.sql and applied in ascending numeric order. New migrations are
// additive (data-model.md: avoid destructive migrations).
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// Migrate applies all pending migrations idempotently. It records applied versions in a
// schema_migrations table and skips any already present, so it is safe to run on every
// daemon startup. Each migration runs in its own transaction: on failure the transaction
// rolls back and no partial version is recorded.
func (d *DB) Migrate() error {
	if err := d.ensureMigrationsTable(); err != nil {
		return err
	}
	applied, err := d.appliedVersions()
	if err != nil {
		return err
	}
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range migs {
		if applied[m.version] {
			continue
		}
		if err := d.applyOne(m); err != nil {
			return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

// AppliedVersions returns the set of migration versions currently recorded as applied.
// Exposed for tests and startup diagnostics.
func (d *DB) AppliedVersions() (map[int]bool, error) {
	if err := d.ensureMigrationsTable(); err != nil {
		return nil, err
	}
	return d.appliedVersions()
}

func (d *DB) ensureMigrationsTable() error {
	_, err := d.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		);`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	return nil
}

func (d *DB) appliedVersions() (map[int]bool, error) {
	rows, err := d.Query("SELECT version FROM schema_migrations;")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func (d *DB) applyOne(m migration) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	// Any early return after this rolls back; the Commit path clears it.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := execScript(tx, m.sql); err != nil {
		return err
	}
	if _, err := tx.Exec(
		"INSERT INTO schema_migrations (version, name) VALUES (?, ?);",
		m.version, m.name,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// execScript runs a multi-statement SQL script. modernc.org/sqlite executes the whole
// script when passed to Exec, but we keep this indirection so behavior is explicit.
func execScript(tx *sql.Tx, script string) error {
	if strings.TrimSpace(script) == "" {
		return nil
	}
	if _, err := tx.Exec(script); err != nil {
		return err
	}
	return nil
}

// loadMigrations reads and parses the embedded migration files, sorted by version.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var migs []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		ver, name, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		content, err := migrationFiles.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		migs = append(migs, migration{version: ver, name: name, sql: string(content)})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	// Guard against duplicate version numbers, which would make ordering ambiguous.
	for i := 1; i < len(migs); i++ {
		if migs[i].version == migs[i-1].version {
			return nil, fmt.Errorf("duplicate migration version %04d", migs[i].version)
		}
	}
	return migs, nil
}

// parseMigrationName parses "0001_initial_schema.sql" into (1, "initial_schema").
func parseMigrationName(fname string) (int, string, error) {
	base := strings.TrimSuffix(fname, ".sql")
	idx := strings.IndexByte(base, '_')
	if idx <= 0 {
		return 0, "", fmt.Errorf("migration filename %q must be NNNN_name.sql", fname)
	}
	ver, err := strconv.Atoi(base[:idx])
	if err != nil {
		return 0, "", fmt.Errorf("migration filename %q: bad version prefix: %w", fname, err)
	}
	return ver, base[idx+1:], nil
}
