package residency

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// drain.go is Phase 5 fix 1: the PRODUCTION queue-drain loop. Before this, queued admission
// requests could never auto-start when capacity freed — Queue().Next() was only called in tests,
// and StopInstance/eviction freed VRAM without ever re-attempting a queued start. A human/API
// re-POST was required. This is the never-implemented path the first storm exposed.
//
// DrainQueue pops pop-ordered head candidates (honoring the P5.0 ordering incl. coalescing),
// re-Admits each, and — on route_loaded/load_new/evict_then_load — starts it through the injected
// QueueStarter. A request that still cannot fit is LEFT queued (its position/blocked_reason
// updated). The loop is triggered on: instance STOPPED/FAILED/evicted, the TTL sweep tick, and a
// periodic re-admit tick (config residency.drain_interval_seconds, default 10s).
//
// Idempotency (no busy-loop, no double-start): a per-request queued→starting guard makes the
// transition atomic — a request being started by one drain pass is skipped by a concurrent/re-entrant
// pass. The whole drain runs under a single-flight guard so overlapping triggers coalesce into one
// pass rather than stacking.

// QueueStarter starts a drained (de-queued) request through the real start path (core.StartInstance
// re-run without the HTTP caller). It returns nil on a successful start (route_loaded/load_new/
// evict_then_load all count), a *QueuedError-equivalent when the request had to re-queue (the drain
// loop treats that as "leave queued"), or any other error (logged, request left queued).
//
// The seam keeps the residency package pure: the actual spawn (descriptor resolution, launch plan,
// engine runner) lives in core; the Manager only orchestrates WHICH queued request to attempt and
// WHEN. core wires a QueueStarter that calls StartInstance with the persisted request context.
type QueueStarter interface {
	// StartQueued attempts to start a de-queued request. Returns (started, requeued, err):
	//   started=true  → an instance was started (or routed to a warm one) — remove from queue.
	//   requeued=true → admission re-queued it (still no room) — LEAVE it queued (a new row was
	//                   created by admission; the drain loop marks the old row done to avoid dupes).
	//   err != nil    → a genuine start failure — leave the request queued for the next tick.
	StartQueued(ctx context.Context, req DrainRequest) (started bool, requeued bool, err error)
}

// DrainRequest is the full context of a queued request handed to the QueueStarter — everything the
// start path needs, restored from the persisted queue row (migration 0006).
type DrainRequest struct {
	QueueID  string
	ModelID  string
	Engine   string
	Ctx      int
	Batch    int
	Pin      bool
	Priority int
	Class    PriorityClass
}

// DrainResult summarizes one DrainQueue pass (for logging + tests).
type DrainResult struct {
	Started  []string // queue IDs whose request started
	Requeued []string // queue IDs still queued (no room)
	Failed   []string // queue IDs whose start errored this pass
	Skipped  int      // candidates skipped by the in-flight start guard
}

// starterSeam / drain bookkeeping live on the Manager (added here to keep admission.go focused).
// The Manager gained a `starter`, a `draining` single-flight flag, and a `startingIDs` set for the
// per-request idempotency guard. These are declared on the Manager in admission.go's struct via
// the drainState embedded value below to avoid editing the struct literal in NewManager.

// drainState is the drain-loop bookkeeping embedded in the Manager.
type drainState struct {
	starter       QueueStarter
	drainInterval time.Duration

	dmu         sync.Mutex
	draining    bool            // single-flight: a pass is in progress
	startingIDs map[string]bool // queue IDs currently being started (queued→starting guard)
}

// SetStarter injects the QueueStarter (wired at core.New time, like SetSeams). Not safe to call
// concurrently with DrainQueue.
func (m *Manager) SetStarter(starter QueueStarter) { m.drain.starter = starter }

// DrainInterval returns the configured periodic re-admit cadence (default 10s).
func (m *Manager) DrainInterval() time.Duration {
	if m.drain.drainInterval <= 0 {
		return DefaultDrainIntervalSecs * time.Second
	}
	return m.drain.drainInterval
}

