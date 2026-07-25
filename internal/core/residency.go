package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/instances"
	"github.com/localhub/localhub/internal/residency"
)

// residency.go wires the Phase-5 coarse-residency manager (internal/residency) into the service
// core. It provides the three seams the manager needs — a footprint estimator (per-engine model),
// an evictor (routes a graceful stop to the owning engine adapter), and an instance lister — and
// the queue read/cancel methods the API exposes. StartInstance (instances.go) calls admitForStart
// instead of the temporary phase4SafetyCheck when Residency is configured.

// --- FootprintEstimator seam --------------------------------------------------------------------

// residencyEstimator implements residency.FootprintEstimator over the compat engine + calibration
// store + hardware snapshot + config, applying the PER-ENGINE footprint model (residency.Footprint):
// llama.cpp/ollama = observed peak or estimate; vLLM = gpu_memory_utilization × total VRAM.
type residencyEstimator struct{ s *Service }

func (e residencyEstimator) Footprint(ctx context.Context, spec residency.AdmitSpec) (int, error) {
	s := e.s
	snap, err := s.deps.Hardware.Snapshot(ctx)
	if err != nil {
		return 0, fmt.Errorf("read hardware snapshot: %w", err)
	}
	totalVRAM := 0
	if len(snap.GPUs) > 0 {
		totalVRAM = snap.GPUs[0].VRAMTotalMiB
	}

	// vLLM: util × total, independent of the model estimate (the P4.5 lesson).
	if spec.Engine == string(instances.EngineVLLM) {
		fp := residency.Footprint(residency.FootprintInputs{
			Engine:            spec.Engine,
			DefaultGPUMemUtil: s.deps.Cfg.Engines.VLLM.DefaultGPUMemoryUtil,
			TotalVRAMMiB:      totalVRAM,
		})
		if fp <= 0 {
			return 0, fmt.Errorf("vLLM footprint could not be computed (no total VRAM)")
		}
		return fp, nil
	}

	// llama.cpp / ollama: use the compat verdict's estimate, preferring a measured observed peak
	// (the calibration store already feeds the compat engine, so verdict.EstimatedVRAMMiB is the
	// observed peak when one exists — Confidence "exact").
	desc, _, derr := s.resolveDescriptor(ctx, CompatRequest{ModelID: spec.ModelID, Ctx: spec.Ctx, Batch: spec.Batch})
	if derr != nil {
		return 0, fmt.Errorf("resolve model %q: %w", spec.ModelID, derr)
	}
	avail := s.engineAvailability(ctx)
	verdict := s.deps.Compat.Evaluate(ctx, compat.Request{
		Descriptor: desc, RequestedCtx: spec.Ctx, Batch: spec.Batch,
	}, snap, avail)
	if verdict.EstimatedVRAMMiB <= 0 {
		return 0, fmt.Errorf("compat produced no VRAM estimate for %q (%s: %s)", spec.ModelID, verdict.Status, verdict.Reason)
	}
	return residency.Footprint(residency.FootprintInputs{
		Engine:       spec.Engine,
		EstimatedMiB: verdict.EstimatedVRAMMiB,
	}), nil
}

// --- Evictor seam -------------------------------------------------------------------------------

// residencyEvictor implements residency.Evictor: it stops one idle instance through the owning
// engine's Phase-4 adapter (all of which do the VRAM-delta-verified graceful stop). Admission
// calls this one at a time and re-reads the probe after each.
type residencyEvictor struct{ s *Service }

func (e residencyEvictor) Evict(ctx context.Context, inst instances.Instance) (int, error) {
	s := e.s
	switch inst.Engine {
	case instances.EngineOllama:
		if s.deps.OllamaRunner == nil {
			return 0, fmt.Errorf("evict %s: ollama runner not configured", inst.ID)
		}
		tag, terr := s.ollamaTagForModel(ctx, inst.ModelID)
		if terr != nil {
			return 0, fmt.Errorf("evict ollama %s: %w", inst.ID, terr)
		}
		res, serr := s.deps.OllamaRunner.Stop(ctx, inst.ID, tag)
		if serr != nil {
			return 0, serr
		}
		s.publishInstanceUpdated()
		return res.FreedMiB, nil
	case instances.EngineVLLM:
		if s.deps.VLLMRunner == nil {
			return 0, fmt.Errorf("evict %s: vllm runner not configured", inst.ID)
		}
		res, serr := s.deps.VLLMRunner.Stop(ctx, inst.ID)
		if serr != nil {
			return 0, serr
		}
		s.publishInstanceUpdated()
		return res.FreedMiB, nil
	default: // llamacpp
		if s.deps.LlamaRunner == nil {
			return 0, fmt.Errorf("evict %s: llama.cpp runner not configured", inst.ID)
		}
		res, serr := s.deps.LlamaRunner.Stop(ctx, inst.ID)
		if serr != nil {
			return 0, serr
		}
		s.publishInstanceUpdated()
		return res.FreedMiB, nil
	}
}

