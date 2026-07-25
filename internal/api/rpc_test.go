package api

// rpc_test.go covers the POST /api/rpc HTTP transport framing (design §3.1): a well-formed call
// is 200 with a JSON-RPC body, a notification is 204 empty, and a non-parseable body is 400 with a
// -32700 Parse error object. The dispatcher itself is exhaustively unit-tested in internal/jsonrpc;
// here we only assert the HTTP handler maps has/no-reply/parse-error to the right status.
//
// WS-frame parity (the same request over /api/ws yields the same reply) is a live integrator check
// once ws.go is wired — the plan is in temp-research/phase-6-wiring-B.md. handleWSFrame's routing
// predicate isJSONRPCFrame is unit-tested below without a live socket.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/jsonrpc"
)

func newTestRPCHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	svc := newTestService(t) // real depless-ish service (DB only); hardware.get returns a config error
	d := jsonrpc.NewDispatcher(svc)
	return rpcHTTPHandler(d, quietLogger())
}

func TestRPCHTTP_WellFormedIs200WithBody(t *testing.T) {
	h := newTestRPCHandler(t)
	// engines.list needs no deps and returns a value (disabled-engine rows) → a success result.
	req := jsonReq(http.MethodPost, "/api/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","method":"engines.list","id":7}`))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not JSON: %v; body=%s", err, rec.Body.String())
	}
	if string(resp["id"]) != "7" {
		t.Errorf("echoed id = %s, want 7", resp["id"])
	}
	if _, ok := resp["result"]; !ok {
		t.Errorf("expected a result member; body=%s", rec.Body.String())
	}
}

func TestRPCHTTP_NotificationIs204(t *testing.T) {
	h := newTestRPCHandler(t)
	req := jsonReq(http.MethodPost, "/api/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","method":"engines.list"}`)) // no id → notification
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("notification produced a body: %s", rec.Body.String())
	}
}

func TestRPCHTTP_ParseErrorIs400(t *testing.T) {
	h := newTestRPCHandler(t)
	req := jsonReq(http.MethodPost, "/api/rpc",
		strings.NewReader(`{"jsonrpc":"2.0",`)) // truncated → not valid JSON
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var resp jsonrpc.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not a JSON-RPC response: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeParseError {
		t.Errorf("want -32700 parse error, got %+v", resp.Error)
	}
}

func TestRPCHTTP_CoreErrorIs200WithErrorBody(t *testing.T) {
	h := newTestRPCHandler(t)
	// hardware.get on a depless service returns a "hardware probe not configured" error → the
	// JSON-RPC error rides INSIDE a 200 body (design §3.1: only malformed JSON is a 400).
	req := jsonReq(http.MethodPost, "/api/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","method":"hardware.get","id":1}`))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (core error rides in body); body=%s", rec.Code, rec.Body.String())
	}
	var resp jsonrpc.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not a JSON-RPC response: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeServerError {
		t.Fatalf("want -32000 server error, got %+v", resp.Error)
	}
	if reason, _ := resp.Error.Data["reason"].(string); reason == "" {
		t.Error("core error must carry data.reason (mandatory-reason gate)")
	}
}

func TestRPCHTTP_Batch(t *testing.T) {
	h := newTestRPCHandler(t)
	body := `[
	  {"jsonrpc":"2.0","method":"engines.list","id":1},
	  {"jsonrpc":"2.0","method":"engines.list"},
	  {"jsonrpc":"2.0","method":"hardware.get","id":2}
	]`
	req := jsonReq(http.MethodPost, "/api/rpc", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var responses []jsonrpc.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &responses); err != nil {
		t.Fatalf("batch body not an array: %v; body=%s", err, rec.Body.String())
	}
	if len(responses) != 2 { // the notification (no id) yields no response object
		t.Fatalf("got %d responses, want 2", len(responses))
	}
}

func TestIsJSONRPCFrame(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"rpc request object", `{"jsonrpc":"2.0","method":"x","id":1}`, true},
		{"rpc notification", `{"jsonrpc":"2.0","method":"x"}`, true},
		{"rpc batch", `[{"jsonrpc":"2.0","method":"x","id":1}]`, true},
		{"server push frame", `{"type":"heartbeat","ts":"2026-07-07T00:00:00Z"}`, false},
		{"push with data", `{"type":"models.updated","data":{}}`, false},
		{"empty", ``, false},
		{"garbage", `not json`, false},
		{"empty batch", `[]`, false},
		{"non-object non-array", `5`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isJSONRPCFrame([]byte(tc.raw)); got != tc.want {
				t.Errorf("isJSONRPCFrame(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
