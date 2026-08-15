package compat

import (
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/compat/gguf"
)

// These are the M3.1–M3.5 gate tests: the embedded-MTP launch profile is permitted ONLY when the
// model AND the selected binary both support it, the emitted flags match the validated pinned
// profile exactly, and every refusal carries a human-readable reason (CLAUDE.md).
//
// Binary fixtures model the three REAL binaries on the reference box as probed 2026-07-25:
//   - mtpBuild  = b10106: --spec-type advertises draft-mtp, --spec-draft-n-max present, -ctk/-ctv.
//   - noMTPFork = llama.cpp-tq3 fork: -ctk/-ctv and --spec-type present, but NO draft-mtp token
//     and NO --spec-draft-n-max (verified by grepping its --help).
var (
	mtpBuild = LlamaBinary{
		Path: "b10106", Detail: "CUDA build", Detected: true,
		HasFlashAttn: true, HasCUDA: true, SupportsKVQuant: true,
		HasSpecMTP: true, HasSpecDraftMax: true,
	}
	noMTPFork = LlamaBinary{
		Path: "fork", Detail: "CUDA fork (has_tq3)", Detected: true,
		HasTQ3: true, HasFlashAttn: true, HasCUDA: true, SupportsKVQuant: true,
		HasSpecMTP: false, HasSpecDraftMax: false,
	}
)

// mtpModel is a descriptor whose MTP head is already known (HasMTP set from the manifest), with no
// GGUF source so modelMTP never touches the filesystem.
func mtpModel() ModelDescriptor {
	return ModelDescriptor{ID: "qwen3.6-mtp", DisplayName: "Qwen3.6 MTP", Format: FormatGGUF, HasMTP: true}
}

func plainModel() ModelDescriptor {
	return ModelDescriptor{ID: "plain-gguf", DisplayName: "Plain", Format: FormatGGUF, HasMTP: false}
}

// mtpProfile is an MTP profile with an ASYMMETRIC KV pair: 8K ctx, -ctk q8_0 -ctv q4_0,
// --spec-type draft-mtp, --spec-draft-n-max 2, -np 1.
//
// The asymmetry is the POINT of this fixture, not a claim about what is deployed. The profile
// actually PROVEN at 64K on the reference box is SYMMETRIC q4_0/q4_0
// (scripts/run-qwen-64k-q4q4.ps1, confirmed against the live process). An asymmetric pair is used
// here because it is the case that distinguishes independent -ctk/-ctv from the old single kv_quant
// bool, and because it is the only shape that can catch a canonical-key regression — a symmetric
// pair renders as the bare type and would have hidden the observed_peaks mis-keying that
// TestPlanLlamaLaunch_CalibKeyIsCanonicalPair now guards.
func mtpProfile() LaunchProfile {
	return LaunchProfile{
		Ctx: 8192, KVTypeK: gguf.KVCacheQ8_0, KVTypeV: gguf.KVCacheQ4_0,
		SpecMode: SpeculativeDraftMTP, SpecDraftMax: 2,
	}
}

// TestPlanLlamaLaunch_MTPExactFlags pins the EXACT flag surface of the validated profile (M3.4).
// Each flag is checked as a name/value PAIR, so a value landing on the wrong flag fails.
func TestPlanLlamaLaunch_MTPExactFlags(t *testing.T) {
	avail := EngineAvailability{LlamaBinaries: []LlamaBinary{mtpBuild}}
	plan, err := PlanLlamaLaunch(mtpModel(), avail, mtpProfile())
	if err != nil {
		t.Fatalf("PlanLlamaLaunch: unexpected error: %v", err)
	}
	for _, want := range [][2]string{
		{"-ngl", "all"},  // full-GPU default, never relaxed by MTP
		{"-c", "8192"},   // requested context
		{"--fit", "off"}, // LocalHub sizes the fit itself
		{"-fa", "on"},    // binary advertises flash-attn
		{"-ctk", "q8_0"}, // INDEPENDENT K
		{"-ctv", "q4_0"}, // INDEPENDENT V — the pair no API caller could express before M2.1
		{"-np", "1"},     // pinned-build acceptance condition
		{"--spec-type", "draft-mtp"},
		{"--spec-draft-n-max", "2"},
	} {
		if !hasFlagPair(plan.Flags, want[0], want[1]) {
			t.Errorf("missing flag pair %s %s; got %v", want[0], want[1], plan.Flags)
		}
	}
	// The old capability-driven `-ctv tq3_0` must be GONE (replaced, not added alongside — M3.4).
	if hasFlagPair(plan.Flags, "-ctv", "tq3_0") {
		t.Errorf("-ctv tq3_0 must no longer be emitted off a capability bit; got %v", plan.Flags)
	}
	if plan.KVTypeK != gguf.KVCacheQ8_0 || plan.KVTypeV != gguf.KVCacheQ4_0 {
		t.Errorf("plan KV pair = %s/%s, want q8_0/q4_0", plan.KVTypeK, plan.KVTypeV)
	}
}

