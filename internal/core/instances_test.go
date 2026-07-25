package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/engines/llamacpp"
	"github.com/localhub/localhub/internal/hardware"
	"github.com/localhub/localhub/internal/instances"
)

// fakeRunner records whether Start was called and returns a canned HEALTHY instance.
type fakeRunner struct {
	startCalled bool
	startSpec   llamacpp.StartSpec
	startErr    error
	inst        instances.Instance
	stopped     string
}

func (f *fakeRunner) Start(_ context.Context, spec llamacpp.StartSpec) (instances.Instance, error) {
	f.startCalled = true
	f.startSpec = spec
	if f.startErr != nil {
		return instances.Instance{}, f.startErr
	}
	f.inst = instances.Instance{
		ID: spec.InstanceID, ModelID: spec.ModelID, Engine: instances.EngineLlamaCPP,
		State: instances.StateHealthy, Port: 40100, BaseURL: "http://127.0.0.1:40100",
		BinaryPath: spec.Binary, ModelPath: spec.ModelPath, Ctx: spec.Ctx,
		VRAMEstMiB: spec.VRAMEstMiB, LaunchFlags: append([]string{spec.Binary}, spec.Flags...),
	}
	return f.inst, nil
}
func (f *fakeRunner) Stop(_ context.Context, id string) (llamacpp.StopResult, error) {
	f.stopped = id
	return llamacpp.StopResult{Instance: instances.Instance{ID: id, State: instances.StateStopped}, Verified: true}, nil
}
func (f *fakeRunner) TinyCompletion(context.Context, string, int) error        { return nil }
func (f *fakeRunner) RecordPeakVRAM(context.Context, string, int) (int, error) { return 0, nil }

// startTestService builds a core.Service with a seeded GGUF model, a fake compat verdict, a
// llama.cpp binary available, and a fake runner. free VRAM defaults to the reference fake (15200).
func startTestService(t *testing.T, verdict compat.Verdict, freeVRAM int) (*Service, *fakeRunner) {
	t.Helper()
	d := coreTestDB(t)
	dir := t.TempDir()
	gguf := filepath.Join(dir, "m1.gguf")
	if err := os.WriteFile(gguf, append([]byte("GGUF"), make([]byte, 64)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, quantization, local_path)
		VALUES ('m1','Model One','local_path','gguf','q4_k_m',?)`, gguf); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO engines (name, installed) VALUES ('llamacpp',1)`); err != nil {
		t.Fatal(err)
	}

	snap := hardware.DefaultFakeSnapshot()
	snap.GPUs[0].VRAMFreeMiB = freeVRAM
	probe := &hardware.FakeProbe{Snap: snap}

	// A llama.cpp binary must be "available" so PlanLlamaLaunch can pick it. We fake engine
	// availability by injecting a llama.cpp detector view via the registry is complex; instead
	// the descriptor path in StartInstance calls engineAvailability(ctx) which reads the
	// detection sweep. With a nil registry there are no binaries → PlanLlamaLaunch fails. So we
	// override the runner's Start to not depend on a real binary, and rely on the verdict gate.
	// To exercise the plan path we insert a detectable binary via a stub adapter is out of scope;
	// these tests focus on the SAFETY gate (no spawn on refusal) + the happy dispatch.

	fc := &fakeCompat{verdict: verdict}
	fr := &fakeRunner{}
	svc := New(Deps{
		DB:            d,
		Hardware:      probe,
		Engines:       EnginesRegistry{},
		Compat:        fc,
		InstanceStore: instances.NewStore(d.DB),
		LlamaRunner:   fr,
		Events:        NewEventBus(),
		Cfg:           config.Default(),
	})
	return svc, fr
}

