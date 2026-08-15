package core

import (
	"context"
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/hardware"
	"github.com/localhub/localhub/internal/instances"
)

// model_launch_state_test.go covers the FOUR-STATE residency policy (migration 0015): state
// round-trip through SetModelLaunchState (the core method PUT /api/models/{id}/launch-state is a
// thin wrapper over, PLAN.md decision 7), the auto-load refusal firing ONLY for `unavailable`, the
// pure ttl/pin derivation for all four states, and that the derivation is actually wired into a
// start (Pinned + TTLSeconds reach the engine spec). The data-migration coverage (auto_load_disabled
// -> launch_state) lives in internal/db/db_test.go, next to the other stepwise-migration tests.

// TestSetModelLaunchState_RoundTrips exercises every state transition, an invalid value (400-shaped
// error listing the valid states), and an unknown model id (404-shaped error).
func TestSetModelLaunchState_RoundTrips(t *testing.T) {
	d := coreTestDB(t)
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, presence) VALUES
		('m1','Model One','local_path','gguf','present')`); err != nil {
		t.Fatal(err)
	}
	svc := New(Deps{DB: d, Events: NewEventBus()})

	// Default (no explicit set yet) is available_on_request, per the migration's column default.
	models, err := svc.Models(context.Background())
	if err != nil || len(models) != 1 {
		t.Fatalf("Models: %v / %+v", err, models)
	}
	if models[0].LaunchState != "available_on_request" {
		t.Errorf("default LaunchState = %q, want available_on_request", models[0].LaunchState)
	}

	for _, state := range []string{"always_available", "available", "available_on_request", "unavailable"} {
		summary, err := svc.SetModelLaunchState(context.Background(), "m1", state)
		if err != nil {
			t.Fatalf("SetModelLaunchState(%q): %v", state, err)
		}
		if summary.LaunchState != state {
			t.Errorf("SetModelLaunchState(%q): summary.LaunchState = %q", state, summary.LaunchState)
		}
		wantDisabled := state == "unavailable"
		if summary.AutoLoadDisabled != wantDisabled {
			t.Errorf("SetModelLaunchState(%q): AutoLoadDisabled = %v, want %v", state, summary.AutoLoadDisabled, wantDisabled)
		}
		// Re-read via Models() to confirm it round-trips through the catalog, not just the echo.
		models, _ := svc.Models(context.Background())
		if len(models) != 1 || models[0].LaunchState != state {
			t.Fatalf("Models() after SetModelLaunchState(%q) = %+v", state, models)
		}
	}

	// An invalid value is refused with a reason listing the valid states.
	if _, err := svc.SetModelLaunchState(context.Background(), "m1", "eagerly_loaded"); err == nil {
		t.Fatal("expected an error for an invalid launch_state value")
	} else {
		for _, tok := range []string{"always_available", "available", "available_on_request", "unavailable"} {
			if !strings.Contains(err.Error(), tok) {
				t.Errorf("invalid-state error %q should list %q as a valid option", err.Error(), tok)
			}
		}
	}

	// An unknown model id is refused explicitly (not a silent zero-row no-op).
	if _, err := svc.SetModelLaunchState(context.Background(), "does-not-exist", "unavailable"); err == nil {
		t.Fatal("expected an error for an unknown model id")
	} else if !strings.Contains(err.Error(), "not in the inventory") {
		t.Errorf("unexpected error for unknown model: %v", err)
	}
}

// TestAutoLoadDisabledReason_OnlyUnavailableRefuses is the core of the four-state contract: ONLY
// launch_state=unavailable refuses an auto-load. always_available, available, and
// available_on_request must ALL be allowed — the state table's whole point is that they differ in
// eagerness/eviction/TTL, never in whether an incoming request may auto-load them.
func TestAutoLoadDisabledReason_OnlyUnavailableRefuses(t *testing.T) {
	d := coreTestDB(t)
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, presence, launch_state) VALUES
		('always','Always','local_path','gguf','present','always_available'),
		('avail','Available','local_path','gguf','present','available'),
		('on-request','OnRequest','local_path','gguf','present','available_on_request'),
		('unavail','Unavailable','local_path','gguf','present','unavailable')`); err != nil {
		t.Fatal(err)
	}
	svc := New(Deps{DB: d})

	for _, allowed := range []string{"always", "avail", "on-request"} {
		if reason := svc.autoLoadDisabledReason(context.Background(), allowed); reason != "" {
			t.Errorf("model %q must be allowed to auto-load, got refusal: %q", allowed, reason)
		}
	}
	reason := svc.autoLoadDisabledReason(context.Background(), "unavail")
	if reason == "" {
		t.Fatal("model with launch_state=unavailable must be refused")
	}
	for _, tok := range []string{"unavail", "POST /api/instances"} {
		if !strings.Contains(reason, tok) {
			t.Errorf("refusal reason %q missing expected token %q", reason, tok)
		}
	}
}

