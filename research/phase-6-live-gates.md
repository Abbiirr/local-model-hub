# Phase 6 Live Gate Evidence — #p61 / #p62 / #p63

Integrator (Phase 6) live verification of the wired proxy (P6.1), async jobs (P6.2), and JSON-RPC
2.0 surface (P6.3) against a real small llama.cpp model, within PLAN.md hardware safeguards.

- **Date:** 2026-07-07, ~06:34–06:45 +06:00 (Asia/Dhaka)
- **Box:** RTX 4060 Ti 16 GB (driver 576.40), AMD Ryzen 5 2600, 16 GB system RAM, Windows 10.
- **Full suite before live checks:** `go vet ./...` OK · `go build ./...` OK · `go test ./... -count=1` OK (exit 0, no failures).

## Hardware safeguard pre-flight (BINDING, PLAN.md)

| Check | Value | Requirement | Result |
|---|---|---|---|
| Free system RAM (start) | **8.5 GB** (8,910,448 KB) | ≥ 2.5 GB | PASS — live checks proceed |
| VRAM baseline (`nvidia-smi memory.used`) | **754 MiB** | record | baseline set (only desktop/UI GPU apps; no model procs) |
| llama-server binary | `K:\projects\llm_models\llama.cpp-tq3\build\bin\llama-server.exe` present (8.4 MB) + winget `llama-server` on PATH | verify exists | PASS |
| Model | `Llama-3.2-1B-Instruct-Q4_K_M.gguf` present in `K:\projects\llm_models\models` | small GGUF from inventory | PASS |
| Ollama | `GET http://127.0.0.1:11434/api/version` → **HTTP 000 (connection refused)** | check up | **DOWN** — Ollama routing checks SKIPPED (nothing heavy started) |

No `wsl --shutdown`, no user-process kills were performed. vLLM + Ollama disabled in scratch cfg.

### Scratch config summary (all artifacts under session scratchpad, NOT C:\ root, NOT the repo)

- Binary: `<scratchpad>\localhub.exe` (`go build -o … ./cmd/localhub`)
- Config: `<scratchpad>\cfg.yaml` — port **39876**, db `<scratchpad>\localhub.db`, gguf scan `K:\projects\llm_models\models`, llamacpp enabled (winget + tq3-fork paths), **ollama + vllm disabled**, residency enabled `vram_budget_mib: 3000`, `drain_interval_seconds: 5`, `jobs.enabled: true`, no admin_token.
- WS test client: `<scratchpad>\wsclient.exe` (coder/websocket v1.8.15), logs under `<scratchpad>\logs\`.

Model id used throughout: `gguf-llama-3-2-1b-instruct-llama-3-2-1b-instruct-q4-k-m`.

---

## #p61 — Thin OpenAI proxy

### a. GET /v1/models — OpenAI list shape
`object:"list"`, `data_count=10`, every row `{id, object:"model", created:<unix>, owned_by:"localhub"}`.
Sample: `{"id":"gguf-glm-ocr-glm-ocr-q8-0","object":"model","created":1783384491,"owned_by":"localhub"}`.
10 llama.cpp GGUF models present incl. the target. **Ollama model presence: SKIP** (Ollama daemon down → 0 ollama rows, expected). **Result: PASS (llama.cpp half); Ollama assertion SKIP.**

### b. POST /v1/chat/completions — cold start → stream, then warm fast path
- **Cold (stream:true, max_tokens 64):** HTTP **200**; **12** SSE `data:` chunks; `data: [DONE]` present (count 1).
  Chunks are raw llama-server SSE passthrough (native shape incl. `timings` block preserved — no re-serialization).
  First chunk: `data: {"choices":[{"finish_reason":null,...,"delta":{"role":"assistant",...}}],...,"object":"chat.completion.chunk"}`.
  Last: `finish_reason":"stop"` then `data: [DONE]`. Daemon logged `decision:"load_new"` then `duration_ms:18480` (real cold-start load).
- **Warm (2nd request):** HTTP 200, **1617 ms** wall (daemon `duration_ms:1525`), 35 chunks + [DONE] — no cold-start wait, instance already HEALTHY. VRAM during: **2188 MiB** (≈1434 MiB over baseline; <2 GB). One instance resident (≤2 rule honored).
- **Result: PASS.**

