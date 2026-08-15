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
	// KVQuant is the LEGACY single-bool KV description: true => q4_0 on BOTH cache sides. Kept
	// for back-compat (existing callers and the UI's simple toggle use it). It is a SHORTHAND for
	// KVTypeK/KVTypeV: when either of those is set explicitly they WIN and this bool is ignored
	// (see startProfile).
	KVQuant bool `json:"kv_quant,omitempty"`
	// KVTypeK / KVTypeV request INDEPENDENT key/value cache dtypes (llama.cpp -ctk/-ctv).
	// Valid: "" (unset), "f16", "q8_0", "q4_0" — validated by compat.ValidateKVCacheType, which
	// refuses tq3_0 because the estimator cannot price it. The validated Qwen3.6-27B MTP profile
	// is kv_type_k=q8_0, kv_type_v=q4_0 — previously inexpressible through this API at all
	// (KNOWN-LIMITATIONS #21). Both the VRAM estimate and the emitted flags follow these.
	KVTypeK string `json:"kv_type_k,omitempty"`
	KVTypeV string `json:"kv_type_v,omitempty"`
	// Slots is llama.cpp `-np` (parallel server slots). 0 => 1. Each slot multiplies the KV cache,
	// so it is admission-relevant, not cosmetic. A draft-mtp start FORCES 1 (the pinned-build
	// acceptance condition) regardless of what is requested here.
	Slots int `json:"slots,omitempty"`
	// SpecDraftMax is `--spec-draft-n-max` for a speculative mode. 0 => omit the flag (engine
	// default). The validated Qwen3.6 MTP profile uses 2. Emitted only when the SELECTED binary
	// advertises the flag (has_spec_draft_max).
	SpecDraftMax int `json:"spec_draft_max,omitempty"`
	// AllowCPUOffload opts into a GPU/CPU split for this start. Default false = FULL GPU, never
	// offload to CPU (the hard default: llama.cpp -ngl all / Ollama num_gpu forced to all layers).
	// When true, borderline models may split across GPU+CPU (llama.cpp -ngl auto / Ollama num_gpu:-1
	// dynamic). It relaxes only the launch FLAGS, NOT admission: a model whose weights exceed VRAM is
	// still refused by the compat verdict (docs/api-spec.md instances §allow_cpu_offload).
	AllowCPUOffload bool `json:"allow_cpu_offload,omitempty"`
	// Embedding starts this llama.cpp instance in embedding-ONLY mode (`--embedding`). It is NOT a
	// superset of a normal start: llama.cpp restricts such a server to the embedding routes, so a
	// generation request to it fails and vice versa. LocalHub therefore treats embedding and
	// generation instances of the same model as DIFFERENT residents — the warm-route match below is
	// mode-aware — and POST /v1/embeddings resolves only to instances started this way.
	Embedding bool `json:"embedding,omitempty"`
	// Pooling is llama.cpp `--pooling` (none|mean|cls|last|rank); "" omits the flag and uses the
	// model default. Meaningful only with Embedding. Validated by compat.ValidatePooling.
	Pooling string `json:"pooling,omitempty"`
	// Reranking starts this llama.cpp instance with `--reranking`. llama.cpp REQUIRES that flag be
	// combined with --embedding and --pooling rank (tools/server/README.md: "Requires a reranker
	// model ... and the --embedding --pooling rank options"), so setting this IMPLIES embedding +
	// pooling rank regardless of what those two fields were set to (compat.LaunchProfile.Normalize
	// does the coupling). The resulting instance answers POST /v1/rerank — NOT
	// /v1/chat/completions and NOT /v1/embeddings, even though it was also started with
	// --embedding — because a reranker returns a relevance SCORE per (query, document) pair, not a
	// reusable vector. That is why the warm-route match below is mode-aware across three states
	// (generation / embedding-only / reranking), not just the two-way embedding check.
	Reranking bool `json:"reranking,omitempty"`
	// Speculative opts into draft-free self-speculative decoding for this start. "" (default) = off,
	// today's flags. "ngram-cache" appends llama.cpp's `--spec-type ngram-cache` (draft-free n-gram
	// self-speculation): no second model, ~zero extra VRAM (a hash pool), so it relaxes NOTHING in
	// admission — it only changes the launch FLAGS and can speed up structured/repetitive output.
	// Any other value is rejected at StartInstance with a reason listing the valid values
	// (compat.ValidateSpeculative; docs/api-spec.md instances §speculative). llama.cpp path only.
	Speculative string `json:"speculative,omitempty"`
	// Manual marks this start as an EXPLICIT operator action (set ONLY by the POST /api/instances
	// handler). It is the sole switch the decommission gate (migration 0014, model_auto_load.go)
	// reads: every implicit/auto path — the OpenAI proxy resolve, the residency queue drain loop, the
	// async jobs worker — leaves this false (their zero value), so a DECOMMISSIONED model
	// (auto_load_disabled=1) is refused there but an operator's direct POST /api/instances still
	// succeeds. Never set this from anything other than a live, explicit admin HTTP call.
	Manual bool `json:"-"`
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

	// --- Truthful monitoring (migration 0010, M5). ---

	// LastHealthAt is the last SUCCESSFUL health probe by the periodic monitor. A state of HEALTHY
	// with a stale LastHealthAt is meaningfully different from a freshly-confirmed one.
	LastHealthAt time.Time `json:"last_health_at,omitempty"`
	// HealthFailures is the count of CONSECUTIVE failed probes. Non-zero on a HEALTHY instance means
	// DEGRADED — the monitor is seeing failures but has not reached the reap threshold.
	HealthFailures int `json:"health_failures,omitempty"`
	// Adopted marks an instance LocalHub ATTACHED to rather than spawned (migration 0012). The UI must
	// show this: a Stop on an adopted row kills a process owned by whatever launched it, so it is
	// refused without an explicit force, and residency never auto-evicts it.
	Adopted bool `json:"adopted,omitempty"`
	// AdoptedSource says HOW it was discovered (e.g. "attach: port 8080, PID 20352").
	AdoptedSource string `json:"adopted_source,omitempty"`

	// ReclaimVerified distinguishes a VERIFIED VRAM reclaim from a timed-out or inconclusive one on
	// a terminal instance. null = no stop attempted; false = the process is gone but reclamation was
	// never proven by an NVML delta. Consumers must not read state=STOPPED alone as "VRAM is back"
	// (M5.5); the paired Reason always spells out which case it was.
	ReclaimVerified *bool `json:"reclaim_verified,omitempty"`
}

