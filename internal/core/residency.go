package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/hardware"
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

// admitGate is the ONE admission gate shared by all three engine start paths (fix 2: llama.cpp
// spawn, Ollama warm, vLLM docker all route through the same residency policy — before this, only
// llama.cpp reached Admit while Ollama/vLLM stopped at the Phase-4 free-VRAM pre-check, so
// /api/instances, /v1/*, and jobs got no route_loaded/load_new/evict_then_load/queue behavior for
// those engines despite the docs promising ONE policy).
//
// It first enforces the compat verdict gate (a wont_run/unknown model is never admitted), then:
//   - Residency configured → residency.Admit decides fit. It returns:
//     route_loaded    → (warm=&view, evicted=false, nil): caller returns the warm instance, no spawn.
//     load_new        → (nil, false, nil): caller proceeds to the engine-specific start.
//     evict_then_load → (nil, true, nil): residency already executed the verified evictions;
//     caller proceeds to start and should re-read its snapshot.
//     queue           → (nil, false, *QueuedError): caller returns it (API → 202 + position;
//     proxy → fast 503; jobs → leave queued). Carries the numeric blocked_reason
//     (now including ram_blocked, fix 4).
//     reject          → (nil, false, error "REFUSED: ...").
//   - Residency NOT configured → the conservative Phase-4 pre-check (free − 1.5 GB budget) so the
//     daemon still refuses an over-budget start.
//
// Engine-specific start/stop mechanics are UNCHANGED — only admission is unified. The caller owns
// the actual warm/spawn/docker-run after a proceed result.
func (s *Service) admitGate(ctx context.Context, req StartInstanceRequest, reqCtx int, verdict compat.Verdict, snap hardware.Snapshot) (warm *InstanceView, evicted bool, err error) {
	if verdict.Status != compat.StatusRuns && verdict.Status != compat.StatusRunsDegraded {
		return nil, false, fmt.Errorf("REFUSED: compatibility verdict is %q (needs 'runs' or 'runs_degraded'). %s",
			verdict.Status, verdict.Reason)
	}
	if s.deps.Residency == nil {
		// Residency disabled → conservative phase-4 pre-check (free − 1.5 GB budget).
		if perr := s.phase4SafetyCheck(verdict, snap); perr != nil {
			return nil, false, perr
		}
		return nil, false, nil
	}
	// Pass the verdict's CONFIDENCE, not just its number: a footprint that came from a measured
	// observed peak earns the smaller additive admission cushion, an estimate keeps the full
	// configured margin. This is the only place the claim is made, and it is derived strictly from
	// the verdict rather than from anything a caller supplied.
	decision, aerr := s.admitForStart(ctx, req, reqCtx, verdict.EstimatedVRAMMiB,
		verdict.Confidence == compat.ConfidenceExact)
	if aerr != nil {
		return nil, false, fmt.Errorf("admission: %w", aerr)
	}
	switch decision.Decision {
	case residency.DecisionRouteLoaded:
		if decision.WarmInstance != nil {
			v := instanceToView(*decision.WarmInstance)
			return &v, false, nil
		}
		return nil, false, nil
	case residency.DecisionEvictThenLoad:
		s.publishQueueUpdated()
		return nil, true, nil
	case residency.DecisionLoadNew:
		return nil, false, nil
	case residency.DecisionQueue:
		// Blocked: nothing started; carry the numeric blocked_reason + position in a typed QUEUED
		// error the caller (API 202 / proxy 503 / jobs leave-queued) classifies.
		s.publishQueueUpdated()
		return nil, false, &QueuedError{QueueID: decision.QueueID, Position: decision.QueuePosition, ReasonS: decision.Reason}
	case residency.DecisionReject:
		return nil, false, fmt.Errorf("REFUSED: %s", decision.Reason)
	}
	return nil, false, nil
}

