package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/localhub/localhub/internal/core"
)

// handleGPUFree serves POST /api/gpu/free (PLAN-ui-gpu-rework.md §2.5): stop everything holding
// the GPU and report how much VRAM actually came back.
//
// Body is optional. An empty body means the default sweep (pinned instances are SKIPPED); send
// {"include_pinned": true} to stop those too. Accepting an empty body matters because the
// dashboard button posts without one.
//
// The response always carries a human-readable `reason` (CLAUDE.md), including for the
// do-nothing case, so the UI never has to invent an explanation for "nothing happened".
func (s *Server) handleGPUFree(w http.ResponseWriter, r *http.Request) {
	var req core.FreeGPURequest
	if r.Body != nil {
		// An absent or empty body is not a client error here — it is the common case.
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "bad_request",
				"could not parse the request body",
				`expected an empty body or {"include_pinned": true|false}: `+err.Error())
			return
		}
	}

	res, err := s.svc.FreeGPU(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gpu_free_failed",
			"could not free the GPU", err.Error())
		return
	}
	// 200 even when individual stops failed: the sweep itself succeeded and the per-instance
	// outcomes are in results[]. A blanket 500 would hide the instances that DID stop.
	writeJSON(w, http.StatusOK, res)
}

// handleGPUHistory serves GET /api/gpu/history: retained GPU telemetry samples, oldest first,
// for the dashboard sparklines (PLAN-ui-gpu-rework.md §2.4).
//
// Always returns a JSON array, never null — an empty history (fresh daemon, no sample yet) must
// render as an empty chart, not as a client-side crash on `null.map`.
func (s *Server) handleGPUHistory(w http.ResponseWriter, r *http.Request) {
	samples := s.svc.GPUHistory()
	if samples == nil {
		samples = []core.GPUSample{}
	}
	writeJSON(w, http.StatusOK, samples)
}