// DefaultDrainIntervalSecs is the periodic re-admit tick when unset (config
// residency.drain_interval_seconds).
const DefaultDrainIntervalSecs = 10

// DrainQueue runs ONE drain pass: pop pop-ordered candidates and try to start each, until the
// queue is exhausted or the head can no longer be admitted. It is single-flighted (a concurrent
// call returns immediately with an empty result) so overlapping triggers do not stack or busy-loop.
//
// Each candidate is popped via Queue().Next (honoring class/aging/tie-break/coalescing), guarded
// against double-start (the queued→starting set), then handed to the QueueStarter. A started
// request is removed from the durable queue (marked ADMITTED); a re-queued one is left (its old row
// marked DONE, admission having enqueued a fresh WAITING row); a failed one is left WAITING for the
// next tick. queue.update is published by the caller (core) after the pass.
func (m *Manager) DrainQueue(ctx context.Context) (DrainResult, error) {
	// Single-flight: only one pass at a time.
	m.drain.dmu.Lock()
	if m.drain.draining {
		m.drain.dmu.Unlock()
		return DrainResult{}, nil
	}
	m.drain.draining = true
	if m.drain.startingIDs == nil {
		m.drain.startingIDs = make(map[string]bool)
	}
	m.drain.dmu.Unlock()
	defer func() {
		m.drain.dmu.Lock()
		m.drain.draining = false
		m.drain.dmu.Unlock()
	}()

	if m.drain.starter == nil {
		return DrainResult{}, nil // no starter wired → nothing to do (queue-only / test mode)
	}

	var res DrainResult
	// Bound the pass by the queue length at entry so a request that keeps re-queuing (still no
	// room) cannot spin the loop forever within one pass — it is retried on the NEXT tick instead.
	maxAttempts := m.queueLen() + 1
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Pop the next candidate under the manager lock (Next mutates the queue), applying the live
		// free-VRAM + resident set so ordering/coalescing match what admission will decide.
		req, ok := m.popNextForDrain(ctx)
		if !ok {
			break // queue empty
		}

		// Idempotency guard: skip if this queue ID is already being started by another pass. We
		// removed it from the in-memory queue via Next; re-enqueue is not needed because the guard
		// set owns it until the start resolves. In practice single-flight prevents this, but the
		// guard makes a re-entrant trigger safe.
		if !m.claimStarting(req.QueueID) {
			res.Skipped++
			continue
		}

		started, requeued, err := m.drain.starter.StartQueued(ctx, req)
		m.releaseStarting(req.QueueID)

		switch {
		case err != nil:
			// Genuine start failure: leave it queued for the next tick. Re-enqueue the popped
			// request so it is not lost (its durable row is still WAITING).
			m.requeueForDrain(req)
			res.Failed = append(res.Failed, req.QueueID)
			// Stop the pass on a hard failure to avoid hammering a broken start path this tick.
			return res, fmt.Errorf("drain: start queued %s (%s): %w", req.QueueID, req.ModelID, err)
		case requeued:
			// Admission re-queued it (a fresh WAITING row was created). Mark the OLD row DONE to
			// avoid a duplicate, and stop the pass: if the head could not be admitted, no lower
			// candidate will fit either (they need at least as much room). Retry next tick.
			_ = m.markQueueDone(ctx, req.QueueID, "re-queued by drain: still no room")
			res.Requeued = append(res.Requeued, req.QueueID)
			return res, nil
		case started:
			_ = m.markQueueAdmitted(ctx, req.QueueID)
			res.Started = append(res.Started, req.QueueID)
			// Continue: capacity may allow more of the queue to drain.
		}
	}
	return res, nil
}

// --- manager helpers for the drain loop (lock-aware) ---

// queueLen returns the current in-memory queue depth (lock-guarded).
func (m *Manager) queueLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.queue.Len()
}

