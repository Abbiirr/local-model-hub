// Command localhub is the LocalHub Compass control-plane daemon entrypoint.
//
// It is a thin wire-up: load config, open+migrate SQLite, build the service core, start
// the HTTP transport bound to loopback, and shut down cleanly on Ctrl-C. All logic lives
// in internal/* — main only assembles the pieces (PLAN.md decision 7).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/localhub/localhub/internal/api"
	"github.com/localhub/localhub/internal/cluster"
	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/compat/hfmeta"
	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/core"
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

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "localhub: fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	var configPath string
	flag.StringVar(&configPath, "config", defaultConfigPath(), "path to config YAML (empty = built-in defaults)")
	flag.Parse()

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := cfg.Logging.NewLogger(os.Stderr)
	logger.Info("localhub starting",
		"version", core.Version,
		"config", configPathLabel(configPath),
		"bind", cfg.Server.Bind,
		"port", cfg.Server.Port,
		"db", cfg.Data.DBPath,
	)

	database, err := db.Open(cfg.Data.DBPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	if err := database.Migrate(); err != nil {
		return fmt.Errorf("migrate db: %w", err)
	}
	if mode, err := database.JournalMode(); err != nil {
		logger.Warn("could not read journal_mode", "err", err)
	} else {
		logger.Info("database ready", "path", database.Path(), "journal_mode", mode)
	}

	// Hardware probe (P1.2): nvidia-smi + Windows CPU/RAM/disk. Report disk on the model
	// volumes so the dashboard shows the disk the models actually live on.
	hwProbe := hardware.New(hardware.Config{DiskPaths: cfg.Data.ModelDirs})

	// Engine detectors (P1.3): all read-only. A disabled engine yields a nil adapter (core
	// reports it as not-detected). The Ollama client is also reused by the inventory adapter.
	reg := core.EnginesRegistry{}
	var ollamaClient *ollama.Client
	if cfg.Engines.Ollama.Enabled {
		ollamaClient = ollama.New(cfg.Engines.Ollama.BaseURL, "")
		reg.Ollama = ollamaClient
	}
	if cfg.Engines.LlamaCPP.Enabled {
		reg.LlamaCPP = llamacpp.NewFromPaths(cfg.Engines.LlamaCPP.BinaryPaths, cfg.Engines.LlamaCPP.ExtraPath)
	}
	if cfg.Engines.VLLM.Enabled {
		reg.VLLM = vllm.New(cfg.Engines.VLLM.Mode, cfg.Engines.VLLM.AttachURL)
	}

	// Inventory scanner (P1.4) + repository. The Ollama source is wired only when Ollama is
	// enabled; the adapter records reachability so a down daemon never marks Ollama rows
	// missing (RescanModels excludes the ollama source when unreachable).
	var ollamaInv *core.OllamaInvAdapter
	scanOpts := []inventory.Option{}
	if ollamaClient != nil {
		ollamaInv = core.NewOllamaInvAdapter(ollamaClient)
		scanOpts = append(scanOpts, inventory.WithOllama(ollamaInv))
	}
	scanner := inventory.New(cfg.Data.ModelDirs, scanOpts...)
	invRepo := inventory.NewRepository(database)

	// Calibration store (P2.5): measured VRAM peaks recorded from real loads, preferred over
	// estimates by the compat engine (Confidence "exact"). Wired into BOTH the compat engine
	// (so it consults the store) and Deps (so RecordObservedPeak / the exact-note enrichment
	// can reach it). Backed by the observed_peaks table (migration 0003).
	calibStore := compat.NewCalibrationStore(database.DB)

	// Supply chain (Phase 3): immutable manifest store + the download orchestrator + the
	// Ollama pull driver. Downloads land in the FIRST configured model dir (path-containment
	// validated per-file). The size guard reads current free disk from the hardware probe so
	// a download that would fill K: is refused BEFORE any bytes are fetched (PLAN.md
	// safeguards). Strict mode (opt-in) upgrades pickle warnings to a refusal.
	manifestStore := manifest.NewStore(database.DB)
	var downloader *manifest.Downloader
	if len(cfg.Data.ModelDirs) > 0 {
		guard := manifest.GuardConfig{FreeDiskBytes: freeDiskBytes(ctxBg(), hwProbe, cfg.Data.ModelDirs[0])}
		downloader = manifest.NewDownloader(
			cfg.Data.ModelDirs[0], guard, cfg.Security.StrictModelArtifactMode, "")
	}
	var ollamaPuller *manifest.OllamaPuller
	if cfg.Engines.Ollama.Enabled {
		ollamaPuller = manifest.NewOllamaPuller(cfg.Engines.Ollama.BaseURL)
	}

	// Phase 4 instance runtime (P4.1/P4.2): the state-machine store, the llama.cpp port pool,
	// the spawn/health/stop Runner (with the VRAM-delta gate), and the startup reconciler. All
	// share the daemon's *sql.DB and the hardware probe. Enabled only when llama.cpp is.
	instStore := instances.NewStore(database.DB)
	var llamaRunner *llamacpp.Runner
	var reconciler *instances.Reconciler
	if cfg.Engines.LlamaCPP.Enabled {
		pr := cfg.Engines.LlamaCPP.PortRange
		portPool := instances.NewPortPool(database.DB, pr[0], pr[1])
		vramProbe := llamacpp.FreeVRAMFromProbe{Probe: hwProbe}
		llamaRunner = llamacpp.NewRunner(instStore, portPool, vramProbe, llamacpp.RunnerConfig{
			HealthTimeout: time.Duration(healthTimeoutSeconds(cfg)) * time.Second,
			LogDir:        instanceLogDir(cfg),
		})
		reconciler = instances.NewReconciler(instStore, portPool,
			llamacpp.NewReconcileHealthChecker(), nil, nil)
	}

	// Ollama instance lifecycle (P4.3): warm/unload against the state machine + the /api/ps →
	// instances mirror. Ollama is a shared daemon LocalHub never spawns or kills. The mirror's
	// tag→model_id resolver reads the inventory so a mirrored row's model_id satisfies the FK.
	var ollamaRunner *ollama.Runner
	var ollamaMirror *ollama.Mirror
	if ollamaClient != nil {
		ollamaVRAMProbe := ollamaFreeVRAMProbe{probe: hwProbe}
		ollamaRunner = ollama.NewRunner(ollamaClient, instStore, ollamaVRAMProbe, ollama.RunnerConfig{})
		resolver := func(ctx context.Context, tag string) (string, bool) {
			var id string
			err := database.DB.QueryRowContext(ctx,
				`SELECT id FROM models WHERE source = 'ollama' AND display_name = ? LIMIT 1`, tag).Scan(&id)
			if err != nil {
				return "", false
			}
			return id, true
		}
		ollamaMirror = ollama.NewMirror(ollamaClient, instStore, resolver)
	}

	// vLLM instance runtime (P4.4, contract level): docker/attach runner + docker reconciler +
	// host-port pool. The real docker E2E is P4.5; this makes the adapter reachable. subprocess
	// mode returns notImplemented on Windows (guarded in the runner).
	var vllmRunner *vllm.Runner
	var vllmReconciler *vllm.DockerReconciler
	var vllmPortPool *instances.PortPool
	if cfg.Engines.VLLM.Enabled {
		vc := cfg.Engines.VLLM
		var dockerClient vllm.DockerClient
		if vc.Mode == "docker" {
			dockerClient = vllm.NewExecDocker("docker")
			pr := vc.PortRange
			vllmPortPool = instances.NewPortPool(database.DB, pr[0], pr[1])
			vllmReconciler = vllm.NewDockerReconciler(instStore, dockerClient)
		}
		vllmVRAMProbe := ollamaFreeVRAMProbe{probe: hwProbe} // same GPU-0 free-VRAM seam
		vllmRunner = vllm.NewRunner(vllm.Mode(vc.Mode), instStore, dockerClient, vllmVRAMProbe, vllm.RunnerConfig{
			HealthTimeout:     time.Duration(vllmHealthTimeoutSeconds(cfg)) * time.Second,
			DefaultGPUMemUtil: vc.DefaultGPUMemoryUtil,
		})
	}

	// Phase 5 coarse residency (P5.0/P5.1/P5.2): the admission-control + eviction + custom
	// priority-queue manager. Shared free-VRAM probe (re-read live per decision + per eviction —
	// the anti-OOM rule), the instance store as the eviction candidate set, and the durable queue
	// mirror. The estimator + evictor seams live in core (they need the compat engine + engine
	// runners), so they are wired inside core.New via the service; here we build the manager with
	// the store-backed lister + probe + in-flight tracker, then hand it in. When
	// residency.enabled=false the manager is nil and StartInstance uses the phase-4 pre-check.
	var residencyMgr *residency.Manager
	var inflight *residency.InFlightTracker
	if cfg.Residency.Enabled {
		inflight = residency.NewInFlightTracker()
		residencyMgr = buildResidencyManager(cfg, database, instStore, hwProbe, inflight, logger)
		if restored, err := residencyMgr.RestoreWaiting(context.Background()); err != nil {
			return fmt.Errorf("restore durable residency queue: %w", err)
		} else if restored > 0 {
			logger.Info("restored durable residency queue", "waiting", restored)
		}
	}

	svc := core.New(core.Deps{
		Cfg:       cfg,
		DB:        database,
		Hardware:  hwProbe,
		Engines:   reg,
		Inventory: scanner,
		InvRepo:   invRepo,
		OllamaInv: ollamaInv,
		// In-process change bus the WebSocket transport subscribes to (P1.5): a rescan
		// or engine re-detect publishes here → connected dashboards refetch over REST.
		Events: core.NewEventBus(),
		// Compatibility rule engine (P2.3) + remote HF metadata fetcher (P2.4). The engine
		// wraps the GGUF + safetensors estimators; HFMeta reads config.json/index.json for
		// the remote-safetensors preview path (metadata only, timeout+size-capped).
		Compat: compat.NewWithCalibration(calibStore),
		HFMeta: &hfmeta.Client{},
		Calib:  calibStore,
		// Phase 3 supply chain.
		Manifests:    manifestStore,
		Downloader:   downloader,
		OllamaPuller: ollamaPuller,
		// Phase 4 instance lifecycle (nil LlamaRunner when llama.cpp is disabled).
		InstanceStore: instStore,
		LlamaRunner:   llamaRunnerOrNil(llamaRunner),
		Reconciler:    reconciler,
		// P4.3 Ollama warm/unload + /api/ps mirror (nil when Ollama is disabled).
		OllamaRunner: ollamaRunnerOrNil(ollamaRunner),
		OllamaMirror: ollamaMirrorOrNil(ollamaMirror),
		// P4.4 vLLM runner + docker reconciler + host-port pool (nil when vLLM disabled).
		VLLMRunner:     vllmRunnerOrNil(vllmRunner),
		VLLMReconciler: vllmReconcilerOrNil(vllmReconciler),
		VLLMPortPool:   vllmPortPool,
		// P5 coarse residency (nil when residency.enabled=false → phase-4 pre-check fallback).
		Residency: residencyMgr,
		InFlight:  inflight,
	})

	adminToken := ""
	if cfg.Server.AdminToken != nil {
		adminToken = *cfg.Server.AdminToken
	}

	// Cluster role wiring (P7.1, ADR-008, docs/cluster-plan.md). role=off wires NOTHING — no
	// identity minting, no registry, no goroutine — so the daemon is byte-identical to 1.0
	// (cluster-plan §1.3). "coordinator" builds the DB-backed soft-state registry and passes it to
	// the API so /api/cluster/* register; "node" builds the heartbeat client (started after the
	// signal context exists, below). Both roles mint/persist this daemon's stable node UUID.
	var clusterOpts []api.Option
	var clusterClient *cluster.Client
	if cfg.Cluster.Role != "off" {
		nodeID, cerr := cluster.LoadOrCreateNodeID(context.Background(), database.DB)
		if cerr != nil {
			return fmt.Errorf("cluster identity: %w", cerr)
		}
		hbInterval := time.Duration(cfg.Cluster.HeartbeatIntervalSeconds) * time.Second
		switch cfg.Cluster.Role {
		case "coordinator":
			reg := cluster.NewRegistry(database.DB, hbInterval)
			clusterOpts = append(clusterOpts,
				api.WithCluster("coordinator", cfg.Cluster.Token, nodeID, cfg.Cluster.NodeName, reg))
			logger.Info("cluster coordinator role enabled",
				"node_id", nodeID, "node_name", cfg.Cluster.NodeName, "heartbeat_interval", hbInterval.String())
		case "node":
			clusterClient = cluster.NewClient(cluster.ClientConfig{
				CoordinatorURL: cfg.Cluster.CoordinatorURL,
				Token:          cfg.Cluster.Token,
				NodeID:         nodeID,
				Name:           cfg.Cluster.NodeName,
				BaseURL:        nodeBaseURL(cfg),
				Version:        core.Version,
				Interval:       hbInterval,
				Logger:         logger,
				// P7.2: report a compact hardware summary (free/total VRAM, GPU name, free RAM) on every
				// heartbeat, read from THIS node's own probe. The coordinator stores it verbatim and shows
				// it in aggregated views — it never parses it for a fit (ADR-008, one-core-per-node).
				HWSummary: nodeHWSummary(hwProbe),
			})
			logger.Info("cluster node role enabled",
				"node_id", nodeID, "node_name", cfg.Cluster.NodeName,
				"coordinator", cfg.Cluster.CoordinatorURL, "base_url", nodeBaseURL(cfg),
				"heartbeat_interval", hbInterval.String())
		}
	}

	srv, err := api.New(svc, logger, cfg.Server.Bind, cfg.Server.Port, adminToken, clusterOpts...)
	if err != nil {
		return fmt.Errorf("start server: %w", err)
	}

	// Cancel the root context on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// P7.1 node heartbeat loop (cluster-plan §2.4). Detached goroutine: the coordinator being
	// down NEVER affects local serving (cluster-plan §1.3, §7) — the client logs+retries and never
	// returns an error to the daemon. Started only in node role (nil otherwise).
	if clusterClient != nil {
		go clusterClient.Run(ctx)
	}

	// Reconcile instances on EVERY daemon startup (playbook rule; P4.1). Orphaned/dead child
	// processes recorded by a prior run are re-attached (PID alive + healthy + identity) or
	// reaped (killed + FAILED) or marked STOPPED (process gone). Runs before serving so the
	// dashboard shows a truthful instance list from the first request.
	if rec, rerr := svc.ReconcileInstances(ctx); rerr != nil {
		logger.Warn("instance reconcile failed", "err", rerr)
	} else if n := len(rec.Reattached) + len(rec.Reaped) + len(rec.Stopped); n > 0 {
		logger.Info("instance reconcile complete",
			"reattached", rec.Reattached, "reaped", rec.Reaped, "stopped", rec.Stopped, "errors", rec.Errors)
	}

	// P6.2: fail any job left `running` by a crash/restart (design §2.6) BEFORE the worker starts,
	// so the worker never sees a stale running row. Queued jobs are left for the worker to re-admit.
	// A reconcile failure is non-fatal — log and continue.
	if err := svc.ReconcileJobs(ctx); err != nil {
		logger.Warn("reconcile jobs failed", "err", err)
	}

	// Ollama mirror (P4.3): one sync at startup (reconcile-time) so the dashboard reflects the
	// daemon's resident set immediately, then a periodic sync. A daemon outage is logged, not fatal
	// (never flap Ollama rows on a transient outage).
	if ollamaMirror != nil {
		if mr, merr := svc.SyncOllamaMirror(ctx); merr != nil {
			logger.Warn("ollama mirror initial sync failed", "err", merr)
		} else if n := len(mr.Added) + len(mr.Promoted) + len(mr.Stopped); n > 0 {
			logger.Info("ollama mirror synced",
				"added", mr.Added, "promoted", mr.Promoted, "stopped", mr.Stopped, "errors", mr.Errors)
		}
		interval := time.Duration(ollamaMirrorIntervalSeconds(cfg)) * time.Second
		go ollamaMirror.StartPeriodic(ctx, interval,
			func(mr ollama.MirrorResult) {
				if n := len(mr.Added) + len(mr.Promoted) + len(mr.Stopped); n > 0 {
					logger.Info("ollama mirror sync",
						"added", mr.Added, "promoted", mr.Promoted, "stopped", mr.Stopped)
				}
			},
			func(err error) { logger.Debug("ollama mirror sync error (transient?)", "err", err) },
		)
	}

	// vLLM docker reconcile at startup (P4.4): re-attach running containers, mark gone ones STOPPED.
	if vllmReconciler != nil {
		if vr, verr := svc.ReconcileVLLM(ctx); verr != nil {
			logger.Warn("vllm reconcile failed", "err", verr)
		} else if n := len(vr.Reattached) + len(vr.Stopped); n > 0 {
			logger.Info("vllm reconcile complete",
				"reattached", vr.Reattached, "stopped", vr.Stopped, "errors", vr.Errors)
		}
	}

	// Production queue-drain loop (Phase 5 fix 1): a periodic re-admit tick that auto-starts queued
	// requests when capacity frees. The loop ALSO fires on instance stop/evict (drainAsync). Runs
	// until shutdown. Owned by the manager (single-flighted internally), detached from any request.
	if residencyMgr != nil {
		go svc.StartPeriodicDrain(ctx,
			func(res residency.DrainResult) {
				logger.Info("residency queue drained",
					"started", res.Started, "requeued", res.Requeued, "failed", res.Failed)
			},
			func(err error) { logger.Debug("residency drain error", "err", err) },
		)
	}

	// P6.2 async-job worker (gated on cfg.Jobs.Enabled, default true; mirrors residency.enabled):
	// drains queued jobs oldest/highest-priority-first, admits each through the SAME residency path
	// as the proxy, relays non-streaming, and records the result. Returns when ctx is cancelled.
	if cfg.Jobs.Enabled {
		go svc.RunJobWorker(ctx)
	}

	// Initial inventory scan in the background (non-blocking, logged). GET /api/models is
	// usable immediately (empty until the scan lands); the scan is header-sniff only, no
	// hashing, so it is cheap even with the ~60 GB of GGUFs on this box.
	go func() {
		res, err := svc.RescanModels(ctx)
		if err != nil {
			logger.Warn("initial inventory scan failed", "err", err)
			return
		}
		logger.Info("initial inventory scan complete",
			"inserted", res.Inserted, "updated", res.Updated,
			"vanished", len(res.Vanished), "skipped", res.SkippedCount,
			"ollama_scanned", res.OllamaScanned, "model_count", res.ModelCount)
		for _, e := range res.ScanErrors {
			logger.Warn("inventory scan note", "detail", e)
		}
	}()

	if err := srv.Serve(ctx); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	logger.Info("localhub stopped")
	return nil
}

