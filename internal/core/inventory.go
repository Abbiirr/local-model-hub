package core

import (
	"context"
	"fmt"
	"sync"

	"github.com/localhub/localhub/internal/engines/ollama"
	"github.com/localhub/localhub/internal/inventory"
)

// ModelSummary is the GET /api/models projection of a `models` row: the normalized
// inventory the dashboard lists. It is read straight from the catalog (the DB), NOT by
// re-walking disk on every request — RescanModels owns the walk. Presence (migration 0002)
// lets the UI show models that vanished from disk without deleting the row.
type ModelSummary struct {
	ID           string `json:"id"`
	DisplayName  string `json:"display_name"`
	Source       string `json:"source"` // ollama | huggingface | local_path | direct_url
	Format       string `json:"format"` // gguf | safetensors | pytorch | ollama
	Quantization string `json:"quantization,omitempty"`
	Repo         string `json:"repo,omitempty"`
	LocalPath    string `json:"local_path,omitempty"`
	SizeBytes    *int64 `json:"size_bytes,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
	CompatStatus string `json:"compat_status,omitempty"`
	Presence     string `json:"presence"` // present | missing
	MissingSince string `json:"missing_since,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// RescanResult summarizes a POST /api/models/rescan pass. Every count and the Ollama
// reachability note carry enough for the caller to explain what happened (CLAUDE.md
// correctness bar: a scan that silently skipped Ollama must say so).
type RescanResult struct {
	Inserted      int                 `json:"inserted"`
	Updated       int                 `json:"updated"`
	Vanished      []string            `json:"vanished"`      // catalog rows not found this pass (marked missing)
	NewlyMissing  []string            `json:"newly_missing"` // subset that transitioned present -> missing now
	SkippedCount  int                 `json:"skipped_count"` // partials/orphans excluded from inventory
	Skipped       []inventory.Skipped `json:"skipped,omitempty"`
	ScanErrors    []string            `json:"scan_errors,omitempty"` // non-fatal walk errors + Ollama-down note
	OllamaScanned bool                `json:"ollama_scanned"`        // false when Ollama was unreachable/omitted
	ModelCount    int                 `json:"model_count"`           // total rows in catalog after the pass
}

// OllamaInvAdapter adapts the read-only engines Ollama client to inventory.OllamaLister and
// records whether the most recent ListModels call succeeded. RescanModels reads LastReachable
// to decide whether `ollama` belongs in the reconcile scanned-sources set (so a down daemon
// never marks Ollama rows missing) and to fill RescanResult.OllamaScanned.
//
// It lives in core (the wiring layer), not in internal/inventory or internal/engines, to keep
// both package boundaries clean (inventory must not import engines).
type OllamaInvAdapter struct {
	client *ollama.Client
	mu     sync.Mutex
	// lastReachable is set by each ListModels call: true on success, false on error.
	lastReachable bool
	// called records whether ListModels has run at least once this process (before any
	// scan, reachability is unknown → treated as not-scanned).
	called bool
}

// NewOllamaInvAdapter wraps a live Ollama client. Pass nil to get a nil adapter (caller
// should then not wire WithOllama).
func NewOllamaInvAdapter(c *ollama.Client) *OllamaInvAdapter {
	return &OllamaInvAdapter{client: c}
}

// ListModels implements inventory.OllamaLister via GET /api/tags (read-only). It records
// reachability for the reconcile decision. On error it returns the error (the scanner logs
// it and continues — Ollama being down must not fail the whole scan) AND marks LastReachable
// false so the caller excludes `ollama` from reconciliation.
func (a *OllamaInvAdapter) ListModels(ctx context.Context) ([]inventory.OllamaModel, error) {
	tags, err := a.client.ListModels(ctx)
	a.mu.Lock()
	a.called = true
	a.lastReachable = err == nil
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]inventory.OllamaModel, 0, len(tags))
	for _, t := range tags {
		out = append(out, inventory.OllamaModel{
			Name:      t.Name,
			Digest:    t.Digest,
			SizeBytes: t.Size,
			Family:    t.Details.Family,
			Quant:     t.Details.QuantizationLevel,
		})
	}
	return out, nil
}

