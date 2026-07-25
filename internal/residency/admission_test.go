package residency

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/instances"
)

// --- fakes for admission tests -----------------------------------------------------------------

// countingProbe returns free VRAM from a script and counts every read (the #p51 re-read
// assertion). Each call returns vals[min(call-1, len-1)] so a short script holds its last value.
type countingProbe struct {
	vals  []int
	calls int
}

func (p *countingProbe) FreeVRAMMiB(ctx context.Context) (int, error) {
	i := p.calls
	p.calls++
	if i >= len(p.vals) {
		i = len(p.vals) - 1
	}
	return p.vals[i], nil
}

// staticLister returns a fixed active-instance set.
type staticLister struct{ list []instances.Instance }

func (l staticLister) ActiveInstances(ctx context.Context) ([]instances.Instance, error) {
	return l.list, nil
}

// recordingEvictor records which instances it was asked to evict and returns a fixed freed value.
// It does NOT itself mutate the lister — the probe script models the resulting free VRAM.
type recordingEvictor struct {
	evicted []string
	freed   int
	err     error
}

func (e *recordingEvictor) Evict(ctx context.Context, inst instances.Instance) (int, error) {
	e.evicted = append(e.evicted, inst.ID)
	return e.freed, e.err
}

// fixedEstimator returns a constant footprint.
type fixedEstimator struct {
	mib int
	err error
}

func (e fixedEstimator) Footprint(ctx context.Context, spec AdmitSpec) (int, error) {
	return e.mib, e.err
}

func newManager(t *testing.T, probe FreeVRAMProbe, lister InstanceLister, evictor Evictor, est FootprintEstimator, clk Clock) *Manager {
	t.Helper()
	return NewManager(ManagerConfig{
		Probe: probe, Lister: lister, Evictor: evictor, Estimator: est, Clock: clk,
		SafetyMarginMiB: 1024,
	})
}

// TestAdmit_WarmHit: a HEALTHY resident instance for the model → route_loaded, no probe read of
// eviction machinery.
func TestAdmit_WarmHit(t *testing.T) {
	warm := mkInst("w", "m1", instances.EngineLlamaCPP, 0, 4000, baseTime)
	m := newManager(t, &countingProbe{vals: []int{15000}}, staticLister{[]instances.Instance{warm}},
		&recordingEvictor{}, fixedEstimator{mib: 4000}, newFakeClock())
	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionRouteLoaded {
		t.Fatalf("decision = %s, want route_loaded", res.Decision)
	}
	if res.WarmInstance == nil || res.WarmInstance.ID != "w" {
		t.Fatalf("route_loaded should carry the warm instance")
	}
	if res.Reason == "" {
		t.Fatal("route_loaded missing reason")
	}
}

// TestAdmit_FitsDirectly: free VRAM >= footprint + margin → load_new, no eviction, exactly ONE
// probe read.
func TestAdmit_FitsDirectly(t *testing.T) {
	probe := &countingProbe{vals: []int{15000}}
	m := newManager(t, probe, staticLister{nil}, &recordingEvictor{}, fixedEstimator{mib: 6000}, newFakeClock())
	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionLoadNew {
		t.Fatalf("decision = %s, want load_new (%s)", res.Decision, res.Reason)
	}
	if res.NeedMiB != 6000+1024 {
		t.Fatalf("need = %d, want 7024", res.NeedMiB)
	}
	if probe.calls != 1 {
		t.Fatalf("probe read %d times for a direct fit, want exactly 1", probe.calls)
	}
}

