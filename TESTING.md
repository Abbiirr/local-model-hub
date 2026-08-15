# Testing

Verification criteria and closing gates per TODO task. A task may be checked off only
when its gate passes with evidence (command output or file path recorded). `MANUAL`
items name who performs them. Global invariants (apply to every gate): no false
`runs`, no killed active streams, manifests for every artifact, localhost bind,
`reason` on every verdict/refusal, VRAM verified by probe delta not HTTP 200.

## P01

Criteria: docs/23 "Machine" + "Tools tested" sections fully filled with real measured
values (no placeholders); Tailscale presence recorded.

```bash
grep -c ":" docs/23-phase-0-field-validation.md   # spot-check; then human-read the section
```

## P02

Criteria: research/phase-0-friction-log.md exists with ≥10 model rows (model, format,
source, target engine, estimated fit, actual result, friction); llama-swap trial
outcome recorded; every "actual result" for attempted loads obtained within PLAN.md
hardware safeguards.

## P03

Criteria: docs/23 Decision filled (GO_STANDALONE + rationale); docs/decisions.md
ADR-003 resolved with trial evidence and ADR-006 marked Accepted (Go).

## P11

Criteria: `go build ./...` clean; daemon starts, listens on 127.0.0.1 only; SQLite file
created with WAL mode + schema matching docs/data-model.md; `go test ./...` passes.

```bash
go vet ./... && go build ./... && go test ./...
netstat -ano | grep <port>   # bound to 127.0.0.1, not 0.0.0.0
```

## P12

Criteria: GET /api/hardware returns real GPU (RTX 4060 Ti, 16GB total) + CPU/RAM/disk;
SSE stream updates; values cross-checked against nvidia-smi manually; unit tests with
canned nvidia-smi output for parser.

## P13

Criteria: GET /api/engines shows Ollama version/models/ps live, llama.cpp + vLLM config
entries with reachability status; audit confirms zero mutating calls (code review +
no POST/PUT/DELETE to engines in codebase).

## P14

Criteria: inventory contains real Ollama models + a test GGUF folder + an HF dir;
fields per docs/data-model.md (format, quant, size, hash where available); re-scan is
idempotent.

## P15

Criteria: dashboard renders live GPU/engines/inventory in browser served by daemon;
`npm run build` clean; no external CDN dependencies.

## P16

Criteria: Tailscale Serve config documented + endpoint responds through tailnet URL
(agent-verifiable via local tailscale CLI status if installed); MANUAL(user): open
dashboard on phone. Phase 1 close gate: UI/API match real state; zero engine writes.

STATUS 2026-07-06: **BLOCKED (owner: user).** Tailscale 1.96.3 installed and logged in
(LoggedOut=false) but the backend is wedged in NoState with two tailscaled processes;
service restart requires an elevated shell, which the agent does not have.
Next action (user): run `Restart-Service Tailscale -Force` from an ADMIN PowerShell
(or quit+relaunch the Tailscale tray app), then run `scripts/tailscale-serve.ps1`,
then open the printed ts.net URL on your phone. Everything agent-preparable is done:
serve script, docs (examples/tailscale-serve.md), SSE proxy-friendly headers, loopback
bind verified. All other Phase 1 close-gate criteria are PASS.

## P21

Criteria: estimates for local GGUF + remote HF GGUF (no full download) via
gguf-parser-go library; unit tests against ≥3 known models with recorded expected
ranges; no hand-written GGUF estimator anywhere.

## P22

Criteria: safetensors estimator handles GQA and MHA correctly (unit tests with real
config.json fixtures, e.g. Llama-3.x GQA + an MHA model); includes weights + KV + overhead
components per docs/compatibility-engine.md.

## P23

Criteria: verdicts carry reason/estimated_vram/max_context_that_fits/run_flags always
(schema-enforced); GGUF never recommended to vLLM; CC gate uses real probe value; unit
tests cover fits/degraded/won't-fit boundaries with margins.

## P24

Criteria: POST /api/compat/preview returns verdict for a remote HF repo without
downloading weights; UI shows verdict incl. reason; error paths return readable reasons.

## P25

Criteria: validation matrix file with 20 models covering GGUF+safetensors, GQA+MHA,
fits+doesn't-fit; per-model estimator error recorded; **zero false-positive `runs`**
(non-waivable); real-load subset executed within safeguards (one model at a time,
VRAM margin, cleanup verified); results appended to docs/23 or research/.

## P31

Criteria: HF download resolves branch/tag→commit SHA and shows it (with verdict) before
committing to download; resumable; per-file sha256 recorded and verified; single-GGUF
URL path works.

## P32

Criteria: `ollama pull` passthrough records digest; local import writes manifest with
explicit `local-import` provenance warning.

## P33

Criteria: every download/import path writes an immutable manifest (source, revision,
files, hashes, license, warnings); pickle/trust_remote_code produce warnings not blocks
(strict mode optional); reproduce export names repo+commit+files+hashes(+flags if run).
Phase 3 close gate: no artifact in inventory without provenance.

## P41

Criteria: state machine transitions persisted; kill -9 the daemon mid-run then restart:
instances reconciled (reattach or reap), no orphaned llama-server processes left
(verified via process list); unit tests for all legal/illegal transitions.

## P42

Criteria: start a small GGUF via adapter → HEALTHY (health endpoint), stop → process
gone AND VRAM freed within tolerance (probe delta ≥ model footprint − 500MB); flags +
port recorded in DB; localhost bind verified.

## P43

Criteria: warm a small Ollama model, /api/ps mirrored; unload via keep_alive:0 and
poll until gone; VRAM delta verified; no direct process kill of shared ollama daemon.

## P44

