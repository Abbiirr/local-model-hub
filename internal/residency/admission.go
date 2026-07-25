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

// FreeRAMProbe is the live free-SYSTEM-RAM seam (fix 4: RAM headroom as an admission constraint).
// On a 16 GB-RAM box RAM binds before VRAM (P5 load-storm evidence: four llama-server processes
// drove free RAM to 0.24–0.54 GB while VRAM never OOMed). Admission probes it FRESH at decision
// time (like the VRAM re-read) and refuses to start a NEW process/warm when free RAM is below the
// configured floor. nil disables the check (RAM floor unset → VRAM-only admission, prior behavior).
//
// This is a GLOBAL GUARD, not a per-model RAM estimate and NOT RAM-based victim selection (out of
// scope — VRAM eviction policy is unchanged). A RAM-blocked request queues through the same
// blockOrReject path as a VRAM shortage; the drain loop re-admits it once RAM recovers (e.g. a
// VRAM-eviction that stops a llama-server also frees that process's RAM).
type FreeRAMProbe interface {
	FreeRAMMiB(ctx context.Context) (int, error)
}

// GPUStateProbe is the live GPU thermal/utilization seam for the two GPU admission ceilings
// (max_gpu_temp_c, max_gpu_util_pct). It returns the current GPU-0 core temperature (Celsius) and
// utilization (percent, 0..100); a value < 0 means "unavailable" (the guard treats an unavailable
// reading as not-triggering, since we can't prove the ceiling is exceeded — fail-open rather than
// wedge admission on a probe gap). Admission reads it FRESH at each decision (like the VRAM re-read
// and the RAM floor); it is a SINGLE combined snapshot seam so one probe read serves both ceilings
// (the hardware Snapshot already carries both fields on the same GPU). nil disables both ceilings.
//
// Like the RAM floor these are GLOBAL admission guards, not per-model estimates and NOT eviction
// victim selection: they only gate STARTING/WARMING a NEW model. A warm route (already-loaded model)
// is exempt (no new load), eviction/stop paths are never affected (never-kill-active), and a blocked
// request queues on the same queue/drain path as a VRAM/RAM shortage — the drain loop re-admits it
// once the card cools / frees up.
type GPUStateProbe interface {
	GPUState(ctx context.Context) (tempC, utilPct int, err error)
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
	ramProbe  FreeRAMProbe
	gpuProbe  GPUStateProbe
	lister    InstanceLister
	evictor   Evictor
	estimator FootprintEstimator
	tracker   ActiveTracker
	clock     Clock

	safetyMarginMiB int
	recentActiveTTL time.Duration
	defaultTTLSecs  int

	// minFreeRAMMiB is the system-RAM floor (fix 4). Admission refuses to start a NEW process/warm
	// when the FRESH free-RAM probe reads below this; 0 disables the check. Requires ramProbe != nil.
	minFreeRAMMiB int

	// maxGPUTempC / maxGPUUtilPct are the GPU thermal / utilization admission ceilings. When set
	// (>0) admission reads the FRESH GPU state and refuses to start/warm a NEW model while the core
	// temperature (resp. utilization) is above the ceiling, queueing with a numeric gpu_hot /
	// gpu_busy reason; the drain loop re-admits once the card cools / frees. 0 disables each check.
	// Both require gpuProbe != nil. Admission-time guards only — never kill a running model.
	maxGPUTempC   int
	maxGPUUtilPct int

	// vramBudgetMiB is the optional ops/testing VRAM ceiling (config residency.vram_budget_mib;
	// 0 = disabled). When > 0, admission's supply is min(probe_free, budget − our_footprints) so
	// the daemon never fills more than the budget with our instances even if the card has more
	// free. See supplyMiB.
	vramBudgetMiB int

	// logger is the admission-observability seam (fix 2). nil = no logging.
	logger AdmissionLogger
	// reservations account for starts accepted but not yet visible to the lister.
	reservations map[string]int

	// drain holds the production queue-drain loop's bookkeeping (fix 1). See drain.go.
	drain drainState
}

