package core

import (
	"context"
	"fmt"
)

// model_auto_load.go was the DECOMMISSION feature (migration 0014): mark a model so it is never
// AUTO-loaded (the OpenAI proxy, the residency queue drain/promotion loop, the async jobs worker all
// refuse it) while it remains loadable MANUALLY through an explicit POST /api/instances. Migration
// 0015 (model_launch_state.go) GENERALIZED this boolean into a four-state residency policy —
// launch_state is now the source of truth, and auto_load_disabled (still a real column, migration
// 0014, kept per the additive-migrations rule) is no longer read by any of the functions below. See
// migration 0015's doc comment for the full motivation and state table.

// SetModelAutoLoad is DEPRECATED — use SetModelLaunchState (PUT /api/models/{id}/launch-state)
// instead. Kept working as a back-compat alias for PUT /api/models/{id}/auto-load, which shipped in
// migration 0014 before the four-state policy existed: enabled=false maps to launch_state=unavailable
// (0014's "decommissioned" meaning), enabled=true maps to launch_state=available_on_request (0014's
// "normal" meaning — the default state, NOT always_available/available; this alias can never grant
// eager loading, only restore or remove the old decommission). Refuses an unknown model_id explicitly,
// same as before.
func (s *Service) SetModelAutoLoad(ctx context.Context, modelID string, enabled bool) (ModelSummary, error) {
	state := launchStateAvailableOnRequest
	if !enabled {
		state = launchStateUnavailable
	}
	return s.SetModelLaunchState(ctx, modelID, state)
}

// modelSummaryByID reads one model row in the ModelSummary shape (same columns as Models(), scoped
// to a single id). Used by SetModelAutoLoad to echo the updated row back to the caller.
func (s *Service) modelSummaryByID(ctx context.Context, modelID string) (ModelSummary, error) {
	const q = `
SELECT id, display_name, source, format,
       COALESCE(quantization,''), COALESCE(repo,''), COALESCE(local_path,''),
       size_bytes, COALESCE(sha256,''), COALESCE(compat_status,''),
       COALESCE(presence,'present'), COALESCE(missing_since,''), COALESCE(updated_at,''),
       COALESCE(launch_state,'available_on_request')
FROM models WHERE id = ?`
	var m ModelSummary
	var size *int64
	err := s.deps.DB.QueryRowContext(ctx, q, modelID).Scan(
		&m.ID, &m.DisplayName, &m.Source, &m.Format,
		&m.Quantization, &m.Repo, &m.LocalPath,
		&size, &m.SHA256, &m.CompatStatus,
		&m.Presence, &m.MissingSince, &m.UpdatedAt,
		&m.LaunchState,
	)
	if err != nil {
		return ModelSummary{}, fmt.Errorf("read model %q: %w", modelID, err)
	}
	m.SizeBytes = size
	m.AutoLoadDisabled = m.LaunchState == launchStateUnavailable
	return m, nil
}

// autoLoadDisabledReason returns a non-empty, human-readable refusal reason when modelID's
// launch_state is `unavailable` (migration 0015 — formerly auto_load_disabled=1, migration 0014),
// and "" when it is allowed to auto-load. Every OTHER launch_state (always_available, available,
// available_on_request) auto-loads — this is the ONLY state that refuses, per the migration 0015
// state table. "" is also returned when the model has no row in the `models` table at all (e.g. a
// config-declared remote llama.cpp model — this gate only ever narrows what the models table already
// advertises, never blocks something it has no opinion on). A DB error is treated as "not refused"
// (fail open on the LOOKUP itself): the admission gates that call this already carry their own
// compat/VRAM refusals, and a query failure here is an environment problem this feature should not
// be blamed for.
//
// The message NAMES the model (id + display name, when known) and says exactly how to load it
// manually, per every other refusal reason in this codebase (CLAUDE.md: every refusal carries a
// human-readable reason) and per the owner's explicit ask that the text be actionable. The wording
// is preserved BYTE-IDENTICAL to the pre-0015 message (including "auto_load_disabled=1") because
// classifyBlockReason (proxy.go) and existing tests match on it verbatim; only the underlying data
// source (launch_state, not the auto_load_disabled column) changed.
func (s *Service) autoLoadDisabledReason(ctx context.Context, modelID string) string {
	if s.deps.DB == nil || modelID == "" {
		return ""
	}
	var state, displayName string
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT COALESCE(launch_state,''), COALESCE(display_name,'') FROM models WHERE id = ?`,
		modelID).Scan(&state, &displayName)
	if err != nil || state != launchStateUnavailable {
		return ""
	}
	name := displayName
	if name == "" {
		name = modelID
	}
	return fmt.Sprintf(
		"model %q (%s) is decommissioned from automatic loading (auto_load_disabled=1) — it will not be "+
			"auto-started by the OpenAI proxy, the residency queue, or the jobs worker. To load it anyway, "+
			"start it explicitly with POST /api/instances {\"model_id\":%q}, or re-enable auto-load with "+
			"PUT /api/models/%s/auto-load {\"enabled\":true}",
		modelID, name, modelID, modelID)
}