// defaultConfigPath returns the config path used when --config is not passed. It prefers
// configs/config.yaml if present in the working directory, else "" (built-in defaults),
// so the daemon runs on this box with zero setup.
func defaultConfigPath() string {
	for _, p := range []string{"configs/config.yaml", "config.yaml"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func configPathLabel(p string) string {
	if p == "" {
		return "(built-in defaults)"
	}
	return p
}

// ctxBg returns a short-lived background context for the startup disk probe. The probe is
// synchronous and fast (a single stat), so a plain Background context is fine; wrapped in a
// helper only to keep the wiring block readable.
func ctxBg() context.Context { return context.Background() }

// freeDiskBytes probes the hardware snapshot and returns the free bytes on the volume that
// hosts dir. Falls back to the largest reported volume's free space, then 0 (unknown → the
// download size guard skips the headroom check but still applies the hard cap). This feeds the
// PLAN.md download safeguard: refuse a download that would fill the disk.
func freeDiskBytes(ctx context.Context, probe hardware.Probe, dir string) int64 {
	snap, err := probe.Snapshot(ctx)
	if err != nil || len(snap.Disks) == 0 {
		return 0
	}
	vol := strings.ToUpper(volumeOf(dir))
	var bestFree float64
	var matched, best float64
	for _, d := range snap.Disks {
		if strings.HasPrefix(strings.ToUpper(d.Path), vol) {
			matched = d.FreeGiB
		}
		if d.FreeGiB > best {
			best = d.FreeGiB
			bestFree = d.FreeGiB
		}
	}
	gib := matched
	if gib == 0 {
		gib = bestFree
	}
	return int64(gib * float64(1<<30))
}

// volumeOf returns the volume prefix of a path (e.g. "K:" on Windows) so the disk probe can be
// matched to the download directory.
func volumeOf(p string) string {
	if len(p) >= 2 && p[1] == ':' {
		return p[:2]
	}
	return p
}

// nodeBaseURL returns the URL the coordinator uses to reach THIS node (cluster-plan §2.4). It
// prefers the explicit cluster.advertise_url; when empty it derives http://<bind>:<port> from the
// server config, which is correct for the loopback two-daemon dev rig. A real second box behind
// NAT/Tailscale must set advertise_url explicitly (never guessed from the socket peer address).
func nodeBaseURL(cfg config.Config) string {
	if cfg.Cluster.AdvertiseURL != "" {
		return cfg.Cluster.AdvertiseURL
	}
	return fmt.Sprintf("http://%s:%d", cfg.Server.Bind, cfg.Server.Port)
}

// nodeHWSummary returns the per-heartbeat hardware-summary closure the node client calls each beat
// (P7.2, cluster-plan §2.4/§5b). It maps THIS node's own hardware snapshot into the compact
// cluster.HWSummary shape (free/total VRAM per GPU + free/total RAM) and marshals it to the JSON string
// reported in JoinRequest.HWSummary. A probe error yields "" (no summary this beat) — the summary is
// advisory display data, never load-bearing, so the node still heartbeats and stays live. The mapping
// lives HERE (the wiring layer, which imports hardware) so the cluster package stays hardware-free.
func nodeHWSummary(probe hardware.Probe) func(context.Context) string {
	return func(ctx context.Context) string {
		snap, err := probe.Snapshot(ctx)
		if err != nil {
			return ""
		}
		hs := cluster.HWSummary{
			RAMFreeMiB:  snap.RAM.FreeMiB,
			RAMTotalMiB: snap.RAM.TotalMiB,
			CapturedAt:  snap.CapturedAt.UTC().Format(time.RFC3339),
		}
		for _, g := range snap.GPUs {
			hs.GPUs = append(hs.GPUs, cluster.HWGPU{
				Name:         g.Name,
				VRAMFreeMiB:  g.VRAMFreeMiB,
				VRAMTotalMiB: g.VRAMTotalMiB,
			})
		}
		return hs.JSON()
	}
}

// healthTimeoutSeconds returns the configured llama.cpp health timeout (default 120s).
func healthTimeoutSeconds(cfg config.Config) int {
	if n := cfg.Engines.LlamaCPP.HealthTimeoutSeconds; n > 0 {
		return n
	}
	return 120
}

// instanceLogDir returns the directory for per-instance engine log files: cfg.Data.DataDir, or
// a "logs" dir beside the DB file when DataDir is empty. Created lazily by the log writer.
func instanceLogDir(cfg config.Config) string {
	if cfg.Data.DataDir != "" {
		return filepath.Join(cfg.Data.DataDir, "logs")
	}
	return filepath.Join(filepath.Dir(cfg.Data.DBPath), "logs")
}

// llamaRunnerOrNil returns the Runner as the core interface, or a genuine nil interface when the
// runner is nil — avoiding the typed-nil-in-interface pitfall (a nil *Runner boxed in the
// interface would be non-nil, defeating the core's nil-checks).
func llamaRunnerOrNil(r *llamacpp.Runner) core.LlamaRunnerIface {
	if r == nil {
		return nil
	}
	return r
}

// ollamaRunnerOrNil / ollamaMirrorOrNil mirror llamaRunnerOrNil's typed-nil guard for the P4.3
// Ollama seams.
func ollamaRunnerOrNil(r *ollama.Runner) core.OllamaRunnerIface {
	if r == nil {
		return nil
	}
	return r
}

func ollamaMirrorOrNil(m *ollama.Mirror) core.OllamaMirrorIface {
	if m == nil {
		return nil
	}
	return m
}

// ollamaMirrorIntervalSeconds returns the configured /api/ps mirror cadence (default 10s).
func ollamaMirrorIntervalSeconds(cfg config.Config) int {
	if n := cfg.Engines.Ollama.MirrorIntervalSeconds; n > 0 {
		return n
	}
	return 10
}

// vllmRunnerOrNil / vllmReconcilerOrNil apply the typed-nil guard for the P4.4 vLLM seams.
func vllmRunnerOrNil(r *vllm.Runner) core.VLLMRunnerIface {
	if r == nil {
		return nil
	}
	return r
}

func vllmReconcilerOrNil(r *vllm.DockerReconciler) core.VLLMReconcilerIface {
	if r == nil {
		return nil
	}
	return r
}

// vllmHealthTimeoutSeconds returns the vLLM STARTING→HEALTHY timeout. vLLM cold start (weight load
// + CUDA graph capture) can take minutes; default 600s.
func vllmHealthTimeoutSeconds(cfg config.Config) int {
	// Reuse the llama.cpp health timeout knob if a vLLM-specific one is not present; default 600s.
	return 600
}

// buildResidencyManager constructs the Phase-5 residency manager from the config + shared
// collaborators. The estimator + evictor seams are wired later inside core.New (they live on the
// service); here we supply the store-backed instance lister, the free-VRAM probe (re-read live per
// decision + per eviction — the anti-OOM rule), the durable queue mirror, the in-flight tracker
// (never-kill-active substrate), and the queue-algorithm tuning from config.
func buildResidencyManager(cfg config.Config, database *db.DB, instStore *instances.Store, hwProbe hardware.Probe, inflight *residency.InFlightTracker, logger *slog.Logger) *residency.Manager {
	rc := cfg.Residency
	qt := rc.Queue
	qcfg := residency.QueueConfig{
		AgePromoteNormal:     durSecs(qt.AgePromoteNormalSeconds),
		AgePromoteBackground: durSecs(qt.AgePromoteBackgroundSeconds),
		CoalesceMax:          qt.CoalesceMax,
		Caps: residency.ClassCaps{
			Interactive: qt.CapInteractive,
			Normal:      qt.CapNormal,
			Background:  qt.CapBackground,
		},
	}
	clock := residency.RealClock()
	return residency.NewManager(residency.ManagerConfig{
		Queue:           residency.NewQueue(qcfg, clock),
		QueueStore:      residency.NewQueueStore(database.DB),
		Probe:           residencyVRAMProbe{probe: hwProbe},
		RAMProbe:        residencyRAMProbe{probe: hwProbe},
		GPUProbe:        residencyGPUProbe{probe: hwProbe},
		Lister:          residency.StoreLister{Store: instStore},
		Tracker:         inflight,
		Clock:           clock,
		SafetyMarginMiB: rc.SafetyMarginMB,
		RecentActiveTTL: durSecs(residency.DefaultRecentActiveTTLSecs),
		DefaultTTLSecs:  rc.DefaultTTLSeconds,
		VRAMBudgetMiB:   rc.VRAMBudgetMB,
		MinFreeRAMMiB:   rc.MinFreeRAMMiB,
		MaxGPUTempC:     rc.MaxGPUTempC,
		MaxGPUUtilPct:   rc.MaxGPUUtilPct,
		DrainInterval:   durSecs(rc.DrainIntervalSeconds),
		Logger:          slogAdmissionLogger{log: logger},
	})
}

// slogAdmissionLogger adapts the daemon's *slog.Logger into residency.AdmissionLogger (fix 2):
// one structured line per admission decision + one per eviction re-read. Kept in the wiring layer
// so the residency package stays free of a log/slog dependency.
type slogAdmissionLogger struct{ log *slog.Logger }

func (l slogAdmissionLogger) AdmissionDecision(d residency.AdmissionLog) {
	if l.log == nil {
		return
	}
	victims := make([]string, 0, len(d.Evictions))
	for _, e := range d.Evictions {
		victims = append(victims, fmt.Sprintf("%s(freed_expected=%d,free_after_reread=%d)",
			e.Victim, e.FreedExpectedMiB, e.FreeAfterReReadMiB))
	}
	l.log.Info("residency admission decision",
		"model", d.Model, "engine", d.Engine, "decision", d.Decision, "class", d.Class,
		"footprint_mib", d.FootprintMiB, "need_mib", d.NeedMiB, "margin_mib", d.MarginMiB,
		"free_mib", d.FreeBeforeMiB, "free_after_mib", d.FreeAfterMiB,
		"evictions", victims, "reason", d.Reason)
}

func (l slogAdmissionLogger) EvictionReRead(e residency.EvictionLog) {
	if l.log == nil {
		return
	}
	l.log.Info("residency eviction re-read",
		"victim", e.Victim, "freed_expected_mib", e.FreedExpectedMiB,
		"freed_measured_mib", e.FreedMeasuredMiB, "free_after_reread_mib", e.FreeAfterReReadMiB)
}

// durSecs converts a seconds int (0 = unset) to a Duration (0 → the manager applies its default).
func durSecs(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// residencyVRAMProbe adapts the hardware probe into residency.FreeVRAMProbe (GPU 0 free VRAM in
// MiB). Admission calls it EVERY decision and after EVERY eviction (never a cached counter).
type residencyVRAMProbe struct{ probe hardware.Probe }

func (p residencyVRAMProbe) FreeVRAMMiB(ctx context.Context) (int, error) {
	snap, err := p.probe.Snapshot(ctx)
	if err != nil {
		return 0, err
	}
	if len(snap.GPUs) == 0 {
		return 0, fmt.Errorf("no GPU in snapshot")
	}
	return snap.GPUs[0].VRAMFreeMiB, nil
}

// residencyRAMProbe adapts the hardware probe into residency.FreeRAMProbe (free SYSTEM RAM in MiB)
// for the RAM-floor admission guard (fix 4). Admission probes it FRESH at decision time (like the
// VRAM re-read). Snapshot always yields a valid RAM struct even with no GPU present.
type residencyRAMProbe struct{ probe hardware.Probe }

func (p residencyRAMProbe) FreeRAMMiB(ctx context.Context) (int, error) {
	snap, err := p.probe.Snapshot(ctx)
	if err != nil {
		return 0, err
	}
	return snap.RAM.FreeMiB, nil
}

// residencyGPUProbe adapts the hardware probe into residency.GPUStateProbe (GPU 0 core temperature
// in Celsius + utilization in percent) for the GPU thermal/utilization admission ceilings. One
// Snapshot read serves both ceilings. The hardware layer reports -1 for an unavailable temp/util
// field; we pass that through as a negative reading, which the residency guard treats as
// not-triggering (fail-open on a probe gap rather than wedging admission). Values are float64 in the
// snapshot; we round toward zero to int since the ceilings are whole degrees / whole percent.
type residencyGPUProbe struct{ probe hardware.Probe }

func (p residencyGPUProbe) GPUState(ctx context.Context) (tempC, utilPct int, err error) {
	snap, err := p.probe.Snapshot(ctx)
	if err != nil {
		return 0, 0, err
	}
	if len(snap.GPUs) == 0 {
		// No GPU in the snapshot: report unavailable (negative) so the ceilings don't trigger.
		return -1, -1, nil
	}
	return int(snap.GPUs[0].TemperatureC), int(snap.GPUs[0].UtilizationPct), nil
}

// ollamaFreeVRAMProbe adapts the hardware probe into the ollama.VRAMProbe seam (GPU 0 free VRAM in
// MiB) for the Ollama unload VRAM-delta gate. Mirrors llamacpp.FreeVRAMFromProbe.
type ollamaFreeVRAMProbe struct{ probe hardware.Probe }

func (p ollamaFreeVRAMProbe) FreeVRAMMiB(ctx context.Context) (int, error) {
	snap, err := p.probe.Snapshot(ctx)
	if err != nil {
		return 0, err
	}
	if len(snap.GPUs) == 0 {
		return 0, fmt.Errorf("no GPU in snapshot")
	}
	return snap.GPUs[0].VRAMFreeMiB, nil
}
