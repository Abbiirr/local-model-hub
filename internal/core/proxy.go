package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/localhub/localhub/internal/instances"
)

// proxy.go is Phase 5 fix 3: the minimal completion pass-through the daemon needs so it can COUNT
// in-flight requests per instance (the never-kill-active substrate). Phase 4 exposed instance
// completions only as direct-to-engine-port calls (the base_url on the instance row), which are
// invisible to the daemon — a streaming instance looked idle to eviction. This adds a thin relay
// (POST /api/instances/{id}/completions → the engine's /v1/chat/completions or /completion) purely
// so the daemon increments/decrements residency.InFlightTracker around each request. Eviction reads
// that counter (an instance with in-flight > 0 is never a victim), so a stream through THIS endpoint
// protects a non-pinned instance from eviction — exactly what the storm exercises.
//
// This is also Phase 6's routing substrate: the relay is deliberately thin (byte passthrough,
// streaming intact) so Phase 6's OpenAI proxy can build on it. Keep it minimal here.

// InstanceRequestHandle is the in-flight accounting handle returned by BeginInstanceRequest. The
// caller (the API relay) MUST call Done exactly once when the proxied request completes (success,
// error, or client disconnect) so the counter never leaks.
type InstanceRequestHandle struct {
	// TargetBaseURL is the engine base URL to relay to (e.g. http://127.0.0.1:40130).
	TargetBaseURL string
	// InstanceID is the instance being served.
	InstanceID string
	// UpstreamModel, when non-empty, is the model name the UPSTREAM engine expects in the relayed
	// OpenAI body — it may differ from the catalog id the client sent. It is set ONLY for the shared
	// Ollama daemon (P6.1 #p61c routing fix): Ollama routes by the `model` field and knows the model
	// by its TAG (e.g. "sam860/deepseek-r1:8b"), not LocalHub's catalog id (e.g.
	// "ollama-sam860-deepseek-r1-8b"), so the relay must rewrite the field to this tag or Ollama 404s.
	// It is EMPTY for llama.cpp/vLLM instances, which own a single loaded model and ignore the field —
	// the relay then passes the client's model through unchanged (byte-identical, prior behavior).
	UpstreamModel string
	done          func()
}

// Done marks the in-flight request complete (decrement + last_used_at touch). Idempotent.
func (h *InstanceRequestHandle) Done() {
	if h != nil && h.done != nil {
		h.done()
		h.done = nil
	}
}

// BeginInstanceRequest opens an in-flight request against a HEALTHY instance: it verifies the
// instance is serving, increments the in-flight counter (so eviction will not select it), stamps
// last_used_at, and returns a handle carrying the engine base URL + a Done closure that decrements.
//
// It returns an error when the instance is unknown, not HEALTHY, or has no base URL (e.g. an Ollama
// mirror row with no owned port — completions to Ollama go through Ollama directly in this phase).
func (s *Service) BeginInstanceRequest(ctx context.Context, id string) (*InstanceRequestHandle, error) {
	if s.deps.InstanceStore == nil {
		return nil, fmt.Errorf("instance store not configured")
	}
	inst, err := s.deps.InstanceStore.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if inst.Engine == instances.EngineOllama {
		return nil, fmt.Errorf("instance %s belongs to the retired Ollama runtime; import a standalone GGUF and run it with llamacpp instead", id)
	}
	if inst.State != instances.StateHealthy {
		return nil, fmt.Errorf("instance %s is %s, not HEALTHY — cannot serve a completion", id, inst.State)
	}
	if inst.BaseURL == "" {
		return nil, fmt.Errorf("instance %s has no base_url to relay to (engine %s exposes no owned port in this phase)", id, inst.Engine)
	}

	// For an Ollama instance, resolve the upstream TAG the daemon expects in the relayed body's
	// `model` field (it differs from LocalHub's catalog id — see UpstreamModel). llama.cpp/vLLM own a
	// single loaded model and ignore the field, so leave it empty (relay passes the client's model
	// through unchanged, preserving the prior byte-identical behavior).
	upstreamModel := ""
	if inst.Engine == instances.EngineOllama {
		if tag, terr := s.ollamaTagForModel(ctx, inst.ModelID); terr == nil {
			upstreamModel = tag
		}
	}

	// Increment the in-flight counter (never-kill-active substrate). Eviction reads this and will
	// not select an instance with count > 0.
	if s.deps.InFlight != nil {
		s.deps.InFlight.IncrementActive(id)
	}
	// Stamp last_used_at so the Ollama recent-active approximation + LRU are accurate.
	s.touchInstanceLastUsed(context.Background(), id)

	handle := &InstanceRequestHandle{
		TargetBaseURL: inst.BaseURL,
		InstanceID:    id,
		UpstreamModel: upstreamModel,
		done: func() {
			if s.deps.InFlight != nil {
				s.deps.InFlight.DecrementActive(id)
			}
			// Touch again on completion so idle-time is measured from the LAST byte, not the first.
			s.touchInstanceLastUsed(context.Background(), id)
		},
	}
	return handle, nil
}