// admitForStart runs the residency admission flow for a start request and returns the decision.
// StartInstance uses it to decide whether to proceed to spawn (RouteLoaded/LoadNew/EvictThenLoad)
// or to refuse-with-queue-position (Queue/Reject). It passes the precomputed footprint from the
// verdict when available so admission does not re-estimate.
func (s *Service) admitForStart(ctx context.Context, req StartInstanceRequest, reqCtx int, footprintMiB int, measured bool) (residency.AdmitResult, error) {
	spec := residency.AdmitSpec{
		ModelID:      req.ModelID,
		Engine:       req.Engine,
		Ctx:          reqCtx,
		Batch:        req.Batch,
		Class:        residency.ClassFromPriority(req.Priority),
		Pin:          req.Pin,
		Priority:     req.Priority,
		FootprintMiB: footprintMiB,
		// True only when footprintMiB is a MEASURED observed peak (Verdict.Confidence "exact").
		MeasuredFootprint: measured,
		// The launch profile the footprint above was computed FOR. Persisted with the queue row so
		// a queued start replays with the same profile instead of a default-shaped one (M2.4).
		Profile: profileSpecFromRequest(req),
	}
	if spec.Engine == "" {
		spec.Engine = string(instances.EngineLlamaCPP)
	}
	// vLLM's real footprint is gpu_memory_utilization × TOTAL VRAM (it pre-allocates its whole
	// configured fraction, not the model size — KNOWN-LIMITATIONS #3 / the P4.5 lesson). The compat
	// verdict estimate is the model's *required* VRAM, which UNDER-counts what vLLM actually grabs.
	// So for vLLM we drop the precomputed footprint and let residency's estimator compute util×total
	// (residencyEstimator.Footprint) — the conservative, anti-OOM number for admission (fix 2).
	if spec.Engine == string(instances.EngineVLLM) {
		spec.FootprintMiB = 0
		// The measurement claim belonged to the discarded compat number; residency will estimate
		// util×total itself, and that is NOT a measured peak.
		spec.MeasuredFootprint = false
	}
	return s.deps.Residency.Admit(ctx, spec)
}

// --- AutoLoadGate seam (migration 0014) ----------------------------------------------------------

// residencyAutoLoadGate implements residency.AutoLoadGate over the models table so the drain loop
// can refuse to PROMOTE a decommissioned model out of the queue (see AutoLoadGate's doc comment in
// internal/residency/drain.go and StartInstance's belt-and-braces check for the other two enforcement
// points of the same flag).
type residencyAutoLoadGate struct{ s *Service }