Criteria: vLLM adapter passes contract tests against stub implementing documented
endpoints (health, /v1 responses, slow-start behavior); STARTING state surfaced in UI;
spawn command matches current vLLM docs (cited in code/task notes).

## P45

Criteria (one controlled session): docker run vLLM (WSL2 RAM cap ≤8GB verified first),
tiny model (≤1B); adapter reports STARTING→HEALTHY; one minimal completion; stop;
container removed; `wsl --shutdown`; GPU VRAM back to baseline. Phase 4 close gate:
all three engines start/stop with hardware-verified VRAM reclamation.

## P50

Criteria: design note (docs/) specifies the custom queue-ordering algorithm precisely
(priority classes, aging function, size/cost term, tie-breaks) with worked examples;
property tests prove: no starvation (any queued request eventually reaches front under
continuous higher-priority arrivals), stable ordering, and that ordering never triggers
eviction of pinned/active instances. `go test ./internal/residency/...` green.

## P51

Criteria: admission decisions unit-tested (warm-hit, fits-directly, fits-after-evict,
cannot-fit → queued/rejected with blocked_reason); free VRAM re-read from probe after
every eviction (asserted in tests via fake probe call counts).

## P52

Criteria: victim ordering property-tested (TTL-expired first, then idle+low-prio+LRU,
then largest); pinned and stream-active instances never selected by default; DRAINING
semantics honored.

## P53

Load-storm on the real GPU with 3–4 small models whose sum exceeds free VRAM:
no OOM (driver log/probe), no killed active stream (a generation in flight completes),
no crash, queue shows blocked_reasons, evictions sensible. Phase 5 close gate. Cleanup:
all processes gone, VRAM baseline restored.

STATUS 2026-07-06 23:20 FINAL: **PASS** — Runs 1+2+3 in research/phase-5-load-storm.md.
Run 3 micro-storms closed the last two live demos: queue auto-drain fired live
(202-queued D auto-started by the drain loop after A stopped, log-evidenced) and a
3000-token stream on pinned A survived a temporally-overlapping eviction of idle GLM
(eviction re-read logged at 23:17:08.287 inside the 23:16:58–23:17:13 stream window,
freed_measured=2049=expected). Prior status kept below for history.

PRIOR STATUS 2026-07-06 23:0x: NEARLY CLOSED — two live demos remained. Evidence:
research/phase-5-load-storm.md Runs 1+2. Proven live: no OOM, no crash, numeric
blocked_reasons, pinned never evicted, 4 live evictions with per-eviction VRAM
re-reads logged, counted streams intact, budget clamp, async 202 stop. All five code
gaps from Run 1 fixed + 18 new tests green (drain loop, admission logging, in-flight
counting via counted completion proxy, DRAINING async-stop, vram_budget_mib).
Remaining (test-covered, unproven live, RAM-ceiling-blocked): (a) stream temporally
overlapping an eviction, (b) queue auto-drain firing live. Plan: two micro-storms
(~3 min, 2 small processes, vram_budget_mib=3000/4500) — scripted in
temp-research/context-summary.md §micro-storms; was interrupted by user pause
2026-07-06 night; resume first thing next session.

## P61

Criteria: one OpenAI base URL: /v1/models lists inventory-runnable models;
chat/completions routes to each engine (llama.cpp + Ollama real; vLLM via stub or the
P45 session if still feasible); streaming byte-passthrough verified (SSE chunks
unmodified); blocked admission → fast 503 + blocked_reason (measured <2s); grep-level
audit confirms no budgets/keys/fallback/multi-provider code. Phase 6 close gate.

STATUS 2026-07-07: **PASS** — evidence research/phase-6-live-gates.md (+ Amendment).
/v1/models OpenAI list shape (10 GGUF models; Ollama model appears once daemon up);
llama.cpp real: cold start streamed 12 chunks + [DONE] raw passthrough, warm 1617ms/35
chunks; Ollama real: 200 via sam860/deepseek-r1-0528-qwen3:8b (relay rewrites model→tag
for Ollama only; three pre-existing routing defects fixed + unit-tested); vLLM via stub
(Agent A unit tests). Blocked→503: initially ~10s (gguf-parser 4×~2.7s CPU parse before
Admit); fixed via mmap+SkipLargeMetadata on local parse (byte-identical estimates,
TestRealFile_EvaluateUnder2s) → live 1.36–1.54s over 8 trials incl. first-call, numeric
blocked_reason preserved. Byte-identity: bytes.Equal capture-and-diff vs httptest fake
upstream (Agent A) + live chunk-shape check. Grep audit of internal/proxy: only
disclaimer-comment hits. In-flight counting through the proxy closes the P5 gap
(ActiveCount asserted in tests; eviction skips active). Full suite green (18 pkgs).

## P62

Criteria: submit async job → id returned immediately; result retrievable by poll AND
delivered over WebSocket subscription; cancellation works mid-queue and mid-inference
(stream aborted server-side, instance not killed); daemon restart with queued jobs →
jobs still present and runnable; all failure states carry reasons. Integration test
against a real small llama.cpp model within safeguards.

STATUS 2026-07-07: **PASS** — evidence research/phase-6-live-gates.md. Real llama.cpp
1B: submit → 201 {id,queued} in 102ms; poll reached succeeded with full OpenAI
result_json; WS jobs.updated frames queued→running→succeeded captured on /api/ws with
pushes flowing alongside. Cancel mid-queue: 200 cancelled, reason non-empty. Restart
with same DB: queued job survived and remained runnable; running→failed with reason
"daemon restarted mid-inference" (live via seeded running row — a 1B finishes <1s so
mid-run kill timing is impractical; path also unit-tested by Agent A). Reasons asserted
non-empty on every failed/cancelled row (unit). ReconcileJobs runs at boot before the
worker (main.go wiring).