// TestResidencyPolicyForLaunchState is a PURE table-driven test of the ttl/pin derivation (no DB):
// always_available and available are exempt from idle-TTL unload (ttlSeconds<=0, the SAME "never
// expires" mechanism eviction.go's ttlExpired already implements); only always_available forces a
// pin; available_on_request (and unavailable, and any unrecognized value) get the configured TTL and
// are never force-pinned.
func TestResidencyPolicyForLaunchState(t *testing.T) {
	const cfgTTL = 900
	cases := []struct {
		state        string
		wantTTL      int
		wantForcePin bool
	}{
		{"always_available", 0, true},
		{"available", 0, false},
		{"available_on_request", cfgTTL, false},
		{"unavailable", cfgTTL, false},
		{"", cfgTTL, false}, // unrecognized/empty falls back to the safe default
	}
	for _, tc := range cases {
		ttl, forcePin := residencyPolicyForLaunchState(tc.state, cfgTTL)
		if ttl != tc.wantTTL || forcePin != tc.wantForcePin {
			t.Errorf("residencyPolicyForLaunchState(%q, %d) = (%d, %v), want (%d, %v)",
				tc.state, cfgTTL, ttl, forcePin, tc.wantTTL, tc.wantForcePin)
		}
	}
}

// TestStartVLLMInstance_AlwaysAvailable_ForcesPinAndExemptsTTL wires the derivation into a REAL
// start: attach mode is used (like TestStartVLLMInstance_AttachMode) specifically because it reaches
// startVLLMInstance's launch_state lookup + spec assembly WITHOUT needing llama.cpp binary detection
// (attach mode never calls PlanLlamaLaunch), so this is the cheapest path that proves Pinned/
// TTLSeconds actually reach the engine start spec rather than staying a theoretical mapping.
func TestStartVLLMInstance_AlwaysAvailable_ForcesPinAndExemptsTTL(t *testing.T) {
	d := coreTestDB(t)
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, repo, launch_state) VALUES
		('vm','vLLM Model','huggingface','safetensors','Qwen/Qwen3-0.6B','always_available')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO engines (name, installed) VALUES ('vllm',1)`); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Engines.VLLM.Mode = "attach"
	cfg.Engines.VLLM.AttachURL = "http://127.0.0.1:8000"

	fr := &fakeVLLMRunner{}
	svc := New(Deps{
		DB: d, Hardware: &hardware.FakeProbe{Snap: hardware.DefaultFakeSnapshot()}, Engines: EnginesRegistry{},
		Compat:        &fakeCompat{verdict: compat.Verdict{Status: compat.StatusRuns, Reason: "ok"}},
		InstanceStore: instances.NewStore(d.DB), VLLMRunner: fr, Events: NewEventBus(), Cfg: cfg,
	})

	// req.Pin is deliberately left false: the pin must come from launch_state, not the caller.
	if _, err := svc.StartInstance(context.Background(), StartInstanceRequest{ModelID: "vm", Engine: "vllm"}); err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	if !fr.startCalled {
		t.Fatal("expected vLLM runner Start called")
	}
	if !fr.startSpec.Pinned {
		t.Error("always_available must force Pinned=true even when the caller did not request a pin")
	}
	if fr.startSpec.TTLSeconds != 0 {
		t.Errorf("always_available must be exempt from idle-TTL unload, got TTLSeconds=%d", fr.startSpec.TTLSeconds)
	}
}

// TestStartVLLMInstance_AvailableOnRequest_NormalTTLNoForcedPin is the available_on_request
// counterpart: the configured default TTL applies (idle-TTL eligible) and Pinned is NOT forced.
func TestStartVLLMInstance_AvailableOnRequest_NormalTTLNoForcedPin(t *testing.T) {
	d := coreTestDB(t)
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, repo, launch_state) VALUES
		('vm','vLLM Model','huggingface','safetensors','Qwen/Qwen3-0.6B','available_on_request')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO engines (name, installed) VALUES ('vllm',1)`); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Engines.VLLM.Mode = "attach"
	cfg.Engines.VLLM.AttachURL = "http://127.0.0.1:8000"
	cfg.Residency.DefaultTTLSeconds = 900

	fr := &fakeVLLMRunner{}
	svc := New(Deps{
		DB: d, Hardware: &hardware.FakeProbe{Snap: hardware.DefaultFakeSnapshot()}, Engines: EnginesRegistry{},
		Compat:        &fakeCompat{verdict: compat.Verdict{Status: compat.StatusRuns, Reason: "ok"}},
		InstanceStore: instances.NewStore(d.DB), VLLMRunner: fr, Events: NewEventBus(), Cfg: cfg,
	})

	if _, err := svc.StartInstance(context.Background(), StartInstanceRequest{ModelID: "vm", Engine: "vllm"}); err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	if fr.startSpec.Pinned {
		t.Error("available_on_request must NOT force a pin")
	}
	if fr.startSpec.TTLSeconds != 900 {
		t.Errorf("available_on_request must use the configured idle-TTL, got TTLSeconds=%d", fr.startSpec.TTLSeconds)
	}
}
