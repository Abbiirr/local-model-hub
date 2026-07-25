package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/localhub/localhub/internal/core"
)

// jobs.go hosts the THIN P6.2 async-inference-jobs handlers (design §2.2). Parse → one core call →
// marshal; all business logic (admission, relay, restart semantics, cancel) lives in core.jobs.go.
//
// WIRING (integrator: paste into server.go routes):
//   mux.HandleFunc("POST /api/jobs", s.handleJobSubmit)
//   mux.HandleFunc("GET /api/jobs", s.handleJobList)
//   mux.HandleFunc("GET /api/jobs/{id}", s.handleJobGet)
//   mux.HandleFunc("DELETE /api/jobs/{id}", s.handleJobCancel)

// jobSubmitBody is the POST /api/jobs body: an OpenAI chat/completions (or completions) request plus
// optional priority + endpoint. The full body is stored verbatim as the job's request_json; `model`
// is read out to resolve the target (design §2.2). Extra OpenAI fields (messages, max_tokens, etc.)
// pass through untouched into request_json.
type jobSubmitBody struct {
	Model    string `json:"model"`
	Priority *int   `json:"priority,omitempty"` // 0 interactive | 1 normal | 2 background (default 1)
	Endpoint string `json:"endpoint,omitempty"` // "chat" (default) | "completions"
}

// handleJobSubmit serves POST /api/jobs → 201 {id, state:"queued"} IMMEDIATELY (design §2.2). The
// job is enqueued durably; the background worker admits + runs it. It does NOT wait for inference.
func (s *Server) handleJobSubmit(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) { // CSRF guard: force a CORS preflight on cross-site attempts
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20)) // 8 MiB cap — an OpenAI body is tiny
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not read request body", err.Error())
		return
	}
	var meta jobSubmitBody
	if err := json.Unmarshal(raw, &meta); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"could not parse job request",
			"the body must be an OpenAI chat/completions request with a \"model\" field (+ optional \"priority\", \"endpoint\"). "+err.Error())
		return
	}
	if strings.TrimSpace(meta.Model) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing model",
			"the job request body must include a non-empty \"model\" field")
		return
	}
	priority := 1
	if meta.Priority != nil {
		priority = *meta.Priority
	}

	view, err := s.svc.SubmitJob(r.Context(), core.JobSubmit{
		Model:       meta.Model,
		RequestJSON: raw, // stored verbatim so a restart re-runs the exact request
		Priority:    priority,
		Endpoint:    meta.Endpoint,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "job_submit_failed", "could not submit job", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// handleJobGet serves GET /api/jobs/{id} → the job with result_json/reason as applicable (§2.2).
func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing job id", "the path must include a job id")
		return
	}
	view, err := s.svc.GetJob(r.Context(), id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", "job not found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "job_query_failed", "could not read job", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleJobList serves GET /api/jobs?state=&model=&limit= → a filterable list (result_json omitted).
func (s *Server) handleJobList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if l := strings.TrimSpace(q.Get("limit")); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			limit = n
		}
	}
	list, err := s.svc.ListJobs(r.Context(), core.JobFilter{
		State: q.Get("state"),
		Model: q.Get("model"),
		Limit: limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "job_query_failed", "could not list jobs", err.Error())
		return
	}
	if list == nil {
		list = []core.JobView{}
	}
	writeJSON(w, http.StatusOK, list)
}

// handleJobCancel serves DELETE /api/jobs/{id} → cancel (design §2.5). A terminal job → 409 (nothing
// to cancel). A queued job is removed from the residency queue + marked cancelled; a running job has
// its relay aborted server-side + marked cancelled (the ENGINE may still finish internally — the
// instance is NOT killed; "cancelled" means LocalHub stopped waiting, see the reason string).
func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing job id", "the path must include a job id")
		return
	}
	// Read current state first so we can distinguish "already terminal → 409" from a fresh cancel.
	before, gerr := s.svc.GetJob(r.Context(), id)
	if gerr != nil {
		if strings.Contains(gerr.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", "job not found", gerr.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "job_query_failed", "could not read job", gerr.Error())
		return
	}
	if isTerminalJobState(before.State) {
		writeError(w, http.StatusConflict, "job_terminal",
			"job already terminal",
			"job "+id+" is "+before.State+" — nothing to cancel")
		return
	}

	view, err := s.svc.CancelJob(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "job_cancel_failed", "could not cancel job", err.Error())
		return
	}
	// A job that raced to terminal between the read and the cancel (e.g. just succeeded) → 409.
	if view.State != "cancelled" {
		writeError(w, http.StatusConflict, "job_terminal",
			"job already terminal",
			"job "+id+" reached "+view.State+" before it could be cancelled")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func isTerminalJobState(state string) bool {
	switch state {
	case "succeeded", "failed", "cancelled":
		return true
	}
	return false
}
