package core

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/engines/llamacpp"
	"github.com/localhub/localhub/internal/hardware"
	"github.com/localhub/localhub/internal/instances"
	"github.com/localhub/localhub/internal/residency"
)

// QueuedError is returned by StartInstance when residency admission QUEUED the request (blocked
// on VRAM but not rejected). The API transport classifies it into a 202 Accepted + queue position
// (POST /api/instances async-start contract) rather than an error status. The reason carries the
// numeric blocked_reason.
type QueuedError struct {
	QueueID  string
	Position int
	ReasonS  string
}

func (e *QueuedError) Error() string {
	return fmt.Sprintf("QUEUED: %s (queue id %s, position %d)", e.ReasonS, e.QueueID, e.Position)
}

// Reason returns the numeric blocked_reason.
func (e *QueuedError) Reason() string { return e.ReasonS }

// This file wires the P4.1/P4.2 instance lifecycle into the single service core: start/stop a
// llama.cpp model, list instances, and reconcile on startup. Architecture rule (PLAN.md
// decision 7): all logic lives here; the API/WS transports only marshal in/out.
//
// Phase-4 admission is a TEMPORARY safety pre-check (real admission is Phase 5): refuse a start
// unless the compat verdict at the requested ctx is runs/runs_degraded AND the estimate fits in
// free-1500MiB. TODO(phase5): replace this guard with the residency admit() algorithm.

// Phase4SafetyMarginMiB is the temporary Phase-4 dev-test headroom: a start is refused if the
// estimate exceeds free VRAM minus this margin. It matches the PLAN.md hardware safeguard
// ("never load beyond free VRAM − 1.5 GB during dev tests"). TODO(phase5): superseded by the
// residency admission margin (SAFETY_MARGIN_MIB) once P5 lands.
const Phase4SafetyMarginMiB = 1500

// StartInstanceRequest is the input to StartInstance (POST /api/instances).
type StartInstanceRequest struct {
	ModelID  string `json:"model_id"`
	Engine   string `json:"engine,omitempty"` // "llamacpp" (default/only in Phase 4)
	Ctx      int    `json:"ctx,omitempty"`
	Batch    int    `json:"batch,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Pin      bool   `json:"pin,omitempty"`
	KVQuant  bool   `json:"kv_quant,omitempty"`
	// AllowCPUOffload opts into a GPU/CPU split for this start. Default false = FULL GPU, never
	// offload to CPU (the hard default: llama.cpp -ngl all / Ollama num_gpu forced to all layers).
	// When true, borderline models may split across GPU+CPU (llama.cpp -ngl auto / Ollama num_gpu:-1
	// dynamic). It relaxes only the launch FLAGS, NOT admission: a model whose weights exceed VRAM is
	// still refused by the compat verdict (docs/api-spec.md instances §allow_cpu_offload).
	AllowCPUOffload bool `json:"allow_cpu_offload,omitempty"`
	// Speculative opts into draft-free self-speculative decoding for this start. "" (default) = off,
	// today's flags. "ngram-cache" appends llama.cpp's `--spec-type ngram-cache` (draft-free n-gram
	// self-speculation): no second model, ~zero extra VRAM (a hash pool), so it relaxes NOTHING in
	// admission — it only changes the launch FLAGS and can speed up structured/repetitive output.
	// Any other value is rejected at StartInstance with a reason listing the valid values
	// (compat.ValidateSpeculative; docs/api-spec.md instances §speculative). llama.cpp path only.
	Speculative string `json:"speculative,omitempty"`
}

// InstanceView is the API projection of an instances.Instance (GET /api/instances). It carries
// the state, identity, VRAM numbers, flags, timestamps, and the mandatory reason.
type InstanceView struct {
	ID           string    `json:"id"`
	ModelID      string    `json:"model_id"`
	Engine       string    `json:"engine"`
	State        string    `json:"state"`
	PID          int       `json:"pid,omitempty"`
	Port         int       `json:"port,omitempty"`
	BaseURL      string    `json:"base_url,omitempty"`
	BinaryPath   string    `json:"binary_path,omitempty"`
	ModelPath    string    `json:"model_path,omitempty"`
	Ctx          int       `json:"ctx,omitempty"`
	VRAMEstMiB   int       `json:"vram_est_mib,omitempty"`
	VRAMObsMiB   int       `json:"vram_obs_mib,omitempty"`
	VRAMFreedMiB int       `json:"vram_freed_mib,omitempty"`
	Pinned       bool      `json:"pinned"`
	Priority     int       `json:"priority"`
	LaunchFlags  []string  `json:"launch_flags,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	CreatedAt    time.Time `json:"created_at,omitempty"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	StoppedAt    time.Time `json:"stopped_at,omitempty"`
}

