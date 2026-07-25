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

## P62

Criteria: submit async job → id returned immediately; result retrievable by poll AND
delivered over WebSocket subscription; cancellation works mid-queue and mid-inference
(stream aborted server-side, instance not killed); daemon restart with queued jobs →
jobs still present and runnable; all failure states carry reasons. Integration test
against a real small llama.cpp model within safeguards.

## P63

Criteria: JSON-RPC 2.0 compliance (id echo, error codes, batch requests) verified by
tests; method coverage mirrors REST core (hardware/engines/models/compat/instances/
residency/jobs); works over both HTTP POST and WebSocket; no business logic duplicated
(code review confirms thin-transport rule from PLAN.md decision 7).

## C1

Criteria: five master success criteria walked and evidenced on this box; docs/18
updated (dated); changelog + known-limitations written; final code-review sweep clean of
blocking findings; all phase gates PASS recorded.

## C2

MANUAL(user): git init + initial commit; phone dashboard confirmation; sign-off on
PLAN.md#decisions (ADR-006 Go, vLLM strategy, Phase 0 GO, model budget).
