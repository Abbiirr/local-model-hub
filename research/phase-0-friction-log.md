# Phase 0 friction log — LocalHub Compass (P0.2)

Field validation run **2026-07-06** on the reference box (RTX 4060 Ti 16 GB, 15.9 GB RAM,
Windows 10 Home). Goal: answer, by hand with **existing tools only** (no product code),
for ≥10 real models — *does it fit on this box, on what engine, at what context?* — and
log every friction point. Each friction point is a candidate LocalHub feature.

All real load tests obeyed the PLAN.md hardware safeguards: one model at a time, full-GPU
offload only, small contexts (≤8192), tiny generations (≤50 tok), VRAM verified back to
baseline via `nvidia-smi` after every load. No safeguard was deviated from (see end).

## Exact tool versions used

| Tool | Version / build | Path | Notes |
|---|---|---|---|
| gguf-parser (gpustack/gguf-parser-go) | v0.24.1 (windows-amd64) | scratchpad/tools/gguf-parser.exe | release binary, ~9.5 MB |
| llama-swap (mostlygeek) | v235 (c59816b), built 2026-07-03 | scratchpad/tools/llama-swap/llama-swap.exe | release binary, ~19 MB |
| llama-server — CUDA fork | turbo-tan/llama.cpp-tq3 commit 1cfa891 | K:\projects\llm_models\llama.cpp-tq3\build\bin\llama-server.exe | MSVC+CUDA; TQ3 kernels; fork-only flags below |
| llama-server — winget | build 8833 (45cac7ca7) | on PATH | RPC/Vulkan/CPU backends, no CUDA |
| Ollama | 0.21.2 | K:\Softwares\ollama\ollama.exe | 5 local models present |
| nvidia-smi / driver | 576.40 (CUDA 12.9 runtime) | system | VRAM probe |
| Go | 1.23.4 | on PATH | (not used to build; release binaries used) |

Baseline VRAM at start of run: **2820 MiB used / 13290 MiB free** of 16380 MiB total
(idle desktop + browser consuming ~2.8 GB; this is the *real* free-VRAM ceiling, not 16 GB).

Budget/margin used everywhere: usable VRAM = free − 1.5 GB. At baseline free that is
~11.8 GB headroom for a new model — **already below the 13 GB weight of the 27B TQ3 quants.**

---

## Model attempts (≥10)

Estimator column: gguf-parser NONUMA VRAM (the figure that matters on Windows/non-UMA),
full offload (`--ngl 999`), flash-attention on, at the stated ctx. "actual" = real load or
estimate-only per safeguards.

