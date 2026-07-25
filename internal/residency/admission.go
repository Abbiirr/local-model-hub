package residency

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/localhub/localhub/internal/instances"
)

// admission.go is P5.1: admission control. It implements the admit() decision flow
// (residency-scheduler.md §admit, spec-digest §6) fused with the lmwrapper allocator enum
// (route_loaded | load_new | evict_then_load | queue | reject):
//
//  1. warm?                → RouteLoaded (+ coalescing credit)
//  2. footprint estimate   → observed peak preferred, else estimate + SAFETY_MARGIN_MIB (additive)
//  3. read free VRAM       → LIVE from the probe, EVERY decision
//  4. fits?                → LoadNew
//  5. doesn't fit          → simulate eviction over victims (P5.2 order); if the eligible idle set
//                            frees enough → EvictThenLoad, executing evictions ONE AT A TIME and
//                            RE-READING free VRAM from the probe after EACH one, re-checking
//                            sufficiency before the next eviction and before the final start.
//  6. else                 → Queue (blocked_reason naming the numbers) or Reject if queue capped.
//
// THE anti-OOM property lives in step 5: the per-eviction probe re-read means a mid-sequence VRAM
// surprise (another process grabbed VRAM between evictions) is caught — the decision degrades to
// Queue, NOT a blind start. This is tested explicitly (TESTING.md#p51 + the mid-sequence surprise
// test).
//
// This file owns the DECISION and the safe execution loop. It does not spawn the model itself —
// the caller (core.StartInstance) performs the actual engine.start() after Admit returns LoadNew
// or EvictThenLoad, because the start path (descriptor resolution, launch-plan, calibration) lives
// in core. Admit executes only the EVICTIONS (through the injected Evictor), which must complete
// before the caller starts.

// SafetyMarginMiB is the ADDITIVE residency admission margin (PLAN.md resolution 9): admission
// requires free >= footprint + SafetyMarginMiB before it will load. This is DISTINCT from the
// compat estimator's multiplicative safety_factor (0.92, applied inside the estimate) and from
// the dev-test "free − 1.5 GB" cap (testing policy). Default 1024 MiB; overridable via config
// (residency.safety_margin_mb).
const SafetyMarginMiB = 1024

// Decision is the admission outcome (lmwrapper allocator enum).
type Decision string

const (
	// DecisionRouteLoaded: the model is already HEALTHY-resident; route to it (no load).
	DecisionRouteLoaded Decision = "route_loaded"
	// DecisionLoadNew: free VRAM already covers footprint + margin; load directly (no eviction).
	DecisionLoadNew Decision = "load_new"
	// DecisionEvictThenLoad: evictions were executed (idle victims stopped, VRAM re-read after
	// each) and now free VRAM covers footprint + margin; the caller should load.
	DecisionEvictThenLoad Decision = "evict_then_load"
	// DecisionQueue: cannot fit now and cannot free enough from idle instances; the request was
	// enqueued (blocked_reason names the numbers). The caller returns 202 + queue position.
	DecisionQueue Decision = "queue"
	// DecisionReject: cannot fit AND the queue for this class is at capacity; rejected with a
	// numeric reason. Nothing was enqueued.
	DecisionReject Decision = "reject"
)

// AdmitResult is the full, auditable outcome of an Admit call. Every field that a user or the
// UI sees carries the numbers behind the decision (the mandatory-reason gate).
type AdmitResult struct {
	Decision Decision
	Reason   string // MANDATORY numeric human-readable why

	// WarmInstance is set for RouteLoaded: the existing HEALTHY instance to route to.
	WarmInstance *instances.Instance

	// FootprintMiB is the estimate used (observed peak preferred, else estimator).
	FootprintMiB int
	// NeedMiB is FootprintMiB + SafetyMarginMiB (what free VRAM had to cover).
	NeedMiB int
	// FreeBeforeMiB / FreeAfterMiB are the live probe reads before the decision and after any
	// evictions (proving the re-read happened; asserted in tests via probe call counts).
	FreeBeforeMiB int
	FreeAfterMiB  int

	// Evicted lists the instance ids actually stopped (in order) for EvictThenLoad.
	Evicted []string
	// ReclaimableMiB is the predicted reclaim from idle victims (from the simulation).
	ReclaimableMiB int
	// IdleVictimCount is the number of eligible idle victims considered.
	IdleVictimCount int

	// QueueID is set for Queue: the id of the enqueued request (echoed to the API as the queue
	// row id / position handle).
	QueueID string
	// QueuePosition is the 1-based position of the queued request in score order (for the 202).
	QueuePosition int
}