// TestStartInstance_RefusedWhenWontRun: a wont_run verdict is REFUSED and nothing is spawned.
func TestStartInstance_RefusedWhenWontRun(t *testing.T) {
	svc, fr := startTestService(t, compat.Verdict{
		Status: compat.StatusWontRun, Reason: "needs 20000 MiB, only 13000 usable", EstimatedVRAMMiB: 20000,
	}, 15200)

	_, err := svc.StartInstance(context.Background(), StartInstanceRequest{ModelID: "m1", Ctx: 4096})
	if err == nil {
		t.Fatal("expected refusal for wont_run verdict")
	}
	if !strings.Contains(err.Error(), "REFUSED") {
		t.Errorf("refusal reason should say REFUSED: %v", err)
	}
	if fr.startCalled {
		t.Error("GATE VIOLATION: runner.Start was called despite a refused verdict")
	}
}

// TestStartInstance_RefusedWhenOverBudget: runs verdict but est > free-1500 → refused, no spawn,
// with a NUMERIC reason.
func TestStartInstance_RefusedWhenOverBudget(t *testing.T) {
	// free 15200, budget = 15200-1500 = 13700; est 14000 > budget → refuse.
	svc, fr := startTestService(t, compat.Verdict{
		Status: compat.StatusRuns, Reason: "tight fit", EstimatedVRAMMiB: 14000,
	}, 15200)

	_, err := svc.StartInstance(context.Background(), StartInstanceRequest{ModelID: "m1", Ctx: 4096})
	if err == nil {
		t.Fatal("expected refusal when est exceeds free-1500 budget")
	}
	msg := err.Error()
	if !strings.Contains(msg, "REFUSED") || !strings.Contains(msg, "14000") || !strings.Contains(msg, "13700") {
		t.Errorf("refusal must carry numeric reason (est/budget): %v", err)
	}
	if fr.startCalled {
		t.Error("GATE VIOLATION: runner.Start called despite over-budget refusal")
	}
}

// TestStopInstance_DispatchesToRunner: DELETE path reads the instance and routes to the owning
// engine's runner. A llama.cpp instance must reach the llama.cpp runner's Stop.
func TestStopInstance_DispatchesToRunner(t *testing.T) {
	svc, fr := startTestService(t, compat.Verdict{Status: compat.StatusRuns, Reason: "ok"}, 15200)
	// Seed a llama.cpp instance row so StopInstance can read its engine and route the stop.
	if _, err := svc.deps.InstanceStore.Create(context.Background(), instances.CreateParams{
		ID: "some-id", ModelID: "m1", Engine: instances.EngineLlamaCPP, Reason: "seed for stop test",
	}); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	if _, err := svc.StopInstance(context.Background(), "some-id"); err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	if fr.stopped != "some-id" {
		t.Errorf("runner.Stop not called with id; got %q", fr.stopped)
	}
}

// TestEngineForModelSource is the FINDING-2 routing gate: with req.Engine=="" the engine is inferred
// from the model's SOURCE and FORMAT (ADR-002/ADR-004): a safetensors row must default to vLLM (not
// fall through to llama.cpp, which then produces a non-GGUF error), a gguf row to llama.cpp, and an
// ollama-source row to Ollama.
func TestEngineForModelSource(t *testing.T) {
	d := coreTestDB(t)
	seed := func(id, source, format string) {
		if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format) VALUES (?,?,?,?)`,
			id, id, source, format); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("st1", "huggingface", "safetensors")
	seed("pt1", "huggingface", "pytorch")
	seed("gg1", "local_path", "gguf")
	seed("ol1", "ollama", "ollama")

	svc := New(Deps{DB: d})
	cases := []struct{ id, want string }{
		{"st1", string(instances.EngineVLLM)},
		{"pt1", string(instances.EngineVLLM)},
		{"gg1", string(instances.EngineLlamaCPP)},
		{"ol1", string(instances.EngineOllama)},
		{"missing", string(instances.EngineLlamaCPP)}, // unknown row → safe llama.cpp default
	}
	for _, c := range cases {
		if got := svc.engineForModelSource(context.Background(), c.id); got != c.want {
			t.Errorf("engineForModelSource(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}