func instanceToView(inst instances.Instance) InstanceView {
	return InstanceView{
		ID: inst.ID, ModelID: inst.ModelID, Engine: string(inst.Engine), State: string(inst.State),
		PID: inst.PID, Port: inst.Port, BaseURL: inst.BaseURL,
		BinaryPath: inst.BinaryPath, ModelPath: inst.ModelPath, Ctx: inst.Ctx,
		VRAMEstMiB: inst.VRAMEstMiB, VRAMObsMiB: inst.VRAMObsMiB, VRAMFreedMiB: inst.VRAMFreedMiB,
		Pinned: inst.Pinned, Priority: inst.Priority, LaunchFlags: inst.LaunchFlags,
		Reason: inst.Reason, CreatedAt: inst.CreatedAt, StartedAt: inst.StartedAt, StoppedAt: inst.StoppedAt,
	}
}

// Instances returns all instances (running + recent), newest first, for GET /api/instances.
func (s *Service) Instances(ctx context.Context) ([]InstanceView, error) {
	if s.deps.InstanceStore == nil {
		return nil, fmt.Errorf("instance store not configured")
	}
	list, err := s.deps.InstanceStore.List(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]InstanceView, 0, len(list))
	for _, inst := range list {
		if inst.Engine == instances.EngineOllama {
			continue // pre-migration shared-daemon rows are no longer a LocalHub runtime.
		}
		out = append(out, instanceToView(inst))
	}
	return out, nil
}

// Instance returns one instance by id (GET /api/instances/{id}).
func (s *Service) Instance(ctx context.Context, id string) (InstanceView, error) {
	if s.deps.InstanceStore == nil {
		return InstanceView{}, fmt.Errorf("instance store not configured")
	}
	inst, err := s.deps.InstanceStore.Get(ctx, id)
	if err != nil {
		return InstanceView{}, err
	}
	if inst.Engine == instances.EngineOllama {
		return InstanceView{}, fmt.Errorf("instance %s belongs to the retired Ollama runtime; import a standalone GGUF and start it with llamacpp instead", id)
	}
	return instanceToView(inst), nil
}

