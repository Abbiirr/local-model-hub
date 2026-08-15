// Package config loads and validates the LocalHub daemon configuration.
//
// The config file format mirrors examples/config.example.yaml (kept in sync with
// configs/config.example.yaml). All defaults are chosen to work on the reference box
// (Windows 10, RTX 4060 Ti, Ollama at 127.0.0.1:11434, llama-server on PATH) with zero
// edits — the "5-minute install" promise from docs/security.md.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the fully-parsed, validated daemon configuration.
type Config struct {
	Server        ServerConfig        `yaml:"server"`
	Data          DataConfig          `yaml:"data"`
	Downloads     DownloadsConfig     `yaml:"downloads"`
	Hardware      HardwareConfig      `yaml:"hardware"`
	Engines       EnginesConfig       `yaml:"engines"`
	Compatibility CompatibilityConfig `yaml:"compatibility"`
	Residency     ResidencyConfig     `yaml:"residency"`
	Security      SecurityConfig      `yaml:"security"`
	Logging       LoggingConfig       `yaml:"logging"`
	Jobs          JobsConfig          `yaml:"jobs"`
	Cluster       ClusterConfig       `yaml:"cluster"`
}

// ClusterConfig is the optional LAN-federation block (P7.1, ADR-008 / docs/cluster-plan.md).
// Role "off" (the default) is a strict zero: no routes, no goroutines, byte-identical to a
// single-box 1.0 daemon (cluster-plan §1.3). The same binary is either a coordinator (accepts
// node join/heartbeat, exposes /api/cluster/*) or a node (dials a coordinator and heartbeats),
// selected here — never a separate binary (cluster-plan §0 decision 1, §2.2).
type ClusterConfig struct {
	// Role selects the cluster behavior: "off" | "coordinator" | "node" (cluster-plan §0.1).
	Role string `yaml:"role"`
	// CoordinatorURL is where a node dials the coordinator (node mode only), e.g.
	// "http://box-a.tailnet.ts.net:39876" (cluster-plan §2.4). Required when role=node.
	CoordinatorURL string `yaml:"coordinator_url"`
	// Token is the shared cluster bearer, DISTINCT from server.admin_token (cluster-plan §4.2):
	// the admin token is one box's human credential; the cluster token is the machine credential
	// shared by the federation. Gates every cluster-plane call. Required for role=node, and
	// mandatory whenever a clustered role binds non-loopback (validation-enforced; §4.4).
	Token string `yaml:"cluster_token"`
	// NodeName is the human label the coordinator shows for this daemon ("basement-4090").
	// Defaults to the OS hostname (normalize()).
	NodeName string `yaml:"node_name"`
	// AdvertiseURL is how the coordinator reaches THIS node back — the node's non-loopback bind
	// or tailnet address, explicitly configured and never guessed from the socket peer address
	// (cluster-plan §2.4, NAT/proxy-safe). Empty => derived from server.bind:port (fine for the
	// loopback two-daemon dev rig; must be set for a real second box).
	AdvertiseURL string `yaml:"advertise_url"`
	// HeartbeatIntervalSeconds is the node's join+heartbeat cadence and the unit the coordinator
	// derives ONLINE/STALE/UNREACHABLE from (cluster-plan §2.4). Default 10; 0 => 10.
	HeartbeatIntervalSeconds int `yaml:"heartbeat_interval_seconds"`
}

// JobsConfig gates the P6.2 async-inference-jobs worker (design §2.3). Enabled by default; a
// gate mirroring residency.enabled so an operator can turn the background job worker off (the
// jobs table + REST endpoints still exist, but no worker drains them). When false, main.go skips
// `go svc.RunJobWorker(ctx)` and submitted jobs sit `queued` until the feature is re-enabled.
type JobsConfig struct {
	Enabled bool `yaml:"enabled"`
}

type ServerConfig struct {
	Bind string `yaml:"bind"`
	Port int    `yaml:"port"`
	// AdminToken is an optional single bearer token for LAN/headless use
	// (docs/security.md §Identity/auth). nil/empty = localhost-trust only.
	AdminToken *string `yaml:"admin_token"`
	// TrustTailscaleHeaders: trust identity headers ONLY when bound to loopback and
	// fronted by Tailscale Serve (docs/security.md). Ignored on non-loopback binds.
	TrustTailscaleHeaders bool `yaml:"trust_tailscale_headers"`
	// AllowNonlocal is a DANGEROUS opt-in. LocalHub can free/allocate GPU memory and
	// stop model processes; exposing it off 127.0.0.1 puts admin controls on the
	// network with no auth by default (docs/security.md threat model #2). Binding to a
	// non-loopback address hard-fails validation unless this is explicitly true.
	AllowNonlocal bool `yaml:"allow_nonlocal"`
}