// TestPlanLlamaLaunch_MTPForcesSingleSlot: draft-mtp pins -np 1 even when more slots are asked
// for. Slots multiply the KV cache, so silently honouring a larger value would ALSO mis-price the
// admitted footprint, not just change throughput.
func TestPlanLlamaLaunch_MTPForcesSingleSlot(t *testing.T) {
	avail := EngineAvailability{LlamaBinaries: []LlamaBinary{mtpBuild}}
	p := mtpProfile()
	p.Slots = 8
	plan, err := PlanLlamaLaunch(mtpModel(), avail, p)
	if err != nil {
		t.Fatalf("PlanLlamaLaunch: %v", err)
	}
	if !hasFlagPair(plan.Flags, "-np", "1") {
		t.Errorf("draft-mtp must force -np 1; got %v", plan.Flags)
	}
	if plan.Profile.Slots != 1 {
		t.Errorf("normalized profile slots = %d, want 1", plan.Profile.Slots)
	}
}

// TestPlanLlamaLaunch_MTPRefusals is the M3.3/M3.5 gate: MTP needs BOTH sides to agree, and each
// refusal names WHICH side failed and what to do instead.
func TestPlanLlamaLaunch_MTPRefusals(t *testing.T) {
	tests := []struct {
		name      string
		desc      ModelDescriptor
		bins      []LlamaBinary
		wantParts []string
	}{
		{
			name:      "binary lacks draft-mtp",
			desc:      mtpModel(),
			bins:      []LlamaBinary{noMTPFork},
			wantParts: []string{"has_spec_mtp", "draft-mtp", "CUDA fork"},
		},
		{
			name:      "model has no MTP head",
			desc:      plainModel(),
			bins:      []LlamaBinary{mtpBuild},
			wantParts: []string{"does not", "MTP", "nextn", "ngram-cache"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			avail := EngineAvailability{LlamaBinaries: tc.bins}
			_, err := PlanLlamaLaunch(tc.desc, avail, mtpProfile())
			if err == nil {
				t.Fatal("expected a refusal, got a launch plan (a false MTP capability spawns flags the engine rejects)")
			}
			low := strings.ToLower(err.Error())
			for _, part := range tc.wantParts {
				if !strings.Contains(low, strings.ToLower(part)) {
					t.Errorf("refusal reason missing %q; got %q", part, err.Error())
				}
			}
		})
	}
}

// TestPlanLlamaLaunch_MTPSelectsCapableBinary: with an incapable binary configured FIRST, an MTP
// profile must still select the capable one — capability requirements outrank configured order
// (M3.6), the inverse of the non-MTP case where configured order decides.
func TestPlanLlamaLaunch_MTPSelectsCapableBinary(t *testing.T) {
	avail := EngineAvailability{LlamaBinaries: []LlamaBinary{noMTPFork, mtpBuild}}
	plan, err := PlanLlamaLaunch(mtpModel(), avail, mtpProfile())
	if err != nil {
		t.Fatalf("PlanLlamaLaunch: %v", err)
	}
	if plan.Binary.Path != "b10106" {
		t.Errorf("selected %q, want b10106 (the only has_spec_mtp binary)", plan.Binary.Path)
	}
	// Sanity: the SAME availability without an MTP request honours configured order instead.
	plainPlan, err := PlanLlamaLaunch(plainModel(), avail, LaunchProfile{Ctx: 8192})
	if err != nil {
		t.Fatalf("PlanLlamaLaunch (plain): %v", err)
	}
	if plainPlan.Binary.Path != "fork" {
		t.Errorf("non-MTP start selected %q, want fork (first configured)", plainPlan.Binary.Path)
	}
}

// TestGGUFRunFlags_SpecDraftMaxGatedOnBinary: --spec-draft-n-max is probed separately from
// draft-mtp, so a build advertising the mode but not the flag must not receive the flag.
func TestGGUFRunFlags_SpecDraftMaxGatedOnBinary(t *testing.T) {
	bin := mtpBuild
	bin.HasSpecDraftMax = false
	flags := ggufRunFlags(mtpModel(), bin, mtpProfile())
	if !hasFlagPair(flags, "--spec-type", "draft-mtp") {
		t.Errorf("--spec-type draft-mtp should still be emitted; got %v", flags)
	}
	for i, f := range flags {
		if f == "--spec-draft-n-max" {
			t.Errorf("--spec-draft-n-max must be omitted when the binary does not advertise it (index %d); got %v", i, flags)
		}
	}
}

// TestPlanLlamaLaunch_KVForcedF16OnIncapableBinary: a binary without independent -ctk/-ctv must be
// launched with an f16 cache, and the plan must REPORT f16 rather than emit flags it cannot honour.
func TestPlanLlamaLaunch_KVForcedF16OnIncapableBinary(t *testing.T) {
	bin := mtpBuild
	bin.SupportsKVQuant = false
	avail := EngineAvailability{LlamaBinaries: []LlamaBinary{bin}}
	plan, err := PlanLlamaLaunch(mtpModel(), avail, mtpProfile())
	if err != nil {
		t.Fatalf("PlanLlamaLaunch: %v", err)
	}
	for _, f := range plan.Flags {
		if f == "-ctk" || f == "-ctv" {
			t.Errorf("binary lacks KV-type support; %s must not be emitted: %v", f, plan.Flags)
		}
	}
	if plan.KVTypeK != gguf.KVCacheF16 || plan.KVTypeV != gguf.KVCacheF16 {
		t.Errorf("plan KV pair = %s/%s, want f16/f16 on a binary without -ctk/-ctv", plan.KVTypeK, plan.KVTypeV)
	}
	if plan.KVQuant {
		t.Error("plan.KVQuant must be false when the KV request was downgraded to f16")
	}
}

