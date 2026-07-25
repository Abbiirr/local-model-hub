package jsonrpc

import (
	"context"
	"encoding/json"
)

// JobService is the LOCAL interface the jobs.* RPC methods bind to. It exists here — not as a
// dependency on concrete core types — because P6.2 (Agent A) lands the real SubmitJob/GetJob/
// ListJobs/CancelJob on core.Service AFTER this package compiles (phase-6-design.md §3.2 note:
// "jobs bound via your local interface … the integrator binds it AFTER Agent A lands"). The
// signatures are kept transport-shaped (raw params in, marshalable value out) so this package
// stays free of the core jobs types (JobView/JobSubmit/JobFilter) and compiles standalone.
//
// The integrator adapts core.Service to this interface with a ~4-line shim (see
// temp-research/wiring-p6b.md) that decodes the raw params into core.JobSubmit/core.JobFilter
// and returns the core.JobView(s). That shim is the ONE place jobs params are interpreted; it
// lives with the integrator wiring, keeping THIS package's import set to core + encoding only.
type JobService interface {
	// SubmitJob accepts an OpenAI chat/completions body plus optional {priority} and returns the
	// created JobView (state "queued") immediately.
	SubmitJob(ctx context.Context, params json.RawMessage) (any, error)
	// GetJob returns one JobView (including result_json when terminal) by id.
	GetJob(ctx context.Context, id string) (any, error)
	// ListJobs returns a filtered []JobView; params is {state?,model?,limit?}.
	ListJobs(ctx context.Context, params json.RawMessage) (any, error)
	// CancelJob cancels a job by id and returns its updated JobView.
	CancelJob(ctx context.Context, id string) (any, error)
}

// unwiredJobService is the default JobService used when the integrator has not yet bound the
// real one (e.g. this package's own tests, or a build before P6.2 lands). Every jobs.* method
// then returns a server error explaining that jobs are not wired — never a panic, never a
// silent nil (mandatory-reason gate).
type unwiredJobService struct{}

const jobsUnwiredReason = "jobs subsystem not wired: the integrator must bind core.Service's " +
	"SubmitJob/GetJob/ListJobs/CancelJob (P6.2) to jsonrpc.JobService"

func (unwiredJobService) SubmitJob(context.Context, json.RawMessage) (any, error) {
	return nil, errJobsUnwired
}
func (unwiredJobService) GetJob(context.Context, string) (any, error)  { return nil, errJobsUnwired }
func (unwiredJobService) ListJobs(context.Context, json.RawMessage) (any, error) {
	return nil, errJobsUnwired
}
func (unwiredJobService) CancelJob(context.Context, string) (any, error) { return nil, errJobsUnwired }

// errJobsUnwired is the sentinel returned by the unwired stub; coreErrorToRPC maps it to a
// -32000 server error carrying jobsUnwiredReason in data.reason.
var errJobsUnwired = jobsUnwiredError{}

type jobsUnwiredError struct{}

func (jobsUnwiredError) Error() string { return jobsUnwiredReason }
