// Package engines contains the read-only engine detection adapters for LocalHub
// (P1.3): Ollama, llama.cpp (llama-server), and vLLM. This package is DETECTION only —
// strictly read-only. It makes zero mutating calls to any engine: no POST/PUT/DELETE to
// Ollama, no spawning of model processes, no docker run/pull. The only external commands
// it runs are read-only version/help probes (`llama-server --version`, `--help`,
// `docker --version`, `docker images`) which load no model and change no state. Model
// start/stop/evict (mutation) is Phase 4 work in separate code paths.
//
// Ownership asymmetry (engine-adapters.md §5): Ollama is a shared daemon we only observe
// over HTTP; llama.cpp and vLLM are engines LocalHub would spawn (later). Detection
// therefore differs per engine: Ollama is an HTTP probe, llama.cpp is a binary probe, and
// vLLM is either an HTTP attach probe or a docker-image presence check.
//
// Non-waivable rule (CLAUDE.md correctness bar; spec-digest §3): every EngineStatus that
// is not-detected or unreachable MUST carry a human-readable Reason explaining why. A
// Detected=false or Reachable=false status with an empty Reason is a bug.
package engines

import "context"

// EngineKind identifies an engine adapter. These strings match the `engines.name` primary
// key in the SQLite schema (data-model.md) and the `name` field of the GET /api/engines
// response (api-spec.md §3): ollama | llamacpp | vllm.
type EngineKind string

const (
	KindOllama   EngineKind = "ollama"
	KindLlamaCPP EngineKind = "llamacpp"
	KindVLLM     EngineKind = "vllm"
)

// EngineStatus is the normalized result of detecting one engine (or, for llama.cpp, one
// binary — a single llama.cpp config can yield several statuses, one per candidate
// binary). It is the internal shape the core maps onto the GET /api/engines response
// (api-spec.md §3: `[{name, installed, version, running, base_url}]`). The extra fields
// here (Reachable, Reason, Details) carry the "why" and the engine-specific evidence that
// the API projection and the dashboard surface.
//
// Field mapping to the API response:
//   - Kind      -> name
//   - Detected  -> installed   (binary/daemon/config present on this box)
//   - Reachable -> running     (the daemon/endpoint actually answered right now)
//   - Version   -> version
//   - Endpoint  -> base_url
//
// Detected vs Reachable is a deliberate two-axis model:
//   - Detected=true, Reachable=false: the engine is installed (Ollama binary on disk, a
//     llama-server binary present, a vLLM image or attach target configured) but not
//     currently answering. This is a normal, non-error state (e.g. Ollama daemon stopped).
//   - Detected=false: the engine is not present/usable on this box at all.
type EngineStatus struct {
	Kind      EngineKind     `json:"kind"`
	Detected  bool           `json:"detected"`
	Version   string         `json:"version,omitempty"`
	Endpoint  string         `json:"endpoint,omitempty"`
	Reachable bool           `json:"reachable"`
	Details   map[string]any `json:"details,omitempty"`
	// Reason is a human-readable explanation of the status. It is REQUIRED whenever
	// Detected or Reachable is false (non-waivable rule). It may also be populated on a
	// healthy status to describe what was found.
	Reason string `json:"reason"`
}

// Detector is the read-only contract every engine adapter satisfies. Implementations must
// not mutate engine state. The context bounds the probe (callers pass a timeout); a
// canceled/expired context must yield a status with Reachable=false and a Reason, never a
// panic or a hang.
//
// Note the adapters expose their own richer detection entry points (Ollama.Detect,
// llamacpp.Detect(paths), vllm.Detect(mode, endpoint)) because their inputs differ. This
// interface is the common projection the core iterates over when serving GET /api/engines;
// each adapter provides a small closure/wrapper that satisfies it. Kept minimal on purpose
// — mutation methods (start/stop) are intentionally absent until Phase 4.
type Detector interface {
	// Kind returns the engine this detector reports on.
	Kind() EngineKind
	// Detect performs the read-only probe and returns one or more statuses. It returns a
	// slice because a single llama.cpp configuration can produce multiple statuses (one
	// per candidate binary); single-daemon engines return exactly one.
	Detect(ctx context.Context) []EngineStatus
}
