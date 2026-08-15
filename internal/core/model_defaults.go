package core

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ModelLaunchDefault is a SAVED, PROVEN launch profile for one model (migration 0011, M2.3/M6.2).
//
// It is a RECOMMENDATION, not a policy: StartInstance never reads it. The UI reads it to pre-fill the
// advanced start form so an operator confirms validated values instead of re-typing them, and
// GET /api/models/{id}/launch-default exposes it for scripts. Auto-applying it would change what an
// UNSPECIFIED request field means — a silent behaviour change of exactly the kind that hid the
// queue-replay bug (KNOWN-LIMITATIONS #21) — so that would need its own ADR and gate (M6.1).
type ModelLaunchDefault struct {
	ModelID string `json:"model_id"`

	// The profile. Same vocabulary as StartInstanceRequest, so the UI maps it field-for-field.
	Ctx             int    `json:"ctx,omitempty"`
	KVTypeK         string `json:"kv_type_k,omitempty"`
	KVTypeV         string `json:"kv_type_v,omitempty"`
	Slots           int    `json:"slots,omitempty"`
	SpecMode        string `json:"spec_mode,omitempty"`
	SpecDraftMax    int    `json:"spec_draft_max,omitempty"`
	AllowCPUOffload bool   `json:"allow_cpu_offload,omitempty"`

	// Provenance (M6.2): WHAT this was validated against. A default is evidence about one engine
	// build and one artifact, nothing more.
	EngineVersion  string `json:"engine_version,omitempty"`
	EngineCommit   string `json:"engine_commit,omitempty"`
	BinaryPath     string `json:"binary_path,omitempty"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
	ValidatedNote  string `json:"validated_note,omitempty"`

	// StaleReason is computed on READ (never stored) when the recorded provenance no longer matches
	// what is currently detected. Non-empty means "this profile was proven somewhere else;
	// re-validate before trusting it".
	StaleReason string `json:"stale_reason,omitempty"`

	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// AsStartRequest projects the saved default onto a start request for the named model. It is what the
// UI's "use saved profile" action sends, and it keeps the mapping in ONE place instead of duplicated
// in TypeScript.
//
// KVQuant is deliberately left false: the saved profile describes the cache with an explicit (K,V)
// pair, and startProfile gives that pair precedence anyway.
func (d ModelLaunchDefault) AsStartRequest() StartInstanceRequest {
	return StartInstanceRequest{
		ModelID:         d.ModelID,
		Ctx:             d.Ctx,
		KVTypeK:         d.KVTypeK,
		KVTypeV:         d.KVTypeV,
		Slots:           d.Slots,
		Speculative:     d.SpecMode,
		SpecDraftMax:    d.SpecDraftMax,
		AllowCPUOffload: d.AllowCPUOffload,
	}
}

// ModelLaunchDefault reads the saved default for a model. ok=false when none is saved — the normal
// state, since a default only exists once someone has proven a profile.
//
// On a hit it also computes StaleReason by comparing the recorded engine build against what is
// detected NOW. A profile validated on b10106 says nothing about a different build; surfacing that is
// the difference between a useful recommendation and a stale claim (M6.2).
func (s *Service) ModelLaunchDefault(ctx context.Context, modelID string) (ModelLaunchDefault, bool, error) {
	if s.deps.DB == nil {
		return ModelLaunchDefault{}, false, fmt.Errorf("database not configured")
	}
	const q = `SELECT model_id, COALESCE(ctx,0), COALESCE(kv_type_k,''), COALESCE(kv_type_v,''),
	       COALESCE(slots,0), COALESCE(spec_mode,''), COALESCE(spec_draft_max,0),
	       COALESCE(allow_cpu_offload,0), COALESCE(engine_version,''), COALESCE(engine_commit,''),
	       COALESCE(binary_path,''), COALESCE(artifact_sha256,''), COALESCE(validated_note,''),
	       COALESCE(created_at,''), COALESCE(updated_at,'')
	FROM model_launch_defaults WHERE model_id = ?`
	var (
		d          ModelLaunchDefault
		offloadInt int
		created    string
		updated    string
	)
	err := s.deps.DB.QueryRowContext(ctx, q, modelID).Scan(
		&d.ModelID, &d.Ctx, &d.KVTypeK, &d.KVTypeV, &d.Slots, &d.SpecMode, &d.SpecDraftMax,
		&offloadInt, &d.EngineVersion, &d.EngineCommit, &d.BinaryPath, &d.ArtifactSHA256,
		&d.ValidatedNote, &created, &updated)
	if err == sql.ErrNoRows {
		return ModelLaunchDefault{}, false, nil
	}
	if err != nil {
		return ModelLaunchDefault{}, false, fmt.Errorf("read launch default for %q: %w", modelID, err)
	}
	d.AllowCPUOffload = offloadInt != 0
	d.CreatedAt = parseCoreTime(created)
	d.UpdatedAt = parseCoreTime(updated)
	d.StaleReason = s.launchDefaultStaleReason(ctx, d)
	return d, true, nil
}

// SaveModelLaunchDefault validates and UPSERTs a model's default profile.
//
// It validates through the SAME path a live start uses (validateStartProfile), so a profile that
// could never be started cannot be saved as a "default" — a stored default that fails at start time
// would be worse than no default at all. It also refuses an unknown model_id explicitly rather than
// letting the foreign key produce an opaque constraint error.
func (s *Service) SaveModelLaunchDefault(ctx context.Context, d ModelLaunchDefault) (ModelLaunchDefault, error) {
	if s.deps.DB == nil {
		return ModelLaunchDefault{}, fmt.Errorf("database not configured")
	}
	if d.ModelID == "" {
		return ModelLaunchDefault{}, fmt.Errorf("save launch default: model_id is required")
	}
	// Validate the profile exactly as a start would (CLAUDE.md: every refusal carries a reason).
	if _, err := validateStartProfile(d.AsStartRequest()); err != nil {
		return ModelLaunchDefault{}, fmt.Errorf("save launch default for %q: %w", d.ModelID, err)
	}
	if d.Ctx < 0 {
		return ModelLaunchDefault{}, fmt.Errorf("save launch default for %q: ctx must be >= 0 (0 => the daemon default), got %d", d.ModelID, d.Ctx)
	}
	var exists int
	if err := s.deps.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM models WHERE id = ?`, d.ModelID).Scan(&exists); err != nil {
		return ModelLaunchDefault{}, fmt.Errorf("check model %q: %w", d.ModelID, err)
	}
	if exists == 0 {
		return ModelLaunchDefault{}, fmt.Errorf("save launch default: model %q is not in the inventory", d.ModelID)
	}

	const q = `INSERT INTO model_launch_defaults
	    (model_id, ctx, kv_type_k, kv_type_v, slots, spec_mode, spec_draft_max, allow_cpu_offload,
	     engine_version, engine_commit, binary_path, artifact_sha256, validated_note, created_at, updated_at)
	    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
	ON CONFLICT(model_id) DO UPDATE SET
	    ctx = excluded.ctx, kv_type_k = excluded.kv_type_k, kv_type_v = excluded.kv_type_v,
	    slots = excluded.slots, spec_mode = excluded.spec_mode, spec_draft_max = excluded.spec_draft_max,
	    allow_cpu_offload = excluded.allow_cpu_offload, engine_version = excluded.engine_version,
	    engine_commit = excluded.engine_commit, binary_path = excluded.binary_path,
	    artifact_sha256 = excluded.artifact_sha256, validated_note = excluded.validated_note,
	    updated_at = datetime('now')`
	_, err := s.deps.DB.ExecContext(ctx, q,
		d.ModelID, nullIfZeroInt(d.Ctx), nullIfEmptyStr(d.KVTypeK), nullIfEmptyStr(d.KVTypeV),
		nullIfZeroInt(d.Slots), nullIfEmptyStr(d.SpecMode), nullIfZeroInt(d.SpecDraftMax),
		boolToIntCore(d.AllowCPUOffload), nullIfEmptyStr(d.EngineVersion), nullIfEmptyStr(d.EngineCommit),
		nullIfEmptyStr(d.BinaryPath), nullIfEmptyStr(d.ArtifactSHA256), nullIfEmptyStr(d.ValidatedNote))
	if err != nil {
		return ModelLaunchDefault{}, fmt.Errorf("save launch default for %q: %w", d.ModelID, err)
	}
	saved, _, err := s.ModelLaunchDefault(ctx, d.ModelID)
	return saved, err
}

