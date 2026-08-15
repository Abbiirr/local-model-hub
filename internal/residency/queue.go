// Package residency is the LocalHub coarse-residency manager (Phase 5): a custom priority
// queue (P5.0), admission control (P5.1), and eviction policy (P5.2). It decides — for a
// model-load request — whether to route to a warm instance, load directly, evict-then-load,
// queue, or reject; and, when it must make room, which idle instance to unload.
//
// Hard constraints it upholds (CLAUDE.md non-waivable gates, ADR-001):
//   - No cross-engine VRAM preemption / fair-share scheduling. Residency is coarse
//     load/unload of whole models; free VRAM is always RE-MEASURED live from the probe.
//   - Never OOM from our decision: after every eviction, free VRAM is re-read from the probe
//     before the next eviction and before the final start (the anti-OOM re-read rule).
//   - Never kill an active stream: eviction NEVER selects an instance with in-flight
//     requests, a DRAINING instance, or a pinned instance.
//   - Every blocked/refused/queued path carries a numeric, human-readable reason.
//
// This file (queue.go) is P5.0: a pure, in-memory, deterministic priority queue that orders
// admission REQUESTS. It does not touch hardware, engines, or the DB — admission (P5.1) and
// eviction (P5.2) own all of that. The algorithm is specified in docs/residency-priority-queue.md
// and this code implements exactly that document.
package residency

import (
	"fmt"
	"sort"
	"time"
)

// PriorityClass is the coarse priority tier of a request. Lower value = higher priority
// (interactive served first). Values match lmwrapper (interactive=0 < normal=1 < background=2)
// so a future Phase-6 gateway can map endpoint→class identically. The value is stored directly
// in instances.priority / queue.priority (INTEGER), consistent with the existing "priority 0 =
// default" rows.
type PriorityClass int

const (
	// ClassInteractive is a human waiting on a model to come warm (chat/generate). Highest.
	ClassInteractive PriorityClass = 0
	// ClassNormal is the default for an API POST /api/instances with no explicit priority.
	ClassNormal PriorityClass = 1
	// ClassBackground is embeddings / batch / prefetch / warmups. Lowest.
	ClassBackground PriorityClass = 2
)

// Valid reports whether c is one of the three known classes.
func (c PriorityClass) Valid() bool {
	return c == ClassInteractive || c == ClassNormal || c == ClassBackground
}

// String renders the class name (for reasons/logging).
func (c PriorityClass) String() string {
	switch c {
	case ClassInteractive:
		return "interactive"
	case ClassNormal:
		return "normal"
	case ClassBackground:
		return "background"
	default:
		return fmt.Sprintf("class(%d)", int(c))
	}
}

// ClassFromPriority maps a raw instances.priority INTEGER to a PriorityClass, clamping
// out-of-range values (any value >= 2 is background; <= 0 is interactive). This lets the queue
// accept the existing priority column and the API's optional `priority` field without a
// separate enum on the wire.
func ClassFromPriority(p int) PriorityClass {
	switch {
	case p <= 0:
		return ClassInteractive
	case p == 1:
		return ClassNormal
	default:
		return ClassBackground
	}
}

// QueueConfig holds the tunable constants of the ordering algorithm. Zero-value fields fall
// back to the documented defaults via withDefaults, so a caller may pass QueueConfig{} for the
// standard behavior. All values are documented in docs/residency-priority-queue.md.
type QueueConfig struct {
	// AgePromoteNormal: a normal(1) request waiting >= this competes as interactive(0).
	AgePromoteNormal time.Duration
	// AgePromoteBackground: a background(2) request waiting >= this competes as normal(1).
	// (With the default 60/120s, a background request that has waited >= AgePromoteNormal too
	// crosses BOTH thresholds and reaches interactive-equivalent — see the design note.)
	AgePromoteBackground time.Duration
	// CoalesceMax bounds how many consecutive same-resident-model requests may be served before
	// the strict score order resumes (default 4). 0 disables coalescing.
	CoalesceMax int
	// Caps is the per-class WAITING depth cap. Enqueue past a cap is rejected with queue_full.
	Caps ClassCaps
}