| # | Model | Format / quant | Source | Target engine | Estimated fit (tool + number) | Actual result | Friction notes | Keep/Drop |
|---:|---|---|---|---|---|---|---|---|
| 1 | GLM-OCR | GGUF Q8_0 (~950 MB, +484 MB mmproj) | local `models/glm-ocr` | llama.cpp CUDA fork | gguf-parser: **1582 MiB** VRAM NONUMA @ ctx4096 (text only, mmproj excluded) | **LOADED** @ ctx4096: HEALTHY, 1 completion OK (5 tok, ~22 tok/s). VRAM 2820→**3925** (Δ **~1105 MiB**), freed to 2822 on stop. | Estimate **over** by ~470 MiB (~30%), conservative/safe direction. mmproj NOT in estimate → real vision use bigger. Estimate excludes prompt-cache buffer (fork enables 8192 MiB limit). | **KEEP** — small OCR, easy fit |
| 2 | Qwen3.6-27B-TQ3_4S | GGUF **TQ3_4S** (~13.0 GB) | local `models/` | llama.cpp CUDA fork (only) | gguf-parser: **CANNOT PARSE** — "GGMLType(46) unsupported" (even `--skip-estimate` fails). Hand estimate: ~13.3 GB weights + KV (q4_0/tq3_0) > usable | **NOT LOADED (estimate-only, safeguard)**. At run time free=13288 MiB (<14 GB gate) and weights alone (~13.3 GB) exceed usable (free−1.5 GB = 11.75 GB). Historically runs via `run-qwen3.6-27b-tq3_4s.bat` (hot=64K ctx) when desktop VRAM lower. | **Biggest friction:** the estimator is blind to the exact quant this box depends on. Fit decided by *actual* free VRAM (2.8 GB already used by desktop), not nominal 16 GB. | **KEEP** — flagship near-limit |
| 3 | Qwen3.5-27B-TQ3_1S | GGUF **TQ3_1S** (~12.9 GB) | local `models/` | llama.cpp CUDA fork | gguf-parser: **CANNOT PARSE** (TQ3). Hand: ~13.0 GB weights, same class as #2 | **NOT LOADED (estimate-only)**. Same fit envelope as #2; `run-qwen3.5-27b.bat` is battle-tested evidence it runs. | TQ3 opaque to every off-the-shelf estimator. No inventory tool even reads its metadata. | **KEEP** — 2nd flagship |
| 4 | Qwopus3.5-27B-v3-TQ3_4S | GGUF **TQ3_4S** (~13.0 GB, +931 MB mmproj) | local `models/` | llama.cpp CUDA fork | gguf-parser: **CANNOT PARSE** (TQ3) | **NOT LOADED (estimate-only)**. `run-qwopus3.5-27b.bat` evidence. Vision variant adds mmproj → tighter. | Same TQ3 gap + mmproj adds unaccounted VRAM. | **KEEP** — vision flagship |
| 5 | Qwen3.6-35B-A3B-TQ3_4S | GGUF **TQ3_4S** MoE (35B, ~12.4 GB, A3B active) | local `models/` | llama.cpp CUDA fork | gguf-parser: **CANNOT PARSE** (TQ3) | **NOT LOADED (estimate-only)**. MoE — active params ≪ total; `run-qwen3.6-35b-moe.bat` runs it. | Estimator can't model MoE active-vs-total even for parseable quants; +TQ3 gap. Fit intuition (35B "won't fit") is wrong — it does, because A3B. | **KEEP** — MoE fit is counter-intuitive; high-value verdict |
| 6 | Qwen3-VL-32B-Instruct | GGUF **Q2_K** (~12.3 GB, +1.2 GB mmproj F16) | local `models/` | llama.cpp CUDA fork | gguf-parser: **12989 MiB** @ ctx8192 (parses — standard quant!) | **NOT LOADED (estimate-only, safeguard)**. 12989 MiB > usable 11752 MiB @ baseline free → verdict *won't fit at ctx8192 now*; `run-qwen3-vl-32b-instruct-q2k.bat` runs it at low ctx when VRAM freer. | Standard Q2_K parses fine → confirms the gap is TQ3-specific. mmproj (1.2 GB) NOT in the 12989 number. | **KEEP** — parseable big-model boundary case |
| 7 | Qwen3-VL-Embedding-8B | GGUF Q6_K (~5.8 GB) | local `models/` | llama.cpp CUDA fork | gguf-parser: **7373 MiB** @4096 / **7957 MiB** @8192 | **NOT LOADED (estimate-only)** — comfortably fits (7.4 GB < 11.75 usable); skipped to conserve load budget. | Clean parse. Embedding model but estimator treats as generative KV — slight over-estimate for embed use. | KEEP — mid fit, safe |
| 8 | nomic-embed-text-v2-moe | GGUF Q4_K_M (344 MB) via **Ollama** | Ollama local + loose GGUF | Ollama | gguf-parser (loose GGUF): **579 MiB** @4096 | **LOADED via Ollama** (`/api/embed`, keep_alive 5m). `/api/ps` size_vram=**911 MiB**, ctx 512. NVML Δ **~700 MiB**. Unloaded via keep_alive:0, `/api/ps` empty in ~2s, VRAM→baseline. | **Three numbers disagree**: gguf-parser 579 (ctx4096), Ollama reports 911, NVML delta 700. Ollama uses its own ctx (512) not gguf-parser's. No single source of truth for "how much VRAM will this take." | **KEEP** — embeddings; exposes cross-tool number mismatch |
| 9 | Meta-Llama-3.1-8B-Instruct | GGUF Q4_K_M (GQA) | **remote HF** `bartowski/…-GGUF` | llama.cpp / Ollama | gguf-parser **remote (no download)**: **6218 MiB** @8192 / **9338 MiB** @32768 | **NOT LOADED (remote estimate-only)** — fits at 8K (6.2 GB), tightens at 32K (9.3 GB, still < 11.75 usable). | Remote metadata read works great (no weight download). But no tool cross-references the estimate against *this box's* live free VRAM or the 1.5 GB margin. | KEEP — canonical GQA fit |
| 10 | Llama-3.3-70B-Instruct | GGUF Q4_K_M | **remote HF** `bartowski/…-GGUF` | (none viable) | gguf-parser **remote**: **43354 MiB** @8192 | **NOT LOADED — won't-fit (correct negative)**. 43.4 GB ≫ 16 GB total. No safe load attempted (would OOM). | Estimator correctly says huge, but gives no *verdict/reason* ("won't fit: needs 43 GB, have 13.3 GB free") — a human must eyeball MiB vs a budget it doesn't know. | **KEEP as won't-fit reference** |
| 11 | Llama-3.2-1B-Instruct | GGUF Q4_K_M | **remote HF** `bartowski/…-GGUF` | llama.cpp / Ollama | gguf-parser **remote**: **1798 MiB** @8192 / **2614 MiB** @32768 | **NOT LOADED (remote estimate-only)** — trivially fits. | Even the tiny/easy case needs manual MiB-vs-budget math; scales linearly with #models to triage. | KEEP — easy-fit baseline |

