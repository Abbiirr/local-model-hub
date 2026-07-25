# C1 Master-Gate Verification Walk

**Date:** 2026-07-07 (local +06:00)
**Agent:** C1 verification agent (read-only; no code/docs/gates edited — this evidence file is the only new artifact).
**Contract executed:** temp-research/c1-master-gate-audit.md Parts A+B ("Verification step" column) + docs/14-closing-gates.md master gate + non-waivable gates.
**Binary:** `localhub-c1.exe` built fresh from `./cmd/localhub` (Go 1.23, `BUILD OK`).

## Configuration summary (scratch, deliberately isolated)

| Knob | Value | Note |
|---|---|---|
| bind / port | 127.0.0.1 : **39911** | loopback only |
| db_path | scratch `c1walk.db` | **fresh DB** (deleted before each daemon start) |
| model_dirs | `K:\projects\llm_models\models` | GGUF-only on this box |
| vram_budget_mib | **4500** | caps residency admission well below the ~13 GB physically free |
| min_free_ram_mib | **512** | **deliberately low** so the RAM floor does NOT interfere with the controlled storm (documented deviation from the 2048 default) |
| drain_timeout_seconds | 5 | |
| drain_interval_seconds | 5 | |
| jobs.enabled | true | |
| engines.llamacpp | enabled (CUDA fork `llama.cpp-tq3`) | |
| engines.ollama | **disabled** | ⇒ zero Ollama rows in inventory (asserted below) |
| engines.vllm | disabled | |

## Hardware envelope (safeguards)

| Moment | Free RAM | VRAM used / free |
|---|---|---|
| Pre-flight | 3,599,512 KiB ≈ **3.43 GB** (≥ 2.5 GB → storm ALLOWED) | **2969 MiB** used / 13141 free (my baseline) |
| Post-teardown | 3,977,764 KiB ≈ 3.79 GB | **2975 MiB** used / 13135 free (baseline +6, within ±200) |

Baseline used includes ~2900 MiB of the user's desktop apps (expected). Max 2 models resident at any time. Small models only. No `wsl --shutdown`, no user processes killed.

---

## Check A — Master criterion #1 (inventory + fit verdicts), no GPU load — **PASS**

`GET /api/models` on the fresh-DB daemon: **10 GGUF rows, all `source=local_path`, engine=none, ZERO Ollama rows** (Ollama disabled — asserted). No HF/safetensors rows: the configured `model_dirs` physically contains only GGUF files on this box, so none can appear — the scanner's HF/safetensors support is proven separately by the P2 matrix rows 18–21 (per audit). Recorded as a **data-availability limitation, not a scanner defect.**

`GET /api/models/{id}/compat` for 3 models — every required field present:

