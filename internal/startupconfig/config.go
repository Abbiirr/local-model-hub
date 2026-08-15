package startupconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// StartupConfigVersion is the current on-disk schema version.
	//
	// v2 removed the "canonical_qwen" mode. That mode's only mechanism was shelling out to
	// scripts/keep-localhub-qwen.ps1, which the tray no longer invokes; localhub_api is now the
	// sole way a model is started. v2 also ships with no models at all, so deleting
	// configs/startup.json can never silently re-enable a large model at the next login.
	StartupConfigVersion = 2

	// legacyVersionCanonicalQwen is the v1 schema that could contain canonical_qwen entries.
	// LoadStartupConfig upgrades those files in place instead of rejecting them.
	legacyVersionCanonicalQwen = 1

	// legacyModeCanonicalQwen is retained only so the v1 -> v2 upgrade can recognise and drop
	// the entries it used to write. It is not a valid mode in v2.
	legacyModeCanonicalQwen = "canonical_qwen"

	StartupModeLocalHubAPI = "localhub_api"

	RetryNever         = "never"
	RetryLimited       = "limited"
	RetryKeepAvailable = "keep_available"

	maxStartupDelaySeconds = 3600
	maxHealthCheckSeconds  = 3600
	maxAttempts            = 100
	maxContext             = 1 << 20
	maxBatch               = 4096
	maxPriority            = 1000
)

// StartupConfig is the versioned, durable startup intent used by the Windows tray app.
// It intentionally contains only startup policy; live process state remains authoritative.
type StartupConfig struct {
	Version             int                  `json:"version"`
	StartLocalHub       bool                 `json:"start_localhub"`
	OpenWebUI           bool                 `json:"open_web_ui"`
	RestoreLastSession  bool                 `json:"restore_last_session"`
	StartupDelaySeconds int                  `json:"startup_delay_seconds"`
	HealthCheckSeconds  int                  `json:"health_check_seconds"`
	Models              []StartupModelConfig `json:"models"`
}

// StartupModelConfig describes one ordered model startup action. API launch fields are
// optional and apply only in localhub_api mode. Zero values mean "not supplied" and are
// omitted from JSON, matching POST /api/instances.
type StartupModelConfig struct {
	ModelID           string `json:"model_id"`
	Enabled           bool   `json:"enabled"`
	Order             int    `json:"order"`
	StartDelaySeconds int    `json:"start_delay_seconds"`
	RetryPolicy       string `json:"retry_policy"`
	MaxAttempts       int    `json:"max_attempts"`
	UseSavedProfile   bool   `json:"use_saved_profile"`
	Mode              string `json:"mode"`

	Engine          string `json:"engine,omitempty"`
	Ctx             int    `json:"ctx,omitempty"`
	Batch           int    `json:"batch,omitempty"`
	Priority        int    `json:"priority,omitempty"`
	Pin             bool   `json:"pin,omitempty"`
	KVTypeK         string `json:"kv_type_k,omitempty"`
	KVTypeV         string `json:"kv_type_v,omitempty"`
	Slots           int    `json:"slots,omitempty"`
	Speculative     string `json:"speculative,omitempty"`
	SpecDraftMax    int    `json:"spec_draft_max,omitempty"`
	AllowCPUOffload bool   `json:"allow_cpu_offload,omitempty"`
}

// StartupConfigPath returns the fixed repo-local configuration path.
func StartupConfigPath(repoRoot string) string {
	return filepath.Join(repoRoot, "configs", "startup.json")
}

// DefaultStartupConfig starts the control plane and nothing else. It deliberately configures
// zero models: an operator who deletes configs/startup.json must get an empty machine back, not
// a re-seeded large model. Models are added explicitly from the dashboard's Startup panel.
//
// RestoreLastSession is off for the same reason — "whatever happened to be loaded last time" is
// startup intent the operator never wrote down.
func DefaultStartupConfig() StartupConfig {
	return StartupConfig{
		Version:             StartupConfigVersion,
		StartLocalHub:       true,
		OpenWebUI:           false,
		RestoreLastSession:  false,
		StartupDelaySeconds: 0,
		HealthCheckSeconds:  60,
		Models:              []StartupModelConfig{},
	}
}

