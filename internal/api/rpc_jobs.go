package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/localhub/localhub/internal/core"
)

// rpc_jobs.go is the ONE place jobs.* JSON-RPC params are interpreted (design §3.2 note), keeping
// internal/jsonrpc free of the core jobs types. coreJobService adapts *core.Service (P6.2) to
// jsonrpc.JobService: decode the raw params, call core, return the marshalable JobView(s).
//
// IMPORTANT — this DEVIATES from temp-research/phase-6-wiring-B.md §4's naive shim, which did
// `json.Unmarshal(params, &core.JobSubmit{})`. That is WRONG for Agent A's actual core.JobSubmit
// shape (verified against internal/core/jobs.go): core.JobSubmit has NO json tags and its
// RequestJSON field is a []byte (which json would base64-decode). A JSON-RPC caller sends the
// OpenAI body itself as params (e.g. {"model":"x","messages":[...],"priority":1}). So — exactly like
// the REST handler internal/api/jobs.go:handleJobSubmit — we store the raw params verbatim as
// RequestJSON and decode only {model, priority, endpoint} out of it. This keeps jobs.submit over
// JSON-RPC byte-for-byte equivalent to POST /api/jobs.

// coreJobService binds core.Service's SubmitJob/GetJob/ListJobs/CancelJob (P6.2) to
// jsonrpc.JobService (raw params in, marshalable value out).
type coreJobService struct{ svc *core.Service }

// rpcJobSubmitBody mirrors api.jobSubmitBody: the small view read out of the OpenAI request body to
// resolve the target + priority/endpoint. The full params bytes are stored verbatim as RequestJSON.
type rpcJobSubmitBody struct {
	Model    string `json:"model"`
	Priority *int   `json:"priority,omitempty"` // 0 interactive | 1 normal | 2 background (default 1)
	Endpoint string `json:"endpoint,omitempty"` // "chat" (default) | "completions"
}

func (j coreJobService) SubmitJob(ctx context.Context, params json.RawMessage) (any, error) {
	if len(params) == 0 || string(params) == "null" {
		return nil, fmt.Errorf("jobs.submit: params must be an OpenAI chat/completions body with a \"model\" field")
	}
	var meta rpcJobSubmitBody
	if err := json.Unmarshal(params, &meta); err != nil {
		return nil, fmt.Errorf("jobs.submit: could not parse the request body: %w", err)
	}
	if strings.TrimSpace(meta.Model) == "" {
		return nil, fmt.Errorf("jobs.submit: the request body must include a non-empty \"model\" field")
	}
	priority := 1
	if meta.Priority != nil {
		priority = *meta.Priority
	}
	return j.svc.SubmitJob(ctx, core.JobSubmit{
		Model:       meta.Model,
		RequestJSON: []byte(params), // verbatim, so a restart re-runs the exact request
		Priority:    priority,
		Endpoint:    meta.Endpoint,
	})
}

func (j coreJobService) GetJob(ctx context.Context, id string) (any, error) {
	return j.svc.GetJob(ctx, id)
}

func (j coreJobService) ListJobs(ctx context.Context, params json.RawMessage) (any, error) {
	var f core.JobFilter
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &f); err != nil {
			return nil, fmt.Errorf("jobs.list: could not parse the filter: %w", err)
		}
	}
	return j.svc.ListJobs(ctx, f)
}

func (j coreJobService) CancelJob(ctx context.Context, id string) (any, error) {
	return j.svc.CancelJob(ctx, id)
}
