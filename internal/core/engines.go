package core

import (
	"context"
	"time"

	"github.com/localhub/localhub/internal/engines"
)

// EngineStatusView is the API projection of engines.EngineStatus for GET /api/engines. The
// api-spec.md §3 tuple is [{name, installed, version, running, base_url}]; we also carry
// `reason` (non-waivable — every verdict/status carries a human-readable why) and `details`
// (backends, caps, running models, images) for the dashboard.
type EngineStatusView struct {
	Name      string         `json:"name"`      // ollama | llamacpp | vllm
	Installed bool           `json:"installed"` // engines.EngineStatus.Detected
	Version   string         `json:"version,omitempty"`
	Running   bool           `json:"running"`  // engines.EngineStatus.Reachable
	BaseURL   string         `json:"base_url"` // engines.EngineStatus.Endpoint
	Reason    string         `json:"reason"`   // ALWAYS present
	Details   map[string]any `json:"details,omitempty"`
}

// Per-engine detection timeouts. One slow engine must never stall the whole sweep, so each
// Detect runs under its own child context. HTTP probes (Ollama/vLLM attach) are quick;
// binary and docker probes exec a subprocess and get more headroom.
const (
	ollamaDetectTimeout   = 3 * time.Second
	llamacppDetectTimeout = 10 * time.Second
	vllmDetectTimeout     = 8 * time.Second
)

// Engines runs the read-only detection sweep across the configured adapters and returns the
// API projection. A nil adapter (engine disabled in config) yields a single Installed=false
// entry with a Reason. llama.cpp yields one entry per candidate binary (all named
// "llamacpp"; disambiguate via details.binary_path).
//
// This is strictly read-only: every underlying Detect performs GET-only HTTP and
// version/help/image subprocess probes. No engine is started or mutated (P1.3 gate).
func (s *Service) Engines(ctx context.Context) []EngineStatusView {
	var out []EngineStatusView
	reg := s.deps.Engines

	// Ollama (single daemon → single status).
	if reg.Ollama != nil {
		cctx, cancel := context.WithTimeout(ctx, ollamaDetectTimeout)
		st := reg.Ollama.Detect(cctx)
		cancel()
		out = append(out, engineStatusToView(st))
	} else {
		out = append(out, disabledEngineView(engines.KindOllama))
	}

	// llama.cpp (one status per candidate binary).
	if reg.LlamaCPP != nil {
		cctx, cancel := context.WithTimeout(ctx, llamacppDetectTimeout)
		for _, st := range reg.LlamaCPP.Detect(cctx) {
			out = append(out, engineStatusToView(st))
		}
		cancel()
	} else {
		out = append(out, disabledEngineView(engines.KindLlamaCPP))
	}

	// vLLM (single status: attach endpoint or docker image presence).
	if reg.VLLM != nil {
		cctx, cancel := context.WithTimeout(ctx, vllmDetectTimeout)
		st := reg.VLLM.Detect(cctx)
		cancel()
		out = append(out, engineStatusToView(st))
	} else {
		out = append(out, disabledEngineView(engines.KindVLLM))
	}

	return out
}

// DetectEngines runs the same read-only sweep as Engines but ALSO announces an
// engines.updated event on the change bus, so live transports (WS) tell clients to refetch
// /api/engines. It backs POST /api/engines/detect (a re-probe), keeping the plain GET
// /api/engines free of side effects. Still zero-mutation: it only re-detects.
func (s *Service) DetectEngines(ctx context.Context) []EngineStatusView {
	views := s.Engines(ctx)
	s.deps.Events.Publish(Event{Type: EventEnginesUpdated})
	return views
}

func engineStatusToView(st engines.EngineStatus) EngineStatusView {
	return EngineStatusView{
		Name:      string(st.Kind),
		Installed: st.Detected,
		Version:   st.Version,
		Running:   st.Reachable,
		BaseURL:   st.Endpoint,
		Reason:    st.Reason,
		Details:   st.Details,
	}
}

func disabledEngineView(kind engines.EngineKind) EngineStatusView {
	return EngineStatusView{
		Name:      string(kind),
		Installed: false,
		Running:   false,
		Reason:    "engine " + string(kind) + " disabled in config",
	}
}