// ClassCaps is the per-class WAITING-depth cap. Defaults {64,256,512} (lmwrapper).
type ClassCaps struct {
	Interactive int
	Normal      int
	Background  int
}

func (c ClassCaps) cap(class PriorityClass) int {
	switch class {
	case ClassInteractive:
		return c.Interactive
	case ClassNormal:
		return c.Normal
	default:
		return c.Background
	}
}

// Default constants (docs/residency-priority-queue.md).
const (
	DefaultAgePromoteNormalSecs     = 60
	DefaultAgePromoteBackgroundSecs = 120
	DefaultCoalesceMax              = 4
	DefaultCapInteractive           = 64
	DefaultCapNormal                = 256
	DefaultCapBackground            = 512
)

func (cfg QueueConfig) withDefaults() QueueConfig {
	if cfg.AgePromoteNormal <= 0 {
		cfg.AgePromoteNormal = DefaultAgePromoteNormalSecs * time.Second
	}
	if cfg.AgePromoteBackground <= 0 {
		cfg.AgePromoteBackground = DefaultAgePromoteBackgroundSecs * time.Second
	}
	if cfg.CoalesceMax == 0 {
		cfg.CoalesceMax = DefaultCoalesceMax
	}
	if cfg.CoalesceMax < 0 {
		cfg.CoalesceMax = 0
	}
	if cfg.Caps.Interactive <= 0 {
		cfg.Caps.Interactive = DefaultCapInteractive
	}
	if cfg.Caps.Normal <= 0 {
		cfg.Caps.Normal = DefaultCapNormal
	}
	if cfg.Caps.Background <= 0 {
		cfg.Caps.Background = DefaultCapBackground
	}
	return cfg
}

// CostHint carries the optional size/cost information for the within-class tie-break. The queue
// itself never probes hardware; the caller (admission) supplies what it knows at enqueue and
// again passes the live free-VRAM at Next-time via NextInput. A zero CostHint means "unknown"
// → treated as expensive (tie_break=1), so the tie-break degrades to a pure FIFO no-op.
type CostHint struct {
	// Warm is true if the request's model is already resident (route_loaded — zero VRAM cost).
	Warm bool
	// EstimateMiB is the admission footprint estimate for the model (0 = unknown). Combined
	// with the live free VRAM at Next-time to decide "cheap-now" (fits without eviction).
	EstimateMiB int
}

// Request is one queued admission request. It is the unit the queue orders. Fields are set at
// Enqueue; seq and enqueuedAt are assigned by the queue (do not set them yourself).
//
// The queue itself only READS Class/ModelID/Cost for ordering; the remaining fields
// (Ctx/Batch/Pin/Priority/BinaryPath) are the FULL request context the drain loop needs to
// re-Admit and start a queued request without the original HTTP caller. They were lost on enqueue
// before Phase 5's drain fix (the never-implemented auto-drain path) — a queued request could
// never auto-start because ctx/pin/priority were dropped. Carrying them here (and persisting them
// in the queue row via the 0006 migration) is what lets DrainQueue re-run the start path faithfully.
type Request struct {
	ID      string        // caller-chosen id (e.g. the queue row uuid); echoed back by Next
	ModelID string        // the model to load (for coalescing + display)
	Engine  string        // target engine ("llamacpp"|"ollama"|"vllm"); informational
	Class   PriorityClass // base priority class
	Cost    CostHint      // optional size/cost hint for the within-class tie-break

	// Full start context carried so the drain loop can faithfully re-Admit + start a queued
	// request (extends the queue row via migration 0006). Zero values are fine (the start path
	// applies its own defaults).
	Ctx      int  // requested context length
	Batch    int  // requested batch
	Pin      bool // pin the started instance (never auto-evict)
	Priority int  // raw priority int the caller supplied (0/1/2 → class); preserved verbatim

	// Profile is the REST of the behaviour-affecting launch profile (migration 0009): the
	// independent KV pair, slot count, speculative mode + draft max, and CPU-offload permission.
	// It MUST round-trip enqueue → daemon restart → drain → start unchanged; dropping any field
	// silently rewrote the user's request on replay (an f16-KV replay of a q4_0 request priced
	// ~1.5 GiB higher and waited forever — KNOWN-LIMITATIONS #21).
	Profile ProfileSpec

	// seq is the monotonic arrival sequence assigned at Enqueue — the FIFO/total-order key.
	seq int64
	// enqueuedAt is the clock time at Enqueue — the aging anchor.
	enqueuedAt time.Time
}

