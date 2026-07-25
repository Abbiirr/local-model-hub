// Package manifest is the LocalHub supply-chain provenance layer (Phase 3,
// docs/model-supply-chain.md, spec-digest §7, ADR-004).
//
// Every model that enters the inventory via a DOWNLOAD or an explicit IMPORT gets an
// immutable manifest recording exactly what bytes landed, at what revision, with what
// per-file sha256 hashes, its license, and any security warnings. The manifest plus an
// instance's launch_flags_json together reproduce a working run (the "reproduce this"
// export, P3.3).
//
// Immutability (spec-digest §2 invariant "Manifests are immutable"): a manifest row is
// NEVER mutated. A re-download of a DIFFERENT revision writes a NEW manifest that
// supersedes the old one; the two are linked (SupersedesID). The store enforces this — an
// attempt to Put a manifest whose id already exists is refused.
//
// Warning policy (ADR-004 "warn, don't block by default"): the warning scanner attaches
// human-readable warnings to the manifest (pickle weights, trust_remote_code, missing
// license, gated repo) but NEVER blocks a download. An opt-in strict_mode (config
// security.strict_model_artifact_mode) upgrades pickle warnings to a refusal — that is the
// ONLY path that blocks, and it is off by default.
//
// This package does NOT perform network I/O itself (the HF/GGUF download machinery lives in
// internal/manifest/download and is driven by the core). It owns the data model, the
// warning scanner, path-containment validation, and the SQLite-backed immutable store.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// SourceKind enumerates where a manifest's artifact came from (docs/model-supply-chain.md
// "Sources supported (MVP)"). These are the manifest-level provenance kinds; they map onto
// the models.source column values via SourceKind.ModelSource().
type SourceKind string

const (
	// SourceHF is a Hugging Face repo snapshot at a pinned commit SHA (safetensors or GGUF).
	SourceHF SourceKind = "hf"
	// SourceGGUFURL is a single .gguf downloaded from a direct URL.
	SourceGGUFURL SourceKind = "gguf_url"
	// SourceOllama is a model pulled via `ollama pull`; Ollama owns the blob, we record
	// name + digest.
	SourceOllama SourceKind = "ollama"
	// SourceLocalImport is a pre-existing file/dir the user pointed LocalHub at. Its origin
	// is unverifiable — it always carries the "origin unverifiable" warning.
	SourceLocalImport SourceKind = "local_import"
)

// Valid reports whether k is a known source kind.
func (k SourceKind) Valid() bool {
	switch k {
	case SourceHF, SourceGGUFURL, SourceOllama, SourceLocalImport:
		return true
	}
	return false
}

// ModelSource maps a manifest SourceKind onto the models.source column vocabulary
// (ollama | huggingface | local_path | direct_url; spec-digest §2). The manifest kinds are
// finer-grained (hf covers both safetensors and GGUF repos); this collapses them.
func (k SourceKind) ModelSource() string {
	switch k {
	case SourceHF:
		return "huggingface"
	case SourceGGUFURL:
		return "direct_url"
	case SourceOllama:
		return "ollama"
	case SourceLocalImport:
		return "local_path"
	default:
		return string(k)
	}
}

// File is one artifact file recorded in a manifest: its repo-relative (or basename) path,
// its sha256 (lowercase hex, no "sha256:" prefix), and its size in bytes. When a hash is
// still being computed in the background (large local imports), SHA256 is "" and
// HashPending is true on the enclosing Manifest.
type File struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// Manifest is the immutable provenance record for one download/import. Its JSON shape
// matches docs/model-supply-chain.md verbatim (id, source, repo, revision, format,
// quantization, license, files, security_warnings, downloaded_at) plus additive fields
// LocalHub needs for reproduction (Name/Digest for Ollama, LocalPath for imports,
// HashPending for deferred hashing, SupersedesID for the immutability chain).
//
// The Files/SecurityWarnings JSON persist into manifests.files_json /
// manifests.security_warnings_json; the scalar fields map onto the manifests columns
// (spec-digest §2). Fields with no column (Name, Digest, LocalPath, HashPending,
// SupersedesID) are folded into files_json's envelope by the store so the DDL stays
// unchanged (additive-only migration policy) — see store.go.
type Manifest struct {
	// ID is the manifest uuid (manifests.id PRIMARY KEY). Assigned once; never changes.
	ID string `json:"id"`
	// Source is the provenance kind.
	Source SourceKind `json:"source"`
	// Repo is the HF repo id (owner/name) for hf sources; "" otherwise.
	Repo string `json:"repo,omitempty"`
	// Revision is the exact commit SHA for hf sources (pinned — never a branch/tag);
	// "" for non-hf sources.
	Revision string `json:"revision,omitempty"`
	// Format is gguf | safetensors | pytorch | ollama.
	Format string `json:"format,omitempty"`
	// Quantization is a best-effort label (q4_k_m, tq3_4s, ...); "" when unknown.
	Quantization string `json:"quantization,omitempty"`
	// License is the SPDX-ish license string when known; "" triggers a missing-license warning.
	License string `json:"license,omitempty"`
	// Files is the per-file hash+size list. For Ollama it is empty (Ollama owns the blob;
	// Digest identifies it instead).
	Files []File `json:"files"`
	// SecurityWarnings are the human-readable warnings from the scanner (ADR-004). Never nil
	// in a persisted manifest (marshals as [] when empty).
	SecurityWarnings []string `json:"security_warnings"`
	// DownloadedAt is the RFC3339 UTC timestamp the manifest was created.
	DownloadedAt string `json:"downloaded_at"`

	// ── Additive LocalHub fields (folded into files_json's envelope by the store) ──

	// Name is the Ollama model name (e.g. "qwen2.5:0.5b") for ollama sources.
	Name string `json:"name,omitempty"`
	// Digest is the Ollama model digest (sha256:...) for ollama sources — the reproduce key.
	Digest string `json:"digest,omitempty"`
	// URL is the direct download URL for gguf_url sources (the reproduce key).
	URL string `json:"url,omitempty"`
	// LocalPath is the absolute on-disk path for local_import sources (and the download
	// target for hf/gguf_url, recorded so reproduce/inventory can locate the bytes).
	LocalPath string `json:"local_path,omitempty"`
	// HashPending is true when a large local-import file's sha256 is still being computed in
	// a background job. The manifest is written immediately (so the model appears in
	// inventory with provenance) and finalized later. Reproduce exports flag this clearly.
	HashPending bool `json:"hash_pending,omitempty"`
	// SupersedesID links to the prior manifest for the same logical model when a NEW revision
	// was downloaded (immutability: the old manifest is not mutated; a new one supersedes it).
	SupersedesID string `json:"supersedes_id,omitempty"`
}

