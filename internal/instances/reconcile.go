package instances

import (
	"context"
	"fmt"

	"github.com/localhub/localhub/internal/instances/proc"
)

// HealthChecker probes whether the process behind a base URL answers its health endpoint AND
// is serving the expected model. It is a seam so the reconciler (and adapter) can be tested
// with a fake, and so the llama.cpp specifics (GET /health, model path match) live in the
// adapter, not here.
//
// Return values:
//   - healthy: the endpoint answered OK.
//   - identityOK: the answering process is serving the model we recorded (port + model path
//     match). A healthy-but-wrong-model process is a reused-port collision → reap.
//   - err: transport/timeout; treated as not-healthy by the reconciler.
type HealthChecker interface {
	Check(ctx context.Context, inst Instance) (healthy bool, identityOK bool, err error)
}

// HealthCheckFunc adapts a function to HealthChecker.
type HealthCheckFunc func(ctx context.Context, inst Instance) (bool, bool, error)

func (f HealthCheckFunc) Check(ctx context.Context, inst Instance) (bool, bool, error) {
	return f(ctx, inst)
}

// AliveChecker reports whether a recorded PID is currently a live process. Seam for tests
// (default wraps proc.IsAlive).
type AliveChecker func(pid int) bool

// TreeKiller force-kills the tree rooted at a PID (reconcile-time orphan reaping). Seam for
// tests (default wraps proc.KillTree).
type TreeKiller func(pid int) error

// ReconcileResult summarizes a reconciliation pass for logging + the P4.1 gate evidence.
type ReconcileResult struct {
	Reattached []string `json:"reattached"` // instance IDs re-attached as HEALTHY
	Reaped     []string `json:"reaped"`     // instance IDs killed + marked FAILED (orphan/mismatch)
	Stopped    []string `json:"stopped"`    // instance IDs marked STOPPED (process already gone)
	Errors     []string `json:"errors"`     // non-fatal per-instance errors
}

// Reconciler runs the daemon-startup reconciliation over the instances table. For each
// non-terminal recorded instance it decides one of three outcomes (spec-digest §1, architecture
// §Process/state model, TESTING.md#p41):
//
//	PID alive AND health answers AND identity matches  → re-attach as HEALTHY.
//	PID alive but unhealthy/wrong-model                → kill tree + FAILED ("orphan reaped at reconcile").
//	PID dead                                           → STOPPED ("process gone at reconcile").
//
// It frees the port for reaped/stopped instances so the pool does not leak.
type Reconciler struct {
	store  *Store
	pool   *PortPool
	health HealthChecker
	alive  AliveChecker
	kill   TreeKiller
}

// NewReconciler builds a reconciler. alive/kill default to the real proc implementations when
// nil (tests inject fakes).
func NewReconciler(store *Store, pool *PortPool, health HealthChecker, alive AliveChecker, kill TreeKiller) *Reconciler {
	if alive == nil {
		alive = proc.IsAlive
	}
	if kill == nil {
		kill = proc.KillTree
	}
	return &Reconciler{store: store, pool: pool, health: health, alive: alive, kill: kill}
}

// Reconcile runs one pass over all non-terminal instances. It is safe to call on every daemon
// startup (playbook rule: reconcile state on every startup). Only llama.cpp/vLLM-owned
// instances (with a PID) are process-reconciled here; Ollama instances (PID 0) are left to the
// Ollama adapter's /api/ps mirror (P4.3) and skipped.
func (r *Reconciler) Reconcile(ctx context.Context) (ReconcileResult, error) {
	var res ReconcileResult
	active, err := r.store.List(ctx, true)
	if err != nil {
		return res, fmt.Errorf("list active instances for reconcile: %w", err)
	}
	for _, inst := range active {
		// Ollama has no owned PID/port — its residency is mirrored from /api/ps elsewhere.
		if inst.Engine == EngineOllama || inst.PID == 0 {
			continue
		}
		r.reconcileOne(ctx, inst, &res)
	}
	return res, nil
}

func (r *Reconciler) reconcileOne(ctx context.Context, inst Instance, res *ReconcileResult) {
	// PID dead → the process is gone; mark STOPPED and free its port.
	if !r.alive(inst.PID) {
		if _, err := r.transitionTo(ctx, inst, StateStopped, "process gone at reconcile (PID "+itoa(inst.PID)+" not alive)"); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: mark stopped: %v", inst.ID, err))
			return
		}
		_ = r.pool.Free(ctx, inst.Port)
		res.Stopped = append(res.Stopped, inst.ID)
		return
	}

	// PID alive → probe health + identity.
	healthy, identityOK, herr := r.health.Check(ctx, inst)
	if herr == nil && healthy && identityOK {
		// Re-attach: the process is ours and serving the right model. If it was mid-STARTING,
		// move it to HEALTHY; if already HEALTHY, leave it (a same-state "transition" would be
		// illegal, so only advance when needed).
		if inst.State != StateHealthy {
			if _, err := r.transitionTo(ctx, inst, StateHealthy, "reattached at reconcile: PID alive, /health OK, model identity matches"); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: reattach: %v", inst.ID, err))
				return
			}
		}
		res.Reattached = append(res.Reattached, inst.ID)
		return
	}

	// PID alive but unhealthy or wrong model → reap the tree and mark FAILED.
	_ = r.kill(inst.PID)
	reason := "orphan reaped at reconcile"
	if herr != nil {
		reason += " (health probe error: " + herr.Error() + ")"
	} else if !identityOK {
		reason += " (PID alive but serving a different/unknown model — reused-PID collision)"
	} else {
		reason += " (PID alive but /health did not answer)"
	}
	if _, err := r.transitionTo(ctx, inst, StateFailed, reason); err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("%s: mark failed: %v", inst.ID, err))
		return
	}
	_ = r.pool.Free(ctx, inst.Port)
	res.Reaped = append(res.Reaped, inst.ID)
}

// transitionTo moves an instance to `to`, tolerating an already-terminal current state (a
// concurrent stop). It returns the updated instance or an error other than illegal-transition
// against a terminal state (which it swallows as a no-op).
func (r *Reconciler) transitionTo(ctx context.Context, inst Instance, to State, reason string) (Instance, error) {
	// Guard: if the recorded state cannot legally reach `to`, but IS terminal, treat as done.
	if !CanTransition(inst.State, to) {
		if inst.State.terminal() {
			return inst, nil
		}
		// Non-terminal but illegal (e.g. REQUESTED→STOPPED is legal via STARTING only). For the
		// reconcile outcomes we use (STOPPED/FAILED/HEALTHY), the source states in practice are
		// STARTING/HEALTHY/DRAINING which can all reach STOPPED/FAILED; HEALTHY is reachable from
		// STARTING. A genuinely illegal case is surfaced as an error.
		return inst, &IllegalTransitionError{InstanceID: inst.ID, From: inst.State, To: to}
	}
	return r.store.Transition(ctx, inst.ID, TransitionParams{To: to, Reason: reason})
}

// itoa is a tiny int→string without importing strconv at call sites.
func itoa(n int) string { return fmt.Sprintf("%d", n) }
