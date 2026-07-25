// This file is the P4.4 MUTATION path for the vLLM adapter — CONTRACT level. It drives the
// internal/instances state machine for three launch modes (PLAN.md resolution 4):
//
//   - docker (Windows/anywhere with docker CLI): `docker run -d` a vllm/vllm-openai container,
//     poll GET /health until ready (cold start can take MINUTES — model load + CUDA graph
//     capture), then HEALTHY. Stop = `docker stop` → `docker rm -f`, VERIFY the container is gone
//     (`docker ps`) AND the VRAM delta via the probe. No real container runs in tests — the
//     DockerClient seam is faked; the real E2E is P4.5.
//   - attach (adopt a pre-existing vLLM endpoint): health-only. Stop = DETACH (mark STOPPED),
//     never kills the endpoint — LocalHub does not own it.
//   - subprocess (Linux future, `vllm serve`): the command is BUILT and unit-tested, but Start
//     returns a typed *NotImplementedError on Windows (vLLM has no native Windows support).
//
// [RE-VERIFY 2026-07-06] Health = GET /health (200 when ready); readiness cross-check = GET
// /v1/models (lists the served model once fully loaded). Verified against docs.vllm.ai. The
// health timeout defaults to 600s for vLLM (cold start is slow) — from config.
package vllm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/localhub/localhub/internal/instances"
)

// VRAMProbe is the hardware seam for the docker-mode stop VRAM-delta gate (GPU 0 free MiB).
type VRAMProbe interface {
	FreeVRAMMiB(ctx context.Context) (int, error)
}

// RunnerStore is the subset of *instances.Store the vLLM runner needs.
type RunnerStore interface {
	Create(ctx context.Context, p instances.CreateParams) (instances.Instance, error)
	Transition(ctx context.Context, id string, p instances.TransitionParams) (instances.Instance, error)
	Get(ctx context.Context, id string) (instances.Instance, error)
}

// NotImplementedError is returned when a mode is not runnable on the current OS (subprocess on
// Windows). Typed + carries a Reason (CLAUDE.md correctness bar).
type NotImplementedError struct {
	Mode   string
	OS     string
	Reason string
}

func (e *NotImplementedError) Error() string { return e.Reason }

// RunnerConfig tunes the vLLM runner.
type RunnerConfig struct {
	// HealthTimeout bounds STARTING→HEALTHY. vLLM cold start (weight load + CUDA graph capture)
	// can take minutes; default 600s (config default for vLLM).
	HealthTimeout time.Duration
	// HealthInterval is the /health poll cadence (default 2s).
	HealthInterval time.Duration
	// StopVerifyTimeout bounds the container-gone + VRAM-reclaim poll after stop (default 30s).
	StopVerifyTimeout time.Duration
	// StopVRAMDeltaSlackMiB is subtracted from the footprint to set the reclaim bar (default 500).
	StopVRAMDeltaSlackMiB int
	// Image overrides the docker image (default DefaultImage).
	Image string
	// DefaultGPUMemUtil is the --gpu-memory-utilization used when a start does not specify one.
	DefaultGPUMemUtil float64
}

func (c RunnerConfig) withDefaults() RunnerConfig {
	if c.HealthTimeout <= 0 {
		c.HealthTimeout = 600 * time.Second
	}
	if c.HealthInterval <= 0 {
		c.HealthInterval = 2 * time.Second
	}
	if c.StopVerifyTimeout <= 0 {
		c.StopVerifyTimeout = 30 * time.Second
	}
	if c.StopVRAMDeltaSlackMiB <= 0 {
		c.StopVRAMDeltaSlackMiB = 500
	}
	if c.Image == "" {
		c.Image = DefaultImage
	}
	return c
}

// Runner drives vLLM instance lifecycle in docker/attach/subprocess mode. One per daemon.
type Runner struct {
	mode   Mode
	store  RunnerStore
	docker DockerClient // nil in attach mode
	vram   VRAMProbe
	cfg    RunnerConfig
	http   *http.Client
	// osOverride lets tests exercise the subprocess-on-Windows guard without being on Windows.
	osOverride string
}

// NewRunner builds a vLLM Runner. docker may be nil for attach mode. vram may be nil (docker stop
// still verifies the container is gone, but records the VRAM check as skipped honestly).
func NewRunner(mode Mode, store RunnerStore, docker DockerClient, vram VRAMProbe, cfg RunnerConfig) *Runner {
	return &Runner{
		mode: mode, store: store, docker: docker, vram: vram, cfg: cfg.withDefaults(),
		http: &http.Client{},
	}
}

