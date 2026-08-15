package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/compat/gguf"
	"github.com/localhub/localhub/internal/engines/llamacpp"
	"github.com/localhub/localhub/internal/instances"
)

// attach.go ADOPTS an already-running llama-server into LocalHub's instance table (migration 0012).
//
// WHY this path exists at all: the reference box runs the proven Qwen3.6-27B 64K MTP profile from
// scripts/run-qwen-64k-q4q4.ps1 on port 8080. It is healthy and serving — and LocalHub's estimator
// REFUSES that same profile (`wont_run`, ~15.7 GiB estimated) because gguf-parser over-prices a hybrid
// SSM model's KV cache. Asking "would this fit?" about a process that is ALREADY SERVING is both
// unnecessary and, here, wrong. Adoption reads reality instead of predicting it.
//
// Deliberate consequences:
//   - NO ADMISSION CHECK. Admission decides whether to ALLOCATE VRAM; adoption allocates nothing. The
//     process's VRAM is already held and therefore already reflected in every NVML free-VRAM read, so
//     later admissions account for it automatically.
//   - The profile is READ from the process's real command line, not supplied by the caller. What the
//     UI shows is what is actually running.
//   - The measured per-process VRAM is recorded as an observed peak, keyed by the canonical (K,V) pair
//     and the real context. That is the one thing the estimator genuinely needs: after adopting the
//     64K profile once, a later verdict for it is driver truth rather than a bad guess.
//   - Adopted instances are PINNED by default and never auto-evicted, because LocalHub did not create
//     them and cannot know what depends on them.

// AttachRequest asks LocalHub to adopt whatever llama-server is listening on Port.
type AttachRequest struct {
	// Port is the only required field: the listening port of the running server.
	Port int `json:"port"`
	// Host is used to build the base URL LocalHub will call. Defaults to 127.0.0.1. Set it when the
	// server binds a LAN address (the reference launcher binds 192.168.0.73).
	Host string `json:"host,omitempty"`
	// ModelID optionally names the inventory model. When empty it is resolved by matching the
	// process's -m path against the catalog, which is the normal case.
	ModelID string `json:"model_id,omitempty"`
	// Pin defaults to TRUE for an adopted instance; send pin:false to opt out.
	Pin *bool `json:"pin,omitempty"`
	// VRAMMiB is the process's MEASURED VRAM footprint, supplied by the operator.
	//
	// It exists because automatic per-process VRAM reading is NOT available on this platform:
	// `nvidia-smi --query-compute-apps=pid,used_gpu_memory` returns `[N/A]` under the Windows WDDM
	// driver model (verified on the reference box, 2026-07-26), so LocalHub cannot read a process's own
	// footprint there. Rather than silently record nothing — the whole payoff of adoption is turning a
	// real run into calibration data — the operator may state the number they observed.
	//
	// When set (and the process reports a context) it is recorded as an observed peak keyed by the
	// canonical (K,V) pair and the real context, replacing the estimator's guess for that exact
	// profile. When omitted and the platform cannot measure, the adoption still succeeds and the
	// instance's reason says the footprint is UNKNOWN — never a fabricated number.
	VRAMMiB int `json:"vram_mib,omitempty"`
}

