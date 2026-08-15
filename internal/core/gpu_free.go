package core

import (
	"context"
	"fmt"
	"time"

	"github.com/localhub/localhub/internal/instances"
)

// This file implements the "free the GPU" operator action (PLAN-ui-gpu-rework.md §2.5): stop
// everything LocalHub is running so the card is available again, in one call.
//
// It is a THIN ORCHESTRATION over the existing per-instance stop path. It deliberately does not
// introduce a second way to kill engines: every stop still goes through StopInstance, which
// marks DRAINING, waits, and verifies the VRAM actually came back via a probe delta rather than
// trusting an HTTP 200 (CLAUDE.md correctness bar).

// FreeGPURequest controls which instances a free-GPU sweep is allowed to stop.
type FreeGPURequest struct {
	// IncludePinned also stops instances the operator pinned. Default false: pinning is an
	// explicit "keep this resident" instruction, and a convenience button must not silently
	// override it. The UI should surface how many pinned instances were skipped.
	IncludePinned bool `json:"include_pinned,omitempty"`
}

// FreeGPUInstanceResult is the per-instance outcome of a sweep.
type FreeGPUInstanceResult struct {
	ID      string `json:"id"`
	ModelID string `json:"model_id"`
	// Stopped is false for both skips and failures; Reason says which.
	Stopped bool   `json:"stopped"`
	Reason  string `json:"reason"`
}

// FreeGPUResult is the whole sweep. VRAM numbers are MEASURED before and after, so
// ReclaimedMiB is observed truth rather than a sum of per-instance estimates.
type FreeGPUResult struct {
	Stopped        int `json:"stopped"`
	Skipped        int `json:"skipped"`
	Failed         int `json:"failed"`
	VRAMFreeBefore int `json:"vram_free_before_mib"`
	VRAMFreeAfter  int `json:"vram_free_after_mib"`
	// ReclaimedMiB is after-minus-before on GPU 0. It can be slightly negative if another
	// process grabbed VRAM during the sweep; that is reported honestly, not clamped to 0.
	ReclaimedMiB int                     `json:"reclaimed_mib"`
	Results      []FreeGPUInstanceResult `json:"results"`
	Reason       string                  `json:"reason"`
}

// freeGPUSettleDelay gives the driver a moment to actually release VRAM after the last process
// exits. Without it the "after" reading can still include memory of a process that is gone,
// making a successful sweep look like it reclaimed nothing.
const freeGPUSettleDelay = 1500 * time.Millisecond

// FreeGPU stops running instances so the GPU is available again, and reports how much VRAM
// actually came back.
//
// Stops are sequential, not parallel: concurrent engine shutdowns make the per-stop VRAM-delta
// verification inside StopInstance ambiguous (two processes releasing at once cannot be
// attributed), and this path is operator-initiated where clarity beats a second of latency.
//
// A failure to stop one instance does not abort the sweep — the operator asked for the GPU
// back, so we take what we can get and report the rest.
func (s *Service) FreeGPU(ctx context.Context, req FreeGPURequest) (FreeGPUResult, error) {
	if s.deps.InstanceStore == nil {
		return FreeGPUResult{}, fmt.Errorf("instance store not configured")
	}

	res := FreeGPUResult{VRAMFreeBefore: -1, VRAMFreeAfter: -1}
	res.VRAMFreeBefore = s.gpu0FreeMiB(ctx)

	list, err := s.Instances(ctx)
	if err != nil {
		return FreeGPUResult{}, fmt.Errorf("list instances: %w", err)
	}

	for _, inst := range list {
		// Only states that actually hold VRAM. DRAINING is already on its way out; touching it
		// again would race the in-flight stop that owns it.
		if inst.State != string(instances.StateHealthy) && inst.State != string(instances.StateStarting) {
			continue
		}
		if inst.Pinned && !req.IncludePinned {
			res.Skipped++
			res.Results = append(res.Results, FreeGPUInstanceResult{
				ID: inst.ID, ModelID: inst.ModelID, Stopped: false,
				Reason: "skipped: instance is pinned (send include_pinned=true to stop it too)",
			})
			continue
		}
		view, serr := s.StopInstance(ctx, inst.ID)
		if serr != nil {
			res.Failed++
			res.Results = append(res.Results, FreeGPUInstanceResult{
				ID: inst.ID, ModelID: inst.ModelID, Stopped: false,
				Reason: fmt.Sprintf("stop failed: %v", serr),
			})
			continue
		}
		res.Stopped++
		reason := view.Reason
		if reason == "" {
			reason = "stopped"
		}
		res.Results = append(res.Results, FreeGPUInstanceResult{
			ID: inst.ID, ModelID: inst.ModelID, Stopped: true, Reason: reason,
		})
	}

	if res.Stopped > 0 {
		select {
		case <-time.After(freeGPUSettleDelay):
		case <-ctx.Done():
		}
	}
	res.VRAMFreeAfter = s.gpu0FreeMiB(ctx)
	if res.VRAMFreeBefore >= 0 && res.VRAMFreeAfter >= 0 {
		res.ReclaimedMiB = res.VRAMFreeAfter - res.VRAMFreeBefore
	}

	switch {
	case res.Stopped == 0 && res.Skipped == 0 && res.Failed == 0:
		res.Reason = "nothing to stop: no instances were holding the GPU"
	case res.Stopped == 0:
		// Nothing was stopped, so a zero VRAM delta is EXPECTED — saying "another process may
		// hold the card" here would send the operator chasing a problem that does not exist.
		// Name the real cause: everything was skipped or failed.
		res.Reason = fmt.Sprintf("stopped nothing%s", skippedSuffix(res.Skipped, res.Failed))
	case res.ReclaimedMiB > 0:
		res.Reason = fmt.Sprintf("stopped %d instance(s); free VRAM rose by %d MiB (measured, not assumed)%s",
			res.Stopped, res.ReclaimedMiB, skippedSuffix(res.Skipped, res.Failed))
	default:
		res.Reason = fmt.Sprintf("stopped %d instance(s); no VRAM delta observed — another process may hold the card%s",
			res.Stopped, skippedSuffix(res.Skipped, res.Failed))
	}
	return res, nil
}

// skippedSuffix appends skip/failure counts to a reason so the operator is never left guessing
// why the GPU is still partly occupied after a sweep.
func skippedSuffix(skipped, failed int) string {
	switch {
	case skipped > 0 && failed > 0:
		return fmt.Sprintf("; %d pinned skipped, %d failed", skipped, failed)
	case skipped > 0:
		return fmt.Sprintf("; %d pinned skipped", skipped)
	case failed > 0:
		return fmt.Sprintf("; %d failed", failed)
	}
	return ""
}

// gpu0FreeMiB reads free VRAM on GPU 0, or -1 when there is no hardware probe / no GPU. GPU 0
// matches what the compat engine and admission use (compat/engine.go), so the numbers the
// operator sees here and in a refusal reason describe the same device.
func (s *Service) gpu0FreeMiB(ctx context.Context) int {
	if s.deps.Hardware == nil {
		return -1
	}
	snap, err := s.deps.Hardware.Snapshot(ctx)
	if err != nil || len(snap.GPUs) == 0 {
		return -1
	}
	return snap.GPUs[0].VRAMFreeMiB
}
