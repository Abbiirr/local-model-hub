// Package db owns the SQLite connection and schema migrations.
//
// Driver choice: modernc.org/sqlite (pure Go). The reference box has no gcc available for
// cgo builds of mattn/go-sqlite3, and ADR-005's spirit ("single static binary, no native
// linking in the control plane") favors a pure-Go driver. See PLAN.md context-summary.
package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no cgo)
)

// DB wraps *sql.DB with LocalHub-specific open/migrate behavior.
type DB struct {
	*sql.DB
	path string
}

// Open opens (creating if needed) the SQLite database at path and applies WAL-mode and
// busy-timeout pragmas on the connection. It does NOT run migrations — call Migrate.
//
// Pragmas:
//   - journal_mode=WAL      concurrent readers + single writer; required by data-model.md.
//   - busy_timeout=5000     wait up to 5s on a locked db instead of erroring immediately.
//   - foreign_keys=ON       enforce the REFERENCES in the schema (data-model.md).
//   - synchronous=NORMAL    safe + fast pairing with WAL.
//
// Pragmas are passed via the modernc DSN so they apply to every pooled connection. We also
// pin MaxOpenConns to keep WAL semantics predictable on a single-box daemon.
func Open(path string) (*DB, error) {
	// modernc.org/sqlite accepts connection pragmas as "_pragma=" query params in the DSN.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)",
		path,
	)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// Verify the connection and that WAL actually engaged (a memory db or a read-only
	// filesystem would silently fall back).
	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("ping sqlite %q: %w", path, err)
	}
	return &DB{DB: sqldb, path: path}, nil
}

// JournalMode returns the active journal mode (e.g. "wal"), for startup verification.
func (d *DB) JournalMode() (string, error) {
	var mode string
	if err := d.QueryRow("PRAGMA journal_mode;").Scan(&mode); err != nil {
		return "", fmt.Errorf("read journal_mode: %w", err)
	}
	return mode, nil
}

// Path returns the on-disk path this DB was opened with.
func (d *DB) Path() string { return d.path }
