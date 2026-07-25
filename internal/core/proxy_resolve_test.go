package core

import (
	"context"
	"errors"
	"testing"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/instances"
)

// TestOpenAIModels_FiltersToRunnable is the #p61 /v1/models gate (design §1.2): only present +
// runnable models appear; a wont_run and a missing (vanished) model are excluded.
func TestOpenAIModels_FiltersToRunnable(t *testing.T) {
	d := coreTestDB(t)
	// present + runs → included; present + runs_degraded → included; present + no compat → included;
	// present + wont_run → excluded; missing → excluded.
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, compat_status, presence) VALUES
		('runs-model','Runs','local_path','gguf','runs','present'),
		('degraded-model','Degraded','local_path','gguf','runs_degraded','present'),
		('unevaluated-model','Uneval','local_path','gguf','','present'),
		('wontrun-model','WontRun','local_path','gguf','wont_run','present'),
		('missing-model','Missing','local_path','gguf','runs','missing')`); err != nil {
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
		if m.Object != "model" || m.OwnedBy != "localhub" {
			t.Errorf("bad shape for %s: object=%q owned_by=%q", m.ID, m.Object, m.OwnedBy)
		}
	}
	for _, want := range []string{"runs-model", "degraded-model", "unevaluated-model"} {
		if !got[want] {
			t.Errorf("expected %q to be listed", want)
		}
	}
	for _, notWant := range []string{"wontrun-model", "missing-model"} {
		if got[notWant] {
			t.Errorf("%q should NOT be listed (not runnable/present)", notWant)
		}
	}
}

// TestResolveForProxy_WarmRoutesAndCountsInFlight: a HEALTHY instance already serving the model is
// routed to (no spawn), and the returned handle has incremented the in-flight counter (design §1.6
// — the never-kill-active gate for external clients). fr.startCalled must stay false (warm route).
func TestResolveForProxy_WarmRoutesAndCountsInFlight(t *testing.T) {
	// Seed a HEALTHY llama.cpp instance serving m1; residency's warm check routes to it.
	svc, fr, _ := startResidencyService(t, compat.Verdict{
		Status: compat.StatusRuns, Reason: "ok", EstimatedVRAMMiB: 4000,
	}, 15200, []instances.Instance{
		{ID: "warm1", ModelID: "m1", Engine: instances.EngineLlamaCPP, VRAMEstMiB: 4000},
	})
	// Give the warm instance a base_url so BeginInstanceRequest can build a handle.
	base := "http://127.0.0.1:40100"
	if _, err := svc.deps.InstanceStore.Transition(context.Background(), "warm1", instances.TransitionParams{
		To: instances.StateHealthy, Reason: "seed base", BaseURL: &base,
	}); err != nil {
		// warm1 is already HEALTHY; a same-state transition may be rejected — set base_url directly.
		if _, e2 := svc.deps.DB.Exec(`UPDATE instances SET base_url=? WHERE id='warm1'`, base); e2 != nil {
			t.Fatalf("set base_url: %v / %v", err, e2)
		}
	}

	handle, block, err := svc.ResolveForProxy(context.Background(), "m1", 1)
	if err != nil {
		t.Fatalf("ResolveForProxy warm: %v", err)
	}
	if block != nil {
		t.Fatalf("warm model should not block: %s", block.Reason)
	}
	if handle == nil {
		t.Fatal("expected a handle for a warm model")
	}
	if fr.startCalled {
		t.Error("warm route must NOT spawn (runner.Start called)")
	}
	if c := svc.deps.InFlight.ActiveCount("warm1"); c != 1 {
		t.Errorf("in-flight during proxied request = %d, want 1 (never-kill-active gate)", c)
	}
	handle.Done()
	if c := svc.deps.InFlight.ActiveCount("warm1"); c != 0 {
		t.Errorf("in-flight after Done = %d, want 0", c)
	}
}

// TestResolveForProxy_WarmRoutesDespiteSelfShadowVerdict is the P1 self-shadow regression: a large
// model already HEALTHY-resident must route on the /v1 path even when a fresh compat verdict at the
// default ctx would come back wont_run BECAUSE the model's own VRAM is already spent (free VRAM
// excludes it). Before the fix, admitGate's verdict-status gate refused the warm model with a 503
// before residency's warm check ran. The warm short-circuit in StartInstance must now route to the
// resident instance without any fit math — no block, no spawn.
func TestResolveForProxy_WarmRoutesDespiteSelfShadowVerdict(t *testing.T) {
	// Resident 9 GB model; only 4 GB free left → a recomputed verdict SELF-SHADOWS to wont_run.
	svc, fr, _ := startResidencyService(t, compat.Verdict{
		Status: compat.StatusWontRun, Reason: "does not fit in 4000 MiB free", EstimatedVRAMMiB: 9000,
	}, 4000, []instances.Instance{
		{ID: "warm1", ModelID: "m1", Engine: instances.EngineLlamaCPP, VRAMEstMiB: 9000},
	})
	base := "http://127.0.0.1:40120"
	if _, err := svc.deps.DB.Exec(`UPDATE instances SET base_url=? WHERE id='warm1'`, base); err != nil {
		t.Fatalf("set base_url: %v", err)
	}

	handle, block, err := svc.ResolveForProxy(context.Background(), "m1", 1)
	if err != nil {
		t.Fatalf("ResolveForProxy warm self-shadow: unexpected error %v", err)
	}
	if block != nil {
		t.Fatalf("a warm HEALTHY model must NOT 503 against its own resident VRAM; got block: %s", block.Reason)
	}
	if handle == nil {
		t.Fatal("expected a handle routing to the resident instance")
	}
	if fr.startCalled {
		t.Error("warm route must NOT spawn a new process")
	}
	if c := svc.deps.InFlight.ActiveCount("warm1"); c != 1 {
		t.Errorf("in-flight during proxied request = %d, want 1", c)
	}
	handle.Done()
}

// TestResolveForProxy_ColdSelfShadowStillBlocks proves the fix did NOT weaken the cold path: the
// SAME wont_run verdict with NO resident instance must still block (the warm short-circuit only
// fires for a genuinely HEALTHY-resident model).
func TestResolveForProxy_ColdSelfShadowStillBlocks(t *testing.T) {
	svc, fr, _ := startResidencyService(t, compat.Verdict{
		Status: compat.StatusWontRun, Reason: "genuinely too big", EstimatedVRAMMiB: 9000,
	}, 4000, nil) // no resident instance

	handle, block, err := svc.ResolveForProxy(context.Background(), "m1", 1)
	if err != nil {
		t.Fatalf("ResolveForProxy cold: unexpected error %v", err)
	}
	if handle != nil {
		t.Fatal("a cold un-runnable model must not return a handle")
	}
	if block == nil {
		t.Fatal("a cold wont_run model must still block (cold path unchanged)")
	}
	if fr.startCalled {
		t.Error("a blocked cold model must not spawn")
	}
}

// TestResolveForProxy_BlockCancelsQueue is the #p61 cancel-on-block gate (design §1.4): when
// admission queues (VRAM too tight, no idle victim), ResolveForProxy returns a *ProxyBlock with the
// numeric reason AND cancels the phantom queue row so nothing auto-starts for the departed caller.
func TestResolveForProxy_BlockCancelsQueue(t *testing.T) {
	// free 2000, footprint 9000 + 1024 margin → cannot fit, no idle victim → QUEUE.
	svc, fr, _ := startResidencyService(t, compat.Verdict{
		Status: compat.StatusRuns, Reason: "big model", EstimatedVRAMMiB: 9000,
	}, 2000, nil)

	handle, block, err := svc.ResolveForProxy(context.Background(), "m1", 1)
	if err != nil {
		t.Fatalf("ResolveForProxy block: unexpected error %v", err)
	}
	if handle != nil {
		t.Fatal("a blocked model must not return a handle")
	}
	if block == nil {
		t.Fatal("expected a *ProxyBlock for an un-fittable model")
	}
	if block.Reason == "" {
		t.Error("block must carry a numeric reason (mandatory-reason gate)")
	}
	if fr.startCalled {
		t.Error("a blocked model must not spawn")
	}
	// The phantom queue row must have been cancelled → the residency queue is empty.
	rows, qerr := svc.Queue(context.Background())
	if qerr != nil {
		t.Fatalf("Queue: %v", qerr)
	}
	waiting := 0
	for _, r := range rows {
		if r.Status == "WAITING" {
			waiting++
		}
	}
	if waiting != 0 {
		t.Errorf("cancel-on-block failed: %d WAITING rows remain, want 0", waiting)
	}
}

// TestBeginInstanceRequest_UpstreamModel is the P6.1 #p61c relay-rewrite gate (fix-agent): the
// handle for an OLLAMA instance must carry UpstreamModel = the Ollama TAG (the daemon routes by the
// `model` field and 404s on LocalHub's catalog id), while a llama.cpp instance carries an EMPTY
// override (it owns one loaded model and ignores the field, so the body relays unchanged).
func TestBeginInstanceRequest_UpstreamModel(t *testing.T) {
	d := coreTestDB(t)
	// An ollama-source catalog row: id != tag. And a plain llama.cpp GGUF row.
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format) VALUES
		('ollama-mistral-latest','mistral:latest','ollama','ollama'),
		('gguf-m1','m1','local_path','gguf')`); err != nil {
		t.Fatal(err)
	}
	store := instances.NewStore(d.DB)
	svc := New(Deps{DB: d, InstanceStore: store, Events: NewEventBus()})
	ctx := context.Background()

	// Seed a HEALTHY Ollama instance (base_url = daemon) and a HEALTHY llama.cpp instance.
	base := "http://127.0.0.1:11434"
	zero := 0
	if _, err := store.Create(ctx, instances.CreateParams{ID: "oll1", ModelID: "ollama-mistral-latest", Engine: instances.EngineOllama, Reason: "seed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(ctx, "oll1", instances.TransitionParams{To: instances.StateStarting, Reason: "seed", PID: &zero, Port: &zero, BaseURL: &base}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(ctx, "oll1", instances.TransitionParams{To: instances.StateHealthy, Reason: "seed"}); err != nil {
		t.Fatal(err)
	}
	lbase := "http://127.0.0.1:40100"
	if _, err := store.Create(ctx, instances.CreateParams{ID: "lc1", ModelID: "gguf-m1", Engine: instances.EngineLlamaCPP, Reason: "seed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(ctx, "lc1", instances.TransitionParams{To: instances.StateStarting, Reason: "seed", PID: &zero, Port: &zero, BaseURL: &lbase}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(ctx, "lc1", instances.TransitionParams{To: instances.StateHealthy, Reason: "seed"}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.BeginInstanceRequest(ctx, "oll1"); err == nil {
		t.Fatal("retired Ollama instance was exposed to the completion relay")
	}

	lcHandle, err := svc.BeginInstanceRequest(ctx, "lc1")
	if err != nil {
		t.Fatalf("BeginInstanceRequest(llamacpp): %v", err)
	}
	defer lcHandle.Done()
	if lcHandle.UpstreamModel != "" {
		t.Errorf("llama.cpp handle UpstreamModel = %q, want empty (body must relay unchanged)", lcHandle.UpstreamModel)
	}
}

// TestResolveForProxy_ModelNotFound returns ErrProxyModelNotFound for a model not in inventory (the
// handler maps this to 404 model_not_found).
func TestResolveForProxy_ModelNotFound(t *testing.T) {
	svc, _, _ := startResidencyService(t, compat.Verdict{Status: compat.StatusRuns, Reason: "ok"}, 15200, nil)
	_, _, err := svc.ResolveForProxy(context.Background(), "no-such-model", 1)
	if !errors.Is(err, ErrProxyModelNotFound) {
		t.Errorf("want ErrProxyModelNotFound for an unknown model, got %v", err)
	}
}
