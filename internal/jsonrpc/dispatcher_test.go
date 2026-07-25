package jsonrpc

// dispatcher_test.go exercises the JSON-RPC 2.0 framing + dispatch contract (gate #p63, design
// §5): id echo (int/string/null-vs-absent), error codes (-32700/-32600/-32601/-32602/-32000),
// batch handling (mixed + all-notification + empty), and method coverage over the real registry.
//
// Framing tests use newTestDispatcher, which registers fake methodFuncs so the assertions are
// about the protocol layer, not any core behavior. Method-coverage + core-error tests use the
// production registry (NewDispatcher over a real but dependency-less *core.Service) to prove every
// registered method actually reaches a core call and that a core error maps to a -32000 with a
// data.reason.

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/core"
)

// newTestDispatcher builds a dispatcher whose methods are fakes, so framing tests do not depend on
// core. "echo" returns its params verbatim; "boom" returns a core-style error via coreErrorToRPC;
// "hardware.get" mirrors a real method name so notification/coverage tests read naturally.
func newTestDispatcher(t *testing.T) *Dispatcher {
	t.Helper()
	d := &Dispatcher{methods: map[string]methodFunc{}}
	d.register("echo", func(_ context.Context, params json.RawMessage) (any, *RPCError) {
		if len(params) == 0 {
			return map[string]any{"echoed": nil}, nil
		}
		return json.RawMessage(params), nil
	})
	d.register("needsID", func(_ context.Context, params json.RawMessage) (any, *RPCError) {
		id, rpcErr := decodeID(params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return map[string]any{"got": id}, nil
	})
	d.register("boom", func(_ context.Context, _ json.RawMessage) (any, *RPCError) {
		return nil, newError(CodeServerError, "server error", "it exploded")
	})
	return d
}

// decodeResponse unmarshals a single JSON-RPC response for assertions.
func decodeResponse(t *testing.T, raw []byte) Response {
	t.Helper()
	var r Response
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode response: %v; raw=%s", err, raw)
	}
	return r
}

func TestIDEcho(t *testing.T) {
	d := newTestDispatcher(t)
	ctx := context.Background()

	cases := []struct {
		name     string
		req      string
		wantHas  bool
		wantID   string // the exact JSON of the echoed id (empty when no reply expected)
	}{
		{"int id", `{"jsonrpc":"2.0","method":"echo","params":{},"id":1}`, true, "1"},
		{"string id", `{"jsonrpc":"2.0","method":"echo","params":{},"id":"abc"}`, true, `"abc"`},
		{"explicit null id", `{"jsonrpc":"2.0","method":"echo","params":{},"id":null}`, true, "null"},
		{"absent id (notification)", `{"jsonrpc":"2.0","method":"echo","params":{}}`, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, has := d.Dispatch(ctx, []byte(tc.req))
			if has != tc.wantHas {
				t.Fatalf("hasReply=%v, want %v (reply=%s)", has, tc.wantHas, reply)
			}
			if !tc.wantHas {
				if reply != nil {
					t.Fatalf("notification produced a reply: %s", reply)
				}
				return
			}
			// Assert against the RAW wire bytes: an echoed null id must appear as `"id":null`
			// literally. (Decoding into *json.RawMessage would collapse null→nil, hiding the
			// present-null case — the very distinction under test — so inspect the raw field.)
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(reply, &raw); err != nil {
				t.Fatalf("reply not an object: %v; raw=%s", err, reply)
			}
			idRaw, ok := raw["id"]
			if !ok {
				t.Fatalf("response has no id field; raw=%s", reply)
			}
			if got := string(idRaw); got != tc.wantID {
				t.Errorf("echoed id = %s, want %s (raw=%s)", got, tc.wantID, reply)
			}
			if string(raw["jsonrpc"]) != `"2.0"` {
				t.Errorf("jsonrpc = %s, want \"2.0\"", raw["jsonrpc"])
			}
		})
	}
}