## P63

Criteria: JSON-RPC 2.0 compliance (id echo, error codes, batch requests) verified by
tests; method coverage mirrors REST core (hardware/engines/models/compat/instances/
residency/jobs); works over both HTTP POST and WebSocket; no business logic duplicated
(code review confirms thin-transport rule from PLAN.md decision 7).

STATUS 2026-07-07: **PASS** — internal/jsonrpc: 13 test functions green (id echo table
incl. present-null vs absent-id notification; -32700/-32600(+empty batch)/-32601/
-32602/-32000 with data.reason; mixed batch 2 req + 1 notif → exactly 2 responses;
method coverage asserts all 18 methods reach core). Thin-transport verified
structurally (TestPackageImportsOnlyCoreAndStdlib: imports = core + stdlib only).
Live (research/phase-6-live-gates.md): POST /api/rpc 200 with id echo; WS parity —
same method over a WS frame returns the same result, notification produces no response
frame, non-RPC frames ignored, pushes keep flowing; jobs.submit over RPC created a real
job (coreJobService shim proven); batch over HTTP → 2 responses. Codex findings on the
fragments (id:null semantics, -32700 for malformed single) fixed with tests.

## C1

Criteria: five master success criteria walked and evidenced on this box; docs/18
updated (dated); changelog + known-limitations written; final code-review sweep clean of
blocking findings; all phase gates PASS recorded.

STATUS 2026-07-07: **PASS (agent-side complete; master #4 pending C2 user check)** —
evidence research/c1-verification-walk.md + research/phase-6-live-gates.md (+Amendment).
Master criteria: #1 inventory+verdicts PASS (3 verdicts w/ reason+vram+ctx+flags, incl.
wont_run cross-checked vs live free VRAM and TQ3 fallback); #2 live evict-then-load PASS
(NVML re-read freed_measured=2311, no OOM); #3 never-kill-active PASS with the audit's
missing live proof closed — a NON-pinned stream survived a temporally-overlapping
eviction protected purely by InFlightTracker ActiveCount (7511 chunks + [DONE] intact);
#4 phone-over-Tailscale BLOCKED-user (all agent-side prep done; closes under C2);
#5 manifests PASS (live local_import manifest + reproduce export with hashes).
All 7 non-waivable gates re-confirmed PASS (localhost binds verified via netstat incl.
child llama-servers; no conversion path; no preemption; reasons on proxy 503 / job
failure / JSON-RPC data.reason). Docs: docs/18 dated 2026-07-07, CHANGELOG.md +
KNOWN-LIMITATIONS.md written, api-spec/architecture/security/README synced. Code review:
three Codex sweeps this cycle (pre-P6, post-integration, final) — 15 findings total, all
triaged and fixed with tests (incl. P1 auth enforcement, P1 job queue-row leak, P1
over-cap download cleanup, unified Ollama/vLLM admission, RAM floor
residency.min_free_ram_mib, format-based safetensors→vLLM routing, WS-default dashboard).
Suites at close: go vet/build/test ./... green (18 pkgs); web 66/66 vitest; web/dist
rebuilt for go:embed.

## C2

MANUAL(user): initial commit (repo already git-initialized, local-only — run
`git add -A && git commit -F COMMIT_MSG.txt`); phone dashboard confirmation; sign-off on
PLAN.md#decisions (ADR-006 Go, vLLM strategy, Phase 0 GO, model budget).

## P71

Criteria (two-daemon rig on this box — coordinator + node on separate ports, disjoint
scratch DBs): node with cluster.role=node joins the coordinator (push join + heartbeat,
one idempotent upsert); GET /api/cluster/nodes shows the node with last_seen/status;
kill the node → status degrades within one heartbeat interval; restart coordinator →
registry rebuilt from heartbeats (soft state). cluster.role=off (default) is
byte-identical to 1.0: full suite green + a regression test proving no new routes/fields
leak when off. cluster_token validation trio per docs/cluster-plan.md when non-loopback.

## P72

Criteria: coordinator /api/cluster/models + /api/cluster/hardware fan out to all UP
nodes and aggregate with node attribution; coordinator /v1/models returns the
union-deduped runnable set; a dead node yields partial results with an explicit
partial/degraded marker, never a hang or hard failure (rig: kill one daemon mid-query).

## P73

Criteria (money milestone): POST /v1/chat/completions on the coordinator routes to the
correct node by placement policy (already-warm > has-model-on-disk > runs beats
runs_degraded > most-free-VRAM > round-robin — divergence created via disjoint
model_dirs + different vram_budget_mib on the rig); streaming byte-passthrough via the
existing relay; block reasons propagate with node-name prefixes; a request for a model
no node has → 404 with reason listing per-node status.

## P74

