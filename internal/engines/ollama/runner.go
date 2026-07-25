// This file is the P4.3 owned-lifecycle half of the Ollama adapter — the analogue of the
// llama.cpp Runner, but for a SHARED daemon. Where the llama.cpp Runner spawns and kills a
// process, the Ollama Runner WARMS and UNLOADS a model in the daemon we do not own: it drives the
// same internal/instances state machine (REQUESTED→STARTING→HEALTHY on warm; HEALTHY→DRAINING→
// STOPPED on unload) so the residency layer, dashboard, and reconciler treat Ollama models like
// any other instance. It NEVER spawns or kills the daemon process.
//
// The stop path enforces the same honesty bar as llama.cpp: keep_alive:0 is only a REQUEST; the
// Runner polls /api/ps until the model is gone AND then verifies the actual VRAM delta via the
// hardware probe (residency-scheduler.md re-measure rule; CLAUDE.md: never trust an API 200).
package ollama

import (
	"context"
	"fmt"
	"time"

	"github.com/localhub/localhub/internal/instances"
)

// VRAMProbe is the minimal hardware seam the Runner needs for the stop-verify delta: current free
// VRAM (MiB) on GPU 0. Satisfied by the same wrapper llama.cpp uses (a small adapter over
// hardware.Probe). A seam so tests can script rising VRAM without a real GPU.
type VRAMProbe interface {
	FreeVRAMMiB(ctx context.Context) (int, error)
}

// RunnerStore is the subset of *instances.Store the Runner needs (create + transition + get).
type RunnerStore interface {
	Create(ctx context.Context, p instances.CreateParams) (instances.Instance, error)
	Transition(ctx context.Context, id string, p instances.TransitionParams) (instances.Instance, error)
	Get(ctx context.Context, id string) (instances.Instance, error)
}

// RunnerConfig tunes the Runner's warm/unload behavior.
type RunnerConfig struct {
	// DefaultKeepAlive is the keep_alive sent on a warm when the caller does not override it
	// (empty => the daemon's own default). A pinned start sends "-1".
	DefaultKeepAlive string
	// WarmHealthTimeout bounds how long we wait for a warmed model to appear in /api/ps before
	// declaring the warm HEALTHY (default 60s: an Ollama cold load can take a while).
	WarmHealthTimeout time.Duration
	// WarmPollInterval is the /api/ps cadence while waiting for the warm to land (default 500ms).
	WarmPollInterval time.Duration
	// Unload configures the keep_alive:0 poll (see UnloadConfig).
	Unload UnloadConfig
	// StopVerifyTimeout bounds the VRAM-reclaim poll after the model leaves /api/ps (default 15s).
	StopVerifyTimeout time.Duration
	// StopVRAMDeltaSlackMiB is subtracted from the footprint to set the reclaim bar (default 500).
	StopVRAMDeltaSlackMiB int
}

func (c RunnerConfig) withDefaults() RunnerConfig {
	if c.WarmHealthTimeout <= 0 {
		c.WarmHealthTimeout = 60 * time.Second
	}
	if c.WarmPollInterval <= 0 {
		c.WarmPollInterval = 500 * time.Millisecond
	}
	if c.StopVerifyTimeout <= 0 {
		c.StopVerifyTimeout = 15 * time.Second
	}
	if c.StopVRAMDeltaSlackMiB <= 0 {
		c.StopVRAMDeltaSlackMiB = 500
	}
	c.Unload = c.Unload.withDefaults()
	return c
}

// Runner drives Ollama model warm/unload against the instance state machine. One per daemon.
type Runner struct {
	client *Client
	store  RunnerStore
	vram   VRAMProbe
	cfg    RunnerConfig
}

// NewRunner builds an Ollama Runner. vram may be nil (the stop still confirms the model left
// /api/ps, but skips the probe-delta verification — recorded honestly in the reason).
func NewRunner(client *Client, store RunnerStore, vram VRAMProbe, cfg RunnerConfig) *Runner {
	return &Runner{client: client, store: store, vram: vram, cfg: cfg.withDefaults()}
}

// WarmSpec is everything the Runner needs to warm one Ollama model.
type WarmSpec struct {
	InstanceID string // caller-supplied uuid (the instances row id)
	ModelID    string // LocalHub catalog model id (instances.model_id, FK to models)
	OllamaTag  string // the Ollama model tag to warm (e.g. "mistral:latest")
	KeepAlive  string // override keep_alive; empty => RunnerConfig.DefaultKeepAlive
	Pinned     bool   // pin => keep_alive:-1 (overrides KeepAlive)
	Priority   int
	TTLSeconds int
}