// FreeVRAMProbe is the live free-VRAM seam (GPU 0), satisfied by the same adapter llama.cpp/vLLM
// use (main.go ollamaFreeVRAMProbe / llamacpp.FreeVRAMFromProbe). Admission calls this EVERY
// decision and after EVERY eviction — never a cached counter (the anti-OOM re-read rule).
type FreeVRAMProbe interface {
	FreeVRAMMiB(ctx context.Context) (int, error)
}

// InstanceLister returns the current active (non-terminal) instances — the eviction candidate
// set and the warm-hit check. Satisfied by a thin adapter over *instances.Store (List(ctx,true)).
type InstanceLister interface {
	ActiveInstances(ctx context.Context) ([]instances.Instance, error)
}

// Evictor stops one instance through the owning engine's Phase-4 adapter (the VRAM-delta-verified
// graceful stop). Admission calls it ONE AT A TIME. It returns the measured freed MiB (for the
// log) and an error if the stop failed. The adapter itself marks DRAINING→STOPPED and verifies
// the reclaim; admission additionally re-reads the probe after each call.
type Evictor interface {
	Evict(ctx context.Context, inst instances.Instance) (freedMiB int, err error)
}

// FootprintEstimator returns the VRAM footprint (MiB) admission should budget for loading a model
// on an engine, using the PER-ENGINE footprint model (the P4.5 lesson): llamacpp/ollama =
// estimated or observed VRAM; vllm = gpu_memory_utilization × total VRAM. Satisfied by an adapter
// over the compat engine + calibration store + config (see Footprint below and core wiring).
type FootprintEstimator interface {
	// Footprint returns (mib, warm, err). warm hints the queue's size/cost tie-break. A non-nil
	// err means the estimate could not be produced (admission then rejects with the reason —
	// never guesses a number that could OOM).
	Footprint(ctx context.Context, spec AdmitSpec) (mib int, err error)
}

// AdmitSpec is the input to Admit: what to load, at what class, plus the per-engine footprint
// inputs the estimator needs.
type AdmitSpec struct {
	ModelID string
	Engine  string
	Ctx     int
	Batch   int
	Class   PriorityClass
	Pin     bool
	// Priority is the raw priority int the caller supplied (0/1/2 → class). Class is derived from
	// it, but the raw value is preserved so a queued request re-started by the drain loop keeps the
	// exact priority the user asked for.
	Priority int

	// FootprintMiB, if > 0, is a pre-computed footprint the caller already has (e.g. from the
	// compat verdict it computed for the start path). Admission uses it verbatim and skips the
	// estimator — avoiding a duplicate estimate. 0 → ask the FootprintEstimator.
	FootprintMiB int
}

// Manager is the residency admission controller (P5.1) + queue owner (P5.0). It serializes all
// admission decisions under one lock (a single-box daemon has low instance churn; a coarse lock
// makes "read free VRAM → decide → evict → re-read → decide" atomic against concurrent admits,
// which is exactly what prevents two admits from both loading into the same freed VRAM).
type Manager struct {
	mu sync.Mutex

	queue     *Queue
	qstore    *QueueStore
	probe     FreeVRAMProbe
	lister    InstanceLister
	evictor   Evictor
	estimator FootprintEstimator
	tracker   ActiveTracker
	clock     Clock

	safetyMarginMiB int
	recentActiveTTL time.Duration
	defaultTTLSecs  int

	// vramBudgetMiB is the optional ops/testing VRAM ceiling (config residency.vram_budget_mib;
	// 0 = disabled). When > 0, admission's supply is min(probe_free, budget − our_footprints) so
	// the daemon never fills more than the budget with our instances even if the card has more
	// free. See supplyMiB.
	vramBudgetMiB int

	// logger is the admission-observability seam (fix 2). nil = no logging.
	logger AdmissionLogger

	// drain holds the production queue-drain loop's bookkeeping (fix 1). See drain.go.
	drain drainState
}