// touchInstanceLastUsed stamps last_used_at on an instance without a state change (best-effort). It
// keeps the eviction LRU + Ollama recent-active window accurate for instances served through the
// completion proxy. A short independent context so a disconnected caller does not abort the touch.
func (s *Service) touchInstanceLastUsed(_ context.Context, id string) {
	if s.deps.InstanceStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	now := time.Now()
	_ = s.deps.InstanceStore.TouchLastUsed(ctx, id, now)
}

// --- P6.1 thin OpenAI proxy: /v1/models projection + model resolution --------------------------
//
// These core methods are the business logic behind the /v1/* handlers (internal/api/proxy.go). The
// handlers stay thin (parse → one core call → marshal/relay); resolution + admission live HERE (the
// one-core rule, PLAN.md decision 7). The byte-passthrough relay itself is in internal/proxy.

// OpenAIModel is one entry in the GET /v1/models list (OpenAI list shape, design §1.2).
type OpenAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`   // always "model"
	Created int64  `json:"created"`  // unix seconds (0 when unknown)
	OwnedBy string `json:"owned_by"` // always "localhub"
}

// OpenAIModels returns the runnable inventory in OpenAI list shape for GET /v1/models (design
// §1.2). A model is INCLUDED iff it is present on disk AND its compat status is empty or
// runs/runs_degraded — a wont_run model is filtered out (advertising a model that can never load
// would produce a guaranteed 503 later). This is a read-only projection over svc.Models.
func (s *Service) OpenAIModels(ctx context.Context) ([]OpenAIModel, error) {
	models, err := s.Models(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]OpenAIModel, 0, len(models))
	for _, m := range models {
		if m.Presence != "present" {
			continue // vanished from disk → never routable
		}
		if m.AutoLoadDisabled {
			continue // decommissioned (migration 0014): advertised as unavailable to OpenAI clients,
			// even though GET /api/models still lists it for the operator.
		}
		switch m.CompatStatus {
		case "", "runs", "runs_degraded":
			// keep (empty = not yet evaluated; runs_degraded loads at reduced ctx but IS runnable)
		default:
			continue // wont_run / unknown / error → filter out (would 503 on load)
		}
		out = append(out, OpenAIModel{
			ID:      m.ID,
			Object:  "model",
			Created: parseCreatedUnix(m.UpdatedAt),
			OwnedBy: "localhub",
		})
	}
	seen := make(map[string]bool, len(out))
	for _, model := range out {
		seen[model.ID] = true
	}
	// A healthy adopted process is runnable even when its backing file is outside the
	// catalog scanner's current roots. Advertise that proven live identity so clients
	// such as Codex can discover the exact model name they already route successfully.
	if s.deps.InstanceStore != nil {
		if live, listErr := s.deps.InstanceStore.List(ctx, true); listErr == nil {
			for _, inst := range live {
				if inst.State != instances.StateHealthy || strings.TrimSpace(inst.ModelID) == "" || seen[inst.ModelID] {
					continue
				}
				out = append(out, OpenAIModel{
					ID:      inst.ModelID,
					Object:  "model",
					Created: inst.StartedAt.Unix(),
					OwnedBy: "localhub",
				})
				seen[inst.ModelID] = true
			}
		}
	}
	for _, model := range s.availableRemoteLlamaCPPModels(ctx) {
		if seen[model.ID] {
			continue
		}
		out = append(out, OpenAIModel{
			ID:      model.ID,
			Object:  "model",
			Created: 0,
			OwnedBy: "localhub",
		})
	}
	return out, nil
}

// parseCreatedUnix derives the OpenAI `created` unix timestamp from a models.updated_at string
// (RFC3339 or SQLite "2006-01-02 15:04:05"). Unparseable → 0 (a valid OpenAI value meaning
// "unknown"); the field is informational only.
func parseCreatedUnix(updatedAt string) int64 {
	updatedAt = strings.TrimSpace(updatedAt)
	if updatedAt == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, updatedAt); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// ProxyBlock is a non-error, fast-return admission block from ResolveForProxy: the model could not
// be served synchronously right now (queued / rejected / wont_run). The /v1/* handler maps it to a
// 503 with the numeric reason (design §1.4 — the sync proxy blocks return FAST, no wait). It is a
// distinct type from an error so the handler can tell "block" (503) from "not found" (404) / "bad
// request" (400) / genuine failure.
type ProxyBlock struct {
	// Reason is the numeric human-readable blocked_reason (the mandatory-reason gate).
	Reason string
	// Kind classifies WHY the block happened (VRAM short / RAM short / decommissioned / GPU busy /
	// mode mismatch / etc.) so the API layer can pick an accurate OpenAI error envelope `type`/`code`
	// WITHOUT re-parsing Reason's human-readable text (bug fix: the envelope used to hard-code
	// "insufficient_vram" for every block, including a decommissioned-model refusal and a ram_blocked
	// refusal — neither is a VRAM problem, and a client keying off `type` would do the wrong thing).
	// Kind is assigned HERE, in core, at the point each ProxyBlock is constructed — never inferred by
	// the API handler (handlers stay thin; classification is business logic — CLAUDE.md).
	Kind ProxyBlockKind
}

// ProxyBlockKind is the closed set of refusal categories a ProxyBlock can carry. Each value maps to
// a distinct OpenAI error envelope `type`/`code` pair in internal/api/proxy.go — adding a category
// here means adding one entry to that mapping, nothing else.
type ProxyBlockKind string

const (
	// ProxyBlockVRAM: genuine VRAM shortage — admission queued/rejected because free (+ reclaimable)
	// VRAM cannot cover the model's footprint + margin (residency's "waiting for VRAM: ..." /
	// "queue (anti-OOM): ..." reasons), or the Phase-4 fallback budget check when residency is
	// disabled ("REFUSED: estimated ... exceeds the Phase-4 budget ..."). This is also the fallback
	// for any reason text this classifier does not recognize (fail SAFE to the pre-existing behavior
	// rather than silently mislabeling an unrecognized refusal as something more specific than known).
	ProxyBlockVRAM ProxyBlockKind = "insufficient_vram"
	// ProxyBlockRAM: the global system-RAM floor blocked a new process/warm (residency admission.go's
	// "ram_blocked: ..." reason, fix 4) — RAM binds before VRAM on a small-RAM box; freeing GPU memory
	// would not help here.
	ProxyBlockRAM ProxyBlockKind = "insufficient_ram"
	// ProxyBlockGPUHot: the GPU thermal ceiling blocked a new load ("gpu_hot: ..."); a transient
	// admission-time guard, not a capacity problem.
	ProxyBlockGPUHot ProxyBlockKind = "gpu_hot"
	// ProxyBlockGPUBusy: the GPU utilization ceiling blocked a new load ("gpu_busy: ...").
	ProxyBlockGPUBusy ProxyBlockKind = "gpu_busy"
	// ProxyBlockQueueFull: the priority class's queue was already at capacity, so the request was
	// rejected outright rather than queued ("queue_full: ..."). Distinct from a plain VRAM/RAM/GPU
	// block: retrying immediately is pointless, but the underlying resource is not necessarily short —
	// the queue itself is saturated.
	ProxyBlockQueueFull ProxyBlockKind = "queue_full"
	// ProxyBlockDecommissioned: the model has auto_load_disabled=1 (migration 0014) and was refused
	// BEFORE any admission math ran (model_auto_load.go autoLoadDisabledReason). Not a resource
	// problem at all — no amount of freed VRAM/RAM fixes this; the operator must explicitly start it
	// or re-enable auto-load.
	ProxyBlockDecommissioned ProxyBlockKind = "model_decommissioned"
	// ProxyBlockIncompatible: the compat verdict is not runs/runs_degraded ("REFUSED: compatibility
	// verdict is ..."), or a MoE CPU-offload footprint could not be honestly derived ("REFUSED: MoE
	// offload sizing not derivable ..." / "REFUSED: cannot estimate VRAM footprint ..."). The model
	// cannot run on this box in this configuration — again, not fixed by freeing VRAM.
	ProxyBlockIncompatible ProxyBlockKind = "model_incompatible"
	// ProxyBlockModeMismatch: the model is already resident in a DIFFERENT launch mode (generation vs
	// embedding vs reranking) than the request needs ("REFUSED: model ... is already resident in ...
	// mode ..."). A client/request-shape problem, not a capacity problem.
	ProxyBlockModeMismatch ProxyBlockKind = "mode_mismatch"
	// ProxyBlockStarting: a duplicate start for the same model is already admitted and becoming
	// visible ("starting: an admitted instance of this model is still becoming visible ..."). A
	// transient race, not a resource shortage — retrying shortly should succeed.
	ProxyBlockStarting ProxyBlockKind = "starting"
)

// classifyBlockReason maps a block's human-readable Reason text to its ProxyBlockKind. It is called
// ONLY at the point resolveForProxy constructs a ProxyBlock (core-owned business logic — CLAUDE.md:
// handlers stay thin), never by the API layer. The reason strings it matches on are NOT incidental
// formatting: every one is a deliberate, stable prefix token this codebase already treats as the
// numeric-reason identifier for that refusal category (ram_blocked / gpu_hot / gpu_busy / queue_full
// are asserted on by existing tests — e.g. internal/residency/admission_ram_test.go — so they are a
// de facto contract, not free text this classifier is guessing at). Unrecognized text intentionally
// falls back to ProxyBlockVRAM (the pre-fix behavior), so an unclassified future reason never regresses
// to something misleading — it just keeps today's (already-shipped) label.
func classifyBlockReason(reason string) ProxyBlockKind {
	switch {
	case strings.Contains(reason, "is decommissioned from automatic loading"):
		return ProxyBlockDecommissioned
	case strings.Contains(reason, "is already resident in") && strings.Contains(reason, "mode"):
		return ProxyBlockModeMismatch
	case strings.Contains(reason, "ram_blocked:"):
		return ProxyBlockRAM
	case strings.Contains(reason, "gpu_hot:"):
		return ProxyBlockGPUHot
	case strings.Contains(reason, "gpu_busy:"):
		return ProxyBlockGPUBusy
	case strings.Contains(reason, "queue_full:"):
		return ProxyBlockQueueFull
	case strings.Contains(reason, "compatibility verdict is"):
		return ProxyBlockIncompatible
	case strings.Contains(reason, "MoE offload sizing not derivable"):
		return ProxyBlockIncompatible
	case strings.Contains(reason, "cannot estimate VRAM footprint"):
		return ProxyBlockIncompatible
	case strings.Contains(reason, "starting: an admitted instance"):
		return ProxyBlockStarting
	default:
		return ProxyBlockVRAM
	}
}

// newProxyBlock builds a ProxyBlock with its Kind classified from reason, so every construction site
// in resolveForProxy carries an accurate Kind without repeating the classification call.
func newProxyBlock(reason string) *ProxyBlock {
	return &ProxyBlock{Reason: reason, Kind: classifyBlockReason(reason)}
}

// ErrProxyModelNotFound is returned by ResolveForProxy when the requested model is not in the
// inventory at all → the handler maps it to 404 model_not_found (design §1.3).
var ErrProxyModelNotFound = errors.New("model not found in inventory")

// ResolveForProxy resolves an OpenAI `model` field to a ready-to-relay instance handle, running the
// SAME residency admission as every other start (via StartInstance — the one-core rule). It returns
// exactly one of:
//   - (handle, nil, nil)  → a HEALTHY instance is serving the model; the caller MUST relay through
//     it and call handle.Done() when the relay finishes (never-kill-active accounting, design §1.6).
//   - (nil, *ProxyBlock, nil) → admission blocked (queued/reject/wont_run); the caller returns a
//     fast 503 with block.Reason. A queued row is CANCELLED here (cancel-on-block, design §1.4) so
//     the departed sync caller never leaves a phantom entry that auto-starts a model nobody wants.
//   - (nil, nil, ErrProxyModelNotFound) → the model is not in inventory → the caller returns 404.
//   - (nil, nil, err) → any other resolution/spawn failure → the caller returns 502/500.
//
// priority maps to residency ClassFromPriority via StartInstanceRequest.Priority (default 1 normal;
// the OpenAI body has no priority field so the handler passes the default). Ctx: 0 → the model's
// configured DefaultContext (the OpenAI body has no context-length field, design §1.3).
func (s *Service) ResolveForProxy(ctx context.Context, modelName string, priority int) (*InstanceRequestHandle, *ProxyBlock, error) {
	return s.resolveForProxy(ctx, modelName, priority, modeGeneration)
}

// ResolveForEmbeddings is the POST /v1/embeddings resolution path. It resolves ONLY to a llama.cpp
// instance started in embedding mode (llama-server spawned with --embedding), because llama.cpp
// restricts such a server to the embedding routes and a generation-mode server cannot answer them
// at all (tools/server/README.md: "--embedding, --embeddings : restrict to only support embedding
// use case; use only with dedicated embedding models"). It has the same three-way return contract
// as ResolveForProxy — see that doc comment for the (handle, nil, nil) / (nil, block, nil) /
// (nil, nil, ErrProxyModelNotFound) / (nil, nil, err) cases.
//
// It must NOT accept a rerank-mode instance: a rerank server also carries --embedding (llama.cpp
// requires --embedding --pooling rank alongside --reranking), so a naive "has --embedding" test
// would wrongly match one. resolveForProxy's mode compare is exact-equality against modeEmbedding,
// never a superset test, so a resident reranking instance is refused here exactly like a resident
// generation instance is.
func (s *Service) ResolveForEmbeddings(ctx context.Context, modelName string, priority int) (*InstanceRequestHandle, *ProxyBlock, error) {
	return s.resolveForProxy(ctx, modelName, priority, modeEmbedding)
}

// ResolveForRerank is the POST /v1/rerank resolution path. It resolves ONLY to a llama.cpp instance
// started in reranking mode (llama-server spawned with --embedding --pooling rank --reranking —
// tools/server/README.md: "--rerank, --reranking : enable reranking endpoint on server ... Requires
// a reranker model ... and the --embedding --pooling rank options"). It has the same three-way
// return contract as ResolveForProxy — see that doc comment for the (handle, nil, nil) /
// (nil, block, nil) / (nil, nil, ErrProxyModelNotFound) / (nil, nil, err) cases. A resident
// embedding-only instance (no --reranking) is refused, not silently reused, by the same exact-mode
// compare that protects ResolveForEmbeddings from a reranking instance.
func (s *Service) ResolveForRerank(ctx context.Context, modelName string, priority int) (*InstanceRequestHandle, *ProxyBlock, error) {
	return s.resolveForProxy(ctx, modelName, priority, modeReranking)
}

// resolveForProxy is the shared body behind ResolveForProxy, ResolveForEmbeddings, and
// ResolveForRerank. mode selects the launch mode the resolved instance MUST be running: it refuses
// (rather than relays to) a remote llama.cpp model whose process mode cannot be verified for any
// non-generation mode, forces the StartInstance request to demand a launch in that mode, and
// belt-and-braces re-checks the resolved instance's actual launch flags — by EXACT mode equality,
// never a superset/substring test — before handing out a handle (residency's warm-route mode check
// is mode-aware separately; this catches any path that isn't).
func (s *Service) resolveForProxy(ctx context.Context, modelName string, priority int, mode launchMode) (*InstanceRequestHandle, *ProxyBlock, error) {
	if strings.TrimSpace(modelName) == "" {
		return nil, nil, fmt.Errorf("proxy resolve: empty model name")
	}
	if remote, ok := s.remoteLlamaCPPModel(modelName); ok {
		if mode != modeGeneration {
			// A remote llama.cpp model is registered as a generation server (config-declared, not
			// spawned by LocalHub), so there is no local launch-flags record to check. LocalHub cannot
			// verify the remote process was actually started in the requested mode, and relaying a
			// non-generation request to a generation-mode server would just get rejected upstream —
			// refuse here instead of forwarding a request that cannot succeed.
			return nil, &ProxyBlock{
				Reason: fmt.Sprintf(
					"REFUSED: remote llama.cpp model %q is registered as a generation server; LocalHub cannot verify it was started in %s mode",
					remote.ID, mode),
				Kind: ProxyBlockModeMismatch,
			}, nil
		}
		if err := s.verifyRemoteLlamaCPPModel(ctx, remote); err != nil {
			return nil, nil, fmt.Errorf("remote llama.cpp model %q is unavailable: %w", remote.ID, err)
		}
		return &InstanceRequestHandle{
			TargetBaseURL: remote.BaseURL,
			InstanceID:    "remote-llamacpp:" + remote.ID,
			UpstreamModel: remote.UpstreamModel,
			done:          func() {},
		}, nil, nil
	}

	// Decommission gate (migration 0014), checked BEFORE StartInstance is called: a DECOMMISSIONED
	// model must never be auto-started by an OpenAI client (/v1/chat/completions, /v1/completions,
	// /v1/responses, /v1/embeddings, /v1/rerank all funnel through this one resolver). StartInstance
	// carries the same refusal as a safety net (Manual defaults to false here), but checking here
	// first avoids the descriptor-resolution + compat-verdict work for a request that can never
	// proceed, and returns the fast 503 ProxyBlock shape this path is documented to use.
	if reason := s.autoLoadDisabledReason(ctx, modelName); reason != "" {
		return nil, newProxyBlock(reason), nil
	}

	// Run the full P5 start flow: compat verdict gate → Admit → route_loaded / load_new /
	// evict_then_load (health-waits the cold start to HEALTHY) / queue / reject. Engine "" lets
	// StartInstance default to llamacpp (an Ollama-registry ref resolves to Ollama inside the
	// descriptor/compat path); do not hard-code engine in the proxy (design §1.3).
	inst, err := s.StartInstance(ctx, StartInstanceRequest{
		ModelID:   modelName,
		Engine:    "",
		Priority:  priority,
		Ctx:       0,
		Embedding: mode == modeEmbedding,
		Reranking: mode == modeReranking,
	})
	if err != nil {
		// Queued → fast 503, and cancel the phantom queue row (cancel-on-block, design §1.4).
		var qErr *QueuedError
		if errors.As(err, &qErr) {
			if qErr.QueueID != "" {
				// Best-effort: the sync caller is gone; leaving the row would let it auto-start a
				// model nobody is waiting for. A cancel failure is non-fatal to the 503.
				_ = s.CancelQueueEntry(context.Background(), qErr.QueueID)
			}
			return nil, newProxyBlock(qErr.Reason()), nil
		}
		// Reject / wont_run refusals carry a numeric reason and are a fast 503 block (not a 500).
		msg := err.Error()
		if strings.Contains(msg, "REFUSED:") {
			return nil, newProxyBlock(msg), nil
		}
		// Model not in inventory → 404. resolveDescriptor wraps the miss as "resolve model %q: ...";
		// treat a resolve-miss as not-found so the handler returns 404, not 502.
		if isModelNotFound(msg) {
			return nil, nil, ErrProxyModelNotFound
		}
		return nil, nil, err
	}

	// Belt-and-braces mode check: a generation server, an embedding-only server, and a reranking
	// server for the same model are three different processes that cannot be substituted for each
	// other (see launchMode's doc comment in instances.go). Residency's warm-route admission is
	// mode-aware separately; this catches any path that isn't, BEFORE the caller is handed a
	// servable handle. The compare is EXACT mode equality (never a superset test) — a reranking
	// instance also carries --embedding, so a naive "has --embedding" check would wrongly accept it
	// here for mode == modeEmbedding; launchModeOfFlags resolves reranking before embedding, so the
	// two modes never alias.
	if mode != modeGeneration {
		if actual := launchModeOfFlags(inst.LaunchFlags); actual != mode {
			return nil, nil, fmt.Errorf("model %q resolved to instance %s, which is running in %s mode (requested %s mode); these are different llama.cpp processes answering different routes and cannot be substituted for each other — stop instance %s and start the model in %s mode", modelName, inst.ID, actual, mode, inst.ID, mode)
		}
	}

	// HEALTHY → open an in-flight request against it (increments the never-kill-active counter,
	// stamps last_used_at). The caller relays then calls handle.Done().
	handle, herr := s.BeginInstanceRequest(ctx, inst.ID)
	if herr != nil {
		return nil, nil, fmt.Errorf("proxy resolve: instance %s became unservable after admission: %w", inst.ID, herr)
	}
	return handle, nil, nil
}

// instanceIsEmbedding reports whether view is a llama.cpp instance running in embedding mode. The
// llama.cpp runner persists the FULL spawned flag vector into Instance.LaunchFlags (see
// internal/engines/llamacpp/runner.go), so scanning for the exact element "--embedding" is the
// authoritative, migration-free record of the mode the process is ACTUALLY in — no separate schema
// field can drift from what was really passed to llama-server.
func instanceIsEmbedding(view InstanceView) bool {
	for _, flag := range view.LaunchFlags {
		if flag == "--embedding" {
			return true
		}
	}
	return false
}

// isModelNotFound recognizes the "model does not exist in inventory" family of resolve errors so
// ResolveForProxy can return a 404 rather than a 502. It is intentionally conservative — only clear
// not-found phrasings map to 404; everything else stays a server error.
func isModelNotFound(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "not found") ||
		strings.Contains(m, "no such model") ||
		strings.Contains(m, "unknown model") ||
		strings.Contains(m, "no model") ||
		strings.Contains(m, "does not exist")
}

// priorityFromString parses an optional priority string (query/body) into 0/1/2, defaulting to 1
// (normal) on empty/invalid. Shared by the proxy and jobs paths. Clamped to the valid 0..2 range so
// a caller cannot inject an out-of-range class.
func priorityFromString(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 1
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 1
	}
	if n < 0 {
		return 0
	}
	if n > 2 {
		return 2
	}
	return n
}
