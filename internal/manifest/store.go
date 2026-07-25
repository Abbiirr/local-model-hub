package manifest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// This file is the immutable SQLite store for manifests (spec-digest §2: "Manifests are
// immutable — never mutate a manifest row"). It persists into the existing `manifests` table
// (migration 0001) with NO schema change: the additive LocalHub fields (Name, Digest, URL,
// LocalPath, HashPending, SupersedesID) that have no dedicated column are folded into the
// files_json column under a JSON ENVELOPE {"files":[...],"meta":{...}}. A plain array (the
// legacy shape) is still read for forward/backward tolerance.
//
// Write-once enforcement: Put refuses to insert a manifest whose id already exists. There is
// no Update method. The one exception is FinalizeHashes, which fills in the deferred sha256
// hashes for a hash_pending local-import manifest — this is NOT a mutation of provenance
// (repo/revision/source/files never change), only the completion of a background hash that
// was always going to be written; it is guarded to only ever transition hash_pending true→
// false and to only fill empty hashes.

// DBHandle is the minimal DB surface the store needs. *sql.DB and *sql.Tx satisfy it; it is
// injected (rather than importing internal/db) so the store is testable with any
// modernc.org/sqlite handle. The core passes the real *db.DB (which embeds *sql.DB).
type DBHandle interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store persists and reads immutable manifests.
type Store struct {
	db DBHandle
}

// NewStore builds a Store over an injected DB handle.
func NewStore(db DBHandle) *Store { return &Store{db: db} }

// filesEnvelope is the JSON shape stored in manifests.files_json. `files` is the canonical
// per-file list (spec-digest §7); `meta` carries the additive fields with no column. Storing
// the envelope (not a bare array) lets reproduce/inventory recover Name/Digest/URL/LocalPath/
// HashPending/SupersedesID without a schema migration.
type filesEnvelope struct {
	Files []File        `json:"files"`
	Meta  *envelopeMeta `json:"meta,omitempty"`
}

type envelopeMeta struct {
	Name         string `json:"name,omitempty"`
	Digest       string `json:"digest,omitempty"`
	URL          string `json:"url,omitempty"`
	LocalPath    string `json:"local_path,omitempty"`
	HashPending  bool   `json:"hash_pending,omitempty"`
	SupersedesID string `json:"supersedes_id,omitempty"`
}

// Put inserts a manifest immutably. It normalizes + validates first, then refuses if the id
// already exists (write-once). It does NOT touch the models row — linking a model to its
// manifest is the caller's job (core wiring), kept separate so the store owns only manifests.
func (s *Store) Put(ctx context.Context, m *Manifest) error {
	m.normalize()
	if err := m.Validate(); err != nil {
		return err
	}
	exists, err := s.exists(ctx, m.ID)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("manifest %s already exists; manifests are immutable (a new revision needs a new manifest)", m.ID)
	}

	env := filesEnvelope{Files: m.Files}
	if m.Name != "" || m.Digest != "" || m.URL != "" || m.LocalPath != "" || m.HashPending || m.SupersedesID != "" {
		env.Meta = &envelopeMeta{
			Name:         m.Name,
			Digest:       m.Digest,
			URL:          m.URL,
			LocalPath:    m.LocalPath,
			HashPending:  m.HashPending,
			SupersedesID: m.SupersedesID,
		}
	}
	filesJSON, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal files envelope: %w", err)
	}
	warnJSON, err := json.Marshal(m.SecurityWarnings)
	if err != nil {
		return fmt.Errorf("marshal security warnings: %w", err)
	}

	const q = `
INSERT INTO manifests
  (id, source, repo, revision, files_json, license, format, quantization,
   security_warnings_json, downloaded_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err = s.db.ExecContext(ctx, q,
		m.ID,
		string(m.Source),
		nullStr(m.Repo),
		nullStr(m.Revision),
		string(filesJSON),
		nullStr(m.License),
		nullStr(m.Format),
		nullStr(m.Quantization),
		string(warnJSON),
		m.DownloadedAt,
	)
	if err != nil {
		return fmt.Errorf("insert manifest %s: %w", m.ID, err)
	}
	return nil
}

// Get reads one manifest by id. Returns (nil, nil) when absent.
func (s *Store) Get(ctx context.Context, id string) (*Manifest, error) {
	const q = `
