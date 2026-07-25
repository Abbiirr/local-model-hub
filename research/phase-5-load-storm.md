# Phase 5 Load-Storm (P5.3) — Real-Hardware Evidence

Date: 2026-07-06, ~21:50–22:00 (+06:00). Box: RTX 4060 Ti 16 GB, driver 576.40,
Windows 10, 16 GB system RAM. Engine: llama.cpp only (CUDA fork
`K:\projects\llm_models\llama.cpp-tq3\build\bin\llama-server.exe`, `-ngl all`).
Daemon: fresh scratch DB, residency enabled, `safety_margin_mb=1024`,
`default_ttl_seconds=45`, `never_kill_active_by_default=true`, log level debug.

**Outcome: PARTIAL PASS — storm ABORTED early on a binding safeguard (system RAM
floor), after the core anti-OOM behaviors were observed. No OOM, no crash, no killed
stream, pinned instance never evicted, correct queue/refusal reasons. Two gate criteria
(live eviction sequence, stream overlapping eviction pressure) were NOT exercised in this
run and remain unproven-live — see "Not exercised" and the honest verdict table.**

Config/artifacts (scratchpad `.../storm/`): `config.yaml`, `daemon.err` (full daemon log,
29 lines), `stream_A.sse` (300-chunk stream), `stream_meta.txt`, `queue_at_abort.json`,
`instances_at_abort.txt`.

---

## Storm composition (as fired)

| Actor | model_id | ctx | class | est @ctx | role |
|---|---|---|---|---|---|
| A | gguf-llama-3-2-1b-instruct-…-q4-k-m | 4096 | interactive(0) **PINNED** | ~1411 | long stream, must survive |
| B | gguf-glm-ocr-glm-ocr-q8-0 | 4096 | normal(1) | ~1331 | normal start |
| C | gguf-qwen3-vl-embedding-8b-…-q6-k | 4096 | normal(1) | ~7123 | normal start |
| D | gguf-paddleocr-vl-1-5 | 4096 | background(2) | ~1178 | background start |
| E | gguf-qwen3-vl-32b-instruct-q2k-…-q2-k | 4096 | normal(1) | ~13179 | **never-fits** |
| dupC | (= C) qwen3-vl-embedding-8b | 8192 | background(2) | 7707 | duplicate to force pressure |

Pre-flight compat verdicts (GET /api/models/{id}/compat) all carried numeric reasons;
E was `wont_run`: *"needs ~14211 MiB at 8K ctx but only 12355 MiB usable … does not fit
even at the smallest useful context (2K)."*

---

## Timeline (client clock, +06:00)

| Time | Event | Result |
|---|---|---|
| 21:50:10 | daemon start, DB WAL ready, 15 models inventoried | INFO, no errors |
| 21:52:05 | **VRAM baseline** | 2681 used / **13429 free** |
| 21:52:18–23 | POST A (pin, interactive, ctx4096) | **201** HEALTHY, pid 20184, port 40130, `-ngl all`; VRAM→3966 used (A ≈ +1285) |
| 21:52:51–53 | **A long stream** (300 tok, /v1/chat/completions on A base_url) | completed intact in ~2 s (see stream integrity) |
| 21:53:08.3 | **STORM FIRED** (B,C,D,E,dupC in parallel) | VRAM at fire: 3968 used / 12142 free |
| 21:53:27 | dupC | **202 queued**, position 1, `blocked_reason` 111 MiB short |
| 21:53:27 | E | **409 REFUSED** `start_refused`, wont_run reason (numeric) |
| 21:53:31 | B | **201** HEALTHY, pid 1664, port 40131 |
| 21:54:20 | D | **201** HEALTHY, pid 10456, port 40132 |
| 21:54:33 | C | **201** HEALTHY, pid 24000, port 40133 |
| ~21:54–55 | RAM monitor fires **RAM-ABORT** repeatedly (free RAM 0.24–0.54 GB, << 2 GB floor) | **SAFEGUARD TRIP → abort** |
| 21:55:08 | abort-state snapshot | VRAM 15024 used / **1086 free**; RAM 0.83 GB |
| 21:55:30→21:57:55 | teardown: cancel dupC, stop A,C,D,B one-by-one | processes killed, VRAM reclaimed |
| 21:58:41 | post-teardown | **VRAM 2801/13309**; RAM 3.11 GB; 0 llama-server procs |
| 22:00:41 | daemon stopped, final | **VRAM 2775 used / 13335 free** (Δ +94 MiB vs baseline); RAM 4.33 GB |

