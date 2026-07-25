package jsonrpc

// dispatcher.go holds the method registry: the map from a JSON-RPC method name to a bound
// core call. Every entry is a THIN adapter (phase-6-design.md §3.2, §3.6): decode params →
// one core.Service call → return the marshalable value or an *RPCError via coreErrorToRPC.
// There is ZERO business logic here — no admission, no estimation, no engine calls. If a
// binding needs more than "decode, call, map error", the logic belongs in core.Service.
//
// Thin-transport structural rule (gate #p63): this package imports only `core` (the single
// service core whose methods it forwards to) + the standard library (context, encoding/json).
// No db, no engine, no residency imports. The bindings below are the only place that fact is
// load-bearing — keep them one-liners.

import (
	"context"
	"encoding/json"

	"github.com/localhub/localhub/internal/core"
)

// methodFunc is one registry entry: raw params in, a marshalable result OR an *RPCError out.
// The dispatcher marshals a nil-error result; coreErrorToRPC maps a returned error.
type methodFunc func(ctx context.Context, params json.RawMessage) (any, *RPCError)

// Dispatcher owns the method registry and is the transport-agnostic execution engine. One
// Dispatcher instance is shared by POST /api/rpc and the /api/ws inbound frame path — both
// call Dispatch (see codec.go). It is safe for concurrent use: methods is read-only after
// construction and every bound core method is itself concurrency-safe.
type Dispatcher struct {
	methods map[string]methodFunc
}

// register adds one method binding. It is unexported so the method set is fixed at construction
// (production) or by the in-package test that builds a dispatcher from fake methodFuncs. Keeping
// it unexported preserves the "the registry is the complete, auditable surface" property.
func (d *Dispatcher) register(name string, fn methodFunc) { d.methods[name] = fn }

// Methods returns the set of registered method names (unordered). Used by the coverage test
// (design §5 #p63 "method coverage mirrors REST core") and any future introspection endpoint.
func (d *Dispatcher) Methods() []string {
	out := make([]string, 0, len(d.methods))
	for name := range d.methods {
		out = append(out, name)
	}
	return out
}

// NewDispatcher builds the production registry, binding every exposed method to svc (the single
// service core). Jobs methods bind to the unwired stub by default; use NewDispatcherWithJobs to
// supply the P6.2-backed JobService once Agent A's core jobs methods land (see
// temp-research/phase-6-wiring-B.md).
//
// Binding to the concrete *core.Service (not an interface) is deliberate: it keeps this package's
// import set to exactly {core, context, encoding/json} — the thin-transport structural gate. The
// framing/dispatch logic is tested independently via newTestDispatcher (dispatcher registers fake
// methodFuncs), so no fake *core.Service is needed.
func NewDispatcher(svc *core.Service) *Dispatcher {
	return NewDispatcherWithJobs(svc, unwiredJobService{})
}

// NewDispatcherWithJobs builds the production registry with an explicit JobService. The integrator
// calls this from main.go passing a ~4-line shim that adapts core.Service's SubmitJob/GetJob/
// ListJobs/CancelJob (P6.2) to jsonrpc.JobService (design §3.2 note; wiring doc has the snippet).
func NewDispatcherWithJobs(svc *core.Service, jobs JobService) *Dispatcher {
	return NewDispatcherWithServices(svc, jobs, nil)
}