### c. Ollama routing — **SKIP** (Ollama daemon down; nothing heavy started per safeguard).

### d. Blocked admission → OpenAI-shaped 503 with numeric blocked_reason
Requested un-fittable 32B (`gguf-qwen3-vl-32b-instruct-q2k-…`) and 8B under the 3000 MiB budget.
- 503 body is correct (OpenAI shape, numeric reason preserved):
  `{"error":{"message":"waiting for VRAM: need 15235 MiB (footprint 14211 + 1024 margin), free 3000 MiB, reclaimable 0 MiB from 0 idle instance(s) — 12235 MiB short","type":"insufficient_vram","code":"admission_blocked","param":null}}`
- **Wall-clock: ~10.1–12.5 s across 4 trials** (32B and 8B alike; with and without an idle instance to reclaim). **This EXCEEDS the <2 s gate.**
- **Root cause (from daemon log):** the residency `decision:"queue"` is logged at the *end* of the request, immediately before the 503 — i.e. admission itself is instant. The ~10 s is spent *before* Admit, inside `StartInstance`'s synchronous **compat-verdict gate** (GGUF metadata read + VRAM estimate) that runs on the proxy resolve path. It is size-independent (8B and 32B both ~10 s) and repeats each call (no proxy-side verdict cache). This is pre-existing P5 `StartInstance` behavior surfaced by the proxy, **not** introduced by the P6 wiring.
- **Result: PARTIAL — 503 status + OpenAI shape + numeric reason all CORRECT; the <2 s wall-clock criterion is NOT met (~10 s) due to the synchronous compat estimate ahead of admission.** Flagged for orchestrator (see Blocking/deviations).

### e. Auth spot-check — N/A (no admin_token in scratch cfg, by instruction).

### grep audit (no budgets/keys/fallback/multi-provider) — covered by Agent A's `internal/proxy` unit gate; not re-run live.

---

## #p62 — Async inference jobs (real llama.cpp)

### a. Submit → id immediately
`POST /api/jobs {model, messages, max_tokens:32}` → HTTP **201**, `{"id":…,"state":"queued","priority":1,"endpoint":"chat",…}`, **submit wall 102 ms** (<1 s). **PASS.**

### b. Result by poll AND over WS
- **Poll:** `GET /api/jobs/{id}` reached **`succeeded`**; `result_json` **non-null** — full OpenAI non-streaming body: `{"choices":[{"finish_reason":"stop",…,"message":{"role":"assistant","content":"Here are three colors:\n\n1. Red\n2. Blue\n3. Yellow"}}],…}`. `instance_id` populated.
- **WS (pre-connected, then submit):** captured `jobs.updated` frames **queued → running → succeeded** for the same job id over `/api/ws`, with push frames (`hardware.snapshot`) flowing alongside.
- **Result: PASS.**

### c. Cancel mid-queue
Submitted a budget-blocked 32B job (stays queued); `DELETE /api/jobs/{id}` → HTTP **200**, `state:"cancelled"`, `reason:"cancelled by user before admission"` (non-empty). Job transitioned queued→cancelled. **PASS.**
Note: this job's own `queue_id` was empty at cancel time (worker had not yet recorded a residency queue row for it); the durable `jobs` row is authoritative and was cancelled correctly. Unrelated residency `queue` rows left by the #p61d proxy-block trials and 32B re-attempts persisted (cosmetic; they belong to no live waiter).

### d. Restart persistence + running→failed reconcile
Submitted a budget-blocked job (→ queued) and injected a synthetic `running` row (a real 1B finishes in <1 s, so timing a hard-kill mid-run is impractical — the running→failed path is also Agent-A unit-tested). Hard-killed the daemon + llama-server (`Stop-Process -Force`), VRAM returned to **776 MiB**, then restarted with the **same** scratch DB:
- Queued job **still `queued`** after restart (durable — lmwrapper persistence lesson satisfied).
- Injected running job reconciled to **`failed`**, `error:"daemon_restart"`, `reason:"daemon restarted mid-inference"` — exactly design §2.6. `ReconcileJobs` ran at boot before the worker.
- **Result: PASS** (queued-persistence proven live; running→failed proven via the reconcile path with a DB-seeded running row, recorded honestly).