Criteria: coordinator subscribes to each node /api/ws and republishes {node_id, event}
frames on its own /api/ws (rig: instance start on the node produces node-attributed
instances.updated on the coordinator socket); jobs submitted to the coordinator are
placed and forwarded, job record durable on the OWNING node (coordinator restart does
not lose it; node restart honors the node's own ReconcileJobs semantics).

## P75

Criteria: with cluster.role != off AND a non-loopback bind, startup REFUSES unless the
cluster token trio (per docs/cluster-plan.md §security) is configured — validation-time
error with a clear reason; negative-auth tests (wrong/missing token → 401 on every
cluster endpoint incl. WS join); token rotation runbook documented; dev-endpoint
allow-list audit extended to the coordinator relay (never forwards vLLM dev endpoints
cross-node); Tailscale cluster guide in examples/.

---

## Qwen3.6-27B MTP run/stop/monitor integration (M-series)

Source: `PLAN-run-stop-monitor.md`. Gates M0→M6; do not close a later gate while an
earlier one is red. Non-waivable global invariants apply to every gate: no false `runs`,
no killed active streams, `reason` on every verdict/refusal, VRAM verified by NVML probe
delta (not HTTP 200), additive migrations only, no model ≥30B loaded on this box.

## M0

Criteria: build/vet/full-Go-suite/web-vitest/typecheck/production-build green; the
`TestRealFile_EvaluateUnder2s` "failure" is NOT a defect (the asserted `Evaluate` call is
~170–190 ms vs a 2 s ceiling; the "3.12 s" was the `internal/compat` package wall time,
misread). Only the 4 pre-existing `gofmt -l` files may remain, and only when in scope.

```bash
go vet ./... && go build ./... && go test ./...
npm --prefix web run lint && npm --prefix web run typecheck && npm --prefix web run build
gofmt -l internal/ compat/ cmd/   # only the 4 known files when in scope
```

STATUS 2026-07-25: **M0.GATE PASS** (4 consecutive `go test ./...` exit 0; web 80/80;
`tsc --noEmit` clean; production build clean). M0.3 (the 4 gofmt files) still open and
only blocks when those files are touched. M0.1/M0.2 closed as non-defects.

## M1

Criteria: target GGUF scanned via the normal configured inventory path (done); immutable
manifest with path/size/SHA-256/MTP metadata exists; the pinned b10106 build is recorded
with version `10106` / commit `1425386fd`, SHA-256, and a re-verification cadence; older
binaries are retained ONLY as capability-gated fallbacks (never deleted to force
selection).

**M1.GATE (binding):** on a clean restart with all binaries configured, LocalHub selects
b10106 for the Qwen3.6-27B MTP model **without deleting any other binary**, AND TQ3_*
models still resolve to the has_tq3 fork. Proven by a selection table-test + a live
`/api/engines` + `/api/instances` check. No config-only workaround (`binary_paths: []`)
may be required to reach b10106.

STATUS 2026-07-25: selection logic LANDED (M3.6/M3.7) + table tests PASS — b10106 configured
first wins for a non-TQ3 model even when a has_cuda fork is present; TQ3 models still resolve
to the fork. Remaining for full close: the LIVE `/api/engines` (b10106 reports CUDA via the
co-located ggml-cuda.dll) + compat-verdict check with fallback binaries restored.

## M2

Criteria: one versioned durable launch-profile contract threads context, **independent
K-type/V-type** (replacing the bare `kv_quant` bool), speculative mode, MTP draft max,
parallel slots, flash-attn, CPU-offload permission, projector, pin, and priority through
REST → core → `LaunchPlan` → UI → **queue schema (additive migration `0009_*`)** →
jobs/JSON-RPC → instance record. A queued start replays to a byte-identical normalized
plan across a daemon restart (no silent f16-KV regression). `kv_quant`/`allow_cpu_offload`
/speculative MUST survive queue replay (migration 0006 dropped them — confirmed defect).

**M2.GATE:** `go test ./internal/...` green with a queue-replay test asserting the
normalized plan is identical post-restart; the new migration is additive and runs cleanly
on an existing DB.

STATUS 2026-07-25 (M2.1–M2.5 landed — UNIT/TABLE PASS; the LIVE replay is still owed):
- **M2.1 REST.** `core.StartInstanceRequest` gained `kv_type_k`, `kv_type_v`, `slots`,
  `spec_draft_max` (alongside the existing `speculative`). `kv_quant` still works and is documented
  as a SHORTHAND for `q4_0/q4_0`; an explicit pair WINS over it. Every value is validated by
  `validateStartProfile` BEFORE any engine dispatch, so a bad value is refused identically for
  llama.cpp/Ollama/vLLM and nothing spawns or queues. `tq3_0` is deliberately refused (the estimator
  cannot price it → no speculative admission). Tests: `TestValidateStartProfile_Refusals`,
  `TestStartProfile_KVPrecedence`, `TestValidateKVCacheType`, `TestValidateSlotsAndSpecDraftMax`.
- **M2.3 UI + saved defaults.** `AdvancedStartForm` exposes the independent (K,V) pair, `-np`, spec
  mode + draft max; collapsed by default, and showing a summary whenever a non-default profile is
  armed so it is never silently in effect. `profileToRequest` OMITS blank fields, so an untouched form
  sends the byte-identical body it sent before. Migration `0011_model_launch_defaults` +
  `GET/PUT/DELETE /api/models/{id}/launch-default` store a per-model default that is **ADVISORY
  only** — the start path never reads it (M6.1); the UI pre-fills from it and the operator confirms.
  Saved defaults are provenance-bound (engine build/commit/binary/sha256) and report `stale_reason`
  when the recorded build is no longer among the detected binaries (M6.2). Web suite **101/101**
  (was 80), `tsc --noEmit` clean, production build clean.
- **M2.4 queue persistence.** Migration `0009_queue_launch_profile` adds
  `req_kv_type_k`/`req_kv_type_v`/`req_slots`/`req_spec_mode`/`req_spec_draft_max`/
  `req_allow_cpu_offload` (all nullable or defaulted; every read COALESCEd so pre-migration rows load
  as "no advanced knobs"). `residency.ProfileSpec` carries them through `Request`/`AdmitSpec`/
  `QueueRow`/`DrainRequest`, `RestoreWaiting` restores them, and `residencyStarter.StartQueued` —
  which was silently dropping all three fields — now rebuilds the request through the single
  `startRequestFromDrain` translation. Tests: `TestQueueReplay_LegacyKVQuantSurvives` (the exact live
  defect: `kv_quant=true` must come back `q4_0/q4_0`, never f16), `TestQueueReplay_FullProfileRoundTrip`,
  and `TestQueueReplay_NoFieldSilentlyDropped` (per-field, so a future field that is added but not
  threaded fails the suite).
- **M2.5** JSON-RPC `instances.start` unmarshals straight into `StartInstanceRequest`, so the new
  fields flow through unchanged; jobs/proxy deliberately start with default profiles.
**LIVE 2026-07-25 — migrations applied cleanly to the real DB (half the gate):** the daemon was rebuilt
and restarted against the production `localhub.db` (backed up WAL-consistently first).
`schema_migrations` now runs 1→11 with `9 queue_launch_profile`, `10 instance_health_reclaim`,
`11 model_launch_defaults`; all six `queue.req_*` columns, all three `instances` health/reclaim columns,
and the `model_launch_defaults` table are present. **Backward compatibility proven on production data:**
the DB holds **165 queue rows written by the OLD binary**, and every one reads back through the new
COALESCEd query as "no advanced knobs" (`req_kv_type_k/v = NULL`, `req_slots = NULL`, `req_spec_mode =
NULL`, `req_allow_cpu_offload = 0`) with no scan errors. Those rows are also the fossil record of the
defect this work fixes: the same request re-queued over and over with `blocked_reason: "re-queued by
drain: still no room"`, because it replayed as f16 and could never fit.

**M2.GATE — LIVE PASS 2026-07-25.** With the 27B running the MTP profile, a start for llama-3.2-1B was
submitted carrying a deliberately non-default profile: `ctx 4096, kv_type_k q8_0, kv_type_v q4_0,
slots 2, speculative ngram-cache, allow_cpu_offload true, priority 1`.

1. It returned **202 queued** with a numeric block — `ram_blocked: free system RAM 936 MiB below floor
   2048 MiB` — so nothing was spawned.
2. The durable row carried the WHOLE profile: `req_ctx=4096, req_kv_type_k='q8_0',
   req_kv_type_v='q4_0', req_slots=2, req_spec_mode='ngram-cache', req_allow_cpu_offload=1,
   priority=1`. (The row id differed from the 202's, because the drain loop had already re-queued it —
   which additionally proves `requeueForDrain` preserves the profile.)
3. **The daemon was killed and restarted.** It logged `restored durable residency queue waiting=1`, and
   the row read back **byte-identical**: `ctx=4096 kvK='q8_0' kvV='q4_0' slots=2 spec='ngram-cache'
   offload=1 prio=1`.
4. The re-derived `blocked_reason` proves the restored profile is actually FEEDING admission rather than
   just sitting in a column: *"waiting for VRAM: need 2359 MiB (footprint 1335 + 1024 margin), free 1740
   MiB"* — 1335 MiB is the estimate computed **with** the asymmetric q8_0/q4_0 pair. Under the old code
   this replayed as f16 and priced differently.
5. The 27B reattached across the same restart at its original pid with its full MTP flags intact.

Entry cancelled afterwards; no stray rows. This is the exact criterion — a queued request yielding an
identical normalized plan after a daemon restart — so **M2.GATE is closed**.

## M3

Criteria: the llama.cpp adapter probes `has_spec_mtp` (`--spec-type … draft-mtp …`),
`--spec-draft-n-max`, and independent `-ctk`/`-ctv` from the binary's real `--help`
(verified primary source, not assumed); the model's embedded MTP tensors are detected from
the GGUF; MTP is gated on BOTH model and selected binary; the validated pinned profile
emits `--spec-type draft-mtp --spec-draft-n-max 2 -np 1` plus `-ctk q8_0 -ctv q4_0`
(independent K/V, replacing the capability-bit-driven `-ctv tq3_0`); every unsupported
combination returns a human-readable reason.

**M3.6/M3.7 (selection — binding, blocks everything):**
- **M3.7 (A)** `HasCUDA` is NOT a selection criterion. Binary selection filters by
  REQUIRED capabilities, then picks the first match in CONFIGURED order. A non-TQ3 model
  with b10106 configured first selects b10106 even when a `has_cuda` fork is present.
- **M3.7 (B)** a modern CUDA build that advertises no backends in `--version` is still
  detected as CUDA (via the co-located `ggml-cuda.dll`), so `/api/engines` stops reporting
  it as backend-less. `has_cuda` remains an accurate REPORTING field, just not a selector.
- **M3.6** selection is deterministic and table-tested: capabilities first, configured
  preference second; TQ3 models still resolve to the fork.

**M3.GATE:** `go test ./internal/compat/... ./internal/engines/llamacpp/...` green with
table tests over (a) selection order [b10106-first beats has_cuda fork; TQ3 → fork;
no-capable-binary → (zero,false)], (b) exact emitted flags for the pinned profile, and
(c) every refusal carrying a reason.

STATUS 2026-07-25 — **M3.GATE CLOSED at the unit/table level** (a), (b) and (c) all PASS:
- **(a) selection order** — `bestLlamaBinary(BinaryRequirements)` capability-filter + configured
  order, plus `backendsFromDir` DLL inference (M3.6/M3.7). Also proven in the INVERSE direction now:
  an MTP profile picks the only `has_spec_mtp` binary even when an incapable one is configured FIRST
  (`TestPlanLlamaLaunch_MTPSelectsCapableBinary`), while the same availability without an MTP request
  honours configured order — capability requirements outrank preference, preference breaks ties.
- **(c) `has_spec_mtp` capability (M3.1)** — RE-VERIFIED against the three real binaries' `--help`
  on 2026-07-25 before any code depended on it:
  - b10106: `--spec-type none,draft-simple,draft-eagle3,draft-mtp,draft-dflash,ngram-simple,
    ngram-map-k,ngram-map-k4v,ngram-mod,ngram-cache`; `--spec-draft-n-max N` (default 3);
    `-ctk/--cache-type-k` + `-ctv/--cache-type-v`; `-np/--parallel N` (default -1 = auto).
  - llama.cpp-tq3 fork AND the winget build: `--spec-type` and `-ctk/-ctv` present, but **0 hits**
    for `draft-mtp` and no `--spec-draft-n-max`.
  `Capabilities` gained `has_spec_mtp`, `has_spec_draft_max`, `has_kv_cache_types`,
  `has_parallel_slots`, all surfaced on `/api/engines` and in the engine reason string. Each bit is
  probed INDEPENDENTLY (a build exposing one must not have the other inferred) —
  `TestParseCapabilities_{MTPBuild,ForkHasNoMTP,IndependentBits,KVNeedsBothSides}`,
  `TestBuildReason_SurfacesMTPCaps`. `core.llamaBinaryFromView` now carries them into selection, and
  `SupportsKVQuant` reflects the probed `has_kv_cache_types` instead of being hardcoded true.
- **(b) exact emitted flags (M3.4)** — `TestPlanLlamaLaunch_MTPExactFlags` pins the pinned profile as
  name/value PAIRS: `-ngl all -c 8192 --fit off -fa on -ctk q8_0 -ctv q4_0 -np 1 --spec-type draft-mtp
  --spec-draft-n-max 2`, and asserts the capability-bit-driven `-ctv tq3_0` is GONE (replaced, not
  added alongside). `--spec-draft-n-max` is gated on the binary advertising it
  (`TestGGUFRunFlags_SpecDraftMaxGatedOnBinary`); a binary without `-ctk/-ctv` is forced to f16 and the
  plan REPORTS f16 rather than emitting flags it cannot honour
  (`TestPlanLlamaLaunch_KVForcedF16OnIncapableBinary`); draft-mtp forces `-np 1` even when more slots
  are requested (`TestPlanLlamaLaunch_MTPForcesSingleSlot`).
- **M3.2/M3.3/M3.5 gating + reasons** — `gguf.InspectMTP` detects the embedded head from the GGUF
  tensor-info (`.nextn.` marker, verified on the real target file; `mtp`/`draft` accepted for forward
  compat, no model load). MTP requires BOTH sides, and each refusal names WHICH side failed and what
  to do instead (`TestPlanLlamaLaunch_MTPRefusals`). `draft-mtp` is now a VALID `speculative` value
  (it used to be a flat 400) while an unvalidated mode such as `draft-eagle3` is still refused even
  though b10106 advertises it.
**MTP RAN LIVE 2026-07-25 — first managed MTP start on this box.** With the 27B stopped, a start with
the full profile (`ctx 2048, kv_type_k q8_0, kv_type_v q4_0, slots 1, speculative draft-mtp,
spec_draft_max 2, pin`) reached **HEALTHY** (pid 2248) and served inference (`content: "MTP OK"`,
`finish_reason: stop`, 150 completion tokens, fingerprint `b10106-1425386fd`). Emitted flags, verbatim:

```
-ngl all -c 2048 --fit off -fa on -ctk q8_0 -ctv q4_0 -np 1 --spec-type draft-mtp --spec-draft-n-max 2
```

That is the pinned profile exactly: independent K/V, single slot, MTP head, draft max 2 — and no
`-ctv tq3_0`. Estimate 13775 MiB, measured peak **13370 MiB** (+405 MiB pessimism; conservative, as
required).

**A HARD FINDING — the q8_0/q4_0 profile does NOT fit at 8K on this card.** LocalHub refused it with
numbers rather than letting it OOM:

| ctx | estimate | usable (16380 × 0.92 − 1216 in use) | outcome |
|---|---|---|---|
| 8192 | 14420 MiB | 13951 MiB | `wont_run` — 469 MiB short |
| 4096 | 13990 MiB | 13951 MiB | `wont_run` — **39 MiB short** (the knife-edge itself) |
| 2048 | 13775 MiB | 13951 MiB | **admitted, ran** |

So the MTP profile is real and works, but on 16 GB it buys context only down at 2K unless the safety
factor is revisited or desktop VRAM (~1.2 GiB in use) is freed. The 4K row is M4.6 in the flesh: a
39 MiB margin is pure noise, which is exactly why a fit that thin is now reported `runs_degraded`.

**A REAL BUG this live run caught (fixed).** The 2048 MTP run recorded `kv_type='q8_0'` in
`observed_peaks` — the bare K type — because `LaunchPlan.KVType` was set to `kvK` alone. That key would
later MATCH a q8_0/q8_0 lookup, letting a peak measured with a cheap q4_0 value cache justify an
expensive q8_0 one: precisely the "never let a measurement from one (K,V) pair justify another" rule.
`LaunchPlan.KVType` is now `CanonicalKVKey(k, v)`; the poisoned row was corrected in place; regression
test `TestPlanLlamaLaunch_CalibKeyIsCanonicalPair` covers both the asymmetric and symmetric renderings.
**No unit test would have caught this** — it needed a real asymmetric run.

- **Still open for M6.5:** an MTP-on vs MTP-off throughput/determinism A/B. Not attempted, and the two
  configurations are not comparable at equal context anyway (MTP fits only at 2K here).

**LIVE-VERIFIED 2026-07-25 (daemon rebuilt + restarted, pinned 27B kept up).** `/api/engines` now
reports the probe against all three real binaries, and it discriminates exactly as the table tests
predict:

| binary | version | backends | has_spec_mtp | has_spec_draft_max | has_kv_cache_types | has_tq3 |
|---|---|---|---|---|---|---|
| b10106 (tools/llama.cpp) | `10106 (1425386fd)` | CPU, CUDA, RPC | **true** | **true** | true | false |
| CUDA TQ3 fork | `1 (1cfa891)` | CUDA | false | false | true | **true** |
| winget build | `8833 (45cac7ca7)` | RPC, Vulkan, CPU | false | false | true | false |

The engine reason string carries them too: *"…caps: flash-attn, fit, sleep-idle, kv-types (-ctk/-ctv),
spec-mtp (draft-mtp), spec-draft-n-max."* This closes M3.1 live: the only binary that can be selected
for a `draft-mtp` profile is b10106, and the fork retains the `has_tq3` capability that TQ3 models
require — both directions of M1.GATE still hold after the change.

## M4

Criteria: K and V are sized INDEPENDENTLY at the requested context; embedded MTP tensors
and runtime overhead are included; the live NVML re-read and all safety floors/margins are
preserved; estimator erring conservative (predicted 14107 MiB vs observed 13537 MiB at 8K
q4_0 = +570 MiB pessimism — no false `runs`); the ~50 MiB admission knife-edge at 8K is
closed (est 14107 vs 14153–14168 usable flips 201↔409 on desktop VRAM noise); calibration
climbs 8K → 16K → 32K → 64K, each tier safe before the next.

**M4.GATE:** zero false-positive `runs` (non-waivable); predicted vs observed
baseline/peak/recovered VRAM recorded per calibration tier; no 64K default persisted until
admission calibration and the managed lifecycle pass at that context.

STATUS 2026-07-25 (M4.1/M4.2/M4.6 landed — UNIT PASS; calibration tiers still owed):
- **M4.1 independent K/V sizing — and a REAL BUG found and fixed.** `gguf.Options` already had
  `KVCacheTypeK/V` for the gguf-parser path, but the canonical calibration key was being smuggled
  through `Options.KVCacheType`, which is ALSO consumed as a real dtype by the TQ3 file-size fallback
  (`kvBytesForType`) and the iSWA floor (`kvBytesPerElemX8`). For an asymmetric profile that field held
  `"q8_0/q4_0"` — not a dtype — so both paths fell through to their `default` branch, priced the cache
  as f16/f16, and printed a note that DISAGREED with the emitted `-ctk/-ctv` flags. Fixed by splitting
  the concerns: a new explicit `Options.CalibKey` carries the lookup key, `Options.CalibrationKey()` is
  the single derivation point, and `kvCacheBytes`/`iswaKVFloorBytes` now price K and V separately. A
  SYMMETRIC pair reproduces the old numbers exactly (asserted), so no recorded estimate moved. The
  mis-pricing was in the SAFE direction (over-count), so it could not have caused a false `runs` —
  but the printed reasoning was wrong, which CLAUDE.md's "every verdict carries a human-readable
  reason" does not tolerate. Tests: the `kvCacheBytes` symmetric-identity + asymmetric-ordering
  assertions, `TestISWAKVFloorBytes_*`, and the pre-existing calibration regression guards
  (`TestVerdictUsesObservedPeak`, `TestVerdictTightensAfterRecording`) which caught the FIRST version
  of this refactor and were themselves reading the wrong field — now routed through
  `CalibrationKey()` like production.
- **M4.2 MTP head cost.** `InspectMTP` sums the `.nextn.*` tensor bytes and the verdict adds them as an
  honest additive surcharge when `draft-mtp` is on (~53 MiB on the Qwen3.6 target), consistently at the
  requested ctx AND across every `ggufMaxContext` probe.
- **M4.6 the ~50 MiB knife-edge — closed as a HONESTY fix, not a policy loosening.** New
  `AdmissionNoiseMiB = 256` (measured: est 14107 MiB vs usable 14153–14168 MiB is a 46–61 MiB margin,
  smaller than ordinary desktop VRAM churn, which is why the SAME request flipped 201↔409). A fit
  inside that band now reports `runs_degraded` with a numeric reason naming the headroom and what makes
  the decision deterministic (record a measured peak for this exact (K,V) pair — which on this profile
  moves the margin from ~50 MiB to ~600 MiB — free VRAM, or lower the ctx). `withinAdmissionNoise` is an
  ABSOLUTE-MiB check, deliberately distinct from the relative ±15% `nearBoundary`, returns false for a
  non-fitting estimate so it can never soften a refusal, and is ordered so a MEASURED (`exact`) verdict
  is never downgraded — a measurement IS the resolution of the knife-edge.
- **STILL OWED for M4.GATE:** the calibration tiers themselves (M4.4/M4.5). No 8K→16K→32K→64K sweep has
  been run, so no 64K default is persisted and none may be until it has.

## M5

Criteria: periodic process + health reconciliation runs AFTER `HEALTHY` (today only a
start-time crash → FAILED was observed); a post-HEALTHY death becomes `FAILED` with a
reason; reclaim-TIMEOUT and reclaim-INCONCLUSIVE must NOT report a clean `STOPPED`;
active-stream drain (stop while a completion streams) ends truthful; `DRAINING`,
PID/port/last-health/exit-reason stay coherent. MTP acceptance is surfaced ONLY with a
stable, tested signal (none promised until one exists).

**M5.GATE:** kill a disposable HEALTHY process → API/UI reflects it within the documented
interval without a LocalHub restart; active-stream drain, reclaim-timeout, and
post-HEALTHY crash tests all end in a truthful terminal state with a reason.

STATUS 2026-07-25 (M5.1/M5.2/M5.5 landed — UNIT PASS; the live kill is still owed):
- **The documented interval is 15 s** (`core.DefaultMonitorInterval`). A dead engine becomes `FAILED`
  within one tick; an alive-but-unresponsive one is reaped after `ReapAfterFailures` × interval
  (3 × 15 s ≈ 45 s at the defaults).
- **M5.1/M5.2 post-HEALTHY monitor.** New `instances.Monitor` + `Service.StartPeriodicMonitor`, wired in
  `main` alongside the drain loop, reusing the SAME health/identity checker as startup reconcile.
  Outcomes: PID gone → `FAILED` (unconditional, nothing to kill); alive but failing → the consecutive
  count is PERSISTED and surfaced, escalating to a kill + `FAILED` only past the threshold; healthy →
  `last_health_at` stamped and the counter reset. Migration `0010_instance_health_reclaim` adds
  `last_health_at`, `health_failures`, `reclaim_verified`. No new instance STATE was invented — these
  add observability to existing states rather than threading an `UNHEALTHY` state through every
  transport.
- **A post-HEALTHY death is FAILED, never STOPPED.** A requested stop goes through `DRAINING`; reporting
  a vanished engine as `STOPPED` would claim a graceful shutdown AND imply a verified VRAM reclaim that
  was never measured. `TestMonitor_PostHealthyDeath_IsFailedNotStopped` asserts the state, the reason
  text ("exited", "after reaching HEALTHY", "not a requested stop", "not verified") and
  `reclaim_verified == false`.
- **Never-kill protections, each with a test.** PINNED is never reaped
  (`TestMonitor_NeverReapsPinned` — the live 27B is pinned, so this is its direct guarantee);
  in-flight requests are never reaped (`TestMonitor_NeverReapsActiveStream`, which also proves the
  protection is scoped to ACTIVITY: once idle, the same failing health DOES reap, so it is not a
  permanent exemption); STARTING/DRAINING are skipped entirely
  (`TestMonitor_SkipsNonHealthyStates`); a single blip never kills
  (`TestMonitor_UnresponsiveBelowThreshold_NotKilled`) and a recovery resets the counter
  (`TestMonitor_HealthySuccessResetsCounter`); `ReapAfterFailures=0` disables killing while the
  PID-gone path still fires (`TestMonitor_ReapDisabled`).
- **M5.5 reclaim honesty.** `Runner.Stop` now persists `reclaim_verified` (1 verified / 0 unproven /
  NULL never attempted), so a reclaim TIMEOUT or a failed baseline probe is MACHINE-distinguishable from
  a clean stop instead of requiring a consumer to parse prose. The API view and the UI both surface it
  ("reclaim UNVERIFIED"), and `health_failures` renders as "health failing ×N" so a HEALTHY badge cannot
  over-claim.
- **LIVE-VERIFIED 2026-07-25 (healthy path).** After the rebuild + daemon restart, the pinned 27B was
  re-attached by startup reconcile at the SAME pid (13884), and the periodic monitor is demonstrably
  running against it: `last_health_at` advances every tick (observed 14:37:31 → 14:39:16 → 14:42:01 UTC)
  with `health_failures = 0` and `reclaim_verified = NULL` (never stopped). That exercises the
  probe → `RecordHealthOK` → persisted-timestamp path end to end on the real engine, and confirms the
  monitor does NOT disturb a healthy pinned instance. The FAILURE branches remain unit-only.
**M5.GATE — LIVE PASS 2026-07-25** (owner brought the 27B down for the window):
1. Stopped the pinned 27B through the API → `STOPPED`, **`reclaim_verified: true`**, `vram_freed_mib:
   13562`, reason *"stopped and VERIFIED: free VRAM rose by 13562 MiB (>= footprint 13856 − 500 slack)
   … NVML delta, not an HTTP-200 claim"*. VRAM 14569 → 948 MiB used; no orphan process.
2. Started a disposable llama-3.2-1B (HEALTHY, pid 28380), then **killed its llama-server externally**
   so LocalHub was never told.
3. At t+0s the API still read `HEALTHY` (the monitor had not ticked). **At t+4s it read `FAILED`** —
   well inside the documented 15 s interval, and **with no LocalHub restart** — with
   `reclaim_verified: false` and the full reason: *"process exited AFTER reaching HEALTHY: PID 28380 is
   gone (detected by the periodic monitor). This was not a requested stop — a requested stop passes
   through DRAINING — so VRAM reclaim was NOT verified by an NVML delta."*
4. No orphan process; VRAM returned to 947 MiB used.

That closes the gate's core criterion (post-HEALTHY death → truthful terminal state within the
documented interval, no restart) and the reclaim-verified distinction in BOTH directions (`true` on the
clean stop, `false` on the unobserved death). The reap-after-N branch, the pinned/active-stream
protections, and the reclaim-TIMEOUT branch remain unit-only — a timeout needs a GPU that refuses to
release VRAM, which cannot be induced on demand.

