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
	engine := req.Engine
	if engine == "" {
		engine = string(instances.EngineLlamaCPP)
	}

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

	// --- Admission (P5.1) — replaces the Phase-4 safety pre-check. ---
	// The verdict must still be runs/runs_degraded (a wont_run/unknown model is never admitted),
	// then residency decides fit: route_loaded (warm) / load_new (fits) / evict_then_load (freed
	// room with a per-eviction VRAM re-read) / queue / reject — each with a numeric reason.
	//
	// When residency is not configured (config residency.enabled=false), fall back to the
	// conservative phase-4 pre-check so the daemon still refuses an over-budget start.
	if verdict.Status != compat.StatusRuns && verdict.Status != compat.StatusRunsDegraded {
		return InstanceView{}, fmt.Errorf("REFUSED: compatibility verdict is %q (needs 'runs' or 'runs_degraded'). %s",
			verdict.Status, verdict.Reason)
	}
	if s.deps.Residency != nil {
		decision, aerr := s.admitForStart(ctx, req, reqCtx, verdict.EstimatedVRAMMiB)
		if aerr != nil {
			return InstanceView{}, fmt.Errorf("admission: %w", aerr)
		}
		switch decision.Decision {
		case residency.DecisionRouteLoaded:
			// Already warm: return the existing HEALTHY instance (no spawn).
			if decision.WarmInstance != nil {
				return instanceToView(*decision.WarmInstance), nil
			}
		case residency.DecisionLoadNew, residency.DecisionEvictThenLoad:
			// Room exists (directly or after the verified evictions residency already executed);
			// fall through to the spawn below. Re-read the snapshot so freeBefore reflects the
			// post-eviction state for the calibration footprint.
			if decision.Decision == residency.DecisionEvictThenLoad {
				if s2, serr := s.deps.Hardware.Snapshot(ctx); serr == nil {
					snap = s2
				}
				s.publishQueueUpdated()
			}
		case residency.DecisionQueue:
			// Blocked: nothing spawned; the caller (API) returns 202 + queue position. Carry the
			// numeric blocked_reason + position in a typed QUEUED error the handler classifies.
			s.publishQueueUpdated()
			return InstanceView{}, &QueuedError{QueueID: decision.QueueID, Position: decision.QueuePosition, ReasonS: decision.Reason}
		case residency.DecisionReject:
			return InstanceView{}, fmt.Errorf("REFUSED: %s", decision.Reason)
		}
	} else {
		// Residency disabled → conservative phase-4 pre-check (free − 1.5 GB budget).
		if err := s.phase4SafetyCheck(verdict, snap); err != nil {
			return InstanceView{}, err
		}
	}

	// --- Build the launch plan (shared binary/flag selection). ---
	plan, err := compat.PlanLlamaLaunch(desc, avail, reqCtx, req.KVQuant)
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
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, serr := s.StopInstance(bg, id); serr != nil {
			// Best-effort: log via the event bus is not available; the reconciler will reap a
			// DRAINING-with-dead-pid row on the next tick if this failed after the kill.
			_ = serr
		}
	}()

	return instanceToView(draining), nil
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
