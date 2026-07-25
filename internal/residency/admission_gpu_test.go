package residency

import (
	"context"
	"testing"

	"github.com/localhub/localhub/internal/instances"
)

// admission_gpu_test.go covers the two GPU admission ceilings: max_gpu_temp_c (gpu_hot) and
// max_gpu_util_pct (gpu_busy). Like the RAM floor these are GLOBAL admission guards: admission
// refuses to start/warm a NEW model when the FRESH GPU state is above the ceiling, queueing with a
// numeric reason on the same queue/drain path; a warm route is exempt; eviction/stop is never
// affected. The drain loop re-admits once the card cools / frees.

// scriptedGPUProbe returns GPU temp+util from parallel scripts and counts reads (proving admission
// probes GPU state FRESH each decision). A short script holds its last value. A nil temps/utils
// slice yields a -1 "unavailable" reading for that field.
type scriptedGPUProbe struct {
	temps []int
	utils []int
	calls int
}

func (p *scriptedGPUProbe) GPUState(ctx context.Context) (tempC, utilPct int, err error) {
	i := p.calls
	p.calls++
	pick := func(vals []int) int {
		if len(vals) == 0 {
			return -1
		}
		j := i
		if j >= len(vals) {
			j = len(vals) - 1
		}
		return vals[j]
	}
	return pick(p.temps), pick(p.utils), nil
}

// newGPUManager builds a manager with plenty of free VRAM/RAM (so neither blocks) and the GPU probe
// + ceilings wired, isolating the GPU-ceiling behavior.
func newGPUManager(t *testing.T, gpu GPUStateProbe, maxTempC, maxUtilPct int) *Manager {
	t.Helper()
	return NewManager(ManagerConfig{
		Probe:           &countingProbe{vals: []int{60000}}, // huge free VRAM — VRAM never the blocker
		GPUProbe:        gpu,
		Lister:          staticLister{nil},
		Evictor:         &recordingEvictor{},
		Estimator:       fixedEstimator{mib: 4000},
		Clock:           newFakeClock(),
		SafetyMarginMiB: 1024,
		MaxGPUTempC:     maxTempC,
		MaxGPUUtilPct:   maxUtilPct,
	})
}

// TestAdmit_GPUOverTemp_Queues: temp above the ceiling → DecisionQueue with a numeric gpu_hot reason
// (never a load, even though VRAM has ample room).
func TestAdmit_GPUOverTemp_Queues(t *testing.T) {
	gpu := &scriptedGPUProbe{temps: []int{91}} // above the 87 ceiling
	m := newGPUManager(t, gpu, 87, 0)

	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionQueue {
		t.Fatalf("decision = %s, want queue (GPU over temp must not load) — reason: %s", res.Decision, res.Reason)
	}
	if res.Reason == "" {
		t.Fatal("SCHEMA VIOLATION: gpu_hot block carries an empty reason")
	}
	for _, tok := range []string{"gpu_hot", "91", "87"} {
		if !contains(res.Reason, tok) {
			t.Errorf("gpu_hot reason %q missing token %q", res.Reason, tok)
		}
	}
	if gpu.calls == 0 {
		t.Fatal("admission did not probe GPU state (fresh GPU read required)")
	}
	if res.QueueID == "" {
		t.Fatal("gpu_hot request should be enqueued (queue id present) so the drain loop can re-admit it")
	}
}

// TestAdmit_GPUTempDisabled_Unaffected: with MaxGPUTempC=0, the temp guard is inert even at an
// alarming temperature — admission behaves as before (load_new). The probe should not be read when
// both ceilings are disabled.
func TestAdmit_GPUTempDisabled_Unaffected(t *testing.T) {
	gpu := &scriptedGPUProbe{temps: []int{110}} // absurdly hot but the check is disabled
	m := newGPUManager(t, gpu, 0, 0)            // both ceilings disabled

	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionLoadNew {
		t.Fatalf("decision = %s, want load_new (temp ceiling disabled) — reason: %s", res.Decision, res.Reason)
	}
	if gpu.calls != 0 {
		t.Fatalf("GPU probe should not be read when both ceilings are disabled (calls=%d)", gpu.calls)
	}
}

// TestAdmit_GPUOverUtil_Queues: utilization above the ceiling → DecisionQueue with a numeric
// gpu_busy reason.
func TestAdmit_GPUOverUtil_Queues(t *testing.T) {
	gpu := &scriptedGPUProbe{utils: []int{95}} // above the 80 ceiling; temp unavailable (-1, inert)
	m := newGPUManager(t, gpu, 0, 80)

	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionQueue {
		t.Fatalf("decision = %s, want queue (GPU over util must not load) — reason: %s", res.Decision, res.Reason)
	}
	for _, tok := range []string{"gpu_busy", "95", "80"} {
		if !contains(res.Reason, tok) {
			t.Errorf("gpu_busy reason %q missing token %q", res.Reason, tok)
		}
	}
	if res.QueueID == "" {
		t.Fatal("gpu_busy request should be enqueued so the drain loop can re-admit it")
	}
}

// TestAdmit_GPUCeilingsAppliesToAllEngines: the guards are engine-agnostic — an Ollama warm and a
// vLLM docker start are gpu_hot the same way a llama.cpp spawn is (all engine spawns/warms).
func TestAdmit_GPUCeilingsAppliesToAllEngines(t *testing.T) {
	for _, engine := range []string{"llamacpp", "ollama", "vllm"} {
		gpu := &scriptedGPUProbe{temps: []int{95}}
		m := newGPUManager(t, gpu, 87, 0)
		res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: engine, Class: ClassNormal})
		if err != nil {
			t.Fatalf("engine %s: %v", engine, err)
		}
		if res.Decision != DecisionQueue {
			t.Fatalf("engine %s: decision = %s, want queue (GPU ceiling must apply to all engines)", engine, res.Decision)
		}
		if !contains(res.Reason, "gpu_hot") {
			t.Errorf("engine %s: reason %q missing gpu_hot", engine, res.Reason)
		}
	}
}

// TestAdmit_GPUCools_ThenAdmits: a request blocked while the GPU is over temp is admitted on a
// subsequent decision once the card cools — the drain-loop re-admit path (Admit is what the drain
// loop calls). Same probe, second read below the ceiling.
func TestAdmit_GPUCools_ThenAdmits(t *testing.T) {
	// First read above ceiling (blocks), second read below (admits).
	gpu := &scriptedGPUProbe{temps: []int{92, 70}}
	m := newGPUManager(t, gpu, 87, 0)

	res1, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res1.Decision != DecisionQueue {
		t.Fatalf("first admit decision = %s, want queue (GPU over temp)", res1.Decision)
	}

	res2, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Decision != DecisionLoadNew {
		t.Fatalf("second admit decision = %s, want load_new once GPU cooled — reason: %s", res2.Decision, res2.Reason)
	}
}

// TestAdmit_GPUUnavailableReading_FailsOpen: a negative (unavailable) temp/util reading must NOT
// trigger the ceiling — admission proceeds (fail-open on a probe gap rather than wedging).
func TestAdmit_GPUUnavailableReading_FailsOpen(t *testing.T) {
	gpu := &scriptedGPUProbe{temps: []int{-1}, utils: []int{-1}} // both unavailable
	m := newGPUManager(t, gpu, 87, 80)

	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionLoadNew {
		t.Fatalf("decision = %s, want load_new (unavailable GPU reading must fail open) — reason: %s", res.Decision, res.Reason)
	}
}

// TestAdmit_GPUHotButWarmRouteExempt: an already-HEALTHY-resident model routes to the warm instance
// even when the GPU is over temp — the ceiling gates NEW loads only (warm route needs no new load),
// exactly like the RAM floor is exempt for a warm route.
func TestAdmit_GPUHotButWarmRouteExempt(t *testing.T) {
	warm := instances.Instance{ID: "i1", ModelID: "m1", Engine: "llamacpp", State: instances.StateHealthy}
	gpu := &scriptedGPUProbe{temps: []int{99}} // scorching, but the model is already resident
	m := NewManager(ManagerConfig{
		Probe:           &countingProbe{vals: []int{60000}},
		GPUProbe:        gpu,
		Lister:          staticLister{[]instances.Instance{warm}},
		Evictor:         &recordingEvictor{},
		Estimator:       fixedEstimator{mib: 4000},
		Clock:           newFakeClock(),
		SafetyMarginMiB: 1024,
		MaxGPUTempC:     87,
		MaxGPUUtilPct:   80,
	})

	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionRouteLoaded {
		t.Fatalf("decision = %s, want route_loaded (warm route exempt from GPU ceiling)", res.Decision)
	}
	if gpu.calls != 0 {
		t.Fatalf("GPU probe should not be read for a warm route (checked after the warm hit); calls=%d", gpu.calls)
	}
}

// TestAdmit_GPUHotBeatsBusy: when the card is BOTH over-temp and over-util, the thermal reason wins
// (temp is checked first — it is the safety-critical one).
func TestAdmit_GPUHotBeatsBusy(t *testing.T) {
	gpu := &scriptedGPUProbe{temps: []int{95}, utils: []int{99}}
	m := newGPUManager(t, gpu, 87, 80)

	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionQueue {
		t.Fatalf("decision = %s, want queue", res.Decision)
	}
	if !contains(res.Reason, "gpu_hot") {
		t.Errorf("hot-and-busy reason %q should report gpu_hot first (safety-critical)", res.Reason)
	}
}