// TestPlanLlamaLaunch_CalibKeyIsCanonicalPair is a regression test for a bug caught LIVE on
// 2026-07-25: a real `-ctk q8_0 -ctv q4_0` run recorded `kv_type='q8_0'` in observed_peaks, because
// LaunchPlan.KVType was set to kvK alone. That key would later MATCH a q8_0/q8_0 lookup, letting a
// peak measured with a cheap q4_0 value cache justify an expensive q8_0 one — exactly what the
// calibration design forbids ("never let a measurement from one (K,V) pair justify another").
//
// LaunchPlan.KVType is what core writes to observed_peaks, so it must be the CANONICAL pair key.
func TestPlanLlamaLaunch_CalibKeyIsCanonicalPair(t *testing.T) {
	avail := EngineAvailability{LlamaBinaries: []LlamaBinary{mtpBuild}}

	// Asymmetric: must render as the pair, never the bare K type.
	plan, err := PlanLlamaLaunch(mtpModel(), avail, mtpProfile())
	if err != nil {
		t.Fatalf("PlanLlamaLaunch: %v", err)
	}
	if got := string(plan.KVType); got != "q8_0/q4_0" {
		t.Errorf("asymmetric calibration key = %q, want %q — recording the bare K type lets a "+
			"q4_0-value measurement justify a q8_0-value profile", got, "q8_0/q4_0")
	}

	// Symmetric: must stay the bare type so peaks recorded before independent K/V still match.
	sym := mtpProfile()
	sym.KVTypeK, sym.KVTypeV = gguf.KVCacheQ4_0, gguf.KVCacheQ4_0
	plan2, err := PlanLlamaLaunch(mtpModel(), avail, sym)
	if err != nil {
		t.Fatalf("PlanLlamaLaunch (symmetric): %v", err)
	}
	if got := string(plan2.KVType); got != "q4_0" {
		t.Errorf("symmetric calibration key = %q, want %q (older rows must keep matching)", got, "q4_0")
	}
}

// TestValidateKVCacheType covers the M2.1 validation surface, including the deliberate tq3_0
// refusal (the estimator cannot price it, so admitting it would be a speculative admission).
func TestValidateKVCacheType(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    gguf.KVCacheType
		wantErr bool
	}{
		{"", "", false},
		{"f16", gguf.KVCacheF16, false},
		{"q8_0", gguf.KVCacheQ8_0, false},
		{"q4_0", gguf.KVCacheQ4_0, false},
		{"tq3_0", "", true},
		{"q5_1", "", true},
		{"Q8_0", "", true}, // case-sensitive: llama.cpp's accepted flag value is lowercase
	} {
		got, err := ValidateKVCacheType(tc.in, "k")
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateKVCacheType(%q): err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("ValidateKVCacheType(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if err != nil {
			// Every refusal must name the side and the valid set (CLAUDE.md).
			if !strings.Contains(err.Error(), "kv_type_k") || !strings.Contains(err.Error(), "q4_0") {
				t.Errorf("refusal for %q must name the side and valid values; got %q", tc.in, err.Error())
			}
		}
	}
}

func TestValidateSlotsAndSpecDraftMax(t *testing.T) {
	if _, err := ValidateSlots(MaxSlots + 1); err == nil {
		t.Error("slots above MaxSlots must be refused (a high count can turn a fit into an OOM)")
	}
	if _, err := ValidateSlots(0); err != nil {
		t.Errorf("slots=0 means unset and must be accepted; got %v", err)
	}
	if _, err := ValidateSpecDraftMax(MaxSpecDraftMax + 1); err == nil {
		t.Error("spec_draft_max above the cap must be refused")
	}
	if _, err := ValidateSpecDraftMax(2); err != nil {
		t.Errorf("the validated Qwen3.6 draft max (2) must be accepted; got %v", err)
	}
}

// TestValidateSpeculative_DraftMTPAccepted: draft-mtp is now a VALID value (it used to be a 400).
func TestValidateSpeculative_DraftMTPAccepted(t *testing.T) {
	got, err := ValidateSpeculative(SpeculativeDraftMTP)
	if err != nil || got != SpeculativeDraftMTP {
		t.Fatalf("ValidateSpeculative(draft-mtp) = (%q, %v), want (draft-mtp, nil)", got, err)
	}
	if _, err := ValidateSpeculative("draft-eagle3"); err == nil {
		t.Error("an unvalidated spec mode must be refused even though b10106 advertises it")
	}
}