// StartSpec is everything a vLLM start needs.
type StartSpec struct {
	InstanceID  string
	ModelID     string // catalog model id (instances.model_id)
	Model       string // --model value (HF repo id or local path); attach mode ignores it
	AttachURL   string // attach mode: the pre-existing endpoint (e.g. http://127.0.0.1:8000)
	HostPort    int    // docker/subprocess: host port to bind (localhost)
	GPUMemUtil  float64
	MaxModelLen int
	Dtype       string
	ExtraArgs   []string
	// ExtraDockerArgs are docker-level args (mounts, env) inserted before the image (docker mode).
	ExtraDockerArgs []string
	VRAMEstMiB      int
	Pinned      bool
	Priority    int
	TTLSeconds  int
}

// Start launches (or adopts) a vLLM instance and drives it to HEALTHY. Dispatches by mode.
func (r *Runner) Start(ctx context.Context, spec StartSpec) (instances.Instance, error) {
	switch r.mode {
	case ModeDocker:
		return r.startDocker(ctx, spec)
	case ModeAttach:
		return r.startAttach(ctx, spec)
	case ModeSubprocess:
		return r.startSubprocess(ctx, spec)
	default:
		return instances.Instance{}, fmt.Errorf("vllm start: unsupported mode %q (want docker|attach|subprocess)", r.mode)
	}
}

// --- docker mode ---