**Estimate-vs-actual deltas (the two real loads):**

- GLM-OCR: gguf-parser **1582 MiB** vs NVML actual **~1105 MiB** → parser **+43% over** (safe/conservative). Excludes mmproj and prompt-cache.
- nomic-embed: gguf-parser **579 MiB** (@ctx4096) vs Ollama-reported **911 MiB** vs NVML **~700 MiB** → three different numbers; the driver-truth (NVML ~700) sits between them; **no single tool is authoritative**.

---

## Friction list (each = a LocalHub feature candidate)

1. **Estimator is blind to custom/newer quants.** gguf-parser v0.24.1 fails hard on TQ3
   (`GGMLType(46) unsupported`) — even metadata read aborts — for the 4 TQ3 models that
   are the *primary* workload of this box. → LocalHub must (a) degrade gracefully to a
   size-based hand estimate + margin when the parser can't read a tensor type, and
   (b) never present "unknown" as "won't fit". *[feeds Phase 2 / ADR-002]*
2. **No tool knows this box's *actual* free VRAM.** gguf-parser prints MiB against nothing;
   baseline desktop already eats ~2.8 GB, so real ceiling is ~13.3 GB free, not 16 GB.
   Fit was decided by hand cross-referencing `nvidia-smi` free vs estimate every time.
   → LocalHub's verdict must subtract live free-VRAM and the 1.5 GB margin automatically.
3. **No margin / verdict / reason.** Every tool gives a raw number; none gives
   `runs | runs_degraded | wont_run` + human reason + max-ctx-that-fits + run-flags.
   The 70B case (43 GB) is obviously-no to a human but the tool won't *say* it.
4. **mmproj (vision) VRAM is invisible.** gguf-parser estimates the text model only;
   4 of the local models ship an mmproj (0.5–1.2 GB) loaded alongside. → estimator must
   sum model + mmproj when a projector is configured.
5. **MoE active-vs-total confusion.** The 35B-A3B "looks" too big but fits (A3B active).
   No off-the-shelf tool encodes that; a naive size rule gives a false won't-run.
6. **Three tools, three VRAM numbers, no source of truth.** gguf-parser vs Ollama
   `size_vram` vs NVML delta disagreed by 30–60% for the same model. → LocalHub should
   record the *measured NVML peak* on first real run and prefer it thereafter (estimator
   error loop; matches lmwrapper "measured-VRAM fit").
7. **No unified inventory across sources.** Ollama models (`ollama list`, 5 of them),
   loose GGUFs in `models/`, HF-style subdirs, and mmproj siblings are four separate
   worlds. Answering "what do I have and what runs?" meant three different CLIs by hand.
8. **Fork-vs-upstream flag confusion is real but narrower than assumed.** After probing
   both `--help` outputs: `--fit`, `--sleep-idle-seconds`, `--models-dir` router, and
   `--reasoning-budget` are **now in upstream too** (winget b8833). The genuine fork-only
   items are the **`tq3_0` KV cache type** and the **TQ3 weight kernels** (+ CUDA backend;
   winget build is Vulkan/CPU only). → adapter must capability-probe `--help` for the KV
   type list and pick the fork binary specifically for TQ3 models. *[corrects earlier
   lmwrapper note that listed `--sleep-idle-seconds`/`--fit` as fork-only]*
