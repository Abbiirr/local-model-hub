package inventory

import (
	"context"
	"errors"
	"testing"
)

// fakeOllama is a test double for OllamaLister. It returns a canned list (or an error) so
// the scanner's Ollama pass can be exercised without a live daemon or the engines package.
type fakeOllama struct {
	models []OllamaModel
	err    error
}

func (f fakeOllama) ListModels(ctx context.Context) ([]OllamaModel, error) {
	return f.models, f.err
}

func TestScanMergesOllama(t *testing.T) {
	root := buildFixtureTree(t)
	fake := fakeOllama{models: []OllamaModel{
		{Name: "llama3.1:8b", Digest: "sha256:abc123", SizeBytes: 4_800_000_000, Family: "llama", Quant: "Q4_K_M"},
		{Name: "nomic-embed-text:latest", Digest: "sha256:def456", SizeBytes: 270_000_000, Family: "nomic-bert"},
		{Name: "", Digest: "sha256:empty"}, // unnamed entry must be defensively skipped
	}}

	res := New([]string{root}, WithOllama(fake)).Scan(context.Background())

	var ollamaModels []Model
	for _, m := range res.Models {
		if m.Source == SourceOllama {
			ollamaModels = append(ollamaModels, m)
		}
	}
	if len(ollamaModels) != 2 {
		t.Fatalf("want 2 ollama models (unnamed skipped), got %d", len(ollamaModels))
	}

	// Disk models still present alongside Ollama (merge, not replace).
	var diskCount int
	for _, m := range res.Models {
		if m.Source != SourceOllama {
			diskCount++
		}
	}
	if diskCount != 5 {
		t.Errorf("disk models should be unaffected by ollama merge, got %d", diskCount)
	}

	// Check field mapping on the first ollama model.
	m, ok := modelByID(res, ollamaNatKey("llama3.1:8b"))
	if !ok {
		t.Fatalf("llama3.1:8b not found in inventory")
	}
	if m.Format != FormatOllama {
		t.Errorf("format = %q, want ollama", m.Format)
	}
	if m.Quantization != "q4_k_m" {
		t.Errorf("quant = %q, want q4_k_m (from details)", m.Quantization)
	}
	if m.SHA256 == nil || *m.SHA256 != "sha256:abc123" {
		t.Errorf("digest not recorded as sha256: %+v", m.SHA256)
	}
	if m.SizeBytes == nil || *m.SizeBytes != 4_800_000_000 {
		t.Errorf("size not recorded: %+v", m.SizeBytes)
	}
	if m.ModelType != "llama" {
		t.Errorf("family->model_type = %q, want llama", m.ModelType)
	}
}

func TestScanOllamaErrorIsNonFatal(t *testing.T) {
	root := buildFixtureTree(t)
	fake := fakeOllama{err: errors.New("connection refused")}
	res := New([]string{root}, WithOllama(fake)).Scan(context.Background())

	// Disk scan still succeeded.
	if len(res.Models) != 5 {
		t.Errorf("ollama error should not lose disk models, got %d", len(res.Models))
	}
	// The error is surfaced, not swallowed.
	if len(res.Errors) == 0 {
		t.Errorf("ollama error should be recorded in res.Errors")
	}
}

func TestScanNoOllamaWhenNil(t *testing.T) {
	root := buildFixtureTree(t)
	res := New([]string{root}).Scan(context.Background()) // no WithOllama
	for _, m := range res.Models {
		if m.Source == SourceOllama {
			t.Errorf("no ollama source configured but got ollama model %q", m.ID)
		}
	}
}