// NewDispatcherWithServices builds the production registry with explicit jobs + cluster services. It
// is the full constructor; NewDispatcher / NewDispatcherWithJobs delegate here with unwired/nil
// services. clusterSvc gates the cluster.* mirrors (P7.2): nil (role off/node, and the coverage gate's
// NewDispatcher path) registers NO cluster methods, so the RPC surface mirrors the REST role-gating.
func NewDispatcherWithServices(svc *core.Service, jobs JobService, clusterSvc ClusterService) *Dispatcher {
	d := &Dispatcher{methods: map[string]methodFunc{}}

	// --- hardware ---
	d.register("hardware.get", func(ctx context.Context, _ json.RawMessage) (any, *RPCError) {
		return callErr(svc.Hardware(ctx))
	})

	// --- engines ---
	// Engines/DetectEngines return no error (verified: core/engines.go) — a nil adapter yields a
	// Detected=false status row, not a Go error. So these bindings never call coreErrorToRPC.
	d.register("engines.list", func(ctx context.Context, _ json.RawMessage) (any, *RPCError) {
		return svc.Engines(ctx), nil
	})
	d.register("engines.detect", func(ctx context.Context, _ json.RawMessage) (any, *RPCError) {
		return svc.DetectEngines(ctx), nil // re-probe + engines.updated event
	})

	// --- models ---
	d.register("models.list", func(ctx context.Context, _ json.RawMessage) (any, *RPCError) {
		return callErr(svc.Models(ctx))
	})
	d.register("models.rescan", func(ctx context.Context, _ json.RawMessage) (any, *RPCError) {
		return callErr(svc.RescanModels(ctx))
	})
	// models.preview: a compat verdict WITHOUT downloading weights, selecting a remote/inventory
	// model (design §3.2). params IS a core.CompatRequest ({model_id}|{hf_repo,file}|{hf_repo}).
	d.register("models.preview", func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
		var req core.CompatRequest
		if rpcErr := decodeParams(params, &req); rpcErr != nil {
			return nil, rpcErr
		}
		return callErr(svc.CompatPreview(ctx, req))
	})
	// models.compat: the verdict for ONE inventory model by id (+ optional ctx/batch). The core has
	// no separate ModelCompat method; the REST GET /api/models/{id}/compat handler builds a
	// CompatRequest{ModelID:id} (verified: api/handlers.go handleModelCompat) — we mirror it exactly.
	d.register("models.compat", func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
		var p compatByIDParams
		if rpcErr := decodeParams(params, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if p.ID == "" {
			return nil, newError(CodeInvalidParams, "invalid params", `"id" is required`)
		}
		return callErr(svc.CompatPreview(ctx, core.CompatRequest{ModelID: p.ID, Ctx: p.Ctx, Batch: p.Batch}))
	})
	// models.manifest: the immutable supply-chain manifest linked to a model id (design §3.2
	// "models.manifest" → ManifestForModel, verified core method name).
	d.register("models.manifest", func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
		id, rpcErr := decodeID(params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return callErr(svc.ManifestForModel(ctx, id))
	})
	d.register("models.reproduce", func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
		id, rpcErr := decodeID(params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return callErr(svc.Reproduce(ctx, id))
	})

	// --- instances ---
	d.register("instances.list", func(ctx context.Context, _ json.RawMessage) (any, *RPCError) {
		return callErr(svc.Instances(ctx))
	})
	d.register("instances.get", func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
		id, rpcErr := decodeID(params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return callErr(svc.Instance(ctx, id))
	})
	// instances.start: routes through the full P5 admission (warm→route / fits→load / evict→load /
	// queue / reject). A queued result surfaces as a *core.QueuedError, folded into a -32000 with
	// blocked_reason + queue_id + position in data (design §3.5). params IS a StartInstanceRequest.
	d.register("instances.start", func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
		var req core.StartInstanceRequest
		if rpcErr := decodeParams(params, &req); rpcErr != nil {
			return nil, rpcErr
		}
		if req.ModelID == "" {
			return nil, newError(CodeInvalidParams, "invalid params", `"model_id" is required`)
		}
		return callErr(svc.StartInstance(ctx, req))
	})
	d.register("instances.stop", func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
		id, rpcErr := decodeID(params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return callErr(svc.StopInstanceAsync(ctx, id))
	})

	// --- queue ---
	d.register("queue.list", func(ctx context.Context, _ json.RawMessage) (any, *RPCError) {
		return callErr(svc.Queue(ctx))
	})
	// queue.cancel returns only an error from core (CancelQueueEntry). On success we return a small
	// {ok:true,id} ack so the RPC result is non-null and self-describing.
	d.register("queue.cancel", func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
		id, rpcErr := decodeID(params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		if err := svc.CancelQueueEntry(ctx, id); err != nil {
			return nil, coreErrorToRPC(err)
		}
		return map[string]any{"ok": true, "id": id}, nil
	})

	// --- jobs (P6.2; bound to `jobs`, the unwired stub until Agent A lands core jobs methods) ---
	d.register("jobs.submit", func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
		return callErr(jobs.SubmitJob(ctx, params))
	})
	d.register("jobs.get", func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
		id, rpcErr := decodeID(params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return callErr(jobs.GetJob(ctx, id))
	})
	d.register("jobs.list", func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
		return callErr(jobs.ListJobs(ctx, params))
	})
	d.register("jobs.cancel", func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
		id, rpcErr := decodeID(params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return callErr(jobs.CancelJob(ctx, id))
	})

	// --- cluster (P7.2 mirrors; registered ONLY on a coordinator — see NewDispatcherWithServices) ---
	// Thin one-liners into the injected ClusterService (cluster-plan §5b: "thin one-line calls into
	// the coordinator core"). Join/heartbeat stays REST-only (machine-to-machine, not an operator
	// surface, cluster-plan §5b).
	if clusterSvc != nil {
		d.register("cluster.nodes.list", func(ctx context.Context, _ json.RawMessage) (any, *RPCError) {
			return callErr(clusterSvc.NodesList(ctx))
		})
		d.register("cluster.models.list", func(ctx context.Context, _ json.RawMessage) (any, *RPCError) {
			return callErr(clusterSvc.ModelsList(ctx))
		})
		d.register("cluster.hardware.get", func(ctx context.Context, _ json.RawMessage) (any, *RPCError) {
			return callErr(clusterSvc.HardwareGet(ctx))
		})
	}

	return d
}

// callErr is the shared tail of every binding: it turns a (result, error) core return into the
// dispatcher's (any, *RPCError) shape, mapping the error through coreErrorToRPC so the mandatory
// reason survives. On success the result is returned untouched for the dispatcher to marshal.
func callErr[T any](result T, err error) (any, *RPCError) {
	if err != nil {
		return nil, coreErrorToRPC(err)
	}
	return result, nil
}
