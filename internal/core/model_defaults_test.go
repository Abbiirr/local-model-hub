package core

import (
	"context"
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/compat"
)

// M2.3 gate tests for the saved per-model launch default (migration 0011).
//
// The contract pinned here is as much about what this feature does NOT do as what it does: a saved
// default is ADVISORY. It must round-trip exactly, refuse a profile that could never be started, and
// flag itself stale when the engine build it was validated on is gone (M6.2) — but it must never
// silently change what a start does (M6.1).

// seedModel inserts an inventory row so the launch-default FK and existence check are satisfied.
func seedModel(t *testing.T, svc *Service, id string) {
	t.Helper()
	_, err := svc.deps.DB.ExecContext(context.Background(),
		`INSERT INTO models (id, display_name, source, format) VALUES (?, ?, 'local_path', 'gguf')`, id, id)
	if err != nil {
		t.Fatalf("seed model %s: %v", id, err)
	}
}

// validatedQwenDefault is the profile this whole M-series exists to make expressible.
func validatedQwenDefault(id string) ModelLaunchDefault {
	return ModelLaunchDefault{
		ModelID:       id,
		Ctx:           8192,
		KVTypeK:       "q8_0",
		KVTypeV:       "q4_0",
		Slots:         1,
		SpecMode:      compat.SpeculativeDraftMTP,
		SpecDraftMax:  2,
		EngineVersion: "10106",
		EngineCommit:  "1425386fd",
		BinaryPath:    `K:\projects\ai\local-model-hub\tools\llama.cpp\llama-server.exe`,
		ValidatedNote: "validated at 8K, non-MTP lifecycle LIVE PASS",
	}
}

func TestModelLaunchDefault_RoundTrip(t *testing.T) {
	d := coreTestDB(t)
	svc := New(Deps{DB: d})
	ctx := context.Background()
	const id = "qwen36-mtp"
	seedModel(t, svc, id)

	want := validatedQwenDefault(id)
	saved, err := svc.SaveModelLaunchDefault(ctx, want)
	if err != nil {
		t.Fatalf("SaveModelLaunchDefault: %v", err)
	}
	if saved.KVTypeK != "q8_0" || saved.KVTypeV != "q4_0" {
		t.Errorf("saved KV pair = %s/%s, want q8_0/q4_0", saved.KVTypeK, saved.KVTypeV)
	}
	if saved.SpecMode != compat.SpeculativeDraftMTP || saved.SpecDraftMax != 2 || saved.Slots != 1 {
		t.Errorf("saved spec profile = %s/%d slots=%d, want draft-mtp/2 slots=1", saved.SpecMode, saved.SpecDraftMax, saved.Slots)
	}
	if saved.CreatedAt.IsZero() || saved.UpdatedAt.IsZero() {
		t.Error("created_at/updated_at must be stamped")
	}

	got, ok, err := svc.ModelLaunchDefault(ctx, id)
	if err != nil || !ok {
		t.Fatalf("ModelLaunchDefault: ok=%v err=%v", ok, err)
	}
	// The projection onto a start request must reproduce the profile the verdict/launch path uses.
	p := startProfile(got.AsStartRequest(), got.Ctx).Normalize()
	wantP := startProfile(want.AsStartRequest(), want.Ctx).Normalize()
	if p != wantP {
		t.Errorf("profile changed through save/load:\n want %+v\n  got %+v", wantP, p)
	}
}