// Clock is the time source the queue reads. Injected so tests drive aging with a fake clock.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// RealClock is the production wall-clock.
func RealClock() Clock { return realClock{} }

// QueueFullError is returned by Enqueue when the request's class is at capacity. It carries the
// numeric reason (schema-tested to contain the numbers) so no request is ever silently dropped.
type QueueFullError struct {
	Class   PriorityClass
	Depth   int // current WAITING depth in the class
	Cap     int // the class cap
	ReasonS string
}

func (e *QueueFullError) Error() string { return e.ReasonS }

// Reason returns the numeric human-readable why (mirrors the verdict/blocked-reason convention).
func (e *QueueFullError) Reason() string { return e.ReasonS }

// Queue is the custom priority queue (P5.0). It is a pure in-memory ordering structure:
// deterministic given the same clock + inputs, with a total order over requests (no
// map-iteration nondeterminism). It NEVER probes hardware, calls an engine, reads the DB, or
// selects an eviction victim — admission (P5.1) and eviction (P5.2) own that. It is NOT safe
// for concurrent use on its own; the ResidencyManager (P5.1) serializes access under its lock.
type Queue struct {
	cfg   QueueConfig
	clock Clock

	items   []*Request
	nextSeq int64

	// coalescing bookkeeping: the model currently being coalesced and how many consecutive
	// coalesced picks have been served for it. Reset when the served model changes or a
	// higher-class request forces a switch.
	coalesceModel  string
	coalesceServed int
}

// NewQueue builds a Queue with the given config (zero value → documented defaults) and clock
// (nil → RealClock).
func NewQueue(cfg QueueConfig, clock Clock) *Queue {
	if clock == nil {
		clock = RealClock()
	}
	return &Queue{cfg: cfg.withDefaults(), clock: clock}
}

// Len returns the number of WAITING requests.
func (q *Queue) Len() int { return len(q.items) }

// DepthByClass returns the current WAITING depth per class (for metrics / caps display).
func (q *Queue) DepthByClass() (interactive, normal, background int) {
	for _, r := range q.items {
		switch r.Class {
		case ClassInteractive:
			interactive++
		case ClassNormal:
			normal++
		default:
			background++
		}
	}
	return
}