// LoadStartupConfig reads, upgrades, validates, and normalizes configs/startup.json. On first use
// it atomically writes and returns safe defaults, making the on-disk contract discoverable.
//
// A file written by an older LocalHub is upgraded and written back, so the migration happens once
// rather than on every read. The upgrade is applied before validation because ValidateAndNormalize
// rejects any version other than the current one.
func LoadStartupConfig(repoRoot string) (StartupConfig, error) {
	path := StartupConfigPath(repoRoot)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg := DefaultStartupConfig()
		if err := SaveStartupConfig(repoRoot, cfg); err != nil {
			return StartupConfig{}, fmt.Errorf("create default startup config: %w", err)
		}
		return cfg, nil
	}
	if err != nil {
		return StartupConfig{}, fmt.Errorf("read startup config %q: %w", path, err)
	}

	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var cfg StartupConfig
	if err := dec.Decode(&cfg); err != nil {
		return StartupConfig{}, fmt.Errorf("decode startup config %q: %w", path, err)
	}
	if err := requireJSONEOF(dec); err != nil {
		return StartupConfig{}, fmt.Errorf("decode startup config %q: %w", path, err)
	}
	upgraded, err := cfg.UpgradeToCurrent()
	if err != nil {
		return StartupConfig{}, fmt.Errorf("upgrade startup config %q: %w", path, err)
	}
	if err := cfg.ValidateAndNormalize(); err != nil {
		return StartupConfig{}, fmt.Errorf("validate startup config %q: %w", path, err)
	}
	if upgraded {
		if err := SaveStartupConfig(repoRoot, cfg); err != nil {
			return StartupConfig{}, fmt.Errorf("persist upgraded startup config %q: %w", path, err)
		}
	}
	return cfg, nil
}

// UpgradeToCurrent migrates an older schema in place and reports whether anything changed.
//
// v1 -> v2 drops every canonical_qwen entry. Those entries named one hardcoded Qwen artifact and
// were executed by a PowerShell supervisor the tray no longer calls, so carrying them forward
// would leave startup intent that nothing can honour. The rest of the v1 document is
// field-compatible with v2 and survives untouched.
//
// An unknown (future) version is an error rather than a silent downgrade: a newer LocalHub may
// have written fields this build would drop on the next save.
func (c *StartupConfig) UpgradeToCurrent() (bool, error) {
	if c == nil {
		return false, fmt.Errorf("startup config is nil")
	}
	if c.Version == StartupConfigVersion {
		return false, nil
	}
	if c.Version != legacyVersionCanonicalQwen {
		return false, fmt.Errorf("unsupported version %d: this build understands %d and %d",
			c.Version, legacyVersionCanonicalQwen, StartupConfigVersion)
	}

	kept := c.Models[:0]
	for _, model := range c.Models {
		if strings.EqualFold(strings.TrimSpace(model.Mode), legacyModeCanonicalQwen) {
			continue
		}
		kept = append(kept, model)
	}
	c.Models = kept
	c.Version = StartupConfigVersion
	return true, nil
}

// SaveStartupConfig validates a copy and atomically replaces configs/startup.json. The
// caller's value is not mutated. The temporary file is created beside the destination so
// rename cannot cross filesystems.
func SaveStartupConfig(repoRoot string, cfg StartupConfig) error {
	if strings.TrimSpace(repoRoot) == "" {
		return fmt.Errorf("repository root is required")
	}
	// StartupConfig is passed by value, but its Models slice still aliases caller memory.
	// Clone it before normalization so sorting and string cleanup cannot surprise the UI.
	cfg.Models = append([]StartupModelConfig(nil), cfg.Models...)
	if err := cfg.ValidateAndNormalize(); err != nil {
		return fmt.Errorf("validate startup config: %w", err)
	}

	destination := StartupConfigPath(repoRoot)
	dir := filepath.Dir(destination)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create startup config directory %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".startup-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temporary startup config: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set temporary startup config permissions: %w", err)
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("encode startup config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("flush startup config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close startup config: %w", err)
	}
	if err := os.Rename(tmpName, destination); err != nil {
		return fmt.Errorf("replace startup config %q: %w", destination, err)
	}
	committed = true
	return nil
}

