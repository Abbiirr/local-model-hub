package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFromManifest_HF(t *testing.T) {
	m := &Manifest{
		Source:   SourceHF,
		Repo:     "owner/model",
		Revision: fakeCommit,
		License:  "apache-2.0",
		Files:    []File{{Path: "model.gguf", SHA256: "abc", SizeBytes: 100}},
	}
	b := FromManifest(m)
	if b.Source != "hf" || b.Commit != fakeCommit || b.Repo != "owner/model" {
		t.Fatalf("HF reproduce bundle wrong: %+v", b)
	}
	if len(b.Files) != 1 || b.Files[0].SHA256 != "abc" {
		t.Fatalf("HF reproduce files wrong: %+v", b.Files)
	}
	if !strings.Contains(b.Provenance, "reproducible") {
		t.Fatalf("expected reproducible provenance, got %q", b.Provenance)
	}
}

func TestFromManifest_Ollama(t *testing.T) {
	b := FromManifest(&Manifest{Source: SourceOllama, Name: "qwen2.5:0.5b", Digest: "sha256:abc"})
	if b.OllamaName != "qwen2.5:0.5b" || b.OllamaDigest != "sha256:abc" {
		t.Fatalf("ollama reproduce wrong: %+v", b)
	}
	if b.ReproduceHint != "ollama pull qwen2.5:0.5b" {
		t.Fatalf("ollama hint wrong: %q", b.ReproduceHint)
	}
}

func TestFromManifest_LocalImport_HasOriginWarning(t *testing.T) {
	m := &Manifest{
		Source:           SourceLocalImport,
		LocalPath:        `K:\models\x.gguf`,
		Files:            []File{{Path: "x.gguf", SHA256: "h", SizeBytes: 1}},
		SecurityWarnings: []string{WarnLocalImportOrigin},
	}
	b := FromManifest(m)
	if b.Source != "local_import" || b.LocalPath != `K:\models\x.gguf` {
		t.Fatalf("local import bundle wrong: %+v", b)
	}
	if !containsWarn(b.Warnings, WarnLocalImportOrigin) {
		t.Fatalf("expected origin warning carried into reproduce bundle, got %v", b.Warnings)
	}
}

func TestFromManifest_Nil_Unknown(t *testing.T) {
	b := FromManifest(nil)
	if b.Source != "unknown" || !strings.Contains(b.Provenance, "pre-existing local file") {
		t.Fatalf("nil manifest should yield unknown/pre-existing provenance, got %+v", b)
	}
}

func TestBuildLocalImportManifest_SyncHash(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tiny.gguf")
	content := []byte("tiny-gguf-content")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := BuildLocalImportManifest(context.Background(), LocalImportRequest{Path: p, DeferHash: false})
	if err != nil {
		t.Fatalf("BuildLocalImportManifest: %v", err)
	}
	if m.HashPending {
		t.Fatal("sync import should not be hash_pending")
	}
	if m.Files[0].SHA256 != sha256hex(content) {
		t.Fatalf("import hash wrong: %q", m.Files[0].SHA256)
	}
	if !containsWarn(m.SecurityWarnings, WarnLocalImportOrigin) {
		t.Fatalf("import manifest must carry origin warning, got %v", m.SecurityWarnings)
	}
}

func TestBuildLocalImportManifest_DeferHash(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.gguf")
	if err := os.WriteFile(p, []byte("pretend-this-is-huge"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := BuildLocalImportManifest(context.Background(), LocalImportRequest{Path: p, DeferHash: true})
	if err != nil {
		t.Fatalf("BuildLocalImportManifest (defer): %v", err)
	}
	if !m.HashPending {
		t.Fatal("deferred import must be hash_pending")
	}
	if m.Files[0].SHA256 != "" {
		t.Fatal("deferred import must not have a hash yet")
	}
	// The background finalizer computes it.
	h, err := HashFile(p)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	if h != sha256hex([]byte("pretend-this-is-huge")) {
		t.Fatalf("HashFile wrong: %q", h)
	}
}