// admitForStart runs the residency admission flow for a start request and returns the decision.
// StartInstance uses it to decide whether to proceed to spawn (RouteLoaded/LoadNew/EvictThenLoad)
// or to refuse-with-queue-position (Queue/Reject). It passes the precomputed footprint from the
// verdict when available so admission does not re-estimate.
func (s *Service) admitForStart(ctx context.Context, req StartInstanceRequest, reqCtx int, footprintMiB int) (residency.AdmitResult, error) {
	spec := residency.AdmitSpec{
		ModelID:      req.ModelID,
		Engine:       req.Engine,
		Ctx:          reqCtx,
		Batch:        req.Batch,
		Class:        residency.ClassFromPriority(req.Priority),
		Pin:          req.Pin,
		Priority:     req.Priority,
		FootprintMiB: footprintMiB,
	}
	if spec.Engine == "" {
		spec.Engine = string(instances.EngineLlamaCPP)
	}
	return s.deps.Residency.Admit(ctx, spec)
}

// --- Queue API surface --------------------------------------------------------------------------

// QueueEntryView is the API projection of a residency.QueueRow (GET /api/queue).
type QueueEntryView struct {
	ID            string `json:"id"`
	ModelID       string `json:"model_id"`
	Engine        string `json:"engine,omitempty"`
	PriorityClass int    `json:"priority_class"`
	Status        string `json:"status"`
	BlockedReason string `json:"blocked_reason,omitempty"`
	FootprintMiB  int    `json:"footprint_mib,omitempty"`
	Position      int    `json:"position,omitempty"`
	AgeSeconds    int    `json:"age_seconds,omitempty"`
}