**M5.3 stays deliberately unimplemented, now with evidence.** With MTP actually running, the engine log
for the instance contained **zero** lines matching `draft|spec|mtp|nextn|accept` (the log was empty of
any such signal). There is no stable acceptance signal to scrape on this build, so nothing is promised
— exactly as the criterion requires.

## M6

Criteria: a model-specific default is saved ONLY after the lower-context live gates pass,
visibly tied to the pinned artifact + engine build; engine/compatibility/API/data-model
(+migration notes)/engine-version-policy docs updated; live acceptance recorded (requests,
flags, timings, VRAM pred/obs/recovered, transitions, cleanup). Context climbs
progressively; 64K is the final target, attempted only after prior tiers pass.

**M6.GATE:** classify as `LIVE PASS`, `VERIFIED REFUSAL`, or `BLOCKED` with a reason.
`LIVE PASS` requires managed start/infer/monitor/stop/restart/queue evidence.

STATUS 2026-07-25: `LIVE PASS` for managed start → HEALTHY → infer (stream + non-stream)
→ drain → stop → NVML-verified reclaim **at 8K with q4_0 KV and no MTP**; `BLOCKED` for the
MTP + 64K q8/q4 profile pending M3.7/M3.6/M3.x and M4. Queue-replay evidence (M6.6) still
missing, so the gate stays open.
