// Package core is the single service core of LocalHub.
//
// Architecture rule (PLAN.md decision 7): there is ONE service core; the REST, SSE,
// WebSocket, and JSON-RPC transports are thin layers over it. All business logic lives
// here (or in packages this depends on: hardware, engines, compat, residency, manifest);
// transports only marshal in/out and route. Adding a method here and calling it from a
// transport is the intended extension pattern for P1.2-P1.4 and beyond.
//
// This P1.1 scaffold intentionally exposes only Health(). Later tasks add methods such as
// Hardware(), Engines(), Models(), Compat(), etc., each backed by an injected dependency
// (see the Deps struct). Keep those methods here so every transport shares one
// implementation and one set of correctness guarantees.
package core

import (
	"context"
	"fmt"
	"time"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/db"
	"github.com/localhub/localhub/internal/engines/llamacpp"
	"github.com/localhub/localhub/internal/engines/ollama"
	"github.com/localhub/localhub/internal/engines/vllm"
	"github.com/localhub/localhub/internal/hardware"
	"github.com/localhub/localhub/internal/instances"
	"github.com/localhub/localhub/internal/inventory"
	"github.com/localhub/localhub/internal/manifest"
	"github.com/localhub/localhub/internal/residency"
)

// Version is the daemon version string. Kept here (not in main) so every transport and the
// health check report the same value. Bump on release.
const Version = "0.1.0-dev"

// Deps are the collaborators the core depends on. P1.2-P1.4 inject their probes/adapters
// here (e.g. Hardware hardware.Probe, Engines []engines.Adapter, Inventory ...). Injecting
// through this struct keeps the core testable with fakes and keeps transports ignorant of
// construction details.
type Deps struct {
	Cfg config.Config
	DB  *db.DB
	// Hardware is the GPU/CPU/RAM/disk probe (P1.2). nil-safe: Hardware() reports a
	// configuration error rather than panicking.
	Hardware hardware.Probe
	// Engines holds the read-only engine detectors (P1.3). A nil adapter inside the
	// registry means "engine disabled in config" and yields a Detected=false status.
	Engines EnginesRegistry
	// Inventory + InvRepo are the disk/Ollama scanner and its SQLite repository (P1.4).
	// nil-safe: Models()/RescanModels() report a configuration error if unset.
	Inventory *inventory.Scanner
	InvRepo   *inventory.Repository
	// OllamaInv is the wiring adapter that both feeds the scanner's Ollama source and
	// records Ollama reachability for RescanModels' reconcile decision. nil when Ollama is
	// disabled. Kept alongside Inventory so the scan summary can report OllamaScanned and
	// reconciliation never marks Ollama rows missing on a transient outage.
	OllamaInv *OllamaInvAdapter
	// Events is the in-process change bus the live transports (WebSocket) subscribe to.
	// nil-safe: the core publishes best-effort and a nil bus is a no-op, so tests and
	// transports that don't need live signals can leave it unset.
	Events *EventBus
	// Compat is the compatibility rule engine (P2.3). nil-safe: CompatPreview reports a
	// configuration error rather than panicking. Injected as an interface so tests can fake it.
	Compat CompatEvaluator
	// HFMeta fetches remote HF metadata (config.json/index.json) for the remote-safetensors
	// preview path (P2.4). nil => a default hfmeta.Client is used. Injected for tests.
	HFMeta HFMetaFetcher
	// Calib is the SQLite-backed calibration store (P2.5): measured VRAM peaks recorded from
	// real loads, preferred over estimates by the compat engine (Confidence "exact"). Wired
	// alongside Compat (which must be constructed via compat.NewWithCalibration(Calib) to
	// consult it). nil-safe: RecordObservedPeak reports a configuration error rather than
	// panicking, and the compat engine simply falls through to parse/fallback estimates.
	Calib *compat.CalibrationStore
	// Manifests is the immutable supply-chain store (Phase 3): manifests written by every
	// download/import, read by the reproduce export. nil-safe: the download/reproduce methods
	// report a configuration error rather than panicking.
	Manifests *manifest.Store
	// Downloader performs HF/gguf_url downloads (resolve → guard → fetch → verify → manifest).
	// nil when the supply chain is not wired.
	Downloader *manifest.Downloader
	// OllamaPuller drives `ollama pull` (P3.2). nil when Ollama is disabled.
	OllamaPuller *manifest.OllamaPuller

	// --- Phase 4 instance lifecycle (P4.1/P4.2) ---

	// InstanceStore is the SQLite-backed instance state machine + transition audit + port pool
	// owner. nil-safe: Instances()/StartInstance() report a configuration error rather than
	// panicking.
	InstanceStore *instances.Store
	// LlamaRunner spawns/stops llama-server processes with the VRAM-delta stop gate (P4.2).
	// Injected as an interface (LlamaRunnerIface) so core tests can fake it. nil when the
	// runtime is not wired.
	LlamaRunner LlamaRunnerIface
	// Reconciler runs daemon-startup reconciliation of orphaned/dead child processes (P4.1).
	// nil-safe: ReconcileInstances is a no-op when unset.
	Reconciler *instances.Reconciler

	// OllamaRunner warms/unloads Ollama models against the instance state machine (P4.3). Ollama
	// is a shared daemon LocalHub never spawns or kills; this seam warms via keep_alive and
	// unloads via keep_alive:0 + /api/ps poll + VRAM-delta verify. nil when Ollama is disabled.
	OllamaRunner OllamaRunnerIface
	// OllamaMirror syncs the Ollama daemon's /api/ps resident set into instance rows (P4.3),
	// run at reconcile time + on a periodic interval. nil when Ollama is disabled.
	OllamaMirror OllamaMirrorIface

	// VLLMRunner starts/stops vLLM instances (P4.4, contract level: docker/attach/subprocess).
	// nil when vLLM is disabled. The real docker E2E is P4.5; this seam makes the adapter
	// reachable and testable via the DockerClient fake.
	VLLMRunner VLLMRunnerIface
	// VLLMReconciler reconciles docker-mode vLLM containers at startup (P4.4). nil when disabled.
	VLLMReconciler VLLMReconcilerIface
	// VLLMPortPool allocates host ports for docker/subprocess vLLM instances (config
	// engines.vllm.port_range). nil when vLLM is disabled or in attach-only mode.
	VLLMPortPool *instances.PortPool

	// --- Phase 5 coarse residency (P5.0/P5.1/P5.2) ---

	// Residency is the admission-control + eviction + priority-queue manager. When set,
	// StartInstance routes through Admit (warm→route, fits→load, evict→load with per-eviction
	// VRAM re-read, else queue/reject) INSTEAD of the temporary phase4SafetyCheck. nil-safe:
	// StartInstance falls back to the phase-4 safety pre-check when Residency is unset (config
	// residency.enabled=false), so the daemon still runs conservatively.
	Residency *residency.Manager
	// InFlight is the per-instance in-flight request counter (never-kill-active substrate). The
	// instance-API completion path increments/decrements it; eviction reads it (an instance with
	// InFlight>0 is never a victim). Shared with the Residency manager's ActiveTracker. nil-safe.
	InFlight *residency.InFlightTracker
}