// Queue returns the current residency queue for GET /api/queue. It reflects the live in-memory
// ordering (positions) merged with the durable blocked_reason / history. Returns an empty slice
// when residency is not configured.
func (s *Service) Queue(ctx context.Context) ([]QueueEntryView, error) {
	if s.deps.Residency == nil {
		return []QueueEntryView{}, nil
	}
	// Build the Next-input from a live free-VRAM read + the resident set so the tie-break / order
	// shown matches what admission would decide.
	in := s.residencyNextInput(ctx)
	rows, err := s.deps.Residency.QueueRows(ctx, in)
	if err != nil {
		return nil, err
	}
	out := make([]QueueEntryView, 0, len(rows))
	for _, r := range rows {
		v := QueueEntryView{
			ID: r.ID, ModelID: r.ModelID, Engine: r.Engine, PriorityClass: r.PriorityClass,
			Status: string(r.Status), BlockedReason: r.BlockedReason,
			FootprintMiB: r.FootprintMiB, Position: r.Position,
		}
		if !r.EnqueuedAt.IsZero() {
			v.AgeSeconds = int(time.Since(r.EnqueuedAt).Seconds())
			if v.AgeSeconds < 0 {
				v.AgeSeconds = 0
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// CancelQueueEntry cancels a WAITING queue entry (DELETE /api/queue/{id}).
func (s *Service) CancelQueueEntry(ctx context.Context, id string) error {
	if s.deps.Residency == nil {
		return fmt.Errorf("residency not configured")
	}
	removed, err := s.deps.Residency.Cancel(ctx, id)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("queue entry %q not found (already admitted, cancelled, or unknown)", id)
	}
	s.publishQueueUpdated()
	return nil
}

// residencyNextInput builds the live NextInput (free VRAM + resident model set) for queue
// ordering / draining. Best-effort: a probe error yields a zero free-VRAM input (tie-break
// degrades to FIFO, which is safe).
func (s *Service) residencyNextInput(ctx context.Context) residency.NextInput {
	in := residency.NextInput{ResidentModels: map[string]bool{}}
	if s.deps.Hardware != nil {
		if snap, err := s.deps.Hardware.Snapshot(ctx); err == nil && len(snap.GPUs) > 0 {
			in.FreeVRAMMiB = snap.GPUs[0].VRAMFreeMiB
		}
	}
	if s.deps.InstanceStore != nil {
		if list, err := s.deps.InstanceStore.List(ctx, true); err == nil {
			for _, inst := range list {
				if inst.State == instances.StateHealthy {
					in.ResidentModels[inst.ModelID] = true
				}
			}
		}
	}
	return in
}

// publishQueueUpdated announces a queue change on the bus (queue.update) so live transports refetch.
func (s *Service) publishQueueUpdated() {
	if s.deps.Events != nil {
		s.deps.Events.Publish(Event{Type: EventQueueUpdated})
	}
}

// --- Drain loop wiring (Phase 5 fix 1) ---------------------------------------------------------

// residencyStarter implements residency.QueueStarter: it re-runs the start path for a de-queued
// request WITHOUT the original HTTP caller. It is the seam the production drain loop calls when
// capacity frees. Kept on the service (the start path lives here); residency only decides WHICH
// queued request to attempt and WHEN.
type residencyStarter struct{ s *Service }

// StartQueued re-runs StartInstance for a drained request. It reports:
//   - started=true when an instance started (or routed to a warm one),
//   - requeued=true when admission re-queued it (still no room) — a *QueuedError,
//   - err for any other start failure.
//
// It sets DrainedFromQueue so StartInstance skips publishing a duplicate queue.update and does not
// re-enqueue under the SAME queue id (admission creates a fresh row on a re-queue; the drain loop
// marks the old row DONE).
func (r residencyStarter) StartQueued(ctx context.Context, req residency.DrainRequest) (started bool, requeued bool, err error) {
	sreq := StartInstanceRequest{
		ModelID:  req.ModelID,
		Engine:   req.Engine,
		Ctx:      req.Ctx,
		Batch:    req.Batch,
		Priority: req.Priority,
		Pin:      req.Pin,
	}
	_, serr := r.s.StartInstance(ctx, sreq)
	if serr == nil {
		return true, false, nil
	}
	var qErr *QueuedError
	if errors.As(serr, &qErr) {
		return false, true, nil
	}
	return false, false, serr
}

// DrainQueue runs one production drain pass: re-Admit queued requests and auto-start any that now
// fit. Triggered on instance stop/evict/TTL and on the periodic tick. Publishes queue.update after
// the pass so live transports refetch. Best-effort: a drain error is returned for logging but the
// daemon keeps serving.
func (s *Service) DrainQueue(ctx context.Context) (residency.DrainResult, error) {
	if s.deps.Residency == nil {
		return residency.DrainResult{}, nil
	}
	res, err := s.deps.Residency.DrainQueue(ctx)
	if len(res.Started) > 0 || len(res.Requeued) > 0 || len(res.Failed) > 0 {
		s.publishQueueUpdated()
		s.publishInstanceUpdated()
	}
	return res, err
}

// StartPeriodicDrain runs the periodic re-admit tick until ctx is cancelled (fix 1). main starts it
// in a goroutine. The interval is residency.drain_interval_seconds (default 10s). Each tick runs one
// drain pass; the pass is single-flighted internally so an event-triggered drain and a tick-triggered
// drain never stack.
func (s *Service) StartPeriodicDrain(ctx context.Context, onDrain func(residency.DrainResult), onErr func(error)) {
	if s.deps.Residency == nil {
		return
	}
	interval := s.deps.Residency.DrainInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			res, err := s.DrainQueue(ctx)
			if err != nil && onErr != nil {
				onErr(err)
			} else if onDrain != nil && (len(res.Started) > 0 || len(res.Requeued) > 0) {
				onDrain(res)
			}
		}
	}
}

// drainAsync fires a best-effort drain pass in the background (used as an event trigger after a
// stop/eviction frees capacity). Detached from the caller's request context so a disconnected
// caller does not abort the drain; bounded by its own timeout.
func (s *Service) drainAsync() {
	if s.deps.Residency == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_, _ = s.DrainQueue(ctx)
	}()
}