// StartInstance is the Phase-4 model-start entry point. It:
//  1. Resolves the model descriptor + estimator source (reusing the compat path).
//  2. Computes the compat verdict at the requested ctx (SAFETY pre-check — refuse unless
//     runs/runs_degraded, and unless the estimate fits in free-1500MiB).
//  3. Builds the launch plan (binary + flags) via compat.PlanLlamaLaunch (shared logic).
//  4. Spawns via the llama.cpp Runner (REQUESTED→STARTING→HEALTHY, VRAM-delta on stop).
//  5. After HEALTHY: fires a tiny completion + samples the peak VRAM and records it to the
//     calibration store (the measured-beats-estimate loop, P2.5).
//
// It returns the HEALTHY instance view, or an error whose message carries a numeric reason for a
// refusal (safety pre-check) so the API can surface it.
func (s *Service) StartInstance(ctx context.Context, req StartInstanceRequest) (InstanceView, error) {
	if s.deps.InstanceStore == nil {
		return InstanceView{}, fmt.Errorf("instance runtime not configured (instance store missing)")
	}
	if req.ModelID == "" {
		return InstanceView{}, fmt.Errorf("start instance: model_id is required")
	}
	if req.Engine == string(instances.EngineOllama) || s.isRetiredOllamaModel(ctx, req.ModelID) {
		return InstanceView{}, fmt.Errorf("start instance: Ollama is retired; import or download a standalone GGUF and run it with llamacpp instead")
	}
	// Validate the per-start speculative knob up front (empty=off is always valid) so an invalid
	// value is refused with a human reason regardless of the target engine. It is applied only on the
	// llama.cpp launch path below; other engines ignore it (see StartInstanceRequest.Speculative).
	spec, err := compat.ValidateSpeculative(req.Speculative)
	if err != nil {
		return InstanceView{}, fmt.Errorf("start instance: %w", err)
	}
	req.Speculative = spec
	engine := req.Engine
	if engine == "" {
		// Infer the owning engine from the model's inventory source when the caller did not pin
		// one. The OpenAI proxy (ResolveForProxy) always passes Engine:"" — an ollama-source model
		// must route to the shared Ollama daemon (warm), NOT the llama.cpp spawn path (which needs a
		// local GGUF file the ollama row has no local_path for, so it would 502 "no local file").
		// This realizes proxy.go's contract: "an Ollama-registry ref resolves to Ollama inside the
		// descriptor/compat path; do not hard-code engine in the proxy" (P6.1 routing). Non-ollama
		// sources (local_path/huggingface GGUF, safetensors) keep the llama.cpp default unchanged.
		engine = s.engineForModelSource(ctx, req.ModelID)
	}
	// Carry the resolved engine on req so the dispatched engine paths (and their unified admission —
	// fix 2) see the concrete engine, not the caller's empty value. Without this an Ollama/vLLM start
	// reached via Engine:"" would admit as llamacpp (admitForStart's default), mis-estimating the
	// footprint and mis-routing eviction.
	req.Engine = engine

	reqCtx := req.Ctx
	if reqCtx <= 0 {
		reqCtx = s.deps.Cfg.Compatibility.DefaultContext
	}

	// Ollama dispatch (P4.3): a shared-daemon warm, not a spawn. Routed to its own path (compat
	// pre-check still applies inside it).
	if engine == string(instances.EngineOllama) {
		return s.startOllamaInstance(ctx, req, reqCtx)
	}
	// vLLM dispatch (P4.4, contract level: docker/attach/subprocess).
	if engine == string(instances.EngineVLLM) {
		return s.startVLLMInstance(ctx, req, reqCtx)
	}
	if engine != string(instances.EngineLlamaCPP) {
		return InstanceView{}, fmt.Errorf("start instance: engine %q not supported in Phase 4 (llamacpp + ollama + vllm)", engine)
	}

	// --- Warm-route short-circuit (fixes proxy self-shadowing, the P1 /v1 503-on-resident bug). ---
	// If a HEALTHY llama.cpp instance already serves this model, route to it WITHOUT recomputing the
	// compat verdict or re-running admission. WHY this must happen BEFORE the verdict: ResolveForProxy
	// starts with Ctx:0 → DefaultContext, and for a large RESIDENT model the recomputed verdict is
	// evaluated against the hardware snapshot's FREE VRAM — which already EXCLUDES the model's own
	// (e.g. a 9 GB resident of ~13 GB free leaves ~4 GB), so Compat.Evaluate returns wont_run and
	// admitGate REFUSES at its verdict-status gate BEFORE residency's own warm check (admission.go
	// step 1) is ever consulted. That is a false self-shadow: the instance is healthy and the counted
	// per-request relay works fine. Residency's warm match is ctx-INDEPENDENT (model+engine+HEALTHY);
	// we mirror it exactly here and run NO fit math against the model itself. A genuinely COLD start
	// (no healthy instance) falls through to the full verdict + admission path below, unchanged — so
	// correctness is preserved (a real over-budget cold start is still refused/queued).
	//
	// Ctx semantics (v1, intentional + documented): the match ignores ctx, exactly like residency. If
	// the client asked for a larger ctx than the running instance was started with, we STILL route to
	// the running instance at its current ctx rather than restart it — the pragmatic v1 (a ctx-aware
	// re-start is future work). See KNOWN-LIMITATIONS / docs/api-spec.md.
	if s.deps.InstanceStore != nil {
		if list, lerr := s.deps.InstanceStore.List(ctx, true); lerr == nil {
			for i := range list {
				if list[i].State == instances.StateHealthy && list[i].ModelID == req.ModelID &&
					list[i].Engine == instances.EngineLlamaCPP {
					return instanceToView(list[i]), nil // route_loaded: warm HEALTHY instance, no spawn.
				}
			}
		}
	}

	if s.deps.LlamaRunner == nil {
		return InstanceView{}, fmt.Errorf("start instance: llama.cpp runner not configured")
	}

	// --- Resolve descriptor + verdict (reuse the compat machinery). ---
	desc, _, err := s.resolveDescriptor(ctx, CompatRequest{ModelID: req.ModelID, Ctx: reqCtx, Batch: req.Batch})
	if err != nil {
		return InstanceView{}, fmt.Errorf("resolve model %q: %w", req.ModelID, err)
	}
	if desc.LocalPath == "" {
		return InstanceView{}, fmt.Errorf("start instance: model %q has no local file on disk to launch (download it first)", req.ModelID)
	}

	snap, err := s.deps.Hardware.Snapshot(ctx)
	if err != nil {
		return InstanceView{}, fmt.Errorf("read hardware snapshot: %w", err)
	}
	avail := s.engineAvailability(ctx)
	verdict := s.deps.Compat.Evaluate(ctx, compat.Request{
		Descriptor: desc, RequestedCtx: reqCtx, Batch: req.Batch, KVQuant: req.KVQuant,
	}, snap, avail)

	// --- MoE CPU-offload sizing (BEFORE admission; no binary needed). ---
	// For a MoE GGUF under allow_cpu_offload, the expert weights are placed on CPU (`-ncmoe`, emitted
	// by PlanLlamaLaunch below), so admission must size against the REDUCED GPU footprint (total −
	// expert bytes) — that reduction is what lets an oversize MoE fit. MoEOffloadSizing reads only the
	// GGUF metadata (no binary, unlike PlanLlamaLaunch), so it is safe to call before admission. It
	// DECLINES (error) when a MoE opts into offload but its expert bytes are not derivable (compat
	// honest-sizing rule; no speculative admission). A non-MoE / unparseable / offload-off model
	// returns 0 and admission proceeds on the full footprint, unchanged.
	admitVerdict := verdict
	if offloadMiB, moeErr := compat.MoEOffloadSizing(desc, req.AllowCPUOffload); moeErr != nil {
		return InstanceView{}, fmt.Errorf("REFUSED: %v", moeErr)
	} else if offloadMiB > 0 && admitVerdict.EstimatedVRAMMiB > offloadMiB {
		admitVerdict.EstimatedVRAMMiB -= offloadMiB
	}

	// --- Admission (P5.1) — replaces the Phase-4 safety pre-check. ---
	// The verdict must still be runs/runs_degraded (a wont_run/unknown model is never admitted),
	// then residency decides fit: route_loaded (warm) / load_new (fits) / evict_then_load (freed
	// room with a per-eviction VRAM re-read) / queue / reject — each with a numeric reason. This is
	// the SAME admission gate Ollama and vLLM use (fix 2 — one residency policy across all engines).
	warm, evicted, aerr := s.admitGate(ctx, req, reqCtx, admitVerdict, snap)
	if aerr != nil {
		return InstanceView{}, aerr
	}
	if warm != nil {
		return *warm, nil // route_loaded: already HEALTHY-resident, no spawn.
	}
	if s.deps.Residency != nil {
		defer s.deps.Residency.ReleaseReservation(req.ModelID, req.Engine)
	}
	if evicted {
		// evict_then_load: residency already executed the verified evictions; re-read the snapshot so
		// freeBefore reflects the post-eviction state for the calibration footprint.
		if s2, serr := s.deps.Hardware.Snapshot(ctx); serr == nil {
			snap = s2
		}
	}

	// --- Build the launch plan (shared binary/flag selection). ---
	// Runs AFTER admission so a genuine refusal/queue surfaces before the binary-dependent plan (a
	// missing binary is a plan error, not an admission decision). For a MoE offload it re-derives and
	// emits `-ncmoe N`; the reduced footprint was already applied to admission above via
	// MoEOffloadSizing, so a non-derivable MoE was declined before we reached here.
	plan, err := compat.PlanLlamaLaunch(desc, avail, reqCtx, req.KVQuant, req.AllowCPUOffload, req.Speculative)
	if err != nil {
		return InstanceView{}, fmt.Errorf("plan launch for %q: %w", req.ModelID, err)
	}

	// Free VRAM just before load (for the observed-peak footprint computation).
	freeBefore := 0
	if len(snap.GPUs) > 0 {
		freeBefore = snap.GPUs[0].VRAMFreeMiB
	}

	instanceID := uuid.NewString()
	inst, err := s.deps.LlamaRunner.Start(ctx, llamacpp.StartSpec{
		InstanceID:   instanceID,
		ModelID:      req.ModelID,
		ModelPath:    desc.LocalPath,
		MMProjPath:   desc.MMProjPath,
		Binary:       plan.Binary.Path,
		ExtraPath:    s.llamaExtraPathFor(plan.Binary.Path),
		Ctx:          reqCtx,
		Flags:        plan.Flags,
		HasFlashAttn: plan.Binary.HasFlashAttn,
		VRAMEstMiB:   verdict.EstimatedVRAMMiB,
		Pinned:       req.Pin,
		Priority:     req.Priority,
		TTLSeconds:   s.deps.Cfg.Residency.DefaultTTLSeconds,
	})
	if err != nil {
		s.publishInstanceUpdated()
		return InstanceView{}, err
	}
	s.publishInstanceUpdated()

	// --- Calibration: tiny completion + peak sample, then record (best-effort, non-fatal). ---
	go s.calibrateAfterHealthy(instanceID, inst.BaseURL, req.ModelID, reqCtx, plan, freeBefore, snap)

	return instanceToView(inst), nil
}