// VLLMRunnerIface is the mutation seam for the vLLM adapter (P4.4), satisfied by *vllm.Runner.
type VLLMRunnerIface interface {
	Start(ctx context.Context, spec vllm.StartSpec) (instances.Instance, error)
	Stop(ctx context.Context, instanceID string) (vllm.StopResult, error)
}

// VLLMReconcilerIface reconciles docker-mode vLLM containers at startup, satisfied by
// *vllm.DockerReconciler.
type VLLMReconcilerIface interface {
	Reconcile(ctx context.Context) (vllm.ReconcileResult, error)
}

// OllamaRunnerIface is the mutation seam for the Ollama adapter (P4.3), satisfied by
// *ollama.Runner. Warm creates REQUESTED→STARTING→HEALTHY; Stop unloads (keep_alive:0) with the
// /api/ps poll + VRAM-delta gate. Injected as an interface so the core is testable without a real
// daemon.
type OllamaRunnerIface interface {
	Warm(ctx context.Context, spec ollama.WarmSpec) (instances.Instance, error)
	Stop(ctx context.Context, instanceID, ollamaTag string) (ollama.StopResult, error)
}

// OllamaMirrorIface is the /api/ps → instances mirror seam (P4.3), satisfied by *ollama.Mirror.
type OllamaMirrorIface interface {
	Sync(ctx context.Context) (ollama.MirrorResult, error)
}

