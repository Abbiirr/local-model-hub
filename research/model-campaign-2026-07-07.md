# LocalHub Model Campaign — 2026-07-07

Proving LocalHub against six real models. Product promise under test: *"which local models
run on this one box, at what flags, with honest reasons."* A correct refusal (won't fit /
unsupported format / over disk budget) is a PASS for the system.

## Environment / baseline

- Box: RTX 4060 Ti 16 GB (16380 MiB), 16 GB system RAM, Windows 10.
- Scratch daemon: `localhub-camp.exe` (built from repo HEAD, no code changes), port 39917,
  fresh db `campaign.db`, `vram_budget_mib 12000`, `min_free_ram_mib 2048`, ollama enabled,
  vllm enabled (docker), llama.cpp enabled. Downloads land in `K:\projects\llm_models\models`.
- Config: `scratchpad/camp/cfg-camp.yaml`.

### Baseline resources (before campaign)

| Resource | Value |
|---|---|
| VRAM free | 12580 MiB (12764–12965 MiB across probes; ~3530 MiB used by desktop) |
| System RAM free | 3737 MiB at start, hovering 2200–2450 MiB during work (RAM binds first — KNOWN-LIMITATIONS #2) |
| K: free disk | 47.62 GiB (probe reported 48 GiB) — NOTE: brief said 21.6 GB; actual is far more |
| C: free disk | 2.43 GiB (nothing written to C:) |

### Guards in effect (from `internal/manifest/guards.go`, verified in code)

- **HardCapBytes = 8 GiB** — absolute per-download ceiling; any single download > 8 GiB
  refused outright regardless of free disk. (Brief mentioned 12 GB; the shipped code cap is
  8 GiB — the tighter cap governs.)
- **DiskHeadroomBytes = 5 GiB** — must remain free after a download.
- `min_free_ram_mib = 2048` RAM floor enforced at admission (all engines).

### Engines detected by the daemon

- llama.cpp: TWO binaries — CUDA fork `1cfa891` (has_tq3, CUDA backend) and WinGet Vulkan
  build `8833`. CUDA fork is the one used for GPU offload.
- ollama: installed at `K:\Softwares\ollama\ollama.exe`, v0.21.2 (I started `ollama serve`
  for the campaign; restored DOWN at teardown).
- vLLM: docker mode; images `vllm/vllm-omni:latest`, `vllm/vllm-openai:latest` present.

---

## The Six

### 1. gemma-4-12B GGUF via llama.cpp — target quant Q4_K_M

HF repo `yuxinlu1/gemma-4-12B-agentic-fable5-composer2.5-v2-3.5x-tau2-GGUF`. File sizes (HF API `/tree/main`):

| Quant | Size | Fits 8 GiB guard? |
|---|---|---|
| Q3_K_M | 5.67 GiB | yes |
| **Q4_K_M** | **6.87 GiB** | **yes (target — largest that fits guard)** |
| Q6_K | 9.11 GiB | no (> 8 GiB cap) |
| Q8_0 | 11.80 GiB | no (> 8 GiB cap) |

**Compat preview BEFORE download (the money feature):**

- Q4_K_M @ 8192 ctx → `runs_degraded`: *"does not fit at the requested context in 11816 MiB
  usable (16380 total × 0.92 safety − 3536 in use), but fits at a reduced 4K ctx — runs
  degraded at the smaller context."* est_vram 14230 MiB, max_context_that_fits 4096.
- Q4_K_M @ 4096 ctx → `runs`: *"GGUF needs ~11150 MiB at 4K ctx and fits in 11830 MiB usable
  (16380 total × 0.92 safety − 3521 in use), with headroom."* est_vram 11150 MiB,
  flags `-ngl all -c 4096 --fit off -fa on`.
- Estimator: `gguf-parser-go@v0.24.1`, confidence approximate.

Q4_K_M is the largest quant that fits BOTH the download guard AND a runs/runs_degraded
verdict → selected.

**Download (two-step HF via LocalHub `/api/downloads`):**

- Resolve (no confirm) → `awaiting_ok`, HTTP 202: pinned commit `190a31365a`, license
  apache-2.0, `gemma4-v2-Q4_K_M.gguf` selected, total 7381381664 bytes (6.87 GiB). Reason:
  *"resolved …@main → commit 190a31365a; 9 file(s), 7039.4 MiB. Confirm to download."*
- Confirm → HTTP 202, state running. (In flight; details below once landed.)

**Download final stats:** completed 2026-07-08 00:16 local (started 21:51 local 07-07) —
**~2 h 25 m** at HF-throttled ~0.8 MB/s, exactly 7,381,381,664 bytes, no `.part` remnant.
Manifest row verified (`GET /api/models/{id}/manifest`): id `c25d12f4…`, source hf, revision
pinned `190a31365a…`, quantization q4_k_m, license apache-2.0, **sha256
`0b9506cab36f7f818e34f9c0f5a3d6568d0b37100f3a3e1092e2eec3c4c96791`**, downloaded_at
2026-07-07T15:51:05Z.

**Finale (2026-07-08, fresh session):** local verdict after rescan: `runs` at **both** 4K
(est 8078 MiB) and 8K (est 8086 MiB, max_context_that_fits 32768) — the full local GGUF parse
estimates ~8 GiB vs the remote metadata preview's 14230 MiB @8K (remote preview over-estimated;
honest delta worth knowing). Run at 4K:

- `POST /api/instances` → HTTP 201 **HEALTHY in 45 s** cold load (02:48:39→02:49:24Z), PID 2452,
  port 40100, CUDA fork binary, flags `-ngl all -c 4096 --fit off -fa on`.
- **Full-GPU verification (llama-server log, verbatim):** `load_tensors: offloaded 49/49
  layers to GPU`; `CUDA0 model buffer size = 7024.41 MiB` (the 787.50 MiB `CPU_Mapped` buffer
  is the mmap'd file view, not compute offload).
- **VRAM:** before 711 MiB used → resident/peak **9746–9754 MiB used** (delta ~9040) → after
  stop 718 MiB. GPU util 89% during generation.
- **Completion (real inference):** prompt "Say hello in five words. Answer immediately without
  deliberation.", max_tokens 64 → **`"Hello, hello, how are you?"`** (finish_reason `stop`,
  29.2 tok/s decode; the model is a reasoning fine-tune — first attempt at max_tokens 48 spent
  all 48 tokens in `reasoning_content` and hit `length`, itself coherent generation proof).
- **UNLOAD:** `DELETE /api/instances/{id}` → STOPPED, *"stopped and VERIFIED: free VRAM rose by
  9041 MiB (>= footprint 9039 − 500 slack) — process gone and VRAM reclaimed (NVML delta, not
  an HTTP-200 claim)."*
- **PRODUCT BUG FOUND (proxy path):** the completion had to go through the counted per-instance
  relay (`POST /api/instances/{id}/completions`, HTTP 200) because **`/v1/chat/completions`
  503'd against the resident, HEALTHY gemma**: *"REFUSED: compatibility verdict is wont_run …
  needs ~9039 MiB at 8K ctx but only 5859 MiB usable (16380 total × 0.92 safety − 10012 in
  use)"*. Two defects compound: (a) the proxy resolve path recomputes the verdict at the
  **default 8K ctx**, not the warm instance's actual 4K; (b) the estimate counts the requested
  model's **own resident footprint as "in use"** (self-shadowing), so any large warm model
  fails its own admission. Small models pass (fit even when double-counted) — which is why the
  llama-1b `/v1` completion worked. Related to KNOWN-LIMITATIONS #10 (verdict on the resolve
  path) but distinct: this blocks warm routing entirely for large models. Not hot-fixed
  (campaign rule); reported as a finding. Same signature seen in the Phase-2 qwen-8b stream
  attempt.

**Verdict: PASS** — verdict-before-download, guarded download with pinned manifest + sha256,
45 s cold load, full GPU offload, real completion, NVML-verified unload.