9. **KV-cache quant changes fit and the estimator can't model the fork's.** The bat
   profiles use `-ctk q4_0 -ctv tq3_0`; gguf-parser only accepts up to `q4_0`/`q5_1` for
   `--ctv`, so it can't reproduce the fork's actual KV footprint even for parseable models.
10. **No engine routing by format+capability.** Deciding "TQ3 → fork build; standard GGUF
    → Ollama or winget; Q2_K VL-32B → fork for CUDA" was manual. winget build has no CUDA
    at all here — routing a CUDA job to it would silently fall back to Vulkan/CPU (slow).
11. **vLLM impossible natively on Windows** — no engine to even try; deferred to Docker
    (see vLLM note). A whole class of safetensors models can't be validated on-box today.
12. **Phone access absent from every tool.** gguf-parser/llama-swap/Ollama are all local
    CLIs/HTTP on localhost; none answer "does it fit?" from a phone. Tailscale is present
    (1.96.3) but nothing serves a verdict UI over it.
13. **llama-swap gaps (see trial):** no VRAM-aware admission, no fit estimate, no priority
    queue — it swaps blindly and will happily try to load something too big. *[ADR-003]*

---

## llama-swap trial report (informs ADR-003)

**Setup:** llama-swap v235 on `127.0.0.1:8090`, config with two small models (glm-ocr,
paddleocr) both spawning the CUDA **fork** `llama-server` with `--host 127.0.0.1`, CUDA
DLLs on PATH, `ttl: 30`. Config file: `scratchpad/llama-swap.yaml`.

**What worked (all verified this session):**

- **Model listing:** `GET /v1/models` → both configured models; `GET /running` → `[]`
  when idle. OpenAI-compatible surface is clean.
- **Swap on request:** a `/v1/chat/completions` for `glm-ocr` **auto-started** its
  llama-server (VRAM 2825→3929, Δ~1104 MiB — same as a direct load). Requesting
  `paddleocr` next **unloaded glm-ocr and loaded paddleocr** — VRAM stayed ~3930 (one
  resident, not stacked). Enforces max-one implicitly by unloading the previous.
- **Running-model status:** `GET /running` reported the live model with `state:"ready"`,
  full spawn `cmd`, `proxy` URL, `ttl`. Good observability primitive.
- **Manual unload endpoint:** `POST /api/models/unload` → `{"msg":"ok"}`, `/running` went
  empty, VRAM returned to 2825 baseline. (`POST /api/models/unload/:id` also exists.)
- **TTL auto-unload:** with `ttl:30`, an idle model auto-unloaded (~30 s idle), VRAM back
  to baseline, `/running` empty — no manual intervention.
- **Localhost bind:** honored `-listen 127.0.0.1:8090` and per-model `--host 127.0.0.1`.

**What's missing for LocalHub's residency executor role (ADR-003 evidence):**

- **No VRAM-aware admission.** llama-swap does NOT check free VRAM or an estimate before
  spawning. It swaps blindly: point it at the 27B TQ3 with the desktop already using
  2.8 GB and it will attempt the load and OOM/fail — exactly the false-`runs` the CLAUDE.md
  correctness bar forbids. It has no concept of the 1.5 GB margin.
- **No fit estimate at all.** No gguf-parser integration, no per-model VRAM prediction, no
  `max_context_that_fits`, no verdict/reason. It's a *swapper*, not a *compatibility engine*.
- **No priority queue / admission ordering.** Concurrency is via a swap "matrix" DSL, not
  a priority/aging scheduler; no anti-starvation, no size/cost-aware ordering (the P5.0
  requirement). Requests that can't be served aren't queued with `blocked_reason`.
- **No stream-safety guarantee surfaced.** Swap-out timing vs in-flight requests isn't
  exposed as a DRAINING state; LocalHub's never-kill-active-streams gate isn't something
  llama-swap advertises or lets you enforce policy on.
- **Config is static YAML.** Models are hand-declared with full spawn commands; no dynamic
  inventory, no format→engine routing, no capability probing.

**Verdict for ADR-003:** llama-swap is an **excellent process-swap executor** — its
on-demand start, single-resident swap, `/running`, `/api/models/unload`, and `ttl`
auto-unload are exactly the low-level mechanics LocalHub needs and are proven working on
this box in this trial. But it is **not** an admission controller: it has no VRAM
awareness, no estimator, no priority queue, no verdict/reason, no margin. **Recommendation
(evidence, not the decision — P0.3 owns ADR-003): LocalHub should NOT reimplement swap
mechanics, but the custom residency layer (VRAM-aware admission + estimate + P5.0 priority
queue + never-kill-streams) must sit *above* whatever executes the swap.** Whether to
drive llama-swap as that executor vs. spawn llama-server directly (as the bat scripts and
lmwrapper already do) is the P0.3 call; the friction here is that llama-swap gives us
mechanics-for-free but zero of the safety/verdict logic that is LocalHub's entire reason
to exist.