type DataConfig struct {
	DBPath    string   `yaml:"db_path"`
	ModelDirs []string `yaml:"model_dirs"`
	// DataDir is where the daemon writes runtime artifacts it owns: per-instance engine log
	// files (P4.2, size-capped), and future scratch. Empty => a "logs" dir beside the DB.
	DataDir string `yaml:"data_dir"`
}

// DownloadsConfig tunes the download safeguards in internal/manifest/guards.go. Both fields
// are byte counts; 0 means "use the built-in constant" so an existing config.yaml that omits
// this block keeps today's behavior exactly.
//
// These were hardcoded constants (HardCapBytes 8 GiB, DiskHeadroomBytes 5 GiB) whose comment
// read "K: has only ~24 GB free". That premise goes stale: the guard kept refusing 10 GiB 27B
// quants long after the disk had 72 GiB free. The headroom check below is the safeguard that
// actually tracks reality — it is measured against live free disk at request time — so MaxBytes
// is a coarse sanity ceiling, not the primary protection.
type DownloadsConfig struct {
	// MaxBytes is the absolute per-download ceiling. A download whose total exceeds this is
	// refused regardless of free disk. 0 => manifest.HardCapBytes.
	MaxBytes int64 `yaml:"max_bytes"`
	// DiskHeadroomBytes is the free space that must REMAIN after a download completes. This is
	// the guard that adapts to actual conditions. 0 => manifest.DiskHeadroomBytes.
	DiskHeadroomBytes int64 `yaml:"disk_headroom_bytes"`
}

type HardwareConfig struct {
	NVIDIA              bool `yaml:"nvidia"`
	ROCm                bool `yaml:"rocm"`
	PollIntervalSeconds int  `yaml:"poll_interval_seconds"`
}

type EnginesConfig struct {
	Ollama   OllamaConfig   `yaml:"ollama"`
	LlamaCPP LlamaCPPConfig `yaml:"llamacpp"`
	VLLM     VLLMConfig     `yaml:"vllm"`
}

type OllamaConfig struct {
	Enabled bool   `yaml:"enabled"`
	BaseURL string `yaml:"base_url"`
	// MirrorIntervalSeconds is how often the /api/ps → instances mirror runs (P4.3). It keeps the
	// dashboard's Ollama instance rows in sync with the daemon's resident set (models loaded via
	// `ollama run`, idle-unloaded by the daemon, etc.). Default 10s; 0 => 10.
	MirrorIntervalSeconds int `yaml:"mirror_interval_seconds"`
}

type LlamaCPPConfig struct {
	Enabled bool `yaml:"enabled"`
	// BinaryPath is the primary llama-server binary. BinaryPaths is the ordered
	// candidate list (PLAN.md: winget llama-server on PATH + the CUDA fork at
	// K:\projects\llm_models\llama.cpp-tq3\build\bin\llama-server.exe). BinaryPath, if
	// set, is prepended to BinaryPaths for backward-compat with the example config.
	BinaryPath  string   `yaml:"binary_path"`
	BinaryPaths []string `yaml:"binary_paths"`
	PortRange   [2]int   `yaml:"port_range"`
	// HealthTimeoutSeconds bounds STARTING→HEALTHY when spawning a model (P4.2). Big models
	// load slowly; default 120s. 0 => 120.
	HealthTimeoutSeconds int `yaml:"health_timeout_seconds"`
	// ExtraPath is a list of directories prepended to PATH when probing/spawning the
	// explicit-path (absolute) llama-server candidates, so their runtime DLLs load. The CUDA
	// fork at K:\projects\llm_models\llama.cpp-tq3\build\bin\llama-server.exe needs
	// K:\Softwares\Nvidia GPU Compiling Toolkit\CUDA\v12.5\bin on PATH to load its CUDA DLLs
	// at --version/--help probe time (orchestrator sign-off item 3; wiring-p13.md). It is
	// applied ONLY to absolute-path candidates (via llamacpp.NewFromPaths); the bare-name
	// winget build on PATH is unaffected.
	ExtraPath []string `yaml:"extra_path"`
	// RemoteModels are already-running single-model llama.cpp servers on this LAN/tailnet.
	// LocalHub does not start, stop, admit, or account their GPU: it verifies /health plus
	// /v1/models identity and relays inference through the same LocalHub /v1 surface.
	// This is deliberately llama.cpp-specific static configuration, not provider routing.
	RemoteModels []RemoteLlamaCPPModelConfig `yaml:"remote_models"`
}

