package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/localhub/localhub/internal/core"
	"github.com/localhub/localhub/internal/proxy"
)

// proxy.go hosts the THIN P6.1 OpenAI-proxy handlers (design §1). They parse → one core call →
// relay/marshal; ALL resolution + admission business logic lives in core (core.ResolveForProxy /
// core.OpenAIModels — the one-core rule, PLAN.md decision 7). The byte-passthrough relay is in
// internal/proxy. These are inference endpoints (not the dangerous admin dev endpoints); the same
// auth middleware + loopback bind cover them. Engine dev endpoints (/sleep, /wake_up,
// /collective_rpc) are NEVER forwarded — the proxy only relays the allowlisted OpenAI
// inference routes documented in docs/security.md.
//
// WIRING (integrator: paste into server.go routes):
//   mux.HandleFunc("GET /v1/models", s.handleV1Models)
//   mux.HandleFunc("POST /v1/chat/completions", s.handleV1ChatCompletions)
//   mux.HandleFunc("POST /v1/completions", s.handleV1Completions)
//   mux.HandleFunc("POST /v1/embeddings", s.handleV1Embeddings)
//   mux.HandleFunc("POST /v1/rerank", s.handleV1Rerank)
//
// SECURITY NOTE (verified against internal/api/middleware.go authMiddleware): the protected-prefix
// check is `strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/v1/")`. llama.cpp also
// exposes a bare POST /rerank (tools/server/README.md), but a bare "/rerank" path matches NEITHER
// prefix, so registering it would create an UNAUTHENTICATED route even when server.admin_token is
// set. Only POST /v1/rerank is registered here; do not add a bare /rerank route without first
// updating authMiddleware's protected-prefix check.

// openAIListResponse is the GET /v1/models envelope (OpenAI list shape, design §1.2).
type openAIListResponse struct {
	Object string             `json:"object"` // always "list"
	Data   []core.OpenAIModel `json:"data"`
	// Models is the Codex model-metadata envelope. Codex custom providers query the
	// same /v1/models route but expect {"models":[...]}; retaining Data preserves
	// the standard OpenAI list response for every other client.
	Models []codexModelInfo `json:"models"`
}

type codexReasoningEffort struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

type codexTruncationPolicy struct {
	Mode  string `json:"mode"`
	Limit int64  `json:"limit"`
}

// codexModelInfo mirrors the required subset of Codex's public ModelInfo wire
// schema. LocalHub supplies conservative local-model capabilities rather than
// letting Codex fall back to cloud-model metadata.
type codexModelInfo struct {
	Slug                              string                 `json:"slug"`
	DisplayName                       string                 `json:"display_name"`
	Description                       string                 `json:"description"`
	DefaultReasoningLevel             *string                `json:"default_reasoning_level,omitempty"`
	SupportedReasoningLevels          []codexReasoningEffort `json:"supported_reasoning_levels"`
	ShellType                         string                 `json:"shell_type"`
	Visibility                        string                 `json:"visibility"`
	SupportedInAPI                    bool                   `json:"supported_in_api"`
	Priority                          int                    `json:"priority"`
	AdditionalSpeedTiers              []string               `json:"additional_speed_tiers"`
	ServiceTiers                      []any                  `json:"service_tiers"`
	DefaultServiceTier                *string                `json:"default_service_tier,omitempty"`
	AvailabilityNUX                   any                    `json:"availability_nux"`
	Upgrade                           any                    `json:"upgrade"`
	BaseInstructions                  string                 `json:"base_instructions"`
	IncludeSkillsUsageInstructions    bool                   `json:"include_skills_usage_instructions"`
	SupportsReasoningSummaryParameter bool                   `json:"supports_reasoning_summary_parameter"`
	DefaultReasoningSummary           string                 `json:"default_reasoning_summary"`
	SupportVerbosity                  bool                   `json:"support_verbosity"`
	DefaultVerbosity                  *string                `json:"default_verbosity"`
	ApplyPatchToolType                *string                `json:"apply_patch_tool_type"`
	WebSearchToolType                 string                 `json:"web_search_tool_type"`
	TruncationPolicy                  codexTruncationPolicy  `json:"truncation_policy"`
	SupportsParallelToolCalls         bool                   `json:"supports_parallel_tool_calls"`
	SupportsImageDetailOriginal       bool                   `json:"supports_image_detail_original"`
	ContextWindow                     int64                  `json:"context_window"`
	MaxContextWindow                  int64                  `json:"max_context_window"`
	AutoCompactTokenLimit             int64                  `json:"auto_compact_token_limit"`
	EffectiveContextWindowPercent     int64                  `json:"effective_context_window_percent"`
	ExperimentalSupportedTools        []string               `json:"experimental_supported_tools"`
	InputModalities                   []string               `json:"input_modalities"`
	SupportsSearchTool                bool                   `json:"supports_search_tool"`
	UseResponsesLite                  bool                   `json:"use_responses_lite"`
	ToolMode                          *string                `json:"tool_mode,omitempty"`
}