func instanceToView(inst instances.Instance) InstanceView {
	return InstanceView{
		ID: inst.ID, ModelID: inst.ModelID, Engine: string(inst.Engine), State: string(inst.State),
		PID: inst.PID, Port: inst.Port, BaseURL: inst.BaseURL,
		BinaryPath: inst.BinaryPath, ModelPath: inst.ModelPath, Ctx: inst.Ctx,
		VRAMEstMiB: inst.VRAMEstMiB, VRAMObsMiB: inst.VRAMObsMiB, VRAMFreedMiB: inst.VRAMFreedMiB,
		Pinned: inst.Pinned, Priority: inst.Priority, LaunchFlags: inst.LaunchFlags,
		Reason: inst.Reason, CreatedAt: inst.CreatedAt, StartedAt: inst.StartedAt, StoppedAt: inst.StoppedAt,
		LastHealthAt: inst.LastHealthAt, HealthFailures: inst.HealthFailures,
		ReclaimVerified: inst.ReclaimVerified,
		Adopted:         inst.Adopted, AdoptedSource: inst.AdoptedSource,
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

// launchFlagsHaveEmbedding reports whether a spawned llama.cpp flag vector includes --embedding.
// The llama.cpp runner persists the FULL spawned flag vector into Instance.LaunchFlags, so the
// flags are the authoritative record of the mode the process is actually in — no schema migration
// and no trust in a remembered request. Shared by launchModeOfFlags below.
func launchFlagsHaveEmbedding(flags []string) bool {
	for _, f := range flags {
		if f == "--embedding" {
			return true
		}
	}
	return false
}

// launchFlagsHaveReranking reports whether a spawned llama.cpp flag vector includes --reranking.
// Mirrors launchFlagsHaveEmbedding exactly (exact-element match), for the same reason: LaunchFlags
// is the authoritative record of what the process was actually started with, not what any caller
// later claims it asked for.
func launchFlagsHaveReranking(flags []string) bool {
	for _, f := range flags {
		if f == "--reranking" {
			return true
		}
	}
	return false
}

// instanceLaunchedForReranking reports whether a running instance was spawned in llama.cpp's
// reranking mode (--reranking, which llama.cpp requires alongside --embedding --pooling rank —
// tools/server/README.md). Mirrors the flags-only check the same way an embedding-mode check would
// at the Instance level; kept for Instance-typed callers even though the warm-route path below
// works from raw LaunchFlags via launchModeOfFlags.
func instanceLaunchedForReranking(inst instances.Instance) bool {
	return launchFlagsHaveReranking(inst.LaunchFlags)
}

// launchMode is the mode a llama.cpp instance is ACTUALLY serving, derived from its launch flags
// (see launchModeOfFlags). Three states, not a bool, because a reranking start ALWAYS also carries
// --embedding (llama.cpp's own requirement), so a two-way embedding check alone cannot distinguish
// a reranker (POST /v1/rerank, relevance scores) from a plain embedding server (POST
// /v1/embeddings, vectors) — conflating them would let a plain embedding request warm-route onto a
// rerank server whose pooling rank output is not a usable embedding vector, or vice versa.
type launchMode int

const (
	modeGeneration launchMode = iota
	modeEmbedding
	modeReranking
)

// String renders a launchMode in the human words used in warm-route refusal messages.
func (m launchMode) String() string {
	switch m {
	case modeEmbedding:
		return "embedding-only"
	case modeReranking:
		return "reranking"
	default:
		return "generation"
	}
}

// launchModeOfFlags derives the mode a llama.cpp process is actually running from its FULL spawned
// flag vector (Instance.LaunchFlags) — the authoritative record, per launchFlagsHaveEmbedding's doc
// comment. Reranking is checked FIRST and wins over embedding: a rerank start always carries
// --embedding too (tools/server/README.md: "Requires ... the --embedding --pooling rank options"),
// so checking embedding first would misclassify every reranking instance as merely embedding-only.
func launchModeOfFlags(flags []string) launchMode {
	if launchFlagsHaveReranking(flags) {
		return modeReranking
	}
	if launchFlagsHaveEmbedding(flags) {
		return modeEmbedding
	}
	return modeGeneration
}

// requestedLaunchMode derives the mode a StartInstanceRequest is asking for, mirroring
// launchModeOfFlags's reranking-wins-over-embedding precedence: Reranking:true implies embedding
// too (compat.LaunchProfile.Normalize enforces this on the profile that actually gets launched), so
// it must be checked before Embedding here as well, or a reranking request would be misclassified
// as a plain embedding request and warm-route onto (or be refused against) the wrong resident mode.
func requestedLaunchMode(req StartInstanceRequest) launchMode {
	if req.Reranking {
		return modeReranking
	}
	if req.Embedding {
		return modeEmbedding
	}
	return modeGeneration
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
	// Decommission gate (migration 0014): a DECOMMISSIONED model refuses every path that is NOT the
	// explicit operator start (Manual=true, set only by the POST /api/instances handler). This is the
	// belt-and-braces safety net — the OpenAI proxy (resolveForProxy) and the residency queue drain
	// loop also refuse earlier/elsewhere so their callers get a more specific response, but every path
	// ultimately funnels through StartInstance, so the gate here is what makes the refusal
	// unconditional regardless of which caller forgot to check first.
	if !req.Manual {
		if reason := s.autoLoadDisabledReason(ctx, req.ModelID); reason != "" {
			return InstanceView{}, fmt.Errorf("REFUSED: %s", reason)
		}
	}
	// Validate every profile-bearing knob up front (all-empty/zero = the historical defaults, always
	// valid) so an invalid value is refused with a human reason regardless of the target engine, and
	// before anything is spawned or queued. The llama.cpp launch path below applies them; Ollama and
	// vLLM ignore the llama.cpp-specific ones (see StartInstanceRequest).
	validated, err := validateStartProfile(req)
	if err != nil {
		return InstanceView{}, fmt.Errorf("start instance: %w", err)
	}
	req = validated
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
				// The mode match (launchModeOfFlags == requestedLaunchMode) is required IN ADDITION
				// to model+engine, and is THREE-WAY, not a two-way embedding bool: an embedding-only
				// llama.cpp server cannot answer a generation request and vice versa
				// (tools/server/README.md), and a reranking server — which ALSO carries --embedding,
				// since llama.cpp requires that combination — answers relevance scores on
				// /v1/rerank, not reusable vectors on /v1/embeddings. Comparing on Embedding alone
				// would let a plain embedding request warm-route onto a resident reranking instance
				// of the same model (or the reverse), so the mode compare must resolve reranking
				// before embedding on both sides (see launchModeOfFlags/requestedLaunchMode).
				if list[i].State == instances.StateHealthy && list[i].ModelID == req.ModelID &&
					list[i].Engine == instances.EngineLlamaCPP &&
					launchModeOfFlags(list[i].LaunchFlags) == requestedLaunchMode(req) {
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
	// ONE profile drives BOTH the verdict below and PlanLlamaLaunch further down (M2.2), so the
	// footprint admission is judged on and the flags actually spawned can never diverge.
	profile := startProfile(req, reqCtx)
	verdict := s.deps.Compat.Evaluate(ctx, compat.Request{
		Descriptor: desc, RequestedCtx: reqCtx, Batch: req.Batch, Profile: profile,
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
		// Residency's own warm check (residency.Admit → route_loaded) matches on model+engine ONLY;
		// it does not know about llama.cpp's embedding-only or reranking modes. Guard it here with
		// the same THREE-WAY mode compare as the pre-admission warm-route loop above: a generation
		// server, an embedding-only server, and a reranking server for the SAME model are three
		// different processes that cannot be substituted for each other (tools/server/README.md —
		// embedding is "restrict to only support embedding use case"; reranking additionally
		// requires --embedding --pooling rank but answers a different route with a different output
		// shape), so silently routing a mismatched request onto the resident instance would send it
		// a request its process cannot answer.
		if residentMode, wantMode := launchModeOfFlags(warm.LaunchFlags), requestedLaunchMode(req); residentMode != wantMode {
			return InstanceView{}, fmt.Errorf("REFUSED: model %q is already resident in %s mode as instance %s. "+
				"A %s llama.cpp server cannot be reused for a %s start — they are different processes answering "+
				"different routes. Stop instance %s first, or drop the mode-changing option(s) to use the resident one",
				req.ModelID, residentMode, warm.ID, residentMode, wantMode, warm.ID)
		}
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
	plan, err := compat.PlanLlamaLaunch(desc, avail, profile)
	if err != nil {
		return InstanceView{}, fmt.Errorf("plan launch for %q: %w", req.ModelID, err)
	}

	// Free VRAM just before load (for the observed-peak footprint computation).
	freeBefore := 0
	if len(snap.GPUs) > 0 {
		freeBefore = snap.GPUs[0].VRAMFreeMiB
	}

	// Four-state residency policy (migration 0015): always_available forces Pin (wired into the
	// EXISTING pin mechanism — eviction's never-evict-pinned gate already refuses a pinned instance,
	// see residencyPolicyForLaunchState's doc comment); always_available/available are exempt from
	// idle-TTL unload (TTLSeconds<=0, the EXISTING "never expires by TTL" behavior). Looked up once,
	// right before the spec that actually carries Pinned/TTLSeconds to the runner.
	launchState := s.launchStateFor(ctx, req.ModelID)
	ttlSeconds, forcePin := residencyPolicyForLaunchState(launchState, s.deps.Cfg.Residency.DefaultTTLSeconds)

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
		Pinned:       req.Pin || forcePin,
		Priority:     req.Priority,
		TTLSeconds:   ttlSeconds,
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
	return s.stopInstance(ctx, id, false)
}

// stopInstance is the synchronous engine-stop implementation. force is carried only from the
// explicit operator ?force=true path for an ADOPTED instance; all internal callers use false.
// Keeping it on the detached path matters: validating force before marking DRAINING and then
// dropping it in the goroutine leaves an adopted row permanently DRAINING without stopping it.
func (s *Service) stopInstance(ctx context.Context, id string, force bool) (InstanceView, error) {
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
	// An ADOPTED process is stopped only when the explicit operator force survives all the way from
	// StopInstanceAsyncForce into this detached synchronous implementation.
	if aerr := guardAdoptedStop(inst, force); aerr != nil {
		return InstanceView{}, aerr
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
	return s.StopInstanceAsyncForce(ctx, id, false)
}

// guardAdoptedStop refuses to stop an ADOPTED instance unless the caller explicitly forced it.
//
// LocalHub did not spawn an adopted process — it belongs to whatever started it, typically a launcher
// script with its own lifecycle (scripts/run-qwen-64k-q4q4.ps1 on the reference box). Killing it on
// LocalHub's own initiative would destroy state LocalHub never owned, so every internal path (TTL
// expiry, the drain loop, a plain DELETE, JSON-RPC) is refused with a reason that names the override.
func guardAdoptedStop(inst instances.Instance, force bool) error {
	if !inst.Adopted || force {
		return nil
	}
	return fmt.Errorf("REFUSED: instance %s was ADOPTED, not started by LocalHub (%s). Stopping it would "+
		"kill PID %d, a process owned by whatever launched it. Re-send with force=true if you really want "+
		"LocalHub to kill it, or stop it where it was started",
		inst.ID, inst.AdoptedSource, inst.PID)
}

// StopInstanceAsyncForce is StopInstanceAsync with an explicit override for ADOPTED instances.
// force=false is every internal caller; force=true is the operator saying "kill it anyway", which the
// API surfaces as an explicit ?force=true.
func (s *Service) StopInstanceAsyncForce(ctx context.Context, id string, force bool) (InstanceView, error) {
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
	if aerr := guardAdoptedStop(inst, force); aerr != nil {
		return InstanceView{}, aerr
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
		if _, serr := s.stopInstance(bg, id, force); serr != nil {
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

// MonitorInstances runs ONE post-HEALTHY process/health reconciliation pass (M5.1/M5.2). Unlike
// ReconcileInstances (startup only), this is driven on a timer for the daemon's whole life, so an
// engine that dies after reaching HEALTHY is reflected in the API/UI without a LocalHub restart.
//
// It publishes an instance.update only when something actually CHANGED, so a quiet box does not
// generate WS traffic every tick.
func (s *Service) MonitorInstances(ctx context.Context) (instances.MonitorResult, error) {
	if s.deps.Monitor == nil {
		return instances.MonitorResult{}, nil
	}
	res, err := s.deps.Monitor.Check(ctx)
	if len(res.Died) > 0 || len(res.Reaped) > 0 {
		s.publishInstanceUpdated()
		// A death or a reap frees VRAM, so queued starts may now fit. Kick a drain rather than
		// waiting up to a full drain interval — the same trigger a normal stop fires.
		s.drainAsync()
	} else if len(res.Degraded) > 0 {
		// State did not change, but health_failures did; refresh so the UI can show DEGRADED.
		s.publishInstanceUpdated()
	}
	return res, err
}

// StartPeriodicMonitor runs MonitorInstances on a ticker until ctx is cancelled. main starts it in a
// goroutine alongside StartPeriodicDrain. The interval is the DOCUMENTED detection latency for the
// M5 gate ("reflected within the documented interval"), so it is passed in explicitly rather than
// hidden: at the default 15 s, a dead engine is FAILED within ~15 s, and an unresponsive one is
// reaped after ReapAfterFailures × interval (~45 s at the defaults).
func (s *Service) StartPeriodicMonitor(ctx context.Context, interval time.Duration, onPass func(instances.MonitorResult), onErr func(error)) {
	if s.deps.Monitor == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultMonitorInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			res, err := s.MonitorInstances(ctx)
			if err != nil && onErr != nil {
				onErr(err)
			} else if onPass != nil && (len(res.Died) > 0 || len(res.Reaped) > 0 || len(res.Degraded) > 0) {
				onPass(res)
			}
		}
	}
}

// DefaultMonitorInterval is the post-HEALTHY health-probe period. 15 s keeps detection latency
// small while costing one cheap HTTP GET per resident instance — /health on a loaded llama-server
// answers in single-digit ms, so this does not perturb inference.
const DefaultMonitorInterval = 15 * time.Second

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