// TestAdmit_FitsAfterEvict_RereadsAfterEachEviction is THE #p51 gate: eviction happens one at a
// time and free VRAM is RE-READ from the probe after EACH eviction (asserted via probe call
// counts). Two evictions are needed; the probe is read once up front + once after each eviction.
func TestAdmit_FitsAfterEvict_RereadsAfterEachEviction(t *testing.T) {
	// Two idle victims, each ~4000 MiB. Footprint 9000 + margin 1024 = 10024 need.
	v1 := mkInst("v1", "a", instances.EngineLlamaCPP, 0, 4000, baseTime.Add(-30*time.Minute))
	v2 := mkInst("v2", "b", instances.EngineLlamaCPP, 0, 4000, baseTime.Add(-20*time.Minute))
	lister := staticLister{[]instances.Instance{v1, v2}}
	evictor := &recordingEvictor{freed: 4000}

	// Probe script: [initial=3000] then after evicting v1 → 7000, after evicting v2 → 11000.
	probe := &countingProbe{vals: []int{3000, 7000, 11000}}
	m := newManager(t, probe, lister, evictor, fixedEstimator{mib: 9000}, EvictionPolicy{Now: baseTime}.clockAt())

	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "target", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionEvictThenLoad {
		t.Fatalf("decision = %s, want evict_then_load (%s)", res.Decision, res.Reason)
	}
	// The re-read assertion: 1 initial + 1 after each of 2 evictions = 3 probe reads.
	if probe.calls != 3 {
		t.Fatalf("probe read %d times; want 3 (1 initial + 1 per eviction) — per-eviction re-read is the anti-OOM gate", probe.calls)
	}
	if len(evictor.evicted) != 2 {
		t.Fatalf("evicted %v, want 2 instances", evictor.evicted)
	}
	if res.FreeAfterMiB != 11000 {
		t.Fatalf("FreeAfterMiB = %d, want 11000 (final re-read)", res.FreeAfterMiB)
	}
}

// TestAdmit_AntiOOM_MidSequenceVRAMSurprise is THE anti-OOM property test: another process grabs
// VRAM between evictions, so the re-read after the last eviction shows we STILL don't fit. The
// decision must DEGRADE to queue (not a blind start).
func TestAdmit_AntiOOM_MidSequenceVRAMSurprise(t *testing.T) {
	v1 := mkInst("v1", "a", instances.EngineLlamaCPP, 0, 4000, baseTime.Add(-30*time.Minute))
	v2 := mkInst("v2", "b", instances.EngineLlamaCPP, 0, 4000, baseTime.Add(-20*time.Minute))
	lister := staticLister{[]instances.Instance{v1, v2}}
	evictor := &recordingEvictor{freed: 4000}

	// Footprint 9000 + 1024 = 10024 need. Simulation predicts 8000 reclaim from v1+v2 over an
	// initial 3000 free = 11000 → looks enough. BUT the probe re-reads show another process ate
	// VRAM: initial 3000, after v1 → 5000 (not 7000!), after v2 → 8500 (not 11000!). Final 8500 <
	// 10024 → must degrade to queue, NOT start.
	probe := &countingProbe{vals: []int{3000, 5000, 8500}}
	m := newManager(t, probe, lister, evictor, fixedEstimator{mib: 9000}, EvictionPolicy{Now: baseTime}.clockAt())

	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "target", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionQueue {
		t.Fatalf("ANTI-OOM FAILURE: decision = %s, want queue — a mid-sequence VRAM surprise must NOT lead to a blind start. reason: %s", res.Decision, res.Reason)
	}
	if probe.calls != 3 {
		t.Fatalf("probe read %d times; want 3 (re-read after each eviction is what CAUGHT the surprise)", probe.calls)
	}
	// The reason must flag the anti-OOM degradation and name the shortfall numbers.
	for _, tok := range []string{"anti-OOM", "8500", "short"} {
		if !contains(res.Reason, tok) {
			t.Errorf("anti-OOM reason %q missing token %q", res.Reason, tok)
		}
	}
	// It must have been enqueued (a queue id assigned).
	if res.QueueID == "" {
		t.Fatal("anti-OOM degradation should enqueue the request (queue id expected)")
	}
}