// AttachInstance adopts the running server on req.Port. It never spawns, signals, or stops anything.
func (s *Service) AttachInstance(ctx context.Context, req AttachRequest) (InstanceView, error) {
	if req.Port <= 0 || req.Port > 65535 {
		return InstanceView{}, fmt.Errorf("attach: port must be 1-65535, got %d", req.Port)
	}
	if s.deps.InstanceStore == nil {
		return InstanceView{}, fmt.Errorf("attach: instance store not configured")
	}

	// (1) Discover the process: PID, real command line, real profile, real per-process VRAM.
	ap, err := llamacpp.DiscoverByPort(ctx, req.Host, req.Port)
	if err != nil {
		return InstanceView{}, fmt.Errorf("attach: %w", err)
	}

	// (2) Refuse a duplicate adoption. Two rows for one PID would make stop/monitor ambiguous.
	if existing, ok := s.findActiveByPID(ctx, ap.PID); ok {
		return InstanceView{}, fmt.Errorf("attach: PID %d is already tracked as instance %s (%s); "+
			"nothing to do", ap.PID, existing.ID, existing.State)
	}

	// (3) Resolve the inventory model. A model LocalHub does not know cannot be routed to by name, so
	// this is required — but the error says exactly how to fix it.
	modelID := req.ModelID
	if modelID == "" {
		modelID, err = s.modelIDForPath(ctx, ap.ModelPath)
		if err != nil {
			return InstanceView{}, fmt.Errorf("attach: %w", err)
		}
	}

	// (4) Confirm it actually answers. Adopting an unhealthy process would put a lie in the table.
	probe := llamacpp.NewReconcileHealthChecker()
	healthy, identityOK, herr := probe.Check(ctx, instances.Instance{
		BaseURL: ap.BaseURL, ModelPath: ap.ModelPath, Engine: instances.EngineLlamaCPP, PID: ap.PID,
	})
	if herr != nil || !healthy {
		return InstanceView{}, fmt.Errorf("attach: PID %d on port %d did not answer a health probe at %s "+
			"(%v); refusing to adopt a process that is not serving", ap.PID, ap.Port, ap.BaseURL, herr)
	}

	pinned := true // adopted instances are pinned by default: LocalHub did not create them
	if req.Pin != nil {
		pinned = *req.Pin
	}

	// Footprint: prefer what discovery MEASURED; fall back to the operator's stated number. The
	// distinction is recorded in the reason, because "measured by nvidia-smi" and "stated by the
	// operator" are different evidentiary claims and only the first is independent of human error.
	footprintMiB, footprintSource := ap.VRAMMiB, "measured per-process via nvidia-smi"
	if footprintMiB == 0 && req.VRAMMiB > 0 {
		footprintMiB, footprintSource = req.VRAMMiB, "stated by the operator (per-process VRAM is not "+
			"readable on this platform: nvidia-smi reports [N/A] for used_gpu_memory under Windows WDDM)"
	}
	if footprintMiB == 0 {
		footprintSource = "UNKNOWN (nvidia-smi could not report per-process VRAM and none was supplied)"
	}

	source := fmt.Sprintf("attach: port %d, PID %d", ap.Port, ap.PID)
	reason := fmt.Sprintf("adopted an already-running llama-server (PID %d) on %s: ctx=%d, kv=%s/%s%s. "+
		"LocalHub did NOT start this process and did not run admission — its VRAM is already held and is "+
		"reflected in every free-VRAM read. The profile was read from the process's own command line. "+
		"Footprint %s: %s.",
		ap.PID, ap.BaseURL, ap.Ctx, orF16Str(ap.KVTypeK), orF16Str(ap.KVTypeV), specSuffix(ap),
		footprintLabel(footprintMiB), footprintSource)
	if !identityOK {
		reason += " NOTE: the served-model identity probe did not confirm the model path, so the model " +
			"association comes from the command line alone."
	}

	// (5) Register it, then walk the normal lifecycle so the audit trail matches a spawned instance.
	inst, err := s.deps.InstanceStore.Create(ctx, instances.CreateParams{
		ID: uuid.NewString(), ModelID: modelID, Engine: instances.EngineLlamaCPP,
		ModelPath: ap.ModelPath, BinaryPath: ap.BinaryPath, Ctx: ap.Ctx,
		VRAMEstMiB: footprintMiB, Pinned: pinned, Reason: reason,
		Adopted: true, AdoptedSource: source,
		BaseURL: ap.BaseURL, PID: ap.PID, Port: ap.Port,
	})
	if err != nil {
		return InstanceView{}, fmt.Errorf("attach: record adopted instance: %w", err)
	}
	pid, port, base := ap.PID, ap.Port, ap.BaseURL
	if _, err := s.deps.InstanceStore.Transition(ctx, inst.ID, instances.TransitionParams{
		To: instances.StateStarting, Reason: "adopted: process already running",
		PID: &pid, Port: &port, BaseURL: &base, LaunchFlags: ap.Flags,
	}); err != nil {
		return InstanceView{}, fmt.Errorf("attach: mark starting: %w", err)
	}
	obs := footprintMiB
	healthyInst, err := s.deps.InstanceStore.Transition(ctx, inst.ID, instances.TransitionParams{
		To: instances.StateHealthy, Reason: reason, VRAMObsMiB: &obs,
	})
	if err != nil {
		return InstanceView{}, fmt.Errorf("attach: mark healthy: %w", err)
	}

	// (6) Record the MEASURED footprint as a calibration peak. This is the payoff: it replaces the
	// estimator's guess for this exact (model, ctx, K/V) with driver truth, so the profile stops being
	// refused. Best-effort — a calibration write failure must not fail an adoption that succeeded.
	if footprintMiB > 0 && ap.Ctx > 0 {
		key := gguf.CanonicalKVKey(gguf.KVCacheType(ap.KVTypeK), gguf.KVCacheType(ap.KVTypeV))
		_ = s.RecordObservedPeak(ctx, compat.ObservedPeak{
			ModelKey: modelID, PeakVRAMMiB: footprintMiB, Ctx: ap.Ctx, KVType: string(key),
		})
	}

	s.publishInstanceUpdated()
	return instanceToView(healthyInst), nil
}