// RemoteLlamaCPPModelConfig gives a remote single-model llama.cpp server a stable LocalHub ID.
// UpstreamModel is the exact model identifier advertised by that server; LocalHub rewrites the
// client's stable ID to it before relay so clients never need the remote filesystem path.
type RemoteLlamaCPPModelConfig struct {
	ID            string `yaml:"id"`
	BaseURL       string `yaml:"base_url"`
	UpstreamModel string `yaml:"upstream_model"`
}

type VLLMConfig struct {
	Enabled bool `yaml:"enabled"`
	// Mode selects how the adapter reaches vLLM. Per PLAN.md resolution 4, vLLM has no
	// native Windows support: on this box only "docker" (spawn container) or "attach"
	// (pre-existing endpoint) are usable. "subprocess" is Linux-only.
	Mode                 string  `yaml:"mode"`
	BinaryPath           string  `yaml:"binary_path"`
	AttachURL            string  `yaml:"attach_url"`
	PortRange            [2]int  `yaml:"port_range"`
	DefaultGPUMemoryUtil float64 `yaml:"default_gpu_memory_utilization"`
	// ExtraArgs are vLLM engine flags appended after the image/model for docker mode and
	// after the positional model for subprocess mode. Use them only for model-specific
	// runtime requirements that cannot be expressed by the admission plan, for example
	// `--skip-mm-profiling` on a constrained multimodal deployment.
	ExtraArgs []string `yaml:"extra_args"`
	// ExtraDockerArgs are DOCKER-level args (mounts, env, etc.) inserted before the image name
	// in `docker run` for mode=docker. Use for e.g. mounting an HF cache off the C: drive
	// (`-v K:\hf-cache:/root/.cache/huggingface`) or setting env (`-e HF_HUB_ENABLE_HF_TRANSFER=0`).
	// These are distinct from the vLLM engine flags, which are appended AFTER the image name.
	// Ignored in attach mode. Added P4.5 (real docker E2E) to keep the model download off C:.
	ExtraDockerArgs []string `yaml:"extra_docker_args"`
}

type CompatibilityConfig struct {
	SafetyFactor   float64 `yaml:"safety_factor"`
	DefaultContext int     `yaml:"default_context"`
	DefaultBatch   int     `yaml:"default_batch"`
	VLLMOverheadMB int     `yaml:"vllm_overhead_mb"`
}