// Enqueue admits a request to the WAITING set, assigning its seq + enqueuedAt. It returns the
// STORED copy (with seq + enqueuedAt populated) so the caller can persist the same FIFO/total-order
// key the in-memory queue uses — the durable mirror (queue.InsertWaiting) MUST record the assigned
// seq, or restart/read paths that ORDER BY seq lose FIFO determinism (fix 3: the copy the queue
// owns held the seq while the caller's original Request stayed seq=0). It returns a *QueueFullError
// (numeric reason) if the request's class is at capacity — the request is NOT added in that case
// (the returned Request is then the zero value). The request's Class is clamped to a valid value.
func (q *Queue) Enqueue(r Request) (Request, error) {
	if !r.Class.Valid() {
		r.Class = ClassNormal
	}
	// Per-class cap check (reject rather than unbounded-buffer).
	depth := q.classDepth(r.Class)
	capN := q.cfg.Caps.cap(r.Class)
	if depth >= capN {
		return Request{}, &QueueFullError{
			Class: r.Class, Depth: depth, Cap: capN,
			ReasonS: fmt.Sprintf(
				"queue_full: %s queue is at capacity (%d/%d waiting); reject rather than unbounded-buffer — retry after a slot frees",
				r.Class, depth, capN),
		}
	}
	rc := r // copy; the queue owns its items
	// seq is 1-based: pre-increment so the FIRST enqueue gets seq=1, never 0. A persisted seq of 0
	// then unambiguously means "never assigned" (fix 3 — the durable row COALESCEs a missing seq to
	// 0, so a legitimate first request must not also be 0 or restart ordering can't tell them apart).
	q.nextSeq++
	rc.seq = q.nextSeq
	rc.enqueuedAt = q.clock.Now()
	q.items = append(q.items, &rc)
	return rc, nil // return the assigned copy so the caller persists the SAME seq (fix 3)
}

// Restore re-seeds a request that was durably queued before a daemon restart. Unlike Enqueue it
// preserves the original sequence and enqueue time and deliberately bypasses today's class caps:
// a persisted request must never be discarded merely because an operator later lowers a cap.
// Restore is only for startup recovery; callers must serialize it with other queue operations.
func (q *Queue) Restore(r Request, seq int64, enqueuedAt time.Time) error {
	if r.ID == "" {
		return fmt.Errorf("restore queue request: missing id")
	}
	if q.Has(r.ID) {
		return fmt.Errorf("restore queue request %q: duplicate id", r.ID)
	}
	if !r.Class.Valid() {
		r.Class = ClassFromPriority(r.Priority)
	}
	if seq <= 0 {
		return fmt.Errorf("restore queue request %q: invalid seq %d", r.ID, seq)
	}
	if enqueuedAt.IsZero() {
		return fmt.Errorf("restore queue request %q: missing enqueue time", r.ID)
	}
	r.seq = seq
	r.enqueuedAt = enqueuedAt
	q.items = append(q.items, &r)
	if seq > q.nextSeq {
		q.nextSeq = seq
	}
	return nil
}

func (q *Queue) classDepth(class PriorityClass) int {
	n := 0
	for _, r := range q.items {
		if r.Class == class {
			n++
		}
	}
	return n
}

// Has reports whether a WAITING request with the given id is currently in the queue. It is the
// authoritative "still queued" signal (the in-memory queue owns WAITING; a request leaves it only
// via Next/Remove). Used by the jobs worker to hold AT MOST ONE live residency row per job (fix 1):
// while a blocked job's queue_id is still WAITING, the worker skips re-admission (the drain loop
// owns it) rather than calling Admit again and minting a duplicate WAITING row.
func (q *Queue) Has(id string) bool {
	for _, r := range q.items {
		if r.ID == id {
			return true
		}
	}
	return false
}

// Remove deletes a WAITING request by id (e.g. cancelled via DELETE /api/queue/{id}, or admitted
// out-of-band). Returns true if it was present.
func (q *Queue) Remove(id string) bool {
	for i, r := range q.items {
		if r.ID == id {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return true
		}
	}
	return false
}

// NextInput is the live context the queue needs to compute the size/cost tie-break and the
// coalescing decision at Next-time. The caller (admission) supplies it fresh on every Next call
// (free VRAM re-read live per the anti-OOM rule; the resident set from the instance store).
type NextInput struct {
	// FreeVRAMMiB is the live free VRAM (re-read from the probe). Used with each request's
	// CostHint.EstimateMiB to decide whether it is "cheap-now" (fits without eviction).
	FreeVRAMMiB int
	// ResidentModels is the set of model_ids currently HEALTHY-resident. Used for coalescing
	// and for confirming a request's Warm hint. May be nil/empty (→ no coalescing).
	ResidentModels map[string]bool
}