// ValidateAndNormalize validates the complete schema, trims string fields, sorts models by
// requested order then model ID, and rewrites order to a dense deterministic sequence.
func (c *StartupConfig) ValidateAndNormalize() error {
	if c == nil {
		return fmt.Errorf("startup config is nil")
	}
	if c.Version != StartupConfigVersion {
		return fmt.Errorf("unsupported version %d: expected %d (call UpgradeToCurrent first)",
			c.Version, StartupConfigVersion)
	}
	if c.StartupDelaySeconds < 0 || c.StartupDelaySeconds > maxStartupDelaySeconds {
		return fmt.Errorf("startup_delay_seconds must be between 0 and %d", maxStartupDelaySeconds)
	}
	if c.HealthCheckSeconds < 1 || c.HealthCheckSeconds > maxHealthCheckSeconds {
		return fmt.Errorf("health_check_seconds must be between 1 and %d", maxHealthCheckSeconds)
	}

	seen := make(map[string]struct{}, len(c.Models))
	for i := range c.Models {
		m := &c.Models[i]
		m.ModelID = strings.TrimSpace(m.ModelID)
		m.Mode = strings.ToLower(strings.TrimSpace(m.Mode))
		m.RetryPolicy = strings.ToLower(strings.TrimSpace(m.RetryPolicy))
		m.Engine = strings.ToLower(strings.TrimSpace(m.Engine))
		m.KVTypeK = strings.ToLower(strings.TrimSpace(m.KVTypeK))
		m.KVTypeV = strings.ToLower(strings.TrimSpace(m.KVTypeV))
		m.Speculative = strings.ToLower(strings.TrimSpace(m.Speculative))

		if m.ModelID == "" {
			return fmt.Errorf("models[%d].model_id is required", i)
		}
		if _, exists := seen[m.ModelID]; exists {
			return fmt.Errorf("duplicate model_id %q", m.ModelID)
		}
		seen[m.ModelID] = struct{}{}
		if m.Order < 0 {
			return fmt.Errorf("model %q order must be >= 0", m.ModelID)
		}
		if m.StartDelaySeconds < 0 || m.StartDelaySeconds > maxStartupDelaySeconds {
			return fmt.Errorf("model %q start_delay_seconds must be between 0 and %d", m.ModelID, maxStartupDelaySeconds)
		}
		if err := validateRetry(*m); err != nil {
			return err
		}
		switch m.Mode {
		case StartupModeLocalHubAPI:
			if err := validateAPILaunch(*m); err != nil {
				return fmt.Errorf("model %q: %w", m.ModelID, err)
			}
		default:
			return fmt.Errorf("model %q has invalid mode %q: the only valid value is %q", m.ModelID, m.Mode, StartupModeLocalHubAPI)
		}
	}

	sort.Slice(c.Models, func(i, j int) bool {
		if c.Models[i].Order != c.Models[j].Order {
			return c.Models[i].Order < c.Models[j].Order
		}
		return c.Models[i].ModelID < c.Models[j].ModelID
	})
	for i := range c.Models {
		c.Models[i].Order = i
	}
	if c.Models == nil {
		c.Models = []StartupModelConfig{}
	}
	return nil
}

func validateRetry(m StartupModelConfig) error {
	switch m.RetryPolicy {
	case RetryNever, RetryKeepAvailable:
		if m.MaxAttempts != 0 {
			return fmt.Errorf("model %q retry_policy %q requires max_attempts=0", m.ModelID, m.RetryPolicy)
		}
	case RetryLimited:
		if m.MaxAttempts < 1 || m.MaxAttempts > maxAttempts {
			return fmt.Errorf("model %q retry_policy %q requires max_attempts between 1 and %d", m.ModelID, m.RetryPolicy, maxAttempts)
		}
	default:
		return fmt.Errorf("model %q has invalid retry_policy %q: valid values are %q, %q, and %q", m.ModelID, m.RetryPolicy, RetryNever, RetryLimited, RetryKeepAvailable)
	}
	return nil
}

func validateAPILaunch(m StartupModelConfig) error {
	switch m.Engine {
	case "", "llamacpp", "vllm":
	default:
		return fmt.Errorf("invalid engine %q: valid values are empty, %q, and %q", m.Engine, "llamacpp", "vllm")
	}
	if m.Ctx < 0 || m.Ctx > maxContext {
		return fmt.Errorf("ctx must be 0 (default) or between 1 and %d", maxContext)
	}
	if m.Batch < 0 || m.Batch > maxBatch {
		return fmt.Errorf("batch must be 0 (default) or between 1 and %d", maxBatch)
	}
	if m.Priority < -maxPriority || m.Priority > maxPriority {
		return fmt.Errorf("priority must be between -%d and %d", maxPriority, maxPriority)
	}
	if m.Slots < 0 || m.Slots > 32 {
		return fmt.Errorf("slots must be 0 (default) or between 1 and 32")
	}
	if !oneOf(m.KVTypeK, "", "f16", "q8_0", "q4_0") {
		return fmt.Errorf("kv_type_k %q is invalid: valid values are empty, f16, q8_0, and q4_0", m.KVTypeK)
	}
	if !oneOf(m.KVTypeV, "", "f16", "q8_0", "q4_0") {
		return fmt.Errorf("kv_type_v %q is invalid: valid values are empty, f16, q8_0, and q4_0", m.KVTypeV)
	}
	if !oneOf(m.Speculative, "", "ngram-cache", "draft-mtp") {
		return fmt.Errorf("speculative %q is invalid: valid values are empty, ngram-cache, and draft-mtp", m.Speculative)
	}
	if m.SpecDraftMax < 0 || m.SpecDraftMax > 16 {
		return fmt.Errorf("spec_draft_max must be 0 (default) or between 1 and 16")
	}
	if m.Speculative == "" && m.SpecDraftMax != 0 {
		return fmt.Errorf("spec_draft_max requires speculative mode")
	}
	if m.Engine == "vllm" && (m.KVTypeK != "" || m.KVTypeV != "" || m.Slots != 0 || m.Speculative != "" || m.SpecDraftMax != 0) {
		return fmt.Errorf("llama.cpp-only KV, slots, and speculative fields cannot be used with engine vllm")
	}
	return nil
}

func oneOf(got string, values ...string) bool {
	for _, value := range values {
		if got == value {
			return true
		}
	}
	return false
}

func requireJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("startup config must contain exactly one JSON value")
}