func (g residencyAutoLoadGate) AutoLoadRefusalReason(ctx context.Context, modelID string) string {
	return g.s.autoLoadDisabledReason(ctx, modelID)
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

// queueEntryWaiting reports whether the given residency queue id is still a live WAITING request
// (the drain loop still owns it). Used by the jobs worker to hold AT MOST ONE live residency row per
// job (fix 1): while a blocked job's queue_id is WAITING, the worker must not re-admit and mint a
// duplicate row. Returns false when residency is unconfigured or the id is empty/gone.
func (s *Service) queueEntryWaiting(queueID string) bool {
	if s.deps.Residency == nil || queueID == "" {
		return false
	}
	return s.deps.Residency.IsWaiting(queueID)
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

// --- Launch-profile ⇄ queue translation (M2.4) --------------------------------------------------
//
// These two functions are the ONLY bridge between `core.StartInstanceRequest` (the API shape) and
// `residency.ProfileSpec` (the persisted queue shape). They are exact inverses for every
// behaviour-affecting field, which is the property TestProfileSpecRoundTrip pins:
//
//	startRequestFromDrain(drainOf(profileSpecFromRequest(req))) == req   (for profile fields)
//
// Keeping the mapping in one pair of functions is what makes the round-trip auditable. The
// 2026-07-25 defect existed precisely because the reverse direction was written inline in
// StartQueued and silently omitted three fields.

// profileSpecFromRequest projects the profile-bearing fields of a start request into the durable
// queue shape. The legacy `kv_quant` bool is RESOLVED here — not stored alongside the pair — so the
// queue holds exactly one representation of the KV cache and a restart cannot surface a
// disagreement between them. The resolution matches startProfile's precedence exactly: an explicit
// (K,V) wins; otherwise kv_quant=true means q4_0 on both sides.
func profileSpecFromRequest(req StartInstanceRequest) residency.ProfileSpec {
	p := startProfile(req, req.Ctx)
	return residency.ProfileSpec{
		KVTypeK:         string(p.KVTypeK),
		KVTypeV:         string(p.KVTypeV),
		Slots:           p.Slots,
		SpecMode:        p.SpecMode,
		SpecDraftMax:    p.SpecDraftMax,
		AllowCPUOffload: p.AllowCPUOffload,
		// Server-MODE fields (migration 0013): dropping these here is the 2026-08-06 defect — a
		// queued embedding/reranking start replayed as a plain generation start and crashed
		// encoder-only models (bge-m3) instantly. See ProfileSpec.Embedding's doc comment.
		Embedding: p.Embedding,
		Pooling:   p.Pooling,
		Reranking: p.Reranking,
	}
}

// startRequestFromDrain rebuilds the start request for a de-queued (drained) request, restoring the
// FULL launch profile the request was admitted with.
//
// KVQuant is deliberately left false: the KV cache is now described by the explicit (K,V) pair the
// queue persisted, and startProfile's precedence gives that pair priority anyway. Setting the bool
// too would be redundant at best and contradictory at worst.
func startRequestFromDrain(req residency.DrainRequest) StartInstanceRequest {
	return StartInstanceRequest{
		ModelID:         req.ModelID,
		Engine:          req.Engine,
		Ctx:             req.Ctx,
		Batch:           req.Batch,
		Priority:        req.Priority,
		Pin:             req.Pin,
		KVTypeK:         req.Profile.KVTypeK,
		KVTypeV:         req.Profile.KVTypeV,
		Slots:           req.Profile.Slots,
		Speculative:     req.Profile.SpecMode,
		SpecDraftMax:    req.Profile.SpecDraftMax,
		AllowCPUOffload: req.Profile.AllowCPUOffload,
		// Server-MODE fields (migration 0013): restored so a replayed start rebuilds the SAME
		// LaunchProfile it was queued with, instead of silently defaulting to plain generation mode
		// (the 2026-08-06 bge-m3 crash-loop defect).
		Embedding: req.Profile.Embedding,
		Pooling:   req.Profile.Pooling,
		Reranking: req.Profile.Reranking,
	}
}

// --- Drain loop wiring (Phase 5 fix 1) ---------------------------------------------------------

// residencyStarter implements residency.QueueStarter: it re-runs the start path for a de-queued
// request WITHOUT the original HTTP caller. It is the seam the production drain loop calls when
// capacity frees. Kept on the service (the start path lives here); residency only decides WHICH
// queued request to attempt and WHEN.
type residencyStarter struct{ s *Service }

func (r residencyStarter) rejectWrongModeReplay(ctx context.Context, view InstanceView, mismatch error) error {
	if r.s.deps.LlamaRunner == nil {
		return mismatch
	}
	if _, stopErr := r.s.deps.LlamaRunner.Stop(ctx, view.ID); stopErr != nil {
		return fmt.Errorf("%w; cleanup of wrong-mode instance %q failed: %v", mismatch, view.ID, stopErr)
	}
	return mismatch
}

// verifyReplayedLaunchMode is the SAFETY GUARD (migration 0013): a replayed start must launch in
// the SAME mode it was queued with. LaunchFlags is the AUTHORITATIVE record of what the process
// actually spawned with (launchFlagsHaveEmbedding's doc comment) — comparing it against what the
// restored request asked for catches ANY way the mode failed to survive the round-trip, not just
// the specific dropped-field bug fixed here. Scoped to llama.cpp: Embedding/Pooling/Reranking are
// llama.cpp-specific fields that ollama/vllm ignore (StartInstanceRequest doc comments), so
// LaunchFlags is not a meaningful mode record for those engines — nil error for them always.
//
// This matters even beyond the bge-m3 case: a model that CAN serve generation traffic without
// crashing would otherwise silently come back in the wrong mode and answer the wrong endpoints — a
// crash is a lucky, loud failure; this guard makes the failure loud even when the process would
// otherwise start "successfully" in the wrong mode (CLAUDE.md: every verdict/error carries a
// human-readable reason; refuse rather than silently mis-launch).
func verifyReplayedLaunchMode(queueID, modelID string, sreq StartInstanceRequest, view InstanceView) error {
	if sreq.Engine != string(instances.EngineLlamaCPP) {
		return nil
	}
	want := requestedLaunchMode(sreq)
	got := launchModeOfFlags(view.LaunchFlags)
	if got == want {
		return nil
	}
	return fmt.Errorf(
		"queued start %s for model %q replayed in %s mode but was requested as %s mode "+
			"(launch-profile restore mismatch) — refusing rather than silently serving the wrong endpoints",
		queueID, modelID, got, want)
}

// StartQueued re-runs StartInstance for a drained request. It reports:
//   - started=true when an instance started (or routed to a warm one),
//   - requeued=true when admission re-queued it (still no room) — a *QueuedError,
//   - err for any other start failure.
//
// It sets DrainedFromQueue so StartInstance skips publishing a duplicate queue.update and does not
// re-enqueue under the SAME queue id (admission creates a fresh row on a re-queue; the drain loop
// marks the old row DONE).
func (r residencyStarter) StartQueued(ctx context.Context, req residency.DrainRequest) (started bool, requeued bool, err error) {
	sreq := startRequestFromDrain(req)
	view, serr := r.s.StartInstance(ctx, sreq)
	if serr == nil {
		if gerr := verifyReplayedLaunchMode(req.QueueID, req.ModelID, sreq, view); gerr != nil {
			// The guard verifies the process that actually launched, so a mismatch can only be known
			// after StartInstance returns. Do not leave that known-wrong server resident: stop it through
			// the engine adapter, whose stop path verifies VRAM release. A cleanup failure is included in
			// the durable queue failure reason rather than hiding the original mode mismatch.
			return false, false, r.rejectWrongModeReplay(ctx, view, gerr)
		}
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
