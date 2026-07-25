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
	if cfg.Engines.Ollama.BaseURL != "http://127.0.0.1:11434" {
		t.Errorf("ollama base_url = %q, want default", cfg.Engines.Ollama.BaseURL)
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