// PrimaryHash returns the sha256 to record on the models.sha256 row for a quick integrity
// check (spec-digest §7 "primary artifact hash, or index file hash for sharded"). It
// prefers, in order: the single weight file's hash; the .index.json hash for sharded
// safetensors; otherwise the first file's hash. Returns "" when no hashes are available
// (Ollama, or a hash-pending import), with the "sha256:" prefix omitted to match the
// existing models.sha256 convention used by the inventory scanner (raw hex).
func (m *Manifest) PrimaryHash() string {
	if len(m.Files) == 0 {
		return ""
	}
	// Prefer a lone weight file.
	var indexHash string
	weightCount := 0
	var lastWeightHash string
	for _, f := range m.Files {
		if isIndexFile(f.Path) {
			indexHash = f.SHA256
		}
		if isWeightFile(f.Path) {
			weightCount++
			lastWeightHash = f.SHA256
		}
	}
	if weightCount == 1 && lastWeightHash != "" {
		return lastWeightHash
	}
	if indexHash != "" {
		return indexHash
	}
	return m.Files[0].SHA256
}

// TotalSizeBytes sums the recorded file sizes.
func (m *Manifest) TotalSizeBytes() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.SizeBytes
	}
	return n
}

// normalize fills defaults that every persisted manifest must have: a non-nil warnings
// slice (so JSON is [] not null) and a DownloadedAt stamp.
func (m *Manifest) normalize() {
	if m.SecurityWarnings == nil {
		m.SecurityWarnings = []string{}
	}
	if m.DownloadedAt == "" {
		m.DownloadedAt = time.Now().UTC().Format(time.RFC3339)
	}
}

// Validate checks the invariants a manifest must satisfy before it is persisted. It returns
// a readable reason (CLAUDE.md correctness bar) on failure.
func (m *Manifest) Validate() error {
	if m.ID == "" {
		return fmt.Errorf("manifest has empty id")
	}
	if !m.Source.Valid() {
		return fmt.Errorf("manifest %s: invalid source kind %q", m.ID, m.Source)
	}
	// HF sources MUST be pinned to an exact commit SHA (the core rule: never "latest").
	if m.Source == SourceHF && !looksLikeCommitSHA(m.Revision) {
		return fmt.Errorf(
			"manifest %s: hf source must pin an exact commit SHA in revision (got %q); resolve the ref first",
			m.ID, m.Revision)
	}
	// Non-pending file manifests must have a hash per file.
	if !m.HashPending {
		for _, f := range m.Files {
			if f.SHA256 == "" {
				return fmt.Errorf("manifest %s: file %q has no sha256 and hash_pending is false", m.ID, f.Path)
			}
		}
	}
	return nil
}

// looksLikeCommitSHA reports whether s is a 40-hex-char (or 7+ hex-char short) git commit
// SHA. HF full commit SHAs are 40 hex chars; we accept 7..64 hex to be lenient about
// abbreviated/future forms while still rejecting a branch/tag name like "main".
func looksLikeCommitSHA(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// HashReader streams r through sha256 and returns the lowercase hex digest plus the number
// of bytes read. It is the one hashing primitive the download and background-import paths
// share, so per-file hashing is computed identically everywhere.
func HashReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// MarshalJSON ensures a persisted/exported manifest always has a non-nil warnings slice and
// files slice so consumers never see JSON null for them.
func (m Manifest) MarshalJSON() ([]byte, error) {
	type alias Manifest // avoid recursion
	a := alias(m)
	if a.SecurityWarnings == nil {
		a.SecurityWarnings = []string{}
	}
	if a.Files == nil {
		a.Files = []File{}
	}
	return json.Marshal(a)
}
