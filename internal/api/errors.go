package api

import (
	"encoding/json"
	"net/http"
)

// ErrorEnvelope is the MANDATORY error response shape (spec-digest §3):
//
//	{ "error": { "code", "message", "reason"? } }
//
// `reason` is the human-readable "why" and mirrors the verdict/blocked-reason philosophy.
// No opaque 500s: every error emitted by the daemon goes through writeError so the shape is
// uniform and a reason is always attachable.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Code    string `json:"code"`             // machine-readable, e.g. "not_found", "internal"
	Message string `json:"message"`          // short human message
	Reason  string `json:"reason,omitempty"` // the "why" — human-readable explanation
}

// writeJSON writes v as JSON with the given status. Encoding failures are logged by the
// caller's recovery middleware; we set the header/status first so a partial write is still
// a well-formed-status response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError emits the standard error envelope. reason may be empty for errors where the
// message already fully explains the cause, but prefer to always supply one.
func writeError(w http.ResponseWriter, status int, code, message, reason string) {
	writeJSON(w, status, ErrorEnvelope{Error: ErrorBody{
		Code:    code,
		Message: message,
		Reason:  reason,
	}})
}

// writeAPINotFound emits the JSON error envelope for an unmatched /api/* path. Go's default
// http.NotFound writes a plain-text "404 page not found", which violates the mandatory
// error-envelope contract (spec-digest §3) — the client's ApiError then has no `reason` to
// surface. This gives every unknown API path a machine-readable envelope with a
// human-readable reason naming the method+path that did not match.
func writeAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found",
		"no such API endpoint",
		"no route matches "+r.Method+" "+r.URL.Path)
}