All failed/cancelled rows carried non-empty `reason` (mandatory-reason gate).

---

## #p63 — JSON-RPC 2.0 surface

### a. POST /api/rpc engines.list
HTTP **200**, `{"jsonrpc":"2.0","id":1,…}` — `id:1` echoed (int type preserved), `result` a list of engine statuses. **PASS.**

### b. WS parity + notification + non-RPC + push liveness
- **engines.list over WS:** `RPC_RESPONSE {"jsonrpc":"2.0","result":[…4 engines…],"id":1}` — same structure/values as the HTTP reply (shared dispatcher → parity confirmed).
- **Notification (no id):** **no** jsonrpc response frame within 2 s; push frames (hardware.snapshot, queue.update, instances.updated) kept flowing → `NO_RPC_RESPONSE_OK`.
- **Non-RPC frame `{"type":"noise"}`:** ignored, no response, connection stayed up, pushes flowed.
- **Result: PASS.**

### c. jobs.submit via JSON-RPC (proves the shim)
`POST /api/rpc {method:"jobs.submit", id:"rpc-sub-1", params:{model, messages, max_tokens}}` → `id:"rpc-sub-1"` (string type preserved), `result:{id:<uuid>, state:"queued", model_id:<target>}`. **Proves `coreJobService` shim** decodes params correctly and stores them verbatim as RequestJSON. **PASS.**

### d. Batch over HTTP
`[engines.list(id1), hardware.get(id2), engines.detect(notification)]` → HTTP 200, array of **exactly 2** responses, ids `[1,2]`, both with `result`; the id-less notification produced no response. **PASS.**

### bonus error codes
`does.not.exist` → `-32601` "method not found: does.not.exist". Malformed JSON body → HTTP **400** with `-32700`. Matches design §3.5. **PASS.**

---

## Teardown proof

| Item | Value |
|---|---|
| Instances deleted via API | all (final HEALTHY instance DELETEd → 202/DRAINING) |
| Daemon + llama-server | `Stop-Process -Force` (killed localhub + llama-server) |
| VRAM final | **754 MiB = baseline** (delta **0 MiB**, within ±200) |
| Orphan check | `Get-Process localhub,llama-server` → **NO_ORPHANS_CONFIRMED** |
| Free RAM (end) | 8.25 GB |

Scratch artifacts (binary, db, cfg, wsclient, logs) remain under the session scratchpad only; none written to C:\ root or the repo.

---

## Gate summary

| Gate | Verdict | One-line evidence |
|---|---|---|
| #p61a /v1/models | PASS (Ollama half SKIP) | OpenAI list shape, 10 GGUF models; Ollama down → 0 ollama rows |
| #p61b chat/completions stream + warm | PASS | 200, 12 chunks + [DONE] cold; warm 1617 ms, 35 chunks |
| #p61c Ollama routing | SKIP | Ollama daemon down (HTTP 000); nothing heavy started |
| #p61d blocked → fast 503 | **PARTIAL** | 503 + OpenAI shape + numeric reason CORRECT; wall ~10 s > 2 s (synchronous compat estimate before admission) |
| #p62a submit → id immediately | PASS | 201 {id, queued} in 102 ms |
| #p62b poll + WS result | PASS | succeeded + non-null result_json; WS queued→running→succeeded |
| #p62c cancel mid-queue | PASS | 200 cancelled, reason "cancelled by user before admission" |
| #p62d restart persistence + reconcile | PASS | queued job survives; running→failed "daemon restarted mid-inference" |
| #p63a rpc engines.list HTTP | PASS | 200, id echoed, result list |
| #p63b WS parity/notification/non-RPC/push | PASS | WS result parity; notification no-response; non-RPC ignored; pushes flow |
| #p63c jobs.submit via RPC | PASS | string id echoed, job queued (shim proven) |
| #p63d batch HTTP | PASS | 3-elem batch → exactly 2 responses [1,2]; -32601/-32700 confirmed |

