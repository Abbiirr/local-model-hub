package startupconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadStartupConfigCreatesSafeDefaults(t *testing.T) {
	root := t.TempDir()
	got, err := LoadStartupConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultStartupConfig()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %#v, want %#v", got, want)
	}
	b, err := os.ReadFile(StartupConfigPath(root))
	if err != nil {
		t.Fatalf("default file was not created: %v", err)
	}
	if !json.Valid(b) {
		t.Fatalf("created startup config is not valid JSON: %s", b)
	}
	if !strings.Contains(string(b), `"version": 2`) {
		t.Fatalf("created startup config is not v2: %s", b)
	}
}

// Deleting configs/startup.json must give the operator an empty machine back. The v1 default
// re-seeded one enabled 27B model, so removing the file silently re-enabled a large load at the
// next login. Ship nothing.
func TestDefaultStartupConfigLoadsNoModels(t *testing.T) {
	cfg := DefaultStartupConfig()
	if !cfg.StartLocalHub || cfg.OpenWebUI || cfg.HealthCheckSeconds <= 0 {
		t.Fatalf("unsafe global defaults: %+v", cfg)
	}
	if cfg.RestoreLastSession {
		t.Error("restore_last_session must default to false: it is startup intent nobody wrote down")
	}
	if len(cfg.Models) != 0 {
		t.Fatalf("models = %#v, want none", cfg.Models)
	}
	if cfg.Version != StartupConfigVersion || StartupConfigVersion != 2 {
		t.Fatalf("version = %d, want 2", cfg.Version)
	}
}

