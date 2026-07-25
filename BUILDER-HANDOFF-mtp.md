# Builder handoff — Qwen3.6-27B MTP integration (M-series)

Paste §0-§3 as the working prompt. Everything below was verified by execution on the
reference box on **2026-07-25**; do not re-derive it, but do re-verify any *engine capability*
claim against a current primary source before relying on it (CLAUDE.md rule).

---

## 0. Your task

Implement the **M-series** in `TODO.md` (source of intent: `PLAN-run-stop-monitor.md`), which
makes `Qwen3.6-27B-16GB-VRAM-MTP-mini-IQ4_XS.gguf` startable/stoppable/monitorable through
LocalHub **with MTP and an asymmetric KV profile**. Work top-to-bottom; do not close a later
gate while an earlier one is red.

**Read first, in order:** `CLAUDE.md` → `docs/product-brief.md` (non-goals) →
`docs/decisions.md` (ADRs) → `PLAN-run-stop-monitor.md` (verified state + per-item scope) →
the area doc for whatever you touch.

**Binding constraints (from ADRs — violating them wastes the work):**
- No cross-engine VRAM preemption / fair-share scheduling. Residency is coarse whole-model
  load/unload, always re-measuring free VRAM. [ADR-001]
- Engines are external supervised processes. Never fork/patch llama.cpp; never link CUDA into
  the control plane. Spawn b10106 and pass flags. [ADR-005]
- Single box. No cluster assumptions in data model/API/scheduler. [ADR-007]
- Don't reinvent the estimator: wrap `gguf-parser-go`. [ADR-002]
- Every user-facing verdict/refusal carries a human-readable `reason`.
- A false `runs` that OOMs is a correctness bug. Prefer conservative estimates + margins.
- **Owner hardware constraint (2026-07-11, still active):** do not load or benchmark any model
  ≥30B params on this box. The 27B target here is allowed; 32B VL / 35B-A3B stay
  estimate/refusal-only.

**Do not touch:** `PLAN.md` (project-wide plan), Phase 7 items P7.3-P7.5 (owner-stopped
2026-07-08), or anything in `PLAN-run-stop-monitor.md` §8 Out of scope.

---

## 1. Verified starting state — trust these, don't re-test

- `go build ./...`, `go vet ./...`, **full `go test ./...` (4 consecutive runs)**, web
  `vitest` 80/80, `tsc --noEmit`, production build: **all green**. The suite is NOT flaky —
  the previously reported `TestRealFile_EvaluateUnder2s` failure was package wall time misread
  as test duration; the asserted call runs in ~170-190 ms against a 2 s ceiling. Only 4
  pre-existing `gofmt -l` files remain (M0.3).
- **Managed lifecycle already works at 8K / q4_0 KV / no MTP** (LIVE PASS): `POST
  /api/instances` → 201 HEALTHY in 43 s → stream + non-stream inference → `DELETE` → 202
  DRAINING → STOPPED with `vram_freed_mib: 13518` NVML-verified. No leak, no orphan.
- Estimator errs **conservative**: predicted 14107 MiB vs observed 13537 MiB at 8K q4_0.
- `speculative` accepts only `""` / `"ngram-cache"`; `"draft-mtp"` → 400 with a good reason.
- Capability object is exactly `{has_fit, has_flash_attn, has_sleep_idle, has_tq3}` — no
  spec/MTP/KV field exists.
- Qwen3.6 is a **reasoning model**: output splits across `reasoning_content` and `content`, so
  a small `max_tokens` yields empty `content`. That is not a relay bug — don't "fix" it.

### The three defects you are actually fixing

