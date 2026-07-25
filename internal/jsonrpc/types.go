// Package jsonrpc is the THIN JSON-RPC 2.0 control-plane surface over the single service
// core (PLAN.md decision 7, phase-6-design.md §3). It contains ZERO business logic: every
// registered method is a one-line call into a core.Service method plus marshal/unmarshal.
// It imports only `core` + the standard library (encoding/json, net/http, context) — no DB,
// no engine, no residency packages. If you find yourself reaching for one of those here, the
// logic belongs in core.Service instead.
//
// Transport-agnostic by design: the same Dispatcher serves both POST /api/rpc (single object
// OR batch array) and inbound /api/ws text frames (a frame carrying "jsonrpc":"2.0" is an RPC
// request; anything else is left for the push channel). The integrator wires the two entry
// points (ServeHTTP + HandleWSFrame) — see temp-research/wiring-p6b.md.
package jsonrpc

import "encoding/json"

// jsonrpcVersion is the only protocol version this surface speaks (JSON-RPC 2.0).
const jsonrpcVersion = "2.0"

// Standard JSON-RPC 2.0 error codes (§5.1) plus our implementation-defined server-error code.
// -32000..-32099 is the reserved implementation-defined server-error range; we use a single
// -32000 for all core-surfaced errors and disambiguate via data.code (phase-6-design.md §3.5).
const (
	CodeParseError     = -32700 // invalid JSON was received
	CodeInvalidRequest = -32600 // the JSON is not a valid Request object
	CodeMethodNotFound = -32601 // the method does not exist / is not available
	CodeInvalidParams  = -32602 // invalid method parameter(s)
	CodeServerError    = -32000 // a core method returned an error (data.reason carries the why)
)

// Request is a single JSON-RPC 2.0 request object.
//
// The id member needs three-way discrimination the spec (§4.1, §5) is strict about:
//   - id absent           → notification (execute, send NO response)
//   - id present == null   → normal request, echo null
//   - id present == 1/"x"  → normal request, echo verbatim
//
// A plain `*json.RawMessage` field with encoding/json CANNOT distinguish absent from present-null:
// both leave the pointer nil (Codex finding 1). So Request has a custom UnmarshalJSON that records
// whether the "id" KEY was present (idPresent) separately from its value (ID). idPresent drives
// notification detection; ID drives the echo.
//
// params is json.RawMessage so each method decodes its own shape (thin transport: the dispatcher
// never interprets params — it hands the raw bytes to the bound method).
type Request struct {
	JSONRPC   string
	Method    string
	Params    json.RawMessage
	ID        *json.RawMessage // the id value (may be a pointer to the literal "null")
	idPresent bool             // whether the "id" key appeared at all (absent ⇒ notification)
}

// UnmarshalJSON decodes a Request while recording id-key presence. It first decodes into a raw
// key→value map to detect whether "id" was supplied (present-null vs absent), then pulls the
// typed fields. Any unparseable body is surfaced as an error by the caller's json.Unmarshal wrapper
// — here we only reach this code for structurally valid JSON objects.
func (r *Request) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v, ok := raw["jsonrpc"]; ok {
		_ = json.Unmarshal(v, &r.JSONRPC) // best-effort; a wrong-typed jsonrpc stays "" → invalid request
	}
	if v, ok := raw["method"]; ok {
		_ = json.Unmarshal(v, &r.Method)
	}
	if v, ok := raw["params"]; ok {
		r.Params = v
	}
	if v, ok := raw["id"]; ok {
		r.idPresent = true
		id := v // copy so &id is stable
		r.ID = &id
	}
	return nil
}

// isNotification reports whether this request is a notification (id key ABSENT → no reply). Per
// spec §4.1 a request WITHOUT an id member is a notification; a present null id is NOT a
// notification (it is a normal request whose id echoes as null).
func (r *Request) isNotification() bool { return !r.idPresent }

// Response is a single JSON-RPC 2.0 response object. Exactly one of Result / Error is set on a
// well-formed response; we marshal Result even when it is JSON null (a method may legitimately
// return null), so Result uses json.RawMessage and Error omitempty.
type Response struct {
	JSONRPC string           `json:"jsonrpc"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
	ID      *json.RawMessage `json:"id"` // always present in a response (null on pre-dispatch errors)
}

// RPCError is the JSON-RPC 2.0 error object. Data is optional structured detail; we use it to
// carry the mandatory human-readable `reason` through to the RPC surface (the mandatory-reason
// gate: proxy 503 message, job reason, RPC data.reason — phase-6-design.md §6).
type RPCError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

// newError builds an RPCError with an optional reason folded into data.reason. Passing an
// empty reason omits the data object entirely (a bare parse error needs no data).
func newError(code int, message, reason string) *RPCError {
	e := &RPCError{Code: code, Message: message}
	if reason != "" {
		e.Data = map[string]any{"reason": reason}
	}
	return e
}

// successResponse builds a success Response, echoing the request id verbatim. result is
// pre-marshaled RawMessage (json null when the method returned nil).
func successResponse(id *json.RawMessage, result json.RawMessage) Response {
	return Response{JSONRPC: jsonrpcVersion, Result: result, ID: id}
}

// errorResponse builds an error Response, echoing the request id (or null when the id could
// not be determined — e.g. a parse error or a non-object element).
func errorResponse(id *json.RawMessage, rpcErr *RPCError) Response {
	return Response{JSONRPC: jsonrpcVersion, Error: rpcErr, ID: id}
}

// nullID is the JSON null id used for responses to requests whose id could not be recovered
// (parse errors, invalid-request objects). Per spec §5, id MUST be null in those cases.
func nullID() *json.RawMessage {
	raw := json.RawMessage("null")
	return &raw
}