// phase4SafetyCheck refuses a start unless the verdict is runs/runs_degraded AND the estimate
// fits in free-1500MiB. Every refusal carries a NUMERIC reason (gate). TODO(phase5): replace
// this whole check with the residency admit() algorithm (SAFETY_MARGIN_MIB + eviction).
func (s *Service) phase4SafetyCheck(v compat.Verdict, snap hardware.Snapshot) error {
	if v.Status != compat.StatusRuns && v.Status != compat.StatusRunsDegraded {
		return fmt.Errorf("REFUSED: compatibility verdict is %q (needs 'runs' or 'runs_degraded'). %s",
			v.Status, v.Reason)
	}
	free := 0
	if len(snap.GPUs) > 0 {
		free = snap.GPUs[0].VRAMFreeMiB
	}
	budget := free - Phase4SafetyMarginMiB
	if v.EstimatedVRAMMiB > budget {
		return fmt.Errorf("REFUSED: estimated %d MiB exceeds the Phase-4 budget of %d MiB (free %d MiB − %d MiB safety margin). %s",
			v.EstimatedVRAMMiB, budget, free, Phase4SafetyMarginMiB, v.Reason)
	}
	return nil
}

// engineForModelSource returns the engine that should run modelID when the caller did not pin one
// (req.Engine==""). Routing is FORMAT-based (ADR-002/ADR-004: GGUF→llama.cpp, safetensors/pytorch→
// vLLM) with the ollama SOURCE taking precedence (it warms the shared daemon, which has no local
// GGUF file to spawn). The `models` row carries both `source` and `format` (migration 0001):
//   - source == "ollama"                  → Ollama (shared-daemon warm, P6.1 routing fix).
//   - format == "safetensors" | "pytorch" → vLLM (a safetensors row has no GGUF for llama.cpp;
//     routing it to llama.cpp yields a non-GGUF error).
//   - everything else (gguf, or unknown)  → llama.cpp — the historical default.
//
// A missing row or DB error falls back to the llama.cpp default so the descriptor-resolution step
// downstream produces the proper not-found reason (this helper never fails the start on its own).
// NOTE: when the routed engine (vLLM) is disabled in config, StartInstance's vLLM dispatch returns a
// CLEAR refusal ("vllm runner not configured (is vLLM enabled?)") — NOT the confusing llama.cpp
// non-GGUF error a mis-route would produce.
func (s *Service) engineForModelSource(ctx context.Context, modelID string) string {
	if s.deps.DB == nil || modelID == "" {
		return string(instances.EngineLlamaCPP)
	}
	var source, format string
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT COALESCE(source,''), COALESCE(format,'') FROM models WHERE id = ?`, modelID).Scan(&source, &format)
	if err != nil {
		return string(instances.EngineLlamaCPP)
	}
	if source == "ollama" {
		return string(instances.EngineOllama)
	}
	switch format {
	case "safetensors", "pytorch":
		return string(instances.EngineVLLM)
	}
	return string(instances.EngineLlamaCPP)
}

// isRetiredOllamaModel prevents a persisted pre-migration catalog row from being
// launched through a caller-supplied engine value. Models() hides those rows, but
// this guard also closes direct REST/RPC/proxy starts by ID.
func (s *Service) isRetiredOllamaModel(ctx context.Context, modelID string) bool {
	if s.deps.DB == nil || modelID == "" {
		return false
	}
	var source, format string
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT COALESCE(source,''), COALESCE(format,'') FROM models WHERE id = ?`, modelID).
		Scan(&source, &format)
	return err == nil && (source == "ollama" || format == "ollama")
}

