package residency

import (
	"testing"
	"time"

	"github.com/localhub/localhub/internal/instances"
)

// fakeTracker is a static in-flight tracker for eviction tests.
type fakeTracker map[string]int

func (f fakeTracker) ActiveCount(id string) int { return f[id] }

// baseTime is a fixed reference for the eviction tests.
var baseTime = time.Unix(1_700_000_000, 0).UTC()

// mkInst builds a HEALTHY instance with the given knobs.
func mkInst(id, model string, engine instances.Engine, prio, estMiB int, lastUsed time.Time) instances.Instance {
	return instances.Instance{
		ID: id, ModelID: model, Engine: engine, State: instances.StateHealthy,
		Priority: prio, VRAMEstMiB: estMiB, LastUsedAt: lastUsed, StartedAt: lastUsed,
	}
}

// TestPickVictims_PinnedNeverSelected: a pinned instance is never eligible, no matter how idle.
func TestPickVictims_PinnedNeverSelected(t *testing.T) {
	pinned := mkInst("pin", "m1", instances.EngineLlamaCPP, 0, 5000, baseTime.Add(-time.Hour))
	pinned.Pinned = true
	idle := mkInst("idle", "m2", instances.EngineLlamaCPP, 0, 4000, baseTime.Add(-time.Minute))

	ordered, all := PickVictims([]instances.Instance{pinned, idle}, fakeTracker{}, EvictionPolicy{Now: baseTime}, "")
	for _, v := range ordered {
		if v.Instance.ID == "pin" {
			t.Fatal("pinned instance was selected as a victim")
		}
	}
	// The pinned one must appear in `all` with a pinned ineligibility reason (auditable).
	found := false
	for _, v := range all {
		if v.Instance.ID == "pin" {
			found = true
			if v.Eligible {
				t.Fatal("pinned instance marked eligible")
			}
			if !contains(v.Ineligible, "pinned") {
				t.Errorf("pinned ineligibility reason = %q, want it to mention 'pinned'", v.Ineligible)
			}
		}
	}
	if !found {
		t.Fatal("pinned instance missing from the classified list")
	}
}

// TestPickVictims_ActiveNeverSelected: an instance with an active in-flight count, and a DRAINING
// instance, are never eligible (never kill an active stream).
func TestPickVictims_ActiveNeverSelected(t *testing.T) {
	busy := mkInst("busy", "m1", instances.EngineLlamaCPP, 0, 5000, baseTime.Add(-time.Hour))
	draining := mkInst("drain", "m2", instances.EngineLlamaCPP, 0, 5000, baseTime.Add(-time.Hour))
	draining.State = instances.StateDraining
	idle := mkInst("idle", "m3", instances.EngineLlamaCPP, 0, 3000, baseTime.Add(-time.Minute))

	tracker := fakeTracker{"busy": 2}
	ordered, _ := PickVictims([]instances.Instance{busy, draining, idle}, tracker, EvictionPolicy{Now: baseTime}, "")
	if len(ordered) != 1 || ordered[0].Instance.ID != "idle" {
		t.Fatalf("expected only 'idle' eligible, got %+v", victimIDs(ordered))
	}
}

// TestPickVictims_TTLExpiredFirst: an expired-TTL instance is ordered ahead of a non-expired one
// even when the non-expired one is lower priority / larger.
func TestPickVictims_TTLExpiredFirst(t *testing.T) {
	// expired: high priority, small — but TTL-expired → must come first.
	expired := mkInst("exp", "m1", instances.EngineLlamaCPP, 5, 1000, baseTime.Add(-time.Hour))
	expired.TTLSeconds = 60 // idle 1h >> 60s → expired
	// fresh: low priority, large, not expired.
	fresh := mkInst("fresh", "m2", instances.EngineLlamaCPP, 0, 9000, baseTime.Add(-time.Second))
	fresh.TTLSeconds = 0 // no TTL

	ordered, _ := PickVictims([]instances.Instance{fresh, expired}, fakeTracker{}, EvictionPolicy{Now: baseTime}, "")
	if len(ordered) != 2 {
		t.Fatalf("expected 2 eligible, got %d", len(ordered))
	}
	if ordered[0].Instance.ID != "exp" {
		t.Fatalf("TTL-expired must be first, got %v", victimIDs(ordered))
	}
}