type ResidencyConfig struct {
	Enabled                  bool `yaml:"enabled"`
	SafetyMarginMB           int  `yaml:"safety_margin_mb"`
	DefaultTTLSeconds        int  `yaml:"default_ttl_seconds"`
	NeverKillActiveByDefault bool `yaml:"never_kill_active_by_default"`
	// DrainTimeoutSeconds bounds a user-initiated stop of a busy instance (DRAINING): after this
	// long with active requests still in flight, the instance is stopped anyway (documented
	// override; default 300s). Eviction NEVER reaches this — it never selects an active instance.
	DrainTimeoutSeconds int `yaml:"drain_timeout_seconds"`
	// DrainIntervalSeconds is the periodic re-admit cadence for the production queue-drain loop
	// (Phase 5 fix 1): every tick the drain loop re-Admits queued requests and auto-starts any
	// that now fit. The loop ALSO fires on instance stop/evict/TTL events. Default 10s.
	DrainIntervalSeconds int `yaml:"drain_interval_seconds"`
	// VRAMBudgetMB caps the VRAM admission may treat as available for OUR instances (0 = disabled
	// → use real free VRAM). Admission's supply becomes min(probe_free, budget − our footprints).
	// Purpose: ops VRAM reservation AND storm testing (a tight budget forces eviction/queue
	// pressure without physically filling the card).
	VRAMBudgetMB int `yaml:"vram_budget_mib"`
	// MinFreeRAMMiB is the system-RAM floor (fix 4, full-project-review finding 4). Admission
	// refuses to start a NEW process/warm (llama.cpp spawn, Ollama warm, vLLM docker) when FRESH
	// free system RAM is below this, queueing with a numeric ram_blocked reason surfaced exactly
	// like VRAM shortages. On a 16 GB-RAM box RAM binds before VRAM (four llama-server processes
	// drove free RAM to ~0.24 GB while VRAM never OOMed). This is a GLOBAL guard — not a per-model
	// RAM estimate and not RAM-based eviction victim selection. Default 2048; 0 disables the check.
	MinFreeRAMMiB int `yaml:"min_free_ram_mib"`
	// MaxGPUTempC is the GPU-temperature admission ceiling (Celsius). When the FRESH GPU core
	// temperature at admission time is above this, admission refuses to start/warm a NEW model,
	// queueing with a numeric gpu_hot reason; the drain loop re-admits once the card cools. This is
	// an admission-time guard only — a RUNNING model is never killed for crossing it (never-kill-
	// active). Default 87: the RTX 4060 Ti's NVIDIA-spec Maximum GPU Temperature is 90C
	// (https://www.nvidia.com/en-us/geforce/graphics-cards/40-series/rtx-4060-4060ti/) and it begins
	// thermal-throttling around 83C, so 87 leaves headroom below the hard max while still allowing
	// normal warm operation. 0 disables the check. Valid: 0 or 60..100.
	MaxGPUTempC int `yaml:"max_gpu_temp_c"`
	// MaxGPUUtilPct is the GPU-utilization admission ceiling (percent, 0..100). When the FRESH GPU
	// utilization at admission time is above this, admission defers the start the same way (gpu_busy
	// reason, drain re-admits when it falls). Disabled by default (0): starting a model while the GPU
	// is momentarily busy is often legitimate. Set it (e.g. 80) when the box doubles as a gaming/
	// render machine and you want model loads to wait for the GPU to be mostly free. Valid: 0..100.
	MaxGPUUtilPct int `yaml:"max_gpu_util_pct"`
	// Queue tunes the custom priority queue (P5.0, docs/residency-priority-queue.md). Zero-value
	// fields fall back to the documented defaults (60/120s aging, coalesce 4, caps 64/256/512).
	Queue QueueTuning `yaml:"queue"`
}

// QueueTuning are the priority-queue algorithm knobs (all optional; documented defaults apply).
type QueueTuning struct {
	AgePromoteNormalSeconds     int `yaml:"age_promote_normal_seconds"`     // normal→interactive-equiv (default 60)
	AgePromoteBackgroundSeconds int `yaml:"age_promote_background_seconds"` // background→normal-equiv (default 120)
	CoalesceMax                 int `yaml:"coalesce_max"`                   // bounded same-model coalescing (default 4)
	CapInteractive              int `yaml:"cap_interactive"`                // WAITING cap (default 64)
	CapNormal                   int `yaml:"cap_normal"`                     // WAITING cap (default 256)
	CapBackground               int `yaml:"cap_background"`                 // WAITING cap (default 512)
}

type SecurityConfig struct {
	StrictModelArtifactMode bool `yaml:"strict_model_artifact_mode"`
	WarnOnPickle            bool `yaml:"warn_on_pickle"`
	WarnOnTrustRemoteCode   bool `yaml:"warn_on_trust_remote_code"`
}

type LoggingConfig struct {
	// Level is one of: debug, info, warn, error. Default info.
	Level string `yaml:"level"`
	// Format is "text" or "json". Default text.
	Format string `yaml:"format"`
	// File, when non-empty, is a path the daemon appends logs to IN ADDITION to stderr.
	// Relative paths resolve against the process working directory.
	//
	// This exists because localhub.exe is built for the Windows GUI subsystem
	// (-H=windowsgui) so it never opens a console window. With no console, writes to
	// stderr go to an invalid handle and are silently discarded — without a file sink the
	// daemon would run completely mute. Empty is still valid (tests and non-Windows builds
	// use it); it just means "stderr only".
	File string `yaml:"file"`
}