// ManagerConfig configures a Manager.
type ManagerConfig struct {
	Queue           *Queue      // nil → a Queue with default config + the manager's clock
	QueueStore      *QueueStore // nil → no durable mirror (queue lives in memory only)
	Probe           FreeVRAMProbe
	Lister          InstanceLister
	Evictor         Evictor
	Estimator       FootprintEstimator
	Tracker         ActiveTracker
	Clock           Clock // nil → RealClock
	SafetyMarginMiB int   // 0 → SafetyMarginMiB default
	RecentActiveTTL time.Duration
	DefaultTTLSecs  int
	// VRAMBudgetMiB caps the VRAM admission may treat as available for OUR instances (0 =
	// disabled → use real free VRAM). Purpose: ops VRAM reservation AND storm testing.
	VRAMBudgetMiB int
	// DrainInterval is the periodic re-admit cadence (config residency.drain_interval_seconds;
	// 0 → DefaultDrainIntervalSecs). The drain loop also fires on stop/evict/TTL events.
	DrainInterval time.Duration
	// Logger is the admission-observability seam (fix 2). nil = no logging.
	Logger      AdmissionLogger
	QueueConfig QueueConfig // used only when Queue is nil
}

// NewManager builds a Manager. Probe, Lister, Evictor, and Estimator are required for a live
// admission decision (Admit returns an error if the essential seams are missing); the others have
// sensible defaults.
func NewManager(cfg ManagerConfig) *Manager {
	clock := cfg.Clock
	if clock == nil {
		clock = RealClock()
	}
	q := cfg.Queue
	if q == nil {
		q = NewQueue(cfg.QueueConfig, clock)
	}
	margin := cfg.SafetyMarginMiB
	if margin <= 0 {
		margin = SafetyMarginMiB
	}
	recent := cfg.RecentActiveTTL
	if recent <= 0 {
		recent = DefaultRecentActiveTTLSecs * time.Second
	}
	return &Manager{
		queue:           q,
		qstore:          cfg.QueueStore,
		probe:           cfg.Probe,
		lister:          cfg.Lister,
		evictor:         cfg.Evictor,
		estimator:       cfg.Estimator,
		tracker:         cfg.Tracker,
		clock:           clock,
		safetyMarginMiB: margin,
		recentActiveTTL: recent,
		defaultTTLSecs:  cfg.DefaultTTLSecs,
		vramBudgetMiB:   cfg.VRAMBudgetMiB,
		logger:          cfg.Logger,
		drain: drainState{
			drainInterval: cfg.DrainInterval,
			startingIDs:   make(map[string]bool),
		},
	}
}

// SetSeams injects the estimator + evictor after construction. This resolves the wiring
// chicken-and-egg: the evictor and estimator live on the service core (they need the compat
// engine + engine runners), but the service core needs the manager in its Deps. main builds the
// manager with the store-backed lister + probe, then core.New calls SetSeams with the core-backed
// estimator/evictor. Passing nil leaves the existing seam unchanged. Not safe to call concurrently
// with Admit (called once at wiring time).
func (m *Manager) SetSeams(estimator FootprintEstimator, evictor Evictor) {
	if estimator != nil {
		m.estimator = estimator
	}
	if evictor != nil {
		m.evictor = evictor
	}
}