// TestModelLaunchDefault_UpsertReplaces: one authoritative default per model, newest write wins.
func TestModelLaunchDefault_UpsertReplaces(t *testing.T) {
	d := coreTestDB(t)
	svc := New(Deps{DB: d})
	ctx := context.Background()
	const id = "m1"
	seedModel(t, svc, id)

	if _, err := svc.SaveModelLaunchDefault(ctx, ModelLaunchDefault{ModelID: id, Ctx: 8192, KVTypeK: "q4_0", KVTypeV: "q4_0"}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, err := svc.SaveModelLaunchDefault(ctx, ModelLaunchDefault{ModelID: id, Ctx: 16384, KVTypeK: "q8_0", KVTypeV: "q4_0"}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	got, ok, err := svc.ModelLaunchDefault(ctx, id)
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if got.Ctx != 16384 || got.KVTypeK != "q8_0" {
		t.Errorf("newest write must win; got ctx=%d kv_k=%s", got.Ctx, got.KVTypeK)
	}
}

// TestModelLaunchDefault_RejectsUnstartableProfile: a default is validated through the SAME path a
// live start uses. Saving a profile that would be refused at start time is worse than saving none.
func TestModelLaunchDefault_RejectsUnstartableProfile(t *testing.T) {
	d := coreTestDB(t)
	svc := New(Deps{DB: d})
	ctx := context.Background()
	const id = "m1"
	seedModel(t, svc, id)

	cases := []struct {
		name string
		def  ModelLaunchDefault
	}{
		{"tq3_0 KV the estimator cannot price", ModelLaunchDefault{ModelID: id, KVTypeK: "tq3_0"}},
		{"slots above the cap", ModelLaunchDefault{ModelID: id, Slots: compat.MaxSlots + 1}},
		{"draft max without a spec mode", ModelLaunchDefault{ModelID: id, SpecDraftMax: 2}},
		{"unvalidated spec mode", ModelLaunchDefault{ModelID: id, SpecMode: "draft-eagle3"}},
		{"negative ctx", ModelLaunchDefault{ModelID: id, Ctx: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.SaveModelLaunchDefault(ctx, tc.def); err == nil {
				t.Fatalf("%s must be refused", tc.name)
			}
		})
	}
	// Nothing was persisted by any of the refusals.
	if _, ok, err := svc.ModelLaunchDefault(ctx, id); err != nil || ok {
		t.Errorf("a refused save must persist nothing; ok=%v err=%v", ok, err)
	}
}

func TestModelLaunchDefault_RejectsUnknownModel(t *testing.T) {
	d := coreTestDB(t)
	svc := New(Deps{DB: d})
	_, err := svc.SaveModelLaunchDefault(context.Background(), ModelLaunchDefault{ModelID: "ghost", Ctx: 8192})
	if err == nil {
		t.Fatal("saving a default for a model that is not in the inventory must be refused")
	}
	if !strings.Contains(err.Error(), "not in the inventory") {
		t.Errorf("reason must say the model is unknown; got %q", err.Error())
	}
}

// TestModelLaunchDefault_StaleWhenEngineBuildGone is the M6.2 guard: a profile validated on one
// llama.cpp build is not evidence about another. The older fork DIED on this model's hybrid SSM
// architecture, so a saved profile silently outliving its build would re-create that failure.
func TestModelLaunchDefault_StaleWhenEngineBuildGone(t *testing.T) {
	d := coreTestDB(t)
	// No engine registry wired => no llama binaries detected at all.
	svc := New(Deps{DB: d})
	ctx := context.Background()
	const id = "m1"
	seedModel(t, svc, id)

	if _, err := svc.SaveModelLaunchDefault(ctx, validatedQwenDefault(id)); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, err := svc.ModelLaunchDefault(ctx, id)
	if err != nil || !ok {
		t.Fatalf("read: ok=%v err=%v", ok, err)
	}
	if got.StaleReason == "" {
		t.Fatal("a default whose validated binary is not currently detected must carry a stale_reason")
	}
	if !strings.Contains(strings.ToLower(got.StaleReason), "detect") {
		t.Errorf("stale_reason must explain what could not be confirmed; got %q", got.StaleReason)
	}
}

// TestModelLaunchDefault_NoProvenanceIsNotStale: an unrecorded build is UNKNOWN, not stale. Flagging
// it anyway would train the operator to ignore the warning.
func TestModelLaunchDefault_NoProvenanceIsNotStale(t *testing.T) {
	d := coreTestDB(t)
	svc := New(Deps{DB: d})
	ctx := context.Background()
	const id = "m1"
	seedModel(t, svc, id)

	if _, err := svc.SaveModelLaunchDefault(ctx, ModelLaunchDefault{ModelID: id, Ctx: 8192, KVTypeK: "q4_0", KVTypeV: "q4_0"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, _, err := svc.ModelLaunchDefault(ctx, id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.StaleReason != "" {
		t.Errorf("a default with no recorded provenance must not be reported stale; got %q", got.StaleReason)
	}
}

// TestModelLaunchDefault_DeleteIsIdempotent: a UI "clear" button must not need to check first.
func TestModelLaunchDefault_DeleteIsIdempotent(t *testing.T) {
	d := coreTestDB(t)
	svc := New(Deps{DB: d})
	ctx := context.Background()
	const id = "m1"
	seedModel(t, svc, id)

	if err := svc.DeleteModelLaunchDefault(ctx, id); err != nil {
		t.Fatalf("deleting an absent default must succeed: %v", err)
	}
	if _, err := svc.SaveModelLaunchDefault(ctx, ModelLaunchDefault{ModelID: id, Ctx: 8192}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := svc.DeleteModelLaunchDefault(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, err := svc.ModelLaunchDefault(ctx, id); err != nil || ok {
		t.Errorf("default must be gone after delete; ok=%v err=%v", ok, err)
	}
}

// TestModelLaunchDefault_MissingIsNotAnError: "no saved default" is the normal state and must be
// reported as ok=false, not as an error the UI has to special-case.
func TestModelLaunchDefault_MissingIsNotAnError(t *testing.T) {
	d := coreTestDB(t)
	svc := New(Deps{DB: d})
	got, ok, err := svc.ModelLaunchDefault(context.Background(), "never-saved")
	if err != nil {
		t.Fatalf("a missing default must not be an error; got %v", err)
	}
	if ok {
		t.Error("ok must be false when nothing is saved")
	}
	if got.ModelID != "" {
		t.Errorf("a miss must return a zero value; got %+v", got)
	}
}