Admission serialized under the residency lock; each spawn health-waited 15–85 s, so the 5
storm requests (fired within the same second client-side) completed over ~85 s.

---

## Per-request outcomes (verbatim reasons)

- **A** — 201. `reason: "llama-server pid 20184 answered /health OK on
  http://127.0.0.1:40130"`; `pinned:true`. Flags: `-ngl all -c 4096 --fit off -fa on`.
- **B** — 201, HEALTHY pid 1664 (`--mmproj` GLM-OCR).
- **C** — 201, HEALTHY pid 24000, `vram_est_mib:7123`.
- **D** — 201, HEALTHY pid 10456 (`--mmproj` PaddleOCR).
- **E** — **409** `start_refused`: `"REFUSED: compatibility verdict is \"wont_run\" …
  needs ~13179 MiB at 4K ctx but only 11171 MiB usable (16380 total × 0.92 safety −
  4238 in use); it does not fit even at the smallest useful context (2K). Use a smaller
  quant, free VRAM, or a bigger card."` — numeric never-fits reason. E was refused at the
  **verdict** gate (pre-admission), so it never entered the queue.
- **dupC** — **202 queued**, position 1: `"waiting for VRAM: need 8731 MiB (footprint
  7707 + 1024 margin), free 8620 MiB, reclaimable 0 MiB from 0 idle instance(s) — 111 MiB
  short"`. At abort (age 102 s) the same accurate blocked_reason was still shown by
  GET /api/queue.

Every user-facing verdict/refusal/block carried a numeric human-readable reason — the
mandatory-reason gate held.

---

## A stream integrity (survival proof)

`stream_A.sse`: **300 content-bearing delta chunks** (= requested max_tokens=300), single
`finish_reason:"length"`, one `data: [DONE]` terminator, 1573 chars / ~248 words of
coherent text, no mid-stream disconnect or truncation. A stayed **HEALTHY and pinned=true**
through the entire storm and teardown; at abort-state it still showed `pin=True`.

**Honest caveat:** the 1B model generated 300 tokens in ~2 s and the stream FINISHED at
21:52:53, ~15 s BEFORE the storm was fired (21:53:08). So the stream did not temporally
overlap admission/eviction pressure. A's protection was validated (pinned, never a victim),
but "an active in-flight stream survives concurrent eviction" was NOT exercised as a live
race. To force overlap, a future run must use a slower/larger model as A or a much larger
`max_tokens`, and fire the storm mid-stream (verified by polling A's `/health` /
completion progress before firing).

---

## Eviction sequence

**Zero evictions occurred.** A+B+C+D (~1285+~1300+~7100+~1200 ≈ ~11 GB observed) fit inside
the ~12 GB usable envelope, so all four were admitted directly (LoadNew). dupC, the 5th
model, needed 8731 MiB with only 8620 free and **0 idle victims** (A pinned; B/C/D not idle
capacity here / no reclaim path chosen) → correctly **queued** 111 MiB short rather than
evicting anything. E was refused pre-admission. Because pressure resolved via *queueing*,
the `evict_then_load` loop and its **per-eviction VRAM re-read** were **not triggered live**
in this run. The mechanism remains proven by the P5.1/P5.2 unit/property tests
(`go test ./internal/residency/...` green) but was not demonstrated on real hardware here.

---

## Failure-criteria audit (all PASS / none tripped)

| Failure criterion | Result | Evidence |
|---|---|---|
| Driver OOM / CUDA alloc failure | **NONE** | `grep -iE "panic\|OOM\|cuda error\|failed to allocate\|fatal"` daemon.err → empty; no llama-server crash |
| A's stream killed/truncated | **NO** | 300/300 chunks, finish_reason length, [DONE] |
| Pinned/active instance evicted | **NO** | A pinned=true throughout; 0 evictions total |
| Blocked entry with empty/vague reason | **NO** | dupC + E reasons carry exact MiB numbers |
| Daemon crash/panic | **NO** | daemon.err clean; served requests to the end |
| Dangling process / VRAM leak at end | **NO** | 0 llama-server procs; VRAM Δ +94 MiB (< 200 MiB tol) |

---

## Not exercised / gaps found (honest)

