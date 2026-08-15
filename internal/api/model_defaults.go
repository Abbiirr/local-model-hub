package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/localhub/localhub/internal/core"
)

// Saved per-model launch-default transport (M2.3). Thin over core, like every transport here
// (PLAN.md decision 7): all validation and provenance logic lives in core.SaveModelLaunchDefault.
//
// IMPORTANT CONTRACT, stated in the API docs and enforced by its absence here: a saved default is
// ADVISORY. POST /api/instances does not read it. It exists so the UI can pre-fill the advanced start
// form with a profile PROVEN on this box, and so a script can fetch it — but an unspecified start
// field keeps meaning "the daemon default", not "whatever was saved". Auto-apply would be a silent
// behaviour change and needs its own ADR + gate (TODO.md M6.1).

// handleModelLaunchDefaultGet serves GET /api/models/{id}/launch-default.
//
// 404 when no default is saved — the NORMAL state for most models, so the body says so plainly rather
// than implying a fault. On a hit, stale_reason is non-empty when the recorded engine build is no
// longer among the detected binaries (M6.2).
func (s *Server) handleModelLaunchDefaultGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, ok, err := s.svc.ModelLaunchDefault(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not read the saved launch default", err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found",
			"no saved launch default for this model",
			"this is the normal state until a profile has been validated and saved with "+
				"PUT /api/models/{id}/launch-default. Starts continue to use the daemon defaults.")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleModelLaunchDefaultPut serves PUT /api/models/{id}/launch-default.
//
// The profile is validated through the SAME path a live start uses, so a profile that could never be
// started cannot be saved as a "default" (a stored default that fails at start time is worse than
// none). An invalid value returns 400 with a reason naming the valid set; an unknown model returns
// 404. The path id WINS over any model_id in the body, so a copy-pasted body cannot write a default
// onto the wrong model.
func (s *Server) handleModelLaunchDefaultPut(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) { // CSRF guard: force a CORS preflight on cross-site attempts
		return
	}
	var d core.ModelLaunchDefault
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"could not parse the launch-default request",
			"the body must be JSON: {ctx?, kv_type_k?, kv_type_v?, slots?, spec_mode?, spec_draft_max?, "+
				"allow_cpu_offload?, engine_version?, engine_commit?, binary_path?, artifact_sha256?, validated_note?}. "+err.Error())
		return
	}
	// The URL identifies the model; the body cannot override it.
	d.ModelID = r.PathValue("id")

	saved, err := s.svc.SaveModelLaunchDefault(r.Context(), d)
	if err != nil {
		status, code := classifyLaunchDefaultError(err)
		writeError(w, status, code, "could not save the launch default", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// handleModelLaunchDefaultDelete serves DELETE /api/models/{id}/launch-default. Idempotent: deleting
// an absent default is a 204, so a UI "clear" button never has to check first.
func (s *Server) handleModelLaunchDefaultDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteModelLaunchDefault(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not delete the saved launch default", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// classifyLaunchDefaultError maps a core save error to an HTTP status. A rejected profile VALUE is the
// caller's fault (400); an unknown model is 404; anything else is a server fault.
func classifyLaunchDefaultError(err error) (int, string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not in the inventory"):
		return http.StatusNotFound, "not_found"
	case strings.Contains(msg, "invalid kv_type"),
		strings.Contains(msg, "invalid slots"),
		strings.Contains(msg, "invalid spec_draft_max"),
		strings.Contains(msg, "invalid speculative"),
		strings.Contains(msg, "spec_draft_max"),
		strings.Contains(msg, "model_id is required"),
		strings.Contains(msg, "ctx must be"):
		return http.StatusBadRequest, "bad_request"
	default:
		return http.StatusInternalServerError, "internal"
	}
}