// Admit runs the admission decision flow for one request and returns the outcome. It is safe for
// concurrent callers (serialized internally). It EXECUTES evictions for EvictThenLoad (through the
// Evictor, one at a time, re-reading VRAM after each) but does NOT start the model — the caller
// starts after a RouteLoaded/LoadNew/EvictThenLoad result.
func (m *Manager) Admit(ctx context.Context, spec AdmitSpec) (AdmitResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.admitLocked(ctx, spec)
}

func (m *Manager) admitLocked(ctx context.Context, spec AdmitSpec) (AdmitResult, error) {
	if m.probe == nil || m.lister == nil {
		return AdmitResult{}, fmt.Errorf("residency admit: probe and instance lister are required")
	}
	if !spec.Class.Valid() {
		spec.Class = ClassNormal
	}

	// --- (1) warm? → route_loaded ---
	active, err := m.lister.ActiveInstances(ctx)
	if err != nil {
		return AdmitResult{}, fmt.Errorf("residency admit: list active instances: %w", err)
	}
	if warm := warmInstance(active, spec.ModelID, spec.Engine); warm != nil {
		w := *warm
		return AdmitResult{
			Decision:     DecisionRouteLoaded,
			WarmInstance: &w,
			Reason: fmt.Sprintf("route_loaded: model %q is already HEALTHY-resident on %s (instance %s) — no load needed",
				spec.ModelID, w.Engine, w.ID),
		}, nil
	}

	// --- (2) footprint estimate (observed peak preferred; else estimator + additive margin) ---
	footprint := spec.FootprintMiB
	if footprint <= 0 {
		if m.estimator == nil {
			return AdmitResult{}, fmt.Errorf("residency admit: no footprint estimator configured and no precomputed footprint for %q", spec.ModelID)
		}
		f, ferr := m.estimator.Footprint(ctx, spec)
		if ferr != nil {
			// A model whose footprint we cannot estimate is REJECTED, never guessed (a guessed
			// number that OOMs is the exact correctness bug this product prevents).
			res := AdmitResult{
				Decision: DecisionReject,
				Reason:   fmt.Sprintf("reject: cannot estimate VRAM footprint for %q on %s (%v) — refusing to guess a number that could OOM", spec.ModelID, spec.Engine, ferr),
			}
			m.logDecision(spec, res, nil)
			return res, nil
		}
		footprint = f
	}
	need := footprint + m.safetyMarginMiB

	// --- (3) read free VRAM — LIVE — and apply the optional VRAM budget (fix 5). ---
	probeFree, err := m.probe.FreeVRAMMiB(ctx)
	if err != nil {
		return AdmitResult{}, fmt.Errorf("residency admit: read free VRAM: %w", err)
	}
	// supply is what admission may actually treat as available: min(probe_free, budget − our
	// resident footprints) when a budget is set, else the raw probe free. This is the anti-OOM
	// term AND the storm-test reservation lever.
	free := m.supplyMiB(probeFree, active)

	res := AdmitResult{FootprintMiB: footprint, NeedMiB: need, FreeBeforeMiB: free, FreeAfterMiB: free}

	// --- (4) fits directly? → load_new ---
	if free >= need {
		res.Decision = DecisionLoadNew
		res.Reason = fmt.Sprintf("load_new: available %d MiB >= footprint %d + %d MiB margin (%d MiB) — loading directly, no eviction%s",
			free, footprint, m.safetyMarginMiB, need, m.budgetNote(probeFree, free))
		m.logDecision(spec, res, nil)
		return res, nil
	}

	// --- (5) doesn't fit → simulate eviction over idle victims ---
	pol := EvictionPolicy{Now: m.clock.Now(), RecentActiveTTL: m.recentActiveTTL}
	ordered, _ := PickVictims(active, m.tracker, pol, spec.ModelID)
	res.IdleVictimCount = len(ordered)

	deficit := need - free
	enough, plan, reclaimable := SimulateReclaim(ordered, deficit)
	res.ReclaimableMiB = reclaimable

	if !enough {
		// Cannot free enough from idle instances → queue (or reject if capped).
		res, err := m.blockOrReject(ctx, spec, res, deficit, reclaimable, len(ordered))
		if err == nil {
			m.logDecision(spec, res, nil)
		}
		return res, err
	}

	// EvictThenLoad: execute the plan ONE AT A TIME, re-reading free VRAM after EACH eviction and
	// re-checking sufficiency before the next eviction and before the final start. This loop is
	// THE anti-OOM property: a mid-sequence VRAM surprise (another process grabs VRAM) is caught
	// by the re-read, and the decision degrades to Queue rather than a blind start.
	//
	// evLogs accumulates the per-eviction re-read detail (fix 2 observability): one entry per
	// eviction naming the victim, the expected reclaim, and the free-VRAM value RE-READ from the
	// probe after the stop. `evicted` shrinks the resident set so the budget supply is recomputed
	// with the victim's footprint no longer counted against the budget.
	var evLogs []EvictionLog
	evicted := make(map[string]bool)
	for _, victim := range plan {
		// Re-check: maybe an earlier eviction already freed enough (or a concurrent free did).
		if free >= need {
			break
		}
		if m.evictor == nil {
			return AdmitResult{}, fmt.Errorf("residency admit: eviction required but no evictor configured")
		}
		expected := victim.ReclaimMiB
		freed, everr := m.evictor.Evict(ctx, victim.Instance)
		if everr != nil {
			// A failed eviction is not fatal to the decision — re-read and continue; the loop's
			// sufficiency check below (post re-read) decides whether we can still proceed.
			_ = freed
		} else {
			res.Evicted = append(res.Evicted, victim.Instance.ID)
			evicted[victim.Instance.ID] = true
		}
		// RE-READ free VRAM from the probe (never trust bookkeeping / the adapter's own delta),
		// then re-apply the budget with the evicted instances no longer counted.
		pf, rerr := m.probe.FreeVRAMMiB(ctx)
		if rerr != nil {
			return AdmitResult{}, fmt.Errorf("residency admit: re-read free VRAM after evicting %s: %w", victim.Instance.ID, rerr)
		}
		probeFree = pf
		free = m.supplyMiB(probeFree, remaining(active, evicted))
		res.FreeAfterMiB = free
		evLog := EvictionLog{
			Victim:             victim.Instance.ID,
			FreedExpectedMiB:   expected,
			FreedMeasuredMiB:   freed,
			FreeAfterReReadMiB: free,
		}
		evLogs = append(evLogs, evLog)
		if m.logger != nil {
			m.logger.EvictionReRead(evLog)
		}
	}

	// Final sufficiency check AFTER the last probe re-read — the load gate.
	if free >= need {
		res.Decision = DecisionEvictThenLoad
		res.Reason = fmt.Sprintf("evict_then_load: evicted %d idle instance(s) %v; free VRAM re-read after each eviction is now %d MiB >= footprint %d + %d margin (%d MiB) — loading%s",
			len(res.Evicted), res.Evicted, free, footprint, m.safetyMarginMiB, need, m.budgetNote(probeFree, free))
		m.logDecision(spec, res, evLogs)
		return res, nil
	}

	// The re-read says we still don't fit (the anti-OOM catch): the prediction was optimistic —
	// evictions freed less than expected, or another process grabbed VRAM mid-sequence. DEGRADE
	// to Queue instead of a blind start. We requeue with the up-to-date shortfall.
	shortfall := need - free
	res, err = m.blockOrReject(ctx, spec, res, shortfall, free, len(ordered))
	if err != nil {
		return res, err
	}
	// Annotate the reason so the log/UI shows this was the anti-OOM degradation, not a plain block.
	res.Reason = fmt.Sprintf("queue (anti-OOM): after evicting %v the re-read of free VRAM is %d MiB, still %d MiB short of footprint %d + %d margin — the eviction prediction was optimistic (another process may have taken VRAM, or reclaim lagged). Refusing a blind start; %s",
		res.Evicted, free, shortfall, footprint, m.safetyMarginMiB, res.Reason)
	m.logDecision(spec, res, evLogs)
	return res, nil
}

