package core

import (
	"context"
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/engines/vllm"
	"github.com/localhub/localhub/internal/hardware"
	"github.com/localhub/localhub/internal/instances"
)

// fakeVLLMRunner records Start/Stop and returns canned instances.
type fakeVLLMRunner struct {
	startCalled bool
	startSpec   vllm.StartSpec
	stopCalled  bool
}

func (f *fakeVLLMRunner) Start(_ context.Context, spec vllm.StartSpec) (instances.Instance, error) {
	f.startCalled = true
	f.startSpec = spec
	return instances.Instance{
		ID: spec.InstanceID, ModelID: spec.ModelID, Engine: instances.EngineVLLM,
		State: instances.StateHealthy, BaseURL: spec.AttachURL, Reason: "healthy",
	}, nil
}

func (f *fakeVLLMRunner) Stop(_ context.Context, id string) (vllm.StopResult, error) {
	f.stopCalled = true
	return vllm.StopResult{Instance: instances.Instance{ID: id, State: instances.StateStopped}, Verified: true, Reason: "detached"}, nil
}

func TestStartVLLMInstance_AttachMode(t *testing.T) {
	d := coreTestDB(t)
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, repo)
		VALUES ('vm','vLLM Model','huggingface','safetensors','Qwen/Qwen3-0.6B')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO engines (name, installed) VALUES ('vllm',1)`); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Engines.VLLM.Mode = "attach"
	cfg.Engines.VLLM.AttachURL = "http://127.0.0.1:8000"

	snap := hardware.DefaultFakeSnapshot()
	fr := &fakeVLLMRunner{}
	svc := New(Deps{
		DB: d, Hardware: &hardware.FakeProbe{Snap: snap}, Engines: EnginesRegistry{},
		Compat:        &fakeCompat{verdict: compat.Verdict{Status: compat.StatusRuns, Reason: "ok"}},
		InstanceStore: instances.NewStore(d.DB), VLLMRunner: fr, Events: NewEventBus(), Cfg: cfg,
	})

	view, err := svc.StartInstance(context.Background(), StartInstanceRequest{ModelID: "vm", Engine: "vllm"})
	if err != nil {
		t.Fatalf("StartInstance(vllm attach): %v", err)
	}
	if !fr.startCalled {
		t.Fatal("expected vLLM runner Start called")
	}
	if fr.startSpec.AttachURL != "http://127.0.0.1:8000" {
		t.Errorf("attach URL not passed: %q", fr.startSpec.AttachURL)
	}
	if view.State != string(instances.StateHealthy) {
		t.Errorf("state: got %q want HEALTHY", view.State)
	}
}

func TestStopVLLMInstance_RoutesToVLLMRunner(t *testing.T) {
	d := coreTestDB(t)
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format) VALUES ('vm','vLLM','huggingface','safetensors')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO engines (name, installed) VALUES ('vllm',1)`); err != nil {
		t.Fatal(err)
	}
	fr := &fakeVLLMRunner{}
	store := instances.NewStore(d.DB)
	svc := New(Deps{
		DB: d, Hardware: &hardware.FakeProbe{Snap: hardware.DefaultFakeSnapshot()}, Engines: EnginesRegistry{},
		InstanceStore: store, VLLMRunner: fr, Events: NewEventBus(), Cfg: config.Default(),
	})
	// Seed a vLLM instance to stop.
	if _, err := store.Create(context.Background(), instances.CreateParams{
		ID: "v1", ModelID: "vm", Engine: instances.EngineVLLM, Reason: "seed",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StopInstance(context.Background(), "v1"); err != nil {
		t.Fatalf("StopInstance(vllm): %v", err)
	}
	if !fr.stopCalled {
		t.Error("expected vLLM runner Stop called")
	}
}

func TestStartVLLMInstance_RefusedWontRun_Docker(t *testing.T) {
	d := coreTestDB(t)
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, repo)
		VALUES ('vm','vLLM','huggingface','safetensors','Qwen/Qwen3-0.6B')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO engines (name, installed) VALUES ('vllm',1)`); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Engines.VLLM.Mode = "docker"

	fr := &fakeVLLMRunner{}
	svc := New(Deps{
		DB: d, Hardware: &hardware.FakeProbe{Snap: hardware.DefaultFakeSnapshot()}, Engines: EnginesRegistry{},
		Compat:        &fakeCompat{verdict: compat.Verdict{Status: compat.StatusWontRun, Reason: "too big", EstimatedVRAMMiB: 99000}},
		InstanceStore: instances.NewStore(d.DB), VLLMRunner: fr, Events: NewEventBus(), Cfg: cfg,
	})
	_, err := svc.StartInstance(context.Background(), StartInstanceRequest{ModelID: "vm", Engine: "vllm"})
	if err == nil || !strings.Contains(err.Error(), "REFUSED") {
		t.Fatalf("expected REFUSED for wont_run docker start, got %v", err)
	}
	if fr.startCalled {
		t.Error("GATE VIOLATION: vLLM Start called despite refused verdict")
	}
}