// findActiveByPID looks for a non-terminal instance already tracking pid.
func (s *Service) findActiveByPID(ctx context.Context, pid int) (instances.Instance, bool) {
	list, err := s.deps.InstanceStore.List(ctx, true)
	if err != nil {
		return instances.Instance{}, false
	}
	for _, i := range list {
		if i.PID == pid && !i.State.Terminal() {
			return i, true
		}
	}
	return instances.Instance{}, false
}

// modelIDForPath maps an on-disk GGUF path to its inventory model id. The comparison is
// case-insensitive and separator-normalised, because a launcher script's path spelling need not match
// the scanner's byte-for-byte on Windows.
func (s *Service) modelIDForPath(ctx context.Context, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("the running process's command line has no -m/--model path, so its model " +
			"cannot be identified; pass model_id explicitly")
	}
	if s.deps.DB == nil {
		return "", fmt.Errorf("database not configured")
	}
	rows, err := s.deps.DB.QueryContext(ctx, `SELECT id, COALESCE(local_path,'') FROM models`)
	if err != nil {
		return "", fmt.Errorf("read inventory: %w", err)
	}
	defer rows.Close()
	want := normalizePath(path)
	for rows.Next() {
		var id, lp string
		if err := rows.Scan(&id, &lp); err != nil {
			return "", err
		}
		if lp != "" && normalizePath(lp) == want {
			return id, nil
		}
	}
	return "", fmt.Errorf("the running process serves %q, which is not in the inventory. Rescan "+
		"(POST /api/models/rescan) if the file is under a configured model_dir, or pass model_id explicitly", path)
}

func normalizePath(p string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), `/`, `\`))
}

// footprintLabel renders the footprint for the human-readable reason, never inventing a number.
func footprintLabel(mib int) string {
	if mib <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d MiB", mib)
}

func orF16Str(s string) string {
	if s == "" {
		return "f16"
	}
	return s
}

func specSuffix(ap llamacpp.AdoptedProcess) string {
	if ap.SpecMode == "" {
		return ""
	}
	if ap.SpecDraftMax > 0 {
		return fmt.Sprintf(", spec=%s(n_max=%d)", ap.SpecMode, ap.SpecDraftMax)
	}
	return ", spec=" + ap.SpecMode
}
