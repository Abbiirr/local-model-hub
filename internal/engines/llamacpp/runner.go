// This file adds the P4.2 MUTATION path to the llama.cpp adapter: spawn a real llama-server,
// poll it to HEALTHY, read its peak VRAM for calibration, and stop it with a VERIFIED VRAM
// reclaim. The detection code (llamacpp.go) stays read-only; this Runner is the owned-process
// half of the engine-adapters.md §5 contract for llama.cpp.
//
// The Runner drives the internal/instances state machine (it does not own it): it Creates a
// REQUESTED instance, transitions it STARTING→HEALTHY (or →FAILED on timeout), and on Stop does
// DRAINING?→STOPPED with the measured NVML delta. The VRAM-delta gate (spec-digest §5,
// TESTING.md#p42, CLAUDE.md correctness bar) is enforced HERE: a stop is only "verified" when the
// probe shows free VRAM rose by >= footprint-500MiB (or a 30s timeout elapses, recorded).
//
// [RE-VERIFY 2026-07-06] llama-server flags used: `-m <gguf> --host 127.0.0.1 --port <p> -c
// <ctx> -ngl all --flash-attn --no-webui` (+ `--mmproj <path>` for vision, + `-ctk/-ctv` for KV
// quant). --no-webui and --flash-attn confirmed present in both the winget build (b8833) and the
// CUDA fork (detection capability probe). GET /health is the readiness endpoint (returns 200
// {"status":"ok"} when the model is loaded; 503 while loading) — llama.cpp server docs.
package llamacpp

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/localhub/localhub/internal/hardware"
	"github.com/localhub/localhub/internal/instances"
	"github.com/localhub/localhub/internal/instances/proc"
)

// VRAMProbe is the minimal hardware seam the Runner needs: current free VRAM (MiB) on GPU 0.
// Satisfied by a small wrapper over hardware.Probe (see FreeVRAMFromProbe). A seam so tests can
// script rising/falling VRAM without a real GPU.
type VRAMProbe interface {
	FreeVRAMMiB(ctx context.Context) (int, error)
}

// FreeVRAMFromProbe adapts a hardware.Probe into a VRAMProbe (reads GPU 0's VRAMFreeMiB).
type FreeVRAMFromProbe struct{ Probe hardware.Probe }

func (f FreeVRAMFromProbe) FreeVRAMMiB(ctx context.Context) (int, error) {
	snap, err := f.Probe.Snapshot(ctx)
	if err != nil {
		return 0, err
	}
	if len(snap.GPUs) == 0 {
		return 0, fmt.Errorf("no GPU in snapshot")
	}
	return snap.GPUs[0].VRAMFreeMiB, nil
}

// RunnerConfig tunes the Runner's timeouts and log location.
type RunnerConfig struct {
	// HealthTimeout bounds STARTING→HEALTHY (big models load slowly; default 120s from config).
	HealthTimeout time.Duration
	// HealthInterval is the /health poll cadence (default 1s).
	HealthInterval time.Duration
	// StopVerifyTimeout bounds the VRAM-reclaim poll after a stop (default 30s).
	StopVerifyTimeout time.Duration
	// PeakSampleWindow is how long to sample VRAM after HEALTHY to find the peak (default 10s).
	PeakSampleWindow time.Duration
	// LogDir is where per-instance stdout/stderr log files are written (size-capped). Empty =>
	// no file log (the ring buffer still captures the tail).
	LogDir string
	// LogCapBytes caps each instance log file (default 8 MiB).
	LogCapBytes int64
	// StopVRAMDeltaSlackMiB is subtracted from the observed footprint to set the reclaim bar:
	// verified when free rises by >= footprint - slack. Default 500 (TESTING.md#p42).
	StopVRAMDeltaSlackMiB int
	// GPU / Driver stamp the observed peak for calibration provenance.
	GPU    string
	Driver string
}

func (c RunnerConfig) withDefaults() RunnerConfig {
	if c.HealthTimeout <= 0 {
		c.HealthTimeout = 120 * time.Second
	}
	if c.HealthInterval <= 0 {
		c.HealthInterval = 1 * time.Second
	}
	if c.StopVerifyTimeout <= 0 {
		c.StopVerifyTimeout = 30 * time.Second
	}
	if c.PeakSampleWindow <= 0 {
		c.PeakSampleWindow = 10 * time.Second
	}
	if c.LogCapBytes <= 0 {
		c.LogCapBytes = 8 << 20
	}
	if c.StopVRAMDeltaSlackMiB <= 0 {
		c.StopVRAMDeltaSlackMiB = 500
	}
	return c
}