// LastReachable reports whether the most recent ListModels succeeded. Returns false if never
// called.
func (a *OllamaInvAdapter) LastReachable() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.called && a.lastReachable
}

// Models returns the current inventory snapshot for GET /api/models. It reads the catalog
// (DB), not disk. Ordered by display_name for a stable UI listing.
func (s *Service) Models(ctx context.Context) ([]ModelSummary, error) {
	if s.deps.DB == nil {
		return nil, fmt.Errorf("database not configured")
	}
	const q = `
SELECT id, display_name, source, format,
       COALESCE(quantization,''), COALESCE(repo,''), COALESCE(local_path,''),
       size_bytes, COALESCE(sha256,''), COALESCE(compat_status,''),
       COALESCE(presence,'present'), COALESCE(missing_since,''), COALESCE(updated_at,'')
FROM models
ORDER BY display_name COLLATE NOCASE, id`
	rows, err := s.deps.DB.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query models: %w", err)
	}
	defer rows.Close()
	var out []ModelSummary
	for rows.Next() {
		var m ModelSummary
		var size *int64
		if err := rows.Scan(
			&m.ID, &m.DisplayName, &m.Source, &m.Format,
			&m.Quantization, &m.Repo, &m.LocalPath,
			&size, &m.SHA256, &m.CompatStatus,
			&m.Presence, &m.MissingSince, &m.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan model row: %w", err)
		}
		m.SizeBytes = size
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model rows: %w", err)
	}
	return out, nil
}

// RescanModels performs a fresh disk (+Ollama, if reachable) scan, upserts into the catalog,
// then reconciles vanished rows (marks them missing, never deletes). Ollama being
// unreachable does not fail the scan — it is logged, recorded in the summary, and excluded
// from reconciliation so Ollama rows are never marked missing on a transient outage.
func (s *Service) RescanModels(ctx context.Context) (RescanResult, error) {
	if s.deps.Inventory == nil || s.deps.InvRepo == nil {
		return RescanResult{}, fmt.Errorf("inventory scanner/repository not configured")
	}

	res := s.deps.Inventory.Scan(ctx)

	stats, err := s.deps.InvRepo.Upsert(ctx, res.Models)
	if err != nil {
		return RescanResult{}, fmt.Errorf("persist scan: %w", err)
	}

	sources := s.scannedSources()
	rec, err := s.deps.InvRepo.Reconcile(ctx, res.Models, sources)
	if err != nil {
		return RescanResult{}, fmt.Errorf("reconcile catalog: %w", err)
	}

	count, _ := s.deps.InvRepo.Count(ctx)

	out := RescanResult{
		Inserted:      stats.Inserted,
		Updated:       stats.Updated,
		Vanished:      rec.VanishedIDs,
		NewlyMissing:  rec.NewlyMissing,
		SkippedCount:  len(res.Skipped),
		Skipped:       res.Skipped,
		OllamaScanned: s.ollamaScanned(),
		ModelCount:    count,
	}
	for _, e := range res.Errors {
		out.ScanErrors = append(out.ScanErrors, e.Error())
	}
	if s.deps.OllamaInv != nil && !s.ollamaScanned() {
		out.ScanErrors = append(out.ScanErrors,
			"ollama daemon unreachable this pass; ollama models not refreshed and ollama rows left untouched (not marked missing)")
	}

	// Announce the catalog change so live transports (WS) tell clients to refetch
	// /api/models. Best-effort + nil-safe; a rescan never blocks on subscribers.
	s.deps.Events.Publish(Event{Type: EventModelsUpdated})

	return out, nil
}

// scannedSources is the set of inventory sources actually covered by the last scan and thus
// safe to reconcile. Disk sources (local_path, huggingface) are ALWAYS scanned. Ollama is
// only included when the adapter was wired AND its last ListModels succeeded — otherwise a
// down daemon would make every Ollama row look vanished.
func (s *Service) scannedSources() []inventory.Source {
	sources := []inventory.Source{inventory.SourceLocalPath, inventory.SourceHuggingFace}
	if s.ollamaScanned() {
		sources = append(sources, inventory.SourceOllama)
	}
	return sources
}

func (s *Service) ollamaScanned() bool {
	return s.deps.OllamaInv != nil && s.deps.OllamaInv.LastReachable()
}
