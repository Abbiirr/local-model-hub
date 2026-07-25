package api

import (
	"encoding/json"
	"net/http"

	"github.com/localhub/localhub/internal/core"
)

// This file hosts the Phase-3 supply-chain HTTP handlers (thin: parse → one core call →
// marshal). Endpoints (spec-digest §3, extended for Phase 3):
//
//   POST /api/downloads            create a download/import (two-step HF via confirm)
//   GET  /api/downloads            list tracked jobs
//   GET  /api/downloads/{id}       one job's status
//   GET  /api/manifests/{id}       an immutable manifest by id
//   GET  /api/models/{id}/manifest the manifest linked to a model
//   GET  /api/models/{id}/reproduce the reproduce export

// handleDownloadCreate serves POST /api/downloads. The body is a core.DownloadRequest. For HF
// without confirm it returns a job in state=awaiting_ok carrying the resolved commit + files
// (show-before-commit); a second call with confirm:true proceeds. Other kinds start
// immediately. A caller/transport problem (bad body, guard refusal, unconfigured dep) is a
// 4xx/5xx envelope; the job itself carries per-file progress and any failure reason.
func (s *Server) handleDownloadCreate(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) { // CSRF guard: an attacker-chosen download URL must never ride a simple cross-site POST
		return
	}
	var req core.DownloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"could not parse download request",
			"body must be JSON: {kind, ...}. "+err.Error())
		return
	}
	job, err := s.svc.StartDownload(r.Context(), req)
	if err != nil {
		// A guard refusal or unconfigured dep is a 400 with a readable reason (no opaque 500).
		writeError(w, http.StatusBadRequest, "download_refused",
			"could not start the download", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// handleDownloadList serves GET /api/downloads: all tracked jobs.
func (s *Server) handleDownloadList(w http.ResponseWriter, r *http.Request) {
	jobs := s.svc.Downloads()
	if jobs == nil {
		jobs = []core.DownloadJob{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

// handleDownloadStatus serves GET /api/downloads/{id}: one job or a 404 envelope.
func (s *Server) handleDownloadStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.svc.DownloadStatus(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such download",
			"no download job with id "+id+" (jobs are in-memory and do not survive a daemon restart)")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// handleManifestGet serves GET /api/manifests/{id}: an immutable manifest by id.
func (s *Server) handleManifestGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := s.svc.GetManifest(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "manifest_read_failed", "could not read manifest", err.Error())
		return
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such manifest", "no manifest with id "+id)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleModelManifest serves GET /api/models/{id}/manifest: the manifest linked to a model, or a
// 404 envelope when the model has no manifest (a discovered pre-existing local file).
func (s *Server) handleModelManifest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := s.svc.ManifestForModel(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadRequest, "manifest_read_failed", "could not read model manifest", err.Error())
		return
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "no_manifest",
			"this model has no manifest",
			"model "+id+" was discovered on disk (pre-existing local file), not downloaded through LocalHub — its provenance is unknown")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleReproduce serves GET /api/models/{id}/reproduce: the reproduce export (manifest +
// launch flags). Works for every source kind; a discovered model returns provenance
// "unknown (pre-existing local file)".
func (s *Server) handleReproduce(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.svc.Reproduce(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "reproduce_failed", "could not build a reproduce bundle", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}