// supplyMiB returns the VRAM admission may treat as available for a NEW instance: the raw probe
// free VRAM, capped by the optional budget. When a budget is set (vramBudgetMiB > 0), the supply
// is min(probeFree, budget − sum(our resident footprints)): the budget bounds how much VRAM OUR
// instances may occupy regardless of how much the card physically has free, so ops can reserve
// headroom (and storm tests can force eviction/queue pressure with a tight budget). Never negative.
func (m *Manager) supplyMiB(probeFree int, active []instances.Instance) int {
	if m.vramBudgetMiB <= 0 {
		return probeFree
	}
	used := 0
	for _, inst := range active {
		if inst.State.Terminal() {
			continue
		}
		used += instanceFootprint(inst)
	}
	budgetRemaining := m.vramBudgetMiB - used
	if budgetRemaining < 0 {
		budgetRemaining = 0
	}
	if budgetRemaining < probeFree {
		return budgetRemaining
	}
	return probeFree
}

// budgetNote annotates a load reason when a budget clamped the supply below the raw probe free
// (so the log shows WHY available < physically-free). Empty when no budget applies.
func (m *Manager) budgetNote(probeFree, supply int) string {
	if m.vramBudgetMiB <= 0 || supply >= probeFree {
		return ""
	}
	return fmt.Sprintf(" (vram_budget_mib=%d caps available to %d MiB below the %d MiB physically free)",
		m.vramBudgetMiB, supply, probeFree)
}