// handleV1Models serves GET /v1/models → the runnable inventory in OpenAI list shape (design §1.2).
// Present + runnable (runs/runs_degraded/unevaluated) models only; a wont_run model is filtered out.
func (s *Server) handleV1Models(w http.ResponseWriter, r *http.Request) {
	models, err := s.svc.OpenAIModels(r.Context())
	if err != nil {
		proxy.WriteOpenAIError(w, http.StatusInternalServerError, "api_error", "internal_error",
			"could not read inventory: "+err.Error())
		return
	}
	if models == nil {
		models = []core.OpenAIModel{}
	}
	writeJSON(w, http.StatusOK, openAIListResponse{
		Object: "list",
		Data:   models,
		Models: codexModels(models),
	})
}

func codexModels(models []core.OpenAIModel) []codexModelInfo {
	out := make([]codexModelInfo, 0, len(models))
	for i, model := range models {
		description := "Local model routed through LocalHub's OpenAI-compatible Responses API."
		contextWindow := int64(65536)
		autoCompactTokenLimit := int64(56000)
		effectiveContextWindowPercent := int64(90)
		if model.ID == "gguf-qwen3-6-27b-16gb-vram-mtp-mini-iq4-xs" {
			description = "Qwen3.6 27B coding model using LocalHub's currently safe supervised 8K runtime profile."
			contextWindow = 8192
			autoCompactTokenLimit = 8000
			effectiveContextWindowPercent = 100
		}
		if model.ID == "kat" {
			description = "KAT-Coder V2.5 agentic-coding model routed through LocalHub."
			contextWindow = 167168
			autoCompactTokenLimit = 153600
			effectiveContextWindowPercent = 100
		}
		applyPatch := "freeform"
		toolMode := "direct"
		out = append(out, codexModelInfo{
			Slug:                              model.ID,
			DisplayName:                       model.ID,
			Description:                       description,
			SupportedReasoningLevels:          []codexReasoningEffort{},
			ShellType:                         "unified_exec",
			Visibility:                        "list",
			SupportedInAPI:                    true,
			Priority:                          len(models) - i,
			AdditionalSpeedTiers:              []string{},
			ServiceTiers:                      []any{},
			AvailabilityNUX:                   nil,
			Upgrade:                           nil,
			BaseInstructions:                  "You are a coding agent. Follow the user's instructions, use tools when needed, preserve unrelated work, and verify changes before reporting success.",
			IncludeSkillsUsageInstructions:    false,
			SupportsReasoningSummaryParameter: false,
			DefaultReasoningSummary:           "none",
			SupportVerbosity:                  false,
			DefaultVerbosity:                  nil,
			ApplyPatchToolType:                &applyPatch,
			WebSearchToolType:                 "text",
			TruncationPolicy:                  codexTruncationPolicy{Mode: "tokens", Limit: autoCompactTokenLimit},
			SupportsParallelToolCalls:         false,
			SupportsImageDetailOriginal:       false,
			ContextWindow:                     contextWindow,
			MaxContextWindow:                  contextWindow,
			AutoCompactTokenLimit:             autoCompactTokenLimit,
			EffectiveContextWindowPercent:     effectiveContextWindowPercent,
			ExperimentalSupportedTools:        []string{},
			InputModalities:                   []string{"text"},
			SupportsSearchTool:                false,
			UseResponsesLite:                  false,
			ToolMode:                          &toolMode,
		})
	}
	return out
}