func (r *Runner) startDocker(ctx context.Context, spec StartSpec) (instances.Instance, error) {
	if r.docker == nil {
		return instances.Instance{}, fmt.Errorf("vllm docker start: no docker client configured")
	}
	if spec.HostPort <= 0 {
		return instances.Instance{}, fmt.Errorf("vllm docker start: HostPort is required")
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", spec.HostPort)
	gmu := spec.GPUMemUtil
	if gmu <= 0 {
		gmu = r.cfg.DefaultGPUMemUtil
	}
	params := LaunchParams{
		InstanceID: spec.InstanceID, Model: spec.Model, HostPort: spec.HostPort,
		GPUMemUtil: gmu, MaxModelLen: spec.MaxModelLen, Dtype: spec.Dtype,
		Image: r.cfg.Image, ExtraArgs: spec.ExtraArgs, ExtraDockerArgs: spec.ExtraDockerArgs,
	}
	runArgs := DockerRunArgs(params)
	fullFlags := append([]string{"docker"}, runArgs...)

	// 1. REQUESTED.
	if _, err := r.store.Create(ctx, instances.CreateParams{
		ID: spec.InstanceID, ModelID: spec.ModelID, Engine: instances.EngineVLLM,
		ModelPath: spec.Model, BinaryPath: r.cfg.Image, VRAMEstMiB: spec.VRAMEstMiB,
		Pinned: spec.Pinned, Priority: spec.Priority, TTLSeconds: spec.TTLSeconds,
		Reason: fmt.Sprintf("vLLM (docker) instance requested for model %q", spec.Model),
	}); err != nil {
		return instances.Instance{}, fmt.Errorf("create instance: %w", err)
	}

	// 2. docker run -d.
	if _, err := r.docker.Run(ctx, runArgs...); err != nil {
		r.fail(ctx, spec.InstanceID, "docker run failed: "+err.Error())
		return instances.Instance{}, fmt.Errorf("vllm docker run: %w", err)
	}

	// 3. STARTING (record base_url + the exact docker command for reproducibility).
	zero := 0
	port := spec.HostPort
	if _, err := r.store.Transition(ctx, spec.InstanceID, instances.TransitionParams{
		To:          instances.StateStarting,
		Reason:      fmt.Sprintf("vLLM container %s started; polling %s/health (cold start can take minutes — model load + CUDA graph capture)", ContainerName(spec.InstanceID), baseURL),
		PID:         &zero, // container, not a tracked PID
		Port:        &port,
		BaseURL:     &baseURL,
		LaunchFlags: fullFlags,
	}); err != nil {
		_, _ = r.docker.Run(ctx, DockerRmArgs(spec.InstanceID)...)
		r.fail(ctx, spec.InstanceID, "state error at starting: "+err.Error())
		return instances.Instance{}, err
	}

	// 4. Poll /health until ready or timeout. A best-effort docker-logs progress note is folded
	//    into the reason on timeout.
	if err := r.waitHealthy(ctx, baseURL); err != nil {
		logs, _ := r.docker.Run(ctx, DockerLogsTailArgs(spec.InstanceID, 20)...)
		reason := "vLLM /health did not become ready: " + err.Error()
		if tail := strings.TrimSpace(logs); tail != "" {
			reason += " — last container logs:\n" + tailLines(tail, 15)
		}
		_, _ = r.docker.Run(ctx, DockerRmArgs(spec.InstanceID)...) // kill the container
		r.fail(ctx, spec.InstanceID, reason)
		return instances.Instance{}, fmt.Errorf("vllm container did not become healthy: %w", err)
	}

	// 5. HEALTHY.
	healthy, err := r.store.Transition(ctx, spec.InstanceID, instances.TransitionParams{
		To:     instances.StateHealthy,
		Reason: fmt.Sprintf("vLLM container %s answered /health OK on %s (/v1/models cross-check passed)", ContainerName(spec.InstanceID), baseURL),
	})
	if err != nil {
		_, _ = r.docker.Run(ctx, DockerRmArgs(spec.InstanceID)...)
		r.fail(ctx, spec.InstanceID, "state error at healthy: "+err.Error())
		return instances.Instance{}, err
	}
	return healthy, nil
}

// --- attach mode ---

func (r *Runner) startAttach(ctx context.Context, spec StartSpec) (instances.Instance, error) {
	base := strings.TrimRight(spec.AttachURL, "/")
	if base == "" {
		return instances.Instance{}, fmt.Errorf("vllm attach start: AttachURL is required")
	}
	if _, err := r.store.Create(ctx, instances.CreateParams{
		ID: spec.InstanceID, ModelID: spec.ModelID, Engine: instances.EngineVLLM,
		VRAMEstMiB: spec.VRAMEstMiB, Pinned: spec.Pinned, Priority: spec.Priority,
		Reason: fmt.Sprintf("adopting pre-existing vLLM endpoint %s (attach; LocalHub does not own it)", base),
	}); err != nil {
		return instances.Instance{}, fmt.Errorf("create instance: %w", err)
	}
	zero := 0
	if _, err := r.store.Transition(ctx, spec.InstanceID, instances.TransitionParams{
		To: instances.StateStarting, Reason: "attaching to existing vLLM endpoint; checking /health",
		PID: &zero, Port: &zero, BaseURL: &base,
	}); err != nil {
		return instances.Instance{}, err
	}
	// Health-only: a short timeout (the endpoint is supposed to be already up).
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := r.waitHealthyDeadline(hctx, base, 15*time.Second); err != nil {
		r.fail(ctx, spec.InstanceID, "attach target /health did not answer: "+err.Error())
		return instances.Instance{}, fmt.Errorf("vllm attach: endpoint not healthy: %w", err)
	}
	return r.store.Transition(ctx, spec.InstanceID, instances.TransitionParams{
		To: instances.StateHealthy, Reason: fmt.Sprintf("attached: vLLM endpoint %s /health OK (adopted, not owned)", base),
	})
}

// --- subprocess mode ---

func (r *Runner) startSubprocess(ctx context.Context, spec StartSpec) (instances.Instance, error) {
	goos := r.osOverride
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos == "windows" {
		return instances.Instance{}, &NotImplementedError{
			Mode: "subprocess", OS: goos,
			Reason: "vllm subprocess mode (vllm serve) is not implemented on Windows — vLLM has no native Windows support (PLAN.md resolution 4). Use mode=docker on this box; subprocess is a Linux-only future path.",
		}
	}
	// On Linux this would spawn `vllm serve` via internal/instances/proc. That spawn is out of
	// scope for P4.4 (contract level, no real vLLM); the command is built + tested (SubprocessArgs)
	// and the real Linux spawn lands with the Linux gate. Return notImplemented with the built
	// command in the reason so the intent is auditable.
	args := SubprocessArgs(LaunchParams{
		Model: spec.Model, HostPort: spec.HostPort, GPUMemUtil: spec.GPUMemUtil,
		MaxModelLen: spec.MaxModelLen, Dtype: spec.Dtype, ExtraArgs: spec.ExtraArgs,
	})
	return instances.Instance{}, &NotImplementedError{
		Mode: "subprocess", OS: goos,
		Reason: "vllm subprocess spawn is not wired in P4.4 (contract level). Command would be: vllm " + strings.Join(args, " "),
	}
}

// StopResult carries the docker-mode stop evidence (container gone + VRAM delta).
type StopResult struct {
	Instance      instances.Instance
	ContainerGone bool
	FreeBeforeMiB int
	FreeAfterMiB  int
	FreedMiB      int
	FootprintMiB  int
	Verified      bool
	TimedOut      bool
	Reason        string
}

// Stop stops a vLLM instance. Docker: `docker stop` → `docker rm -f`, verify the container is gone
// via `docker ps` AND the VRAM delta via the probe. Attach: DETACH only (mark STOPPED; never kills
// the endpoint). Subprocess: notImplemented on Windows.
func (r *Runner) Stop(ctx context.Context, instanceID string) (StopResult, error) {
	inst, err := r.store.Get(ctx, instanceID)
	if err != nil {
		return StopResult{}, err
	}
	if inst.State.Terminal() {
		return StopResult{Instance: inst, Verified: true, ContainerGone: true, Reason: "instance already " + string(inst.State)}, nil
	}

	switch r.mode {
	case ModeAttach:
		return r.stopAttach(ctx, inst)
	case ModeDocker:
		return r.stopDocker(ctx, inst)
	case ModeSubprocess:
		return StopResult{}, &NotImplementedError{Mode: "subprocess", OS: runtime.GOOS, Reason: "vllm subprocess stop not implemented on this OS"}
	default:
		return StopResult{}, fmt.Errorf("vllm stop: unsupported mode %q", r.mode)
	}
}

func (r *Runner) stopAttach(ctx context.Context, inst instances.Instance) (StopResult, error) {
	// Detach only: never issue docker or kill commands against an endpoint we do not own.
	if inst.State == instances.StateHealthy {
		_, _ = r.store.Transition(ctx, inst.ID, instances.TransitionParams{
			To: instances.StateDraining, Reason: "detaching from attached vLLM endpoint",
		})
	}
	reason := fmt.Sprintf("detached from vLLM endpoint %s (attach mode; the endpoint is NOT owned by LocalHub and was left running)", inst.BaseURL)
	stopped, err := r.store.Transition(ctx, inst.ID, instances.TransitionParams{To: instances.StateStopped, Reason: reason})
	if err != nil {
		return StopResult{}, err
	}
	return StopResult{Instance: stopped, ContainerGone: false, Verified: true, Reason: reason}, nil
}

func (r *Runner) stopDocker(ctx context.Context, inst instances.Instance) (StopResult, error) {
	footprint := inst.VRAMObsMiB
	if footprint == 0 {
		footprint = inst.VRAMEstMiB
	}
	if inst.State == instances.StateHealthy {
		_, _ = r.store.Transition(ctx, inst.ID, instances.TransitionParams{
			To: instances.StateDraining, Reason: "stop requested; draining before docker stop",
		})
	}
	freeBefore := 0
	if r.vram != nil {
		freeBefore, _ = r.vram.FreeVRAMMiB(ctx)
	}

	// docker stop → docker rm -f (best-effort; rm -f is idempotent).
	_, _ = r.docker.Run(ctx, DockerStopArgs(inst.ID)...)
	_, _ = r.docker.Run(ctx, DockerRmArgs(inst.ID)...)

	// Verify the container is gone via docker ps + poll the VRAM reclaim.
	gone, freeAfter, freed, verified, timedOut := r.waitContainerGoneAndReclaim(ctx, inst.ID, freeBefore, footprint)

	freedForRow := freed
	if freedForRow < 0 {
		freedForRow = 0
	}
	reason := r.stopReason(gone, freed, footprint, verified, timedOut)
	stopped, err := r.store.Transition(ctx, inst.ID, instances.TransitionParams{
		To: instances.StateStopped, Reason: reason, VRAMFreedMiB: &freedForRow,
	})
	if err != nil {
		return StopResult{}, fmt.Errorf("mark stopped: %w", err)
	}
	return StopResult{
		Instance: stopped, ContainerGone: gone, FreeBeforeMiB: freeBefore, FreeAfterMiB: freeAfter,
		FreedMiB: freed, FootprintMiB: footprint, Verified: verified, TimedOut: timedOut, Reason: reason,
	}, nil
}

// waitContainerGoneAndReclaim polls `docker ps` until the container is absent AND (if a probe is
// wired) free VRAM rose by >= footprint - slack, or StopVerifyTimeout elapses.
func (r *Runner) waitContainerGoneAndReclaim(ctx context.Context, instanceID string, freeBefore, footprint int) (gone bool, freeAfter, freed int, verified, timedOut bool) {
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
		gone = r.containerGone(ctx, instanceID)
		if r.vram != nil {
			if cur, err := r.vram.FreeVRAMMiB(ctx); err == nil {
				if cur > freeAfter {
					freeAfter = cur
				}
				freed = freeAfter - freeBefore
			}
		}
		vramOK := r.vram == nil || freed >= bar
		if gone && vramOK {
			// If no probe, "verified" means the container is gone (best we can assert).
			return true, freeAfter, freed, true, false
		}
		if time.Now().After(deadline) {
			return gone, freeAfter, freeAfter - freeBefore, false, true
		}
		select {
		case <-ctx.Done():
			return gone, freeAfter, freeAfter - freeBefore, false, true
		case <-tick.C:
		}
	}
}

