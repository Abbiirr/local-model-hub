package llamacpp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/localhub/localhub/internal/instances"
	"github.com/localhub/localhub/internal/instances/proc"
)

// StopResult carries the VRAM-delta evidence for a stop (the P4.2 gate).
type StopResult struct {
	Instance      instances.Instance
	FreeBeforeMiB int  // free VRAM just before the stop
	FreeAfterMiB  int  // free VRAM after reclaim settled
	FreedMiB      int  // FreeAfterMiB - FreeBeforeMiB (the measured reclaim)
	FootprintMiB  int  // the instance's observed/estimated footprint we expected to reclaim
	Verified      bool // freed >= footprint - slack
	TimedOut      bool // reclaim did not reach the bar within StopVerifyTimeout
	Reason        string
}

// Stop gracefully stops an instance and VERIFIES the VRAM reclaim. Sequence (spec-digest §5,
// TESTING.md#p42, CLAUDE.md correctness bar):
//  1. Mark DRAINING (from HEALTHY) — the never-kill-active gate lives in residency; a direct
//     admin stop still passes through DRAINING for the audit trail.
//  2. Read free VRAM (before).
//  3. proc.Stop: taskkill graceful → wait 5s → Job Object hard kill (the lmwrapper rule).
//  4. Poll free VRAM until it rises by >= footprint - slack (500 MiB) OR 30s elapses.
//  5. Persist STOPPED with the measured vram_freed_mib. An HTTP-200-based stop claim is
//     FORBIDDEN — this only reports success when the probe delta clears the bar (or records the
//     timeout honestly).
func (r *Runner) Stop(ctx context.Context, instanceID string) (StopResult, error) {
	inst, err := r.store.Get(ctx, instanceID)
	if err != nil {
		return StopResult{}, err
	}
	if inst.State.Terminal() {
		return StopResult{Instance: inst, Verified: true, Reason: "instance already " + string(inst.State)}, nil
	}

	// Footprint we expect to reclaim: observed peak if we have it, else the estimate.
	footprint := inst.VRAMObsMiB
	if footprint == 0 {
		footprint = inst.VRAMEstMiB
	}

	// 1. DRAINING (only from HEALTHY; if STARTING we go straight to stop below).
	if inst.State == instances.StateHealthy {
		if _, terr := r.store.Transition(ctx, instanceID, instances.TransitionParams{
			To:     instances.StateDraining,
			Reason: "stop requested; draining before process stop",
		}); terr != nil {
			// Non-fatal: proceed to stop anyway (a concurrent state change), but surface it.
			_ = terr
		}
	}

	// 2. Free VRAM before.
	freeBefore, _ := r.vram.FreeVRAMMiB(ctx)

	// 3. Stop the process (graceful → hard via Job Object).
	live, ok := r.live.get(instanceID)
	if ok && live.proc != nil {
		_ = live.proc.Stop(ctx)
	} else if inst.PID != 0 {
		// No live handle (e.g. a reattached instance from before a restart): kill by PID tree.
		_ = proc.KillTree(inst.PID)
	}

	// 4. Poll for the VRAM reclaim.
	freeAfter, freed, verified, timedOut := r.waitReclaim(ctx, freeBefore, footprint)

	// 5. Persist STOPPED with the measured delta.
	freedForRow := freed
	if freedForRow < 0 {
		freedForRow = 0
	}
	reason := r.stopReason(freed, footprint, verified, timedOut)
	stopped, serr := r.store.Transition(ctx, instanceID, instances.TransitionParams{
		To:           instances.StateStopped,
		Reason:       reason,
		VRAMFreedMiB: &freedForRow,
	})
	if serr != nil {
		return StopResult{}, fmt.Errorf("mark stopped: %w", serr)
	}

	// Free the port + clear the live handle + close the log.
	_ = r.pool.Free(ctx, inst.Port)
	if ok {
		_ = live.logFile.Close()
	}
	r.live.remove(instanceID)

	return StopResult{
		Instance:      stopped,
		FreeBeforeMiB: freeBefore,
		FreeAfterMiB:  freeAfter,
		FreedMiB:      freed,
		FootprintMiB:  footprint,
		Verified:      verified,
		TimedOut:      timedOut,
		Reason:        reason,
	}, nil
}

// waitReclaim polls free VRAM until it rises by >= footprint - slack, or StopVerifyTimeout
// elapses. Returns (freeAfter, freed, verified, timedOut). When footprint is 0 (unknown), the
// bar is any positive reclaim (freed > 0) — we still require the probe to move, never trusting a
// bare process exit.
func (r *Runner) waitReclaim(ctx context.Context, freeBefore, footprint int) (freeAfter, freed int, verified, timedOut bool) {
	bar := footprint - r.cfg.StopVRAMDeltaSlackMiB
	if footprint == 0 {
		bar = 1 // any measurable reclaim
	}
	if bar < 1 {
		bar = 1
	}
	deadline := time.Now().Add(r.cfg.StopVerifyTimeout)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	freeAfter = freeBefore
	for {
		cur, err := r.vram.FreeVRAMMiB(ctx)
		if err == nil {
			if cur > freeAfter {
				freeAfter = cur
			}
			freed = freeAfter - freeBefore
			if freed >= bar {
				return freeAfter, freed, true, false
			}
		}
		if time.Now().After(deadline) {
			return freeAfter, freeAfter - freeBefore, false, true
		}
		select {
		case <-ctx.Done():
			return freeAfter, freeAfter - freeBefore, false, true
		case <-tick.C:
		}
	}
}

