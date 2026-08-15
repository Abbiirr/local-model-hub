package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default() must validate cleanly, got: %v", err)
	}
	if cfg.Server.Bind != "127.0.0.1" {
		t.Errorf("default bind = %q, want 127.0.0.1", cfg.Server.Bind)
	}
	if cfg.Server.Port != 39876 {
		t.Errorf("default port = %d, want 39876", cfg.Server.Port)
	}
}

func TestLoadEmptyPathReturnsDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error: %v", err)
	}
	if cfg.Server.Port != 39876 {
		t.Errorf("port = %d, want default 39876", cfg.Server.Port)
	}
}

func TestLoadMergesOverDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	// Only override the port; everything else must fall back to defaults.
	if err := os.WriteFile(p, []byte("server:\n  port: 45000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Server.Port != 45000 {
		t.Errorf("port = %d, want 45000", cfg.Server.Port)
	}
	if cfg.Server.Bind != "127.0.0.1" {
		t.Errorf("bind = %q, want default 127.0.0.1 (merge over defaults)", cfg.Server.Bind)
	}
	if cfg.Engines.Ollama.Enabled {
		t.Error("ollama must be disabled by default after the llama.cpp migration")
	}
}

func TestValidateRejectsRetiredOllamaRuntime(t *testing.T) {
	cfg := Default()
	cfg.Engines.Ollama.Enabled = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted retired Ollama runtime")
	}
}

func TestLoadVLLMExtraArgs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "vllm.yaml")
	if err := os.WriteFile(p, []byte("engines:\n  vllm:\n    extra_args: [--skip-mm-profiling, --trust-remote-code]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	want := []string{"--skip-mm-profiling", "--trust-remote-code"}
	if len(cfg.Engines.VLLM.ExtraArgs) != len(want) {
		t.Fatalf("extra_args = %#v, want %#v", cfg.Engines.VLLM.ExtraArgs, want)
	}
	for i := range want {
		if cfg.Engines.VLLM.ExtraArgs[i] != want[i] {
			t.Errorf("extra_args[%d] = %q, want %q", i, cfg.Engines.VLLM.ExtraArgs[i], want[i])
		}
	}
}

func TestValidateBindLoopbackOnly(t *testing.T) {
	tests := []struct {
		name          string
		bind          string
		allowNonlocal bool
		wantErr       bool
	}{
		{"loopback v4", "127.0.0.1", false, false},
		{"loopback v6", "::1", false, false},
		{"localhost name", "localhost", false, false},
		{"nonlocal rejected", "0.0.0.0", false, true},
		{"lan ip rejected", "192.168.1.50", false, true},
		{"nonlocal allowed with flag", "0.0.0.0", true, false},
		{"lan ip allowed with flag", "192.168.1.50", true, false},
		{"unknown hostname rejected", "myhost.local", false, true},
		{"unknown hostname allowed with flag", "myhost.local", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBind(tc.bind, tc.allowNonlocal)
			if tc.wantErr && err == nil {
				t.Errorf("validateBind(%q, %v) = nil, want error", tc.bind, tc.allowNonlocal)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateBind(%q, %v) = %v, want nil", tc.bind, tc.allowNonlocal, err)
			}
		})
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"port too low", func(c *Config) { c.Server.Port = 0 }},
		{"port too high", func(c *Config) { c.Server.Port = 70000 }},
		{"empty bind", func(c *Config) { c.Server.Bind = "" }},
		{"nonlocal bind no flag", func(c *Config) { c.Server.Bind = "0.0.0.0" }},
		{"empty db path", func(c *Config) { c.Data.DBPath = "" }},
		{"bad poll interval", func(c *Config) { c.Hardware.PollIntervalSeconds = 0 }},
		{"safety factor > 1", func(c *Config) { c.Compatibility.SafetyFactor = 1.5 }},
		{"safety factor <= 0", func(c *Config) { c.Compatibility.SafetyFactor = 0 }},
		{"bad log level", func(c *Config) { c.Logging.Level = "verbose" }},
		{"bad log format", func(c *Config) { c.Logging.Format = "xml" }},
		{"bad vllm mode", func(c *Config) { c.Engines.VLLM.Mode = "spawn" }},
		{"vllm attach without url", func(c *Config) { c.Engines.VLLM.Mode = "attach"; c.Engines.VLLM.AttachURL = "" }},
		{"bad llamacpp port range", func(c *Config) { c.Engines.LlamaCPP.PortRange = [2]int{500, 100} }},
		{"gpu temp below sane range", func(c *Config) { c.Residency.MaxGPUTempC = 40 }},
		{"gpu temp above sane range", func(c *Config) { c.Residency.MaxGPUTempC = 120 }},
		{"gpu util negative", func(c *Config) { c.Residency.MaxGPUUtilPct = -1 }},
		{"gpu util over 100", func(c *Config) { c.Residency.MaxGPUUtilPct = 101 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate() = nil, want error for %s", tc.name)
			}
		})
	}
}

// TestValidateAcceptsGPUCeilingBoundaries: the disabled sentinel (0) and the inclusive range
// boundaries for the GPU ceilings must validate cleanly (temp 0/60/100, util 0/100), and the
// default (temp 87, util 0) already validates via TestDefaultIsValid.
func TestValidateAcceptsGPUCeilingBoundaries(t *testing.T) {
	cases := []struct {
		name string
		temp int
		util int
	}{
		{"both disabled", 0, 0},
		{"temp low bound", 60, 0},
		{"temp high bound", 100, 0},
		{"util high bound", 0, 100},
		{"default-ish", 87, 80},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Residency.MaxGPUTempC = tc.temp
			cfg.Residency.MaxGPUUtilPct = tc.util
			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil for temp=%d util=%d", err, tc.temp, tc.util)
			}
		})
	}
}

func TestNormalizeFoldsBinaryPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	yaml := `
engines:
  llamacpp:
    enabled: true
    binary_path: "primary-server"
    binary_paths:
      - "fallback-a"
      - "primary-server"
`
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	got := cfg.Engines.LlamaCPP.BinaryPaths
	want := []string{"primary-server", "fallback-a"}
	if len(got) != len(want) {
		t.Fatalf("BinaryPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("BinaryPaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRemoteLlamaCPPModelConfigLoadsNormalizesAndValidates(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "remote.yaml")
	yaml := `
engines:
  llamacpp:
    enabled: true
    remote_models:
      - id: " kat "
        base_url: "http://192.168.0.157:8080/"
        upstream_model: " /models/kat.gguf "
`
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load remote config: %v", err)
	}
	got := cfg.Engines.LlamaCPP.RemoteModels
	if len(got) != 1 {
		t.Fatalf("RemoteModels = %+v", got)
	}
	if got[0].ID != "kat" || got[0].BaseURL != "http://192.168.0.157:8080" ||
		got[0].UpstreamModel != "/models/kat.gguf" {
		t.Errorf("remote normalization failed: %+v", got[0])
	}
}

func TestRemoteLlamaCPPModelConfigRefusesUnsafeOrAmbiguousEntries(t *testing.T) {
	tests := []struct {
		name   string
		models []RemoteLlamaCPPModelConfig
	}{
		{"missing id", []RemoteLlamaCPPModelConfig{{BaseURL: "http://127.0.0.1:8080", UpstreamModel: "m"}}},
		{"missing upstream", []RemoteLlamaCPPModelConfig{{ID: "m", BaseURL: "http://127.0.0.1:8080"}}},
		{"bad scheme", []RemoteLlamaCPPModelConfig{{ID: "m", BaseURL: "file:///tmp/server", UpstreamModel: "m"}}},
		{"credentials", []RemoteLlamaCPPModelConfig{{ID: "m", BaseURL: "http://user:pass@127.0.0.1:8080", UpstreamModel: "m"}}},
		{"path", []RemoteLlamaCPPModelConfig{{ID: "m", BaseURL: "http://127.0.0.1:8080/admin", UpstreamModel: "m"}}},
		{"duplicate id", []RemoteLlamaCPPModelConfig{
			{ID: "m", BaseURL: "http://127.0.0.1:8080", UpstreamModel: "m1"},
			{ID: "m", BaseURL: "http://127.0.0.1:8081", UpstreamModel: "m2"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Engines.LlamaCPP.RemoteModels = tc.models
			cfg.normalize()
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got := expandHome("~/models"); got != filepath.Join(home, "models") {
		t.Errorf("expandHome(~/models) = %q, want %q", got, filepath.Join(home, "models"))
	}
	if got := expandHome("/abs/path"); got != "/abs/path" {
		t.Errorf("expandHome(/abs/path) = %q, want unchanged", got)
	}
}

// TestClusterDefaultOff pins the zero-behavioral-change default (ADR-008, cluster-plan §1.3).
func TestClusterDefaultOff(t *testing.T) {
	cfg := Default()
	if cfg.Cluster.Role != "off" {
		t.Fatalf("default cluster.role = %q, want off", cfg.Cluster.Role)
	}
	if cfg.Cluster.HeartbeatIntervalSeconds != DefaultHeartbeatIntervalSeconds {
		t.Errorf("default heartbeat interval = %d, want %d", cfg.Cluster.HeartbeatIntervalSeconds, DefaultHeartbeatIntervalSeconds)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default (role=off) must validate: %v", err)
	}
}

// TestClusterValidation covers the P7.1 rules (cluster-plan §4.2, §4.4): node needs a coordinator
// URL + token; any clustered role on a non-loopback bind needs the token; role=off is unconstrained.
func TestClusterValidation(t *testing.T) {
	base := func() Config { return Default() } // valid single-box baseline

	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
	}{
		{"off is unconstrained", func(c *Config) { c.Cluster.Role = "off" }, false},
		{"invalid role", func(c *Config) { c.Cluster.Role = "leader" }, true},
		{"node missing coordinator_url", func(c *Config) {
			c.Cluster.Role = "node"
			c.Cluster.Token = "tok"
		}, true},
		{"node missing token", func(c *Config) {
			c.Cluster.Role = "node"
			c.Cluster.CoordinatorURL = "http://127.0.0.1:39921"
		}, true},
		{"node complete on loopback", func(c *Config) {
			c.Cluster.Role = "node"
			c.Cluster.CoordinatorURL = "http://127.0.0.1:39921"
			c.Cluster.Token = "tok"
		}, false},
		{"coordinator loopback tokenless ok", func(c *Config) {
			c.Cluster.Role = "coordinator"
		}, false},
		{"coordinator non-loopback without token refused", func(c *Config) {
			c.Server.Bind = "192.168.1.50"
			c.Server.AllowNonlocal = true
			c.Cluster.Role = "coordinator"
		}, true},
		{"coordinator non-loopback with token ok", func(c *Config) {
			c.Server.Bind = "192.168.1.50"
			c.Server.AllowNonlocal = true
			c.Cluster.Role = "coordinator"
			c.Cluster.Token = "tok"
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			tt.mutate(&cfg)
			cfg.normalize()
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("expected valid config, got: %v", err)
			}
		})
	}
}
