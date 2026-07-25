// Package inventory discovers local models across the sources LocalHub supports (GGUF
// folders, HuggingFace/safetensors directories, and the Ollama daemon) and normalizes
// them into a single record shape that maps onto the `models` table (spec-digest §2 /
// docs/data-model.md). It also provides an idempotent repository that upserts scan
// results into SQLite.
//
// Scope boundary (P1.4): this package does READ-ONLY discovery + persistence. It does NOT
// parse GGUF metadata beyond the magic/version header (full parse is P2.1 via
// gguf-parser-go), it does NOT hash file contents (see the hash-deferred note below), and
// it does NOT call any engine API itself — the Ollama source is consumed through the
// OllamaLister interface, which the wiring agent adapts to the real engines client.
//
// Hash policy (DELIBERATE — see temp-research/wiring-p14.md for orchestrator sign-off):
// sha256 of GGUF weight files is expensive (13 GB files on the reference box). The scanner
// records sha256 as NULL with the semantic "hash: deferred (computed on download/import in
// Phase 3)". The cheap identity of a local artifact is (path, size, mtime); that tuple is
// what re-scan uses to detect change. A real content hash is only produced by the
// download/import path (internal/manifest, Phase 3), never by a background inventory walk.
package inventory

import "time"

// Source enumerates where a model record came from. Values match the `models.source`
// column semantics in spec-digest §2 (ollama | huggingface | local_path | direct_url).
// The scanner produces only the first three; direct_url is a download-time provenance set
// by internal/manifest in Phase 3.
type Source string

const (
	// SourceOllama is a model known to the Ollama daemon (via OllamaLister).
	SourceOllama Source = "ollama"
	// SourceHuggingFace is a safetensors/pytorch model root discovered on disk
	// (a directory containing config.json + weights). These are typically HF snapshots.
	SourceHuggingFace Source = "huggingface"
	// SourceLocalPath is a loose GGUF file discovered on disk under a configured model dir.
	SourceLocalPath Source = "local_path"
)

// Format enumerates the artifact format. Matches `models.format`
// (gguf | safetensors | pytorch | ollama).
type Format string

const (
	FormatGGUF        Format = "gguf"
	FormatSafetensors Format = "safetensors"
	FormatPyTorch     Format = "pytorch"
	FormatOllama      Format = "ollama"
)

// Model is the normalized inventory record. Field names and semantics map 1:1 onto the
// `models` table columns (spec-digest §2). Fields the scanner cannot know (compat_status,
// recommended_engine, manifest_id) are left zero/nil here and filled by later phases —
// the repository never overwrites them with empty values on re-scan (see repository.go).
//
// Pointer fields model SQL NULL: a nil pointer persists as NULL, distinguishing "unknown"
// from a real empty/zero value (e.g. SizeBytes==nil means we could not stat the artifact,
// which is different from a 0-byte file).
type Model struct {
	// ID is the stable slug / natural key. It is deterministic per source (see natkey.go)
	// so a re-scan of the same artifact produces the same ID and updates rather than
	// duplicates. Maps to models.id (PRIMARY KEY).
	ID string

	// DisplayName is a human-friendly label (filename without extension, HF repo basename,
	// or Ollama model name). Maps to models.display_name (NOT NULL).
	DisplayName string

	// Source is where this record was discovered. Maps to models.source (NOT NULL).
	Source Source

	// Format is the artifact format. Maps to models.format (NOT NULL).
	Format Format

	// Quantization is a best-effort label from a filename/details heuristic (e.g. q4_k_m,
	// q8_0, bf16, tq3_4s). Empty string when unknown — persisted as NULL. Maps to
	// models.quantization.
	Quantization string

	// Repo is the HF repo id when derivable (else empty → NULL). Maps to models.repo.
	Repo string

	// Revision is an HF commit SHA when a .git / refs metadata exists (else empty → NULL).
	// The scanner does NOT invent a revision. Maps to models.revision.
	Revision string

	// LocalPath is the absolute path to the primary artifact (the .gguf file, or the model
	// root directory for safetensors). Empty for Ollama models (Ollama owns the blob).
	// Maps to models.local_path.
	LocalPath string

	// SizeBytes is the total on-disk size of the artifact (single file, or sum of shards
	// for a multi-part / safetensors model), or the Ollama-reported size. nil when
	// unknown. Maps to models.size_bytes.
	SizeBytes *int64

	// SHA256 is intentionally nil for scanner-discovered artifacts (hash-deferred policy,
	// see package doc). For Ollama, the daemon's digest is recorded here when present, as
	// it is a cheap, already-computed identity. Maps to models.sha256.
	SHA256 *string

	// License is the SPDX-ish license string when a LICENSE file or metadata reveals it,
	// else empty → NULL. Maps to models.license.
	License string

	// Architectures/ModelType are captured from a safetensors config.json for downstream
	// use (compat engine). They are NOT columns in the `models` DDL (spec-digest §2 has no
	// architecture column) — they are carried on the record for the wiring layer to place
	// (e.g. into a manifest or a future additive column) and are reported as a DDL gap in
	// temp-research/wiring-p14.md. The repository does not persist them.
	Architectures []string
	ModelType     string

	// Companions are non-standalone files linked to this model (e.g. mmproj vision
	// projectors that live beside a GGUF). They are NOT separate inventory rows. Carried
	// for the detail view / launch flags; not a `models` column, so not persisted by the
	// repository (reported as a DDL gap).
	Companions []Companion

	// Parts lists the shard paths for a multi-part GGUF (-00001-of-000NN). For a
	// single-file model this is nil. Carried for detail; not persisted.
	Parts []string

	// Warnings are human-readable notes attached during discovery (e.g. an orphan mmproj,
	// a config.json that failed to parse). Surfaced to the user; carried, not persisted as
	// a column (there is no warnings column on `models`; download-time warnings live on
	// manifests). Every warning carries a reason (CLAUDE.md correctness bar).
	Warnings []string

	// ModTime is the artifact mtime, part of the cheap change-detection identity
	// (path,size,mtime). Not a `models` column; used only by the repository to decide
	// whether a row changed. Zero for Ollama.
	ModTime time.Time
}

// Companion is a file linked to a model that is not itself a standalone inventory entry.
// The canonical case is an mmproj (multimodal projector) GGUF next to a base GGUF.
type Companion struct {
	Kind      string // "mmproj" (only kind in P1.4)
	Path      string // absolute path
	SizeBytes int64
}

// Skipped records an artifact the scanner deliberately did not turn into a model, with a
// reason. Partial downloads (.part/.tmp/.aria2) and orphan companions land here so the
// caller can log them (TESTING.md#p14: partials must be excluded AND logged).
type Skipped struct {
	Path   string
	Reason string
}

// ScanResult is the full output of one scan pass across all configured sources.
type ScanResult struct {
	// Models are the normalized, standalone inventory rows.
	Models []Model
	// Skipped are artifacts excluded from inventory, with reasons (for logging).
	Skipped []Skipped
	// Errors are non-fatal problems encountered during the walk (unreadable dir, etc.).
	// The scan continues past them; the caller decides how loudly to surface them.
	Errors []error
}

// sizePtr / strPtr are small helpers to build the NULL-able pointer fields.
func sizePtr(n int64) *int64  { return &n }
func strPtr(s string) *string { return &s }
