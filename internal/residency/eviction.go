package residency

import (
	"fmt"
	"sort"
	"time"

	"github.com/localhub/localhub/internal/instances"
)

// eviction.go is P5.2: the eviction policy — victim ordering + eligibility + DRAINING
// semantics. It is a PURE decision function over a candidate instance list; it does not stop
// anything itself (admission executes the stop through the Phase-4 adapters, one at a time,
// re-reading VRAM after each). Keeping the policy pure makes the ordering invariants
// property-testable without a real GPU (TESTING.md#p52).
//
// Victim order (residency-scheduler.md §pick_victim, spec-digest §6):
//   1. Expired TTL   — idle longer than its ttl_seconds.
//   2. Idle + lowest priority — no active requests, lowest priority (highest class value),
//      then LRU (oldest last_used_at).
//   3. Largest reclaimable — only if still short after 1–2, prefer freeing the most VRAM.
//
// NEVER selected (the never-kill-active / never-touch-pinned gates):
//   - pinned = 1.
//   - any instance with active in-flight requests (marked DRAINING or with a live active
//     counter > 0) — never killed by eviction, ever.
//   - a DRAINING instance (already stopping / draining active streams).
//   - the instance we are making room for (excluded by model_id at the call site).
//   - any non-HEALTHY instance (REQUESTED/STARTING are not reclaimable idle capacity;
//     STOPPED/FAILED hold no VRAM).

// ActiveTracker reports whether an instance currently has in-flight requests. Phase 4 has no
// proxy-level stream tracking yet (that lands in Phase 6); this seam lets admission supply the
// best available signal today:
//   - llama.cpp / vLLM: an atomic in-flight counter incremented per instance-API-initiated
//     completion (IncrementActive/DecrementActive on the manager); zero = idle.
//   - Ollama: /api/ps is not stream-aware, so "recently active" (last request < RecentActiveTTL,
//     default 30s) is treated as active — a documented conservative approximation that errs
//     toward NOT evicting a possibly-busy model.
//
// A DRAINING state is always treated as active regardless of the counter.
type ActiveTracker interface {
	// ActiveCount returns the number of in-flight requests attributed to instanceID (0 = idle).
	ActiveCount(instanceID string) int
}

// EvictionPolicy holds the tunables for victim selection.
type EvictionPolicy struct {
	// Now is the reference time for TTL-expiry and LRU comparisons (fake clock in tests).
	Now time.Time
	// RecentActiveTTL is how long after an instance's last_used_at an Ollama instance is still
	// treated as "recently active" (approximation; default 30s). llama.cpp/vLLM use the exact
	// counter and ignore this.
	RecentActiveTTL time.Duration
}

// DefaultRecentActiveTTLSecs is the Ollama recently-active approximation window (spec §5.1
// honesty limit: /api/ps is not stream-aware).
const DefaultRecentActiveTTLSecs = 30

// Victim is one eviction candidate with its computed reclaimable footprint and eligibility.
type Victim struct {
	Instance   instances.Instance
	ReclaimMiB int    // footprint we expect to reclaim (observed peak preferred, else estimate)
	Eligible   bool   // passed all never-evict gates
	Ineligible string // reason it is NOT a candidate (when Eligible=false), for logging
	TTLExpired bool   // idle past ttl_seconds
	Active     bool   // has in-flight requests (never-kill gate)
}

// footprintMiB returns the VRAM we expect to reclaim by stopping inst: observed peak if known,
// else the admission estimate. 0 when neither is known (still evictable, but we cannot predict
// the reclaim — admission re-reads the probe to learn the truth).
func footprintMiB(inst instances.Instance) int {
	if inst.VRAMObsMiB > 0 {
		return inst.VRAMObsMiB
	}
	return inst.VRAMEstMiB
}

// isActive reports whether inst counts as having in-flight requests (never-kill gate). DRAINING
// is always active. Otherwise the ActiveTracker counter is authoritative for llama.cpp/vLLM; for
// Ollama a recent last_used_at within RecentActiveTTL is treated as active (approximation).
func isActive(inst instances.Instance, tracker ActiveTracker, now time.Time, recentTTL time.Duration) bool {
	if inst.State == instances.StateDraining {
		return true
	}
	if tracker != nil && tracker.ActiveCount(inst.ID) > 0 {
		return true
	}
	if inst.Engine == instances.EngineOllama && recentTTL > 0 && !inst.LastUsedAt.IsZero() {
		if now.Sub(inst.LastUsedAt) < recentTTL {
			return true
		}
	}
	return false
}

// ttlExpired reports whether inst is idle past its ttl_seconds. An instance with no TTL
// (ttl_seconds <= 0) never expires by TTL. Idle time is measured from last_used_at, falling back
// to started_at when the instance has never served a request.
func ttlExpired(inst instances.Instance, now time.Time) bool {
	if inst.TTLSeconds <= 0 {
		return false
	}
	idleSince := inst.LastUsedAt
	if idleSince.IsZero() {
		idleSince = inst.StartedAt
	}
	if idleSince.IsZero() {
		return false
	}
	return now.Sub(idleSince) >= time.Duration(inst.TTLSeconds)*time.Second
}

// classifyVictim evaluates one instance against the never-evict gates and computes its
// footprint + TTL/active flags. It returns a Victim with Eligible set. Called for every candidate
// so the decision log can show WHY each ineligible instance was skipped (auditable eviction).
func classifyVictim(inst instances.Instance, tracker ActiveTracker, pol EvictionPolicy) Victim {
	v := Victim{Instance: inst, ReclaimMiB: footprintMiB(inst)}
	v.Active = isActive(inst, tracker, pol.Now, pol.RecentActiveTTL)
	v.TTLExpired = ttlExpired(inst, pol.Now)

	switch {
	case inst.Adopted:
		// An ADOPTED instance (migration 0012) was started OUTSIDE LocalHub — typically by a launcher
		// script with its own lifecycle. LocalHub did not create it and cannot know what depends on it,
		// so reclaiming its VRAM is not LocalHub's call. Checked BEFORE Pinned so the reason stays
		// specific even when a caller explicitly un-pinned it.
		v.Ineligible = "adopted from an externally-started process (LocalHub did not spawn it; never auto-evict)"
	case inst.Pinned:
		v.Ineligible = "pinned (never auto-evict)"
	case inst.State != instances.StateHealthy:
		// Only HEALTHY idle instances are reclaimable capacity. DRAINING is already stopping;
		// REQUESTED/STARTING hold a slot but are not idle capacity to reclaim; STOPPED/FAILED
		// hold no VRAM.
		v.Ineligible = fmt.Sprintf("state %s is not HEALTHY-idle (not reclaimable)", inst.State)
	case v.Active:
		v.Ineligible = "has active in-flight requests (never kill an active stream)"
	default:
		v.Eligible = true
	}
	return v
}

// PickVictims returns the eligible eviction candidates in the exact victim order (TTL-expired
// first, then idle+lowest-priority+LRU, then largest reclaimable), plus the full classified list
// (including ineligible ones with reasons) for the decision log.
//
// excludeModelID is the model we are making room FOR — any instance serving it is excluded (we
// never evict the thing we are trying to load, obviously). Pass "" to exclude nothing.
//
// This function does NOT stop anything and does NOT read hardware. Admission consumes the ordered
// eligible list and executes stops ONE AT A TIME, re-reading free VRAM after each (the anti-OOM
// rule), re-checking sufficiency before the next stop and before the final start.
func PickVictims(candidates []instances.Instance, tracker ActiveTracker, pol EvictionPolicy, excludeModelID string) (ordered []Victim, all []Victim) {
	if pol.RecentActiveTTL <= 0 {
		pol.RecentActiveTTL = DefaultRecentActiveTTLSecs * time.Second
	}
	if pol.Now.IsZero() {
		pol.Now = time.Now()
	}

	all = make([]Victim, 0, len(candidates))
	eligible := make([]Victim, 0, len(candidates))
	for _, inst := range candidates {
		if excludeModelID != "" && inst.ModelID == excludeModelID {
			v := Victim{Instance: inst, ReclaimMiB: footprintMiB(inst),
				Ineligible: "is the model being loaded (never evict the target)"}
			all = append(all, v)
			continue
		}
		v := classifyVictim(inst, tracker, pol)
		all = append(all, v)
		if v.Eligible {
			eligible = append(eligible, v)
		}
	}

	// Order the eligible candidates by the victim priority:
	//  1. TTL-expired before non-expired.
	//  2. Lowest priority first (higher priority INTEGER = keep longer, so evict the LOWEST;
	//     note class value ↑ = keep-longer here per the DDL comment "higher = keep longer").
	//  3. LRU: oldest last_used_at first.
	//  4. Largest reclaimable first (frees the most VRAM when still short).
	//  5. Stable id tie-break for determinism.
	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		// 1. TTL-expired first.
		if a.TTLExpired != b.TTLExpired {
			return a.TTLExpired // true sorts before false
		}
		// 2. Lowest priority (keep-longer value ascending → evict lowest keep-value first).
		if a.Instance.Priority != b.Instance.Priority {
			return a.Instance.Priority < b.Instance.Priority
		}
		// 3. LRU: oldest last_used_at first. Zero (never used) is oldest.
		au, bu := lruKey(a.Instance), lruKey(b.Instance)
		if !au.Equal(bu) {
			return au.Before(bu)
		}
		// 4. Largest reclaimable first.
		if a.ReclaimMiB != b.ReclaimMiB {
			return a.ReclaimMiB > b.ReclaimMiB
		}
		// 5. Deterministic id tie-break.
		return a.Instance.ID < b.Instance.ID
	})

	return eligible, all
}

// lruKey returns the timestamp used for LRU ordering: last_used_at, falling back to started_at,
// falling back to created_at. A zero time (unknown) sorts oldest (evicted first among ties).
func lruKey(inst instances.Instance) time.Time {
	if !inst.LastUsedAt.IsZero() {
		return inst.LastUsedAt
	}
	if !inst.StartedAt.IsZero() {
		return inst.StartedAt
	}
	return inst.CreatedAt
}

// SimulateReclaim returns how much VRAM the ordered victims would reclaim in total, and the
// prefix of victims needed to reach needMiB. It is the admission "would eviction free enough?"
// pre-check (spec §6 admit loop): admission only proceeds to evict_then_load if this says the
// eligible idle set can cover the deficit. It is a PREDICTION from footprints; admission still
// re-reads the probe after each real stop (the prediction can be wrong — another process may
// grab VRAM — and the re-read is what catches it: the anti-OOM property).
func SimulateReclaim(ordered []Victim, needMiB int) (enough bool, prefix []Victim, totalMiB int) {
	for _, v := range ordered {
		prefix = append(prefix, v)
		totalMiB += v.ReclaimMiB
		if totalMiB >= needMiB {
			return true, prefix, totalMiB
		}
	}
	return totalMiB >= needMiB, prefix, totalMiB
}