// instanceFootprint returns the VRAM an active instance is assumed to occupy against the budget:
// observed peak preferred, else the estimate.
func instanceFootprint(inst instances.Instance) int {
	if inst.VRAMObsMiB > 0 {
		return inst.VRAMObsMiB
	}
	return inst.VRAMEstMiB
}

// remaining returns the active instances not in the evicted set (for recomputing budget supply
// after each eviction — the evicted instance's footprint no longer counts against the budget).
func remaining(active []instances.Instance, evicted map[string]bool) []instances.Instance {
	if len(evicted) == 0 {
		return active
	}
	out := make([]instances.Instance, 0, len(active))
	for _, inst := range active {
		if evicted[inst.ID] {
			continue
		}
		out = append(out, inst)
	}
	return out
}

// blockOrReject enqueues the request (DecisionQueue) with a numeric blocked_reason, or rejects it
// (DecisionReject) when the class queue is at capacity. The reason always names the numbers:
// needed, free, reclaimable, and the number of idle instances.
func (m *Manager) blockOrReject(ctx context.Context, spec AdmitSpec, res AdmitResult, shortfallMiB, reclaimableMiB, idleCount int) (AdmitResult, error) {
	_ = ctx
	blockedReason := fmt.Sprintf(
		"waiting for VRAM: need %d MiB (footprint %d + %d margin), free %d MiB, reclaimable %d MiB from %d idle instance(s) — %d MiB short",
		res.NeedMiB, res.FootprintMiB, m.safetyMarginMiB, res.FreeAfterMiB, reclaimableMiB, idleCount, shortfallMiB)

	// Build the queue request; the size/cost hint uses the footprint so the queue's tie-break
	// agrees with what admission just decided. The FULL start context (ctx/batch/pin/priority) is
	// carried so the drain loop can re-Admit + start this request later without the HTTP caller.
	req := Request{
		ID:       newQueueID(),
		ModelID:  spec.ModelID,
		Engine:   spec.Engine,
		Class:    spec.Class,
		Cost:     CostHint{Warm: false, EstimateMiB: res.FootprintMiB},
		Ctx:      spec.Ctx,
		Batch:    spec.Batch,
		Pin:      spec.Pin,
		Priority: spec.Priority,
	}
	if err := m.queue.Enqueue(req); err != nil {
		// Capped → reject (nothing enqueued). The QueueFullError carries its own numeric reason.
		var qf *QueueFullError
		if asQueueFull(err, &qf) {
			res.Decision = DecisionReject
			res.Reason = fmt.Sprintf("%s (%s)", qf.Reason(), blockedReason)
			return res, nil
		}
		return res, fmt.Errorf("residency admit: enqueue: %w", err)
	}

	// Durable mirror: persist the WAITING row (best-effort; a DB error does not undo the
	// in-memory enqueue — the queue still works, only the UI/restart mirror lags).
	if err := m.qstore.InsertWaiting(ctx, req, blockedReason, res.FootprintMiB); err != nil {
		_ = err // logged by the caller if it cares; the in-memory queue is authoritative
	}

	res.Decision = DecisionQueue
	res.QueueID = req.ID
	res.Reason = blockedReason
	// Position in score order (1-based) for the 202 response.
	res.QueuePosition = m.queuePositionLocked(req.ID)
	return res, nil
}

