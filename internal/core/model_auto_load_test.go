package core

import (
	"context"
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/instances"
)

// model_auto_load_test.go covers the DEPRECATED decommission alias (migration 0014, now backed by
// launch_state — migration 0015, see model_launch_state_test.go for the four-state coverage): a
// model with launch_state='unavailable' is (a) excluded from OpenAIModels (GET /v1/models), (b)
// STILL listed by Models (GET /api/models) with the derived AutoLoadDisabled flag exposed, (c)
// refused by the proxy resolve path with a reason, (d) still startable via an EXPLICIT (Manual=true)
// StartInstance call — the core method the POST /api/instances handler is a thin wrapper over
// (PLAN.md decision 7: business logic and its tests live in core, not in the transport) — and (e)
// the SetModelAutoLoad DEPRECATED alias — the core method PUT /api/models/{id}/auto-load is a thin
// wrapper over — round-trips onto launch_state.

// TestOpenAIModels_ExcludesDecommissioned is requirement (a): a decommissioned model must not be
// advertised to OpenAI clients even though it is present and runnable.
func TestOpenAIModels_ExcludesDecommissioned(t *testing.T) {
	d := coreTestDB(t)
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, compat_status, presence, launch_state) VALUES
		('normal-model','Normal','local_path','gguf','runs','present','available_on_request'),
		('decommissioned-model','Decommissioned','local_path','gguf','runs','present','unavailable')`); err != nil {
		t.Fatal(err)
	}
	svc := New(Deps{DB: d})
	models, err := svc.OpenAIModels(context.Background())
	if err != nil {
		t.Fatalf("OpenAIModels: %v", err)
	}
	got := map[string]bool{}
	for _, m := range models {
		got[m.ID] = true
	}
	if !got["normal-model"] {
		t.Error("normal-model should be listed")
	}
	if got["decommissioned-model"] {
		t.Error("decommissioned-model must NOT be listed on GET /v1/models")
	}
}

// TestModels_StillListsDecommissionedWithFlag is requirement (b): the admin inventory
// (GET /api/models) must still show a decommissioned model, with the flag visible in the JSON shape.
func TestModels_StillListsDecommissionedWithFlag(t *testing.T) {
	d := coreTestDB(t)
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, presence, launch_state) VALUES
		('decommissioned-model','Decommissioned','local_path','gguf','present','unavailable')`); err != nil {
		t.Fatal(err)
	}
	svc := New(Deps{DB: d})
	models, err := svc.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	var found *ModelSummary
	for i := range models {
		if models[i].ID == "decommissioned-model" {
			found = &models[i]
		}
	}
	if found == nil {
		t.Fatal("decommissioned-model must still appear in GET /api/models")
	}
	if !found.AutoLoadDisabled {
		t.Error("AutoLoadDisabled should be true on the admin inventory row")
	}
}