1. **Binary selection ignores config order (root cause, blocks everything).**
   `compat.bestLlamaBinary` (`internal/compat/input.go:137-150`) returns the first binary with
   `HasCUDA`, never consulting config order. **b10106 is detected `has_cuda=false`** because its
   `--version` prints no backend line (modern llama.cpp loads `ggml-cuda.dll` dynamically),
   so the old TQ3 fork wins — and the fork dies on this model with
   `llama_model_load: error loading model: missing tensor 'blk.64.ssm_conv1d.weight'`
   (it predates Qwen3.6's hybrid SSM/attention arch).
   **`HasCUDA` has only 3 sites:** `internal/compat/input.go:118,144` and
   `internal/core/compat.go:180,189`. Small blast radius.
2. **Asymmetric KV exists but is derived from the wrong input.**
   `internal/compat/flags.go:80-89` already emits `-ctk <kvType>` + **`-ctv tq3_0` when
   `bin.HasTQ3`**, else the same type for both. So this is a **replacement**, not a new
   feature: the (K,V) pair must come from the request/profile, not a capability bit.
   `kv_quant` is a bare `bool` in `core.StartInstanceRequest`
   (`internal/core/instances.go:48-69`) — `q8_0`/`q4_0` is currently inexpressible end-to-end.
3. **Queue replay silently drops behavior-affecting fields — CONFIRMED, not theoretical.**
   `internal/db/migrations/0006_queue_start_context.sql` adds only `req_ctx`, `req_batch`,
   `req_pin` (priority survives via `priority`/`priority_class`). There is **no column for
   `kv_quant`, `allow_cpu_offload`, or `speculative`.** For this model that is a functional
   bug: a queued start replays as **f16 KV**, whose estimate is 15602 MiB and does not fit.
   Fix with an **additive** migration (`0009_*`); `docs/data-model.md` forbids destructive ones.

---

## 2. Order of work

**M3.7 + M3.6 first** — until selection is fixed, every start either picks the wrong binary or
depends on deleting the other binaries (which globally breaks TQ3 support).

1. **M3.7** backend/CUDA detection for builds that advertise nothing, and/or drop `HasCUDA` as
   a selection criterion. **See §3 — one decision needed before you code this.**
2. **M3.6** select by required capability first, configured preference second.
   Gate: b10106 is chosen for this model **without** removing the other binaries, and TQ3
   models still resolve to the fork.
3. **M1.2 / M1.4** manifest + pinned engine provenance (b10106 = `10106` / `1425386fd`).
4. **M2.x** durable launch profile (independent K/V types, spec mode, MTP draft max, slots,
   flash-attn, offload, projector, pin, priority) threaded REST → core → `LaunchPlan` → UI →
   **queue schema (additive migration)** → jobs/JSON-RPC → instance record.
   Gate: queued request replays to an identical normalized plan across daemon restart.
5. **M3.1-M3.5** capability probe (`has_spec_mtp`, `--spec-draft-*`, independent K/V), GGUF
   MTP-tensor detection, MTP gated on model **and** binary, emit
   `--spec-type draft-mtp --spec-draft-n-max 2 -np 1`, reasons for every refusal.
   Treat `N=2`, `-np 1`, and the projector restriction as **pinned-build acceptance
   conditions**, not timeless llama.cpp facts.
6. **M4.x** honest admission: size K and V independently, include MTP head + overhead, and
   **close the ~50 MiB knife-edge at 8K** (est 14107 vs 14153-14168 usable makes the same
   request flip between 201 and 409 on desktop VRAM noise).
7. **M5.x** post-HEALTHY process/health reconciliation; reclaim-timeout and
   reclaim-inconclusive must not report a clean `STOPPED`; active-stream drain.
8. **M6.x** persist the profile as this model's default only after live gates pass; update
   `docs/engine-adapters.md`, `docs/compatibility-engine.md`, `docs/api-spec.md`,
   `docs/data-model.md` + migration notes, `docs/22-engine-version-policy.md`.

**Ground truth for the target profile** (measured standalone, outside LocalHub): 64K with
`-ctk q8_0 -ctv q4_0 --spec-type draft-mtp --spec-draft-n-max 2 -np 1` uses ~13.7 GB leaving
~2.4 GB free, ~25 tok/s; 32K q8/q8 ~33 tok/s; draft acceptance 0.84-0.90. **Do not persist 64K
as a default until admission calibration and the managed lifecycle pass at that context** —
climb 8K → 16K → 32K → 64K.

---

## 3. Decide this before coding M3.7

`HasCUDA` is doing two jobs: "can this binary use the GPU" and "prefer this binary." Options:

- **(A) Recommended — drop `HasCUDA` from selection; select on required capabilities, then
  configured order.** Config order becomes meaningful (which is what operators expect), MTP
  and TQ3 both become explicit requirements, and it removes the class of bug where a newer
  binary loses to an older one for cosmetic banner reasons. Larger diff, 3 call sites.
- **(B) Smaller — fix detection so a modern build reports `has_cuda=true`** (e.g. probe
  `--list-devices`/device output, or infer from a co-located `ggml-cuda.dll`). Keeps current
  preference logic, but leaves selection dependent on banner text that upstream changes freely.

**Recommendation: do (A), and additionally fix detection as in (B)** so `/api/engines` stops
reporting a CUDA build as backend-less — the two are complementary, and (B) alone would leave
selection order still ignoring config. Confirm with the owner before starting; it is the one
open question in `PLAN-run-stop-monitor.md` §7.

---

## 4. Test-rig state to clean up or inherit

- **`configs/config.yaml` is untracked and a deliberate expedient.** It adds the models
  dir, pins b10106, sets `health_timeout_seconds: 300`, and sets **`binary_paths: []`, which
  globally disables the TQ3 fork** — every TQ3_* model currently returns `wont_run`. Once M3.6
  lands, restore the fallbacks and delete that workaround; M1.GATE explicitly requires b10106
  to win *without* deleting the other binaries.
- One `FAILED` and one `STOPPED` instance row plus logs under `logs/`.
- A daemon may still be listening on `127.0.0.1:39876` (`taskkill /IM localhub.exe /F`).

## 5. Handoff gaps you should close early

- **The M-series has no `TESTING.md` entries.** Every P-task carries `Gate: TESTING.md#pXX`
  with explicit verification criteria; M0-M6 reference `PLAN-run-stop-monitor.md` gates
  instead. Add `TESTING.md#m0..#m6` before claiming any M gate, so the criteria are written
  down before the code that satisfies them.
- **Git is read-only for agents** (user's global rule): never commit, push, rebase, or reset.
  Staging (`git add`) requires explicit per-time approval. The owner commits.
