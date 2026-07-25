# LocalHub Optimization-Support Research & Test Plan

**Task:** Verified optimization-support matrix + implementation/test plan for inference optimizations.
**Verbatim user instruction:** *"we also need to be very efficient so test speculative decoding, dspark, sliding attention and all the other new optimization support in this"*
**Date of research:** 2026-07-07. **Box:** RTX 4060 Ti 16 GB (compute 8.9), single GPU.
**Method:** local binary `--help`/`--version` dumps (no GPU load, no server start) + web primary sources (dated inline).
**Binding rule (CLAUDE.md):** no invented engine behavior — every claim cites a primary source or direct local-binary evidence.

> ⚠️ Cost/scope note carried from session: this is research-only. NO code was changed. NO models were loaded.

---

## 0. Ground-truth: the two local llama-server binaries (authoritative for THIS box)

Captured 2026-07-07 by running each binary with `--version` and `--help` (help dumps only; no model load).

| | winget build | tq3 fork |
|---|---|---|
| Path | `C:\Users\abirh\AppData\Local\Microsoft\WinGet\Packages\ggml.llamacpp_Microsoft.Winget.Source_8wekyb3d8bbwe\llama-server.exe` | `K:\projects\llm_models\llama.cpp-tq3\build\bin\llama-server.exe` |
| Version | **b8833 (45cac7ca7)**, Clang 19.1.5 | fork **commit 1cfa891**, MSVC 19.44 |
| Backends loaded | RPC + **Vulkan** + CPU (haswell) — **no CUDA** in that package dir | **CUDA** (found RTX 4060 Ti, 16379 MiB) |
| Git remote | (winget package) | `https://github.com/turbo-tan/llama.cpp-tq3.git` |
| tq3 HEAD commit msg | — | `perf: VDR=16 for TQ3_4S MMVQ on SM120 Blackwell` |

**Flag-surface diff (verbatim, computed from the two `--help` dumps):**
- Flags in **tq3 but not winget:** *(none)*
- Flags in **winget but not tq3:** `--clear-idle`, `--rpc`

**Load-bearing conclusion:** The tq3 fork's server exposes an **identical optimization flag surface** to upstream b8833. The fork's value is entirely its **TQ3 quant CUDA kernels** (Blackwell/SM120 perf, `tq3_0` KV type), NOT any new decoding feature. Every optimization flag below is available on **both** builds. The winget build's own package folder ships Vulkan/CPU/RPC only (no CUDA dll there) — so for CUDA speed on this box LocalHub correctly prefers the tq3 fork (already encoded in `bestLlamaBinary`).

### Optimization-relevant flags present in BOTH local binaries (b8833), verbatim from `--help`

```
-b,   --batch-size N            logical max batch size (default: 2048)
-ub,  --ubatch-size N           physical max batch size (default: 512)
--swa-full                      use full-size SWA cache (default: false)
-fa,  --flash-attn [on|off|auto] set Flash Attention use (default: 'auto')
-ctk, --cache-type-k TYPE       KV cache data type for K
-ctv, --cache-type-v TYPE       KV cache data type for V
-ctkd,--cache-type-k-draft TYPE KV cache data type for K for the draft model
-ctvd,--cache-type-v-draft TYPE KV cache data type for V for the draft model
-ot,  --override-tensor <pat>=<buftype>,...
-cmoe,--cpu-moe                 keep all MoE weights on CPU
-ncmoe,--n-cpu-moe N            keep MoE weights of first N layers on CPU
-otd, --override-tensor-draft   (draft-model tensor override)
-cmoed,--cpu-moe-draft / -ncmoed,--n-cpu-moe-draft N
--cache-reuse N                 min chunk size to reuse from cache via KV shifting
--context-shift/--no-context-shift  context shift on infinite text gen
-np,  --parallel N              number of server slots (default -1 = auto)
-cb,  --cont-batching/-nocb     continuous (dynamic) batching
--slots/--no-slots              expose slots monitoring endpoint
--slot-save-path PATH           save slot kv cache
-sps, --slot-prompt-similarity SIM  prompt-match threshold to reuse a slot (default 0.10)
-lcs, --lookup-cache-static FNAME  / -lcd, --lookup-cache-dynamic FNAME
-ctxcp,--ctx-checkpoints,--swa-checkpoints N  max ctx checkpoints per slot
--sleep-idle-seconds SECONDS    server sleeps after idle (-1 = disabled)
-- SPECULATIVE DECODING --
-td/--threads-draft, -tbd/--threads-batch-draft
--draft,--draft-n,--draft-max N       tokens to draft (default 16)  [env LLAMA_ARG_DRAFT_MAX]
--draft-min,--draft-n-min N           (default 0)                    [env LLAMA_ARG_DRAFT_MIN]
--draft-p-min P                       min greedy prob (default 0.75) [env LLAMA_ARG_DRAFT_P_MIN]
-cd,  --ctx-size-draft N              draft ctx (0 = from model)
-devd,--device-draft <devs>           devices for draft offload
-ngld,--gpu-layers-draft N            draft layers in VRAM (default auto)
-md,  --model-draft FNAME             draft model (default: unused)  [env LLAMA_ARG_MODEL_DRAFT]
--spec-replace TARGET DRAFT           translate TARGET->DRAFT when draft & main not compatible
--spec-type [none|ngram-cache|ngram-simple|ngram-map-k|ngram-map-k4v|ngram-mod]
                                      self-speculative type when NO draft model provided (default none)
--spec-ngram-size-n N (default 12) / --spec-ngram-size-m N (default 48) / --spec-ngram-min-hits N (default 1)
-hfd/-hfrd,--hf-repo-draft <user>/<model>[:quant]  draft model from HF
--fim-qwen-7b-spec / --fim-qwen-14b-spec           built-in Qwen-Coder spec presets
```

