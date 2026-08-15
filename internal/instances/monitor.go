package instances

import (
	"context"
	"fmt"
	"time"

	"github.com/localhub/localhub/internal/instances/proc"
)

// Monitor is the POST-HEALTHY process/health reconciler (M5.1/M5.2). Reconcile (reconcile.go) runs
// once at daemon STARTUP; this runs on a timer for the whole life of the daemon, so an engine that
// dies AFTER reaching HEALTHY stops being reported as healthy without a LocalHub restart.
//
// It is deliberately CONSERVATIVE about killing, because a false positive kills a model the user is
// relying on:
//
//	PID gone                      → FAILED, port freed. Unconditional, and risk-free: there is no
//	                                process left to kill, and a vanished engine is a FAILURE, not a
//	                                clean STOPPED (a requested stop goes through Stop, which marks
//	                                DRAINING first — see the skip rules below).
//	PID alive, health probe fails → record the consecutive-failure count; do NOT change state.
//	                                Only after ReapAfterFailures CONSECUTIVE failures is the tree
//	                                killed and the row marked FAILED.
//	PID alive, health OK          → stamp last_health_at, reset the failure counter.
//
// NEVER touched, by design:
//   - Non-HEALTHY rows. A STARTING instance is the start path's business (a 13.7 GB IQ4_XS
//     legitimately takes ~43 s, and far longer on a cold cache), and DRAINING means a stop is
//     already in flight — reaping it would race the stop path and could report a kill for a clean
//     shutdown.
//   - PINNED instances are never reaped. Pinning means "this must stay up"; a probe blip must not
//     override that. They ARE still probed, so a pinned model's staleness stays visible.
//   - Instances with IN-FLIGHT requests (the never-kill-active rule, CLAUDE.md): a busy
//     llama-server can be slow to answer /health precisely because it is mid-generation. Killing it
//     would destroy exactly the work that rule exists to protect.
//   - Ollama rows (PID 0): a shared daemon LocalHub does not own; its residency is mirrored from
//     /api/ps (P4.3).
type Monitor struct {
	store  *Store
	pool   *PortPool
	health HealthChecker
	alive  AliveChecker
	kill   TreeKiller
	// activeCount reports in-flight requests for an instance (residency's InFlightTracker in
	// production). nil => treat every instance as idle, which is the RISKIER default, so the
	// production wiring must supply it.
	activeCount func(instanceID string) int
	cfg         MonitorConfig
	now         func() time.Time
}

// MonitorConfig tunes the monitor.
type MonitorConfig struct {
	// ReapAfterFailures is the number of CONSECUTIVE failed health probes after which a
	// still-running-but-unresponsive instance is killed and marked FAILED. 0 DISABLES reaping
	// (failures are still recorded and surfaced, but no process is ever killed on a health signal).
	// The PID-gone → FAILED path is unaffected either way.
	//
	// The default is 3 rather than 1 on purpose: llama-server's /health can transiently fail under
	// load or during a long prompt-eval, and a single missed probe is not evidence of death.
	ReapAfterFailures int
}

// DefaultReapAfterFailures is the shipped threshold: three consecutive failures. At the default
// 15 s monitor interval that is ~45 s of continuous unresponsiveness before a kill.
const DefaultReapAfterFailures = 3

// NewMonitor builds a post-HEALTHY monitor. A nil activeCount treats everything as idle.
func NewMonitor(store *Store, pool *PortPool, health HealthChecker, activeCount func(string) int, cfg MonitorConfig) *Monitor {
	return &Monitor{
		store: store, pool: pool, health: health,
		alive: proc.IsAlive, kill: proc.KillTree,
		activeCount: activeCount, cfg: cfg, now: time.Now,
	}
}

// MonitorResult summarizes one pass, for logging and the M5 gate evidence.
type MonitorResult struct {
	Checked  []string `json:"checked"`  // HEALTHY instances probed this pass
	Died     []string `json:"died"`     // PID gone → FAILED
	Reaped   []string `json:"reaped"`   // unresponsive past the threshold → killed + FAILED
	Degraded []string `json:"degraded"` // probe failed but below the threshold (state unchanged)
	Skipped  []string `json:"skipped"`  // pinned or busy, so never reaped
	Errors   []string `json:"errors"`   // non-fatal per-instance errors
}

// Check runs ONE monitoring pass. A per-instance problem lands in Result.Errors rather than being
// returned; only a failure to list instances at all is returned, since that is a daemon-level fault.
func (m *Monitor) Check(ctx context.Context) (MonitorResult, error) {
	var res MonitorResult
	active, err := m.store.List(ctx, true)
	if err != nil {
		return res, fmt.Errorf("list active instances for monitor: %w", err)
	}
	for _, inst := range active {
		// Only HEALTHY, LocalHub-owned processes. STARTING is the start path's business, DRAINING
		// means a stop is already in flight, and PID 0 is the shared Ollama daemon.
		if inst.State != StateHealthy || inst.PID == 0 || inst.Engine == EngineOllama {
			continue
		}
		res.Checked = append(res.Checked, inst.ID)
		m.checkOne(ctx, inst, &res)
	}
	return res, nil
}

