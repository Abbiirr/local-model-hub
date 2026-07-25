package jsonrpc

import "context"

// cluster.go declares the LOCAL interface the cluster.* RPC mirrors bind to (P7.2, docs/cluster-plan
// §5b). It exists here — not as a dependency on internal/cluster or internal/api types — for exactly
// the same reason as JobService: this package must import ONLY internal/core + the standard library
// (thin-transport structural gate #p63, thinness_test.go). The integrator (internal/api) supplies a
// ~3-line shim adapting the coordinator's registry + aggregator to this interface.
//
// Registration is CONDITIONAL: cluster.* methods are wired only when a non-nil ClusterService is
// passed to NewDispatcherWithServices (i.e. only on a coordinator). A role=off/node daemon — and the
// default NewDispatcher path used by the method-coverage gate — wires nil, so the RPC surface exposes
// no cluster.* methods, mirroring the REST role-gating (cluster-plan §1.3).
type ClusterService interface {
	// NodesList == GET /api/cluster/nodes: the coordinator's registry with live-derived status.
	NodesList(ctx context.Context) (any, error)
	// ModelsList == GET /api/cluster/models: fanned-out per-node inventory, node-attributed.
	ModelsList(ctx context.Context) (any, error)
	// HardwareGet == GET /api/cluster/hardware: fanned-out per-node hardware snapshots, attributed.
	HardwareGet(ctx context.Context) (any, error)
}