---

## vLLM note

**vLLM has no native Windows support** — not installed, cannot be exercised on-box. It is a
Linux-only engine; on this machine it is reachable only via Docker Desktop (WSL2, GPU
passthrough), which is deferred to the controlled P4.5 session (tiny ≤1B model, WSL RAM
cap ≤8 GB, torn down after). **Friction:** an entire model family — HF safetensors served
by vLLM — cannot be validated natively here at all. LocalHub's vLLM adapter must therefore
be built against vLLM's *documented* CLI/HTTP contract with a Windows stub for continuous
testing, and only one real Docker E2E. This is friction row #11 above.

---

## Fork vs upstream flag findings (P0.2 sub-deliverable)

Probed both `llama-server --help` outputs directly:

- **Fork-only (`llama.cpp-tq3`, commit 1cfa891):** `tq3_0` value for `-ctk`/`-ctv` KV
  cache; the **TQ3 weight quant kernels** (GGMLType 46) required to load the TQ3 model
  files at all; **CUDA backend** (this build detects the 4060 Ti).
- **NOT fork-only (present in winget upstream b8833 too):** `--fit [on|off]`,
  `--fit-target`, `--fit-ctx`, `--sleep-idle-seconds`, `--models-dir/--models-preset`
  router flags, `--reasoning-budget`, `--jinja`, `--no-warmup`. *This corrects
  research/lmwrapper-reuse-notes.md, which listed `--sleep-idle-seconds` and (tentatively)
  `--fit` as fork-only — they have since landed upstream.*
- **winget build has NO CUDA** — backends are RPC/Vulkan/CPU only. So even standard GGUFs
  needing GPU offload should route to the fork build on this box, not winget. → the
  llama.cpp adapter must (a) capability-probe `--help` for the KV-type list and backend,
  and (b) select the fork binary for TQ3 models specifically.

---

## vLLM note

<!-- FILLED AS RUN -->

---

## Safeguard compliance

**No safeguard was deviated from.**

- **Max one model loaded at any time:** yes. Loads were strictly sequential; every load
  was stopped and VRAM verified before the next. llama-swap enforced single-resident.
- **1.5 GB VRAM margin honored:** yes — the 27B TQ3 (#2) and VL-32B Q2_K (#6) were
  **skipped** precisely because weights exceeded free−1.5 GB; recorded as estimate-only.
- **Real loads (2 total):** GLM-OCR (fork, ctx4096) and nomic-embed (Ollama). Both small.
  Big/near-limit models were estimate-only per the free-VRAM gate (free 13.3 GB < 14 GB).
- **Full GPU offload only (`-ngl 999`/`all`), small ctx (≤4096 on real loads), tiny
  generations (≤5 tok):** yes. No CPU-offload inference at any point.
- **VRAM verified back to baseline after every load** via `nvidia-smi`: GLM-OCR
  2820→3925→**2822**; Ollama 2822→3522→**2938→**(idle settle); llama-swap
  2825→3929→(swap)→**2825**; all processes confirmed gone via `tasklist`.
- **All binds 127.0.0.1:** GLM-OCR :8092, llama-swap :8090 + child :5800, Ollama :11434.
  Never 0.0.0.0. (The reference bat scripts default HOST=0.0.0.0 — I overrode to
  127.0.0.1 for every launch; that default is itself a security-gate friction note.)
- **Downloads:** only the two tool binaries (gguf-parser ~9.5 MB + llama-swap ~11 MB zip)
  = **~20 MB total**, well under the ≤2 GB new-download budget. **Zero model downloads**
  (remote HF estimates read metadata only, no weights). Nothing written to C:. Tool
  binaries live in the scratchpad.
- **Cleanup:** all spawned `llama-server` / `llama-swap` processes killed; final VRAM
  2822 MiB = baseline. Ollama daemon left running (pre-existing, not spawned by me).

**Final VRAM: 2822 MiB used / 13288 MiB free — baseline restored.**