// handleV1ChatCompletions serves POST /v1/chat/completions (design §1.3–§1.6).
func (s *Server) handleV1ChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.relayOpenAI(w, r, "/v1/chat/completions")
}

// handleV1Completions serves POST /v1/completions (design §1.3–§1.6).
func (s *Server) handleV1Completions(w http.ResponseWriter, r *http.Request) {
	s.relayOpenAI(w, r, "/v1/completions")
}

// handleV1Responses serves the OpenAI Responses surface needed by local Codex
// agents. It uses the same admission and counted relay as chat completions.
func (s *Server) handleV1Responses(w http.ResponseWriter, r *http.Request) {
	s.relayOpenAI(w, r, "/v1/responses")
}

// handleV1Embeddings serves POST /v1/embeddings. llama.cpp exposes an OpenAI-compatible
// /v1/embeddings route ONLY on a server started with --embedding, and that route requires the
// model to use a pooling type other than "none" (source: llama.cpp tools/server/README.md).
// Resolution therefore goes through the embedding-only path (core.ResolveForEmbeddings) so a
// generation-mode instance is never handed an embeddings request.
func (s *Server) handleV1Embeddings(w http.ResponseWriter, r *http.Request) {
	s.relayOpenAIMode(w, r, "/v1/embeddings", relayModeEmbedding)
}

// handleV1Rerank serves POST /v1/rerank. llama.cpp exposes /v1/rerank (also /rerank, /reranking,
// /v1/reranking) ONLY on a server started with --embedding --pooling rank --reranking (source:
// llama.cpp tools/server/README.md: "--rerank, --reranking : enable reranking endpoint on server
// ... Requires a reranker model ... and the --embedding --pooling rank options"). Resolution
// therefore goes through the reranking-only path (core.ResolveForRerank) so neither a generation
// instance nor a plain embedding instance is ever handed a rerank request. Request/response bodies
// ({model, query, documents, top_n} / {results:[{index, relevance_score}]}) are never parsed or
// reshaped here — this stays a byte-passthrough relay (ADR-002: thin proxy).
func (s *Server) handleV1Rerank(w http.ResponseWriter, r *http.Request) {
	s.relayOpenAIMode(w, r, "/v1/rerank", relayModeRerank)
}

// relayMode selects which core resolution path relayOpenAIMode uses, and which chat-only body
// rewrites (Qwen chat_template_kwargs, /v1/responses normalization) are skipped. It is a local
// enum, not core's unexported launchMode, because this package cannot see an unexported core type;
// it exists purely to fan out to the right core.ResolveFor* call and uniformly skip the chat-only
// rewrites for both embeddings and rerank.
type relayMode int

const (
	relayModeGeneration relayMode = iota
	relayModeEmbedding
	relayModeRerank
)

// relayOpenAI is the shared body of the OpenAI handlers: parse the model out of the OpenAI
// body (decode-then-remarshal, design §1.5), resolve it to a warm/admitted instance (core), then
// byte-identically stream the upstream response back (internal/proxy). Every request goes through
// BeginInstanceRequest/Done (inside ResolveForProxy → the returned handle) so in-flight counting
// closes the Phase-5 gap for external clients (design §1.6). Blocks return a FAST 503 (design §1.4).
func (s *Server) relayOpenAI(w http.ResponseWriter, r *http.Request, path string) {
	s.relayOpenAIMode(w, r, path, relayModeGeneration)
}