// scoredKey is the lexicographic ordering tuple for a request at a given now.
type scoredKey struct {
	class    int   // effective class after aging (0/1/2)
	tieBreak int   // 0 = cheap-now, 1 = otherwise (within-class only)
	seq      int64 // FIFO arrival sequence (total-order tie-break)
}

// less reports whether a should be served before b (smaller tuple wins).
func (a scoredKey) less(b scoredKey) bool {
	if a.class != b.class {
		return a.class < b.class
	}
	if a.tieBreak != b.tieBreak {
		return a.tieBreak < b.tieBreak
	}
	return a.seq < b.seq
}

// effectiveClass folds aging in: promotions are counted by AGE against BOTH thresholds (see
// the design note's exact promotion math), clamped at interactive(0).
func (q *Queue) effectiveClass(r *Request, now time.Time) int {
	age := now.Sub(r.enqueuedAt)
	promotions := 0
	if age >= q.cfg.AgePromoteNormal {
		promotions++
	}
	if age >= q.cfg.AgePromoteBackground {
		promotions++
	}
	c := int(r.Class) - promotions
	if c < 0 {
		c = 0
	}
	return c
}

// tieBreak returns 0 if the request is "cheap-now" (served without eviction: warm, or estimate
// fits in current free VRAM), else 1. A zero/unknown hint → 1 (expensive), a pure FIFO no-op.
func (q *Queue) tieBreak(r *Request, in NextInput) int {
	warm := r.Cost.Warm || (in.ResidentModels != nil && in.ResidentModels[r.ModelID])
	if warm {
		return 0
	}
	if r.Cost.EstimateMiB > 0 && in.FreeVRAMMiB > 0 && r.Cost.EstimateMiB <= in.FreeVRAMMiB {
		return 0
	}
	return 1
}

// scoreKey computes a request's ordering tuple at now under the given input.
func (q *Queue) scoreKey(r *Request, now time.Time, in NextInput) scoredKey {
	return scoredKey{
		class:    q.effectiveClass(r, now),
		tieBreak: q.tieBreak(r, in),
		seq:      r.seq,
	}
}

// Peek returns the request that Next WOULD return, without removing it or touching coalescing
// state. Returns (Request, false) when the queue is empty. Useful for admission to look at the
// head before deciding, and for tests.
func (q *Queue) Peek(in NextInput) (Request, bool) {
	r := q.pick(in)
	if r == nil {
		return Request{}, false
	}
	return *r, true
}

// Next selects and REMOVES the highest-priority request to attempt admission for, honoring
// aging, the size/cost tie-break, and bounded same-model coalescing with the higher-class
// starvation guard. It returns (Request, coalesced, true) or (_, _, false) when empty.
//
// coalesced reports whether this pick was a coalescing pick (same resident model, credit not
// exhausted, no higher-class waiter) — the caller can use it to maintain observability, but the
// queue maintains its own coalescing credit internally.
func (q *Queue) Next(in NextInput) (Request, bool, bool) {
	r, coalesced := q.pickWithCoalescing(in)
	if r == nil {
		return Request{}, false, false
	}
	q.Remove(r.ID)
	return *r, coalesced, true
}

// pick returns the strict score-order head (no coalescing), or nil if empty.
func (q *Queue) pick(in NextInput) *Request {
	now := q.clock.Now()
	var best *Request
	var bestKey scoredKey
	for _, r := range q.items {
		k := q.scoreKey(r, now, in)
		if best == nil || k.less(bestKey) {
			best = r
			bestKey = k
		}
	}
	return best
}