// calibrateAfterHealthy runs the P2.5 calibration loop for a freshly HEALTHY instance: a tiny
// (<=16 token) completion to force full VRAM allocation, then a peak sample, then a record into
// the calibration store keyed by model_id. All best-effort — a failure here never affects the
// running instance.
func (s *Service) calibrateAfterHealthy(instanceID, baseURL, modelID string, reqCtx int, plan compat.LaunchPlan, freeBefore int, snap hardware.Snapshot) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Fire a tiny completion to trigger KV/CUDA-graph allocation before sampling the peak.
	_ = s.deps.LlamaRunner.TinyCompletion(ctx, baseURL, 16)
	peak, err := s.deps.LlamaRunner.RecordPeakVRAM(ctx, instanceID, freeBefore)
	if err != nil || peak <= 0 {
		return
	}
	if s.deps.Calib == nil {
		return
	}
	gpu, driver := "", ""
	if len(snap.GPUs) > 0 {
		gpu = snap.GPUs[0].Name
		driver = snap.GPUs[0].DriverVersion
	}
	_ = s.deps.Calib.Record(ctx, compat.ObservedPeak{
		ModelKey: modelID, PeakVRAMMiB: peak, Ctx: reqCtx,
		KVType: compat.KVTypeName(plan.KVType), GPU: gpu, Driver: driver,
	})
	s.publishInstanceUpdated()
}

