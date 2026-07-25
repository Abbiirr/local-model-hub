package api

import (
	"mime"
	"net/http"

	"github.com/localhub/localhub/internal/proxy"
)

// csrf.go — Content-Type enforcement for state-changing JSON endpoints.
//
// Browsers send "simple" cross-site POSTs (Content-Type text/plain or form encodings) WITHOUT a
// CORS preflight and WITHOUT any server opt-in. A JSON API that decodes bodies regardless of the
// declared Content-Type is therefore CSRF-able: any web page the operator's browser loads could
// POST a JSON payload to the loopback daemon (spawn instances, start downloads, submit jobs) on
// the operator's behalf — the localhost/tailnet bind does not stop the victim's own browser.
// Requiring an explicit `Content-Type: application/json` makes every cross-site attempt a
// non-simple request, which forces a CORS preflight the daemon never approves (it sets no
// Access-Control-Allow-Origin), so the browser blocks it. Same-origin dashboard calls and
// non-browser clients (SDKs, curl -H) are unaffected — they already declare JSON.
// (full-codebase-review 2026-07-12, CRITICAL: CSRF via simple cross-site requests.)

// requireJSONBody gates a state-changing /api handler on an application/json Content-Type.
// It writes the standard 415 error envelope and returns false when the check fails.
func requireJSONBody(w http.ResponseWriter, r *http.Request) bool {
	if jsonContentType(r) {
		return true
	}
	writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
		"Content-Type must be application/json",
		"this endpoint changes state and only accepts an explicit `Content-Type: application/json` "+
			"header. Cross-site \"simple\" POSTs (text/plain, form data) skip the browser's CORS "+
			"preflight, so accepting them would let any web page drive this daemon (CSRF guard).")
	return false
}

// requireJSONBodyOpenAI is requireJSONBody with the OpenAI error shape for the /v1 proxy
// endpoints (OpenAI SDKs always send application/json, so real clients never hit this).
func requireJSONBodyOpenAI(w http.ResponseWriter, r *http.Request) bool {
	if jsonContentType(r) {
		return true
	}
	proxy.WriteOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error",
		"unsupported_media_type",
		"Content-Type must be application/json (cross-site request forgery guard; OpenAI clients always send it)")
	return false
}

// jsonContentType reports whether the request declares application/json (parameters like
// charset are allowed; a missing or non-JSON type is not).
func jsonContentType(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}
