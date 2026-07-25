package inventory

import (
	"context"
	"strings"
)

// OllamaLister is the narrow dependency the inventory scanner needs from the Ollama engine
// client. It is defined HERE (consumer-side interface) rather than imported from
// internal/engines because:
//   - internal/engines is built concurrently by another agent (coordination boundary), and
//   - the scanner must not itself talk HTTP to the daemon (engines package owns clients).
//
// The wiring agent provides a thin adapter from the real Ollama client (which calls
// GET /api/tags for installed models) to this interface. Tests use fakeOllama (ollama_test).
//
// ListModels returns the models the Ollama daemon has installed (its /api/tags view), NOT
// the currently-resident ones (/api/ps) — inventory is about what CAN be served, residency
// is a later concern.
type OllamaLister interface {
	ListModels(ctx context.Context) ([]OllamaModel, error)
}

// OllamaModel is the subset of Ollama's /api/tags entry the scanner consumes. Field names
// mirror the documented Ollama API shape (github.com/ollama/ollama/docs/api.md, "List
// Local Models") — RE-VERIFY against the current Ollama version at wiring time
// (spec-digest §13 note 8: Ollama field shapes are time-sensitive).
type OllamaModel struct {
	// Name is the fully-qualified model tag, e.g. "llama3.1:8b". Required.
	Name string
	// Digest is the sha256 manifest digest Ollama reports (e.g. "sha256:abc..."). Used as
	// the model's SHA256 identity when present — it is already computed, so recording it is
	// free (unlike hashing a 13 GB GGUF ourselves).
	Digest string
	// SizeBytes is the on-disk size Ollama reports for the model.
	SizeBytes int64
	// Family is the model family from details.family (e.g. "llama", "qwen2"). Optional.
	Family string
	// Quant is the quantization level from details.quantization_level (e.g. "Q4_K_M").
	// Optional; used as-is (lowercased) when present, else derived from the name.
	Quant string
}

// modelsFromOllama converts the daemon's listing into normalized inventory records.
func modelsFromOllama(list []OllamaModel) []Model {
	out := make([]Model, 0, len(list))
	for _, om := range list {
		name := strings.TrimSpace(om.Name)
		if name == "" {
			continue // an unnamed entry is unusable as a natural key; skip defensively.
		}
		m := Model{
			ID:          ollamaNatKey(name),
			DisplayName: name,
			Source:      SourceOllama,
			Format:      FormatOllama,
			// Ollama owns the blob; there is no LocalPath we control.
		}
		// Quantization: prefer the daemon-reported quant, else parse the name.
		if q := strings.TrimSpace(om.Quant); q != "" {
			m.Quantization = strings.ToLower(q)
		} else {
			m.Quantization = DetectQuant(name)
		}
		if om.SizeBytes > 0 {
			m.SizeBytes = sizePtr(om.SizeBytes)
		}
		if d := strings.TrimSpace(om.Digest); d != "" {
			m.SHA256 = strPtr(d)
		}
		if fam := strings.TrimSpace(om.Family); fam != "" {
			m.ModelType = fam
		}
		out = append(out, m)
	}
	return out
}