// TestResolveForProxy_RefusesDecommissioned is requirement (c): the OpenAI proxy resolve path
// (backing /v1/chat/completions etc.) must refuse a decommissioned model with a reason that names
// the model and says how to load it manually, WITHOUT ever attempting a start.
func TestResolveForProxy_RefusesDecommissioned(t *testing.T) {
	d := coreTestDB(t)
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, compat_status, presence, local_path, launch_state) VALUES
		('decommissioned-model','Big Model','local_path','gguf','runs','present','/tmp/big.gguf','unavailable')`); err != nil {
		t.Fatal(err)
	}
	svc := New(Deps{DB: d})

	handle, block, err := svc.ResolveForProxy(context.Background(), "decommissioned-model", 1)
	if err != nil {
		t.Fatalf("ResolveForProxy should block, not error: %v", err)
	}
	if handle != nil {
		t.Fatal("a decommissioned model must never resolve to a servable handle")
	}
	if block == nil {
		t.Fatal("expected a ProxyBlock for a decommissioned model")
	}
	for _, tok := range []string{"decommissioned-model", "decommissioned", "POST /api/instances"} {
		if !strings.Contains(block.Reason, tok) {
			t.Errorf("refusal reason %q missing expected token %q (must name the model + how to load it manually)", block.Reason, tok)
		}
	}
}

// TestStartInstance_ManualBypassesDecommission is requirement (d): an EXPLICIT operator start
// (Manual=true, what POST /api/instances sets) is NOT refused by the decommission flag, while the
// SAME request left implicit (Manual=false, what every auto path leaves it) IS refused with a reason
// naming the model. A HEALTHY instance is seeded so the explicit call can succeed via the warm-route
// short-circuit without needing a real llama.cpp binary (this test targets the decommission gate,
// not the spawn machinery, which is exercised elsewhere).
func TestStartInstance_ManualBypassesDecommission(t *testing.T) {
	d := coreTestDB(t)
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, presence, launch_state) VALUES
		('decommissioned-model','Big Model','local_path','gguf','present','unavailable')`); err != nil {
		t.Fatal(err)
	}
	store := instances.NewStore(d.DB)
	svc := New(Deps{DB: d, InstanceStore: store, Events: NewEventBus()})

	// Seed a HEALTHY llama.cpp instance for the decommissioned model so the warm-route short-circuit
	// can succeed the explicit call with no real binary/spawn involved.
	created, err := store.Create(context.Background(), instances.CreateParams{
		ID: "warm-decom", ModelID: "decommissioned-model", Engine: instances.EngineLlamaCPP, Reason: "seed",
	})
	if err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	if _, err := store.Transition(context.Background(), created.ID, instances.TransitionParams{To: instances.StateStarting, Reason: "seed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), created.ID, instances.TransitionParams{To: instances.StateHealthy, Reason: "seed"}); err != nil {
		t.Fatal(err)
	}

	// Implicit (Manual=false, the default for every auto path): refused, naming the model.
	_, err = svc.StartInstance(context.Background(), StartInstanceRequest{ModelID: "decommissioned-model"})
	if err == nil {
		t.Fatal("an implicit start of a decommissioned model must be refused")
	}
	if !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(err.Error(), "decommissioned-model") {
		t.Errorf("refusal must be REFUSED and name the model: %v", err)
	}

	// Explicit (Manual=true, what POST /api/instances sets): must succeed.
	view, err := svc.StartInstance(context.Background(), StartInstanceRequest{ModelID: "decommissioned-model", Manual: true})
	if err != nil {
		t.Fatalf("an EXPLICIT start of a decommissioned model must still succeed: %v", err)
	}
	if view.ID != "warm-decom" {
		t.Errorf("expected the explicit start to route to the warm instance, got %q", view.ID)
	}
}

// TestSetModelAutoLoad_RoundTrips is requirement (e): the toggle (PUT /api/models/{id}/auto-load is
// a thin wrapper over SetModelAutoLoad, PLAN.md decision 7 — business logic and its tests live in
// core) flips the flag both ways and is reflected by Models(), and refuses an unknown model id.
func TestSetModelAutoLoad_RoundTrips(t *testing.T) {
	d := coreTestDB(t)
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, presence) VALUES
		('m1','Model One','local_path','gguf','present')`); err != nil {
		t.Fatal(err)
	}
	svc := New(Deps{DB: d, Events: NewEventBus()})

	summary, err := svc.SetModelAutoLoad(context.Background(), "m1", false)
	if err != nil {
		t.Fatalf("decommission: %v", err)
	}
	if !summary.AutoLoadDisabled {
		t.Error("expected AutoLoadDisabled=true after enabled=false")
	}
	// The deprecated alias must map onto launch_state=unavailable, not just flip a legacy bit.
	if summary.LaunchState != "unavailable" {
		t.Errorf("expected LaunchState=unavailable after enabled=false, got %q", summary.LaunchState)
	}
	models, _ := svc.Models(context.Background())
	if len(models) != 1 || !models[0].AutoLoadDisabled {
		t.Fatalf("Models() must reflect the decommission: %+v", models)
	}

	summary, err = svc.SetModelAutoLoad(context.Background(), "m1", true)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if summary.AutoLoadDisabled {
		t.Error("expected AutoLoadDisabled=false after enabled=true")
	}
	// enabled=true must map onto available_on_request — the pre-0015 "normal" state — never onto
	// always_available/available (this alias can never grant eager loading).
	if summary.LaunchState != "available_on_request" {
		t.Errorf("expected LaunchState=available_on_request after enabled=true, got %q", summary.LaunchState)
	}

	if _, err := svc.SetModelAutoLoad(context.Background(), "does-not-exist", false); err == nil {
		t.Fatal("expected an error for an unknown model id")
	} else if !strings.Contains(err.Error(), "not in the inventory") {
		t.Errorf("unexpected error for unknown model: %v", err)
	}
}