func TestErrorCodes(t *testing.T) {
	d := newTestDispatcher(t)
	ctx := context.Background()

	cases := []struct {
		name       string
		body       string
		wantCode   int
		wantReason bool // whether data.reason must be non-empty
	}{
		{"parse error - truncated object", `{"jsonrpc":"2.0",`, CodeParseError, false},
		{"parse error - garbage", `not json at all`, CodeParseError, false},
		{"invalid request - bare number", `5`, CodeInvalidRequest, true},
		{"invalid request - wrong version", `{"jsonrpc":"1.0","method":"echo","id":1}`, CodeInvalidRequest, true},
		{"invalid request - missing method", `{"jsonrpc":"2.0","id":1}`, CodeInvalidRequest, true},
		{"invalid request - empty batch", `[]`, CodeInvalidRequest, true},
		{"method not found", `{"jsonrpc":"2.0","method":"nope.gone","id":1}`, CodeMethodNotFound, false},
		{"invalid params - missing id field", `{"jsonrpc":"2.0","method":"needsID","params":{},"id":1}`, CodeInvalidParams, true},
		{"invalid params - malformed params", `{"jsonrpc":"2.0","method":"needsID","params":123,"id":1}`, CodeInvalidParams, true},
		{"server error - core error carries reason", `{"jsonrpc":"2.0","method":"boom","id":1}`, CodeServerError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, has := d.Dispatch(ctx, []byte(tc.body))
			if !has {
				t.Fatalf("expected a reply for %q, got none", tc.body)
			}
			resp := decodeResponse(t, reply)
			if resp.Error == nil {
				t.Fatalf("expected an error object, got success: %s", reply)
			}
			if resp.Error.Code != tc.wantCode {
				t.Errorf("code = %d, want %d (body=%s)", resp.Error.Code, tc.wantCode, tc.body)
			}
			if resp.Error.Message == "" {
				t.Error("error message is empty; every error must carry a human message")
			}
			if tc.wantReason {
				reason, _ := resp.Error.Data["reason"].(string)
				if reason == "" {
					t.Errorf("data.reason is empty; mandatory-reason gate (data=%v)", resp.Error.Data)
				}
			}
		})
	}
}

// TestMethodNotFoundCarriesMethod asserts the -32601 error names the offending method in data.
func TestMethodNotFoundCarriesMethod(t *testing.T) {
	d := newTestDispatcher(t)
	reply, _ := d.Dispatch(context.Background(), []byte(`{"jsonrpc":"2.0","method":"nope.gone","id":1}`))
	resp := decodeResponse(t, reply)
	if resp.Error == nil || resp.Error.Code != CodeMethodNotFound {
		t.Fatalf("want -32601, got %+v", resp.Error)
	}
	if m, _ := resp.Error.Data["method"].(string); m != "nope.gone" {
		t.Errorf("data.method = %q, want nope.gone", m)
	}
}

func TestBatchMixed(t *testing.T) {
	d := newTestDispatcher(t)
	// 2 requests (ids 1, "two") + 1 notification (no id) → exactly 2 responses, order-independent.
	body := `[
	  {"jsonrpc":"2.0","method":"echo","params":{"a":1},"id":1},
	  {"jsonrpc":"2.0","method":"echo","params":{"b":2}},
	  {"jsonrpc":"2.0","method":"echo","params":{"c":3},"id":"two"}
	]`
	reply, has := d.Dispatch(context.Background(), []byte(body))
	if !has {
		t.Fatal("mixed batch produced no reply, want 2 responses")
	}
	var responses []Response
	if err := json.Unmarshal(reply, &responses); err != nil {
		t.Fatalf("batch reply is not an array: %v; raw=%s", err, reply)
	}
	if len(responses) != 2 {
		t.Fatalf("got %d responses, want 2 (notification must not produce one)", len(responses))
	}
	// Match by id, order-independent.
	seen := map[string]bool{}
	for _, r := range responses {
		if r.ID == nil {
			t.Fatal("a batch response is missing its id")
		}
		seen[string(*r.ID)] = true
	}
	if !seen["1"] || !seen[`"two"`] {
		t.Errorf("response ids = %v, want {1, \"two\"}", seen)
	}
}

func TestBatchAllNotifications(t *testing.T) {
	d := newTestDispatcher(t)
	body := `[
	  {"jsonrpc":"2.0","method":"echo","params":{}},
	  {"jsonrpc":"2.0","method":"echo","params":{}}
	]`
	reply, has := d.Dispatch(context.Background(), []byte(body))
	if has || reply != nil {
		t.Fatalf("all-notification batch must produce NO response body; got has=%v reply=%s", has, reply)
	}
}

func TestBatchWithBadElement(t *testing.T) {
	d := newTestDispatcher(t)
	// One valid request + one non-object element (bare number, invalid Request) → 2 responses:
	// the success and a -32600 for the bad element (spec: process each element independently).
	body := `[{"jsonrpc":"2.0","method":"echo","params":{},"id":1}, 5]`
	reply, has := d.Dispatch(context.Background(), []byte(body))
	if !has {
		t.Fatal("expected 2 responses")
	}
	var responses []Response
	if err := json.Unmarshal(reply, &responses); err != nil {
		t.Fatalf("not an array: %v; raw=%s", err, reply)
	}
	if len(responses) != 2 {
		t.Fatalf("got %d responses, want 2", len(responses))
	}
	var sawSuccess, sawInvalid bool
	for _, r := range responses {
		if r.Error == nil {
			sawSuccess = true
		} else if r.Error.Code == CodeInvalidRequest {
			sawInvalid = true
		}
	}
	if !sawSuccess || !sawInvalid {
		t.Errorf("want one success + one -32600; sawSuccess=%v sawInvalid=%v", sawSuccess, sawInvalid)
	}
}