**Divergence flag (docs vs local):** llama.cpp *master* `docs/speculative.md` (fetched 2026-07-07) already renames these to a `--spec-draft-*` namespace and adds `--spec-type draft-eagle3` (reads target hidden states) and `draft-dflash` (block-diffusion). **Our local b8833 does NOT have those** — it has the older `--model-draft`/`--draft-max`/`--spec-type ngram-*` surface. Any LocalHub code MUST detect the flag from the local `--help` (as the engines package already does), never assume the master namespace. [primary: github.com/ggml-org/llama.cpp/blob/master/docs/speculative.md, 2026-07-07; local b8833 `--help`, 2026-07-07]

---

## 1. Speculative decoding

**(a) What it is.** A small "draft" model (or a draft-free n-gram predictor) proposes k tokens; the target model verifies them in one forward pass and keeps the longest accepted run. Output is provably identical to greedy/sampled target output (rejection sampling). Typical speedup 1.5–3× t/s when acceptance is high. [Ollama/llama.cpp writeups, 2026]

**(b) Support status (2026-07-07):**
- **llama.cpp (both local builds, b8833):** ✅ Full. Draft-model path (`-md`, `--draft-max/-min`, `--draft-p-min`, `-ngld`, `-cd`, `-ctkd/-ctvd`) AND draft-free self-speculative path (`--spec-type ngram-cache|ngram-simple|ngram-map-k|ngram-map-k4v|ngram-mod`, `--spec-ngram-*`). Cross-tokenizer bridging via `--spec-replace TARGET DRAFT`. Lookup-cache decoding via `-lcs/-lcd` (static/dynamic). [local `--help`, 2026-07-07]
- **vLLM:** ✅ Rich. `--speculative-config` JSON: `{"method":"draft_model|ngram|eagle|mtp|...","model":"<draft>","num_speculative_tokens":N}`. Supported methods: EAGLE, MTP, Draft Model, Parallel Draft (PARD), MLP, N-Gram, Suffix Decoding, Hidden-State Extraction, Dynamic Spec, custom proposer. Constraint: "Pipeline parallelism is not composable with speculative decoding as of vllm<=0.15.0"; draft-model spec unsupported in vllm<=0.10.0. [docs.vllm.ai/en/latest/features/speculative_decoding/, 2026-07-07]
- **Ollama:** ⚠️ Partial/auto. Enabled via `OLLAMA_SPECULATIVE_DECODE=1` (Ollama 5.x); auto-tunes draft length; draft model specifiable at model creation per recent notes. Historically the explicit-draft PR (#8134) was rejected pending backend rewrite; issue #9216 (opened 2025-02-19) closed as dup of #5800. Status is "improving, mostly automatic," not a clean per-request draft API. Treat as **secondary** — LocalHub's llama.cpp path is the ground truth. [github.com/ollama/ollama issues #9216/#5800; secondary 2026 writeups — flag as needs-reverify if Ollama becomes a supported LocalHub engine]

**(c) Exact flags to enable (llama.cpp, this box):**
`-m <target.gguf> -md <draft.gguf> --draft-max 16 --draft-min 0 --draft-p-min 0.75 -ngld all` (draft-model).
Draft-free: `-m <target.gguf> --spec-type ngram-cache` (no second model, ~free RAM, lower speedup).

**(d) Constraints.**
- Draft-model path: draft SHOULD share the target's tokenizer/vocab for a clean accept; if not, `--spec-replace` can bridge specific token-string differences but is fragile.
- **VRAM cost:** the draft model AND its own KV cache reside in VRAM (`-ngld`, `-cd`, `-ctkd/-ctvd`) — **two models resident**. On 16 GB this is only viable when target+draft+both KV caches fit. A ~1 GB draft alongside a ~10–12 GB target is fine; alongside a 27B TQ3 (~11–13 GB) it's tight.
- Draft-free `ngram-*`: negligible VRAM (a hash pool, ~16 MB for ngram-mod), shared across slots; speedup depends on repetitive/structured output.

**(e) LocalHub gap.** **Needs a code change.** Current `ggufRunFlags` emits no draft flags and the estimator/admission accounts for ONE model only. See §5.

---

## 2. "dspark" — IDENTIFIED

**Verdict: DSpark is a real 2026 technique/framework, but it is NOT a llama.cpp / vLLM / Ollama feature and is out of scope for LocalHub as a single-box control plane.**

**What it is:** **DSpark** = *"Confidence-Scheduled Speculative Decoding with Semi-Autoregressive Generation,"* paper by **DeepSeek-AI + Peking University**, released **2026-06-27**. It is an advanced form of speculative decoding: every drafted token gets a confidence score, and an adaptive scheduler *skips verification* of low-confidence drafts, verifying only likely-accept tokens — plus semi-autoregressive (block) drafting. DeepSeek reports 57–85% faster per-user generation on V4-Flash/V4-Pro vs their prior stack. [venturebeat 2026-06-30 (403 to fetch, via search snippet); fullstack.com blog, fetched 2026-07-07; braindetox/medium/alphasignal, 2026-06/07]

**How it's shipped:**
- DeepSeek open-sourced **DeepSpec** — "a full-stack codebase for training and evaluating speculative decoding draft models" (training + eval scripts), with target implementations for DeepSeek V4, and reportedly Qwen and Gemma.
- DSpark-enabled *model variants* exist on HF (e.g. `DeepSeek-V4-Pro-DSpark`). **NVIDIA NeMo AutoModel** is adding explicit DSpark support.
- **No integration into llama.cpp, Ollama, or vLLM** is documented as of 2026-07-07. It requires DSpark-trained draft heads, not general GGUF models. [fullstack.com, 2026-07-07]

**Why out of scope (justify):** DSpark needs (i) a training pipeline (DeepSpec) to produce DSpark draft heads, and (ii) a serving engine that implements confidence-scheduled verification — neither of which exists in LocalHub's supervised engines. It also targets the DeepSeek V4 family, none of which is in this box's inventory. If a llama.cpp `--spec-type` variant or a vLLM `method` for DSpark/DeepSpec appears later, it would slot in exactly where §5 puts speculative decoding. **Recommendation: do NOT implement; watch llama.cpp `--spec-type` and vLLM `speculative-config method` enums for a future `dspark`/`dflash`-style entry.** (Note: llama.cpp master ALREADY added `--spec-type draft-dflash` = block-diffusion drafting — the nearest in-engine cousin — but it is NOT in local b8833.)

---

## 3. Sliding-window attention (SWA / iSWA)

**(a) What it is.** Attention that only attends to the last `n_swa` tokens on some layers, bounding KV-cache growth. **Gemma-4 uses interleaved SWA (iSWA):** local layers with `n_swa = 1024` interleaved with global full-context layers. [ggml-org issues #22527/#21769; ik_llama #1607; 2026]

**(b) llama.cpp support (both local builds, b8833):** ✅ **Automatic** for SWA models. Maintainer am17an: *"--swa-full basically keeps the whole history instead of the sliding window."* Default = SWA active (window-sized KV on local layers → less VRAM). `--swa-full` forces full attention on ALL layers → **much more VRAM, OOMs at high context** (community warning: "The critical mistake is adding --swa-full"). `--swa-checkpoints N` (aka `-ctxcp`) bounds context checkpoints per slot. [github.com/ggml-org/llama.cpp/discussions/24543, 2026; local `--help`, 2026-07-07]
- **Known bug:** Gemma-4 + SWA checkpoint + **Flash Attention** → illegal memory access (#22527); SWA checkpoint restoration could discard mid-conversation context (#21769). Both April–May 2026. **Re-verify against b8833/b9893** before pairing `-fa on` with Gemma-4 in LocalHub. b9893 (2026-07-07) added "DK=512 handling for Gemma-4" FA kernels — the fix may already be in newer builds than local.
- **vLLM:** ✅ SWA supported for SWA model families; MTP "Gemma 4 assistant checkpoints" referenced in spec-decode docs. [docs.vllm.ai, 2026-07-07]

**(c/d) Flags/constraints.** Do **NOT** emit `--swa-full` by default. Leave SWA automatic. For Gemma-4 at large ctx, SWA is what makes big context fit at all.

**(e) LocalHub impact — ESTIMATOR ACCURACY (important for incoming gemma-4-12B).** LocalHub wraps `gguf-parser-go` for VRAM/ctx estimation (ADR-002); LocalHub itself does not compute KV bytes (no SWA math in `internal/compat`). **The correctness question is whether gguf-parser-go models iSWA window-sized KV vs full-context KV for Gemma-4.** If it assumes full-context KV on all layers, LocalHub will **overestimate** VRAM for Gemma-4 (pessimistic — refuses fits that would actually run) OR if it assumes SWA where a layer is global, **underestimate** (dangerous — false `runs` → OOM, a correctness bug per CLAUDE.md). **Action:** verify gguf-parser-go's Gemma-4/iSWA handling and calibrate against a real load of the gemma-4-12B once downloaded (a campaign-agent job, not this task). This is the single highest-value finding for estimator correctness.

---

## 4. The rest — what 2026 llama.cpp/vLLM offers that LocalHub should surface

Verified against local b8833 `--help` (2026-07-07) unless noted.

| Optimization | Local llama.cpp (b8833) | LocalHub today | Notes |
|---|---|---|---|
| **Flash attention** `-fa on\|off\|auto` | ✅ (default auto) | ✅ **emitted** (`-fa on` when `HasFlashAttn`) | free win; caution: SWA+FA Gemma-4 bug (§3) |
| **KV-cache quant** `-ctk/-ctv` (+ fork `tq3_0`) | ✅ | ✅ **emitted** on degraded fits | already plumbed incl. fork `tq3_0` value cache |
| **Context shift** `--context-shift/--no-context-shift` | ✅ | ❌ not emitted (llama.cpp default applies) | flag-only if we want to force it |
| **Prompt/prefix (slot) caching** `--cache-reuse`, `-sps`, `--slot-save-path`, `--slots` | ✅ | ❌ | llama.cpp reuses slot KV automatically; surfacing `--cache-reuse N` is flag-only |
| **Continuous batching / slots** `-cb` (default on), `-np N` | ✅ | ❌ (single slot implied) | multi-slot changes VRAM math; see §5 (iii) |
| **vLLM chunked prefill** | — | n/a (vLLM path) | on by default in modern vLLM; safetensors path only |
| **CUDA graphs** | implicit in CUDA build (no user flag) | n/a | not a surfaced flag; fork handles it |
| **Tensor/pipeline parallel** | flags exist but **single-GPU box** | n/a | **OUT OF SCOPE** — one GPU (ADR-007 single box); say so |
| **MoE offload** `-cmoe`, `-ncmoe N` | ✅ | ❌ | **relevant:** Qwen3.6-35B-A3B MoE + Qwopus TQ3. Lets MoE experts sit on CPU/RAM to fit a bigger MoE on 16 GB. Maps to a per-start knob like `allow_cpu_offload` |
| **Draft-model KV/offload** `-ctkd/-ctvd`, `-ngld`, `-devd`, `-otd`, `-cmoed/-ncmoed` | ✅ | ❌ | only needed once speculative decoding lands (§1) |
| **Lookup-cache decoding** `-lcs/-lcd` | ✅ | ❌ | draft-free spec via static/dynamic n-gram cache files |
| **Self-spec (draft-free)** `--spec-type ngram-*` | ✅ | ❌ | zero extra VRAM; easiest spec win to demo on this box |
| **Idle sleep** `--sleep-idle-seconds` (+ winget `--clear-idle`) | ✅ | ❌ | residency-adjacent; could reduce idle VRAM but interacts w/ LocalHub's own residency mgr — likely leave to LocalHub, not the engine |
| **tq3 fork additions** | TQ3 quant CUDA kernels, `tq3_0` KV type, SM120 perf | ✅ (`HasTQ3` gate) | fork adds NO new server flags vs b8833 (diff proven §0) |

**2026-new decoding accelerations found in recent releases:** `--spec-type draft-eagle3` / `draft-dflash` (block diffusion) exist in **master docs** but **NOT in local b8833** (flag-namespace divergence, §0). b9895 (2026-07-07) fixed an ngram-map OOB read; b9893 added Gemma-4 FA (DK=512) OpenCL kernels. [github.com/ggml-org/llama.cpp/releases, 2026-07-07]

---

## 5. Mapping into LocalHub architecture

Current surface (verified in code, 2026-07-07):
- `internal/compat/flags.go` → `ggufRunFlags(d, bin, ctx, kvType, kvQuantUsed, allowCPUOffload)` emits `-ngl {all|auto} -c <ctx> --fit off [-fa on] [-ctk/-ctv ...] [--mmproj ...]`.
- `internal/compat/input.go` → `LlamaBinary{Detected, HasTQ3, HasFlashAttn, HasCUDA, SupportsKVQuant}` (caps grepped from `--help` in `internal/engines/llamacpp/llamacpp.go` `parseCapabilities`).
- `internal/core/instances.go` → `StartInstanceRequest{ModelID, Engine, Ctx, Batch, Priority, Pin, KVQuant, AllowCPUOffload}` — `AllowCPUOffload` is the established per-start-knob pattern.
- `PlanLlamaLaunch(desc, avail, ctx, kvQuant, allowCPUOffload)` reuses `bestLlamaBinary` + `ggufRunFlags`.

### (i) Free wins — already active
- **Flash attention** (`-fa on`) — emitted. ✅
- **KV quant** (`-ctk/-ctv`, fork `tq3_0`) — emitted on degraded fits. ✅
- **SWA** — automatic in llama.cpp; LocalHub must NOT add `--swa-full`. ✅ (verify estimator §3.)
- **Continuous batching** — llama.cpp default `-cb` on. ✅ (implicit)

### (ii) Flag-plumbing only (small — extend `ggufRunFlags` + one cap bit each)
1. **Self-speculative `--spec-type ngram-cache`** — add a `HasSpecType` cap (grep `--help` for `--spec-type`) and a per-start bool `spec_ngram` (or config default) → append `--spec-type ngram-cache`. Zero extra VRAM, no draft model, no estimator change. **Lowest-risk speedup; best first demo.**
2. **MoE CPU offload `-ncmoe N` / `-cmoe`** — extend the existing `AllowCPUOffload` semantics OR add `moe_cpu_layers int`. Relevant to Qwen3.6-35B-A3B MoE. **Caveat:** this DOES change VRAM footprint (experts leave VRAM) — admission math must account for it, so it's really borderline (ii)/(iii).
3. **`--cache-reuse N`** — expose slot-KV reuse threshold; flag-only, no admission change.
4. **`--context-shift/--no-context-shift`** — expose if we want deterministic control.

### (iii) Real features (design work)
**Draft-model speculative decoding** — the big one. Needs:
- **A draft-model picker.** A `draft_model_id` on `StartInstanceRequest`, resolved to a GGUF path. Auto-suggest a same-family draft (see §6 pairing table).
- **A vocab/tokenizer-compat check.** Read `tokenizer.ggml.*` / vocab hash from both GGUFs (the `internal/compat/gguf` parser already reads headers). Refuse mismatch with a human `reason` (CLAUDE.md: every verdict carries a reason). Optionally allow `--spec-replace` only behind an advanced flag.
- **VRAM accounting for TWO models.** Estimator must sum target-weights + target-KV + draft-weights + draft-KV (`-ngld`, `-cd`, `-ctkd/-ctvd`). This is a new admission code path (currently single-model). A false `runs` here OOMs → correctness bug.
- **Admission/residency interplay.** Two co-resident models count against the `≤2 resident` budget and NVML free-VRAM re-measure (ADR-001). Draft is pinned to its target's lifecycle (evicting one must evict the pair). `DRAINING` semantics unchanged.
- **Flags emitted:** `-md <draft> --draft-max 16 --draft-p-min 0.75 -ngld all [-cd N] [-ctkd/-ctvd ...]`.

### (iv) Out of scope for a single-box control plane (justify)
- **Tensor/pipeline parallel** — one GPU (ADR-007). N/A.
- **DSpark / DeepSpec** — needs a training pipeline + engine support not present in any LocalHub engine; targets DeepSeek V4 (not in inventory). Watch-list only (§2).
- **vLLM chunked prefill / APC** — lives entirely inside vLLM; LocalHub's vLLM path is a thin passthrough (ADR-002/005). Surface as informational, don't re-implement.
- **Engine idle-sleep (`--sleep-idle-seconds`)** — overlaps LocalHub's own residency manager; delegating idle-VRAM reclaim to the engine would fight the scheduler. Keep in LocalHub.

**Recommended implementation order:**
1. **Self-spec `--spec-type ngram-cache`** (ii-1): tiny, zero-VRAM, immediate t/s demo, no admission change.
2. **Verify gemma-4 iSWA estimator accuracy** (§3): correctness, not a feature — do before gemma-4-12B goes live.
3. **MoE CPU offload `-ncmoe`** (ii-2): unlocks the 35B-A3B MoE on 16 GB; moderate (admission-aware).
4. **Draft-model speculative decoding** (iii): highest value, highest effort; ship after 1–3.
5. **`--cache-reuse` / context-shift** (ii-3/4): polish.

---

## 6. Test plan (RTX 4060 Ti 16 GB, inventory-based)

**Inventory GGUFs (verified on disk 2026-07-07):**
`Llama-3.2-1B-Instruct-Q4_K_M`, `GLM-OCR-Q8_0`(+mmproj), `PaddleOCR-VL-1.5`(+mmproj), `nomic-embed-text-v2-moe`, `Qwen3-VL-32B-Instruct-Q2_K`(+mmproj), `Qwen3-VL-Embedding-8B-Q6_K`, `Qwen3.5-27B-TQ3_1S`, `Qwen3.6-27B-TQ3_4S`(+mmproj), `Qwen3.6-35B-A3B-TQ3_4S`(MoE), `Qwopus3.5-27B-v3-TQ3_4S`(+mmproj), `gemma-4-12B-...-GGUF` (downloading).

### Draft/target pairing analysis (vocab/family)
| Target | Same-family draft in inventory? | Verdict |
|---|---|---|
| Qwen3.5/3.6-27B TQ3, Qwopus3.5-27B | ❌ no tiny Qwen3.x draft on disk | can't demo draft-spec without a download |
| Qwen3.6-35B-A3B MoE | ❌ same | — |
| Llama-3.2-1B | it IS the tiny model; no larger Llama target in inventory | can't pair (no target) |
| gemma-4-12B (incoming) | ❌ no gemma draft on disk | — |

**Conclusion: NO inventory pair can demo *draft-model* speculative decoding today.** Two ways forward:
- **Zero-download demo:** use **draft-free `--spec-type ngram-cache`** on any target (e.g. Qwen3.6-27B-TQ3) — proves the speculative machinery with no second model, no extra VRAM. **Recommended first test.**
- **Smallest download to enable a REAL draft-model demo:** a **Qwen3 0.6B GGUF draft** paired with the Qwen3.x-27B targets (Qwen3 0.6B→8B is a documented 2026 pair; the 27B works too if vocab matches the TQ3 target's Qwen3 tokenizer — **verify vocab hash before trusting acceptance**). Q4/Q8 of a 0.6B ≈ **0.4–0.8 GB**. Alternatively **Gemma 3/4 1B GGUF** (~0.7–1 GB) once the gemma-4-12B target lands (Gemma 1B→27B is a documented 1.8× pair). Either keeps disk ≥6 GB free easily.

### Per-optimization "proof it's active" checks (all within safeguards: ≥6 GB disk free post-download, ≤2 resident, teardown after each)
| Optimization | How to run (NO code needed — direct llama-server, campaign agent owns GPU) | Proof it's active |
|---|---|---|
| **Flash attention** | already default `-fa on` | server log: `flash_attn = 1`; compare prompt-eval t/s `-fa on` vs `-fa off` |
| **Self-spec ngram** | `-m Qwen3.6-27B-TQ3_4S.gguf --spec-type ngram-cache -c 8192` | server log shows spec/ngram init; **t/s with vs without** on a repetitive prompt (e.g. code); `n_drafted`/`n_accepted` counters in `/slots` or logs |
| **Draft-model spec** (after 0.6B download) | `-m <27B> -md <qwen3-0.6B> --draft-max 16 -ngld all` | log `draft model` line + acceptance stats; t/s delta; NVML shows TWO models resident |
| **KV quant** | `-m <target> -ctk q4_0 -ctv tq3_0` (fork) | log `cache_type_k/v`; VRAM delta vs f16 KV; larger ctx fits |
| **MoE offload** | `-m Qwen3.6-35B-A3B-TQ3_4S.gguf -ncmoe <N>` | log MoE-on-CPU lines; NVML VRAM drops as N rises; model that OOMed at `-ngl all` now loads |
| **SWA (gemma-4)** | `-m gemma-4-12B... -c 32768` (NO `--swa-full`) | log SWA/`n_swa` lines; big ctx fits in 16 GB; then confirm `--swa-full` OOMs/uses far more VRAM (proves SWA saves it). **Watch for SWA+FA illegal-mem bug — test `-fa off` first, then `-fa on`.** |
| **Slot/prefix cache** | `--slots`, send same prefix twice | `/slots` endpoint shows cache reuse; 2nd request faster prompt-eval |

**Measurement harness:** `llama-bench.exe` (in the fork's `build/bin`) for clean t/s A/B, or the server `/slots` + timing on identical prompts. All are `--help`/bench/serve operations the campaign agent can run; this research task did none of them (no GPU used).

---

## Appendix — primary sources (all accessed 2026-07-07)
- Local `llama-server --help`/`--version`, both binaries — **authoritative for this box**.
- llama.cpp master `docs/speculative.md` — github.com/ggml-org/llama.cpp (shows `--spec-draft-*`, eagle3, dflash — NEWER than local b8833).
- llama.cpp releases — github.com/ggml-org/llama.cpp/releases (b9891–b9895, July 2026).
- `--swa-full` — github.com/ggml-org/llama.cpp/discussions/24543 (am17an).
- Gemma-4 SWA bugs — github.com/ggml-org/llama.cpp issues #22527, #21769; ik_llama #1607.
- vLLM spec decode — docs.vllm.ai/en/latest/features/speculative_decoding/.
- Ollama spec decode — github.com/ollama/ollama issues #9216, #5800 (+ 2026 secondary writeups, flagged).
- DSpark — fullstack.com blog + venturebeat 2026-06-30 + medium/braindetox/alphasignal (2026-06/07): DeepSeek+PKU, paper 2026-06-27, DeepSpec repo, NeMo support; NOT in llama.cpp/vLLM/Ollama.

**Re-verify triggers:** local binary upgrade (flag namespace may shift to `--spec-draft-*`); gemma-4 estimator calibration on real load; Ollama draft API stabilization; any `dspark`/`dflash` entry appearing in a llama.cpp `--spec-type` or vLLM `method` enum.

---

## 7. Gemma-4 iSWA estimator audit — FINDING (with numbers), 2026-07-08

**Method:** header-only metadata read of the real on-disk GGUF
`K:\projects\llm_models\models\yuxinlu1__gemma-4-12B-agentic-fable5-composer2.5-v2-3.5x-tau2-GGUF\gemma4-v2-Q4_K_M.gguf`
(7.38 GB) — **no model load, no GPU** — plus source reading of the vendored
`github.com/gpustack/gguf-parser-go@v0.24.1` (`go.mod`) and running LocalHub's own
wrapper (`internal/compat/gguf`) end-to-end at ctx 4096/8192/32768.

### 7.1 What the metadata actually says (ground truth from the file)

| key | value |
|---|---|
| `general.architecture` | **`gemma4`** |
| `gemma4.block_count` | 48 |
| `gemma4.attention.sliding_window` (n_swa) | 1024 |
| `gemma4.attention.sliding_window_pattern` | **bool[48]** `[T,T,T,T,T,F, …]` → **40 sliding + 8 global** (every 6th layer is global) |
| `gemma4.attention.head_count_kv` | **int32[48]** `[8,8,8,8,8,1, …]` → **sliding layers = 8 KV heads, global layers = 1 KV head** |
| `gemma4.attention.key_length` / `value_length` (global) | 512 / 512 |
| `gemma4.attention.key_length_swa` / `value_length_swa` (sliding) | 256 / 256 |

Gemma-4's KV is **heterogeneous per layer** — only the metadata describes it.

### 7.2 What gguf-parser-go v0.24.1 does — it MIS-models gemma-4 two ways

1. **No `gemma4` arch case.** `file_architecture.go` has switch cases for `gemma2`
   (pattern 2) and `gemma3` (pattern 6) but **none for `gemma4`**, so
   `AttentionSlidingWindowPattern` stays at its default `1` = "no SWA". The library
   would price **full-context KV on all 48 layers**.
2. **Per-layer `head_count_kv` array is dropped under `SkipLargeMetadata()`** — the
   option LocalHub's local parse uses (the P6.1 <2s perf fix, `localReadOptions`).
   With the array gone, `AttentionHeadCountKV` resolves to **0**, and KV bytes =
   `(k+v)·headsKV·… = 0`. **The library prices the KV cache at 0 MiB at every ctx.**

**Measured (LocalHub's actual wrapper output BEFORE the fix):**

| ctx | library KV | library VRAM (NonUMA) |
|---|---|---|
| 4096 | **0 MiB** | 8078.8 MiB |
| 8192 | **0 MiB** | 8086.8 MiB (the +8 is compute buffers, not KV) |

**Verdict: UNDERPRICES.** KV priced at 0 → a false `runs` that OOMs at high ctx —
the dangerous direction per CLAUDE.md ("no false `runs` that OOMs"). (For the record,
a *full*-metadata parse is ALSO wrong the other way: it reads scalar `head_count_kv`=8
+ scalar dim 512 + pattern=1 → ~3072 MiB @4K, an ~8× **over**-price. Neither library
path is correct for gemma-4.)

### 7.3 The correct iSWA KV (theoretical, from the layer split)

Per layer per token, f16 (2 B/elem): `(head_dim_k + head_dim_v) · head_count_kv · 2`.
Sliding layers are window-capped to `min(ctx, n_swa + n_batch)` cells (llama.cpp sizes
the SWA cache to the window plus a logical batch, default 2048 → 1024+2048 = 3072);
global layers hold the full ctx.

- **Sliding ×40:** `(256+256)·8·2 · 3072` = 24 MiB/layer × 40 = **960 MiB** (ctx-independent once ctx ≥ 3072).
- **Global ×8:** `(512+512)·1·2 · ctx` = `2048·ctx` B/layer × 8.
  - ctx 4096 → 8 MiB/layer × 8 = **64 MiB** → total **≈1024 MiB**
  - ctx 8192 → 16 MiB/layer × 8 = **128 MiB** → total **≈1088 MiB**
  - ctx 32768 → 64 MiB/layer × 8 = **512 MiB** → total **≈1472 MiB**

At 128K the global layers alone reach ~2.1 GiB (KV ~3.1 GiB total) — the omission the
library would make grows without bound in ctx, which is exactly why a 0-priced KV is a
real OOM hazard at long context, not just a rounding error.

### 7.4 The fix (ADR-002: wrap, don't fork) — `internal/compat/gguf/iswa.go`

When the model is SWA (`<arch>.attention.sliding_window` present) and our
metadata-derived iSWA floor exceeds the library's KV number, LocalHub adds the
shortfall to `VRAMMiB` and records the floor in `Result.CtxVRAMMiB` with a
human-readable note. Sliding layers priced window+batch (conservative), global layers
full-ctx; per-layer `head_count_kv`/pattern read straight from the GGUF (no invented
behavior). For local sources one extra full-metadata parse is done **only** for SWA
models (gated on the cheap parse's `AttentionSlidingWindow > 0`), so ordinary models
pay nothing.

**Measured (LocalHub wrapper AFTER the fix):**

| ctx | iSWA KV floor added | corrected VRAM |
|---|---|---|
| 4096 | **+1024 MiB** | 9102 MiB |
| 8192 | **+1088 MiB** | 9174 MiB |
| 32768 | **+1472 MiB** | 9606 MiB |

Layer split detected by the code: **40 sliding + 8 global of 48, n_swa=1024** — matches
7.1 exactly. Tests: `internal/compat/gguf/iswa_test.go` (synthetic fixture pins the
128 MiB hand-calc for a 6-layer model + global-only ctx scaling + non-SWA no-op + quant
scaling; a guarded real-file test pins the ~1 GiB @4K correction on the actual gemma-4).

**Net:** gemma-4-12B still fits comfortably on the 16 GB card at all contexts even with
the correct KV (weights ~7.4 GB + KV ≤ ~3 GiB at 128K), but the estimate is now honest
and the false-fit OOM risk at long context is closed. See `KNOWN-LIMITATIONS.md` for the
residual v1 caveats (batch-allowance assumption; other future SWA arches).

## 8. Addendum (2026-07-08): PagedAttention — covered automatically, nothing to build

Asked about by the owner; recording the answer so it isn't re-researched. PagedAttention
is vLLM's core KV-cache memory manager (block-paged KV, the original vLLM paper's
contribution) — it is **always on inside vLLM's engine and is not a flag** LocalHub
could pass or omit. LocalHub's only interaction with it is the one already documented:
vLLM pre-allocates `gpu-memory-utilization × total VRAM` for its paged KV pool, which is
exactly why the estimator prices vLLM by util×total and why co-residency needs a lowered
util (KNOWN-LIMITATIONS #3). llama.cpp and Ollama do NOT implement PagedAttention; they
use llama.cpp's own unified KV cache + slot management (covered in the §1 matrix rows
for slot/prefix cache and continuous batching). Status: knowledge-based summary of
stable vLLM architecture; re-confirm wording against docs.vllm.ai at the next engine
revalidation sweep (no behavior depends on it — there is nothing to enable or verify
beyond what limitation #3 already covers).
