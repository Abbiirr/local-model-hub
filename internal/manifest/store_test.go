package manifest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/localhub/localhub/internal/db"
)

// openTestDB opens a fresh migrated DB (WAL needs a real file).
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	p := filepath.Join(t.TempDir(), "manifest_test.db")
	d, err := db.Open(p)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return d
}

func TestStore_PutGet_RoundTrip(t *testing.T) {
	d := openTestDB(t)
	s := NewStore(d)
	ctx := context.Background()

	m := &Manifest{
		ID:           "mani-1",
		Source:       SourceHF,
		Repo:         "owner/model",
		Revision:     "e3a1c0f1234567890abcdef1234567890abcdef1",
		Format:       "gguf",
		Quantization: "q4_k_m",
		License:      "apache-2.0",
		Files: []File{
			{Path: "model.gguf", SHA256: "abc123", SizeBytes: 400_000_000},
			{Path: "config.json", SHA256: "def456", SizeBytes: 812},
		},
		SecurityWarnings: []string{},
		LocalPath:        `K:\models\owner__model\model.gguf`,
	}
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, "mani-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil for an existing manifest")
	}
	if got.Repo != m.Repo || got.Revision != m.Revision || got.LocalPath != m.LocalPath {
		t.Fatalf("round-trip mismatch: got %+v", got)
	}
	if len(got.Files) != 2 || got.Files[0].SHA256 != "abc123" {
		t.Fatalf("files not round-tripped: %+v", got.Files)
	}
	if got.PrimaryHash() != "abc123" {
		t.Fatalf("PrimaryHash = %q, want the single weight file hash", got.PrimaryHash())
	}
}

// TestStore_Immutable verifies write-once: re-Put of the same id is refused (spec-digest §2
// "Manifests are immutable — never mutate a manifest row"). Gate #p33.
func TestStore_Immutable(t *testing.T) {
	d := openTestDB(t)
	s := NewStore(d)
	ctx := context.Background()

	m := &Manifest{ID: "dup", Source: SourceOllama, Name: "qwen2.5:0.5b", Digest: "sha256:deadbeef"}
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	// A second Put with the same id (even different content) must be refused.
	if err := s.Put(ctx, &Manifest{ID: "dup", Source: SourceOllama, Name: "changed"}); err == nil {
		t.Fatal("expected re-Put of an existing manifest id to be refused (immutability)")
	}
}

// TestStore_OllamaMetaFold verifies the additive Name/Digest fold into files_json survives a
// round-trip (no schema change needed).
func TestStore_OllamaMetaFold(t *testing.T) {
	d := openTestDB(t)
	s := NewStore(d)
	ctx := context.Background()

	m := &Manifest{ID: "oll", Source: SourceOllama, Name: "qwen2.5:0.5b", Digest: "sha256:abc", SecurityWarnings: []string{}}
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, _ := s.Get(ctx, "oll")
	if got.Name != "qwen2.5:0.5b" || got.Digest != "sha256:abc" {
		t.Fatalf("ollama meta not folded/round-tripped: %+v", got)
	}
}

// TestStore_FinalizeHashes verifies the deferred-hash completion path for local imports.
func TestStore_FinalizeHashes(t *testing.T) {
	d := openTestDB(t)
	s := NewStore(d)
	ctx := context.Background()

	m := &Manifest{
		ID:               "imp",
		Source:           SourceLocalImport,
		LocalPath:        `K:\models\big.gguf`,
		HashPending:      true,
		Files:            []File{{Path: "big.gguf", SizeBytes: 1_000_000_000}},
		SecurityWarnings: []string{WarnLocalImportOrigin},
	}
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put (pending): %v", err)
	}
	// Finalize with the computed hash.
	if err := s.FinalizeHashes(ctx, "imp", map[string]string{"big.gguf": "cafebabe"}); err != nil {
		t.Fatalf("FinalizeHashes: %v", err)
	}
	got, _ := s.Get(ctx, "imp")
	if got.HashPending {
		t.Fatal("expected hash_pending cleared after finalize")
	}
	if got.Files[0].SHA256 != "cafebabe" {
		t.Fatalf("expected finalized hash, got %q", got.Files[0].SHA256)
	}
	// A second finalize must refuse (no longer pending).
	if err := s.FinalizeHashes(ctx, "imp", map[string]string{"big.gguf": "other"}); err == nil {
		t.Fatal("expected finalize on a non-pending manifest to be refused")
	}
}

// TestManifest_ValidateHFRequiresCommitSHA guards the pin-the-revision rule.
func TestManifest_ValidateHFRequiresCommitSHA(t *testing.T) {
	m := &Manifest{ID: "x", Source: SourceHF, Repo: "a/b", Revision: "main", Files: []File{{Path: "f", SHA256: "h"}}}
	m.normalize()
	if err := m.Validate(); err == nil {
		t.Fatal("expected validation to refuse an HF manifest pinned to a branch name, not a commit SHA")
	}
	m.Revision = "e3a1c0f1234567890abcdef1234567890abcdef1"
	if err := m.Validate(); err != nil {
		t.Fatalf("expected a real commit SHA to validate, got %v", err)
	}
}