// containerGone reports whether `docker ps --filter name=<name>` returns no running container.
func (r *Runner) containerGone(ctx context.Context, instanceID string) bool {
	out, err := r.docker.Run(ctx, DockerPsNameArgs(instanceID)...)
	if err != nil {
		return false // could not confirm; treat as not-gone (conservative)
	}
	return strings.TrimSpace(out) == ""
}

func (r *Runner) stopReason(gone bool, freed, footprint int, verified, timedOut bool) string {
	switch {
	case !gone:
		return fmt.Sprintf("docker stop+rm issued but the container is still listed by `docker ps` after %s — verify manually; VRAM reclaim not asserted", r.cfg.StopVerifyTimeout)
	case r.vram == nil:
		return "stopped: container removed (verified via `docker ps`); no hardware probe wired so the VRAM delta was not asserted"
	case verified && footprint > 0:
		return fmt.Sprintf("stopped and VERIFIED: container removed and free VRAM rose by %d MiB (>= footprint %d − %d slack) — VRAM reclaimed (probe delta, not a docker-200 claim)",
			freed, footprint, r.cfg.StopVRAMDeltaSlackMiB)
	case verified:
		return fmt.Sprintf("stopped and VERIFIED: container removed and free VRAM rose by %d MiB (footprint unknown; any positive reclaim required)", freed)
	case timedOut:
		return fmt.Sprintf("stopped: container removed but the VRAM reclaim of %d MiB did not reach the expected %d MiB within %s — re-probe to confirm",
			freed, footprint-r.cfg.StopVRAMDeltaSlackMiB, r.cfg.StopVerifyTimeout)
	default:
		return fmt.Sprintf("stopped: container removed; measured VRAM reclaim %d MiB", freed)
	}
}