// A v1 file on an existing install must load, not hard-fail on its version, and must come back
// without the canonical_qwen entries whose only mechanism (the PowerShell supervisor) is gone.
func TestLoadStartupConfigUpgradesV1AndStripsCanonicalQwen(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(StartupConfigPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	v1 := `{"version":1,"start_localhub":true,"open_web_ui":false,"restore_last_session":true,` +
		`"startup_delay_seconds":15,"health_check_seconds":45,"models":[` +
		`{"model_id":"gguf-qwen3-6-27b-16gb-vram-mtp-mini-iq4-xs","enabled":true,"order":0,` +
		`"start_delay_seconds":0,"retry_policy":"keep_available","max_attempts":0,` +
		`"use_saved_profile":false,"mode":"canonical_qwen"},` +
		`{"model_id":"keeper","enabled":true,"order":1,"start_delay_seconds":0,` +
		`"retry_policy":"never","max_attempts":0,"use_saved_profile":false,"mode":"localhub_api"}]}`
	if err := os.WriteFile(StartupConfigPath(root), []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadStartupConfig(root)
	if err != nil {
		t.Fatalf("v1 config must upgrade, not fail: %v", err)
	}
	if got.Version != StartupConfigVersion {
		t.Fatalf("version = %d, want %d", got.Version, StartupConfigVersion)
	}
	if len(got.Models) != 1 || got.Models[0].ModelID != "keeper" || got.Models[0].Order != 0 {
		t.Fatalf("models = %#v, want only a densely ordered 'keeper'", got.Models)
	}
	// Unrelated v1 settings survive the upgrade.
	if got.StartupDelaySeconds != 15 || got.HealthCheckSeconds != 45 || !got.RestoreLastSession {
		t.Fatalf("upgrade dropped unrelated settings: %+v", got)
	}
	// The migration is durable: the file on disk is now v2, so it happens once.
	b, err := os.ReadFile(StartupConfigPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"version": 2`) || strings.Contains(string(b), "canonical_qwen") {
		t.Fatalf("upgrade was not written back: %s", b)
	}
	// Loading again is a no-op that still succeeds.
	if _, err := LoadStartupConfig(root); err != nil {
		t.Fatalf("second load after upgrade failed: %v", err)
	}
}

func TestUpgradeToCurrentRejectsFutureVersions(t *testing.T) {
	cfg := StartupConfig{Version: StartupConfigVersion + 1}
	if _, err := cfg.UpgradeToCurrent(); err == nil || !strings.Contains(err.Error(), "unsupported version") {
		t.Fatalf("error = %v, want unsupported version", err)
	}
	current := DefaultStartupConfig()
	changed, err := current.UpgradeToCurrent()
	if err != nil || changed {
		t.Fatalf("current version should be a no-op: changed=%v err=%v", changed, err)
	}
}

func TestSaveLoadRoundTripAndDeterministicOrder(t *testing.T) {
	root := t.TempDir()
	cfg := DefaultStartupConfig()
	cfg.Models = []StartupModelConfig{
		{ModelID: "z-model", Enabled: true, Order: 9, RetryPolicy: RetryLimited, MaxAttempts: 3, Mode: StartupModeLocalHubAPI, Engine: "LLAMACPP", Ctx: 65536, Batch: 8, Priority: -2, Pin: true, KVTypeK: "Q8_0", KVTypeV: "Q4_0", Slots: 1, Speculative: "DRAFT-MTP", SpecDraftMax: 2},
		{ModelID: "a-model", Enabled: true, Order: 9, RetryPolicy: RetryNever, Mode: StartupModeLocalHubAPI, UseSavedProfile: true},
		{ModelID: "first", Enabled: false, Order: 1, RetryPolicy: RetryKeepAvailable, Mode: StartupModeLocalHubAPI, AllowCPUOffload: true},
	}
	if err := SaveStartupConfig(root, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadStartupConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if ids := []string{got.Models[0].ModelID, got.Models[1].ModelID, got.Models[2].ModelID}; !reflect.DeepEqual(ids, []string{"first", "a-model", "z-model"}) {
		t.Fatalf("normalized order = %v", ids)
	}
	for i := range got.Models {
		if got.Models[i].Order != i {
			t.Errorf("models[%d].order = %d", i, got.Models[i].Order)
		}
	}
	if got.Models[2].Engine != "llamacpp" || got.Models[2].KVTypeK != "q8_0" || got.Models[2].Speculative != "draft-mtp" {
		t.Fatalf("enum normalization failed: %+v", got.Models[2])
	}
	// Save normalizes a copy and must not mutate caller-owned ordering.
	if cfg.Models[0].ModelID != "z-model" || cfg.Models[0].Engine != "LLAMACPP" {
		t.Fatalf("SaveStartupConfig mutated its argument: %+v", cfg.Models[0])
	}
}

func TestLoadStartupConfigRejectsUnknownAndTrailingJSON(t *testing.T) {
	tests := []string{
		`{"version":1,"start_localhub":true,"open_web_ui":false,"restore_last_session":true,"startup_delay_seconds":0,"health_check_seconds":5,"models":[],"surprise":true}`,
		`{"version":1,"start_localhub":true,"open_web_ui":false,"restore_last_session":true,"startup_delay_seconds":0,"health_check_seconds":5,"models":[]} {}`,
	}
	for i, body := range tests {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(StartupConfigPath(root)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(StartupConfigPath(root), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadStartupConfig(root); err == nil {
			t.Fatalf("case %d: expected invalid JSON contract to fail", i)
		}
	}
}

func TestValidateAndNormalizeRejectsInvalidConfigs(t *testing.T) {
	valid := func() StartupConfig {
		return StartupConfig{Version: StartupConfigVersion, HealthCheckSeconds: 5, Models: []StartupModelConfig{{ModelID: "m", Enabled: true, RetryPolicy: RetryNever, Mode: StartupModeLocalHubAPI}}}
	}
	tests := []struct {
		name string
		edit func(*StartupConfig)
		want string
	}{
		{"version", func(c *StartupConfig) { c.Version++ }, "unsupported version"},
		{"global delay", func(c *StartupConfig) { c.StartupDelaySeconds = -1 }, "startup_delay_seconds"},
		{"health interval", func(c *StartupConfig) { c.HealthCheckSeconds = 0 }, "health_check_seconds"},
		{"duplicate model", func(c *StartupConfig) { c.Models = append(c.Models, c.Models[0]) }, "duplicate model_id"},
		{"blank model", func(c *StartupConfig) { c.Models[0].ModelID = "  " }, "model_id is required"},
		{"negative order", func(c *StartupConfig) { c.Models[0].Order = -1 }, "order must"},
		{"model delay", func(c *StartupConfig) { c.Models[0].StartDelaySeconds = 3601 }, "start_delay_seconds"},
		{"retry enum", func(c *StartupConfig) { c.Models[0].RetryPolicy = "sometimes" }, "invalid retry_policy"},
		{"limited attempts low", func(c *StartupConfig) { c.Models[0].RetryPolicy = RetryLimited }, "requires max_attempts"},
		{"limited attempts high", func(c *StartupConfig) { c.Models[0].RetryPolicy = RetryLimited; c.Models[0].MaxAttempts = 101 }, "requires max_attempts"},
		{"unbounded attempts conflict", func(c *StartupConfig) { c.Models[0].MaxAttempts = 2 }, "requires max_attempts=0"},
		{"mode enum", func(c *StartupConfig) { c.Models[0].Mode = "script" }, "invalid mode"},
		{"empty mode", func(c *StartupConfig) { c.Models[0].Mode = "" }, "invalid mode"},
		// canonical_qwen is no longer a mode. A hand-edited v2 file naming it must be refused
		// outright rather than silently accepted; upgrading v1 is the only way it is tolerated.
		{"retired canonical mode", func(c *StartupConfig) { c.Models[0].Mode = "canonical_qwen" }, "invalid mode"},
		{"engine enum", func(c *StartupConfig) { c.Models[0].Engine = "ollama" }, "invalid engine"},
		{"ctx bound", func(c *StartupConfig) { c.Models[0].Ctx = maxContext + 1 }, "ctx must"},
		{"batch bound", func(c *StartupConfig) { c.Models[0].Batch = -1 }, "batch must"},
		{"priority bound", func(c *StartupConfig) { c.Models[0].Priority = maxPriority + 1 }, "priority must"},
		{"slots bound", func(c *StartupConfig) { c.Models[0].Slots = 33 }, "slots must"},
		{"kv k enum", func(c *StartupConfig) { c.Models[0].KVTypeK = "tq3_0" }, "kv_type_k"},
		{"kv v enum", func(c *StartupConfig) { c.Models[0].KVTypeV = "int8" }, "kv_type_v"},
		{"spec enum", func(c *StartupConfig) { c.Models[0].Speculative = "draft" }, "speculative"},
		{"draft bound", func(c *StartupConfig) { c.Models[0].Speculative = "draft-mtp"; c.Models[0].SpecDraftMax = 17 }, "spec_draft_max"},
		{"draft without spec", func(c *StartupConfig) { c.Models[0].SpecDraftMax = 2 }, "requires speculative"},
		{"vllm llama fields", func(c *StartupConfig) { c.Models[0].Engine = "vllm"; c.Models[0].Slots = 1 }, "llama.cpp-only"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.edit(&cfg)
			if err := cfg.ValidateAndNormalize(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestSaveStartupConfigDoesNotReplaceValidFileOnValidationFailure(t *testing.T) {
	root := t.TempDir()
	if err := SaveStartupConfig(root, DefaultStartupConfig()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(StartupConfigPath(root))
	if err != nil {
		t.Fatal(err)
	}
	bad := DefaultStartupConfig()
	bad.Version = 99
	if err := SaveStartupConfig(root, bad); err == nil {
		t.Fatal("expected invalid config save to fail")
	}
	after, err := os.ReadFile(StartupConfigPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("invalid save changed the existing startup config")
	}
	temps, err := filepath.Glob(filepath.Join(root, "configs", ".startup-*.json.tmp"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("temporary files left behind: %v (err=%v)", temps, err)
	}
}
