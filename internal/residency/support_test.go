package residency

import (
	"testing"
	"time"

	"github.com/localhub/localhub/internal/instances"
)

// TestInFlightTracker: increment/decrement/active-count, clamped at zero.
func TestInFlightTracker(t *testing.T) {
	tr := NewInFlightTracker()
	if tr.ActiveCount("i1") != 0 {
		t.Fatal("fresh count should be 0")
	}
	if tr.IncrementActive("i1") != 1 || tr.IncrementActive("i1") != 2 {
		t.Fatal("increment should return the new count")
	}
	if tr.ActiveCount("i1") != 2 {
		t.Fatal("active count should be 2")
	}
	if tr.DecrementActive("i1") != 1 {
		t.Fatal("decrement should return 1")
	}
	tr.DecrementActive("i1")
	// Over-decrement must clamp at 0, never negative.
	if tr.DecrementActive("i1") != 0 || tr.ActiveCount("i1") != 0 {
		t.Fatal("count must clamp at 0 on over-decrement")
	}
	tr.Forget("i1")
}

// TestFootprint_PerEngine: llama.cpp/ollama use observed-peak-or-estimate; vLLM uses util×total.
func TestFootprint_PerEngine(t *testing.T) {
	// llama.cpp prefers observed peak over estimate.
	if got := Footprint(FootprintInputs{Engine: "llamacpp", EstimatedMiB: 6000, ObservedPeakMiB: 5200}); got != 5200 {
		t.Errorf("llamacpp footprint = %d, want observed peak 5200", got)
	}
	// llama.cpp falls back to estimate when no peak.
	if got := Footprint(FootprintInputs{Engine: "llamacpp", EstimatedMiB: 6000}); got != 6000 {
		t.Errorf("llamacpp footprint = %d, want estimate 6000", got)
	}
	// ollama same model-sized behavior.
	if got := Footprint(FootprintInputs{Engine: "ollama", EstimatedMiB: 1567}); got != 1567 {
		t.Errorf("ollama footprint = %d, want 1567", got)
	}
	// vLLM uses util×total regardless of model size.
	if got := Footprint(FootprintInputs{Engine: "vllm", GPUMemUtil: 0.90, TotalVRAMMiB: 16380}); got != 14742 {
		t.Errorf("vllm footprint = %d, want 14742", got)
	}
	// vLLM lowering util lowers footprint (the co-reside lever).
	half := Footprint(FootprintInputs{Engine: "vllm", GPUMemUtil: 0.45, TotalVRAMMiB: 16380})
	if half >= 14742 {
		t.Errorf("lowering vLLM util should lower footprint, got %d", half)
	}
	// vLLM with default util when none specified.
	if got := Footprint(FootprintInputs{Engine: "vllm", DefaultGPUMemUtil: 0.90, TotalVRAMMiB: 16380}); got != 14742 {
		t.Errorf("vllm footprint with default util = %d, want 14742", got)
	}
}

// TestEvaluateDrain: a busy instance keeps draining until it drains or the timeout elapses; an
// idle one stops immediately. Eviction never reaches this — it is the user-stop drain path.
func TestEvaluateDrain(t *testing.T) {
	now := baseTime
	drainingSince := now.Add(-10 * time.Second)

	// Idle → stop immediately (drained).
	d := EvaluateDrain("i1", fakeTracker{}, drainingSince, now, 300*time.Second)
	if !d.Stop || d.TimedOut {
		t.Fatalf("idle instance should stop cleanly, got %+v", d)
	}

	// Busy, within timeout → do NOT stop (never-kill-active).
	d = EvaluateDrain("i1", fakeTracker{"i1": 2}, drainingSince, now, 300*time.Second)
	if d.Stop {
		t.Fatalf("busy instance within drain timeout must NOT stop, got %+v", d)
	}

	// Busy, past timeout → stop anyway (documented drain-timeout override for a user stop).
	past := now.Add(-400 * time.Second)
	d = EvaluateDrain("i1", fakeTracker{"i1": 2}, past, now, 300*time.Second)
	if !d.Stop || !d.TimedOut {
		t.Fatalf("busy instance past drain timeout should stop (timed out), got %+v", d)
	}
}

// TestClassFromPriority maps raw priority ints to classes with clamping.
func TestClassFromPriority(t *testing.T) {
	cases := map[int]PriorityClass{
		-5: ClassInteractive, 0: ClassInteractive, 1: ClassNormal, 2: ClassBackground, 9: ClassBackground,
	}
	for in, want := range cases {
		if got := ClassFromPriority(in); got != want {
			t.Errorf("ClassFromPriority(%d) = %v, want %v", in, got, want)
		}
	}
}

// TestStoreLister_NilStore returns a readable error rather than panicking.
func TestStoreLister_NilStore(t *testing.T) {
	l := StoreLister{}
	if _, err := l.ActiveInstances(nil); err == nil {
		t.Fatal("nil store should error")
	}
}

// sanity: instances engine constants line up with the strings we use.
func TestEngineConstantsSanity(t *testing.T) {
	if string(instances.EngineVLLM) != "vllm" || string(instances.EngineLlamaCPP) != "llamacpp" {
		t.Fatal("engine constant drift")
	}
}
