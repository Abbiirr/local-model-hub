package residency

import (
	"context"
	"testing"
)

// admission_ram_test.go covers fix 4: RAM headroom as an admission constraint (full-project-review
// finding 4). On a small-RAM box RAM binds before VRAM, so admission must refuse to start a NEW
// process/warm when FRESH free system RAM is below the configured floor — queueing with a numeric
// ram_blocked reason on the SAME queue/drain path as a VRAM shortage. It is a global guard: no
// per-model RAM estimate, no RAM-based victim selection (VRAM eviction policy unchanged).

// scriptedRAMProbe returns free system RAM from a script and counts reads (proving admission probes
// RAM FRESH each decision, like the VRAM re-read). A short script holds its last value.
type scriptedRAMProbe struct {
	vals  []int
	calls int
}

func (p *scriptedRAMProbe) FreeRAMMiB(ctx context.Context) (int, error) {
	i := p.calls
	p.calls++
	if i >= len(p.vals) {
		i = len(p.vals) - 1
	}
	return p.vals[i], nil
}

// newRAMManager builds a manager with plenty of free VRAM (so VRAM never blocks) and a RAM floor +
// RAM probe wired, isolating the RAM-floor behavior.
func newRAMManager(t *testing.T, ram FreeRAMProbe, floorMiB int) *Manager {
	t.Helper()
	return NewManager(ManagerConfig{
		Probe:           &countingProbe{vals: []int{60000}}, // huge free VRAM — VRAM never the blocker
		RAMProbe:        ram,
		Lister:          staticLister{nil},
		Evictor:         &recordingEvictor{},
		Estimator:       fixedEstimator{mib: 4000},
		Clock:           newFakeClock(),
		SafetyMarginMiB: 1024,
		MinFreeRAMMiB:   floorMiB,
	})
}

// TestAdmit_RAMBelowFloor_Queues: free RAM below the floor → DecisionQueue with a numeric
// ram_blocked reason (never a load, even though VRAM has ample room).
func TestAdmit_RAMBelowFloor_Queues(t *testing.T) {
	ram := &scriptedRAMProbe{vals: []int{1800}} // below the 2048 floor
	m := newRAMManager(t, ram, 2048)

	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionQueue {
		t.Fatalf("decision = %s, want queue (RAM below floor must not load) — reason: %s", res.Decision, res.Reason)
	}
	if res.Reason == "" {
		t.Fatal("SCHEMA VIOLATION: RAM block carries an empty reason")
	}
	for _, tok := range []string{"ram_blocked", "1800", "2048"} {
		if !contains(res.Reason, tok) {
			t.Errorf("ram_blocked reason %q missing token %q", res.Reason, tok)
		}
	}
	if ram.calls == 0 {
		t.Fatal("admission did not probe RAM (fresh RAM read required)")
	}
	if res.QueueID == "" {
		t.Fatal("RAM-blocked request should be enqueued (queue id present) so the drain loop can re-admit it")
	}
}

// TestAdmit_RAMAboveFloor_Unaffected: free RAM at/above the floor → admission proceeds exactly as
// before (load_new here), i.e. the RAM guard is invisible when RAM is healthy.
func TestAdmit_RAMAboveFloor_Unaffected(t *testing.T) {
	ram := &scriptedRAMProbe{vals: []int{8000}} // well above the 2048 floor
	m := newRAMManager(t, ram, 2048)

	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionLoadNew {
		t.Fatalf("decision = %s, want load_new (RAM above floor must not block) — reason: %s", res.Decision, res.Reason)
	}
}

// TestAdmit_RAMFloorAppliesToAllEngines: the guard is engine-agnostic — an Ollama warm and a vLLM
// docker start are RAM-blocked the same way a llama.cpp spawn is (item 4: ALL engine spawns/warms).
func TestAdmit_RAMFloorAppliesToAllEngines(t *testing.T) {
	for _, engine := range []string{"llamacpp", "ollama", "vllm"} {
		ram := &scriptedRAMProbe{vals: []int{500}}
		m := newRAMManager(t, ram, 2048)
		res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: engine, Class: ClassNormal})
		if err != nil {
			t.Fatalf("engine %s: %v", engine, err)
		}
		if res.Decision != DecisionQueue {
			t.Fatalf("engine %s: decision = %s, want queue (RAM floor must apply to all engines)", engine, res.Decision)
		}
		if !contains(res.Reason, "ram_blocked") {
			t.Errorf("engine %s: reason %q missing ram_blocked", engine, res.Reason)
		}
	}
}

// TestAdmit_RAMRecovers_ThenAdmits: a request blocked while RAM is below the floor is admitted on a
// subsequent decision once RAM recovers — the drain-loop re-admit path (Admit is what the drain
// loop calls). Same probe, second read above floor.
func TestAdmit_RAMRecovers_ThenAdmits(t *testing.T) {
	// First read below floor (blocks), second read above floor (admits).
	ram := &scriptedRAMProbe{vals: []int{1500, 4096}}
	m := newRAMManager(t, ram, 2048)

	res1, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res1.Decision != DecisionQueue {
		t.Fatalf("first admit decision = %s, want queue (RAM below floor)", res1.Decision)
	}

	// RAM has recovered; a re-admit (what DrainQueue does) now proceeds to load_new.
	res2, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Decision != DecisionLoadNew {
		t.Fatalf("second admit decision = %s, want load_new once RAM recovered — reason: %s", res2.Decision, res2.Reason)
	}
}

// TestAdmit_RAMFloorDisabledWhenUnset: with MinFreeRAMMiB=0 (or no RAM probe), the RAM guard is
// inert — admission behaves as VRAM-only (prior behavior), even with near-zero free RAM.
func TestAdmit_RAMFloorDisabledWhenUnset(t *testing.T) {
	ram := &scriptedRAMProbe{vals: []int{10}} // basically no RAM
	m := newRAMManager(t, ram, 0)             // floor disabled
	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionLoadNew {
		t.Fatalf("decision = %s, want load_new (RAM floor disabled → VRAM-only)", res.Decision)
	}
	if ram.calls != 0 {
		t.Fatalf("RAM probe should not be read when the floor is disabled (calls=%d)", ram.calls)
	}
}