### 2. nvidia/nemotron-3.5-asr-streaming-0.6b — HONEST VERDICT (refused correctly)

- Format (HF API): safetensors `model.safetensors` (2.377 GiB) + `nemotron-3.5-asr-streaming-0.6b.nemo`
  (2.206 GiB) + processor_config.json → a **NeMo streaming-ASR encoder**, not a causal LM.
  Total repo 4.58 GiB.
- Compat preview → `status: unknown`, reason: *"could not read a usable config.json for this
  safetensors model (missing num_hidden_layers / dimensions), so the VRAM estimate cannot be
  computed. Ensure config.json is present (local) or reachable (remote HF), then retry."*
  recommended_engine vllm, est_vram 0.
- **Verdict: REFUSED-CORRECTLY.** The ASR model's config.json is not a standard causal-LM
  config, so the estimator honestly declines to size it rather than guessing. No artifacts
  downloaded. This is exactly the honest format-routing behavior the promise requires.

### 3. baidu/Unlimited-OCR — VERDICT captured

- Format (HF API): safetensors `model-00001-of-000001.safetensors` (6.214 GiB) + custom code
  `modeling_unlimitedocr.py`, `modeling_deepseekv2.py`, `deepencoder.py`, `configuration_deepseek_v2.py`
  → a **DeepSeek-V2-based vision/OCR multimodal model** requiring trust_remote_code. Total 6.22 GiB.
- Compat preview → `status: runs_degraded` via vLLM: *"needs ~8891 MiB (weights 6363 + KV 480
  + overhead 2048) at 8K ctx and fits in 11921 MiB usable (16380 total × 0.92 safety), but
  vLLM here runs in DOCKER mode — expect a slow cold start … Runs, degraded by cold-start
  latency."* est_vram 8891, max_context_that_fits 32768, confidence **exact**
  (localhub-safetensors estimator), flags `--gpu-memory-utilization 0.90 --max-model-len 8192 --dtype auto`.
  Warning: docker cold start slow.
- **Verdict recorded.** The estimator sized it (readable deepseek-v2 config) and says
  runs_degraded on vLLM/docker. NOT run: it is a vision-OCR multimodal model with custom
  `trust_remote_code`, not a plain text LM; running it is outside the OpenAI-chat proxy path
  and outside campaign budgets (would need the slow vLLM docker cold start + custom code
  trust). The honest verdict is the deliverable. No artifacts downloaded.

### 4. nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B-BF16 — REFUSED at BOTH layers

- Format/size (HF API): 13 BF16 safetensors shards, total **58.84 GiB** (63174962335 bytes),
  confirms the ~60 GB expectation.
- **VRAM layer** — compat preview → `wont_run`: *"weights+overhead need ~62278 MiB, which
  exceeds 97% of the 16380 MiB device even at max gpu-memory-utilization; won't run on vLLM.
  Use a smaller quant (AWQ/GPTQ), tensor-parallel across GPUs, or a GGUF via llama.cpp."*
  est_vram 62694, confidence exact.
- **Disk layer** — download resolve → `awaiting_ok` (metadata only, no bytes), 35 files,
  60248.0 MiB, commit `cbd3fa9f93`. On **confirm** → **HTTP 400 `download_refused`**:
  *"refusing download of 58.84 GiB: exceeds the 8.00 GiB hard cap for this box (PLAN.md
  safeguard — disk is nearly full)."* Refused BEFORE any bytes fetched.
- **Verdict: REFUSED-CORRECTLY at both layers.** The size guard did exactly its job — no
  60 GB download onto a box that can't hold it. No artifacts downloaded.

### 5. ornith via Ollama — REFUSED by engine version gate (honest propagation)

- Library page (ollama.com/library/ornith): variants `ornith:9b` 5.6 GB, `ornith:latest`
  5.6 GB, `ornith:35b` 21 GB. Smallest ≤ 12 GB → `ornith:9b` selected.