// Warm creates the REQUESTED→STARTING→HEALTHY lifecycle for an Ollama model: it POSTs the warm
// (keep_alive) to the daemon, then POLLS /api/ps until the model is resident (HEALTHY) or a
// timeout (FAILED). It records size_vram as the observed VRAM on HEALTHY (calibration input). It
// NEVER spawns a process. A daemon-down condition fails the start with a typed reason.
func (r *Runner) Warm(ctx context.Context, spec WarmSpec) (instances.Instance, error) {
	if spec.OllamaTag == "" {
		return instances.Instance{}, fmt.Errorf("ollama warm: OllamaTag is required")
	}
	keepAlive := spec.KeepAlive
	if keepAlive == "" {
		keepAlive = r.cfg.DefaultKeepAlive
	}
	if spec.Pinned {
		keepAlive = "-1"
	}

	// 1. REQUESTED row.
	base := r.client.BaseURL
	if _, err := r.store.Create(ctx, instances.CreateParams{
		ID:         spec.InstanceID,
		ModelID:    spec.ModelID,
		Engine:     instances.EngineOllama,
		Pinned:     spec.Pinned,
		Priority:   spec.Priority,
		TTLSeconds: spec.TTLSeconds,
		Reason:     fmt.Sprintf("warm requested for Ollama model %q (keep_alive=%q)", spec.OllamaTag, keepAlive),
	}); err != nil {
		return instances.Instance{}, fmt.Errorf("create instance: %w", err)
	}

	// 2. STARTING (pid=0, port=0, base_url=daemon).
	zero := 0
	if _, err := r.store.Transition(ctx, spec.InstanceID, instances.TransitionParams{
		To: instances.StateStarting, Reason: "warming model in Ollama daemon (shared daemon; no process spawned)",
		PID: &zero, Port: &zero, BaseURL: &base,
	}); err != nil {
		return instances.Instance{}, err
	}

	// 3. Send the warm request.
	if _, err := r.client.Warm(ctx, spec.OllamaTag, keepAlive); err != nil {
		r.failWarm(ctx, spec.InstanceID, "warm request failed: "+err.Error())
		return instances.Instance{}, err
	}

	// 4. Poll /api/ps until the model is resident.
	vramMiB, ok := r.waitResident(ctx, spec.OllamaTag)
	if !ok {
		reason := fmt.Sprintf("model %q did not appear in /api/ps within %s after warm — the daemon may lack VRAM to load it, or the tag is wrong",
			spec.OllamaTag, r.cfg.WarmHealthTimeout)
		r.failWarm(ctx, spec.InstanceID, reason)
		return instances.Instance{}, fmt.Errorf("ollama warm did not become resident: %s", reason)
	}

	// 5. HEALTHY (record observed VRAM from size_vram — the calibration input).
	healthy, err := r.store.Transition(ctx, spec.InstanceID, instances.TransitionParams{
		To:         instances.StateHealthy,
		Reason:     fmt.Sprintf("model %q resident in Ollama /api/ps (size_vram %d MiB); warm confirmed", spec.OllamaTag, vramMiB),
		VRAMObsMiB: &vramMiB,
	})
	if err != nil {
		return instances.Instance{}, err
	}
	return healthy, nil
}

// OllamaTagFromReason is not needed; Stop reads the tag from the caller. StopSpec carries it.

// StopResult carries the VRAM-delta evidence for an Ollama unload (the P4.3 gate).
type StopResult struct {
	Instance      instances.Instance
	Gone          bool // model left /api/ps
	FreeBeforeMiB int
	FreeAfterMiB  int
	FreedMiB      int
	FootprintMiB  int
	Verified      bool // freed >= footprint - slack (probe delta), OR probe absent + Gone
	TimedOut      bool // reclaim did not reach the bar within StopVerifyTimeout
	Reason        string
}

// Stop unloads an Ollama model (keep_alive:0), polls /api/ps until it is gone, then verifies the
// VRAM reclaim via the probe. It drives HEALTHY→DRAINING→STOPPED. It NEVER kills the daemon. The
// ollamaTag is the model to unload (the caller resolves it from the instance's model_id).
func (r *Runner) Stop(ctx context.Context, instanceID, ollamaTag string) (StopResult, error) {
	inst, err := r.store.Get(ctx, instanceID)
	if err != nil {
		return StopResult{}, err
	}
	if inst.State.Terminal() {
		return StopResult{Instance: inst, Verified: true, Gone: true, Reason: "instance already " + string(inst.State)}, nil
	}

	footprint := inst.VRAMObsMiB
	if footprint == 0 {
		footprint = inst.VRAMEstMiB
	}

	// DRAINING (only from HEALTHY) for the audit trail.
	if inst.State == instances.StateHealthy {
		_, _ = r.store.Transition(ctx, instanceID, instances.TransitionParams{
			To: instances.StateDraining, Reason: "unload requested; draining before Ollama keep_alive:0",
		})
	}

	freeBefore := 0
	if r.vram != nil {
		freeBefore, _ = r.vram.FreeVRAMMiB(ctx)
	}

	// keep_alive:0 unload + poll /api/ps until gone.
	ur, err := r.client.Unload(ctx, ollamaTag, r.cfg.Unload)
	if err != nil {
		return StopResult{}, err
	}
	if ur.FreedVRAMHint > 0 {
		footprint = int(ur.FreedVRAMHint / bytesPerMiB)
	}

	// Verify the VRAM delta (only meaningful once the model is gone and a probe exists).
	freeAfter, freed, verified, timedOut := freeBefore, 0, false, false
	if r.vram != nil && ur.Gone {
		freeAfter, freed, verified, timedOut = r.waitReclaim(ctx, freeBefore, footprint)
	} else if r.vram == nil {
		// No probe: the honest signal is "model left /api/ps" (Gone). Mark verified iff Gone.
		verified = ur.Gone
	}

	freedForRow := freed
	if freedForRow < 0 {
		freedForRow = 0
	}
	reason := r.stopReason(ur, freed, footprint, verified, timedOut)
	stopped, serr := r.store.Transition(ctx, instanceID, instances.TransitionParams{
		To: instances.StateStopped, Reason: reason, VRAMFreedMiB: &freedForRow,
	})
	if serr != nil {
		return StopResult{}, fmt.Errorf("mark stopped: %w", serr)
	}

	return StopResult{
		Instance: stopped, Gone: ur.Gone, FreeBeforeMiB: freeBefore, FreeAfterMiB: freeAfter,
		FreedMiB: freed, FootprintMiB: footprint, Verified: verified, TimedOut: timedOut, Reason: reason,
	}, nil
}

// waitResident polls /api/ps until the model appears (returns its size_vram in MiB) or the warm
// timeout elapses (returns ok=false).
func (r *Runner) waitResident(ctx context.Context, tag string) (vramMiB int, ok bool) {
	deadline := time.Now().Add(r.cfg.WarmHealthTimeout)
	tick := time.NewTicker(r.cfg.WarmPollInterval)
	defer tick.Stop()
	for {
		if resident, ps, err := r.client.IsResident(ctx, tag); err == nil && resident {
			return int(ps.SizeVRAM / bytesPerMiB), true
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		select {
		case <-ctx.Done():
			return 0, false
		case <-tick.C:
		}
	}
}

// waitReclaim polls free VRAM until it rises by >= footprint - slack, or StopVerifyTimeout
// elapses. Mirrors the llama.cpp Runner's bar. When footprint is 0 (unknown) any positive reclaim
// counts (the model is already confirmed gone from /api/ps).
func (r *Runner) waitReclaim(ctx context.Context, freeBefore, footprint int) (freeAfter, freed int, verified, timedOut bool) {
	bar := footprint - r.cfg.StopVRAMDeltaSlackMiB
	if footprint == 0 {
		bar = 1
	}
	if bar < 1 {
		bar = 1
	}
	deadline := time.Now().Add(r.cfg.StopVerifyTimeout)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	freeAfter = freeBefore
	for {
		if cur, err := r.vram.FreeVRAMMiB(ctx); err == nil {
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

func (r *Runner) stopReason(ur UnloadResult, freed, footprint int, verified, timedOut bool) string {
	if !ur.Gone {
		return "unload requested (keep_alive:0) but " + ur.Reason
	}
	switch {
	case r.vram == nil:
		return fmt.Sprintf("unloaded: model left /api/ps after %s (no hardware probe wired — VRAM release requested; the daemon reports it gone)", ur.WaitedFor.Round(time.Millisecond))
	case verified && footprint > 0:
		return fmt.Sprintf("unloaded and VERIFIED: model left /api/ps and free VRAM rose by %d MiB (>= footprint %d − %d slack) — VRAM reclaimed (probe delta, not an HTTP-200 claim)",
			freed, footprint, r.cfg.StopVRAMDeltaSlackMiB)
	case verified:
		return fmt.Sprintf("unloaded and VERIFIED: model left /api/ps and free VRAM rose by %d MiB (footprint unknown; any positive reclaim required)", freed)
	case timedOut:
		return fmt.Sprintf("unloaded: model left /api/ps but the VRAM reclaim of %d MiB did not reach the expected %d MiB within %s — Ollama frees asynchronously; re-probe to confirm",
			freed, footprint-r.cfg.StopVRAMDeltaSlackMiB, r.cfg.StopVerifyTimeout)
	default:
		return fmt.Sprintf("unloaded: model left /api/ps; measured VRAM reclaim %d MiB", freed)
	}
}

func (r *Runner) failWarm(ctx context.Context, id, reason string) {
	_, _ = r.store.Transition(ctx, id, instances.TransitionParams{To: instances.StateFailed, Reason: reason})
}
