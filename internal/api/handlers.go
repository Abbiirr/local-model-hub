package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/localhub/localhub/internal/core"
)

// handleHardware serves GET /api/hardware: a point-in-time hardware snapshot. Thin: parse
// nothing, call core.Hardware, marshal. The probe guarantees a missing GPU is non-fatal.
func (s *Server) handleHardware(w http.ResponseWriter, r *http.Request) {
	snap, err := s.svc.Hardware(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "hardware_probe_failed",
			"could not read hardware", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleHardwareLive serves GET /api/hardware/live as an SSE stream of hardware snapshots at
// the configured poll cadence (Hardware.PollIntervalSeconds). It sends one frame immediately
// so the UI paints without waiting a full interval, then one per tick. A transient probe
// error skips a single frame rather than tearing down the stream. The stream ends when the
// client disconnects (r.Context().Done()).
func (s *Server) handleHardwareLive(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "sse_unsupported",
			"streaming unsupported", "the response writer does not implement http.Flusher")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering (Tailscale Serve friendliness)

	interval := s.svc.HardwarePollInterval()

	writeFrame := func() {
		snap, err := s.svc.Hardware(r.Context())
		if err != nil {
			return // skip this frame; don't tear down the stream on a transient probe error
		}
		b, err := json.Marshal(snap)
		if err != nil {
			return
		}
		// SSE framing: "data: <json>\n\n".
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}

	// Immediate first frame, then tick.
	writeFrame()

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			writeFrame()
		}
	}
}

// handleEngines serves GET /api/engines: the read-only engine detection sweep. Thin.
func (s *Server) handleEngines(w http.ResponseWriter, r *http.Request) {
	views := s.svc.Engines(r.Context())
	writeJSON(w, http.StatusOK, views)
}

// handleEnginesDetect serves POST /api/engines/detect: re-probe now. It is read-only despite
// being a POST (no engine mutation — it only re-runs the same detection sweep), so it is safe
// under the P1.3 zero-mutation gate.
func (s *Server) handleEnginesDetect(w http.ResponseWriter, r *http.Request) {
	views := s.svc.DetectEngines(r.Context())
	writeJSON(w, http.StatusOK, views)
}

// handleModels serves GET /api/models: the normalized inventory read from the catalog.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.svc.Models(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "models_query_failed",
			"could not read model inventory", err.Error())
		return
	}
	if models == nil {
		models = []core.ModelSummary{} // marshal [] not null for an empty catalog
	}
	writeJSON(w, http.StatusOK, models)
}

// handleCompatPreview serves POST /api/compat/preview: compute a compatibility verdict for a
// model WITHOUT downloading weights. The body is a core.CompatRequest selecting one of:
// {model_id} (inventory), {hf_repo, file} (remote GGUF), or {hf_repo} (remote safetensors).
// The verdict itself always carries a reason (even for wont_run/unknown); an error envelope is
// returned only for a caller/transport problem (bad body, unconfigured engine, failed fetch).
func (s *Server) handleCompatPreview(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) { // CSRF guard (triggers remote HF fetches — not free to forge)
		return
	}
	var req core.CompatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"could not parse compat preview request",
			"the request body must be JSON: {model_id} | {hf_repo,file} | {hf_repo}. "+err.Error())
		return
	}
	v, err := s.svc.CompatPreview(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "compat_preview_failed",
			"could not compute a compatibility verdict", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleModelCompat serves GET /api/models/{id}/compat?ctx=&batch=: the verdict for one
// inventory model. Thin: parse the path id + optional query, call CompatPreview, marshal.
func (s *Server) handleModelCompat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing model id", "the path must include a model id")
		return
	}
	req := core.CompatRequest{ModelID: id}
	if c := r.URL.Query().Get("ctx"); c != "" {
		if n, err := strconv.Atoi(c); err == nil {
			req.Ctx = n
		}
	}
	if b := r.URL.Query().Get("batch"); b != "" {
		if n, err := strconv.Atoi(b); err == nil {
			req.Batch = n
		}
	}
	v, err := s.svc.CompatPreview(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "compat_preview_failed",
			"could not compute a compatibility verdict", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleModelsRescan serves POST /api/models/rescan: trigger a disk (+Ollama) scan, persist,
// reconcile, and return a summary. Ollama being unreachable does not fail the scan (recorded
// in the summary). Admin action (walks disk) — auth is enforced by middleware when a token is
// configured.
func (s *Server) handleModelsRescan(w http.ResponseWriter, r *http.Request) {
	summary, err := s.svc.RescanModels(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "rescan_failed",
			"model rescan failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