**Blocking for gate closure:** #p61d's **<2 s** criterion is not met (~10 s) because `StartInstance`'s synchronous compat-verdict computation runs on the proxy resolve path before admission. The 503 contract (status, OpenAI shape, numeric blocked_reason) is fully satisfied. This is pre-existing P5 behavior, not a P6 wiring defect. Orchestrator decision needed: waive the timing sub-criterion, or require a proxy-side fast-path that short-circuits admission for an already-blocked model (e.g. cache/skip the verdict when the free-VRAM read alone proves the model cannot fit).

---

## Amendment (fix agent) — 2026-07-07

Closes the two open #p61 items from the gate summary: **#p61d** (blocked-admission latency)
and **#p61c** (Ollama routing, previously SKIPPED). All measurements below are on the reference
box (RTX 4060 Ti 16 GB, HDD models, Windows 10) via a scratch daemon in the session scratchpad
(port 39901, scratch DB, `residency.vram_budget_mib=3000`), torn down after.

### Item 1 (#p61d) — root cause + fix

**Measured root cause.** The ~10 s was NOT a timeout — it was `gguf-parser-go`'s
`ParseGGUFFile` doing buffered file reads, at a **size-independent ~2.3–2.9 s per parse**, called
**4× per `Engine.Evaluate`** (once at the requested ctx + three `ctxCandidates` 4096/16384/32768
in `ggufMaxContext`). `StartInstance` runs that `Evaluate` synchronously before residency Admit on
the proxy path, so 4 × ~2.7 s ≈ the observed 10.1–12.5 s. Per-call measurements (32B Q2_K):

| read options | one parse | VRAM estimate |
|---|---|---|
| default (buffered) | 2.33 s | 14211 MiB |
| `SkipLargeMetadata()` | 1.12 s | 14211 MiB |
| `UseMMap()` | 0.14 s | 14211 MiB |
| `UseMMap()`+`SkipLargeMetadata()` | **0.031 s** | **14211 MiB** |

The VRAM number is **byte-identical** across all options and every ctx candidate
(4096/8192/16384/32768) for 1B/8B/32B — verified by a scratch correctness harness. TQ3_* still
fails to parse with the same `GGMLType(46)` error, so the file-size fallback path is unchanged.

**Fix (smallest correct).** `internal/compat/gguf/parse.go`: add `UseMMap()` + `SkipLargeMetadata()`
to the **local-file** parse only (`localReadOptions`). Remote (HF/URL) range-reads are untouched
(no mmap; their `RemoteTimeout` is the byte cap). No cache, no short-circuit — the estimator itself
is now fast, so the FIRST-ever request is fast too. Conservative estimates and the mandatory reason
are fully preserved (identical output).

**Unit evidence.**
- `internal/compat/gguf/realfile_test.go::TestRealFile_LocalParseIsFast` — one local parse+estimate
  = 17 ms (< 2 s ceiling trip-wire).
- `internal/compat/evaluate_latency_test.go::TestRealFile_EvaluateUnder2s` — full `Engine.Evaluate`
  over the real 32B (the 4×-parse path) = **170 ms**, status `wont_run`, est 14211 MiB (unchanged).
- Existing `TestRealFile_TQ3_Fallback` / `TestRealFile_GLMOCR_Parses` stay green (fallback + VRAM
  band unchanged).

**Live re-measure** (POST /v1/chat/completions, `vram_budget_mib=3000`, ≥3 trials incl. first call):

| Model | Trial 1 (cold/first) | Trial 2 | Trial 3 | Trial 4 | Gate |
|---|---|---|---|---|---|
| 32B Qwen3-VL Q2_K | **1.539 s** | 1.366 s | 1.362 s | 1.448 s | PASS (< 2 s) |
| 8B Qwen3-VL Q6_K  | **1.503 s** | 1.393 s | 1.377 s | 1.380 s | PASS (< 2 s) |

503 body verbatim (OpenAI shape + numeric reason preserved):
`{"error":{"message":"waiting for VRAM: need 15235 MiB (footprint 14211 + 1024 margin), free 3000 MiB, reclaimable 0 MiB from 0 idle instance(s) — 12235 MiB short","type":"insufficient_vram","code":"admission_blocked",...}}`
Server-side `duration_ms` 1358–1533. The residual ~1.4 s is the hardware snapshot (nvidia-smi
poll) + engine-availability sweep; the estimator is no longer the bottleneck. **#p61d: PASS.**

