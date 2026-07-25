package instances

import (
	"encoding/json"
	"time"
)

// Engine identifies which engine owns an instance's process. Mirrors engines.EngineKind
// values (ollama | llamacpp | vllm) but kept as a plain string here so this package does not
// import internal/engines (avoids a cycle: the llamacpp adapter imports this package).
type Engine string

const (
	EngineLlamaCPP Engine = "llamacpp"
	EngineOllama   Engine = "ollama"
	EngineVLLM     Engine = "vllm"
)

// Instance is the in-memory view of one `instances` row plus its P4 lifecycle columns
// (migration 0004). It is the shape the state machine reads/writes and the API projects.
// Times are stored as SQLite TEXT and parsed on read; zero time means the column was NULL.
type Instance struct {
	ID      string `json:"id"`
	ModelID string `json:"model_id"`
	Engine  Engine `json:"engine"`
	State   State  `json:"state"`

	PID     int    `json:"pid,omitempty"`      // 0 for ollama / not-yet-spawned
	Port    int    `json:"port,omitempty"`     // 0 for ollama / not-yet-allocated
	BaseURL string `json:"base_url,omitempty"` // http://127.0.0.1:PORT

	// Identity + reproducibility (recorded at spawn; reconciliation compares these).
	BinaryPath string `json:"binary_path,omitempty"`
	ModelPath  string `json:"model_path,omitempty"`
	Ctx        int    `json:"ctx,omitempty"`

	// VRAM accounting.
	VRAMEstMiB   int `json:"vram_est_mib,omitempty"`   // estimate at admission (instances.vram_bytes_est, MiB)
	VRAMObsMiB   int `json:"vram_obs_mib,omitempty"`   // observed peak while HEALTHY (instances.vram_bytes_obs, MiB)
	VRAMFreedMiB int `json:"vram_freed_mib,omitempty"` // measured NVML delta reclaimed on stop (gate)

	Pinned      bool     `json:"pinned"`
	Priority    int      `json:"priority"`
	TTLSeconds  int      `json:"ttl_seconds,omitempty"`
	LaunchFlags []string `json:"launch_flags,omitempty"` // from launch_flags_json (reproducibility)

	// Reason is the latest human-readable state reason (mandatory on every transition).
	Reason string `json:"reason,omitempty"`

	CreatedAt  time.Time `json:"created_at,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"` // when it went HEALTHY
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	StoppedAt  time.Time `json:"stopped_at,omitempty"`
}

// launchFlagsJSON marshals LaunchFlags to the instances.launch_flags_json text form. Returns
// "" (→ NULL) for an empty slice so a not-yet-launched row stores NULL, not "[]".
func launchFlagsJSON(flags []string) string {
	if len(flags) == 0 {
		return ""
	}
	b, err := json.Marshal(flags)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseLaunchFlags decodes launch_flags_json back into a slice ([] on empty/invalid).
func parseLaunchFlags(s string) []string {
	if s == "" {
		return nil
	}
	var flags []string
	if err := json.Unmarshal([]byte(s), &flags); err != nil {
		return nil
	}
	return flags
}

// Transition is one persisted state change (an instance_transitions row).
type Transition struct {
	InstanceID string    `json:"instance_id"`
	From       State     `json:"from_state,omitempty"` // empty for the first (REQUESTED) row
	To         State     `json:"to_state"`
	Reason     string    `json:"reason"`
	At         time.Time `json:"at"`
}
