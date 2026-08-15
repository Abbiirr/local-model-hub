package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// DEPRECATED decommission toggle transport (migration 0014; superseded by
// PUT /api/models/{id}/launch-state, migration 0015 — see model_launch_state.go). Kept working
// because it shipped recently: SetModelAutoLoad is now a thin alias over SetModelLaunchState. Thin
// over core, like every transport here (PLAN.md decision 7).

// modelAutoLoadRequest is the DEPRECATED PUT /api/models/{id}/auto-load body. Prefer
// PUT /api/models/{id}/launch-state {"state": "..."}.
type modelAutoLoadRequest struct {
	// Enabled true = launch_state=available_on_request (the default state for every model — this
	// does NOT grant always_available/available). Enabled false = launch_state=unavailable: never
	// auto-started by the OpenAI proxy, the residency queue, or the jobs worker, and excluded from
	// GET /v1/models — but still startable via an explicit POST /api/instances, and still listed
	// (with this flag) by GET /api/models.
	Enabled bool `json:"enabled"`
}

// handleModelAutoLoadPut serves the DEPRECATED PUT /api/models/{id}/auto-load {"enabled": bool}.
// Prefer PUT /api/models/{id}/launch-state.
func (s *Server) handleModelAutoLoadPut(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) { // CSRF guard: force a CORS preflight on cross-site attempts
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing model id", "the path must include a model id")
		return
	}
	var req modelAutoLoadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"could not parse the auto-load request",
			"the body must be JSON: {\"enabled\": bool}. "+err.Error())
		return
	}
	summary, err := s.svc.SetModelAutoLoad(r.Context(), id, req.Enabled)
	if err != nil {
		status, code := http.StatusInternalServerError, "internal"
		if strings.Contains(err.Error(), "not in the inventory") {
			status, code = http.StatusNotFound, "not_found"
		} else if strings.Contains(err.Error(), "model_id is required") {
			status, code = http.StatusBadRequest, "bad_request"
		}
		writeError(w, status, code, "could not update the model's auto-load setting", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
