package residency

import (
	"context"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/instances"
)

// captureLogger records admission decisions + eviction re-reads for the fix-2 observability test.
type captureLogger struct {
	decisions []AdmissionLog
	evictions []EvictionLog
}

func (c *captureLogger) AdmissionDecision(d AdmissionLog) { c.decisions = append(c.decisions, d) }
func (c *captureLogger) EvictionReRead(e EvictionLog)     { c.evictions = append(c.evictions, e) }

// managerWithLogger builds a manager with a capturing logger.
func managerWithLogger(t *testing.T, log AdmissionLogger, probe FreeVRAMProbe, lister InstanceLister, evictor Evictor, est FootprintEstimator) *Manager {
	t.Helper()
	return NewManager(ManagerConfig{
		Probe: probe, Lister: lister, Evictor: evictor, Estimator: est, Clock: newFakeClock(),
		SafetyMarginMiB: 1024, Logger: log,
	})
}

// TestAdmit_Logs_EvictionReReadValues is THE fix-2 gate: admission logs the free-VRAM value
// RE-READ from the probe after each eviction (proving the anti-OOM re-read is observable). Two
// evictions → two eviction re-read log lines carrying the post-eviction free values, plus one
// admission-decision line.
func TestAdmit_Logs_EvictionReReadValues(t *testing.T) {
	v1 := mkInst("v1", "a", instances.EngineLlamaCPP, 0, 4000, baseTime.Add(-30*time.Minute))
	v2 := mkInst("v2", "b", instances.EngineLlamaCPP, 0, 4000, baseTime.Add(-20*time.Minute))
	lister := staticLister{[]instances.Instance{v1, v2}}
	evictor := &recordingEvictor{freed: 4000}
	// Footprint 9000 + 1024 = 10024 need. Probe: 3000 → 7000 (after v1) → 11000 (after v2).
	probe := &countingProbe{vals: []int{3000, 7000, 11000}}
	log := &captureLogger{}
	m := managerWithLogger(t, log, probe, lister, evictor, fixedEstimator{mib: 9000})

	res, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionEvictThenLoad {
		t.Fatalf("decision = %s, want evict_then_load", res.Decision)
	}

	// Two eviction re-read lines, each carrying the RE-READ free value.
	if len(log.evictions) != 2 {
		t.Fatalf("expected 2 eviction re-read log lines, got %d: %+v", len(log.evictions), log.evictions)
	}
	if log.evictions[0].Victim != "v1" || log.evictions[0].FreeAfterReReadMiB != 7000 {
		t.Errorf("eviction[0] = %+v, want victim v1 free_after_reread 7000", log.evictions[0])
	}
	if log.evictions[1].Victim != "v2" || log.evictions[1].FreeAfterReReadMiB != 11000 {
		t.Errorf("eviction[1] = %+v, want victim v2 free_after_reread 11000", log.evictions[1])
	}

	// One admission-decision line carrying the numbers.
	if len(log.decisions) != 1 {
		t.Fatalf("expected 1 admission decision log line, got %d", len(log.decisions))
	}
	d := log.decisions[0]
	if d.Decision != "evict_then_load" || d.NeedMiB != 10024 || d.FreeAfterMiB != 11000 {
		t.Errorf("decision log = %+v, want decision evict_then_load need 10024 free_after 11000", d)
	}
	if len(d.Evictions) != 2 {
		t.Errorf("decision log should embed the 2 evictions, got %d", len(d.Evictions))
	}
}

// TestAdmit_Logs_LoadNewDecision: a direct fit logs exactly one decision line and zero eviction
// lines.
func TestAdmit_Logs_LoadNewDecision(t *testing.T) {
	log := &captureLogger{}
	m := managerWithLogger(t, log, &countingProbe{vals: []int{15000}}, staticLister{nil},
		&recordingEvictor{}, fixedEstimator{mib: 6000})
	if _, err := m.Admit(context.Background(), AdmitSpec{ModelID: "m1", Engine: "llamacpp", Class: ClassNormal}); err != nil {
		t.Fatal(err)
	}
	if len(log.decisions) != 1 || log.decisions[0].Decision != "load_new" {
		t.Fatalf("want one load_new decision line, got %+v", log.decisions)
	}
	if len(log.evictions) != 0 {
		t.Errorf("want zero eviction lines for a direct fit, got %d", len(log.evictions))
	}
}
