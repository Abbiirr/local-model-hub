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
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the fully-parsed, validated daemon configuration.
type Config struct {
	Server        ServerConfig        `yaml:"server"`
	Data          DataConfig          `yaml:"data"`
	Hardware      HardwareConfig      `yaml:"hardware"`
	Engines       EnginesConfig       `yaml:"engines"`
	Compatibility CompatibilityConfig `yaml:"compatibility"`
	Residency     ResidencyConfig     `yaml:"residency"`
	Security      SecurityConfig      `yaml:"security"`
	Logging       LoggingConfig       `yaml:"logging"`
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
		Data: DataConfig{
			DBPath: "./localhub.db",
			ModelDirs: []string{
				`K:\projects\llm_models\models`,
			},
		},
		Hardware: HardwareConfig{
			NVIDIA:              true,
			ROCm:                false,
			PollIntervalSeconds: 1,
		},
		Engines: EnginesConfig{
			Ollama: OllamaConfig{
				Enabled:               true,
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
			VRAMBudgetMB:             0, // disabled by default → real free VRAM
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
	}
}

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
	c.Logging.Level = strings.ToLower(strings.TrimSpace(c.Logging.Level))
	c.Logging.Format = strings.ToLower(strings.TrimSpace(c.Logging.Format))
	c.Engines.VLLM.Mode = strings.ToLower(strings.TrimSpace(c.Engines.VLLM.Mode))
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
	if err := validateBind(c.Server.Bind, c.Server.AllowNonlocal); err != nil {
		return err
	}
	if strings.TrimSpace(c.Data.DBPath) == "" {
		return fmt.Errorf("data.db_path must not be empty")
	}
	if c.Hardware.PollIntervalSeconds < 1 {
		return fmt.Errorf("hardware.poll_interval_seconds must be >= 1")
	}
	if c.Engines.Ollama.Enabled && strings.TrimSpace(c.Engines.Ollama.BaseURL) == "" {
		return fmt.Errorf("engines.ollama.base_url must be set when ollama is enabled")
	}
	if c.Engines.LlamaCPP.Enabled {
		if err := validatePortRange("engines.llamacpp.port_range", c.Engines.LlamaCPP.PortRange); err != nil {
			return err
		}
		if len(c.Engines.LlamaCPP.BinaryPaths) == 0 {
			return fmt.Errorf("engines.llamacpp: no binary_path/binary_paths configured")
		}
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
	if !validLogLevels[c.Logging.Level] {
		return fmt.Errorf("logging.level %q invalid (want debug|info|warn|error)", c.Logging.Level)
	}
	if !validLogFormats[c.Logging.Format] {
		return fmt.Errorf("logging.format %q invalid (want text|json)", c.Logging.Format)
	}
	return nil
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
