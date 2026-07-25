package core

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/engines/ollama"
)

// This file is the P4.3 core dispatch for Ollama: warm (start) + the mirror sync driver, plus the
// model_id ↔ Ollama-tag mapping the runner/mirror need to keep the instances.model_id FK valid.
// Ollama is a shared daemon LocalHub never spawns or kills; StartInstance(engine=ollama) warms a
// model, StopInstance unloads it (routed in instances.go).

// startOllamaInstance warms an Ollama model into the daemon (P4.3). It reuses the Phase-4 compat
// pre-check philosophy: the verdict must be runs/runs_degraded and the estimate must fit in
// free-1500MiB before we ask the daemon to load. On a runnable verdict it drives the Ollama
// Runner (REQUESTED→STARTING→HEALTHY via /api/ps), then records size_vram as the observed peak in
// the calibration store.
func (s *Service) startOllamaInstance(ctx context.Context, req StartInstanceRequest, reqCtx int) (InstanceView, error) {
	if s.deps.OllamaRunner == nil {
		return InstanceView{}, fmt.Errorf("start instance: ollama runner not configured (is Ollama enabled?)")
	}

	// Resolve the Ollama tag from the catalog row (display_name of the ollama-source model).
	tag, err := s.ollamaTagForModel(ctx, req.ModelID)
	if err != nil {
		return InstanceView{}, fmt.Errorf("start ollama instance: %w", err)
	}

	// --- Compat pre-check (same gate as llama.cpp; every refusal carries a numeric reason). ---
	desc, _, derr := s.resolveDescriptor(ctx, CompatRequest{ModelID: req.ModelID, Ctx: reqCtx, Batch: req.Batch})
	if derr != nil {
		return InstanceView{}, fmt.Errorf("resolve model %q: %w", req.ModelID, derr)
	}
	snap, herr := s.deps.Hardware.Snapshot(ctx)
	if herr != nil {
		return InstanceView{}, fmt.Errorf("read hardware snapshot: %w", herr)
	}
	avail := s.engineAvailability(ctx)
	verdict := s.deps.Compat.Evaluate(ctx, compat.Request{
		Descriptor: desc, RequestedCtx: reqCtx, Batch: req.Batch, KVQuant: req.KVQuant,
	}, snap, avail)
	if err := s.phase4SafetyCheck(verdict, snap); err != nil {
		return InstanceView{}, err
	}

	freeBefore := 0
	if len(snap.GPUs) > 0 {
		freeBefore = snap.GPUs[0].VRAMFreeMiB
	}

	instanceID := uuid.NewString()
	inst, err := s.deps.OllamaRunner.Warm(ctx, ollama.WarmSpec{
		InstanceID: instanceID,
		ModelID:    req.ModelID,
		OllamaTag:  tag,
		Pinned:     req.Pin,
		Priority:   req.Priority,
		TTLSeconds: s.deps.Cfg.Residency.DefaultTTLSeconds,
	})
	if err != nil {
		s.publishInstanceUpdated()
		return InstanceView{}, err
	}
	s.publishInstanceUpdated()

	// Calibration: the observed VRAM was already recorded from size_vram at HEALTHY. Fold it into
	// the calibration store keyed by model_id (measured-beats-estimate loop, P2.5). Best-effort.
	if s.deps.Calib != nil && inst.VRAMObsMiB > 0 {
		gpu, driver := "", ""
		if len(snap.GPUs) > 0 {
			gpu = snap.GPUs[0].Name
			driver = snap.GPUs[0].DriverVersion
		}
		_ = s.deps.Calib.Record(ctx, compat.ObservedPeak{
			ModelKey: req.ModelID, PeakVRAMMiB: inst.VRAMObsMiB, Ctx: reqCtx,
			KVType: "default", GPU: gpu, Driver: driver,
		})
	}
	_ = freeBefore // Ollama reports size_vram directly; no post-hoc peak sample needed.

	return instanceToView(inst), nil
}

// SyncOllamaMirror runs one mirror pass (/api/ps → instance rows). Wired at reconcile time and on
// the periodic interval by main. Returns the mirror result for logging.
func (s *Service) SyncOllamaMirror(ctx context.Context) (ollama.MirrorResult, error) {
	if s.deps.OllamaMirror == nil {
		return ollama.MirrorResult{}, nil
	}
	res, err := s.deps.OllamaMirror.Sync(ctx)
	if err == nil {
		s.publishInstanceUpdated()
	}
	return res, err
}

// ollamaTagForModel returns the Ollama model tag (e.g. "llama3.1:8b") for a catalog model_id. The
// inventory stores the tag as the ollama-source row's display_name. Errors if the model is not an
// Ollama-source row.
func (s *Service) ollamaTagForModel(ctx context.Context, modelID string) (string, error) {
	if s.deps.DB == nil {
		return "", fmt.Errorf("database not configured")
	}
	var displayName, source string
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT display_name, source FROM models WHERE id = ?`, modelID).Scan(&displayName, &source)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("model %q not found in inventory", modelID)
	}
	if err != nil {
		return "", fmt.Errorf("read model %q: %w", modelID, err)
	}
	if source != "ollama" {
		return "", fmt.Errorf("model %q is a %q model, not an Ollama model — use the owning engine to run it", modelID, source)
	}
	if displayName == "" {
		return "", fmt.Errorf("model %q has no Ollama tag recorded", modelID)
	}
	return displayName, nil
}

// ollamaModelIDForTag maps an Ollama /api/ps tag to a catalog model_id (the mirror resolver). It
// looks up the ollama-source row whose display_name matches the tag; ok=false when no such row
// exists (the model is resident but not inventoried — do not mirror it, to keep the FK valid).
// The instances.model_id FK requires the row to exist; this guarantees it.
func (s *Service) ollamaModelIDForTag(ctx context.Context, tag string) (string, bool) {
	if s.deps.DB == nil {
		return "", false
	}
	var id string
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT id FROM models WHERE source = 'ollama' AND display_name = ? LIMIT 1`, tag).Scan(&id)
	if err != nil {
		return "", false
	}
	return id, true
}