// Default returns a Config populated with the reference-box defaults. These match
// examples/config.example.yaml and must run on this box with zero edits.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Bind:                  "127.0.0.1",
			Port:                  39876,
			AdminToken:            nil,
			TrustTailscaleHeaders: true,
			AllowNonlocal:         false,
		},
		Data: defaultDataConfig(),
		Downloads: DownloadsConfig{
			// 24 GiB: comfortably clears the largest single-file quant of a 27B (Q8_0 ~26.7 GB
			// is still refused, which is intentional on a 16 GB box), while the disk-headroom
			// check does the real work of protecting free space.
			MaxBytes:          24 << 30,
			DiskHeadroomBytes: 5 << 30,
		},
		Hardware: HardwareConfig{
			NVIDIA:              true,
			ROCm:                false,
			PollIntervalSeconds: 1,
		},
		Engines: EnginesConfig{
			Ollama: OllamaConfig{
				// Deprecated: Ollama is no longer a LocalHub runtime. Existing
				// registry blobs must be re-imported as standalone GGUF files.
				Enabled:               false,
				BaseURL:               "http://127.0.0.1:11434",
				MirrorIntervalSeconds: 10,
			},
			LlamaCPP: LlamaCPPConfig{
				Enabled: true,
				// winget llama-server is on PATH; the CUDA fork is the second candidate.
				BinaryPath: "llama-server",
				BinaryPaths: []string{
					`K:\projects\llm_models\llama.cpp-tq3\build\bin\llama-server.exe`,
				},
				ExtraPath: []string{
					`K:\Softwares\Nvidia GPU Compiling Toolkit\CUDA\v12.5\bin`,
				},
				PortRange:            [2]int{40100, 40200},
				HealthTimeoutSeconds: 120,
			},
			VLLM: VLLMConfig{
				Enabled:              true,
				Mode:                 "docker",
				BinaryPath:           "vllm",
				AttachURL:            "",
				PortRange:            [2]int{40201, 40300},
				DefaultGPUMemoryUtil: 0.90,
			},
		},
		Compatibility: CompatibilityConfig{
			SafetyFactor:   0.92,
			DefaultContext: 8192,
			DefaultBatch:   4,
			VLLMOverheadMB: 2048,
		},
		Residency: ResidencyConfig{
			Enabled:                  true,
			SafetyMarginMB:           1024,
			DefaultTTLSeconds:        900,
			NeverKillActiveByDefault: true,
			DrainTimeoutSeconds:      300,
			DrainIntervalSeconds:     10,
			VRAMBudgetMB:             0,    // disabled by default → real free VRAM
			MinFreeRAMMiB:            2048, // RAM floor: don't start a new process/warm below 2 GiB free (fix 4)
			MaxGPUTempC:              87,   // GPU-temp ceiling: don't start/warm above 87C (RTX 4060 Ti max 90C, throttles ~83C); 0 disables
			MaxGPUUtilPct:            0,    // GPU-util ceiling disabled by default (starting while GPU busy is often legitimate)
			// Queue tuning left zero → the documented defaults apply (60/120s aging, coalesce 4,
			// caps 64/256/512). See docs/residency-priority-queue.md.
		},
		Security: SecurityConfig{
			StrictModelArtifactMode: false,
			WarnOnPickle:            true,
			WarnOnTrustRemoteCode:   true,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
		},
		Jobs: JobsConfig{
			Enabled: true, // async inference jobs on by default (design §2.3)
		},
		Cluster: ClusterConfig{
			// Off by default: a fresh daemon is a single box, byte-identical to 1.0 (ADR-008,
			// cluster-plan §1.3). Clustering is opt-in per box.
			Role:                     "off",
			HeartbeatIntervalSeconds: 10,
		},
	}
}

// defaultDataConfig keeps the reference-box paths on Windows while giving a
// service launched from an arbitrary Linux working directory stable, writable
// XDG locations. Operators can always override these in YAML.
func defaultDataConfig() DataConfig {
	if runtime.GOOS == "windows" {
		return DataConfig{DBPath: "./localhub.db", ModelDirs: []string{`K:\projects\llm_models\models`}}
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			state = filepath.Join(home, ".local", "state")
		} else {
			state = "/var/lib"
		}
	}
	base := filepath.Join(state, "localhub")
	return DataConfig{DBPath: filepath.Join(base, "localhub.db"), ModelDirs: []string{filepath.Join(base, "models")}, DataDir: base}
}

