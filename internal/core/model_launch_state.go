package core

import (
	"context"
	"fmt"
)

// model_launch_state.go is the FOUR-STATE per-model residency policy (migration 0015), generalizing
// the migration-0014 boolean auto_load_disabled decommission flag. See the migration's doc comment
// for the full state table and the RAM-exhaustion incident that motivated it.
//
// The four states are about RESIDENCY/EAGERNESS — whether a model sits loaded in VRAM — never about
// visibility: every state except unavailable auto-loads on an incoming request exactly like every
// model did before this feature, and unavailable is the ONLY state that hides a model from
// GET /v1/models (identical to 0014's rule). Nothing here adds a second list-hiding rule.
//
//	launch_state           loaded idle? evictable?  idle-TTL unload?  auto-loads on request?
//	always_available        yes    NO (pinned)        NO                yes
//	available                yes    yes                NO                yes
//	available_on_request    NO     yes                YES               yes
//	unavailable              NO     n/a                n/a               NEVER (manual POST
//	                                                                      /api/instances only)
//
// available_on_request is the DEFAULT for every model (existing and new).

const (
	launchStateAlwaysAvailable    = "always_available"
	launchStateAvailable          = "available"
	launchStateAvailableOnRequest = "available_on_request"
	launchStateUnavailable        = "unavailable"
)

// validLaunchStates is the closed set PUT /api/models/{id}/launch-state accepts. A value outside
// this set is refused with a 400 listing every valid option (CLAUDE.md: every refusal carries a
// human-readable reason).
var validLaunchStates = map[string]bool{
	launchStateAlwaysAvailable:    true,
	launchStateAvailable:          true,
	launchStateAvailableOnRequest: true,
	launchStateUnavailable:        true,
}

// validLaunchStatesList is validLaunchStates rendered as a stable, human-readable list for error
// messages (map iteration order is not stable, so this is spelled out explicitly rather than built
// by ranging the map).
const validLaunchStatesList = "always_available, available, available_on_request, unavailable"

// SetModelLaunchState sets a model's four-state residency policy (PUT /api/models/{id}/launch-state
// {"state": "..."}). Refuses an unknown model_id explicitly (matching SetModelAutoLoad's stance)
// rather than silently no-op'ing an UPDATE that matched zero rows, and refuses a value outside the
// four known states with a message listing them.
func (s *Service) SetModelLaunchState(ctx context.Context, modelID, state string) (ModelSummary, error) {
	if s.deps.DB == nil {
		return ModelSummary{}, fmt.Errorf("database not configured")
	}
	if modelID == "" {
		return ModelSummary{}, fmt.Errorf("set model launch state: model_id is required")
	}
	if !validLaunchStates[state] {
		return ModelSummary{}, fmt.Errorf(
			"set model launch state: invalid state %q — must be one of: %s", state, validLaunchStatesList)
	}
	res, err := s.deps.DB.ExecContext(ctx,
		`UPDATE models SET launch_state = ?, updated_at = datetime('now') WHERE id = ?`,
		state, modelID)
	if err != nil {
		return ModelSummary{}, fmt.Errorf("set model launch state for %q: %w", modelID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ModelSummary{}, fmt.Errorf("set model launch state: model %q is not in the inventory", modelID)
	}
	m, ferr := s.modelSummaryByID(ctx, modelID)
	if ferr != nil {
		return ModelSummary{}, ferr
	}
	s.deps.Events.Publish(Event{Type: EventModelsUpdated})
	return m, nil
}

// launchStateFor returns the four-state residency policy for modelID, defaulting to
// available_on_request (today's default behavior for every model) when the model has no row in the
// catalog, the DB is unreachable, or the stored value is somehow outside the known set. Fail-open on
// the LOOKUP itself mirrors autoLoadDisabledReason's stance (model_auto_load.go): a lookup failure
// here is an environment problem this feature should not be blamed for, and it must never make a
// normal model behave as if it had an exotic residency policy it never asked for.
//
// This is also correct for a model with NO row in the `models` table at all (e.g. a config-declared
// remote llama.cpp model) — it simply gets the default policy, same as before this feature existed.
func (s *Service) launchStateFor(ctx context.Context, modelID string) string {
	if s.deps.DB == nil || modelID == "" {
		return launchStateAvailableOnRequest
	}
	var state string
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT COALESCE(launch_state,'') FROM models WHERE id = ?`, modelID).Scan(&state)
	if err != nil || !validLaunchStates[state] {
		return launchStateAvailableOnRequest
	}
	return state
}

// residencyPolicyForLaunchState derives the pin-override and idle-TTL a start should use from a
// model's launch_state. It is a PURE function (no DB) so the mapping is unit-testable without a
// service — StartInstance/startOllamaInstance/startVLLMInstance all call it the same way right
// before building their engine-specific start spec.
//
//   - always_available: forcePin=true (wired into the EXISTING Pinned mechanism — eviction's
//     never-evict-pinned gate, internal/residency/eviction.go classifyVictim, already refuses a
//     pinned instance, so no separate eviction-exemption path is needed), ttlSeconds=0 (exempt from
//     idle-TTL unload — eviction's ttlExpired treats <=0 as "never expires by TTL", the SAME existing
//     mechanism, not a new one).
//   - available: forcePin=false (a caller-requested pin still applies on top), ttlSeconds=0 (exempt
//     from idle-TTL unload, but — unlike always_available — still evictable under VRAM pressure,
//     since it is not pinned).
//   - available_on_request (and unavailable, and anything unrecognized): normal idle-TTL
//     (cfgTTLSeconds), not forced pinned. unavailable never reaches an auto-start (refused earlier by
//     autoLoadDisabledReason), so this branch only matters for an EXPLICIT manual start of an
//     unavailable model, which behaves exactly like every model did before this feature.
func residencyPolicyForLaunchState(state string, cfgTTLSeconds int) (ttlSeconds int, forcePin bool) {
	switch state {
	case launchStateAlwaysAvailable:
		return 0, true
	case launchStateAvailable:
		return 0, false
	default:
		return cfgTTLSeconds, false
	}
}