// popNextForDrain pops the next queued request in pop-order (class/aging/tie-break/coalescing),
// returning its full persisted context for the start path. It reads the durable row for the
// context fields (ctx/batch/pin/priority) since the in-memory Request now carries them too.
func (m *Manager) popNextForDrain(ctx context.Context) (DrainRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in := m.drainNextInput(ctx)
	r, _, ok := m.queue.Next(in)
	if !ok {
		return DrainRequest{}, false
	}
	dr := DrainRequest{
		QueueID:  r.ID,
		ModelID:  r.ModelID,
		Engine:   r.Engine,
		Ctx:      r.Ctx,
		Batch:    r.Batch,
		Pin:      r.Pin,
		Priority: r.Priority,
		Class:    r.Class,
	}
	// If the in-memory request lost its context (e.g. re-seeded from the store on restart without
	// the fields), fall back to the durable row.
	if dr.Ctx == 0 || dr.Engine == "" {
		if row, err := m.qstore.get(ctx, r.ID); err == nil {
			if dr.Ctx == 0 {
				dr.Ctx = row.ReqCtx
			}
			if dr.Batch == 0 {
				dr.Batch = row.ReqBatch
			}
			if dr.Engine == "" {
				dr.Engine = row.Engine
			}
			if !dr.Pin {
				dr.Pin = row.ReqPin
			}
			if dr.Priority == 0 {
				dr.Priority = row.Priority
			}
		}
	}
	return dr, true
}

// drainNextInput builds the queue NextInput (free VRAM + resident set) for pop-ordering during a
// drain. Best-effort: a probe error yields zero free VRAM (tie-break degrades to FIFO). The
// resident set is derived from the current active instances.
func (m *Manager) drainNextInput(ctx context.Context) NextInput {
	in := NextInput{ResidentModels: map[string]bool{}}
	if m.probe != nil {
		if free, err := m.probe.FreeVRAMMiB(ctx); err == nil {
			in.FreeVRAMMiB = free
		}
	}
	if m.lister != nil {
		if active, err := m.lister.ActiveInstances(ctx); err == nil {
			for _, inst := range active {
				if !inst.State.Terminal() {
					in.ResidentModels[inst.ModelID] = true
				}
			}
		}
	}
	return in
}

// requeueForDrain puts a popped request back in the in-memory queue (used when a start failed so
// the request is retried next tick). It preserves the full context. Enqueue assigns a fresh seq,
// so ordering is FIFO-fair relative to new arrivals (acceptable for a retried failed start).
func (m *Manager) requeueForDrain(req DrainRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.queue.Enqueue(Request{
		ID:       req.QueueID,
		ModelID:  req.ModelID,
		Engine:   req.Engine,
		Class:    req.Class,
		Cost:     CostHint{EstimateMiB: 0},
		Ctx:      req.Ctx,
		Batch:    req.Batch,
		Pin:      req.Pin,
		Priority: req.Priority,
	})
}

// claimStarting marks a queue ID as being started; returns false if already claimed (guard against
// double-start under a re-entrant trigger).
func (m *Manager) claimStarting(id string) bool {
	m.drain.dmu.Lock()
	defer m.drain.dmu.Unlock()
	if m.drain.startingIDs[id] {
		return false
	}
	m.drain.startingIDs[id] = true
	return true
}

func (m *Manager) releaseStarting(id string) {
	m.drain.dmu.Lock()
	delete(m.drain.startingIDs, id)
	m.drain.dmu.Unlock()
}

// markQueueAdmitted / markQueueDone update the durable queue row status (lock not required — the
// store has its own DB serialization).
func (m *Manager) markQueueAdmitted(ctx context.Context, id string) error {
	return m.qstore.SetStatus(ctx, id, QueueAdmitted, "")
}

func (m *Manager) markQueueDone(ctx context.Context, id, reason string) error {
	return m.qstore.SetStatus(ctx, id, QueueDone, reason)
}
