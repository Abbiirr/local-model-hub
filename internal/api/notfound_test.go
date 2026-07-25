package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAPINotFoundEnvelope asserts an unmatched /api/* path returns the mandatory JSON error
// envelope (spec-digest §3) with a human-readable reason — NOT Go's default plain-text
// "404 page not found". The client's ApiError needs `reason` to surface.
func TestAPINotFoundEnvelope(t *testing.T) {
	h := testMux(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json (not plain text)", ct)
	}
	if strings.Contains(rec.Body.String(), "404 page not found") {
		t.Errorf("got Go's default plain-text 404; want JSON envelope: %s", rec.Body.String())
	}

	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the JSON envelope: %v; body=%s", err, rec.Body.String())
	}
	if env.Error.Code != "not_found" {
		t.Errorf("code = %q, want not_found", env.Error.Code)
	}
	if env.Error.Reason == "" {
		t.Error("reason is empty; the 404 envelope must carry a human-readable reason")
	}
	if !strings.Contains(env.Error.Reason, "/api/nope") {
		t.Errorf("reason = %q, want it to name the unmatched path", env.Error.Reason)
	}
}

// TestAPINotFoundDoesNotServeSPA guards the security-in-depth rule: an unknown /api/ path
// must never leak the dashboard HTML instead of a JSON error.
func TestAPINotFoundDoesNotServeSPA(t *testing.T) {
	h := testMux(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/models/bogus/deep", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `id="root"`) {
		t.Errorf("unknown /api path served the SPA shell instead of the JSON envelope")
	}
}
