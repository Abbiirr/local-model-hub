package api

import (
	"errors"
	"net/http"

	"github.com/localhub/localhub/internal/core"
	"github.com/localhub/localhub/internal/proxy"
)

// handleV1Messages provides the deliberately narrow Anthropic Messages
// compatibility surface used by Claude Code. Residency and model admission
// remain protocol-neutral in core; only wire translation lives here and in
// internal/proxy.
func (s *Server) handleV1Messages(w http.ResponseWriter, r *http.Request) {
	if version := r.Header.Get("anthropic-version"); version != "2023-06-01" {
		proxy.WriteAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			`anthropic-version must be "2023-06-01"`)
		return
	}
	if !jsonContentType(r) {
		proxy.WriteAnthropicError(w, http.StatusUnsupportedMediaType, "invalid_request_error",
			"Content-Type must be application/json (cross-site request forgery guard)")
		return
	}
	req, err := proxy.ParseAnthropicRequest(r.Body)
	if err != nil {
		proxy.WriteAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	handle, block, resolveErr := s.svc.ResolveForProxy(r.Context(), req.Model, 1)
	if resolveErr != nil {
		if errors.Is(resolveErr, core.ErrProxyModelNotFound) {
			proxy.WriteAnthropicError(w, http.StatusNotFound, "not_found_error",
				`model "`+req.Model+`" is not in the local inventory`)
			return
		}
		proxy.WriteAnthropicError(w, http.StatusBadGateway, "api_error",
			"could not resolve or start the model: "+resolveErr.Error())
		return
	}
	if block != nil {
		proxy.WriteAnthropicError(w, 529, "overloaded_error", block.Reason)
		return
	}
	defer handle.Done()

	if handle.UpstreamModel != "" {
		req.OpenAIBody = proxy.SetJSONField(req.OpenAIBody, "model", handle.UpstreamModel)
	}
	if relayErr := proxy.RelayAnthropic(r.Context(), w, handle.TargetBaseURL, req); relayErr != nil {
		var typed *proxy.RelayError
		if errors.As(relayErr, &typed) && typed.Kind == proxy.RelayUpstreamUnreachable {
			proxy.WriteAnthropicError(w, http.StatusBadGateway, "api_error",
				"could not reach the local engine: "+typed.Error())
			return
		}
		proxy.WriteAnthropicError(w, http.StatusInternalServerError, "api_error",
			"could not translate the local engine response: "+relayErr.Error())
	}
}
