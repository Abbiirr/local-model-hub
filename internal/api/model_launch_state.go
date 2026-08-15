package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Four-state residency policy transport (migration 0015). Thin over core, like every transport here
// (PLAN.md decision 7): SetModelLaunchState owns the validation + the UPDATE + the "unknown model"
// refusal; this handler only parses the body and maps the result/error to HTTP.

// modelLaunchStateRequest is the PUT /api/models/{id}/launch-state body.
type modelLaunchStateRequest struct {
	// State is one of: always_available | available | available_on_request | unavailable. See
	// docs/api-spec.md for the full semantics table. An unrecognized value is refused with a 400
	// listing every valid option.
	State string `json:"state"`
}

// handleModelLaunchStatePut serves PUT /api/models/{id}/launch-state {"state": "..."} ->
// ModelSummary. 400 on a bad state (lists the valid ones); 404 for an unknown model id.
func (s *Server) handleModelLaunchStatePut(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) { // CSRF guard: force a CORS preflight on cross-site attempts
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing model id", "the path must include a model id")
		return
	}
	var req modelLaunchStateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"could not parse the launch-state request",
			"the body must be JSON: {\"state\": \"always_available|available|available_on_request|unavailable\"}. "+err.Error())
		return
	}
	summary, err := s.svc.SetModelLaunchState(r.Context(), id, req.State)
	if err != nil {
		status, code := http.StatusInternalServerError, "internal"
		switch {
		case strings.Contains(err.Error(), "not in the inventory"):
			status, code = http.StatusNotFound, "not_found"
		case strings.Contains(err.Error(), "invalid state") || strings.Contains(err.Error(), "model_id is required"):
			status, code = http.StatusBadRequest, "bad_request"
		}
		writeError(w, status, code, "could not update the model's launch state", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