// validClusterRoles are the cluster modes the daemon understands (cluster-plan §0.1).
var validClusterRoles = map[string]bool{"off": true, "coordinator": true, "node": true}

// DefaultHeartbeatIntervalSeconds is the fallback join/heartbeat cadence (cluster-plan §2.4).
const DefaultHeartbeatIntervalSeconds = 10

// validVLLMModes are the launch modes the adapter understands (PLAN.md resolution 4).
var validVLLMModes = map[string]bool{"docker": true, "attach": true, "subprocess": true}

var validLogLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
var validLogFormats = map[string]bool{"text": true, "json": true}

// Load reads a YAML config from path (expanding "~"), merging it over Default() so an
// absent or partial file still yields a working config, then validates it. An empty
// path returns Default() validated.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config %q: %w", path, err)
		}
		// Unmarshal over the defaults: YAML only overwrites keys that are present.
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config %q: %w", path, err)
		}
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// normalize expands paths and folds legacy fields into canonical form.
func (c *Config) normalize() {
	c.Data.DBPath = expandHome(c.Data.DBPath)
	for i, d := range c.Data.ModelDirs {
		c.Data.ModelDirs[i] = expandHome(d)
	}
	// Fold the single binary_path into the ordered candidate list, de-duplicated,
	// primary first.
	lc := &c.Engines.LlamaCPP
	if lc.BinaryPath != "" {
		merged := make([]string, 0, len(lc.BinaryPaths)+1)
		seen := map[string]bool{}
		for _, p := range append([]string{lc.BinaryPath}, lc.BinaryPaths...) {
			if p != "" && !seen[p] {
				seen[p] = true
				merged = append(merged, p)
			}
		}
		lc.BinaryPaths = merged
	}
	for i := range lc.RemoteModels {
		lc.RemoteModels[i].ID = strings.TrimSpace(lc.RemoteModels[i].ID)
		lc.RemoteModels[i].BaseURL = strings.TrimRight(strings.TrimSpace(lc.RemoteModels[i].BaseURL), "/")
		lc.RemoteModels[i].UpstreamModel = strings.TrimSpace(lc.RemoteModels[i].UpstreamModel)
	}
	c.Logging.Level = strings.ToLower(strings.TrimSpace(c.Logging.Level))
	c.Logging.Format = strings.ToLower(strings.TrimSpace(c.Logging.Format))
	c.Engines.VLLM.Mode = strings.ToLower(strings.TrimSpace(c.Engines.VLLM.Mode))

	// Cluster normalization (P7.1). An empty/omitted role folds to "off" so a config that never
	// mentions clustering behaves byte-identically to 1.0 (cluster-plan §1.3). node_name defaults
	// to the OS hostname; heartbeat cadence folds to the documented default.
	cl := &c.Cluster
	cl.Role = strings.ToLower(strings.TrimSpace(cl.Role))
	if cl.Role == "" {
		cl.Role = "off"
	}
	cl.CoordinatorURL = strings.TrimSpace(cl.CoordinatorURL)
	cl.AdvertiseURL = strings.TrimSpace(cl.AdvertiseURL)
	cl.NodeName = strings.TrimSpace(cl.NodeName)
	if cl.NodeName == "" {
		if hn, err := os.Hostname(); err == nil {
			cl.NodeName = hn
		}
	}
	if cl.HeartbeatIntervalSeconds <= 0 {
		cl.HeartbeatIntervalSeconds = DefaultHeartbeatIntervalSeconds
	}
}