| Model | status | reason | estimated_vram_mb | max_context_that_fits | run_flags |
|---|---|---|---|---|---|
| Llama-3.2-1B-Q4_K_M (fits) | `runs` | ✓ non-empty | 1547 | 32768 | `-ngl all -c 8192 --fit off -fa on` |
| Qwen3-VL-32B-Q2_K (won't) | `wont_run` | ✓ "needs ~14211 MiB … does not fit even at 2K" | 14211 | n/a (nothing fits) | n/a (no launch for wont_run) |
| Qwen3.6-35B-A3B-**TQ3_4S** (fallback) | `wont_run` | ✓ + explicit **TQ3 FALLBACK NOTE** + `warnings[]` | 14137 | n/a | n/a |

- TQ3 fallback row carries `confidence:"fallback"`, `estimator:"gguf-parser-go@v0.24.1+fallback-heuristic"`, and a `warnings` array flagging the tq3_0 KV under-count — exactly the friction-log-documented behavior.
- **Cross-check (wont_run vs live free VRAM):** 32B `estimated_vram_mb=14211` **>** nvidia-smi free VRAM **13135 MiB** (and > 16380 total). Verdict also self-reports `vram_free_mb:13135` matching nvidia-smi exactly → estimator reads LIVE free VRAM. Refusal is physically correct.
- Note: the compat `reason` string showed mojibake (`×`, `−` mangled) over one curl pipe; the SAME characters render cleanly in Check F. This is a console/pipe encoding artifact, **not** corrupt stored data.

**Justification:** every verdict carries non-empty reason + estimated_vram + (where applicable) max_context_that_fits + run_flags; wont_run correctly exceeds live free VRAM.

## Check B — Master criterion #5 / non-waivable #3 (reproducible manifest), no GPU — **PASS**

Fresh-DB discovered files have NO manifest by design: `GET /api/models/{1B}/manifest` → **404 `no_manifest`** with reason "discovered on disk … provenance is unknown"; reproduce → 200 with explicit provenance-unknown text. No real localhub DB exists anywhere on this machine (searched repo root, K:, AppData) — so no pre-existing minted manifest.

To prove the full immutable-manifest path **live** (no network), I drove a `local_import` of the on-disk 1B GGUF via `POST /api/downloads {kind:local_import}` → job `state:succeeded`. The minted manifest (`GET /api/models/{id}/manifest`, HTTP 200) carries:

- `source: "local_import"`, `format:"gguf"`, `quantization:"q4_k_m"`
- `files[]` with per-file **`sha256: 6f85a640…df83`** + `size_bytes`
- `security_warnings[]`: **(1)** "No license detected…" **(2)** explicit **local-import provenance warning** ("origin unverifiable… recorded sha256 identifies these exact bytes only")
- `downloaded_at`, `local_path`

`GET /api/models/{id}/reproduce` (HTTP 200) carries: `source`, `provenance` (explicit local-import text substituting for the unknown repo/commit), `files[]` with sha256, `primary_sha256`, `size_bytes`, `warnings[]`.

**Justification:** For a local import there is legitimately no upstream repo/revision/commit; the manifest correctly substitutes the explicit local-import warning — precisely what criterion #5 / non-waivable #3 allows ("…or explicit local-import warning"). File list + sha256s + license field + warnings all present.

## Check C — Master criterion #3 / non-waivable #2 (never-kill-active), the live storm — **PASS**

The one missing live proof: a **non-pinned** active stream protected by **in-flight active counting** (not a pin). Verified in code first: proxy `BeginInstanceRequest` calls `InFlight.IncrementActive(id)` / `DecrementActive(id)` around every relayed request (internal/core/proxy.go:87–99); eviction `classifyVictim` marks any instance with `tracker.ActiveCount(inst.ID) > 0` **Ineligible** = "has active in-flight requests (never kill an active stream)" (internal/residency/eviction.go:132–133). This is the P6.1 wiring the audit said was owed — it is present.

**Definitive run** (fresh DB, single-process orchestrator firing the competitor from inside the live stream reader at chunk 30 to guarantee overlap):

| Event | epoch | detail |
|---|---|---|
| start A (1B, **pin:false**) | 830.44 | HEALTHY port 40100 |
| start GLM (**pin:false**, idle victim) | 834.97 | HEALTHY port 40101 (both coexist, `load_new` no eviction) |
| stream_open on A (`max_tokens:20000`, stream) | 834.97 | |
| **fire D (PaddleOCR, doesn't fit) at chunk 30** | 836.73 | stream provably LIVE |
| **eviction re-read, victim = GLM** | **844.23** | INSIDE stream window; NVML delta re-read |
| D reaches HEALTHY | 864.43 | port 40101 (GLM's freed port) |
| **stream_end** | **875.63** | **7511 chunks, done=True, error=None** |

Timing proof: eviction (844.23) is **inside** [stream_open 834.97, stream_end 875.63] and 7.5 s **after** D fired; **the stream continued 31.4 s AFTER the eviction.** So D's admission+eviction ran while A was actively streaming.

Verbatim daemon log (definitive run):
```
residency eviction re-read  victim=9e8dbb43…(GLM)  freed_expected_mib=1611 freed_measured_mib=2311 free_after_reread_mib=2953
residency admission decision  decision=evict_then_load  "evicted 1 idle instance(s) [9e8dbb43…]; free VRAM re-read after each eviction is now 2953 MiB >= footprint 1258 + 1024 margin (2282 MiB) — loading"
```

Final states: **A (7f7a2ce2) HEALTHY, pid 14104 unchanged** (survived); **GLM (9e8dbb43) STOPPED** ("VERIFIED: free VRAM rose by 2311 MiB"); D HEALTHY. Both A and GLM were **non-pinned** — A was protected purely by ActiveCount, GLM (idle) was the victim.

**No OOM** at any point (non-waivable #1 held: A + GLM + D loads all HEALTHY, none OOM'd).

### Honest note on prior attempts (not failures of the gate)
Two earlier attempts did NOT achieve live overlap because the 1B on this GPU generates ~180–400 chunks/s and hit natural EOS in 3–9 s — the competing start fired **after** the stream had already ended, so A (then idle + LRU) was legitimately evicted in one attempt. This reproduced the exact P5.3 timing gap the audit documented. The gate itself never failed: the mechanism was corrected by firing the competitor from inside the live stream reader (chunk-30 threaded fire) + a longer generation, yielding the clean overlap above. An additional independent live eviction (run 1, victim GLM `7167fe6b`, `freed_expected=2314 freed_measured=2311 free_after_reread=3053`) is preserved in `c1daemon-run1.log`.

**Justification:** live eviction with NVML re-read between evict and load, victim was idle/non-pinned, streaming non-pinned instance survived intact via ActiveCount, no OOM. Closes master #2 (live eviction) and master #3 / non-waivable #2 (never-kill-active for **non-pinned** streams).

## Check D — Non-waivable #4 (localhost bind only), no GPU — **PASS**

While the daemon + 2 child llama-servers were live:
```
TCP  127.0.0.1:39911  LISTENING  22664   (daemon)
TCP  127.0.0.1:40100  LISTENING  14104   (child llama-server A)
TCP  127.0.0.1:40101  LISTENING  3648    (child llama-server D)
```
`netstat` for `0.0.0.0:(39911|40100|40101)` → **NONE**. Every child launch_flags array contains `--host 127.0.0.1`, so the llama.cpp adapter forces loopback, overriding the reference .bat default of HOST=0.0.0.0.

**Justification:** daemon and all children bind 127.0.0.1 only; no public bind.

## Check E — Non-waivable #5 & #6 (grep/code audits), no GPU — **PASS** (both)

**#6 no auto Safetensors→GGUF conversion in core.** `grep -ri "convert_hf|convert.*to.*gguf|safetensors.*gguf"` over `internal/` returns only: (a) a **user-facing advice string** in a `wont_run` verdict — "convert to GGUF for llama.cpp, or use a newer GPU" (internal/compat/safetensors_path.go:36); (b) routing/manifest format checks; (c) a test asserting safetensors **never** produces a GGUF choice (engine_test.go:257 `TestSafetensors_RoutesToVLLM_NeverGGUF`). There is **no conversion code path** — routing is by format, safetensors→vLLM only (ADR-004). **Verdict: PASS.**

**#5 no cross-engine VRAM preemption / per-layer control.** `internal/residency` eviction operates on whole `instances.Instance` units: `classifyVictim`/`PickVictims` reclaim `footprintMiB(inst)` (the entire model) and admission "executes stops ONE AT A TIME, re-reading free VRAM after each" (eviction.go:148–149). No per-layer/`-ngl N`/memory-fraction control anywhere in residency. The only `gpu_memory_utilization` references are the vLLM adapter's whole-container fraction (an engine-owned knob LocalHub reads, not a preemption LocalHub performs). Eviction = whole-model unload only. **Verdict: PASS.**

## Check F — Non-waivable #7 (human-readable reasons on P6 surfaces), no GPU — **PASS**

All three P6 surfaces, un-fittable 32B model, each carries a human-readable + numeric reason:

- **Proxy 503** (`POST /v1/chat/completions`): HTTP **503**, `error.message` = "REFUSED: compatibility verdict is \"wont_run\" … needs ~14211 MiB at 8K ctx but only 8805 MiB usable (16380 × 0.92 − 6809 in use) …", `code:"admission_blocked"`, `type:"insufficient_vram"`.
- **Job failure** (`POST /api/jobs` → poll): `state:"failed"`, `error:"admission"`, `reason` = same numeric REFUSED string.
- **JSON-RPC error** (`POST /api/rpc instances.start`): `error.code:-32000`, **`error.data.reason`** = same numeric REFUSED string (JSON-RPC 2.0 shape).

(The live numbers vary — e.g. "8812 usable, 6802 in use" — because A+D were resident, correctly shrinking available VRAM.) The `×` renders cleanly here, confirming Check A's mojibake was a pipe artifact.

**Justification:** all P6 refusal surfaces carry human-readable, numeric reasons.

---

## Teardown proof

- `DELETE /api/instances/{id}` for both HEALTHY instances → all rows STOPPED.
- VRAM after DELETE: **2969 MiB** used (exact baseline, delta 0).
- Daemon killed (taskkill pid 22664); port 39911 **NOT LISTENING**.
- Orphan check: **NO llama-server processes** remain.
- Final VRAM: 2975 MiB used (baseline +6, within ±200). Final free RAM 3.79 GB.

## Verdict table

| Check | Gate(s) | Verdict |
|---|---|---|
| A | Master #1 (inventory + verdicts) | **PASS** |
| B | Master #5 / non-waivable #3 (manifest) | **PASS** (via live-minted local_import manifest) |
| C | Master #2 + #3 / non-waivable #1,#2 (live evict + never-kill-active) | **PASS** |
| D | Non-waivable #4 (localhost bind) | **PASS** |
| E | Non-waivable #5 (no preemption/per-layer) + #6 (no auto conversion) | **PASS** (both) |
| F | Non-waivable #7 (human-readable reasons) | **PASS** |

**Nothing blocks C1 from the checks in this walk.** Master #4 (phone-over-Tailscale) remains the sole BLOCKED-user item (elevated `Restart-Service Tailscale` + phone check) and is out of scope for this GPU/API walk. This walk upgrades the two previously-PARTIAL master criteria (#2 live eviction, #3 non-pinned active-stream survival) to demonstrated PASS on real hardware, and re-confirms all seven non-waivable gates.