// ManagerConfig configures a Manager.
type ManagerConfig struct {
	Queue           *Queue      // nil → a Queue with default config + the manager's clock
	QueueStore      *QueueStore // nil → no durable mirror (queue lives in memory only)
	Probe           FreeVRAMProbe
	RAMProbe        FreeRAMProbe  // nil → RAM floor disabled (VRAM-only admission)
	GPUProbe        GPUStateProbe // nil → GPU temp/util ceilings disabled
	Lister          InstanceLister
	Evictor         Evictor
	Estimator       FootprintEstimator
	Tracker         ActiveTracker
	Clock           Clock // nil → RealClock
	SafetyMarginMiB int   // 0 → SafetyMarginMiB default
	RecentActiveTTL time.Duration
	DefaultTTLSecs  int
	// MinFreeRAMMiB is the system-RAM floor (fix 4, config residency.min_free_ram_mib). 0 disables
	// the check. Effective only when RAMProbe is also set.
	MinFreeRAMMiB int
	// MaxGPUTempC / MaxGPUUtilPct are the GPU thermal / utilization admission ceilings (config
	// residency.max_gpu_temp_c / max_gpu_util_pct). 0 disables each check. Effective only when
	// GPUProbe is also set.
	MaxGPUTempC   int
	MaxGPUUtilPct int
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
		reservations:    make(map[string]int),
		qstore:          cfg.QueueStore,
		probe:           cfg.Probe,
		ramProbe:        cfg.RAMProbe,
		gpuProbe:        cfg.GPUProbe,
		lister:          cfg.Lister,
		evictor:         cfg.Evictor,
		estimator:       cfg.Estimator,
		tracker:         cfg.Tracker,
		clock:           clock,
		safetyMarginMiB: margin,
		recentActiveTTL: recent,
		defaultTTLSecs:  cfg.DefaultTTLSecs,
		minFreeRAMMiB:   cfg.MinFreeRAMMiB,
		maxGPUTempC:     cfg.MaxGPUTempC,
		maxGPUUtilPct:   cfg.MaxGPUUtilPct,
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

	// --- (1b) RAM floor (fix 4) — a GLOBAL guard checked for EVERY new process/warm across all
	// engines (llama.cpp spawn, Ollama warm, vLLM docker). RAM binds before VRAM on a small-RAM box
	// (P5 evidence), so a start that would fit in VRAM can still be operationally unsafe under RAM
	// pressure. Probe FRESH (like the VRAM re-read); when free system RAM is below the floor, QUEUE
	// (or reject if capped) with a numeric ram_blocked reason. Eviction MAY free RAM (stopping a
	// llama-server frees its process RAM), so the queue+drain path is correct — but we do NOT do
	// RAM-based victim selection here (out of scope; VRAM eviction policy unchanged). A warm route
	// above needs no new process, so it is exempt.
	if m.minFreeRAMMiB > 0 && m.ramProbe != nil {
		freeRAM, rerr := m.ramProbe.FreeRAMMiB(ctx)
		if rerr != nil {
			return AdmitResult{}, fmt.Errorf("residency admit: read free system RAM: %w", rerr)
		}
		if freeRAM < m.minFreeRAMMiB {
			ramReason := fmt.Sprintf(
				"ram_blocked: free system RAM %d MiB below floor %d MiB — refusing to start a new process/warm until RAM recovers (RAM binds before VRAM on this box; global guard, no RAM-based eviction)",
				freeRAM, m.minFreeRAMMiB)
			res := AdmitResult{}
			res, err = m.enqueueOrReject(ctx, spec, res, ramReason)
			if err == nil {
				m.logDecision(spec, res, nil)
			}
			return res, err
		}
	}

	// --- (1c) GPU thermal / utilization ceilings — GLOBAL guards checked for EVERY new start/warm
	// across all engines, mirroring the RAM floor. Read the GPU state ONCE (single combined probe)
	// and FRESH per decision. When the core temp is above max_gpu_temp_c, or utilization is above
	// max_gpu_util_pct, QUEUE (or reject if capped) with a numeric gpu_hot / gpu_busy reason on the
	// same queue/drain path — the drain loop re-admits once the card cools / frees. A warm route
	// (handled above) is exempt (no new load); eviction/stop paths are never affected (never-kill-
	// active). An unavailable reading (< 0) is treated as not-triggering (fail-open — we can't prove
	// the ceiling is exceeded, and wedging admission on a probe gap would be worse). Temp is checked
	// before util so a hot-AND-busy card reports the thermal reason first (the safety-critical one).
	if (m.maxGPUTempC > 0 || m.maxGPUUtilPct > 0) && m.gpuProbe != nil {
		tempC, utilPct, gerr := m.gpuProbe.GPUState(ctx)
		if gerr != nil {
			return AdmitResult{}, fmt.Errorf("residency admit: read GPU state: %w", gerr)
		}
		if m.maxGPUTempC > 0 && tempC > m.maxGPUTempC {
			hotReason := fmt.Sprintf(
				"gpu_hot: GPU temp %dC above ceiling %dC — deferring new load until it cools (drain re-checks); admission-time guard only, running models are never killed",
				tempC, m.maxGPUTempC)
			res := AdmitResult{}
			res, err = m.enqueueOrReject(ctx, spec, res, hotReason)
			if err == nil {
				m.logDecision(spec, res, nil)
			}
			return res, err
		}
		if m.maxGPUUtilPct > 0 && utilPct > m.maxGPUUtilPct {
			busyReason := fmt.Sprintf(
				"gpu_busy: GPU utilization %d%% above ceiling %d%% — deferring new load until it frees (drain re-checks); admission-time guard only, running models are never killed",
				utilPct, m.maxGPUUtilPct)
			res := AdmitResult{}
			res, err = m.enqueueOrReject(ctx, spec, res, busyReason)
			if err == nil {
				m.logDecision(spec, res, nil)
			}
			return res, err
		}
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
	// A matching model is already between admission and process registration. Do not
	// launch a duplicate process while the first start is still reserved.
	if m.reservations[reservationKey(spec.ModelID, spec.Engine)] > 0 {
		res := AdmitResult{FootprintMiB: footprint, NeedMiB: need}
		return m.enqueueOrReject(ctx, spec, res,
			"starting: an admitted instance of this model is still becoming visible; waiting instead of launching a duplicate")
	}

	// --- (3) read free VRAM — LIVE — and apply the optional VRAM budget (fix 5). ---
	probeFree, err := m.probe.FreeVRAMMiB(ctx)
	if err != nil {
		return AdmitResult{}, fmt.Errorf("residency admit: read free VRAM: %w", err)
	}
	// supply is what admission may actually treat as available: min(probe_free, budget − our
	// resident footprints) when a budget is set, else the raw probe free. This is the anti-OOM
	// term AND the storm-test reservation lever.
	free := m.supplyMiB(probeFree, active) - m.reservedMiBLocked()
	if free < 0 {
		free = 0
	}

	res := AdmitResult{FootprintMiB: footprint, NeedMiB: need, FreeBeforeMiB: free, FreeAfterMiB: free}

	// --- (4) fits directly? → load_new ---
	if free >= need {
		res.Decision = DecisionLoadNew
		m.reserveLocked(spec, footprint)
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
		// Never-kill-active TOCTOU guard: the victim was classified idle when PickVictims ran,
		// but BeginInstanceRequest increments the in-flight counter WITHOUT taking m.mu, so a
		// request may have routed warm to this instance since — a window that widens with every
		// multi-second eviction executed earlier in this loop. Re-check the live counter
		// immediately before the stop and SKIP a victim that became active; the final
		// sufficiency gate below degrades to Queue if the remaining victims can't free enough.
		if m.tracker != nil && m.tracker.ActiveCount(victim.Instance.ID) > 0 {
			continue
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
		m.reserveLocked(spec, footprint)
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
func reservationKey(modelID, engine string) string { return engine + "\x00" + modelID }

func (m *Manager) reserveLocked(spec AdmitSpec, footprint int) {
	if footprint > 0 {
		m.reservations[reservationKey(spec.ModelID, spec.Engine)] += footprint
	}
}

func (m *Manager) reservedMiBLocked() int {
	total := 0
	for _, n := range m.reservations {
		total += n
	}
	return total
}

// ReleaseReservation clears a pending-start reservation after the external engine start
// succeeds or fails. It is idempotent.
func (m *Manager) ReleaseReservation(modelID, engine string) {
	m.mu.Lock()
	delete(m.reservations, reservationKey(modelID, engine))
	m.mu.Unlock()
}

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
	blockedReason := fmt.Sprintf(
		"waiting for VRAM: need %d MiB (footprint %d + %d margin), free %d MiB, reclaimable %d MiB from %d idle instance(s) — %d MiB short",
		res.NeedMiB, res.FootprintMiB, m.safetyMarginMiB, res.FreeAfterMiB, reclaimableMiB, idleCount, shortfallMiB)
	return m.enqueueOrReject(ctx, spec, res, blockedReason)
}

// enqueueOrReject enqueues the request (DecisionQueue) with the supplied numeric blockedReason, or
// rejects it (DecisionReject) when the class queue is at capacity. It is the shared mechanism for
// BOTH the VRAM-shortage block (blockOrReject) and the RAM-floor block (fix 4) so a ram_blocked
// request rides the same queue/drain/persistence path as a VRAM-blocked one — the blocked_reason is
// surfaced identically in /api/queue, proxy 503s, and job reasons.
func (m *Manager) enqueueOrReject(ctx context.Context, spec AdmitSpec, res AdmitResult, blockedReason string) (AdmitResult, error) {
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
	stored, err := m.queue.Enqueue(req)
	if err != nil {
		// Capped → reject (nothing enqueued). The QueueFullError carries its own numeric reason.
		var qf *QueueFullError
		if asQueueFull(err, &qf) {
			res.Decision = DecisionReject
			res.Reason = fmt.Sprintf("%s (%s)", qf.Reason(), blockedReason)
			return res, nil
		}
		return res, fmt.Errorf("residency admit: enqueue: %w", err)
	}

	// Persist the WAITING row before reporting a queue acceptance. The durable queue is the restart
	// contract, so returning 202 after a failed insert would be a lie: the request would disappear
	// on daemon restart. Roll back the in-memory insertion and surface the persistence failure.
	// Persist `stored`, not the seq=0 original, so restart ordering remains FIFO-deterministic.
	if err := m.qstore.InsertWaiting(ctx, stored, blockedReason, res.FootprintMiB); err != nil {
		m.queue.Remove(stored.ID)
		return res, fmt.Errorf("residency admit: persist queued request %s: %w", stored.ID, err)
	}

	res.Decision = DecisionQueue
	res.QueueID = req.ID
	res.Reason = blockedReason
	// Position in score order (1-based) for the 202 response.
	res.QueuePosition = m.queuePositionLocked(req.ID)
	return res, nil
}

// RestoreWaiting re-seeds the in-memory ordering queue from durable WAITING rows after daemon
// startup. It preserves each request's original sequence, enqueue time, footprint, and complete
// start context so recovery neither loses work nor changes FIFO/aging semantics. It is intended
// to run once before the service begins accepting admission requests.
func (m *Manager) RestoreWaiting(ctx context.Context) (int, error) {
	if m.qstore == nil {
		return 0, nil
	}
	rows, err := m.qstore.ListWaiting(ctx)
	if err != nil {
		return 0, fmt.Errorf("restore waiting queue rows: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range rows {
		err := m.queue.Restore(Request{
			ID:       row.ID,
			ModelID:  row.ModelID,
			Engine:   row.Engine,
			Class:    PriorityClass(row.PriorityClass),
			Cost:     CostHint{EstimateMiB: row.FootprintMiB},
			Ctx:      row.ReqCtx,
			Batch:    row.ReqBatch,
			Pin:      row.ReqPin,
			Priority: row.Priority,
		}, row.Seq, row.EnqueuedAt)
		if err != nil {
			return 0, fmt.Errorf("restore waiting queue row %s: %w", row.ID, err)
		}
	}
	return len(rows), nil
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

// IsWaiting reports whether the given queue id is still a live WAITING request in the in-memory
// queue (the authoritative ordering structure). It is the jobs worker's "the drain loop still owns
// this" check (fix 1): a blocked job whose queue_id is still WAITING must NOT be re-admitted by the
// worker (which would mint a duplicate WAITING row + duplicate durable mirror) — the drain loop
// re-admits it when capacity frees, and the worker's next tick then finds the model warm.
func (m *Manager) IsWaiting(id string) bool {
	if id == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.queue.Has(id)
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