// Validate returns the first configuration error found. The loopback-bind check is the
// most important gate: LocalHub is an admin surface for GPU memory and must not land on
// a routable interface by accident (docs/security.md; TESTING.md#p11).
func (c *Config) Validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d out of range 1..65535", c.Server.Port)
	}
	if strings.TrimSpace(c.Server.Bind) == "" {
		return fmt.Errorf("server.bind must not be empty")
	}
	if c.Downloads.MaxBytes < 0 {
		return fmt.Errorf("downloads.max_bytes must not be negative (got %d); use 0 for the built-in default", c.Downloads.MaxBytes)
	}
	if c.Downloads.DiskHeadroomBytes < 0 {
		return fmt.Errorf("downloads.disk_headroom_bytes must not be negative (got %d); use 0 for the built-in default", c.Downloads.DiskHeadroomBytes)
	}
	if err := validateBind(c.Server.Bind, c.Server.AllowNonlocal); err != nil {
		return err
	}
	if strings.TrimSpace(c.Data.DBPath) == "" {
		return fmt.Errorf("data.db_path must not be empty")
	}
	if c.Hardware.PollIntervalSeconds < 1 {
		return fmt.Errorf("hardware.poll_interval_seconds must be >= 1")
	}
	if c.Engines.Ollama.Enabled {
		return fmt.Errorf("engines.ollama is retired: import or download a standalone GGUF and run it with llamacpp instead")
	}
	if c.Engines.LlamaCPP.Enabled {
		if err := validatePortRange("engines.llamacpp.port_range", c.Engines.LlamaCPP.PortRange); err != nil {
			return err
		}
		if len(c.Engines.LlamaCPP.BinaryPaths) == 0 {
			return fmt.Errorf("engines.llamacpp: no binary_path/binary_paths configured")
		}
	}
	if err := validateRemoteLlamaCPPModels(c.Engines.LlamaCPP.Enabled, c.Engines.LlamaCPP.RemoteModels); err != nil {
		return err
	}
	if c.Engines.VLLM.Enabled {
		if !validVLLMModes[c.Engines.VLLM.Mode] {
			return fmt.Errorf("engines.vllm.mode %q invalid (want docker|attach|subprocess)", c.Engines.VLLM.Mode)
		}
		if c.Engines.VLLM.Mode == "attach" && strings.TrimSpace(c.Engines.VLLM.AttachURL) == "" {
			return fmt.Errorf("engines.vllm.mode=attach requires engines.vllm.attach_url")
		}
		if err := validatePortRange("engines.vllm.port_range", c.Engines.VLLM.PortRange); err != nil {
			return err
		}
	}
	if c.Compatibility.SafetyFactor <= 0 || c.Compatibility.SafetyFactor > 1 {
		return fmt.Errorf("compatibility.safety_factor %.3f must be in (0,1]", c.Compatibility.SafetyFactor)
	}
	if c.Compatibility.DefaultContext < 1 {
		return fmt.Errorf("compatibility.default_context must be >= 1")
	}
	if t := c.Residency.MaxGPUTempC; t != 0 && (t < 60 || t > 100) {
		return fmt.Errorf("residency.max_gpu_temp_c %d out of range: use 0 (disabled) or 60..100 C", t)
	}
	if u := c.Residency.MaxGPUUtilPct; u < 0 || u > 100 {
		return fmt.Errorf("residency.max_gpu_util_pct %d out of range 0..100", u)
	}
	if !validLogLevels[c.Logging.Level] {
		return fmt.Errorf("logging.level %q invalid (want debug|info|warn|error)", c.Logging.Level)
	}
	if !validLogFormats[c.Logging.Format] {
		return fmt.Errorf("logging.format %q invalid (want text|json)", c.Logging.Format)
	}
	if err := c.validateCluster(); err != nil {
		return err
	}
	return nil
}

func validateRemoteLlamaCPPModels(enabled bool, models []RemoteLlamaCPPModelConfig) error {
	if len(models) > 0 && !enabled {
		return fmt.Errorf("engines.llamacpp.remote_models requires engines.llamacpp.enabled=true")
	}
	seen := make(map[string]bool, len(models))
	for i, model := range models {
		field := fmt.Sprintf("engines.llamacpp.remote_models[%d]", i)
		if model.ID == "" {
			return fmt.Errorf("%s.id must not be empty", field)
		}
		if seen[model.ID] {
			return fmt.Errorf("%s.id %q is duplicated; remote model IDs must be unique", field, model.ID)
		}
		seen[model.ID] = true
		if model.UpstreamModel == "" {
			return fmt.Errorf("%s.upstream_model must not be empty", field)
		}
		u, err := url.Parse(model.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%s.base_url %q must be an absolute http(s) URL", field, model.BaseURL)
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return fmt.Errorf("%s.base_url %q must contain only scheme and host (no credentials, path, query, or fragment)", field, model.BaseURL)
		}
	}
	return nil
}