SELECT id, source, COALESCE(repo,''), COALESCE(revision,''), files_json,
       COALESCE(license,''), COALESCE(format,''), COALESCE(quantization,''),
       COALESCE(security_warnings_json,'[]'), downloaded_at
FROM manifests WHERE id = ?`
	row := s.db.QueryRowContext(ctx, q, id)
	m, err := scanManifest(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// FinalizeHashes completes a hash_pending local-import manifest: it fills the per-file
// sha256 hashes computed by the background job and clears hash_pending. It is guarded to
// only apply to a manifest that is currently hash_pending (an immutable manifest whose hashes
// were always going to be written); it never changes source/repo/revision/files-list, only
// fills empty hashes. Errors if the manifest is absent or not hash_pending.
func (s *Store) FinalizeHashes(ctx context.Context, id string, hashes map[string]string) error {
	m, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("finalize: manifest %s not found", id)
	}
	if !m.HashPending {
		return fmt.Errorf("finalize: manifest %s is not hash_pending; refusing to alter an immutable manifest", id)
	}
	for i := range m.Files {
		if h, ok := hashes[m.Files[i].Path]; ok && m.Files[i].SHA256 == "" {
			m.Files[i].SHA256 = h
		}
	}
	m.HashPending = false

	env := filesEnvelope{Files: m.Files, Meta: &envelopeMeta{
		Name: m.Name, Digest: m.Digest, URL: m.URL, LocalPath: m.LocalPath,
		HashPending: false, SupersedesID: m.SupersedesID,
	}}
	filesJSON, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal finalized files: %w", err)
	}
	const q = `UPDATE manifests SET files_json = ? WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, string(filesJSON), id); err != nil {
		return fmt.Errorf("finalize manifest %s: %w", id, err)
	}
	return nil
}

func (s *Store) exists(ctx context.Context, id string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM manifests WHERE id = ?`, id).Scan(&one)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("probe manifest %s: %w", id, err)
	default:
		return true, nil
	}
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanManifest scans one manifests row (in the column order of Get's SELECT) and unfolds the
// files_json envelope. It tolerates both the envelope shape and a legacy bare array.
func scanManifest(row rowScanner) (*Manifest, error) {
	var (
		id, source, repo, revision, filesJSON, license, format, quant, warnJSON, downloadedAt string
	)
	if err := row.Scan(&id, &source, &repo, &revision, &filesJSON, &license, &format, &quant, &warnJSON, &downloadedAt); err != nil {
		return nil, err
	}
	m := &Manifest{
		ID:           id,
		Source:       SourceKind(source),
		Repo:         repo,
		Revision:     revision,
		Format:       format,
		Quantization: quant,
		License:      license,
		DownloadedAt: downloadedAt,
	}
	// Warnings.
	if warnJSON != "" {
		_ = json.Unmarshal([]byte(warnJSON), &m.SecurityWarnings)
	}
	if m.SecurityWarnings == nil {
		m.SecurityWarnings = []string{}
	}
	// Files envelope (or legacy bare array).
	if filesJSON != "" {
		var env filesEnvelope
		if err := json.Unmarshal([]byte(filesJSON), &env); err == nil && (env.Files != nil || env.Meta != nil) {
			m.Files = env.Files
			if env.Meta != nil {
				m.Name = env.Meta.Name
				m.Digest = env.Meta.Digest
				m.URL = env.Meta.URL
				m.LocalPath = env.Meta.LocalPath
				m.HashPending = env.Meta.HashPending
				m.SupersedesID = env.Meta.SupersedesID
			}
		} else {
			// Legacy bare array [{path,sha256,size_bytes}].
			_ = json.Unmarshal([]byte(filesJSON), &m.Files)
		}
	}
	if m.Files == nil {
		m.Files = []File{}
	}
	return m, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