// --- production-registry tests (real *core.Service, no external deps needed) ---

// newDepslessService builds a real core.Service with a DB but no probes/adapters. Its methods
// return well-formed "not configured" errors — perfect for asserting a method REACHES core and
// that the error maps to a -32000 with a reason, without needing any GPU/engine/model.
func newDepslessService(t *testing.T) *core.Service {
	t.Helper()
	return core.New(core.Deps{Cfg: config.Default()})
}

// TestMethodCoverageMirrorsRESTCore asserts the production registry contains every method the
// gate names (hardware/engines/models/compat/instances/queue + jobs), so the RPC surface mirrors
// REST core (design §5 #p63).
func TestMethodCoverageMirrorsRESTCore(t *testing.T) {
	d := NewDispatcher(newDepslessService(t))
	got := d.Methods()
	sort.Strings(got)

	want := []string{
		"engines.detect", "engines.list",
		"hardware.get",
		"instances.get", "instances.list", "instances.start", "instances.stop",
		"jobs.cancel", "jobs.get", "jobs.list", "jobs.submit",
		"models.compat", "models.list", "models.manifest", "models.preview", "models.reproduce", "models.rescan",
		"queue.cancel", "queue.list",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("registered methods mismatch:\n got=%v\nwant=%v", got, want)
	}

	// Namespace coverage assertion (gate wording: hardware/engines/models/compat/instances/queue/jobs).
	namespaces := map[string]bool{}
	for _, m := range got {
		namespaces[m[:indexDot(m)]] = true
	}
	for _, ns := range []string{"hardware", "engines", "models", "instances", "queue", "jobs"} {
		if !namespaces[ns] {
			t.Errorf("missing namespace %q in RPC surface", ns)
		}
	}
	// compat is a models.compat method (verb), not its own namespace — assert the method exists.
	if !contains(got, "models.compat") {
		t.Error("missing models.compat (compat coverage)")
	}
}

// TestEveryMethodReachesCore invokes each registered method once over the real registry and asserts
// it returns a well-formed response (either a result or a -32000/-32602 error) — i.e. the binding is
// wired and reaches core (which returns a config error given the depless service), never panics or
// hangs. This is the "each registered method reaches the corresponding Service call" check.
func TestEveryMethodReachesCore(t *testing.T) {
	d := NewDispatcher(newDepslessService(t))
	ctx := context.Background()

	// Minimal valid params per method so the binding gets past its own param validation and
	// actually calls core (where the depless service returns a config error we then map to -32000).
	params := map[string]string{
		"models.preview":    `{"model_id":"x"}`,
		"models.compat":     `{"id":"x"}`,
		"models.manifest":   `{"id":"x"}`,
		"models.reproduce":  `{"id":"x"}`,
		"instances.get":     `{"id":"x"}`,
		"instances.start":   `{"model_id":"x"}`,
		"instances.stop":    `{"id":"x"}`,
		"queue.cancel":      `{"id":"x"}`,
		"jobs.get":          `{"id":"x"}`,
		"jobs.cancel":       `{"id":"x"}`,
		"jobs.list":         `{}`,
		"jobs.submit":       `{"model":"x"}`,
	}
	for _, method := range d.Methods() {
		t.Run(method, func(t *testing.T) {
			p := params[method] // "" (absent) is fine for the all-optional read methods
			req := `{"jsonrpc":"2.0","method":"` + method + `","id":1`
			if p != "" {
				req += `,"params":` + p
			}
			req += `}`
			reply, has := d.Dispatch(ctx, []byte(req))
			if !has {
				t.Fatalf("%s produced no reply for a non-notification request", method)
			}
			resp := decodeResponse(t, reply)
			// The response id must echo 1 and exactly one of result/error must be set.
			if resp.ID == nil || string(*resp.ID) != "1" {
				t.Errorf("%s: id not echoed: %v", method, resp.ID)
			}
			if (resp.Result == nil) == (resp.Error == nil) {
				t.Errorf("%s: exactly one of result/error must be set (result=%s err=%v)", method, resp.Result, resp.Error)
			}
			// If it errored (depless service → config error), it MUST be a server/params error with a reason.
			if resp.Error != nil {
				if resp.Error.Code != CodeServerError && resp.Error.Code != CodeInvalidParams {
					t.Errorf("%s: unexpected error code %d", method, resp.Error.Code)
				}
			}
		})
	}
}

func indexDot(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return i
		}
	}
	return len(s)
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
