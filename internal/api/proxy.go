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
// /collective_rpc) are NEVER forwarded — the proxy only ever relays /v1/chat/completions and
// /v1/completions (docs/security.md).
//
// WIRING (integrator: paste into server.go routes):
//   mux.HandleFunc("GET /v1/models", s.handleV1Models)
//   mux.HandleFunc("POST /v1/chat/completions", s.handleV1ChatCompletions)
//   mux.HandleFunc("POST /v1/completions", s.handleV1Completions)

// openAIListResponse is the GET /v1/models envelope (OpenAI list shape, design §1.2).
type openAIListResponse struct {
	Object string             `json:"object"` // always "list"
	Data   []core.OpenAIModel `json:"data"`
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
	writeJSON(w, http.StatusOK, openAIListResponse{Object: "list", Data: models})
}

// handleV1ChatCompletions serves POST /v1/chat/completions (design §1.3–§1.6).
func (s *Server) handleV1ChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.relayOpenAI(w, r, "/v1/chat/completions")
}

// handleV1Completions serves POST /v1/completions (design §1.3–§1.6).
func (s *Server) handleV1Completions(w http.ResponseWriter, r *http.Request) {
	s.relayOpenAI(w, r, "/v1/completions")
}

// relayOpenAI is the shared body of the two completion handlers: parse the model out of the OpenAI
// body (decode-then-remarshal, design §1.5), resolve it to a warm/admitted instance (core), then
// byte-identically stream the upstream response back (internal/proxy). Every request goes through
// BeginInstanceRequest/Done (inside ResolveForProxy → the returned handle) so in-flight counting
// closes the Phase-5 gap for external clients (design §1.6). Blocks return a FAST 503 (design §1.4).
func (s *Server) relayOpenAI(w http.ResponseWriter, r *http.Request, path string) {
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
	handle, block, rerr := s.svc.ResolveForProxy(r.Context(), head.Model, 1)
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
		proxy.WriteOpenAIError(w, http.StatusServiceUnavailable, "insufficient_vram", "admission_blocked",
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
	if handle.UpstreamModel != "" {
		relayBody = proxy.SetJSONField(relayBody, "model", handle.UpstreamModel)
	}
	_, relayErr := proxy.Relay(r.Context(), w, handle.TargetBaseURL, path, r.Header.Get("Accept"),
		strings.NewReader(string(relayBody)))
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
