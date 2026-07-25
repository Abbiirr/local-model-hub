package core

import (
	"context"
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/engines/ollama"
	"github.com/localhub/localhub/internal/hardware"
	"github.com/localhub/localhub/internal/instances"
)

// fakeOllamaRunner records Warm/Stop calls and returns canned instances.
type fakeOllamaRunner struct {
	warmCalled bool
	warmSpec   ollama.WarmSpec
	warmErr    error
	stopCalled bool
	stopTag    string
}

func (f *fakeOllamaRunner) Warm(_ context.Context, spec ollama.WarmSpec) (instances.Instance, error) {
	f.warmCalled = true
	f.warmSpec = spec
	if f.warmErr != nil {
		return instances.Instance{}, f.warmErr
	}
	return instances.Instance{
		ID: spec.InstanceID, ModelID: spec.ModelID, Engine: instances.EngineOllama,
		State: instances.StateHealthy, VRAMObsMiB: 700, Reason: "warm confirmed",
	}, nil
}

func (f *fakeOllamaRunner) Stop(_ context.Context, id, tag string) (ollama.StopResult, error) {
	f.stopCalled = true
	f.stopTag = tag
	return ollama.StopResult{
		Instance: instances.Instance{ID: id, State: instances.StateStopped}, Gone: true, Verified: true,
		Reason: "unloaded and VERIFIED",
	}, nil
}

// startOllamaTestService builds a Service with a seeded ollama-source model and a fake runner.
func startOllamaTestService(t *testing.T, verdict compat.Verdict, freeVRAM int) (*Service, *fakeOllamaRunner) {
	t.Helper()
	d := coreTestDB(t)
	// An Ollama catalog model: id="ollama-mistral-latest", display_name="mistral:latest".
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, quantization)
		VALUES ('ollama-mistral-latest','mistral:latest','ollama','ollama','q4_0')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO engines (name, installed) VALUES ('ollama',1)`); err != nil {
		t.Fatal(err)
	}

	snap := hardware.DefaultFakeSnapshot()
	snap.GPUs[0].VRAMFreeMiB = freeVRAM
	probe := &hardware.FakeProbe{Snap: snap}

	fr := &fakeOllamaRunner{}
	svc := New(Deps{
		DB:            d,
		Hardware:      probe,
		Engines:       EnginesRegistry{},
		Compat:        &fakeCompat{verdict: verdict},
		InstanceStore: instances.NewStore(d.DB),
		OllamaRunner:  fr,
		Events:        NewEventBus(),
		Cfg:           config.Default(),
	})
	return svc, fr
}

func TestStartOllamaInstance_WarmsOnRunsVerdict(t *testing.T) {
	svc, fr := startOllamaTestService(t, compat.Verdict{
		Status: compat.StatusRuns, Reason: "fits", EstimatedVRAMMiB: 700,
	}, 15200)

	view, err := svc.StartInstance(context.Background(), StartInstanceRequest{
		ModelID: "ollama-mistral-latest", Engine: "ollama", Ctx: 4096,
	})
	if err != nil {
		t.Fatalf("StartInstance(ollama): %v", err)
	}
	if !fr.warmCalled {
		t.Fatal("expected the Ollama runner's Warm to be called")
	}
	// The runner must receive the resolved Ollama TAG, not the catalog id.
	if fr.warmSpec.OllamaTag != "mistral:latest" {
		t.Errorf("OllamaTag: got %q want mistral:latest", fr.warmSpec.OllamaTag)
	}
	if fr.warmSpec.ModelID != "ollama-mistral-latest" {
		t.Errorf("ModelID: got %q want ollama-mistral-latest", fr.warmSpec.ModelID)
	}
	if view.State != string(instances.StateHealthy) {
		t.Errorf("state: got %q want HEALTHY", view.State)
	}
	if view.Engine != "ollama" {
		t.Errorf("engine: got %q want ollama", view.Engine)
	}
}

func TestStartOllamaInstance_RefusedWhenWontRun(t *testing.T) {
	svc, fr := startOllamaTestService(t, compat.Verdict{
		Status: compat.StatusWontRun, Reason: "needs 20000 MiB", EstimatedVRAMMiB: 20000,
	}, 15200)

	_, err := svc.StartInstance(context.Background(), StartInstanceRequest{
		ModelID: "ollama-mistral-latest", Engine: "ollama",
	})
	if err == nil {
		t.Fatal("expected refusal for wont_run verdict")
	}
	if !strings.Contains(err.Error(), "REFUSED") {
		t.Errorf("refusal should say REFUSED: %v", err)
	}
	if fr.warmCalled {
		t.Error("GATE VIOLATION: Warm called despite a refused verdict")
	}
}

func TestStopOllamaInstance_UnloadsWithTag(t *testing.T) {
	svc, fr := startOllamaTestService(t, compat.Verdict{Status: compat.StatusRuns, Reason: "ok"}, 15200)
	ctx := context.Background()
	// Seed a HEALTHY Ollama instance for the model.
	if _, err := svc.deps.InstanceStore.Create(ctx, instances.CreateParams{
		ID: "oll-1", ModelID: "ollama-mistral-latest", Engine: instances.EngineOllama, Reason: "seed",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.StopInstance(ctx, "oll-1"); err != nil {
		t.Fatalf("StopInstance(ollama): %v", err)
	}
	if !fr.stopCalled {
		t.Fatal("expected the Ollama runner's Stop to be called")
	}
	if fr.stopTag != "mistral:latest" {
		t.Errorf("Stop tag: got %q want mistral:latest (resolved from model_id)", fr.stopTag)
	}
}