// queuePositionLocked returns the 1-based score-order position of the request id in the queue
// (1 = would be admitted next). Assumes the manager lock is held.
func (m *Manager) queuePositionLocked(id string) int {
	snap := m.queue.Snapshot(NextInput{})
	for i, r := range snap {
		if r.ID == id {
			return i + 1
		}
	}
	return 0
}

// Queue exposes the underlying queue for read-only snapshotting (GET /api/queue) and for the
// drain loop. Callers must not mutate it outside the manager's lock.
func (m *Manager) Queue() *Queue { return m.queue }

// QueueRows returns the current queue entries for GET /api/queue: the in-memory WAITING requests
// (with live score-order positions) merged over the durable rows (which also carry the
// blocked_reason and recently-finished entries). The in-memory queue is authoritative for
// ordering; the store supplies blocked_reason and history.
func (m *Manager) QueueRows(ctx context.Context, in NextInput) ([]QueueRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Score-ordered in-memory snapshot → position map.
	snap := m.queue.Snapshot(in)
	pos := make(map[string]int, len(snap))
	for i, r := range snap {
		pos[r.ID] = i + 1
	}

	// Durable rows (blocked_reason + finished history).
	rows, err := m.qstore.List(ctx, 100)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]int, len(rows))
	for i := range rows {
		byID[rows[i].ID] = i
		if p, ok := pos[rows[i].ID]; ok {
			rows[i].Position = p
		}
	}

	// Any in-memory request without a durable row (store unconfigured / lag) is added so the API
	// still reflects the live queue.
	for _, r := range snap {
		if _, ok := byID[r.ID]; ok {
			continue
		}
		rows = append(rows, QueueRow{
			ID: r.ID, ModelID: r.ModelID, Engine: r.Engine, PriorityClass: int(r.Class),
			Status: QueueWaiting, FootprintMiB: r.Cost.EstimateMiB, Seq: r.Seq(),
			Position: pos[r.ID], EnqueuedAt: r.EnqueuedAt(),
		})
	}
	return rows, nil
}

// Cancel removes a WAITING request from the queue (DELETE /api/queue/{id}) and marks its durable
// row DONE. Returns true if it was present in the in-memory queue.
func (m *Manager) Cancel(ctx context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := m.queue.Remove(id)
	err := m.qstore.SetStatus(ctx, id, QueueDone, "cancelled by user")
	return removed, err
}

// MarkAdmitted marks a queued request's durable row ADMITTED (the drain loop calls this when a
// WAITING request is successfully admitted+started). The in-memory removal happens in the drain
// loop via Queue().Next.
func (m *Manager) MarkAdmitted(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.qstore.SetStatus(ctx, id, QueueAdmitted, "")
}

// warmInstance returns a HEALTHY instance already serving modelID on engine (or any engine when
// engine==""), or nil. This is the route_loaded check.
func warmInstance(active []instances.Instance, modelID, engine string) *instances.Instance {
	for i := range active {
		inst := active[i]
		if inst.ModelID != modelID || inst.State != instances.StateHealthy {
			continue
		}
		if engine != "" && string(inst.Engine) != engine {
			continue
		}
		return &active[i]
	}
	return nil
}