// DeleteModelLaunchDefault removes a model's saved default. Deleting an absent row is NOT an error
// (idempotent), so a UI "clear" button need not know whether one existed.
func (s *Service) DeleteModelLaunchDefault(ctx context.Context, modelID string) error {
	if s.deps.DB == nil {
		return fmt.Errorf("database not configured")
	}
	if _, err := s.deps.DB.ExecContext(ctx, `DELETE FROM model_launch_defaults WHERE model_id = ?`, modelID); err != nil {
		return fmt.Errorf("delete launch default for %q: %w", modelID, err)
	}
	return nil
}

// launchDefaultStaleReason compares a saved default's recorded provenance against the CURRENTLY
// detected llama.cpp builds. It returns "" when the recorded binary is still present, or when nothing
// was recorded to compare against (an unrecorded build is unknown, not stale).
//
// This is the M6.2 guard in practice: the entire reason capability-first selection exists is that a
// profile which runs on one build can hard-fail on another — the older fork died on this model's
// hybrid SSM architecture with `missing tensor 'blk.64.ssm_conv1d.weight'`. A saved profile silently
// outliving its engine build would re-create that class of failure.
func (s *Service) launchDefaultStaleReason(ctx context.Context, d ModelLaunchDefault) string {
	if d.EngineVersion == "" && d.BinaryPath == "" {
		return "" // nothing recorded to compare against
	}
	avail := s.engineAvailability(ctx)
	var detected []string
	for _, b := range avail.LlamaBinaries {
		if !b.Detected {
			continue
		}
		detected = append(detected, b.Path)
		if d.BinaryPath != "" && b.Path == d.BinaryPath {
			return ""
		}
	}
	if len(detected) == 0 {
		return "no llama-server binary is currently detected, so this saved profile cannot be checked against the build it was validated on"
	}
	// The recorded binary is not among the detected ones. Report it WITH what IS available, so the
	// operator can see what changed rather than just being told "stale".
	return fmt.Sprintf("this profile was validated on llama.cpp build %s (%s) at %s, which is not among the currently detected binaries (%s) — re-validate before relying on it",
		orUnknown(d.EngineVersion), orUnknown(d.EngineCommit), orUnknown(d.BinaryPath), joinCommaList(detected))
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func joinCommaList(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// parseCoreTime parses SQLite's datetime('now') text form, falling back to RFC3339.
func parseCoreTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, l := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func nullIfEmptyStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfZeroInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func boolToIntCore(b bool) int {
	if b {
		return 1
	}
	return 0
}
