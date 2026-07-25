package api

// rpc.go is the THIN transport wiring for the JSON-RPC 2.0 control surface (P6.3, design §3):
// the POST /api/rpc HTTP handler and the /api/ws inbound-frame router. Both funnel raw bytes into
// ONE shared jsonrpc.Dispatcher (the same instance serves HTTP and WS, so a method behaves
// identically on both — gate #p63 "works over HTTP POST and WS"). There is ZERO business logic
// here: dispatch, then frame the reply per transport. All method logic lives in the dispatcher's
// bound core calls (internal/jsonrpc), which are themselves one-line core.Service calls.
//
// WIRING (integrator — see temp-research/phase-6-wiring-B.md):
//   server.go routes(): mux.HandleFunc("POST /api/rpc", rpcHTTPHandler(s.rpc, s.logger))
//   ws.go handleWS():   replace `ctx = conn.CloseRead(ctx)` with the read loop that calls
//                       handleWSFrame(ctx, s.rpc, raw) on each inbound text frame.
//   New()/Server:       add an `rpc *jsonrpc.Dispatcher` field, set it to
//                       jsonrpc.NewDispatcher(svc) (or NewDispatcherWithJobs once P6.2 lands).
//
// These are FREE functions (not *Server methods) on purpose: rpc.go compiles standalone without
// touching server.go/ws.go (Agent A tests the api package concurrently). The integrator adds the
// one Server field + two wiring lines in the serial tail.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/localhub/localhub/internal/jsonrpc"
)

// rpcMaxBodyBytes caps a single /api/rpc request body. JSON-RPC control calls are small (a method
// name + a few params); a multi-MB body is abuse, not a control call. This mirrors the loopback,
// low-trust posture — the surface is localhost-bound but still gets a defensive ceiling.
const rpcMaxBodyBytes = 1 << 20 // 1 MiB

// rpcHTTPHandler builds the POST /api/rpc handler over the shared dispatcher. Framing (design §3.1):
//   - well-formed transport (single object OR batch)     → HTTP 200, JSON-RPC reply body (errors
//     ride inside the body's `error` object, NOT the HTTP status).
//   - all-notification request / all-notification batch  → HTTP 204, empty body (nothing to send).
//   - non-parseable JSON body                            → HTTP 400 + a -32700 Parse error object.
//
// The dispatcher decides has/no-reply and parse-error; this handler only maps that to HTTP status.
func rpcHTTPHandler(d *jsonrpc.Dispatcher, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireJSONBody(w, r) { // CSRF guard: the dispatcher mutates state (instances.start/stop, jobs.submit)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, rpcMaxBodyBytes))
		if err != nil {
			// A read failure is a transport problem, not a JSON-RPC one; use the standard envelope.
			writeError(w, http.StatusBadRequest, "bad_request",
				"could not read request body", "the /api/rpc body could not be read: "+err.Error())
			return
		}

		reply, hasReply := d.Dispatch(r.Context(), body)
		if !hasReply {
			// A notification (or all-notification batch): execute, send no body (§4.1 / §6).
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// A non-parseable body yields a lone -32700 Parse error object; per §3.1 that (and only
		// that) rides on a transport-level 400. Everything else (valid transport, even if the
		// method errored) is a 200 with the JSON-RPC error inside the body.
		status := http.StatusOK
		if !json.Valid(body) {
			status = http.StatusBadRequest
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		if _, werr := w.Write(reply); werr != nil {
			logger.Debug("rpc http write ended", "err", werr)
		}
	}
}

// handleWSFrame routes one inbound WebSocket text frame. It reports whether the frame was a
// JSON-RPC request (a frame carrying "jsonrpc":"2.0" — design §3.1 frame disambiguation) and, if
// so, writes the JSON-RPC response frame back over the same connection.
//
// Return value `isRPC` lets the ws read loop distinguish "this was an RPC frame I handled" from
// "some other client frame" (which the push-only channel logs and ignores). A JSON-RPC
// notification is an RPC frame that produces no response (isRPC=true, but nothing written).
//
// It writes at most one text frame (the response) using the same short-deadline pattern as the
// push path; a write error is returned so the caller can decide whether to end the loop. A frame
// that is not a JSON-RPC request returns isRPC=false and writes nothing.
func handleWSFrame(ctx context.Context, conn *websocket.Conn, d *jsonrpc.Dispatcher, raw []byte, logger *slog.Logger) (isRPC bool, err error) {
	if !isJSONRPCFrame(raw) {
		return false, nil // not an RPC request: the push channel ignores client frames (Phase 1)
	}
	reply, hasReply := d.Dispatch(ctx, raw)
	if !hasReply {
		return true, nil // a JSON-RPC notification: executed, no response frame (§4.1)
	}
	writeCtx, cancel := context.WithTimeout(ctx, rpcWSWriteDeadline)
	defer cancel()
	if werr := conn.Write(writeCtx, websocket.MessageText, reply); werr != nil {
		logger.Info("rpc ws write ended", "err", werr)
		return true, werr
	}
	return true, nil
}

// isJSONRPCFrame reports whether a raw text frame is a JSON-RPC request: a JSON object (or batch
// array) carrying a "jsonrpc" member. We do a cheap structural check — decode into a shape that
// only pulls "jsonrpc" — so a server→client push frame ({"type","data"}, no jsonrpc) is NOT
// misrouted, and a batch array (which starts with '[') is recognized too.
//
// A batch is treated as RPC iff its FIRST element carries "jsonrpc" (a batch is homogeneous in
// practice; if the first element is an RPC request the whole array is dispatched, and the
// dispatcher validates each element independently, so a stray non-RPC element still gets a proper
// -32600 rather than being silently dropped).
func isJSONRPCFrame(raw []byte) bool {
	trimmed := trimSpaceBytes(raw)
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '{':
		var probe struct {
			JSONRPC *string `json:"jsonrpc"`
		}
		if err := json.Unmarshal(trimmed, &probe); err != nil {
			return false
		}
		return probe.JSONRPC != nil
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(trimmed, &elems); err != nil || len(elems) == 0 {
			return false
		}
		var probe struct {
			JSONRPC *string `json:"jsonrpc"`
		}
		if err := json.Unmarshal(elems[0], &probe); err != nil {
			return false
		}
		return probe.JSONRPC != nil
	default:
		return false
	}
}

// trimSpaceBytes trims leading ASCII whitespace so the first structural byte can be inspected
// without pulling in bytes.TrimSpace for one call site (keeps this file's import set minimal).
func trimSpaceBytes(b []byte) []byte {
	i := 0
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return b[i:]
		}
	}
	return b[i:]
}

// rpcWSWriteDeadline bounds a single JSON-RPC response-frame write so a dead client cannot wedge
// the ws goroutine. It matches the 10s push-path write deadline in ws.go (wsSend).
const rpcWSWriteDeadline = 10 * time.Second