// pickWithCoalescing applies the bounded same-model coalescing rule with the higher-class
// starvation guard, and updates the coalescing credit. It returns the chosen request and
// whether the choice was a coalescing pick.
//
// Algorithm (docs/residency-priority-queue.md §coalescing):
//  1. Compute the strict score-order head H.
//  2. If coalescing is enabled AND we have an active coalescing model M with credit remaining
//     AND there is a WAITING request for M whose EFFECTIVE CLASS is not strictly lower priority
//     than H's (i.e. no strictly-higher-class request would be starved), prefer that request
//     for M (a coalesced pick). Otherwise return H and (re)set the coalescing model to H's model
//     with a fresh credit.
//
// The guard clause is the load-bearing one: coalescing may only skip ahead among requests whose
// effective class is >= the head's effective class (numerically), so a strictly-higher-class
// request (numerically smaller effective class) is NEVER deferred by coalescing.
func (q *Queue) pickWithCoalescing(in NextInput) (*Request, bool) {
	head := q.pick(in)
	if head == nil {
		return nil, false
	}
	if q.cfg.CoalesceMax <= 0 || len(in.ResidentModels) == 0 {
		q.startCoalesceRun(head)
		return head, false
	}

	now := q.clock.Now()
	headKey := q.scoreKey(head, now, in)

	// Continue an active coalescing run only if the coalesced model is still resident, still
	// has WAITING requests, and the credit is not exhausted.
	if q.coalesceModel != "" && q.coalesceServed < q.cfg.CoalesceMax && in.ResidentModels[q.coalesceModel] {
		if cand := q.bestForModel(q.coalesceModel, now, in); cand != nil {
			// Starvation guard: suppress coalescing if the candidate's effective class is
			// strictly LOWER priority than the head's (i.e. a strictly-higher-class request —
			// the head — waits). Equivalently, only coalesce when the candidate does not defer a
			// strictly-higher-class request: candidate effective class <= head effective class.
			candKey := q.scoreKey(cand, now, in)
			if candKey.class <= headKey.class {
				q.coalesceServed++
				return cand, true
			}
		}
	}

	// No active coalescing pick: serve the head and (re)start a coalescing run on ITS model,
	// but only if that model is resident (so we can chain further requests for it). If the
	// head's model is not resident, clear the run (loading a new model — nothing to coalesce
	// yet).
	q.startCoalesceRun(head)
	// Serving the head counts as the first pick of its run when its model is resident.
	if in.ResidentModels[head.ModelID] {
		q.coalesceServed = 1
	}
	return head, false
}

// startCoalesceRun (re)initializes the coalescing bookkeeping for a newly-served head.
func (q *Queue) startCoalesceRun(head *Request) {
	if q.coalesceModel != head.ModelID {
		q.coalesceModel = head.ModelID
		q.coalesceServed = 0
	}
}

// bestForModel returns the highest-score WAITING request whose model == modelID, or nil.
func (q *Queue) bestForModel(modelID string, now time.Time, in NextInput) *Request {
	var best *Request
	var bestKey scoredKey
	for _, r := range q.items {
		if r.ModelID != modelID {
			continue
		}
		k := q.scoreKey(r, now, in)
		if best == nil || k.less(bestKey) {
			best = r
			bestKey = k
		}
	}
	return best
}

// Snapshot returns a stable, score-ordered copy of the WAITING requests (highest priority
// first) for GET /api/queue and diagnostics. It does NOT remove anything and does NOT touch
// coalescing state. Ordering is the strict score order at now (coalescing is a Next-time
// concern, not a display concern).
func (q *Queue) Snapshot(in NextInput) []Request {
	now := q.clock.Now()
	idx := make([]*Request, len(q.items))
	copy(idx, q.items)
	sort.SliceStable(idx, func(i, j int) bool {
		return q.scoreKey(idx[i], now, in).less(q.scoreKey(idx[j], now, in))
	})
	out := make([]Request, len(idx))
	for i, r := range idx {
		out[i] = *r
	}
	return out
}

// EnqueuedAt exposes a request's enqueue time (for the caller to compute age in a queue row).
func (r Request) EnqueuedAt() time.Time { return r.enqueuedAt }

// Seq exposes a request's arrival sequence (for stable display ordering).
func (r Request) Seq() int64 { return r.seq }
