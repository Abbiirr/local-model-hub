package jsonrpc

// errors.go maps a core-surfaced Go error into a JSON-RPC error object, preserving the mandatory
// human-readable reason in data.reason (phase-6-design.md §3.5, §6 mandatory-reason invariant).
// This is the ONE place a core error becomes an *RPCError, so the reason can never be dropped on
// the RPC hop.

import (
	"errors"

	"github.com/localhub/localhub/internal/core"
)

// coreErrorToRPC folds a core.Service error into an *RPCError.
//
// Mapping (design §3.5):
//   - *core.QueuedError  → -32000 "admission blocked", data carries the numeric blocked_reason,
//     queue_id, and position (the sync caller can decide to poll the queue or resubmit). This is
//     the RPC analogue of the proxy's fast-503-with-blocked_reason (design §1.4) and the REST
//     202+position (POST /api/instances).
//   - jobsUnwiredError    → -32000 with the "jobs not wired" reason (integrator hasn't bound P6.2
//     yet) — never a panic, never a silent nil (mandatory-reason gate).
//   - any other error     → -32000 with data.reason = err.Error(). We use a single -32000 (the
//     reserved implementation-defined server-error code, JSON-RPC 2.0 §5.1) for all core-surfaced
//     errors and disambiguate via data — the transport does not attempt to translate arbitrary
//     core error strings into distinct protocol codes (that would leak business logic here).
//
// Params-level validation errors (-32602) and framing errors (-32700/-32600/-32601) are produced
// by the dispatcher/codec directly, NOT here — this function only sees errors returned FROM a core
// call, which are always execution (server) errors, i.e. the -32000 range.
func coreErrorToRPC(err error) *RPCError {
	if err == nil {
		return nil
	}

	// Admission block: a queued start (instances.start / jobs.submit) — the richest mapping.
	var qErr *core.QueuedError
	if errors.As(err, &qErr) {
		e := &RPCError{
			Code:    CodeServerError,
			Message: "admission blocked",
			Data: map[string]any{
				// reason is the numeric blocked_reason (P5 guarantees it is non-empty).
				"reason":   qErr.Reason(),
				"code":     "admission_blocked",
				"queue_id": qErr.QueueID,
				"position": qErr.Position,
			},
		}
		return e
	}

	// Jobs-not-wired sentinel: surface the integrator instruction as the reason.
	var jobsUnwired jobsUnwiredError
	if errors.As(err, &jobsUnwired) {
		return newError(CodeServerError, "jobs not wired", err.Error())
	}

	// Generic core error: preserve the full message as the reason.
	return newError(CodeServerError, "server error", err.Error())
}