// Runner owns spawned llama-server processes and their lifecycle. One Runner per daemon;
// it tracks live proc.Process handles by instance ID so Stop can drive the Job Object it holds.
type Runner struct {
	store *instances.Store
	pool  *instances.PortPool
	vram  VRAMProbe
	cfg   RunnerConfig
	http  *http.Client

	live *liveProcesses
}

// NewRunner builds a Runner. store/pool are the shared instance store + port pool; vram is the
// hardware seam for the VRAM-delta gate.
func NewRunner(store *instances.Store, pool *instances.PortPool, vram VRAMProbe, cfg RunnerConfig) *Runner {
	return &Runner{
		store: store,
		pool:  pool,
		vram:  vram,
		cfg:   cfg.withDefaults(),
		http:  &http.Client{},
		live:  newLiveProcesses(),
	}
}

// StartSpec is everything the Runner needs to launch one model.
type StartSpec struct {
	InstanceID   string   // caller-supplied uuid (the instances row id)
	ModelID      string   // catalog model id (for calibration key + the instances row)
	ModelPath    string   // absolute .gguf path
	MMProjPath   string   // optional vision projector (--mmproj)
	Binary       string   // absolute llama-server path (chosen by the caller via compat bestLlamaBinary)
	ExtraPath    []string // dirs prepended to PATH (CUDA fork DLLs)
	Ctx          int      // context length (-c)
	Flags        []string // exact run flags from compat.RunFlags (already includes -ngl/-c/-fa/--mmproj/-ctk/-ctv)
	HasFlashAttn bool
	VRAMEstMiB   int // admission estimate (recorded on the row)
	Pinned       bool
	Priority     int
	TTLSeconds   int
}

