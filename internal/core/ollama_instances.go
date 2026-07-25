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

	// Ollama models carry no local GGUF path (the daemon owns the blob), so the GGUF estimator
	// returns an "unknown" verdict (empty source). That is not a real incompatibility — Ollama
	// CAN serve the model — so for the unknown case we substitute a CONSERVATIVE size-based
	// estimate derived from the catalog row's size_bytes (the on-disk blob size, which for a
	// GGUF-backed Ollama model is a sound lower bound on the resident VRAM footprint; Ollama
	// full-offloads by default). This keeps the correctness bar (never a false "runs" that OOMs:
	// the size-based number over-estimates slightly, and the real size_vram is re-measured at
	// HEALTHY) while letting a genuinely-fitting Ollama model warm (P6.1 #p61c routing). A
	// runs/runs_degraded verdict from a real estimate (e.g. the sibling local GGUF was found)
	// is used as-is. TODO(agent): a pre-warm /api/show probe could tighten this; size_bytes is
	// the conservative stand-in.
	if verdict.Status == compat.StatusUnknown {
		if sizeMiB := s.ollamaSizeEstimateMiB(ctx, req.ModelID); sizeMiB > 0 {
			verdict.Status = compat.StatusRuns
			verdict.EstimatedVRAMMiB = sizeMiB
			verdict.Reason = fmt.Sprintf("Ollama model (no local GGUF to parse); using a conservative "+
				"size-based estimate of ~%d MiB from the catalog blob size for the admission pre-check. "+
				"The real VRAM footprint is re-measured from Ollama /api/ps size_vram once resident.", sizeMiB)
		}
	}

	// Unified admission (fix 2): the SAME residency gate llama.cpp uses — route_loaded / load_new /
	// evict_then_load / queue / reject (falling back to the Phase-4 pre-check when residency is off).
	// admitForStart passes verdict.EstimatedVRAMMiB (here the size-based fallback) as the precomputed
	// footprint, so admission does NOT re-run the estimator (which would re-derive the unknown Ollama
	// verdict and reject). Engine-specific warm mechanics below are unchanged.
	warm, _, aerr := s.admitGate(ctx, req, reqCtx, verdict, snap)
	if aerr != nil {
		return InstanceView{}, aerr
	}
	if warm != nil {
		return *warm, nil // already HEALTHY-resident in the Ollama daemon.
	}

	freeBefore := 0
	if len(snap.GPUs) > 0 {
		freeBefore = snap.GPUs[0].VRAMFreeMiB
	}

	// Layer placement (num_gpu): the FULL-GPU default forces all layers onto the GPU
	// (ollama.FullGPULayers()), matching the user directive "all full gpu, never offload to cpu" and
	// aligning Ollama with llama.cpp's -ngl all. NOTE this is a deliberate behavior CHANGE: before
	// this the warm sent NO options, so the daemon used its dynamic default (num_gpu -1 = auto-split
	// across GPU+CPU). allow_cpu_offload=true restores that auto-split by sending num_gpu=nil (omitted
	// → daemon dynamic -1). It changes only the launch option, NOT admission.
	var numGPU *int
	if !req.AllowCPUOffload {
		numGPU = ollama.FullGPULayers()
	}

	instanceID := uuid.NewString()
	inst, err := s.deps.OllamaRunner.Warm(ctx, ollama.WarmSpec{
		InstanceID: instanceID,
		ModelID:    req.ModelID,
		OllamaTag:  tag,
		Pinned:     req.Pin,
		Priority:   req.Priority,
		TTLSeconds: s.deps.Cfg.Residency.DefaultTTLSeconds,
		NumGPU:     numGPU,
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

// ollamaSizeEstimateMiB returns a CONSERVATIVE VRAM estimate (MiB) for an Ollama model from its
// catalog size_bytes, or 0 when unknown. The blob size is scaled by a small overhead (weights load
// ~1:1 into VRAM under Ollama full-offload; the +8% covers the KV cache + CUDA context reserve so
// the pre-check stays on the safe/over-estimating side of the correctness bar). It is only used to
// unblock the admission pre-check for an ollama-source model whose GGUF cannot be parsed locally;
// the authoritative footprint is re-measured from /api/ps size_vram at HEALTHY.
func (s *Service) ollamaSizeEstimateMiB(ctx context.Context, modelID string) int {
	if s.deps.DB == nil {
		return 0
	}
	var size sql.NullInt64
	if err := s.deps.DB.QueryRowContext(ctx,
		`SELECT size_bytes FROM models WHERE id = ?`, modelID).Scan(&size); err != nil {
		return 0
	}
	if !size.Valid || size.Int64 <= 0 {
		return 0
	}
	const mib = 1024 * 1024
	// +8% overhead, conservative (over-estimate) to preserve "no false runs that OOMs".
	return int(float64(size.Int64) * 1.08 / float64(mib))
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