// TestPickVictims_IdleLowestPriorityLRU: among non-expired idle instances, lowest priority first,
// then LRU (oldest last_used_at).
func TestPickVictims_IdleLowestPriorityLRU(t *testing.T) {
	highPrio := mkInst("high", "m1", instances.EngineLlamaCPP, 10, 4000, baseTime.Add(-10*time.Minute))
	lowNew := mkInst("lowNew", "m2", instances.EngineLlamaCPP, 1, 4000, baseTime.Add(-1*time.Minute))
	lowOld := mkInst("lowOld", "m3", instances.EngineLlamaCPP, 1, 4000, baseTime.Add(-30*time.Minute))

	ordered, _ := PickVictims([]instances.Instance{highPrio, lowNew, lowOld}, fakeTracker{}, EvictionPolicy{Now: baseTime}, "")
	got := victimIDs(ordered)
	// lowest priority first (lowOld, lowNew both prio 1), LRU within → lowOld (older) before lowNew,
	// then highPrio last.
	want := []string{"lowOld", "lowNew", "high"}
	if !equalStrs(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestPickVictims_LargestReclaimableTieBreak: with equal priority + equal LRU, the larger
// reclaimable footprint is evicted first (frees the most VRAM).
func TestPickVictims_LargestReclaimableTieBreak(t *testing.T) {
	small := mkInst("small", "m1", instances.EngineLlamaCPP, 0, 2000, baseTime.Add(-5*time.Minute))
	large := mkInst("large", "m2", instances.EngineLlamaCPP, 0, 8000, baseTime.Add(-5*time.Minute))
	ordered, _ := PickVictims([]instances.Instance{small, large}, fakeTracker{}, EvictionPolicy{Now: baseTime}, "")
	if victimIDs(ordered)[0] != "large" {
		t.Fatalf("largest reclaimable should be first on tie, got %v", victimIDs(ordered))
	}
}

// TestPickVictims_ExcludeTarget: the model being loaded is never a victim.
func TestPickVictims_ExcludeTarget(t *testing.T) {
	target := mkInst("t", "loadme", instances.EngineLlamaCPP, 0, 4000, baseTime.Add(-time.Hour))
	other := mkInst("o", "other", instances.EngineLlamaCPP, 0, 4000, baseTime.Add(-time.Hour))
	ordered, _ := PickVictims([]instances.Instance{target, other}, fakeTracker{}, EvictionPolicy{Now: baseTime}, "loadme")
	for _, v := range ordered {
		if v.Instance.ModelID == "loadme" {
			t.Fatal("the model being loaded was selected as a victim")
		}
	}
}

// TestPickVictims_OllamaRecentActiveApprox: an Ollama instance used within RecentActiveTTL is
// treated as active (approximation), a llama.cpp one is not (uses the counter only).
func TestPickVictims_OllamaRecentActiveApprox(t *testing.T) {
	ollamaRecent := mkInst("olla", "m1", instances.EngineOllama, 0, 4000, baseTime.Add(-10*time.Second))
	llamaRecent := mkInst("lla", "m2", instances.EngineLlamaCPP, 0, 4000, baseTime.Add(-10*time.Second))
	pol := EvictionPolicy{Now: baseTime, RecentActiveTTL: 30 * time.Second}
	ordered, _ := PickVictims([]instances.Instance{ollamaRecent, llamaRecent}, fakeTracker{}, pol, "")
	got := victimIDs(ordered)
	// The Ollama one is treated active (excluded); only the llama.cpp one is eligible.
	if len(got) != 1 || got[0] != "lla" {
		t.Fatalf("expected only llama.cpp 'lla' eligible (Ollama recent = active), got %v", got)
	}
}

// TestSimulateReclaim: the prefix needed to reach the deficit, and the enough flag.
func TestSimulateReclaim(t *testing.T) {
	ordered := []Victim{
		{Instance: instances.Instance{ID: "a"}, ReclaimMiB: 2000},
		{Instance: instances.Instance{ID: "b"}, ReclaimMiB: 3000},
		{Instance: instances.Instance{ID: "c"}, ReclaimMiB: 5000},
	}
	enough, prefix, total := SimulateReclaim(ordered, 4000)
	if !enough || len(prefix) != 2 || total != 5000 {
		t.Fatalf("SimulateReclaim(4000) = enough %v, prefix %v, total %d; want true, [a b], 5000",
			enough, victimIDs(prefix), total)
	}
	enough2, _, total2 := SimulateReclaim(ordered, 99999)
	if enough2 || total2 != 10000 {
		t.Fatalf("SimulateReclaim(99999) = enough %v total %d; want false, 10000", enough2, total2)
	}
}

func victimIDs(vs []Victim) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Instance.ID
	}
	return out
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
