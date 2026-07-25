package api

import (
	"io"
	"net/http"
	"net/http/httptest"
)

// jsonReq builds an httptest request that declares Content-Type: application/json for a non-nil
// body — mirroring every real client (browser dashboard, OpenAI SDK, curl -H, the cluster node
// client). It exists because the CSRF guard (csrf.go) rejects state-changing bodies that do not
// declare JSON; tests must model a real client, not a bare cross-site "simple" POST.
func jsonReq(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}
