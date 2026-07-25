package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
)

// Dispatch is the transport-agnostic entry point: it takes a raw JSON-RPC payload (a single
// request object OR a batch array) and returns the raw JSON reply bytes plus a hasReply flag.
//
// hasReply is false (and reply is nil) when there is NOTHING to send back per spec:
//   - a single notification (no id) → no response (§4.1)
//   - a batch containing ONLY notifications → no response (§6)
//
// A well-formed transport always yields hasReply=false OR a marshalable reply; the caller
// decides the HTTP/WS framing (POST /api/rpc → 200 with reply or 204 empty; ws → write the
// reply frame or nothing). Malformed JSON yields a single -32700 Parse error response object
// (hasReply=true) — the HTTP layer sends it with 400; the ws layer sends it as a frame.
func (d *Dispatcher) Dispatch(ctx context.Context, raw []byte) (reply []byte, hasReply bool) {
	// Distinguish a batch (array) from a single object by the first non-whitespace byte, then
	// let json.Unmarshal validate the actual shape. An empty/whitespace body is a parse error.
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return mustMarshal(errorResponse(nullID(), newError(CodeParseError, "parse error", ""))), true
	}

	if trimmed[0] == '[' {
		return d.dispatchBatch(ctx, trimmed)
	}
	// Single request. A non-parseable body is a -32700 Parse error, NOT -32600 (Codex finding 2 /
	// design §3.5: "any unparseable body, single or batch → -32700"). We check json.Valid here so a
	// malformed non-batch body like `{"jsonrpc":"2.0",` maps to parse-error; dispatchOne then only
	// ever sees valid JSON and reserves -32600 for well-formed-but-not-a-Request values.
	if !json.Valid(trimmed) {
		return mustMarshal(errorResponse(nullID(), newError(CodeParseError, "parse error", ""))), true
	}
	resp, has := d.dispatchOne(ctx, trimmed)
	if !has {
		return nil, false // a notification: nothing to send
	}
	return mustMarshal(resp), true
}

// dispatchBatch handles a JSON-RPC 2.0 batch (array) per §6.
func (d *Dispatcher) dispatchBatch(ctx context.Context, raw []byte) (reply []byte, hasReply bool) {
	// Decode into raw elements first so each element is validated independently and a single
	// bad element does not sink the whole batch (spec: process each; return an array of the
	// non-notification results).
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		// The array itself is not valid JSON → parse error, id null.
		return mustMarshal(errorResponse(nullID(), newError(CodeParseError, "parse error", ""))), true
	}
	// An empty batch array `[]` is an Invalid Request per §6 (nothing to process, but the array
	// is well-formed) → a single Invalid Request error object, id null.
	if len(elems) == 0 {
		return mustMarshal(errorResponse(nullID(), newError(CodeInvalidRequest, "invalid request", "empty batch array"))), true
	}

	responses := make([]Response, 0, len(elems))
	for _, elem := range elems {
		resp, has := d.dispatchOne(ctx, elem)
		if has {
			responses = append(responses, resp)
		}
	}
	// If every element was a notification, the batch produces NO response (§6): hasReply=false.
	if len(responses) == 0 {
		return nil, false
	}
	return mustMarshal(responses), true
}

// dispatchOne validates and executes a single request element. It returns the response and a
// hasResponse flag (false for a valid notification, which produces no response). Every error
// path except a notification produces a response object.
//
// Order of checks matters for correct error codes:
//  1. element parses as an object at all      → else -32600 Invalid Request (a batch element
//     that is not an object; a scalar/array is not a Request)
//  2. jsonrpc == "2.0" and method is a string → else -32600 Invalid Request
//  3. method is registered                    → else -32601 Method not found
//  4. the bound method runs; its error maps via coreErrorToRPC (-32602 / -32000)
func (d *Dispatcher) dispatchOne(ctx context.Context, raw []byte) (Response, bool) {
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		// Not a JSON object (e.g. a bare number/string/array element in a batch). We cannot
		// recover an id, so echo null. This is an Invalid Request, not a parse error — the outer
		// JSON already parsed (we are inside a decoded batch element or a single object attempt).
		return errorResponse(nullID(), newError(CodeInvalidRequest, "invalid request", err.Error())), true
	}

	isNotif := req.isNotification()

	// Validate the request envelope. On a validation failure the response id echoes the request
	// id if present (the id is still meaningful even on an invalid request), else null.
	if req.JSONRPC != jsonrpcVersion {
		if isNotif {
			return Response{}, false // malformed notification: spec is silent; we drop it (no id to reply to)
		}
		return errorResponse(idOrNull(req.ID), newError(CodeInvalidRequest, "invalid request", `"jsonrpc" must be "2.0"`)), true
	}
	if req.Method == "" {
		if isNotif {
			return Response{}, false
		}
		return errorResponse(idOrNull(req.ID), newError(CodeInvalidRequest, "invalid request", "missing method")), true
	}

	method, ok := d.methods[req.Method]
	if !ok {
		if isNotif {
			return Response{}, false // unknown notification: no reply (§4.1)
		}
		notFound := newError(CodeMethodNotFound, "method not found: "+req.Method, "").withMethod(req.Method)
		return errorResponse(req.ID, notFound), true
	}

	// Execute the bound core call. The method returns a marshalable result OR an *RPCError.
	result, rpcErr := method(ctx, req.Params)
	if isNotif {
		return Response{}, false // notification: run it, but send no response even on error (§4.1)
	}
	if rpcErr != nil {
		return errorResponse(req.ID, rpcErr), true
	}
	resultRaw, merr := json.Marshal(result)
	if merr != nil {
		// A result that will not marshal is a server bug; surface it as a server error, not a panic.
		return errorResponse(req.ID, newError(CodeServerError, "result marshal failed", merr.Error())), true
	}
	return successResponse(req.ID, resultRaw), true
}

// idOrNull returns the request id if present, else a null id. Used on invalid-request replies
// where the id may or may not have been supplied.
func idOrNull(id *json.RawMessage) *json.RawMessage {
	if id == nil {
		return nullID()
	}
	return id
}

// withMethod adds the offending method name to a method-not-found error's data object so the
// client can see which method it asked for (phase-6-design.md §3.5).
func (e *RPCError) withMethod(method string) *RPCError {
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	e.Data["method"] = method
	return e
}

// mustMarshal marshals a value we constructed ourselves (a Response or []Response). These are
// always marshalable; a failure is a programming error, so we fall back to a hard-coded
// internal-error object rather than propagating a nil reply.
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"response marshal failed"},"id":null}`)
	}
	return b
}