- Started `ollama serve` (v0.21.2). Pull `ornith:9b` via LocalHub `/api/downloads`
  (kind ollama) → HTTP 202 running, then **failed** (start 15:52:51Z, failed 15:52:53Z, ~2 s,
  before any bytes): reason *"ollama pull \"ornith:9b\" failed: pull model manifest: 412:
  The model you are attempting to pull requires a newer version of Ollama. Please download the
  latest version at: https://ollama.com/download"*.
- **Verdict: REFUSED-CORRECTLY.** ornith requires a newer Ollama than the installed 0.21.2;
  the engine's own 412 manifest gate rejected it and LocalHub faithfully surfaced the
  human-readable reason (did not swallow it). Per campaign constraints I did NOT hack in
  support or upgrade the engine — the honest reason IS the answer. No completion possible.

### 6. vLLM + gemma GGUF — experimental path verdict

vLLM+GGUF is the experimental path ADR-004 routes away from. *(Captured in Phase 1 via a
StartInstance with Engine:"vllm" on the downloaded GGUF — see below.)*

---

## Residency mechanics drill (Directive 2 — Phase 2)

Run on a second scratch daemon (port 39918, `phase2.db`, `vram_budget_mib 9000`, min_free_ram
2048) so it wouldn't interrupt the gemma download on daemon-1. Real models used: **qwen3-vl-
embedding-8b (Q6_K, ~7123 MiB, 37/37 layers GPU)** as the big model, **llama-3.2-1b (Q4_K_M,
~1411 MiB, 17/17 layers GPU)** as the light model. (gemma's own live run is the Phase-1 finale
once its download lands; the mechanics themselves are proven here with real large GGUFs.)

### Test 0 — one real llama.cpp run + completion (done first, on daemon-1, budget 12000)

