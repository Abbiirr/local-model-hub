package core

import (
	"context"
	"fmt"
	"time"

	"github.com/localhub/localhub/internal/instances"
)

// proxy.go is Phase 5 fix 3: the minimal completion pass-through the daemon needs so it can COUNT
// in-flight requests per instance (the never-kill-active substrate). Phase 4 exposed instance
// completions only as direct-to-engine-port calls (the base_url on the instance row), which are
// invisible to the daemon — a streaming instance looked idle to eviction. This adds a thin relay
// (POST /api/instances/{id}/completions → the engine's /v1/chat/completions or /completion) purely
// so the daemon increments/decrements residency.InFlightTracker around each request. Eviction reads
// that counter (an instance with in-flight > 0 is never a victim), so a stream through THIS endpoint
// protects a non-pinned instance from eviction — exactly what the storm exercises.
//
// This is also Phase 6's routing substrate: the relay is deliberately thin (byte passthrough,
// streaming intact) so Phase 6's OpenAI proxy can build on it. Keep it minimal here.

// InstanceRequestHandle is the in-flight accounting handle returned by BeginInstanceRequest. The
// caller (the API relay) MUST call Done exactly once when the proxied request completes (success,
// error, or client disconnect) so the counter never leaks.
type InstanceRequestHandle struct {
	// TargetBaseURL is the engine base URL to relay to (e.g. http://127.0.0.1:40130).
	TargetBaseURL string
	// InstanceID is the instance being served.
	InstanceID string
	done       func()
}

// Done marks the in-flight request complete (decrement + last_used_at touch). Idempotent.
func (h *InstanceRequestHandle) Done() {
	if h != nil && h.done != nil {
		h.done()
		h.done = nil
	}
}

// BeginInstanceRequest opens an in-flight request against a HEALTHY instance: it verifies the
// instance is serving, increments the in-flight counter (so eviction will not select it), stamps
// last_used_at, and returns a handle carrying the engine base URL + a Done closure that decrements.
//
// It returns an error when the instance is unknown, not HEALTHY, or has no base URL (e.g. an Ollama
// mirror row with no owned port — completions to Ollama go through Ollama directly in this phase).
func (s *Service) BeginInstanceRequest(ctx context.Context, id string) (*InstanceRequestHandle, error) {
	if s.deps.InstanceStore == nil {
		return nil, fmt.Errorf("instance store not configured")
	}
	inst, err := s.deps.InstanceStore.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if inst.State != instances.StateHealthy {
		return nil, fmt.Errorf("instance %s is %s, not HEALTHY — cannot serve a completion", id, inst.State)
	}
	if inst.BaseURL == "" {
		return nil, fmt.Errorf("instance %s has no base_url to relay to (engine %s exposes no owned port in this phase)", id, inst.Engine)
	}

	// Increment the in-flight counter (never-kill-active substrate). Eviction reads this and will
	// not select an instance with count > 0.
	if s.deps.InFlight != nil {
		s.deps.InFlight.IncrementActive(id)
	}
	// Stamp last_used_at so the Ollama recent-active approximation + LRU are accurate.
	s.touchInstanceLastUsed(context.Background(), id)

	handle := &InstanceRequestHandle{
		TargetBaseURL: inst.BaseURL,
		InstanceID:    id,
		done: func() {
			if s.deps.InFlight != nil {
				s.deps.InFlight.DecrementActive(id)
			}
			// Touch again on completion so idle-time is measured from the LAST byte, not the first.
			s.touchInstanceLastUsed(context.Background(), id)
		},
	}
	return handle, nil
}

// touchInstanceLastUsed stamps last_used_at on an instance without a state change (best-effort). It
// keeps the eviction LRU + Ollama recent-active window accurate for instances served through the
// completion proxy. A short independent context so a disconnected caller does not abort the touch.
func (s *Service) touchInstanceLastUsed(_ context.Context, id string) {
	if s.deps.InstanceStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	now := time.Now()
	_ = s.deps.InstanceStore.TouchLastUsed(ctx, id, now)
}