func (r *Runner) stopReason(freed, footprint int, verified, timedOut bool) string {
	switch {
	case verified && footprint > 0:
		return fmt.Sprintf("stopped and VERIFIED: free VRAM rose by %d MiB (>= footprint %d − %d slack) — process gone and VRAM reclaimed (NVML delta, not an HTTP-200 claim)",
			freed, footprint, r.cfg.StopVRAMDeltaSlackMiB)
	case verified:
		return fmt.Sprintf("stopped and VERIFIED: free VRAM rose by %d MiB (footprint was unknown; any positive reclaim required) — process gone and VRAM reclaimed",
			freed)
	case timedOut:
		return fmt.Sprintf("stopped: process killed, but VRAM reclaim of %d MiB did not reach the expected %d MiB within %s — reclaim may be lagging (engines free asynchronously) or the footprint estimate was high; re-probe to confirm",
			freed, footprint-r.cfg.StopVRAMDeltaSlackMiB, r.cfg.StopVerifyTimeout)
	default:
		return fmt.Sprintf("stopped: measured VRAM reclaim %d MiB", freed)
	}
}

// RecordPeakVRAM samples free VRAM over PeakSampleWindow while the instance is HEALTHY, computes
// the peak footprint (baseline_free_before_load − min_free_observed is NOT available post-hoc, so
// we use the recorded pre-start free if given; here we use the DROP from the current free to the
// lowest free seen as the footprint proxy). The caller supplies freeBeforeLoad (free VRAM just
// before Start) so the footprint is (freeBeforeLoad − minFreeDuringWindow). It returns the peak
// footprint MiB and persists it on the instance row (vram_bytes_obs).
//
// This is the calibration read (P2.5 loop): the returned peak is what the caller passes to
// RecordObservedPeak so future verdicts prefer the measured number.
func (r *Runner) RecordPeakVRAM(ctx context.Context, instanceID string, freeBeforeLoad int) (int, error) {
	minFree := freeBeforeLoad
	deadline := time.Now().Add(r.cfg.PeakSampleWindow)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		cur, err := r.vram.FreeVRAMMiB(ctx)
		if err == nil && cur < minFree {
			minFree = cur
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-tick.C:
		}
	}
	peak := freeBeforeLoad - minFree
	if peak < 0 {
		peak = 0
	}
	if peak > 0 {
		_ = r.store.SetObservedPeak(ctx, instanceID, peak)
	}
	return peak, nil
}

// TinyCompletion sends a <=maxTokens completion to trigger full VRAM allocation (KV cache,
// CUDA graphs) so the peak sample captures the real footprint (P4.2: "one tiny completion <=20
// tokens to trigger full allocation"). It POSTs the llama.cpp /completion endpoint with a
// trivial prompt. Best-effort: an error is returned but the caller may proceed (the health
// check already proved the model loaded).
func (r *Runner) TinyCompletion(ctx context.Context, baseURL string, maxTokens int) error {
	if maxTokens <= 0 || maxTokens > 20 {
		maxTokens = 16
	}
	body := fmt.Sprintf(`{"prompt":"Hello","n_predict":%d,"temperature":0,"stream":false}`, maxTokens)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := newJSONPost(cctx, baseURL+"/completion", body)
	if err != nil {
		return err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("tiny completion returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// --- liveProcesses: track running proc handles by instance id ---

type liveEntry struct {
	proc    proc.Process
	ring    *instances.RingBuffer
	logFile *instances.CappedLogFile
}

type liveProcesses struct {
	mu sync.Mutex
	m  map[string]*liveEntry
}

func newLiveProcesses() *liveProcesses { return &liveProcesses{m: map[string]*liveEntry{}} }

func (l *liveProcesses) put(id string, p proc.Process, ring *instances.RingBuffer, logFile *instances.CappedLogFile) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[id] = &liveEntry{proc: p, ring: ring, logFile: logFile}
}

func (l *liveProcesses) get(id string) (*liveEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[id]
	return e, ok
}

func (l *liveProcesses) remove(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, id)
}

// Ring returns the live ring buffer for an instance (for the logs tail), if tracked.
func (r *Runner) Ring(instanceID string) (*instances.RingBuffer, bool) {
	e, ok := r.live.get(instanceID)
	if !ok {
		return nil, false
	}
	return e.ring, true
}