1. **RAM was the binding limit, not VRAM.** With 4 llama-server processes resident, system
   RAM fell to 0.24–0.54 GB free (16 GB box, much already consumed by Docker Desktop,
   Claude, etc.), tripping the 2 GB PLAN safeguard. The storm was aborted per policy. VRAM
   never OOMed (min free 1086 MiB). **This is the real single-box ceiling for 4 concurrent
   llama-server processes on this machine** and should inform residency defaults / a RAM-aware
   admission term (currently admission budgets VRAM only).

2. **No production queue-drain loop (pre-existing Phase-5 gap).** `queue.Next()` is never
   called outside tests; `StopInstance`/eviction free VRAM but never re-attempt a queued
   start, and there is no periodic/TTL drain tick. So queued dupC would **never** have
   auto-started when room freed — a human/API re-POST is required. Storm step 6 ("queued C/D
   eventually start") is therefore **unimplemented**, not merely untested. Cleanest fix:
   a `core.Service.DrainQueue` that pops `Manager.Queue().Next(residencyNextInput)` and
   re-runs the existing `StartInstance` path, triggered after each stop/eviction and on a
   TTL tick. This is a reviewed feature change (the queue `Request` also loses ctx/batch/pin
   on enqueue and must carry them), out of scope for a storm-test touch-up.

3. **Admission decisions are not logged to the daemon.** `internal/core` and
   `internal/residency` do no logging; `decision.Reason` / per-eviction `FreeAfterMiB` are
   only returned in the struct (surfaced to the API), never emitted to stderr. Adding the
   "log the re-read value" line requires threading a `*slog.Logger` into the pure core/
   residency packages — not a one-liner. In this run I sourced VRAM re-read evidence from
   **nvidia-smi probe deltas** (ground truth) instead, which is strictly stronger than a
   self-reported log for an anti-OOM gate.

4. **DB state-lag on client-disconnected stop.** B/C/D DELETEs exceeded my 30 s client
   timeout (the daemon's stop+VRAM-verify poll is slow); the client disconnect left those
   three instances stuck in `DRAINING` in the DB even though the processes were killed and
   VRAM reclaimed. Not a resource leak (verified 0 procs, VRAM at baseline), but a
   reconciliation gap: a stop whose HTTP caller disconnects mid-drain never writes the final
   STOPPED row. Daemon-restart reconciliation would reap it. Worth a fix: make stop
   idempotent/resumable and reconcile DRAINING-with-dead-pid → STOPPED on a tick.

5. **`vram_obs_mib` in the abort snapshot looked like cumulative total-used** (C showed
   11059) rather than a per-model footprint — likely the observed-peak sampler recording the
   whole-GPU used figure under concurrency rather than a delta. Flag for P2.5/calibration
   review; it does not affect admission (which used estimates here).

---

## Cleanup (mandatory, done)

Queue cancelled; all 4 instances stopped via API (A verified: *"free VRAM rose by 1287 MiB
… process gone and VRAM reclaimed (NVML delta, not an HTTP-200 claim)"*); daemon stopped.
Final: 0 llama-server processes, ollama `/api/ps` empty, **VRAM 2775/13335 (Δ +94 MiB,
within 200 MiB tolerance)**, RAM 4.33 GB. Ollama shared daemon left running (never
touched — correct).

## Phase 6 notes (in-flight tracking)

`residency.InFlightTracker` exists and is wired into the Manager but has **zero
Increment/Decrement callers** in production — nothing counts in-flight requests today. A
llama.cpp stream sent directly to an instance `base_url` (as A's was) is invisible to the
tracker, so a non-pinned streaming instance would be eviction-eligible on the active gate
(it is protected only if pinned or within Ollama's recent-active window). **Phase 6's proxy
must own stream counting**: increment on proxied request start, decrement on completion,
feeding `ActiveTracker.ActiveCount` so eviction's never-kill-active gate protects real
in-flight streams for non-pinned instances too. Until then, "never kill an active stream"
holds only for pinned instances or via the coarse Ollama recent-active approximation.

---

# Run 2 — RAM-safe re-run after the four Phase-5 fixes (2026-07-06, ~22:40–22:50 +06:00)

Same box (RTX 4060 Ti 16 GB, driver 576.40, Windows 10, 16 GB RAM). Daemon rebuilt with the
five reviewed fixes (production queue-drain loop; admission observability; in-flight counting via
a counted completion proxy; DRAINING robustness / async stop; `vram_budget_mib`). Storm config
(`scratchpad/storm2/config.yaml`): `vram_budget_mib=4500`, `drain_interval_seconds=5`,
`safety_margin_mb=512`, `default_context=2048`, Ollama+vLLM disabled, log level debug. Models:
A=`gguf-llama-3-2-1b-instruct-…-q4-k-m` (pinned, interactive), B=`gguf-glm-ocr-glm-ocr-q8-0`
(normal), D=`gguf-paddleocr-vl-1-5-paddleocr-vl-1-5` (normal).

**Outcome: PARTIAL PASS — the SAME system-RAM ceiling that aborted Run 1 bound Run 2, and worsened
mid-run (the box's ~5.4 GB of the user's active Docker containers — postgres/minio/qdrant/kind —
left free RAM oscillating 0.23–1.5 GB, never enough for the 2-concurrent-llama-server storm and its
transient 3-process eviction moment). BUT the behaviors Run 1 could NOT exercise live were proven
live before the RAM abort: FOUR live evictions each with a per-eviction VRAM re-read logged, a 500-
and a 2486-token stream completing intact through the NEW counted completion proxy, the budget clamp,
and the async 202 stop. The two gate criteria still unproven-live are (a) the stream TEMPORALLY
overlapping the eviction, and (b) the queue auto-drain (fix 1) — both fully covered by green
unit+integration tests, but not fired on hardware because the live timing never produced the needed
overlap/queue and the RAM window for a slower/second process was denied by the user's Docker load.
No OOM, no crash, no killed stream; cleanup clean (0 LocalHub procs, 0 LocalHub VRAM by process list).**

## What the RAM ceiling is (unchanged from Run 1, measured again)

Empirical direct-llama-server RAM probes (before the daemon storm):
- Baseline free RAM at rest: 2.85–3.76 GB (volatile; `vmmem`/Docker holds ~5.37 GB constant).
- ONE 1B llama-server (ctx 2048/4096, `-ngl all`): free RAM 2.85 → 2.32 GB (proc WS ~1.1 GB).
- TWO concurrent (1B + GLM-OCR): free RAM bottomed 1.66 GB, settled 1.87 GB — held, no OOM.
- During the daemon storm's evict_then_load + stream (A resident + B resident + D spawning) the
  transient 3-process moment crashed free RAM to 1.05 GB, tripping the 1.5 GB RAM guard (killed
  llama-server), exactly as designed. Later, free RAM at REST oscillated 0.23–1.5 GB for 2+ minutes,
  never reaching the >=2.8 GB a 2-process storm needs. The user's live Docker workload is the binding
  constraint; it must not be evicted, so the full 2-process storm cannot complete on this box now.

## Live evidence captured (verbatim daemon log lines)

Live eviction with per-eviction VRAM re-read (fix 2 observability + the P5.2 anti-OOM re-read — the
#2/#3 Run-1 gaps, NOW PROVEN LIVE). D's admission evicted the idle B and re-read free VRAM from the
probe after the stop:

    time=2026-07-06T22:44:02.091+06:00 level=INFO msg="residency eviction re-read" victim=ce6b1165-6c9d-4580-afbf-4cff5ad80b78 freed_expected_mib=1928 freed_measured_mib=1920 free_after_reread_mib=3271
    time=2026-07-06T22:44:02.091+06:00 level=INFO msg="residency admission decision" model=gguf-paddleocr-vl-1-5-paddleocr-vl-1-5 engine=llamacpp decision=evict_then_load class=normal footprint_mib=1138 need_mib=1650 margin_mib=512 free_mib=1343 free_after_mib=3271 evictions="[ce6b1165-6c9d-4580-afbf-4cff5ad80b78(freed_expected=1928,free_after_reread=3271)]" reason="evict_then_load: evicted 1 idle instance(s) [ce6b1165-6c9d-4580-afbf-4cff5ad80b78]; free VRAM re-read after each eviction is now 3271 MiB >= footprint 1138 + 512 margin (1650 MiB) — loading (vram_budget_mib=4500 caps available to 3271 MiB below the 12338 MiB physically free)"

Three more live evictions were logged the same way. One shows the anti-OOM honesty — freed_measured_mib=3
when the killed process's VRAM had not yet been reclaimed at re-read time; the decision still gated on
the LIVE probe value (free_after_reread=3271), not the expected number:

    time=2026-07-06T22:48:15.549+06:00 level=INFO msg="residency eviction re-read" victim=adfeddc4-efa3-4fd7-b69a-70beef15a56d freed_expected_mib=2026 freed_measured_mib=3 free_after_reread_mib=3271
    time=2026-07-06T22:49:14.330+06:00 level=INFO msg="residency eviction re-read" victim=27d309ee-2d6e-4954-b37c-883c0effdaef freed_expected_mib=1956 freed_measured_mib=1903 free_after_reread_mib=3207

vram_budget_mib (fix 5) working live. Every decision reason shows the supply clamped to the 4500
budget below the ~12–13 GB physically free — the reservation lever:

    reason="load_new: available 4500 MiB >= footprint 1343 + 512 MiB margin (1855 MiB) — loading directly, no eviction (vram_budget_mib=4500 caps available to 4500 MiB below the 13559 MiB physically free)"

Stream completing intact through the NEW counted completion proxy (fix 3). Two streams were relayed
through POST /api/instances/{id}/completions?path=/completion (the daemon endpoint that increments/
decrements the in-flight counter around the relay — the never-kill-active substrate that had ZERO
callers in Run 1):
- 500-token stream: stream_A.sse — 501 content chunks, predicted_n:500, stop:true.
- 2486-token stream: stream_A2.sse — 2487 content chunks, predicted_n:2486, stop:true, 207.9 tok/s;
  daemon logged the relay as `POST …/completions status=200 duration_ms=12281`.
Both completed intact, no truncation, no mid-stream disconnect.

Async 202 stop (fix 4) live. DELETE /api/instances/{A} returned HTTP 202 immediately with a DRAINING
view; the actual process-stop + VRAM verify ran detached in the daemon's background goroutine (the fix
for Run-1 gap #4, the DRAINING-stuck-on-client-disconnect bug).

## Stream/eviction temporal overlap — NOT achieved live (honest)

Both stream runs finished BEFORE the eviction they were meant to overlap. Cause: the 1B model
generates at ~208 tok/s, so even a 2486-token stream lasts only ~12 s, while the eviction EXECUTES in
~14 s (the P4.2 stop does a VRAM-delta poll on the killed victim). The D request that triggers the
eviction was also issued across a separate orchestration step, adding wall-clock latency that exceeded
the stream's lifetime. So A's protection during eviction was demonstrated via its PIN (A stayed
HEALTHY+pinned throughout every eviction, never a victim) but NOT via a live in-flight stream racing
the eviction. The in-flight-protects-a-streaming-instance behavior is proven by the tests
(TestInFlight_StreamingInstanceNotEvictable, TestCompletionProxy_StreamsAndCountsInFlight) but not on
hardware here. A future run needs a slower/larger A (or a single-script fire so D is issued <1 s into
the stream) to force the overlap — which on this box requires the RAM window the user's Docker workload
currently denies.

## Queue auto-drain (fix 1) — NOT fired live (honest)

In every live sequence B fit via evict_then_load directly (an idle victim was available at admission
time), so B never had to QUEUE, and the periodic drain loop never had a queued request to auto-start.
The auto-drain path — the single never-implemented behavior from Run-1 gap #2 — is now fully
implemented and covered by green tests (TestDrainQueue_AutoStartsQueuedRequest end-to-end in core;
TestDrainQueue_AutoStartsWhenRoom / _LeavesQueuedWhenNoRoom / _SingleFlight in residency), but was not
exercised on hardware because the storm's live timing never produced a genuine queue. Forcing a live
queue requires the only eviction candidate to be briefly non-idle (in-flight) at B's admission so B
queues, then idle so the drain evicts it — which needs the 2-process RAM window the box currently lacks.

## Run 2 per-criterion verdict

| Gate criterion | Verdict | Evidence |
|---|---|---|
| >=1 live eviction with per-eviction re-read logged + visible in daemon log | PASS | 4 live evict_then_load evictions, each with a `residency eviction re-read` line (verbatim above) |
| All reasons numeric | PASS | every admission/eviction line carries MiB figures |
| Stream completing intact through the counted endpoint | PASS | 500- and 2486-token streams, stop:true, relayed via /api/instances/{id}/completions |
| No OOM / crash | PASS | no CUDA/OOM/panic in daemon logs; RAM guard aborted cleanly at 1.05 GB (no swap-death) |
| Cleanup clean (baseline +-200, zero procs) | PASS (VRAM by process list) | 0 llama-server/localhub procs; nvidia-smi compute-apps shows no LocalHub VRAM; residual +~280 MiB used is other apps' drift, not a LocalHub leak |
| Stream temporally overlapping eviction pressure | NOT PROVEN LIVE | stream (~12 s) finished before the eviction (~14 s to execute) + inter-step latency; A protected via pin, not via a live in-flight race. Covered by tests. |
| Queue auto-drain starting B with no manual trigger | NOT PROVEN LIVE | B always fit via evict_then_load; never queued. Fully covered by green unit+integration tests. |
| RAM safety (>= floor, no OOM) | BOUND (as Run 1) | free RAM 0.23–1.5 GB throughout; 2-process storm needs >=2.8 GB; user's ~5.4 GB Docker workload is the binding, non-evictable constraint |

## Honest bottom line

The four genuine Run-1 gaps in the CODE are fixed and green (go vet/build/test ./… clean, gofmt clean
on all changed files). Two of the four — live eviction-with-re-read and the counted-stream substrate —
are now ALSO proven on real hardware. The remaining two live demonstrations (stream/eviction overlap;
queue->auto-drain) are blocked not by the code (both tested green) but by the same system-RAM ceiling
that PARTIAL-PASSED Run 1: this 16 GB box, with ~5.4 GB held by the user's live Docker containers,
cannot sustain the 2-concurrent-llama-server storm without OOM risk to the user's workload. Closing
gate P5.3's live overlap+drain criteria requires either freeing that RAM (a user action — pause Docker)
or a bigger-RAM box; the daemon code is ready for both.

---

## Run 3 — micro-storms closing the two remaining live demonstrations (2026-07-06 ~23:10-23:18, orchestrator-driven)

Setup: scratch daemon, `vram_budget_mib` 3000 (storm 1) / 4500 (storm 2), drain 5s.
Models: A=Llama-3.2-1B Q4_K_M (pinned), GLM=GLM-OCR Q8_0, D=PaddleOCR-VL.

### Micro-storm 1 — queue auto-drain, live: PASS
- A pinned HEALTHY (est 1411). D requested → **202 queued** with numeric reason:
  "need 2202 MiB (footprint 1178 + 1024 margin), free 2059 MiB, reclaimable 0 MiB
  from 0 idle instance(s) — 143 MiB short" (pinned A correctly invisible).
- DELETE A → DRAINING (202 async) → STOPPED. Drain loop then auto-started D with
  NO manual call. Daemon log: `queue drained ... requeued=[1daaddfa]` →
  `requeued=[6a0fe69a]` → `started=[dd0cd9f7]`. Final queue: 1 ADMITTED, 2 DONE,
  0 WAITING. D reached HEALTHY.
- Bonus: daemon hard-restart mid-drain → reconciler swept the DRAINING row to
  STOPPED (the Run-1 DB state-lag gap, proven fixed live).

### Micro-storm 2 — stream survives concurrent eviction, live: PASS
- A (pinned) + GLM resident under budget 4500. 3000-token stream fired on A via the
  counted completions endpoint; D requested 2.1s later.
- Timeline: STREAM_START 23:16:58.286 → D_REQUEST 23:17:00.392 →
  **eviction re-read logged 23:17:08.287** (inside the stream window) →
  STREAM_END 23:17:13.364 (3003 data frames, finish_reason=length, [DONE] — intact)
  → D HEALTHY 23:17:31 (HTTP 201).
- Log (verbatim): `residency eviction re-read victim=4bfaaaf8… freed_expected_mib=2049
  freed_measured_mib=2049 free_after_reread_mib=3099` and the admission decision line
  naming footprint/need/margin/free/after + the budget clamp
  ("vram_budget_mib=4500 caps available to 3099 MiB below the 12239 MiB physically free").
- End state: A HEALTHY (pinned, streamed to completion), GLM STOPPED (evicted idle),
  D HEALTHY. Teardown: 0 processes, VRAM baseline (~2650 used).

### Gate P5.3 verdict: **PASS** (Runs 1+2+3 combined)
More-than-fits requested ✓ · no OOM ✓ · no crash ✓ · numeric blocked_reasons ✓ ·
sensible idle-only evictions with per-eviction VRAM re-reads ✓ (live) ·
pinned/active never evicted ✓ (live) · queue auto-drain ✓ (live) ·
active stream survives temporally-overlapping eviction ✓ (live).