// StopInstance stops a running instance (DELETE /api/instances/{id}) with the VRAM-delta gate.
// It returns the stopped instance view; the StopResult's verified/freed detail is folded into
// the instance's reason.
func (s *Service) StopInstance(ctx context.Context, id string) (InstanceView, error) {
	if s.deps.InstanceStore == nil {
		return InstanceView{}, fmt.Errorf("instance store not configured")
	}
	// Read the instance to route the stop to the owning engine's adapter.
	inst, err := s.deps.InstanceStore.Get(ctx, id)
	if err != nil {
		return InstanceView{}, err
	}
	if inst.Engine == instances.EngineOllama {
		return InstanceView{}, fmt.Errorf("stop instance: Ollama is retired; its legacy instance is not managed by LocalHub")
	}

	// Ollama: unload (keep_alive:0) + /api/ps poll + VRAM-delta verify (never kills the daemon).
	if inst.Engine == instances.EngineOllama {
		if s.deps.OllamaRunner == nil {
			return InstanceView{}, fmt.Errorf("stop instance: ollama runner not configured")
		}
		tag, terr := s.ollamaTagForModel(ctx, inst.ModelID)
		if terr != nil {
			return InstanceView{}, fmt.Errorf("stop ollama instance %s: %w", id, terr)
		}
		res, serr := s.deps.OllamaRunner.Stop(ctx, id, tag)
		if serr != nil {
			return InstanceView{}, serr
		}
		s.publishInstanceUpdated()
		s.drainAsync() // capacity freed → re-admit queued requests (fix 1)
		return instanceToView(res.Instance), nil
	}

	// vLLM: docker stop+rm (+verify) or attach detach; routed to its own runner.
	if inst.Engine == instances.EngineVLLM {
		if s.deps.VLLMRunner == nil {
			return InstanceView{}, fmt.Errorf("stop instance: vllm runner not configured")
		}
		res, serr := s.deps.VLLMRunner.Stop(ctx, id)
		if serr != nil {
			return InstanceView{}, serr
		}
		s.publishInstanceUpdated()
		s.drainAsync()
		return instanceToView(res.Instance), nil
	}

	// llama.cpp (default): kill the process with the VRAM-delta gate.
	if s.deps.LlamaRunner == nil {
		return InstanceView{}, fmt.Errorf("llama.cpp runner not configured")
	}
	res, err := s.deps.LlamaRunner.Stop(ctx, id)
	if err != nil {
		return InstanceView{}, err
	}
	s.publishInstanceUpdated()
	s.drainAsync()
	return instanceToView(res.Instance), nil
}

