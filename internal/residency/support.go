package residency

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/localhub/localhub/internal/instances"
)

// support.go holds the small concrete pieces the manager needs: id generation, error unwrapping,
// the active-stream counter (Phase-4 seam for the Phase-6 proxy), the per-engine Footprint model,
// and the drain-timeout semantics for DRAINING instances.

// newQueueID returns a uuid for a queue row.
func newQueueID() string { return uuid.NewString() }

// asQueueFull unwraps err into a *QueueFullError (target set + true when it matches).
func asQueueFull(err error, target **QueueFullError) bool {
	var qf *QueueFullError
	if errors.As(err, &qf) {
		*target = qf
		return true
	}
	return false
}

// --- Active-stream tracking (Phase-4 seam for the Phase-6 proxy) -------------------------------
//
// Phase 4/5 have no request proxy yet (that is Phase 6), so there is no true stream counter. This
// tracker exposes IncrementActive/DecrementActive that the instance-API-initiated completion path
// (and, in Phase 6, the proxy) call around each in-flight request. For Ollama — whose /api/ps is
// not stream-aware — the eviction policy additionally treats a recent last_used_at as active (the
// documented approximation in eviction.go). The counter is authoritative for llama.cpp/vLLM.
//
// This is the never-kill-active-streams substrate: eviction reads ActiveCount and NEVER selects an
// instance whose count is > 0.

// InFlightTracker is a concurrency-safe per-instance in-flight counter. Zero value is usable.
type InFlightTracker struct {
	mu     sync.RWMutex
	counts map[string]*int64
}

// NewInFlightTracker builds an empty tracker.
func NewInFlightTracker() *InFlightTracker {
	return &InFlightTracker{counts: make(map[string]*int64)}
}