func (m *Monitor) checkOne(ctx context.Context, inst Instance, res *MonitorResult) {
	// (1) PID gone → the engine exited after reaching HEALTHY. This is a FAILURE, not a clean stop:
	// an intentional stop goes through Runner.Stop, which transitions to DRAINING first and then to
	// STOPPED with a measured NVML reclaim. Reporting this as STOPPED would claim a graceful
	// shutdown that never happened AND imply a verified VRAM reclaim we never measured.
	if !m.alive(inst.PID) {
		reason := fmt.Sprintf("process exited AFTER reaching HEALTHY: PID %d is gone (detected by the periodic monitor%s). "+
			"This was not a requested stop — a requested stop passes through DRAINING — so VRAM reclaim was NOT verified by an NVML delta.",
			inst.PID, lastSeenSuffix(inst.LastHealthAt))
		if _, err := m.transition(ctx, inst, StateFailed, reason, boolPtr(false)); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: mark failed after exit: %v", inst.ID, err))
			return
		}
		_ = m.pool.Free(ctx, inst.Port)
		res.Died = append(res.Died, inst.ID)
		return
	}

	// (2) PID alive → probe health + identity.
	healthy, identityOK, herr := m.health.Check(ctx, inst)
	if herr == nil && healthy && identityOK {
		if err := m.store.RecordHealthOK(ctx, inst.ID, m.now()); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: record health ok: %v", inst.ID, err))
		}
		return
	}

	// (3) Alive but unhealthy / serving the wrong model. Count it; escalate only past the threshold.
	failures, err := m.store.RecordHealthFailure(ctx, inst.ID)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("%s: record health failure: %v", inst.ID, err))
		return
	}
	detail := healthFailureDetail(healthy, identityOK, herr)

	// Protections that outrank the threshold. Checked AFTER recording the failure so the count and
	// the symptom still surface — a protected instance is reported, just never killed.
	if inst.Pinned {
		res.Skipped = append(res.Skipped, inst.ID)
		return
	}
	if m.activeCount != nil && m.activeCount(inst.ID) > 0 {
		// never-kill-active (CLAUDE.md): a busy engine may be slow to answer /health precisely
		// BECAUSE it is generating. Killing it would destroy the work the rule protects.
		res.Skipped = append(res.Skipped, inst.ID)
		return
	}
	if m.cfg.ReapAfterFailures <= 0 || failures < m.cfg.ReapAfterFailures {
		res.Degraded = append(res.Degraded, inst.ID)
		return
	}

	// (4) Reap: kill the tree, mark FAILED with the count and the observed symptom.
	_ = m.kill(inst.PID)
	reason := fmt.Sprintf("health failed %d consecutive times (threshold %d) while PID %d was still alive: %s%s. "+
		"Process tree killed and marked FAILED by the periodic monitor; VRAM reclaim was NOT verified by an NVML delta.",
		failures, m.cfg.ReapAfterFailures, inst.PID, detail, lastSeenSuffix(inst.LastHealthAt))
	if _, err := m.transition(ctx, inst, StateFailed, reason, boolPtr(false)); err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("%s: mark failed after reap: %v", inst.ID, err))
		return
	}
	_ = m.pool.Free(ctx, inst.Port)
	res.Reaped = append(res.Reaped, inst.ID)
}

// transition applies a state change, tolerating a row that went terminal concurrently (a stop that
// landed between our list and our write) rather than reporting a spurious error.
func (m *Monitor) transition(ctx context.Context, inst Instance, to State, reason string, reclaimVerified *bool) (Instance, error) {
	if !CanTransition(inst.State, to) {
		if inst.State.terminal() {
			return inst, nil
		}
		return inst, &IllegalTransitionError{InstanceID: inst.ID, From: inst.State, To: to}
	}
	return m.store.Transition(ctx, inst.ID, TransitionParams{
		To: to, Reason: reason, ReclaimVerified: reclaimVerified,
	})
}

// healthFailureDetail describes WHY a probe failed, in the same vocabulary the startup reconciler
// uses so the two are comparable in logs.
func healthFailureDetail(healthy, identityOK bool, herr error) string {
	switch {
	case herr != nil:
		return "health probe error: " + herr.Error()
	case !healthy:
		return "/health did not answer OK"
	case !identityOK:
		return "PID alive but serving a different/unknown model (reused-port collision)"
	default:
		return "unknown health failure"
	}
}

// lastSeenSuffix renders ", last healthy at <t>" when a successful probe was ever recorded. It
// answers the question an operator actually asks on seeing a FAILED row: how long was it broken?
func lastSeenSuffix(lastHealth time.Time) string {
	if lastHealth.IsZero() {
		return ""
	}
	return ", last healthy at " + lastHealth.UTC().Format(timeLayout) + " UTC"
}

func boolPtr(b bool) *bool { return &b }