// StopInstanceAsync begins stopping an instance and returns IMMEDIATELY, detaching the actual
// process-stop + VRAM-verify from the HTTP caller's request context (Phase 5 fix 4). Before this,
// a slow VRAM-reclaim poll that outlived the client's timeout left the instance stuck in DRAINING
// even though the process was killed and VRAM reclaimed — the client disconnect cancelled the
// context mid-stop, so the final STOPPED row was never written. Now:
//  1. The instance is transitioned to DRAINING synchronously (fast, in the request ctx) so the
//     API can return 202 with a truthful "draining" view.
//  2. The engine stop (kill + VRAM-delta poll + final STOPPED write) runs in a BACKGROUND goroutine
//     owned by the service, with its OWN timeout context — it completes regardless of the caller.
//  3. A drain pass fires after the stop settles (capacity freed → re-admit queued requests).
//
// It returns the DRAINING (or already-terminal) instance view. A stop of an already-terminal or
// unknown instance returns synchronously without detaching.
func (s *Service) StopInstanceAsync(ctx context.Context, id string) (InstanceView, error) {
	if s.deps.InstanceStore == nil {
		return InstanceView{}, fmt.Errorf("instance store not configured")
	}
	inst, err := s.deps.InstanceStore.Get(ctx, id)
	if err != nil {
		return InstanceView{}, err
	}
	if inst.State.Terminal() {
		return instanceToView(inst), nil
	}

	// Mark DRAINING synchronously when coming from HEALTHY so the 202 view is truthful and the
	// never-kill-active audit trail is recorded. Ollama instances (no owned process) and non-HEALTHY
	// states skip the DRAINING mark and are stopped directly in the background.
	draining := inst
	if inst.Engine != instances.EngineOllama && inst.State == instances.StateHealthy {
		if d, terr := s.deps.InstanceStore.Transition(ctx, id, instances.TransitionParams{
			To:     instances.StateDraining,
			Reason: "stop requested; draining before process stop (async; detached from HTTP caller)",
		}); terr == nil {
			draining = d
			s.publishInstanceUpdated()
		}
	}

	// Detach the real stop from the request context. It runs to completion (final STOPPED write +
	// VRAM verify) even if the HTTP caller disconnects — the fix for the DRAINING-stuck bug.
	// Before the kill, the goroutine honors the DRAINING contract via residency.EvaluateDrain:
	// active in-flight requests are allowed to finish (bounded by drain_timeout_seconds) so a
	// user stop never cuts a live stream mid-generation. Without this wait the DRAINING mark was
	// purely cosmetic — the process was signalled immediately after the transition.
	drainingSince := time.Now()
	go func() {
		drainTimeout := time.Duration(s.deps.Cfg.Residency.DrainTimeoutSeconds) * time.Second
		if drainTimeout <= 0 {
			drainTimeout = residency.DefaultDrainTimeoutSecs * time.Second
		}
		bg, cancel := context.WithTimeout(context.Background(), drainTimeout+5*time.Minute)
		defer cancel()
		s.waitForDrain(bg, id, drainingSince, drainTimeout)
		if _, serr := s.StopInstance(bg, id); serr != nil {
			// Best-effort: log via the event bus is not available; the reconciler will reap a
			// DRAINING-with-dead-pid row on the next tick if this failed after the kill.
			_ = serr
		}
	}()

	return instanceToView(draining), nil
}

