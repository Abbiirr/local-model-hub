package jsonrpc

// errors_test.go covers coreErrorToRPC: the admission-block (*core.QueuedError) mapping that folds
// the numeric blocked_reason + queue_id + position into data (design §3.5), the jobs-unwired
// sentinel, and a generic error. The mandatory-reason invariant (§6) requires data.reason on all.

import (
	"errors"
	"testing"

	"github.com/localhub/localhub/internal/core"
)

func TestCoreErrorToRPC_QueuedError(t *testing.T) {
	qErr := &core.QueuedError{QueueID: "q-123", Position: 4, ReasonS: "insufficient free VRAM: need 8000MiB, have 3000MiB"}
	rpcErr := coreErrorToRPC(qErr)

	if rpcErr.Code != CodeServerError {
		t.Errorf("code = %d, want %d", rpcErr.Code, CodeServerError)
	}
	if rpcErr.Message != "admission blocked" {
		t.Errorf("message = %q, want admission blocked", rpcErr.Message)
	}
	if got, _ := rpcErr.Data["reason"].(string); got != qErr.Reason() {
		t.Errorf("data.reason = %q, want %q (blocked_reason must survive)", got, qErr.Reason())
	}
	if got, _ := rpcErr.Data["queue_id"].(string); got != "q-123" {
		t.Errorf("data.queue_id = %q, want q-123", got)
	}
	if got, _ := rpcErr.Data["position"].(int); got != 4 {
		t.Errorf("data.position = %v, want 4", rpcErr.Data["position"])
	}
	if got, _ := rpcErr.Data["code"].(string); got != "admission_blocked" {
		t.Errorf("data.code = %q, want admission_blocked", got)
	}
}

// wrapped ensures errors.As unwrapping works (a queued error surfaced through a wrapping layer).
func TestCoreErrorToRPC_QueuedErrorWrapped(t *testing.T) {
	qErr := &core.QueuedError{QueueID: "q-1", Position: 1, ReasonS: "queued"}
	wrapped := errors.Join(errors.New("context"), qErr)
	rpcErr := coreErrorToRPC(wrapped)
	if rpcErr.Message != "admission blocked" {
		t.Errorf("wrapped queued error not recognized: message=%q", rpcErr.Message)
	}
}

func TestCoreErrorToRPC_JobsUnwired(t *testing.T) {
	rpcErr := coreErrorToRPC(errJobsUnwired)
	if rpcErr.Code != CodeServerError {
		t.Errorf("code = %d, want %d", rpcErr.Code, CodeServerError)
	}
	reason, _ := rpcErr.Data["reason"].(string)
	if reason == "" {
		t.Error("jobs-unwired error must carry a reason")
	}
}

func TestCoreErrorToRPC_Generic(t *testing.T) {
	rpcErr := coreErrorToRPC(errors.New("hardware probe not configured"))
	if rpcErr.Code != CodeServerError {
		t.Errorf("code = %d, want %d", rpcErr.Code, CodeServerError)
	}
	if got, _ := rpcErr.Data["reason"].(string); got != "hardware probe not configured" {
		t.Errorf("data.reason = %q, want the full error string", got)
	}
}

func TestCoreErrorToRPC_Nil(t *testing.T) {
	if coreErrorToRPC(nil) != nil {
		t.Error("nil error must map to nil *RPCError")
	}
}