- llama-3.2-1b started via `POST /api/instances` → HEALTHY in ~7 s, PID 32064, port 40100.
- **Full-GPU verification:** llama-server log `load_tensors: offloaded 17/17 layers to GPU`;
  runner passed `-ngl all -c 4096 --fit off -fa on`. (The 205 MiB `CPU_Mapped` buffer is the
  mmap'd file header, not compute offload — all compute tensors in CUDA0.)
- Completion via `/v1/chat/completions` (proxy, counted path), prompt "Say hello in five
  words.", max_tokens 32 → **`"Hello, how are you?"`** (HTTP 200). nvidia-smi during gen:
  4431 MiB used, 11679 free.
- **UNLOAD (Test 1):** `DELETE /api/instances/{id}` → async DRAINING → STOPPED with
  *"stopped and VERIFIED: free VRAM rose by 1285 MiB (>= footprint 1284 − 500 slack) — process
  gone and VRAM reclaimed (NVML delta, not an HTTP-200 claim)."* VRAM 4430→3142 used. PASS.

### Test 2 — QUEUEING + auto-drain (budget 9000)

- Start qwen-8b **pinned** → HEALTHY, `37/37 layers to GPU`, PID 27512. VRAM 10106 used.
- Request llama-1b → **HTTP 202 queued**, position 1, numeric blocked_reason (surfaced in the
  202 body AND `/api/queue`): *"ram_blocked: free system RAM 1212 MiB below floor 2048 MiB —
  refusing to start a new process/warm until RAM recovers (RAM binds before VRAM on this box…)."*
  → **RAM bound first, not VRAM** — exactly KNOWN-LIMITATIONS #2. PASS (correct numeric refusal).
- **CancelQueueEntry:** works on a stable id (proven with HTTP 200 on the daemon-1 vLLM entry
  later). NOTE/finding: a *RAM-blocked* entry under the 5 s drain loop is re-queued with a NEW
  id each cycle, so an external fetch→DELETE round-trip often 404s the just-superseded id (each
  404 carries a clear reason). Not a correctness bug (the never-admitted entry causes no harm),
  but external cancel of a continuously-re-queued entry is racy on this box.
- **AUTO-DRAIN:** with llama-1b queued, `DELETE` the pinned qwen-8b (frees VRAM+RAM). The drain
  loop **auto-admitted** llama-1b (STARTING→HEALTHY) with **no manual re-POST**; WAITING entry
  cleared. qwen-8b STOPPED VERIFIED (freed 6951 MiB NVML delta). PASS.

### Test 3 — EVICTION (evict_then_load) + never-kill-active

- With idle non-pinned llama-1b resident, request qwen-8b (non-pinned) → HTTP 201 HEALTHY
  directly. Verbatim daemon log:
  - `residency eviction re-read` — `freed_expected_mib:1296, freed_measured_mib:1285,
    free_after_reread_mib:9000`.
  - `decision:"evict_then_load"` — *"evicted 1 idle instance(s) [ba244d67…]; free VRAM re-read
    after each eviction is now 9000 MiB >= footprint 6960 + 1024 margin (7984 MiB) — loading…"*
  - victim llama-1b STOPPED (VERIFIED freed 1285 MiB); new qwen-8b HEALTHY, 37/37 GPU. PASS.
- **never-kill-active:** long streaming completion on qwen-8b via the counted per-instance relay
  (`POST /api/instances/{id}/completions`, max_tokens 2000, stream) — 488 KB SSE, ~2000 tokens,
  nvidia-smi mid-gen **91% GPU util, 10110 MiB used**. Fired a competing llama-1b start
  mid-stream → **HTTP 202 queued (ram_blocked, free RAM 334 MiB)** — NOT satisfied by killing
  the streaming qwen-8b. qwen-8b stayed **HEALTHY** and streamed to completion. PASS.
  - Caveat: on this run the RAM floor bound *before* the eviction victim-selection stage, so the
    never-kill was enforced by the RAM guard here; the eviction path's own idle-only victim
    selection was proven separately in Test 3 part 1 (it picked the idle llama-1b, and would
    never pick an ActiveCount>0 instance). Both guards held.

**Phase 2 verdict: all residency mechanics PASS** — unload (NVML-verified), queue (numeric
reason + position), CancelQueueEntry, auto-drain (no re-POST), evict_then_load (freed_measured),
never-kill-active. RAM-binds-first is the dominant gate on this 16 GB box (KNOWN-LIMITATIONS #2),
faithfully reproduced. All VRAM reclaims verified by NVML delta, not HTTP 200.

### #6 vLLM + GGUF — the honest experimental-path chain

- GGUF compat routing (`/api/models/{gguf}/compat`) → `recommended_engine: llamacpp`,
  `engine_choices: [llamacpp]` — the estimator routes GGUF to llama.cpp only (ADR-004). vLLM is
  never *offered* for a GGUF.
- Forcing it (`POST /api/instances {engine:"vllm"}` on the local llama-1b GGUF) → **HTTP 202
  queued**: *"waiting for VRAM: need 15766 MiB (footprint 14742 + 1024 margin), free 11660 MiB,
  reclaimable 0 MiB … — 4106 MiB short."* The vLLM footprint for a 0.75 GB GGUF is **14742 MiB**
  because vLLM pre-allocates `gpu-memory-utilization × TOTAL VRAM` regardless of model size
  (KNOWN-LIMITATIONS #3). It exceeds the 16 GB card → refused/queued. **No docker container was
  started** (verified `docker ps`); the queue entry was cancelled (CancelQueueEntry → HTTP 200).
- **Verdict: REFUSED-CORRECTLY / honest chain captured.** Per the campaign constraint, no
  vllm-gguf plugin was installed and no container launched — the honest reason (GGUF routes to
  llama.cpp; forced-vLLM needs the whole card and doesn't fit) IS the deliverable. (Also, free
  system RAM was far below the 8 GB WSL2 threshold the constraint required for an actual start.)

---

## Final table

| # | Model | Result | One-line evidence |
|---|---|---|---|
| 1 | gemma-4-12B Q4_K_M (llama.cpp) | **PASS** | Verdict-before-download → guarded 6.87 GiB download (sha256 in manifest) → HEALTHY 45 s → `offloaded 49/49 layers to GPU` → `"Hello, hello, how are you?"` @29.2 tok/s → unload NVML-verified (+9041 MiB) |
| 2 | nvidia/nemotron-3.5-asr-streaming-0.6b | **REFUSED-CORRECTLY** | NeMo/ASR safetensors; verdict `unknown`: "could not read a usable config.json … cannot be computed" — honest decline, 0 bytes downloaded |
| 3 | baidu/Unlimited-OCR | **VERDICT CAPTURED (not run)** | `runs_degraded` via vLLM/docker (~8891 MiB, exact) — vision-OCR w/ trust_remote_code, outside chat-proxy scope + budgets; 0 bytes downloaded |
| 4 | NVIDIA-Nemotron-3-Nano-30B-A3B-BF16 | **REFUSED-CORRECTLY (both layers)** | `wont_run` (~62278 MiB > 97% of card) + HTTP 400 `download_refused`: "refusing download of 58.84 GiB: exceeds the 8.00 GiB hard cap" — before any bytes |
| 5 | ornith via Ollama | **REFUSED-CORRECTLY** | Ollama 412 "requires a newer version of Ollama" (installed 0.21.2) surfaced verbatim through LocalHub's download route; no artifact |
| 6 | vLLM + gemma GGUF | **REFUSED-CORRECTLY** | Estimator routes GGUF→llamacpp only (ADR-004); forced Engine:"vllm" → queued "need 15766 MiB … 4106 MiB short" (whole-card prealloc); no container started |

**Product bugs / findings (not hot-fixed, per campaign rules):**

1. **Proxy self-shadowing blocks warm large models** (see #1 finale): `/v1/chat/completions`
   recomputes the verdict at default 8K ctx counting the requested model's own resident VRAM as
   "in use" → 503 `admission_blocked` for a HEALTHY resident 12B. Counted per-instance relay
   works. Extends KNOWN-LIMITATIONS #10.
2. **Queue-id churn makes external cancel racy** for a continuously re-queued (RAM-blocked)
   entry: the 5 s drain loop re-queues under a NEW id, so fetch→DELETE often 404s. Harmless but
   noisy; cancel on a stable id works (HTTP 200 proven).
3. **Transient `unknown` verdict** when the 1 Hz nvidia-smi probe returns an empty GPU snapshot
   (one occurrence; retry succeeded). Facet of KNOWN-LIMITATIONS #8.
4. **Remote-vs-local estimate gap:** remote GGUF metadata preview said 14230 MiB @8K; the local
   parse said 8086 MiB. Conservative direction (safe), but a 76% over-estimate can push users to
   smaller quants than needed.
5. (Process note) At finale time the repo HEAD didn't compile (`ggufRunFlags` signature change,
   in-progress work in `internal/compat/flags.go` [AM]); finale ran on the campaign binary built
   2026-07-07 21:43 from the then-working tree.

## Disk / artifacts / teardown

- **New artifacts kept on disk (user decides):**
  - `K:\projects\llm_models\models\yuxinlu1__gemma-4-12B-agentic-fable5-composer2.5-v2-3.5x-tau2-GGUF\gemma4-v2-Q4_K_M.gguf` — 7,381,381,664 bytes (6.87 GiB), sha256 `0b9506ca…c96791`.
  - Nothing else: ASR/OCR/30B/ornith correctly left 0 bytes.
- **Disk:** K: 47.62 GiB free at campaign start → **48.99 GiB free at finale end** (other
  activity on the volume freed space; the campaign's own delta is −6.87 GiB for gemma).
  C: untouched (2.4–2.7 GiB free throughout).
- **Teardown verified (2026-07-08):** no `localhub-camp`/`llama-server`/`ollama` processes;
  Ollama daemon DOWN (connection refused — restored state); no vLLM docker containers;
  **VRAM 713 MiB used vs 711–790 finale baseline (±200 met)**; free RAM 8645 MiB.