// LlamaRunnerIface is the mutation seam for the llama.cpp runner (P4.2), satisfied by
// *llamacpp.Runner. Injected as an interface so the core can be tested without spawning real
// processes.
type LlamaRunnerIface interface {
	Start(ctx context.Context, spec llamacpp.StartSpec) (instances.Instance, error)
	Stop(ctx context.Context, instanceID string) (llamacpp.StopResult, error)
	TinyCompletion(ctx context.Context, baseURL string, maxTokens int) error
	RecordPeakVRAM(ctx context.Context, instanceID string, freeBeforeLoad int) (int, error)
}

// EnginesRegistry holds the three constructed, read-only engine detection adapters (P1.3).
// Any field may be nil when that engine is disabled in config; the Engines() sweep reports a
// Detected=false status with a Reason for a nil adapter rather than skipping it silently.
type EnginesRegistry struct {
	Ollama   *ollama.Client
	LlamaCPP *llamacpp.Adapter
	VLLM     *vllm.Adapter
}

// Service is the single service core. Construct with New; share one instance across all
// transports.
type Service struct {
	deps Deps
	// registry tracks in-flight download/import jobs (Phase 3). Ephemeral by design — the
	// durable artifacts are the manifest + models row; a restart loses job records (a killed
	// download's .part is still resumable).
	registry *downloadRegistry
}

// New constructs the service core from its dependencies.
func New(deps Deps) *Service {
	s := &Service{deps: deps, registry: newDownloadRegistry()}
	// Wire the residency manager's estimator + evictor seams, which live on the service (they need
	// the compat engine + engine runners). Resolves the manager↔service wiring cycle: main builds
	// the manager with the store-backed lister + probe; here we complete it with the core seams.
	if deps.Residency != nil {
		deps.Residency.SetSeams(residencyEstimator{s: s}, residencyEvictor{s: s})
		// Wire the production queue-drain starter (fix 1): the drain loop re-runs StartInstance for
		// a de-queued request through this seam when capacity frees.
		deps.Residency.SetStarter(residencyStarter{s: s})
	}
	return s
}

// Config returns the effective configuration (read-only view for transports that need it,
// e.g. to know the poll interval or bind address).
func (s *Service) Config() config.Config { return s.deps.Cfg }

// Events returns the in-process change bus so a transport can Subscribe to models.updated /
// engines.updated signals. Returns nil if none was wired (Subscribe is nil-safe).
func (s *Service) Events() *EventBus { return s.deps.Events }

// HealthResult is the payload behind GET /api/health and any equivalent JSON-RPC/WS call.
type HealthResult struct {
	Status  string `json:"status"`  // "ok" | "degraded"
	Version string `json:"version"` // daemon version
	DBOK    bool   `json:"db_ok"`   // database reachable
	// Reason is populated when Status != "ok", per the error/verdict convention
	// (spec-digest §3: every user-facing result carries a human-readable reason).
	Reason string `json:"reason,omitempty"`
}

// Health reports daemon liveness and database reachability. It is the one method wired for
// the P1.1 milestone; it establishes the pattern (context in, typed result out, no
// transport concerns) that every later core method follows.
func (s *Service) Health(ctx context.Context) HealthResult {
	res := HealthResult{Status: "ok", Version: Version, DBOK: true}
	if s.deps.DB == nil {
		res.Status = "degraded"
		res.DBOK = false
		res.Reason = "database not initialized"
		return res
	}
	if err := s.deps.DB.PingContext(ctx); err != nil {
		res.Status = "degraded"
		res.DBOK = false
		res.Reason = "database ping failed: " + err.Error()
	}
	return res
}

// Hardware returns a point-in-time hardware snapshot for GET /api/hardware and each SSE
// frame of GET /api/hardware/live. Thin passthrough to the injected probe; the probe
// guarantees a missing GPU is non-fatal (zero GPUs, not an error).
func (s *Service) Hardware(ctx context.Context) (hardware.Snapshot, error) {
	if s.deps.Hardware == nil {
		return hardware.Snapshot{}, fmt.Errorf("hardware probe not configured")
	}
	return s.deps.Hardware.Snapshot(ctx)
}

// HardwarePollInterval is the cadence for the SSE hardware stream and the metrics loop. It
// derives from Hardware.PollIntervalSeconds (config), clamped to >= 1s.
func (s *Service) HardwarePollInterval() time.Duration {
	n := s.deps.Cfg.Hardware.PollIntervalSeconds
	if n < 1 {
		n = 1
	}
	return time.Duration(n) * time.Second
}