// relayOpenAIMode is the shared body behind relayOpenAI, handleV1Embeddings, and handleV1Rerank.
// mode selects the resolution path: core.ResolveForEmbeddings for /v1/embeddings,
// core.ResolveForRerank for /v1/rerank, or core.ResolveForProxy (generation) for everything else.
// The three resolution paths never cross.
func (s *Server) relayOpenAIMode(w http.ResponseWriter, r *http.Request, path string, mode relayMode) {
	if !requireJSONBodyOpenAI(w, r) { // CSRF guard; OpenAI SDKs always declare application/json
		return
	}
	// Parse the model out of the body WITHOUT consuming the stream irrecoverably (decode into a map,
	// re-marshal — small bodies, lossless; the RESPONSE is what must be byte-identical, not the
	// request). A missing/empty model or bad JSON → 400 invalid_body.
	head, ok, err := proxy.ParseRequestHead(r.Body)
	if err != nil {
		proxy.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_body",
			"could not parse the OpenAI request body as JSON: "+err.Error())
		return
	}
	if !ok {
		proxy.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_body",
			"the request body must include a non-empty \"model\" field naming a local model")
		return
	}

	// Resolve model → instance handle (runs the full P5 admission via core). Priority is not a field
	// on the OpenAI body → default normal (1). Ctx defaults to the model's DefaultContext in core.
	// mode selects ResolveForEmbeddings (embedding-only instances), ResolveForRerank (reranking-only
	// instances), or ResolveForProxy (generation instances) — the three resolution paths never cross.
	var handle *core.InstanceRequestHandle
	var block *core.ProxyBlock
	var rerr error
	switch mode {
	case relayModeEmbedding:
		handle, block, rerr = s.svc.ResolveForEmbeddings(r.Context(), head.Model, 1)
	case relayModeRerank:
		handle, block, rerr = s.svc.ResolveForRerank(r.Context(), head.Model, 1)
	default:
		handle, block, rerr = s.svc.ResolveForProxy(r.Context(), head.Model, 1)
	}
	if rerr != nil {
		if errors.Is(rerr, core.ErrProxyModelNotFound) {
			proxy.WriteOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
				"model \""+head.Model+"\" is not in the local inventory")
			return
		}
		// Genuine resolution/spawn failure → 502 (the engine/start failed). The reason is preserved.
		proxy.WriteOpenAIError(w, http.StatusBadGateway, "api_error", "upstream_unreachable",
			"could not resolve or start the model: "+rerr.Error())
		return
	}
	if block != nil {
		// Admission blocked (queued/reject/wont_run) → FAST 503 with the numeric reason (design §1.4;
		// the queue row, if any, was already cancelled inside ResolveForProxy — cancel-on-block).
		errType, errCode := proxyBlockError(block.Kind)
		proxy.WriteOpenAIError(w, http.StatusServiceUnavailable, errType, errCode,
			block.Reason)
		return
	}

	// HEALTHY: relay the (re-marshaled) body byte-identically. handle.Done() releases the in-flight
	// count on every exit path (success, error, client disconnect) so the never-kill-active gate is
	// accurate (design §1.6). r.Context() cancels the upstream generation on client disconnect.
	defer handle.Done()

	// Upstream model rewrite (P6.1 #p61c): the shared Ollama daemon routes by the `model` field and
	// knows the model by its TAG, not LocalHub's catalog id, so it 404s on the id the client sent.
	// When core resolved an upstream-model override (set ONLY for Ollama instances), rewrite the
	// relayed body's `model` field to that tag. llama.cpp/vLLM own a single loaded model and ignore
	// the field, so their handle carries no override → the body is relayed unchanged (byte-identical
	// to the prior behavior; the response byte-identity the gate tests is unaffected either way).
	relayBody := head.Body
	// Chat-template-only rewrite: an embeddings/rerank body has no chat template / thinking trace,
	// so never touch it here — only the generation path may rewrite the body.
	if mode == relayModeGeneration && head.Model == "gguf-qwen3-6-27b-16gb-vram-mtp-mini-iq4-xs" {
		// This Qwen chat template defaults to a verbose thinking trace, which can
		// consume the entire small supervised context before emitting content or
		// a tool call. The live llama.cpp runtime accepts this standard template
		// argument and emits the requested answer directly when it is disabled.
		relayBody = proxy.SetJSONField(relayBody, "chat_template_kwargs", map[string]any{
			"enable_thinking": false,
		})
	}
	var responseNamespaces proxy.ResponsesNamespaceMap
	// /v1/responses normalization is a chat/agentic-response concept; embeddings/rerank never hit this
	// path (path is fixed per mode), but guard explicitly for clarity.
	if mode == relayModeGeneration && path == "/v1/responses" {
		relayBody, responseNamespaces = proxy.NormalizeResponsesRequest(relayBody)
	}
	if handle.UpstreamModel != "" {
		relayBody = proxy.SetJSONField(relayBody, "model", handle.UpstreamModel)
	}
	var relayErr error
	// RelayResponses carries Responses-specific namespace rewriting; an embeddings/rerank request
	// always uses the plain byte-passthrough Relay below, never the Responses relay.
	if mode == relayModeGeneration && path == "/v1/responses" {
		_, relayErr = proxy.RelayResponses(
			r.Context(),
			w,
			handle.TargetBaseURL,
			path,
			r.Header.Get("Accept"),
			strings.NewReader(string(relayBody)),
			responseNamespaces,
		)
	} else {
		_, relayErr = proxy.Relay(
			r.Context(),
			w,
			handle.TargetBaseURL,
			path,
			r.Header.Get("Accept"),
			strings.NewReader(string(relayBody)),
		)
	}
	if relayErr != nil {
		// Relay errors only BEFORE any bytes were written (build/unreachable) — safe to write an
		// OpenAI error envelope. A mid-stream break returns nil (the client already got a 2xx).
		var relErr *proxy.RelayError
		if errors.As(relayErr, &relErr) && relErr.Kind == proxy.RelayUpstreamUnreachable {
			proxy.WriteOpenAIError(w, http.StatusBadGateway, "api_error", "upstream_unreachable",
				"could not reach the engine: "+relErr.Error())
			return
		}
		proxy.WriteOpenAIError(w, http.StatusInternalServerError, "api_error", "internal_error",
			"could not build the upstream request: "+relayErr.Error())
	}
}

// proxyBlockError preserves the core's refusal category in the OpenAI error envelope. The original
// insufficient-VRAM pair remains unchanged for compatibility; other categories use stable,
// machine-readable codes so clients do not retry a decommissioned or incompatible model as if more
// free VRAM would help.
func proxyBlockError(kind core.ProxyBlockKind) (errorType, errorCode string) {
	switch kind {
	case core.ProxyBlockRAM:
		return "insufficient_ram", "ram_blocked"
	case core.ProxyBlockGPUHot:
		return "gpu_hot", "gpu_hot"
	case core.ProxyBlockGPUBusy:
		return "gpu_busy", "gpu_busy"
	case core.ProxyBlockQueueFull:
		return "queue_full", "queue_full"
	case core.ProxyBlockDecommissioned:
		return "model_decommissioned", "model_decommissioned"
	case core.ProxyBlockIncompatible:
		return "model_incompatible", "model_incompatible"
	case core.ProxyBlockModeMismatch:
		return "mode_mismatch", "mode_mismatch"
	case core.ProxyBlockStarting:
		return "starting", "model_starting"
	default:
		return "insufficient_vram", "admission_blocked"
	}
}
