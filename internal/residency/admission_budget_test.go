package residency

import (
	"context"
	"testing"

	"github.com/localhub/localhub/internal/instances"
)

// managerWithBudget builds a manager with a VRAM budget for the fix-5 tests.
func managerWithBudget(t *testing.T, budget int, probe FreeVRAMProbe, lister InstanceLister, evictor Evictor, est FootprintEstimator) *Manager {
	t.Helper()
	return NewManager(ManagerConfig{
		Probe: probe, Lister: lister, Evictor: evictor, Estimator: est, Clock: newFakeClock(),
		SafetyMarginMiB: 1024, VRAMBudgetMiB: budget,
	})
}

// TestAdmit_Budget_ClampsSupplyBelowPhysicalFree: the card has 15000 MiB physically free, but a
// 4500 MiB budget with a 2000 MiB resident instance leaves only 2500 MiB of budget headroom. A
// 3000-MiB-footprint request (need 4024) does NOT fit under the budget even though it would fit in
// physical free → it queues. This is the storm's reservation lever (fix 5).
func TestAdmit_Budget_ClampsSupplyBelowPhysicalFree(t *testing.T) {
	resident := mkInst("r", "resident", instances.EngineLlamaCPP, 0, 2000, baseTime)
	resident.Pinned = true // pinned so it is not an eviction candidate → forces a queue
	m := managerWithBudget(t, 4500,
		&countingProbe{vals: []int{15000}},
		staticLister{[]instances.Instance{resident}},
		&recordingEvictor{}, fixedEstimator{mib: 3000})

	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionQueue {
		t.Fatalf("decision = %s, want queue (budget should clamp supply below physical free). reason=%s", res.Decision, res.Reason)
	}
	// Supply reported in the blocked reason must be the budget headroom (4500-2000=2500), not 15000.
	if res.FreeAfterMiB != 2500 {
		t.Errorf("FreeAfterMiB = %d, want 2500 (budget 4500 − 2000 resident footprint)", res.FreeAfterMiB)
	}
}

// TestAdmit_Budget_FitsWithinHeadroom: a request that fits under the budget headroom loads directly,
// and the load reason notes the budget clamp when it applies.
func TestAdmit_Budget_FitsWithinHeadroom(t *testing.T) {
	m := managerWithBudget(t, 4500,
		&countingProbe{vals: []int{15000}},
		staticLister{nil}, &recordingEvictor{}, fixedEstimator{mib: 3000})
	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionLoadNew {
		t.Fatalf("decision = %s, want load_new (need 4024 <= budget 4500). reason=%s", res.Decision, res.Reason)
	}
	if res.FreeAfterMiB != 4500 {
		t.Errorf("supply = %d, want 4500 (budget clamps the 15000 physical free)", res.FreeAfterMiB)
	}
}

// TestAdmit_Budget_EvictionFreesBudgetHeadroom: under a tight budget an idle resident is evicted so
// its footprint no longer counts against the budget, freeing headroom for the new model. Verifies
// the supply is recomputed against the budget after the eviction (not just against the probe).
func TestAdmit_Budget_EvictionFreesBudgetHeadroom(t *testing.T) {
	idle := mkInst("idle", "old", instances.EngineLlamaCPP, 0, 2000, baseTime)
	// budget 4500, idle occupies 2000 → 2500 headroom. Need 4024 (footprint 3000). After evicting
	// idle, budget headroom = 4500 → fits. Probe rises 13000→15000 after the kill.
	probe := &countingProbe{vals: []int{13000, 15000}}
	evictor := &recordingEvictor{freed: 2000}
	m := managerWithBudget(t, 4500, probe,
		staticLister{[]instances.Instance{idle}}, evictor, fixedEstimator{mib: 3000})

	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionEvictThenLoad {
		t.Fatalf("decision = %s, want evict_then_load. reason=%s", res.Decision, res.Reason)
	}
	if len(res.Evicted) != 1 || res.Evicted[0] != "idle" {
		t.Errorf("evicted = %v, want [idle]", res.Evicted)
	}
	// After eviction, supply is min(probe 15000, budget 4500 − 0 resident) = 4500 >= need 4024.
	if res.FreeAfterMiB != 4500 {
		t.Errorf("post-evict supply = %d, want 4500", res.FreeAfterMiB)
	}
}

// TestAdmit_NoBudget_UsesPhysicalFree: with budget disabled (0) the supply is the raw probe free.
func TestAdmit_NoBudget_UsesPhysicalFree(t *testing.T) {
	m := newManager(t, &countingProbe{vals: []int{15000}}, staticLister{nil},
		&recordingEvictor{}, fixedEstimator{mib: 3000}, newFakeClock())
	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.FreeAfterMiB != 15000 {
		t.Errorf("supply = %d, want 15000 (no budget)", res.FreeAfterMiB)
	}
}