// waitForDrain blocks until the instance has no active in-flight requests or the drain timeout
// elapses — the residency.EvaluateDrain contract for user-initiated stops (never-kill-active).
// Nil-safe: with no in-flight tracker configured the decision is an immediate stop (nothing is
// counted, so nothing can be waited for). The context bounds the wait as a last resort.
func (s *Service) waitForDrain(ctx context.Context, id string, drainingSince time.Time, drainTimeout time.Duration) {
	// A typed-nil *InFlightTracker must not reach the interface parameter as a non-nil
	// interface value; pass an untyped nil instead so EvaluateDrain's nil check works.
	var tracker residency.ActiveTracker
	if s.deps.InFlight != nil {
		tracker = s.deps.InFlight
	}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		d := residency.EvaluateDrain(id, tracker, drainingSince, time.Now(), drainTimeout)
		if d.Stop {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ReconcileInstances runs the daemon-startup reconciliation. Called once at boot (main wires it)
// so orphaned/dead child processes recorded in a prior run are re-attached or reaped.
func (s *Service) ReconcileInstances(ctx context.Context) (instances.ReconcileResult, error) {
	if s.deps.Reconciler == nil {
		return instances.ReconcileResult{}, nil
	}
	res, err := s.deps.Reconciler.Reconcile(ctx)
	if err == nil {
		s.publishInstanceUpdated()
	}
	return res, err
}

// llamaExtraPathFor returns the ExtraPath dirs to prepend to PATH for the given binary — the
// CUDA fork's DLL directory. It matches the config: absolute-path candidates get the configured
// ExtraPath; the bare-name winget build gets none.
func (s *Service) llamaExtraPathFor(binaryPath string) []string {
	lc := s.deps.Cfg.Engines.LlamaCPP
	// Only attach ExtraPath to explicit/absolute paths (the CUDA fork), not the PATH-resolved
	// winget build — mirrors llamacpp.NewFromPaths.
	if len(binaryPath) >= 2 && binaryPath[1] == ':' {
		return lc.ExtraPath
	}
	return nil
}

// publishInstanceUpdated announces an instance change on the bus so live transports refetch.
func (s *Service) publishInstanceUpdated() {
	if s.deps.Events != nil {
		s.deps.Events.Publish(Event{Type: EventInstancesUpdated})
	}
}