// TestAdmit_CannotFit_Queued: no evictable idle capacity → queued with a numeric blocked_reason.
func TestAdmit_CannotFit_Queued(t *testing.T) {
	// One pinned instance (never evictable) → nothing reclaimable.
	pinned := mkInst("p", "a", instances.EngineLlamaCPP, 0, 4000, baseTime.Add(-time.Hour))
	pinned.Pinned = true
	probe := &countingProbe{vals: []int{2000}}
	m := newManager(t, probe, staticLister{[]instances.Instance{pinned}}, &recordingEvictor{},
		fixedEstimator{mib: 9000}, EvictionPolicy{Now: baseTime}.clockAt())

	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "target", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionQueue {
		t.Fatalf("decision = %s, want queue (%s)", res.Decision, res.Reason)
	}
	// blocked_reason must name: needed, free, reclaimable, idle count.
	for _, tok := range []string{"need", "free", "reclaimable", "short"} {
		if !contains(res.Reason, tok) {
			t.Errorf("blocked_reason %q missing token %q", res.Reason, tok)
		}
	}
	if res.QueuePosition != 1 {
		t.Errorf("queue position = %d, want 1 (first in an empty queue)", res.QueuePosition)
	}
}

// TestAdmit_CannotFit_RejectWhenCapped: when the class queue is at capacity, a non-fitting request
// is rejected (not enqueued) with the queue_full reason.
func TestAdmit_CannotFit_RejectWhenCapped(t *testing.T) {
	q := NewQueue(QueueConfig{Caps: ClassCaps{Interactive: 1, Normal: 1, Background: 1}}, EvictionPolicy{Now: baseTime}.clockAt())
	// Pre-fill the normal queue to its cap of 1.
	if err := q.Enqueue(Request{ID: "filler", ModelID: "x", Class: ClassNormal}); err != nil {
		t.Fatal(err)
	}
	probe := &countingProbe{vals: []int{1000}}
	m := NewManager(ManagerConfig{
		Queue: q, Probe: probe, Lister: staticLister{nil}, Evictor: &recordingEvictor{},
		Estimator: fixedEstimator{mib: 9000}, Clock: EvictionPolicy{Now: baseTime}.clockAt(),
		SafetyMarginMiB: 1024,
	})
	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "target", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionReject {
		t.Fatalf("decision = %s, want reject (queue capped)", res.Decision)
	}
	if !contains(res.Reason, "queue_full") {
		t.Errorf("reject reason %q should mention queue_full", res.Reason)
	}
}

// TestAdmit_EstimatorError_Rejects: a footprint we cannot estimate is rejected, never guessed.
func TestAdmit_EstimatorError_Rejects(t *testing.T) {
	probe := &countingProbe{vals: []int{15000}}
	m := newManager(t, probe, staticLister{nil}, &recordingEvictor{},
		fixedEstimator{err: fmt.Errorf("unparseable TQ3 quant")}, newFakeClock())
	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "weird", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionReject {
		t.Fatalf("decision = %s, want reject on unestimable footprint (never guess a number that could OOM)", res.Decision)
	}
	if !contains(res.Reason, "cannot estimate") {
		t.Errorf("reject reason %q should explain the unestimable footprint", res.Reason)
	}
}

// TestAdmit_VLLMFootprint: a vLLM request budgets util×total, not the model size.
func TestAdmit_VLLMFootprint(t *testing.T) {
	// Footprint precomputed via the per-engine model: 0.90 × 16380 = 14742.
	fp := Footprint(FootprintInputs{Engine: "vllm", GPUMemUtil: 0.90, TotalVRAMMiB: 16380})
	if fp != 14742 {
		t.Fatalf("vLLM footprint = %d, want 14742 (0.90 × 16380)", fp)
	}
	// With only 13000 free, a vLLM instance at util 0.90 does NOT fit (needs 14742 + margin),
	// even though a same-VRAM llama.cpp model of 6000 MiB would.
	probe := &countingProbe{vals: []int{13000}}
	m := newManager(t, probe, staticLister{nil}, &recordingEvictor{}, fixedEstimator{}, newFakeClock())
	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "vm", Engine: "vllm", Class: ClassNormal, FootprintMiB: fp})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision == DecisionLoadNew {
		t.Fatalf("vLLM at util 0.90 must NOT load_new with only 13000 free (needs %d + margin); got load_new", fp)
	}
}