// Start spawns llama-server, records the REQUESTED→STARTING→HEALTHY lifecycle (or →FAILED on
// timeout), and returns the HEALTHY instance. On any failure the process tree is killed, the
// port freed, and the instance marked FAILED with the last stderr lines in the reason.
//
// It does NOT do admission (that is Phase 5). The caller (core) does the Phase-4 safety
// pre-check before calling Start.
func (r *Runner) Start(ctx context.Context, spec StartSpec) (instances.Instance, error) {
	// 1. Allocate a port (bind-tested, persisted).
	port, err := r.pool.Allocate(ctx, spec.InstanceID)
	if err != nil {
		return instances.Instance{}, fmt.Errorf("allocate port: %w", err)
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	// 2. Build the full argv. host is ALWAYS 127.0.0.1 (localhost bind gate). We compose the
	//    complete flag list here: model + host/port + the compat-derived flags + --no-webui.
	args := r.buildArgs(spec, port)
	fullFlags := append([]string{spec.Binary}, args...)

	// 3. Create the REQUESTED row (records model_path, binary, ctx, est VRAM, launch flags come
	//    at STARTING when the port/argv are final).
	if _, err := r.store.Create(ctx, instances.CreateParams{
		ID:         spec.InstanceID,
		ModelID:    spec.ModelID,
		Engine:     instances.EngineLlamaCPP,
		ModelPath:  spec.ModelPath,
		BinaryPath: spec.Binary,
		Ctx:        spec.Ctx,
		VRAMEstMiB: spec.VRAMEstMiB,
		Pinned:     spec.Pinned,
		Priority:   spec.Priority,
		TTLSeconds: spec.TTLSeconds,
		Reason:     "instance requested for model " + spec.ModelID + " on llama.cpp",
	}); err != nil {
		_ = r.pool.Free(ctx, port)
		return instances.Instance{}, fmt.Errorf("create instance: %w", err)
	}

	// 4. Capture output: a ring-buffer tail (for the FAILED reason) + a size-capped log file.
	ring := instances.NewRingBuffer(200)
	logPath := r.logPath(spec.InstanceID)
	logFile, _ := instances.NewCappedLogFile(logPath, r.cfg.LogCapBytes) // best-effort; nil ok
	out := instances.MultiWriter(ring, logFile)

	// 5. Spawn (Job Object, CREATE_NO_WINDOW, ExtraPath on PATH).
	p, serr := proc.Spawn(proc.SpawnSpec{
		Path:   spec.Binary,
		Args:   args,
		Env:    envWithExtraPath(spec.ExtraPath),
		Stdout: out,
		Stderr: out,
	})
	if serr != nil {
		r.failStart(ctx, spec.InstanceID, port, logFile, "spawn failed: "+serr.Error(), ring)
		return instances.Instance{}, fmt.Errorf("spawn llama-server: %w", serr)
	}
	r.live.put(spec.InstanceID, p, ring, logFile)

	// 6. STARTING: record PID/port/base_url + the exact launch flags (reproducibility).
	pid := p.PID()
	if _, err := r.store.Transition(ctx, spec.InstanceID, instances.TransitionParams{
		To:          instances.StateStarting,
		Reason:      fmt.Sprintf("spawned llama-server pid %d on %s", pid, baseURL),
		PID:         &pid,
		Port:        &port,
		BaseURL:     &baseURL,
		LaunchFlags: fullFlags,
	}); err != nil {
		_ = p.Kill()
		r.failStart(ctx, spec.InstanceID, port, logFile, "state error: "+err.Error(), ring)
		return instances.Instance{}, err
	}

	// 7. Poll /health until OK or timeout.
	if err := r.waitHealthy(ctx, p, baseURL); err != nil {
		// Timeout/crash: kill tree, FAILED with the last stderr lines.
		_ = p.Kill()
		tail := ring.Tail(15)
		reason := "health check failed: " + err.Error()
		if tail != "" {
			reason += " — last output:\n" + tail
		}
		r.failStart(ctx, spec.InstanceID, port, logFile, reason, ring)
		r.live.remove(spec.InstanceID)
		return instances.Instance{}, fmt.Errorf("llama-server did not become healthy: %w", err)
	}

	// 8. HEALTHY.
	healthy, err := r.store.Transition(ctx, spec.InstanceID, instances.TransitionParams{
		To:     instances.StateHealthy,
		Reason: fmt.Sprintf("llama-server pid %d answered /health OK on %s", pid, baseURL),
	})
	if err != nil {
		_ = p.Kill()
		r.failStart(ctx, spec.InstanceID, port, logFile, "state error at healthy: "+err.Error(), ring)
		r.live.remove(spec.InstanceID)
		return instances.Instance{}, err
	}
	return healthy, nil
}

// buildArgs composes the full llama-server argument list. host is ALWAYS 127.0.0.1. The compat
// Flags already carry -ngl/-c/-fa/--mmproj/-ctk/-ctv; we add model, host, port, and --no-webui,
// and defensively add -c/--mmproj if the caller passed them via spec but not via Flags.
func (r *Runner) buildArgs(spec StartSpec, port int) []string {
	args := []string{
		"-m", spec.ModelPath,
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
	}
	// The compat run flags (authoritative). If they do not already include -c, add it.
	flags := spec.Flags
	if !hasFlag(flags, "-c") && spec.Ctx > 0 {
		args = append(args, "-c", fmt.Sprintf("%d", spec.Ctx))
	}
	args = append(args, flags...)
	// mmproj: add only if not already in the flags (compat adds it when it has the path).
	if spec.MMProjPath != "" && !hasFlag(flags, "--mmproj") {
		args = append(args, "--mmproj", spec.MMProjPath)
	}
	// Disable the built-in web UI (headless control-plane use).
	if !hasFlag(flags, "--no-webui") {
		args = append(args, "--no-webui")
	}
	return args
}

// waitHealthy polls GET {baseURL}/health until it returns 200, the process exits, or the
// timeout elapses. A process exit during startup is a fast-fail (crashed on load).
func (r *Runner) waitHealthy(ctx context.Context, p proc.Process, baseURL string) error {
	deadline := time.Now().Add(r.cfg.HealthTimeout)
	tick := time.NewTicker(r.cfg.HealthInterval)
	defer tick.Stop()
	for {
		if p.Done() {
			return fmt.Errorf("process exited during startup (crashed before /health answered)")
		}
		if r.probeHealth(ctx, baseURL) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for /health", r.cfg.HealthTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// probeHealth does one GET /health, returning true on a 200. A 503 (loading) or transport error
// is false (keep polling).
func (r *Runner) probeHealth(ctx context.Context, baseURL string) bool {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
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
	return resp.StatusCode == http.StatusOK
}

// failStart marks an instance FAILED (best-effort, tolerating an already-terminal state), frees
// its port, and closes the log file. Used on every start-path failure.
func (r *Runner) failStart(ctx context.Context, id string, port int, logFile *instances.CappedLogFile, reason string, ring *instances.RingBuffer) {
	// The instance may be REQUESTED or STARTING here; both can reach FAILED.
	_, _ = r.store.Transition(ctx, id, instances.TransitionParams{To: instances.StateFailed, Reason: reason})
	_ = r.pool.Free(ctx, port)
	_ = logFile.Close()
	_ = ring // retained by liveProcesses if put; harmless here
}

// hasFlag reports whether flag appears in args.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// logPath returns the per-instance log file path under LogDir (empty LogDir => "").
func (r *Runner) logPath(instanceID string) string {
	if r.cfg.LogDir == "" {
		return ""
	}
	safe := strings.ReplaceAll(instanceID, ":", "_")
	return filepath.Join(r.cfg.LogDir, "llamacpp-"+safe+".log")
}

// newJSONPost builds a POST request with a JSON body and the right content type.
func newJSONPost(ctx context.Context, url, body string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}
