package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/localhub/localhub/internal/core"
)

// This file hosts the Phase-4 instance transport (POST/GET/DELETE /api/instances). Handlers are
// THIN: parse → one core call → marshal or a standard error envelope (PLAN.md decision 7).
// These are ADMIN actions (they spawn/kill GPU processes) — auth is enforced by middleware when
// a token is configured, and the daemon binds loopback only.

// handleInstancesList serves GET /api/instances → running + recent instances.
func (s *Server) handleInstancesList(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.Instances(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "instances_query_failed",
			"could not read instances", err.Error())
		return
	}
	if list == nil {
		list = []core.InstanceView{}
	}
	writeJSON(w, http.StatusOK, list)
}

// handleInstanceGet serves GET /api/instances/{id}.
func (s *Server) handleInstanceGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing instance id", "the path must include an instance id")
		return
	}
	inst, err := s.svc.Instance(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "instance not found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

// handleInstanceCreate serves POST /api/instances {model_id, engine?, ctx?, priority?, pin?}.
// A Phase-4 safety-refused start (verdict not runs/degraded, or est > free-1500MiB) returns 409
// Conflict with the numeric reason in the envelope — nothing is spawned (gate). A model with no
// local file returns 400. A genuine spawn/health failure returns 502.
func (s *Server) handleInstanceCreate(w http.ResponseWriter, r *http.Request) {
	var req core.StartInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"could not parse start-instance request",
			"the request body must be JSON: {model_id, engine?, ctx?, batch?, priority?, pin?, kv_quant?}. "+err.Error())
		return
	}
	inst, err := s.svc.StartInstance(r.Context(), req)
	if err != nil {
		// A residency-QUEUED start is NOT an error: return 202 Accepted + the queue position and
		// blocked_reason (the async-start contract). The daemon spawned nothing; the request waits
		// for VRAM and will be admitted when room frees.
		var qErr *core.QueuedError
		if errors.As(err, &qErr) {
			writeJSON(w, http.StatusAccepted, QueuedResponse{
				Status:        "queued",
				QueueID:       qErr.QueueID,
				Position:      qErr.Position,
				BlockedReason: qErr.Reason(),
			})
			return
		}
		status, code := classifyStartError(err)
		writeError(w, status, code, "could not start instance", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, inst)
}

// QueuedResponse is the 202 body for a residency-queued start (POST /api/instances). It carries
// the queue handle + position + the numeric blocked_reason so the UI can show "waiting: N MiB
// short, position P".
type QueuedResponse struct {
	Status        string `json:"status"` // "queued"
	QueueID       string `json:"queue_id"`
	Position      int    `json:"position"`
	BlockedReason string `json:"blocked_reason"`
}

// handleQueueList serves GET /api/queue → residency queue entries (WAITING + recent) with
// blocked_reason + position + age.
func (s *Server) handleQueueList(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.Queue(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "queue_query_failed",
			"could not read the residency queue", err.Error())
		return
	}
	if list == nil {
		list = []core.QueueEntryView{}
	}
	writeJSON(w, http.StatusOK, list)
}

// handleQueueCancel serves DELETE /api/queue/{id} → cancel a WAITING entry.
func (s *Server) handleQueueCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing queue entry id", "the path must include a queue entry id")
		return
	}
	if err := s.svc.CancelQueueEntry(r.Context(), id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", "queue entry not found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "queue_cancel_failed", "could not cancel queue entry", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled", "id": id})
}

// handleInstanceStop serves DELETE /api/instances/{id}: begin drain → stop with the VRAM-delta
// gate, DETACHED from this request (Phase 5 fix 4). It returns 202 Accepted with the DRAINING view
// immediately; the actual process-stop + VRAM-verify + final STOPPED write run in a background
// goroutine owned by the service, so a client disconnect never leaves the instance stuck DRAINING.
// An already-terminal instance returns 200 with its terminal view.
func (s *Server) handleInstanceStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing instance id", "the path must include an instance id")
		return
	}
	inst, err := s.svc.StopInstanceAsync(r.Context(), id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", "instance not found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "instance_stop_failed",
			"could not begin stopping instance", err.Error())
		return
	}
	// Terminal already → 200 (nothing to drain); otherwise 202 (draining in the background).
	status := http.StatusAccepted
	if inst.State == "STOPPED" || inst.State == "FAILED" {
		status = http.StatusOK
	}
	writeJSON(w, status, inst)
}