// TestAdmit_EveryDecisionCarriesReason is the schema gate: every admission outcome (route/load/
// evict/queue/reject) MUST carry a non-empty reason. Drives all five decisions and asserts the
// reason is present (and, for the numeric paths, names at least one number).
func TestAdmit_EveryDecisionCarriesReason(t *testing.T) {
	hasDigit := func(s string) bool {
		for _, c := range s {
			if c >= '0' && c <= '9' {
				return true
			}
		}
		return false
	}
	type scenario struct {
		name    string
		build   func() (*Manager, AdmitSpec)
		wantDec Decision
		numeric bool
	}
	scenarios := []scenario{
		{
			name: "route_loaded",
			build: func() (*Manager, AdmitSpec) {
				warm := mkInst("w", "m1", instances.EngineLlamaCPP, 0, 4000, baseTime)
				return newManager(t, &countingProbe{vals: []int{15000}}, staticLister{[]instances.Instance{warm}},
					&recordingEvictor{}, fixedEstimator{mib: 4000}, newFakeClock()), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal}
			},
			wantDec: DecisionRouteLoaded,
		},
		{
			name: "load_new",
			build: func() (*Manager, AdmitSpec) {
				return newManager(t, &countingProbe{vals: []int{15000}}, staticLister{nil}, &recordingEvictor{},
					fixedEstimator{mib: 6000}, newFakeClock()), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal}
			},
			wantDec: DecisionLoadNew, numeric: true,
		},
		{
			name: "evict_then_load",
			build: func() (*Manager, AdmitSpec) {
				v := mkInst("v", "a", instances.EngineLlamaCPP, 0, 8000, baseTime.Add(-time.Hour))
				return newManager(t, &countingProbe{vals: []int{3000, 12000}}, staticLister{[]instances.Instance{v}},
						&recordingEvictor{freed: 8000}, fixedEstimator{mib: 9000}, EvictionPolicy{Now: baseTime}.clockAt()),
					AdmitSpec{ModelID: "target", Engine: "llamacpp", Class: ClassNormal}
			},
			wantDec: DecisionEvictThenLoad, numeric: true,
		},
		{
			name: "queue",
			build: func() (*Manager, AdmitSpec) {
				pinned := mkInst("p", "a", instances.EngineLlamaCPP, 0, 4000, baseTime.Add(-time.Hour))
				pinned.Pinned = true
				return newManager(t, &countingProbe{vals: []int{2000}}, staticLister{[]instances.Instance{pinned}},
						&recordingEvictor{}, fixedEstimator{mib: 9000}, EvictionPolicy{Now: baseTime}.clockAt()),
					AdmitSpec{ModelID: "target", Engine: "llamacpp", Class: ClassNormal}
			},
			wantDec: DecisionQueue, numeric: true,
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			m, spec := sc.build()
			res, err := m.Admit(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if res.Decision != sc.wantDec {
				t.Fatalf("decision = %s, want %s (%s)", res.Decision, sc.wantDec, res.Reason)
			}
			if res.Reason == "" {
				t.Fatal("SCHEMA VIOLATION: decision carries an empty reason")
			}
			if sc.numeric && !hasDigit(res.Reason) {
				t.Errorf("reason for %s should name a number: %q", sc.name, res.Reason)
			}
		})
	}
}

// clockAt turns an EvictionPolicy's Now into a Clock for the manager (small test helper).
func (p EvictionPolicy) clockAt() Clock { return &fixedClock{t: p.Now} }

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }
