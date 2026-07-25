# Residency Priority Queue — Custom Ordering Algorithm (P5.0 design note)

Status: **Accepted** (authored P5.0, 2026-07-06). This is the design note the P5.0 gate
(TESTING.md#p50) requires: it specifies the custom queue-ordering algorithm *precisely*
— priority classes, the aging function, the size/cost tie-break, the coalescing rule and
its starvation guard, the per-class caps — with worked examples and the exact promotion
math. `internal/residency/queue.go` implements exactly this document; the property tests
in `queue_test.go` assert the invariants stated here.

## Why a custom algorithm (and why it does NOT violate ADR-001)

PLAN.md decision 7(a) requires a priority queue with a *documented custom algorithm*:
priority classes + aging to prevent starvation + size/cost awareness. This SUPERSEDES
residency-scheduler.md §"Queue semantics" ("no starvation guarantees in v1") per PLAN.md
spec-conflict resolution 2.

The queue orders **admission requests** — "which waiting model-load request should we try
to admit next." It is *not* a VRAM scheduler and does *not* preempt or fair-share GPU
memory across engines. Residency stays coarse: whole-model load/unload, free VRAM
re-measured live (ADR-001 untouched). The queue only decides *order of attempts*;
admission control (P5.1) decides whether an attempt succeeds, and eviction (P5.2) decides
what to unload. The queue never selects a victim and never reorders around pinned/active
instances — that is admission's and eviction's job. This separation is a tested invariant
(#p50: "ordering never triggers eviction of pinned/active").

Prior art ported: `lmwrapper/scheduler.py` (3 tiers, per-class caps {64,256,512}, bounded
same-profile coalescing with a starvation guard) — see research/lmwrapper-reuse-notes.md §2.

## Priority classes (lmwrapper-compatible)

Three classes, lower value = higher priority (heap-min on class):

| Class          | Value | Default source                                            |
|----------------|-------|-----------------------------------------------------------|
| `interactive`  | 0     | chat / generate / a human waiting on a model to come warm |
| `normal`       | 1     | default for an API `POST /api/instances` with no priority |
| `background`   | 2     | embeddings / batch / prefetch / warmups                   |

Values match `lmwrapper` exactly (interactive=0 < normal=1 < background=2) so a future
gateway (Phase 6) can map endpoint→class the same way. The `instances.priority` /
`queue.priority` INTEGER column stores the class value directly (0/1/2); higher-value =
lower priority, consistent with the existing "priority 0 = default" rows.

## Effective score (the ordering key)

A queued request `r` is ordered by the tuple, compared lexicographically, **smallest wins**
(is served/attempted first):

```
score(r, now) = ( effective_class(r, now),          -- 0/1/2 after aging promotion
                  tie_break(r),                       -- size/cost term (see below)
                  seq(r) )                            -- FIFO monotonic arrival sequence
```

- `effective_class` folds aging in (below) so a long-waiting request competes as if it
  were a higher class. This is the anti-starvation mechanism.
- `tie_break` is the **size/cost term**, applied ONLY within the same effective class
  (never across classes — a cheap background job never jumps an interactive one).
- `seq(r)` is a strictly monotonic arrival counter assigned at enqueue. It guarantees a
  total order (no ties) and gives **stable FIFO within a class** when the tie-break does
  not distinguish two requests. Using a monotonic sequence rather than a wall-clock
  timestamp makes ordering deterministic under a fake clock and immune to equal-timestamp
  ties (which a coarse `datetime('now')` second-resolution stamp would produce).

### Aging / anti-starvation — the exact promotion math

Each request records `enqueued_at` (fake-clock-driven in tests). Its **age** is
`now - enqueued_at`. Aging promotes a request one class boundary per configured wait
threshold, clamped at `interactive` (0):

```
promotions(r, now) = 0
if age(r) >= AgePromoteBackgroundSecs : promotions += 1   # background(2) -> normal(1)
if age(r) >= AgePromoteNormalSecs     : promotions += 1   # normal(1)     -> interactive(0)

effective_class(r, now) = max(0, base_class(r) - promotions(r, now))
```

Defaults (config `residency.queue`, all overridable):

| Constant                      | Default | Meaning                                                    |
|-------------------------------|---------|------------------------------------------------------------|
| `AgePromoteNormalSecs`        | 60      | a `normal` request waiting ≥60s competes as `interactive`  |
| `AgePromoteBackgroundSecs`    | 120     | a `background` request waiting ≥120s competes as `normal`  |

Note the thresholds are **cumulative on age**, not sequential-on-promotion: a `background`
request reaches `normal`-equivalent at 120s and would reach `interactive`-equivalent at
`max(AgePromoteNormalSecs, AgePromoteBackgroundSecs)` = at the point BOTH thresholds are
crossed. With the defaults (60 and 120), a background request crosses the 60s threshold
before 120s, so at age ≥120s it has crossed both and lands at effective class
`max(0, 2 - 2) = 0` (interactive-equivalent). This is intentional: a request that has
waited longer than *any* higher-class request could plausibly wait is promoted all the way
to the front. This is what guarantees **no starvation**: under continuous higher-priority
arrivals, any request's age grows without bound, so after at most
`max(AgePromoteNormalSecs, AgePromoteBackgroundSecs)` its effective class reaches 0 and it
ties the highest arriving class; among effective-class-0 requests, the older `seq` wins, so
it reaches the front in bounded time. (Proven by `TestQueue_NoStarvation`.)

### Size/cost awareness — the TIE-BREAK (within class only)

Cheap wins early, but never across a class boundary. Within one effective class, order by:

```
tie_break(r) = 0   if r is "cheap-now"    (served without eviction: model already warm,
                                            OR estimate fits in current free VRAM)
             = 1   otherwise               (would require eviction / does not currently fit)
```

Rationale (lmwrapper §3 measured-fit spirit + PLAN 7a "size/cost awareness"): when two
requests are in the same class, serving the one that is *already loadable or warm* first is
a strict win — it completes immediately and frees the queue slot, and it does not perturb
residency (no eviction). A request that needs an eviction is deferred slightly so a
free-riding cheap request in the same class goes first. Among requests with the SAME
tie_break value, `seq` (FIFO) breaks the tie — so the size/cost term never reorders two
equally-cheap or two equally-expensive same-class requests; it only floats a cheap one
ahead of an expensive one *in the same class*. This keeps FIFO stable and auditable.

The `cheap-now` bit is computed by the queue from a `CostHint` the caller supplies at
enqueue (warm? / estimate MiB) plus the current free-VRAM snapshot passed to `Next`. The
queue does NOT itself probe hardware or select victims — it only reads the hint. If no hint
is supplied, `tie_break = 1` (treat as expensive), so the term is a pure FIFO no-op and the
queue degrades to strict class + FIFO. This makes the size/cost term *optional and
conservative*: it can only ever move a demonstrably-cheap request forward.

## Bounded same-model coalescing (with the higher-class starvation guard)

Ported from `lmwrapper.scheduler.next_for_profile` / `drain_same_profile` (max 4). When a
model M is already resident (warm), it is far cheaper to serve several queued requests for M
back-to-back than to evict M, load another model, then reload M (A/B/A reload thrash). So:

> When `Next(residentModels)` is asked for the next request and the resident set is
> non-empty, the queue MAY prefer a queued request whose model is already resident — up to
> **N = `CoalesceMax` (default 4)** consecutive coalesced picks for the same resident model
> — UNLESS a **strictly-higher-effective-class** request waits.

The starvation guard is the load-bearing clause: coalescing may skip ahead to a
same-model request ONLY among requests of the **same-or-lower** effective class as the
current head. If any waiting request has a *strictly higher* effective class (numerically
smaller) than the coalescing candidate, coalescing is suppressed and the higher-class
request wins. This guarantees coalescing can never starve a higher-priority request
(#p50: "coalescing never violates the higher-class guard").

Coalescing is bounded two ways: (1) a per-model counter resets when the queue switches to a
different model or when a higher-class request forces a switch; after `CoalesceMax`
consecutive coalesced picks the counter is exhausted and the strict score order resumes
even for same-model requests (prevents an endless same-model stream from deferring an
equal-class different-model request forever). (2) The guard above bounds it by class.

Coalescing is an *opt-in optimization surfaced through `Next`*: the caller passes the
current resident model set and a coalescing-credit counter; the queue returns both the
chosen request and whether it was a coalesced pick so the caller can maintain the credit.
A caller that passes an empty resident set (or `CoalesceMax=0`) gets pure score ordering.

## Per-class caps → `queue_full` rejection (with a numeric reason)

Each class has a bounded WAITING depth; enqueue past the cap is rejected immediately (not
silently dropped), carrying a numeric human-readable reason:

| Class          | Cap (`QueueCaps`) |
|----------------|-------------------|
| `interactive`  | 64                |
| `normal`       | 256               |
| `background`   | 512               |

Rejection reason format (schema-tested to contain the numbers):
`"queue_full: interactive queue is at capacity (64/64 waiting); reject rather than
unbounded-buffer — retry after a slot frees"`. The rejected request never enters the queue.
Caps match lmwrapper {64,256,512}.

## What the queue is NOT allowed to do (tested invariants)

1. It never probes hardware, never calls an engine, never selects an eviction victim, never
   reads/writes the `instances` table. It is a pure in-memory ordering structure. Admission
   (P5.1) and eviction (P5.2) own all of that.
2. It never reorders *around* pinned/active instances — it has no concept of them. It orders
   *requests*, not instances. The property test `TestQueue_NeverSelectsInstanceEviction`
   asserts the queue's public surface exposes no instance/victim selection at all.
3. Ordering is a **total order** (via `seq`), so `Next` is deterministic given the same
   clock and inputs — no map-iteration nondeterminism. `TestQueue_StableFIFOWithinClass`
   asserts equal-class, equal-cost requests come out strictly in arrival order.

## Worked examples

All examples use the default constants (60/120s aging, caps 64/256/512, CoalesceMax=4) and
a fake clock. `t=0` is when the first request enqueues.

### Example 1 — strict class ordering, FIFO within class

Enqueue at `t=0`: `A`(normal, seq 1), `B`(interactive, seq 2), `C`(normal, seq 3),
`D`(background, seq 4). No hints (all `tie_break=1`), no residency.

`Next` order at `t=1s`:
- `B` — effective class 0 (interactive), lowest class wins.
- `A` — class 1, seq 1 (FIFO ahead of C).
- `C` — class 1, seq 3.
- `D` — class 2 (background), last.

Result: `B, A, C, D`. Class dominates; `seq` breaks the within-class tie (A before C).

### Example 2 — aging promotion prevents starvation

Enqueue `X`(background, seq 1) at `t=0`. Then a **continuous stream** of `interactive`
requests arrives, one every 5s (`seq 2, 3, 4, …`). Without aging, `X` (class 2) never
reaches the front while interactive requests keep coming.

Aging math for `X`:
- `t < 60s`:  age<60 → promotions 0 → effective class 2. `X` sits behind every interactive.
- `60s ≤ t < 120s`: age≥60 → crosses `AgePromoteBackgroundSecs`? No — 60s crosses the
  *normal* threshold value but X is background; promotions counts BOTH thresholds by age.
  At age≥60s, `age >= AgePromoteNormalSecs(60)` is true (+1) and
  `age >= AgePromoteBackgroundSecs(120)` is false → promotions = 1 → effective class
  `max(0, 2-1) = 1` (normal-equivalent). Still behind arriving interactive requests.
- `t ≥ 120s`: both thresholds crossed → promotions = 2 → effective class `max(0, 2-2) = 0`
  (interactive-equivalent). Now `X` competes AT the interactive class, and among
  effective-class-0 requests it has the **smallest seq** (1) — so at the next `Next` call at
  `t≥120s`, `X` is returned ahead of every later-arriving interactive request.

Bounded wait: `X` reaches the front within `AgePromoteBackgroundSecs = 120s` regardless of
arrival pressure. `TestQueue_NoStarvation` drives exactly this with a fake clock and asserts
`X` is eventually returned by `Next`.

### Example 3 — size/cost tie-break within class

Resident set (warm): `{llama-1b}`. Free VRAM: 3000 MiB. Enqueue two `normal` requests at
`t=0`: `P`(model `qwen-7b`, estimate 6200 MiB, seq 1) and `Q`(model `llama-1b`, warm,
seq 2). Both class 1.

- `Q` is **cheap-now**: its model is warm (route_loaded, zero VRAM cost) → `tie_break=0`.
- `P` needs 6200 MiB but only 3000 free → would require eviction → `tie_break=1`.

`Next` returns `Q` first (same class, `tie_break` 0 < 1) even though `P` arrived… also
before? No — here P has the smaller seq but the LARGER tie_break, and tie_break outranks
seq. So `Q` (0, seq 2) beats `P` (1, seq 1). The cheap warm request goes first; `P` follows.
This is the intended "cheap wins early" behavior — and it never crosses a class boundary
(a cheap *background* request would still lose to any *normal* request).

### Example 4 — coalescing with the higher-class guard

Resident: `{gemma-2b}`. Queue (all enqueued `t=0`): `R1`(gemma-2b, normal, seq 1),
`R2`(gemma-2b, normal, seq 2), `R3`(gemma-2b, normal, seq 3), `R4`(gemma-2b, normal,
seq 4), `R5`(gemma-2b, normal, seq 5), `H`(other-model, **interactive**, seq 6).

- With coalescing and NO higher-class waiter, the queue would drain up to 4 gemma-2b
  requests (`R1..R4`) before switching, avoiding reload thrash.
- BUT `H` is **interactive** (effective class 0) — strictly higher than the normal
  gemma-2b requests (class 1). The starvation guard SUPPRESSES coalescing: `Next` returns
  `H` first, then resumes with the gemma-2b coalescing run. Coalescing may never defer a
  strictly-higher-class request. `TestQueue_CoalesceRespectsHigherClass` asserts `H`
  precedes the coalesced gemma-2b run.
- After `H`, the queue coalesces `R1..R4` (4 = CoalesceMax), then the credit is exhausted;
  `R5` is returned by strict score order on the next switch. If a different-model *normal*
  request `S`(seq 7) were also waiting, it would be returned before `R5` only if the credit
  were exhausted — the bound stops gemma-2b from monopolizing indefinitely.

## Cross-references

- Admission control that consumes `Next`: `docs/residency-scheduler.md` §admit + P5.1
  (`internal/residency/admission.go`). The queue is drained by admission on instance
  stop/eviction, TTL sweep, and periodic tick.
- Eviction ordering: P5.2 (`internal/residency/eviction.go`).
- Schema: additive `queue` columns (priority class, `enqueued_at` already exists, coalescing
  bookkeeping) in migration `0005_residency_queue.sql`.
- Margins: admission adds additive `SAFETY_MARGIN_MIB = 1024` on top of estimates (PLAN
  resolution 9); the queue's `cheap-now` bit uses the same admission fit test so the
  tie-break agrees with what admission will actually decide.