// --- health probing ---

func (r *Runner) waitHealthy(ctx context.Context, baseURL string) error {
	return r.waitHealthyDeadline(ctx, baseURL, r.cfg.HealthTimeout)
}

// waitHealthyDeadline polls GET {baseURL}/health until 200, then cross-checks GET /v1/models,
// or the timeout elapses.
func (r *Runner) waitHealthyDeadline(ctx context.Context, baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	interval := r.cfg.HealthInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if r.probeHealth(ctx, baseURL) {
			// Readiness cross-check: /v1/models lists the served model once fully loaded. A
			// transient failure here is tolerated (health already passed) but we prefer to see it.
			if r.probeModels(ctx, baseURL) {
				return nil
			}
			// Health OK but /v1/models not ready yet → keep polling briefly.
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for /health + /v1/models", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (r *Runner) probeHealth(ctx context.Context, baseURL string) bool {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode == http.StatusOK
}

// probeModels does GET /v1/models and returns true on a 200 with a non-empty data list.
func (r *Runner) probeModels(ctx context.Context, baseURL string) bool {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return false
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var m struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	return len(m.Data) > 0
}

func (r *Runner) fail(ctx context.Context, id, reason string) {
	_, _ = r.store.Transition(ctx, id, instances.TransitionParams{To: instances.StateFailed, Reason: reason})
}

// tailLines returns the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