// validateCluster enforces the P7.1 cluster rules (cluster-plan §4.2, §4.4). role=off is a
// strict zero — no cluster requirements apply, so a non-clustered daemon is validated exactly as
// 1.0. For clustered roles the security posture changes (this is the first feature to leave
// localhost): a node needs a coordinator to dial and the shared token; ANY clustered role on a
// non-loopback bind must carry the cluster token (the validation-enforced trio: non-loopback bind
// + allow_nonlocal already gated by validateBind, PLUS cluster_token here). P7.5 hardens this
// further (both-sides per-endpoint scoping, negative-auth suites); P7.1 enforces this slice.
func (c *Config) validateCluster() error {
	cl := c.Cluster
	if !validClusterRoles[cl.Role] {
		return fmt.Errorf("cluster.role %q invalid (want off|coordinator|node)", cl.Role)
	}
	if cl.Role == "off" {
		return nil // zero behavioral change; nothing else to check
	}
	if cl.Role == "node" {
		// A node must know where to dial and must present the shared credential (cluster-plan §2.4,
		// §4.2). Missing coordinator_url is the #1 setup mistake; give a readable reason.
		if cl.CoordinatorURL == "" {
			return fmt.Errorf("cluster.coordinator_url must be set when cluster.role=node (the URL of the coordinator to join)")
		}
		if cl.Token == "" {
			return fmt.Errorf("cluster.cluster_token must be set when cluster.role=node (the shared cluster bearer, distinct from server.admin_token)")
		}
	}
	// Mandatory-token-on-non-loopback (cluster-plan §4.4): there is no unauthenticated cluster mode
	// off 127.0.0.1. The loopback two-daemon dev rig MAY run tokenless (coordinator side); once the
	// bind is routable the token is required so a clustered admin surface never lands unauthenticated
	// on the LAN — the same hard-fail philosophy as validateBind.
	if !isLoopbackBind(c.Server.Bind) && cl.Token == "" {
		return fmt.Errorf(
			"cluster.cluster_token must be set when cluster.role=%q on a non-loopback bind %q "+
				"(refusing to expose the cluster plane unauthenticated off 127.0.0.1 — see docs/security.md, docs/cluster-plan.md §4.4)",
			cl.Role, c.Server.Bind)
	}
	return nil
}

// isLoopbackBind reports whether bind is a recognized loopback address/name. Mirrors the accept
// set of validateBind (IP loopback literals and the "localhost" name) so the cluster non-loopback
// token rule keys off the same notion of "off 127.0.0.1" as the bind guard.
func isLoopbackBind(bind string) bool {
	if ip := net.ParseIP(strings.TrimSpace(bind)); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(strings.TrimSpace(bind), "localhost")
}

// validateBind enforces loopback-only unless allowNonlocal is explicitly set. A hostname
// that does not resolve to a loopback literal is treated as non-loopback and rejected.
func validateBind(bind string, allowNonlocal bool) error {
	ip := net.ParseIP(bind)
	if ip != nil {
		if ip.IsLoopback() {
			return nil
		}
		if allowNonlocal {
			return nil
		}
		return fmt.Errorf(
			"server.bind %q is not a loopback address; refusing to expose admin controls off 127.0.0.1. "+
				"Set server.allow_nonlocal: true to override (DANGEROUS: no auth by default — see docs/security.md)",
			bind)
	}
	// Non-IP literal (hostname like "localhost"): only accept the well-known loopback name.
	if strings.EqualFold(bind, "localhost") {
		return nil
	}
	if allowNonlocal {
		return nil
	}
	return fmt.Errorf(
		"server.bind %q is not a recognized loopback address; refusing to bind. "+
			"Use 127.0.0.1 (or ::1), or set server.allow_nonlocal: true to override (DANGEROUS)",
		bind)
}

func validatePortRange(field string, r [2]int) error {
	if r[0] == 0 && r[1] == 0 {
		return nil // unset range is allowed; port allocation is a later phase
	}
	if r[0] < 1 || r[1] > 65535 || r[0] > r[1] {
		return fmt.Errorf("%s %v invalid (want lo<=hi within 1..65535)", field, r)
	}
	return nil
}

// expandHome replaces a leading "~" with the user's home directory. Paths that cannot be
// expanded are returned unchanged.
func expandHome(p string) string {
	if p == "" || (p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`)) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}