### Item 2 (#p61c) — Ollama routing, live

Ollama was DOWN before this session (baseline). Started `ollama serve` (v0.21.2) in the scratchpad;
`ollama list` had no chat model ≤2 GB (smallest chat-capable was `glm-ocr:latest` 2.22 GB, and
`sam860/deepseek-r1-0528-qwen3:8b` 5.03 GB). **No model was pulled/downloaded.**

Three routing defects surfaced (all pre-existing P4.3/P6.1 gaps the previously-SKIPPED check never
exercised), each fixed minimally:

1. **Engine not inferred from source** — `ResolveForProxy` passes `Engine:""`, and `StartInstance`
   defaulted to llama.cpp, hitting "no local file on disk to launch" (502) for an ollama row. Fix:
   `internal/core/instances.go::engineForModelSource` routes `source=ollama` → the Ollama warm path.
2. **Compat verdict "unknown" for Ollama** — the ollama row has no local GGUF, so the estimator
   returned unknown → REFUSED. Fix: `internal/core/ollama_instances.go` substitutes a **conservative**
   size-based estimate (`size_bytes × 1.08`) for the unknown case only; the real footprint is still
   re-measured from `/api/ps size_vram` at HEALTHY (no false "runs").
3. **HEALTHY→HEALTHY race + model-name 404** — the periodic `/api/ps` mirror could adopt the warming
   row to HEALTHY first (illegal-transition 502); and the relay sent LocalHub's catalog id to Ollama,
   which routes by tag (404). Fixes: `internal/engines/ollama/runner.go` makes the warm's final
   HEALTHY transition idempotent to a mirror-won race; `internal/core/proxy.go` +
   `internal/api/proxy.go` add `InstanceRequestHandle.UpstreamModel` (the Ollama tag) and rewrite the
   relayed body's `model` field ONLY for Ollama (llama.cpp/vLLM relay unchanged, byte-identical).

`glm-ocr:latest` fails to load **directly via Ollama** (`GGML_ASSERT(ggml_nbytes(src0) <= INT_MAX)`
— an Ollama/ggml bug for that glmocr F16 model, not a LocalHub issue), so the routing 200 was proven
with `sam860/deepseek-r1-0528-qwen3:8b`, which loads directly (200) and via LocalHub:

POST /v1/chat/completions {model:"ollama-sam860-deepseek-r1-0528-qwen3-8b", max_tokens:24} →
**HTTP 200**, verbatim:
`{"object":"chat.completion","model":"sam860/deepseek-r1-0528-qwen3:8b","system_fingerprint":"fp_ollama","choices":[{"message":{"role":"assistant","content":"<think>..."},"finish_reason":"length"}],"usage":{"completion_tokens":24,...}}`
Note `model` correctly rewritten to the Ollama tag; `fp_ollama` confirms Ollama served it. Trials
2+3 also 200 (~9.7 s, warm-hit; time is generation, not load). **#p61c: PASS.**

**Unit evidence for Item 2:** `TestStartOllamaInstance_EngineInferredFromSource`,
`TestBeginInstanceRequest_UpstreamModel`, `TestRunner_WarmIdempotentWhenMirrorRaces`.

### Suite + teardown

`go vet ./...`, `go build ./...`, `go test ./... -count=1` — all clean, **0 failures**. Teardown:
scratch daemon stopped (port 39901 closed), all instances STOPPED, Ollama process stopped and port
11434 closed (DOWN baseline restored), no orphan localhub/ollama/llama-server processes, GPU VRAM
770 MiB used vs 754–780 baseline (within ±200).

### Updated gate summary

| Gate | Verdict | Evidence |
|---|---|---|
| #p61c Ollama routing | **PASS** | 200 via `sam860/deepseek-r1:8b`, model rewritten to tag, `fp_ollama` |
| #p61d blocked → fast 503 | **PASS** | 1.36–1.54 s over 4 trials incl. first call; root cause = 4× buffered GGUF parse, fixed by local mmap+skip-large-metadata |