// handleInstanceCompletion serves POST /api/instances/{id}/completions: a THIN pass-through relay
// to the instance's engine (llama.cpp /v1/chat/completions by default), streaming intact. Its
// PURPOSE (Phase 5 fix 3) is to let the daemon COUNT in-flight requests: core increments the
// in-flight counter for the instance before the relay and decrements after (via the request handle),
// so eviction never selects a streaming instance. It is also Phase 6's routing substrate.
//
// The relay preserves the request body and streams the response byte-for-byte with flushing (SSE
// intact). The upstream path defaults to /v1/chat/completions; pass ?path=/completion to hit the
// native llama.cpp endpoint. The in-flight count is held for the FULL duration of the stream.
func (s *Server) handleInstanceCompletion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing instance id", "the path must include an instance id")
		return
	}

	handle, err := s.svc.BeginInstanceRequest(r.Context(), id)
	if err != nil {
		// Not HEALTHY / unknown / no base_url → 409 (the instance cannot serve right now).
		writeError(w, http.StatusConflict, "instance_not_ready", "instance cannot serve a completion", err.Error())
		return
	}
	// Decrement the in-flight counter when the relay finishes — even on a client disconnect or an
	// upstream error, so the counter never leaks (the eviction gate depends on it being accurate).
	defer handle.Done()

	upstreamPath := r.URL.Query().Get("path")
	if upstreamPath == "" {
		upstreamPath = "/v1/chat/completions"
	}
	if !strings.HasPrefix(upstreamPath, "/") {
		upstreamPath = "/" + upstreamPath
	}
	target := strings.TrimRight(handle.TargetBaseURL, "/") + upstreamPath

	// Build the upstream request. Use the incoming request context so a client disconnect cancels
	// the upstream stream (server-side abort; the instance is NOT killed — DrainQueue/eviction is
	// separate). No timeout is imposed here: a long generation must be allowed to complete.
	upReq, uerr := http.NewRequestWithContext(r.Context(), http.MethodPost, target, r.Body)
	if uerr != nil {
		writeError(w, http.StatusInternalServerError, "proxy_build_failed", "could not build upstream request", uerr.Error())
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	if accept := r.Header.Get("Accept"); accept != "" {
		upReq.Header.Set("Accept", accept)
	}

	client := &http.Client{Timeout: 0} // streaming; no overall timeout
	resp, derr := client.Do(upReq)
	if derr != nil {
		writeError(w, http.StatusBadGateway, "upstream_unreachable", "could not reach the engine", derr.Error())
		return
	}
	defer resp.Body.Close()

	// Relay status + content-type, then stream the body with flushing so SSE chunks pass through
	// unbuffered (byte passthrough — Phase 6 will build the OpenAI proxy on this).
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // client gone; defer decrements the counter
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				// Upstream broke mid-stream; nothing more to send. The count is released by defer.
				return
			}
			return
		}
	}
}

// classifyStartError maps a StartInstance error to an HTTP status + code. A REFUSED safety
// pre-check → 409 Conflict (the numeric reason is in the message). A missing local file or bad
// request → 400. Everything else (spawn/health failure) → 502 Bad Gateway (the engine failed).
func classifyStartError(err error) (int, string) {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "REFUSED:") || strings.Contains(msg, "REFUSED:"):
		return http.StatusConflict, "start_refused"
	case strings.Contains(msg, "is required") || strings.Contains(msg, "has no local file") || strings.Contains(msg, "not supported in Phase 4"):
		return http.StatusBadRequest, "bad_request"
	case strings.Contains(msg, "not configured"):
		return http.StatusServiceUnavailable, "not_configured"
	default:
		return http.StatusBadGateway, "instance_start_failed"
	}
}