func (t *InFlightTracker) slot(instanceID string) *int64 {
	t.mu.RLock()
	p, ok := t.counts[instanceID]
	t.mu.RUnlock()
	if ok {
		return p
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, ok := t.counts[instanceID]; ok {
		return p
	}
	if t.counts == nil {
		t.counts = make(map[string]*int64)
	}
	var n int64
	t.counts[instanceID] = &n
	return &n
}

// IncrementActive marks the start of an in-flight request on instanceID and returns the new count.
func (t *InFlightTracker) IncrementActive(instanceID string) int {
	return int(atomic.AddInt64(t.slot(instanceID), 1))
}

// DecrementActive marks the completion of an in-flight request and returns the new count (clamped
// at 0 — a double-decrement never goes negative).
func (t *InFlightTracker) DecrementActive(instanceID string) int {
	p := t.slot(instanceID)
	for {
		cur := atomic.LoadInt64(p)
		if cur <= 0 {
			return 0
		}
		if atomic.CompareAndSwapInt64(p, cur, cur-1) {
			return int(cur - 1)
		}
	}
}

// ActiveCount implements ActiveTracker: the current in-flight count for instanceID (0 = idle).
func (t *InFlightTracker) ActiveCount(instanceID string) int {
	if t == nil {
		return 0
	}
	return int(atomic.LoadInt64(t.slot(instanceID)))
}

// Forget drops the counter for a stopped instance (housekeeping; safe to omit).
func (t *InFlightTracker) Forget(instanceID string) {
	t.mu.Lock()
	delete(t.counts, instanceID)
	t.mu.Unlock()
}

// --- Per-engine footprint model (the P4.5 lesson) ---------------------------------------------
//
// vLLM-docker occupies gpu_memory_utilization × TOTAL VRAM (~util×16 GB regardless of model
// size), while llama.cpp/Ollama occupy the model's estimated/observed VRAM. Admission MUST budget
// the right number per engine or it will wildly mis-admit vLLM (under-budget → OOM) or refuse
// co-residency it could allow (over-budget). Footprint encodes this.

// FootprintInputs carries the numbers Footprint needs. The caller (core) fills these from the
// compat verdict / calibration store / hardware snapshot / config.
type FootprintInputs struct {
	Engine string
	// EstimatedMiB is the compat estimate (or observed peak, if the caller already preferred it)
	// for llama.cpp/Ollama.
	EstimatedMiB int
	// ObservedPeakMiB is the measured peak from the calibration store (0 = none). Preferred over
	// EstimatedMiB for llama.cpp/Ollama when > 0.
	ObservedPeakMiB int
	// GPUMemUtil is the vLLM --gpu-memory-utilization the instance will use (0 → DefaultGPUMemUtil).
	GPUMemUtil float64
	// DefaultGPUMemUtil is the config default vLLM utilization (e.g. 0.90).
	DefaultGPUMemUtil float64
	// TotalVRAMMiB is GPU 0's total VRAM (from the hardware snapshot) — the base vLLM scales.
	TotalVRAMMiB int
}

// Footprint returns the VRAM (MiB) admission should budget for loading a model on an engine, per
// the per-engine model. It is the single place the vLLM util×total rule lives, so admission and
// the queue's cost hint agree.
//
//	llamacpp / ollama : observed peak if known, else the estimate.
//	vllm              : ceil(gpu_memory_utilization × total VRAM) — vLLM FILLS that fraction with
//	                    KV cache regardless of model size (P4.5). Lowering util per instance lowers
//	                    the footprint, enabling co-residency (the only vLLM co-reside lever).
func Footprint(in FootprintInputs) int {
	switch instances.Engine(in.Engine) {
	case instances.EngineVLLM:
		util := in.GPUMemUtil
		if util <= 0 {
			util = in.DefaultGPUMemUtil
		}
		if util <= 0 {
			util = 0.90 // last-resort default matching config
		}
		if in.TotalVRAMMiB <= 0 {
			// Without a total we cannot apply util×total; fall back to the estimate so admission
			// still has a number (better than 0 → false "fits"). The caller should always supply
			// TotalVRAMMiB from the snapshot.
			return in.EstimatedMiB
		}
		mib := int(util*float64(in.TotalVRAMMiB) + 0.9999) // ceil
		return mib
	default: // llamacpp, ollama, and anything else → model-sized footprint
		if in.ObservedPeakMiB > 0 {
			return in.ObservedPeakMiB
		}
		return in.EstimatedMiB
	}
}

// --- DRAINING / drain-timeout semantics --------------------------------------------------------
//
// A USER-INITIATED explicit stop on an instance with active streams transitions it to DRAINING
// (no new requests routed; existing complete; then stop). If it does not drain within
// DrainTimeout (config, default 300s) it is stopped anyway (documented). EVICTION defaults NEVER
// select an active instance at all — the never-kill rule is about eviction defaults; an explicit
// user stop is allowed to eventually complete via the drain timeout.

// DefaultDrainTimeoutSecs is the drain timeout for a user-initiated stop of a busy instance.
const DefaultDrainTimeoutSecs = 300

// DrainDecision is the outcome of evaluating a DRAINING instance at a point in time.
type DrainDecision struct {
	// Stop is true when the instance should now be stopped: either it has fully drained
	// (ActiveCount == 0) or the drain timeout elapsed.
	Stop bool
	// TimedOut is true when Stop was forced by the timeout rather than a clean drain.
	TimedOut bool
	Reason   string
}

// EvaluateDrain decides whether a DRAINING instance (marked at drainingSince) should be stopped
// now. It stops when the instance has no active requests OR the drain timeout elapsed. This is the
// only path by which an active instance is ever stopped, and it exists only for user-initiated
// stops — eviction never reaches it (eviction excludes active instances).
func EvaluateDrain(instanceID string, tracker ActiveTracker, drainingSince, now time.Time, drainTimeout time.Duration) DrainDecision {
	active := 0
	if tracker != nil {
		active = tracker.ActiveCount(instanceID)
	}
	if active == 0 {
		return DrainDecision{Stop: true, Reason: "drained: no active in-flight requests remain — stopping"}
	}
	if drainTimeout <= 0 {
		drainTimeout = DefaultDrainTimeoutSecs * time.Second
	}
	if !drainingSince.IsZero() && now.Sub(drainingSince) >= drainTimeout {
		return DrainDecision{
			Stop: true, TimedOut: true,
			Reason: "drain timeout elapsed: " + drainTimeout.String() +
				" passed with active requests still in flight — stopping anyway (user-initiated stop; documented drain-timeout override)",
		}
	}
	return DrainDecision{Stop: false, Reason: "draining: active in-flight requests still completing; not stopping yet (never-kill-active)"}
}

// --- Store-backed InstanceLister ---------------------------------------------------------------

// StoreLister adapts *instances.Store into the InstanceLister seam: ActiveInstances returns the
// non-terminal instances (List(ctx, activeOnly=true)) — the eviction candidate set + warm check.
type StoreLister struct {
	Store *instances.Store
}

// ActiveInstances implements InstanceLister.
func (l StoreLister) ActiveInstances(ctx context.Context) ([]instances.Instance, error) {
	if l.Store == nil {
		return nil, errors.New("residency: instance store not configured")
	}
	return l.Store.List(ctx, true)
}
