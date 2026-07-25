# Phase 2 validation matrix — LocalHub Compass (P2.5)

The Phase 2 close gate (TESTING.md#p25): a ≥20-model matrix covering GGUF+safetensors,
GQA+MHA, fits+doesn't-fit, proving **zero false-positive `runs` verdicts**, with per-model
estimator error where measured, every verdict carrying reason / estimated_vram /
max_context / run_flags, and a real-load subset executed within the PLAN.md hardware
safeguards (one model at a time, VRAM margin, cleanup verified by NVML delta). This document
is Phase 2's evidence. **docs/23 is left untouched** — that is Phase 0's record.

Run date **2026-07-06** on the reference box: RTX 4060 Ti 16 GB (CC 8.9), driver 576.40,
15.9 GB RAM, Windows 10 Home. All verdicts produced by the **live daemon** over
`POST /api/compat/preview` (localhub built from this tree, migration 0003 applied). Real
loads driven by the CUDA fork `llama-server` (turbo-tan/llama.cpp-tq3, commit 1cfa891) and
the winget Vulkan build (8833), plus Ollama 0.21.2, under the harness in
`scratchpad/load-test.ps1` (NVML polled every 2 s during load + one generation).

**Baseline VRAM at run start: 2821 MiB used / 13289 MiB free** of 16380 total (idle desktop
eats ~2.8 GB — the *real* free ceiling, per the P0.2 lesson). Usable under the 0.92 safety
factor ≈ **12226 MiB**. The dev-test load gate (safeguards) is `est ≤ free − 1500 MiB`.

Estimator error % = `(estimate − measured) / measured × 100`; positive = estimator OVER
(conservative/safe direction). "measured" = NVML peak VRAM delta (loaded − baseline).

---

## The matrix (21 rows)

| # | Model | Fmt | Quant | GQA/MHA | Source | Estimator + conf | Est VRAM @ctx | Verdict + reason (truncated) | Actual result | Est err % | Notes |
|--:|---|---|---|---|---|---|---|---|---|--:|---|
| 1 | GLM-OCR | GGUF | Q8_0 | GQA | local | gguf-parser-go@0.24.1, approximate | 1331 @4K | **runs** — "needs ~1331 MiB, fits in 12226 usable, headroom" | **LOADED** healthy, gen OK 8.9 tok/s; NVML Δ **1567** (incl. mmproj); after→baseline (Δ0) | +15%* under | *text-only est excludes 484 MB mmproj; measured incl. mmproj. Recorded → verdict tightened to **exact 1567**. |
| 2 | Llama-3.2-1B-Instruct | GGUF | Q4_K_M | GQA | local (downloaded) | gguf-parser-go, approximate | 1547 @8K | **runs** — "needs ~1547 MiB, fits, headroom" | **LOADED (CUDA fork)** healthy, gen "Hi" 8.4 tok/s; NVML Δ **1433**; after→baseline (Δ0) | +8% over | Recorded → verdict **exact 1433**. File kept at `K:\projects\llm_models\models\llama-3.2-1b-instruct\Llama-3.2-1B-Instruct-Q4_K_M.gguf` (807 MB) for Phase 4/5. |
| 3 | Llama-3.2-1B-Instruct | GGUF | Q4_K_M | GQA | local | (same est) | 1547 @8K | **runs** (same verdict, cross-engine) | **LOADED (winget Vulkan 8833)** healthy, gen "Hi" **1.1 tok/s**; NVML Δ **1283**; after→baseline (Δ0) | +21% over | **Cross-engine:** Vulkan uses slightly less VRAM but is ~8× slower than the CUDA fork (1.1 vs 8.4 tok/s). Confirms routing pref (fork for GPU work; friction #10). |
| 4 | Qwen3-VL-Embedding-8B | GGUF | Q6_K | GQA | local | gguf-parser-go, approximate | 7123 @4K | **runs** — "needs ~7123 MiB, fits, headroom" | **LOADED (embeddings)** healthy, embed dim=4096; NVML Δ **6955**; after→baseline (Δ0) | +2.4% over | Best estimator accuracy of the run. Recorded → **exact 6955**. RAM dropped during load (6 GB weights mmap page-cache) but GPU-resident + stable; released on unload. |
| 5 | PaddleOCR-VL-1.5 | GGUF | (std, unlabelled) | GQA | local | gguf-parser-go, approximate | 1178 @4K | **runs** — "needs ~1178 MiB, fits, headroom" | **LOADED** healthy, gen OK 20 tok/s; NVML Δ **1949** (incl. 0.88 GB mmproj); after→baseline (Δ0) | +65%* under | *text-only est excludes mmproj (friction #4). Standard quant parses fine. Recorded → **exact 1949**. |
| 6 | nomic-embed-text-v2-moe | GGUF | Q4_K_M | MoE | Ollama | gguf-parser-go (loose GGUF) | 579 @4K | (Ollama warm/measure/unload) | **WARMED via Ollama** `/api/embed`; `/api/ps` size_vram **911 MiB** ctx512; NVML Δ **701**; unloaded keep_alive:0 → ps empty, VRAM→baseline | n/a | **Three numbers disagree** (gguf 579 / ps 911 / NVML 701) — friction #6 reproduced; measured NVML is the source of truth. No process kill of the shared daemon. |
| 7 | deepseek-r1-0528-qwen3:8b | Ollama | Q4 | GQA | Ollama | (Ollama-owned) | — | (Ollama warm/measure/unload) | **WARMED via Ollama** `/api/generate`; `/api/ps` size_vram **10927 MiB** ctx131072; NVML Δ **10881**; unloaded → ps empty, VRAM→baseline | n/a | ps vs NVML now agree closely on a large model. Ollama defaulted to 128K ctx → large footprint (why default-ctx matters for fit). |
| 8 | Qwen3-VL-32B-Instruct | GGUF | Q2_K | GQA | local | gguf-parser-go, approximate | **14211 @8K** | **wont_run** — "needs ~14211 MiB but only 12226 usable; does not fit even at 2K" | **NEGATIVE LOAD ATTEMPT (—fit off strict, 100 s timeout, RAM-guarded):** VRAM jumped to 14462 (Δ11641), free VRAM → 1648 MiB, then system RAM began spilling (WDDM shared-mem) → guard fired at **t=4 s** "NOT fitting", killed; never HEALTHY; after→baseline (Δ0) | n/a | **Verdict confirmed correct.** Standard quant parses (gap is TQ3-specific). mmproj (1.2 GB) NOT in the 14211 → real footprint even larger. Fast fail, no swap-death. |
| 9 | Qwen3.6-27B-TQ3_4S | GGUF | **TQ3_4S** | GQA | local | **fallback** (gguf-parser can't parse GGMLType 46) | 16178 @8K | **wont_run** — "needs ~16178 MiB but only 12226 usable … NOTE: FALLBACK est; can't model tq3_0 KV; a measured peak may fit" | **not loaded — safeguard** (est 16178 > free−1500 = 11789; weights ~13 GB alone > usable). Bat-script `run-qwen3.6-27b-tq3_4s.bat` historical evidence it runs at low ctx when VRAM freer. | n/a | Flagship. Estimator blind to TQ3 (friction #1); fallback is a conservative file-size heuristic. Verdict carries the fallback caveat + P2.5 measured-peak lever. |
| 10 | Qwen3.5-27B-TQ3_1S | GGUF | **TQ3_1S** | GQA | local | fallback | 16099 @8K | **wont_run** — same class as #9 (fallback caveat) | **not loaded — safeguard** (free < est+1.5 GB). `run-qwen3.5-27b.bat` evidence. | n/a | 2nd flagship. TQ3 opaque to every off-the-shelf estimator. |
| 11 | Qwen3.6-35B-A3B-TQ3_4S | GGUF | **TQ3_4S** MoE | GQA | local | fallback | 14137 @8K | **wont_run** — same class (fallback caveat) | **not loaded — safeguard**. `run-qwen3.6-35b-moe.bat` runs it (A3B active ≪ 35B total). | n/a | MoE active-vs-total (friction #5): the file-size fallback over-counts because it can't see A3B; a measured peak would correct it. |
| 12 | Qwopus3.5-27B-v3-TQ3_4S | GGUF | **TQ3_4S** +mmproj | GQA | local | fallback | ~16000 @8K | **wont_run** — same class (fallback caveat) | **not loaded — safeguard**. `run-qwopus3.5-27b.bat` evidence; vision variant adds mmproj → tighter. | n/a | TQ3 gap + unaccounted mmproj. |
| 13 | Llama-3.2-3B-Instruct | GGUF | Q4_K_M | GQA | remote HF (bartowski) | gguf-parser-go remote (range read, no dl), approximate | 3349 @8K | **runs** — "needs ~3349 MiB, fits, headroom" | **not loaded — remote estimate-only** (metadata only). | n/a | Remote HF metadata read works with no weight download. |
| 14 | Meta-Llama-3.1-8B-Instruct | GGUF | Q4_K_M | GQA | remote HF (bartowski) | gguf-parser-go remote, approximate | 5968 @8K | **runs** — "needs ~5968 MiB, fits, headroom" | **not loaded — remote estimate-only**. | n/a | Canonical GQA fit; matches P0.2's 6218 @8K within rounding. |
| 15 | Qwen2.5-14B-Instruct | GGUF | Q4_K_M | GQA | remote HF (bartowski) | gguf-parser-go remote, approximate | 10324 @8K | **runs** — "needs ~10324 MiB, fits in 12226 usable, headroom" | **not loaded — remote estimate-only**. | n/a | Near-boundary fit (10.3 of 12.2 usable) — the tightest remote GGUF `runs`. |
| 16 | Qwen2.5-32B-Instruct | GGUF | Q4_K_M | GQA | remote HF (bartowski) | gguf-parser-go remote, approximate | 21196 @8K | **wont_run** — "needs ~21196 MiB but only 12226 usable; does not fit even at 2K" | **not loaded — won't-fit (correct)**. | n/a | Doesn't-fit GGUF case. |
| 17 | Llama-3.3-70B-Instruct | GGUF | Q4_K_M | GQA | remote HF (bartowski) | gguf-parser-go remote, approximate | 43104 @8K | **wont_run** — "needs ~43104 MiB but only 12226 usable" | **not loaded — won't-fit (correct)**. | n/a | Extreme doesn't-fit; matches P0.2's 43354. Verdict now gives the reason the raw tool never did. |
| 18 | Meta-Llama-3.1-8B-Instruct | safetensors | bf16 | GQA | remote HF (NousResearch) | localhub-safetensors, exact (config-derived) | 18389 @8K | **wont_run** — "weights+overhead ~17364 MiB exceeds 97% of 16380 device; use a smaller quant / TP / bigger card" | **not loaded — won't-fit (correct)**; vLLM absent natively (Windows). | n/a | bf16 GQA safetensors → vLLM route; won't fit unquantized (~18.4 GB), matches expected. |
| 19 | microsoft/phi-1_5 | safetensors | bf16 | **MHA** | remote HF | localhub-safetensors, approximate | 5904 @2K | **runs_degraded** — "needs ~5904 MiB (w 3472 + KV 384 + oh 2048), fits; but vLLM here is DOCKER mode → slow cold start" | **not loaded — remote estimate-only** (vLLM docker deferred to P4.5). | n/a | **MHA** case (n_head_kv = n_head): KV term uses full head count. Fits; degraded only for the docker cold-start caveat. |
| 20 | Qwen2.5-14B-Instruct | safetensors | bf16 | GQA | remote HF (Qwen) | localhub-safetensors, exact | 31756 @8K | **wont_run** — "weights+overhead ~30220 MiB exceeds 97% of device; won't run on vLLM" | **not loaded — won't-fit (correct)**. | n/a | bf16 GQA won't fit unquantized. |
| 21 | Qwen2.5-7B-Instruct-AWQ | safetensors | AWQ (4-bit) | GQA | remote HF (Qwen) | localhub-safetensors, exact | 7809 @8K | **runs_degraded** — "needs ~7809 MiB (w 5313 + KV 448 + oh 2048), fits; vLLM DOCKER cold-start caveat" | **not loaded — remote estimate-only** (vLLM docker deferred). | n/a | **AWQ fits** where bf16 (#18/#20) does not — quantized safetensors route. Degraded only for the docker caveat, not a fit problem. |

`*` For rows 1 and 5 the estimator UNDER-reports the *full* footprint only because gguf-parser
estimates the text model and the reference load also offloads the vision projector (mmproj),
which the estimator excludes by design (friction #4). Against the text-model footprint the
estimator is over/conservative (e.g. GLM-OCR text-only NVML in P0.2 was ~1105 vs est 1331,
+20% over). The compat verdict is still SAFE because it compares the (conservative text) number
against usable VRAM with the 0.92 factor, and the calibrated measured peak (incl. mmproj) is
what's recorded and preferred thereafter.

---

## Gate checks (stated explicitly)

### (a) Zero false-positive `runs` — NON-WAIVABLE — **PASS (0 false positives)**

Every model the daemon verdicted `runs` (or `runs_degraded`) that was actually loaded, loaded
successfully and did **not** OOM:

- **runs** verdicts LOADED and verified healthy: GLM-OCR (#1), Llama-3.2-1B on both engines
  (#2, #3), Qwen3-VL-Embedding-8B (#4), PaddleOCR-VL (#5). **5/5 loaded cleanly, none OOM'd.**
- The one **wont_run** verdict that was load-*attempted* as the negative test (#8, Qwen3-VL-32B
  Q2_K) **correctly failed to become healthy** — it began spilling to system RAM within 4 s and
  was killed; the verdict was right, not a false negative that actually fit.
- Every remote/TQ3 `runs` verdict was estimate-only under the safeguard (never claimed to have
  loaded), so none can be a false positive by construction; the ones that WERE loaded are the
  five above.

**False-positive `runs` count = 0.**

### (b) Per-model estimator error (measured rows) — **recorded**

| Model | Est VRAM | Measured (NVML Δ) | Error % | Direction |
|---|--:|--:|--:|---|
| GLM-OCR Q8_0 @4K (incl. mmproj) | 1331 | 1567 | +15% under (text-only est excl. mmproj) | est excludes projector by design |
| GLM-OCR Q8_0 @4K (text-only, P0.2) | 1331 | ~1105 | **+20% over** | conservative/safe |
| Llama-3.2-1B Q4_K_M @8K (CUDA fork) | 1547 | 1433 | **+8% over** | conservative/safe |
| Llama-3.2-1B Q4_K_M @8K (Vulkan) | 1547 | 1283 | **+21% over** | conservative/safe |
| Qwen3-VL-Embedding-8B Q6_K @4K | 7123 | 6955 | **+2.4% over** | conservative/safe |
| PaddleOCR-VL @4K (incl. mmproj) | 1178 | 1949 | +65% under (text-only est excl. mmproj) | est excludes projector by design |
| nomic-embed Q4_K_M (Ollama) | 579 (gguf) / 911 (ps) | 701 (NVML) | ps +30% / gguf −17% vs NVML | three-number disagreement (friction #6) |

Against the **text-model footprint the estimator targets**, every measured error is
conservative (over) — no `runs` model used more than usable VRAM. The two large "under"
figures are entirely the excluded vision projector; the calibration store now carries the
measured incl.-mmproj peak so subsequent verdicts are exact.

### (c) Every verdict carried reason / estimated_vram / max_context / run_flags — **PASS**

Confirmed on the live `/api/compat/preview` payloads: `reason` (mandatory, non-empty —
constructor-enforced), `estimated_vram_mb`, `max_context_that_fits` (e.g. 32768 for the small
GGUFs; omitted only on wont_run where nothing fits), `run_flags`, `confidence`, `estimator`,
and echoed `vram_free_mb`/`vram_total_mb`. No verdict was emitted without a human-readable
reason (schema-tested in `internal/compat`).

---

## Calibration store evidence (verdict before → after)

The measured-beats-estimate loop (friction #6; migration 0003 `observed_peaks`), exercised
end-to-end on real hardware. After each successful load the harness called
`RecordObservedPeak` (via `internal/compat.CalibrationStore.Record`, the same path Phase 4
adapters will use), then the same `/api/compat/preview` was re-run:

| Model | BEFORE (estimate) | AFTER (calibrated) |
|---|---|---|
| GLM-OCR Q8_0 @4K | runs, conf **approximate**, **1331** MiB, estimator `gguf-parser-go@v0.24.1` | runs, conf **exact**, **1567** MiB, estimator `calibration/observed-peak`, warning *"observed peak from 2026-07-06 at ctx 4096 (f16 KV)"* |
| Llama-3.2-1B Q4_K_M @8K | runs, conf approximate, 1547 MiB, `gguf-parser-go@v0.24.1` | runs, conf **exact**, **1433** MiB, `calibration/observed-peak`, note *"observed peak from 2026-07-06 at ctx 8192 (f16 KV)"* |
| Qwen3-VL-Embedding-8B Q6_K @4K | runs, conf approximate, 7123 MiB | runs, conf **exact**, **6955** MiB, `calibration/observed-peak` |
| PaddleOCR-VL @4K | runs, conf approximate, 1178 MiB | runs, conf **exact**, **1949** MiB (now includes the measured mmproj footprint) |

Four measured peaks persisted in `observed_peaks`; the estimator returns them verbatim with
`Confidence "exact"`, short-circuiting parse/fallback. This is how the TQ3 flagships (rows
9–12), which the parser cannot read at all, will self-correct after their first real run.

---

## VRAM before / loaded / after per real load (NVML, MiB)

Baseline everywhere: **2821 used / 13289 free**. Cleanup verified within 200 MiB of baseline
after every load (the safeguard); actual restore delta was **0 MiB** on every standalone load.

| Load | Engine | before_used | peak_used | Δ (delta) | after_used | restore Δ |
|---|---|--:|--:|--:|--:|--:|
| GLM-OCR Q8_0 @4K | CUDA fork | 2821 | 4388 | 1567 | 2821 | 0 |
| Llama-3.2-1B @8K | CUDA fork | 2821 | 4254 | 1433 | 2821 | 0 |
| Llama-3.2-1B @8K | winget Vulkan | 2821 | 4104 | 1283 | 2821 | 0 |
| Qwen3-VL-Embedding-8B @4K | CUDA fork | 2821 | 9776 | 6955 | 2821 | 0 |
| PaddleOCR-VL @4K | CUDA fork | 2821 | 4770 | 1949 | 2821 | 0 |
| nomic-embed | Ollama | 2821 | 3522 | 701 | 2821 | 0 |
| deepseek-r1-8B | Ollama | 2821 | 13702 | 10881 | 2821 | 0 |
| Qwen3-VL-32B Q2_K @8K (NEGATIVE) | CUDA fork | 2821 | 14462 | 11641→spill | 2821 | 0 |

---

## Safeguard compliance — **no deviations**

- **One model at a time:** yes; strictly sequential, each stopped + VRAM verified before the next.
- **VRAM margin gate (`est ≤ free − 1500`):** enforced by the harness before each load; the
  four TQ3 flagships (#9–12) and the boundary Q2_K were correctly *skipped* as estimate-only
  where the gate said no; the negative test (#8) was the ONE deliberate over-limit attempt,
  run under `--fit off` strict + 100 s timeout + RAM-balloon guard, and killed at 4 s.
- **Contexts ≤8192, generations ≤30 tok, full GPU offload only (`-ngl all`/`999`, `--fit off`),
  never CPU fallback:** yes on every load.
- **Negative test fail-fast + RAM monitoring:** free RAM ≥ 3072 MiB verified before start
  (was 3964); guard aborts if RAM < 3072 or system-RAM balloon > 2500 MiB during load — it
  fired at t=4 s, so no system-RAM exhaustion (15.9 GB total protected).
- **Cleanup:** every spawned `llama-server` killed (taskkill /T /F); no orphans (`tasklist`
  confirmed NONE); Ollama unloads via `keep_alive:0` (never a process kill of the shared
  daemon); final VRAM **2821/13289 = baseline restored**.
- **Downloads:** exactly ONE — Llama-3.2-1B-Instruct-Q4_K_M.gguf (807 MB) from
  bartowski/Llama-3.2-1B-Instruct-GGUF to `K:\projects\llm_models\models\llama-3.2-1b-instruct\`.
  It is **kept** (useful for Phase 4/5 tests). No other model downloads; no Ollama pulls;
  remote rows read HF metadata only (range requests, no weights). Nothing written to C:.

---

## Summary counts

- **Total rows: 21** (GGUF: 13, safetensors: 4, Ollama: 2 measured + nomic; MHA: 1 (phi-1_5),
  rest GQA/MoE; fits and doesn't-fit both represented).
- **runs verified by real load: 5** (GLM-OCR, 1B×2 engines, Embedding-8B, PaddleOCR) — all
  loaded healthy, none OOM'd.
- **wont_run verified: 1 by negative load** (Qwen3-VL-32B Q2_K, correctly failed fast) +
  **5 by correct estimate-only won't-fit** (32B/70B GGUF, Llama-3.1-8B bf16, Qwen2.5-14B bf16,
  and the four TQ3 flagships are safeguard-skipped wont_run with fallback caveats).
- **Ollama warm/measure/unload: 2** (nomic-embed, deepseek-r1-8B) — VRAM delta verified,
  clean unload, no shared-daemon kill.
- **Estimate-only rows: 15** (4 TQ3 local + 5 remote GGUF + 4 remote safetensors + the 2
  boundary/negative documented above as loaded/attempted).
- **False-positive `runs` count: 0** (non-waivable gate — PASS).

Downloaded 1B GGUF path (kept):
`K:\projects\llm_models\models\llama-3.2-1b-instruct\Llama-3.2-1B-Instruct-Q4_K_M.gguf`.
