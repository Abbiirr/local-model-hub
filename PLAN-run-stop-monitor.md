# PLAN — Run / Stop / Monitor Qwen3.6-27B MTP through LocalHub

> Integrate `Qwen3.6-27B-16GB-VRAM-MTP-mini-IQ4_XS.gguf` so LocalHub can admit, start, infer,
> monitor, drain, stop, and recover it with the validated llama.cpp MTP profile. Supervised
> external-process integration only (ADR-005), single box (ADR-007), no preemption (ADR-001).

## 0. Method and status of this revision

**Revision 3 (2026-07-25).** Every claim below was re-executed on this box in this session.
Claims are tagged **VERIFIED** (I ran it and saw the output), **REFUTED** (I ran it and the
previous revision's claim did not hold), or **UNVERIFIED** (not exercised — say so, don't infer).

The headline change: **Revision 2 concluded "this exact model/profile is not integrated" and
that no LocalHub-managed process was ever spawned. That is refuted.** A LocalHub-managed
start → HEALTHY → inference → drain → stop → NVML-verified reclaim now has live evidence
(§2). What Revision 2 got right is that MTP and the 64K q8/q4 profile are genuinely absent.

## 1. Verified baseline (re-run this session)

| Check | Revision 2 claim | Observed now | Verdict |
|---|---|---|---|
| `go build ./cmd/localhub` + `go build ./...` | PASS | exit 0, both | **VERIFIED** |
| `go vet ./...` | PASS | exit 0 | **VERIFIED** |
| Web `vitest run` | PASS 80/80 | 10 files, **80 passed** | **VERIFIED** |
| Web typecheck + production build | PASS | `tsc --noEmit` clean, `built in 1.85s` | **VERIFIED** |
| `gofmt -l` | 4 files | exactly those 4 files | **VERIFIED** |
| Full Go suite | **NOT GREEN** — `TestRealFile_EvaluateUnder2s` took ~3.12 s / ~2.12 s | **4 consecutive full-suite runs, all exit 0**, every package `ok` | **REFUTED** |

### The "failing test" does not fail — the number was misread

`TestRealFile_EvaluateUnder2s` exists (`internal/compat/evaluate_latency_test.go:24`) and its
assertion is `elapsed >= 2*time.Second` around **one `Engine.Evaluate` call** (lines 45-51).

- Isolated, 3 runs: **169.4 ms, 188.5 ms, 180.6 ms** — about 10× under the 2 s ceiling.
- Full-suite, 4 runs: **all green**. The `internal/compat` *package* wall time varied
  **0.968 s → 3.111 s**.
- **3.111 s is the package elapsed time printed by `go test` (`ok … internal/compat 3.111s`),
  which matches Revision 2's "about 3.12 s" exactly.** Package wall time covers every test in
  the package plus build/link; it is not the asserted `Evaluate` duration.

**Conclusion:** there is no baseline defect here and no wall-clock assertion to renegotiate.
M0.1/M0.2 in TODO.md were chasing an artifact of misread output. The only real baseline finding
is the 4 `gofmt` files, pre-existing and untouched by this work.

## 2. Verified live LocalHub evidence — managed lifecycle **LIVE PASS** (non-MTP, 8K)

Config-only, no code changed. `configs/config.yaml` (new, untracked) adds
`K:\projects\ai\local-model-hub\models` to `data.model_dirs` and pins the b10106 binary.
Daemon on `127.0.0.1:39876`, SQLite WAL, `health_timeout_seconds: 300`.

| Step | Evidence |
|---|---|
| Inventory | `inserted=1 updated=12 … model_count=13`; scanned as `gguf-qwen3-6-27b-16gb-vram-mtp-mini-iq4-xs`, `size_bytes 13671793728`, `presence present` |
| Engine detect | `10106 (1425386fd)` at the b10106 path |
| Compat verdict | `runs_degraded`, `max_context_that_fits 8192`, f16 est **15602 MiB** (does not fit), q4_0-KV est **14107 MiB** in **14168 MiB** usable |
| **Start** | `POST /api/instances {model_id, ctx:8192, kv_quant:true}` → **HTTP 201**, `state: HEALTHY`, pid 10748, port 40100, **43 s** to ready (03:19:51 → 03:20:34) |
| Launch flags (recorded) | `-ngl all -c 8192 --fit off -fa on -ctk q4_0 -ctv q4_0 --no-webui` |
| Monitor | `GET /api/instances/{id}` → HEALTHY; `vram_est_mib 14107` vs **`vram_obs_mib 13537`**; `GET /api/hardware` → `vram_used 14247, vram_free 1863, util 36, temp 50` |
| Inference (non-stream) | `POST /api/instances/{id}/completions` → `content: "INTEGRATION OK"`, `finish_reason: stop`, 232 completion tokens |
| Inference (stream) | SSE relayed intact, `system_fingerprint: "b10106-1425386fd"` — proof b10106 is the serving process |
| **Stop** | `DELETE` → **HTTP 202**, `state: DRAINING`, "async; detached from HTTP caller" |
| **NVML reclaim** | polled to `STOPPED`, `vram_freed_mib: 13518`, reason: *"stopped and VERIFIED: free VRAM rose by 13518 MiB (>= footprint 13537 − 500 slack) — process gone and VRAM reclaimed (NVML delta, not an HTTP-200 claim)"* |
| Cleanup | VRAM 14596 → **1105 MiB used**; no `llama-server` process; port 40100 only `TIME_WAIT`, not LISTENING |
| Web UI | `GET /` → HTTP 200, `<title>LocalHub Compass</title>`, embedded assets |

**Estimator is conservative in the safe direction:** predicted 14107 MiB, observed 13537 MiB
(**+570 MiB** of pessimism). No false `runs`. Satisfies the CLAUDE.md correctness bar.

### Corrections to Revision 2's live section

- **"A non-MTP 8K start was conservatively rejected with HTTP 409 (est ~14,107 MiB vs
  13,768 MiB usable)."** — **REFUTED as a hard refusal.** The 14107 MiB estimate reproduces
  exactly, but usable VRAM on my runs was **14153–14168 MiB**, so the same request was
  **admitted (201)**. The 409 was a transient consequence of ~400 MiB less free VRAM at that
  moment, not a standing block.
  **The fragile part worth keeping:** the margin is only ~50 MiB, so admission at 8K flips
  between 201 and 409 with ordinary desktop VRAM noise. That instability is a real finding and
  belongs in the estimator work (§4 E) — not a "not integrated" verdict.
- **"No LocalHub-managed process was spawned… managed inference, drain/stop, NVML reclamation
  are therefore not proven."** — **REFUTED**; all four have live evidence above.
- **`speculative:"draft-mtp"` → HTTP 400** — **VERIFIED**: reason is
  *"invalid speculative value \"draft-mtp\": valid values are \"\" (off, today's flags) or
  \"ngram-cache\""* (`compat.ValidateSpeculative`, `internal/compat/flags.go:34-42`).

## 3. Root cause Revision 2 missed: binary selection ignores config order

Revision 2's B-3 says to register b10106 as explicit primary `binary_path` because
"normalization prepends the default legacy `binary_path`." **That is not the mechanism, and
doing only that does not work.** Measured chain:

1. `compat.bestLlamaBinary` (`internal/compat/input.go:137-150`) returns the **first detected
   binary whose `HasCUDA` is true**, else any detected binary. **Config order is never consulted.**
2. **b10106 is detected as `has_cuda = false`.** Its `--version` prints only
   `version: 10106 (1425386fd)` / `built with Clang 20.1.8` — **no backend line at all**, because
   modern llama.cpp loads `ggml-cuda.dll` as a dynamic backend. The old fork's banner does print
   `ggml_cuda_init: found 1 CUDA devices`, so the fork wins on `HasCUDA`. `/api/engines` confirms:
   b10106 → `"backends": null`; fork → `"backends": ["CUDA"]`.
3. With all three binaries configured (b10106 first), LocalHub selected
   `…\llama.cpp-tq3\build\bin\llama-server.exe` and the start **failed**:
   ```
   llama_model_load: error loading model: missing tensor 'blk.64.ssm_conv1d.weight'
   ```
   The fork predates Qwen3.6's hybrid SSM/attention architecture. Result: HTTP 502,
   `state: FAILED` with the engine tail in `reason`, no VRAM leak, no orphan.
4. Because `bin.HasTQ3` was true, the fork was also handed **`-ctv tq3_0`** — a fork-only KV
   type — chosen off a capability bit, not off the request.

**Config-only workaround used for §2 (and its cost):** making b10106 the *sole* candidate
(`binary_paths: []`) forces the `anyDetected` fallback to pick it. **This globally disables the
TQ3 fork, so every TQ3_* model in the collection now gets a `wont_run` verdict** (no `has_tq3`
binary available). Fine for a test rig, **not** an acceptable end state — which is exactly why
capability-based selection (§4 D) is the real fix.

**Two honest failure-path passes fell out of this:** a crashing engine is reported as `FAILED`
with a human-readable reason plus a captured log, and it leaks neither VRAM nor processes.

## 4. Remaining integration work

### A. Baseline — **mostly closed**
Only the 4 pre-existing `gofmt` files remain, and only when those files are in scope. **Drop the
`TestRealFile_EvaluateUnder2s` investigation** (§1). Gate: build, vet, full Go suite, web 80/80,
typecheck, production build — all currently pass.

### B. Provenance and configuration
1. Keep the target scanned through the normal configured inventory path. **(done, §2)**
2. Store/link an immutable manifest: exact path, size, SHA-256, MTP metadata. **(open)**
3. **Replace** Revision 2's B-3: the fix is not "set `binary_path`" — it is (a) make backend
   detection recognise a modern llama.cpp that reports no backends in `--version`, and/or
   (b) select by required capability (§4 D). Record why b10106 currently loses.
4. Record b10106 version/commit (`10106` / `1425386fd`), SHA-256, re-verification cadence.
5. Retain older binaries as **capability-gated** fallbacks — noting that today removing them is
   the only way to reach b10106, which regresses TQ3 support.

Gate: a clean restart selects b10106 for this model **without** deleting the other binaries, and
TQ3 models still resolve to the fork.

### C. One durable launch-profile contract
Versioned profile: context, **independent K/V cache types**, speculative mode, MTP draft max,
parallel slots, flash attention, CPU-offload permission, projector, pin, priority. Thread it
through REST → core → `LaunchPlan` → UI → queue persistence/replay → jobs/JSON-RPC → instance
records → reproducibility output.

Confirmed shape of today's request (`core.StartInstanceRequest`, `internal/core/instances.go:48-69`):
`{model_id, engine?, ctx?, batch?, priority?, pin?, kv_quant?, allow_cpu_offload?, speculative?}`
— **`kv_quant` is a bare `bool`**, so `q8_0`/`q4_0` is not expressible from the API at all.

Known blocker (carried from Revision 2, **UNVERIFIED by me**): queued starts persist context,
batch, pin, priority but **not** `KVQuant`, `AllowCPUOffload`, `Speculative`; replay could
silently change behavior. Confirm against the queue schema before coding.

Gate: a queued request yields an identical normalized launch plan after a daemon restart.

### D. Capability-aware llama.cpp support
1. Probe the selected binary for `draft-mtp`, `--spec-draft-n-max`, and independent K/V types;
   add `has_spec_mtp` (+ the `--spec-draft-*` family). Today's capability object is exactly
   `{has_fit, has_flash_attn, has_sleep_idle, has_tq3}` — **verified, no spec/MTP/KV field**.
2. Fix backend/CUDA detection for binaries whose `--version` advertises nothing (§3.2).
3. Detect the embedded MTP tensors in the GGUF; permit MTP only when model **and** binary agree.
4. Emit the validated profile: `--spec-type draft-mtp --spec-draft-n-max 2`, `-np 1`.
5. **Generalize KV to an explicit (K-type, V-type) pair.** This *replaces* existing hard-coded
   asymmetry: `internal/compat/flags.go:80-89` already emits `-ctk <kvType>` + `-ctv tq3_0`
   **when `bin.HasTQ3`**, else the same type for both. Asymmetric KV is therefore not
   greenfield — it is currently derived from a capability bit instead of the request, and that
   is the bug to replace.
6. Select binaries by **required capabilities first, configured preference second**.
7. Human-readable `reason` on every refusal (already the norm — every refusal I triggered had one).
8. Treat `N=2`, `-np 1`, and the projector restriction as **pinned-build acceptance conditions**,
   not timeless llama.cpp facts.

Gate: table-driven tests over capable/incapable combinations, exact flags, and selection order.

### E. Honest admission
1. Size K and V **independently** at the requested context.
2. Include embedded MTP tensors and runtime overhead.
3. Preserve the live NVML re-read and all margins/floors (already correct, §2).
4. **Close the ~50 MiB knife-edge at 8K** (§2) so admission is not decided by desktop VRAM noise;
   measured pessimism today is +570 MiB at 8K q4_0.
5. Calibrate against real b10106 loads at safe contexts before attempting 64K.
6. Never bypass admission because a direct-engine load succeeded once.

Gate: zero false-positive `runs`; predicted vs observed baseline/peak/recovered VRAM recorded
per calibration.

### F. Truthful monitoring after startup
1. Add/verify periodic process + health reconciliation **after** `HEALTHY`. **UNVERIFIED — I only
   observed a start-time crash (correctly `FAILED`), never a post-HEALTHY death.**
2. Later exit/health failure → `FAILED` with a reason.
3. Keep PID, port, last-health time, exit reason, state, telemetry coherent.
4. Expose MTP acceptance **only** with a stable, tested signal. **UNVERIFIED:** per-instance logs
   are captured to `logs/llamacpp-<id>.log`, so scraping a `draft acceptance` line is plausible,
   but nothing emits it without MTP flags — do not promise this metric yet.

Gate: killing a disposable healthy process is reflected by API/UI within the documented interval,
without restarting LocalHub.

### G. Stop / reclaim semantics — **largely verified**
`DRAINING` preserved, async detached stop, and NVML-verified reclaim with an explicit
"VERIFIED … not an HTTP-200 claim" reason all confirmed (§2). Remaining, **UNVERIFIED**:
reclaim-**timeout** and reclaim-**inconclusive** paths must not report a clean `STOPPED`, and
active-stream drain (stop while a completion streams) is untested.

Gate: normal stop, active-stream drain, process failure, and reclaim-timeout all end truthful.

### H. Persist defaults and document
Save a model-specific default only after live gates pass; tie it to the pinned artifact + engine
build; update engine-adapter, compatibility, API, data-model/migration, and engine-version-policy
docs. Reconcile stale product/roadmap references separately.

## 5. End-to-end acceptance

One model at a time; preserve host/WSL/Docker headroom. **Steps 1-3, 6, 8 now have evidence at
8K non-MTP (§2)** — re-run per profile change; treat the rest as open:

1. Idle process/port/RAM/temperature/NVML baseline. **(done)**
2. Start through LocalHub: `REQUESTED → STARTING → HEALTHY`. **(done)**
3. One non-streaming and one streaming request. **(done)**
4. MTP-on vs MTP-off sanity/determinism for the pinned build. **(blocked on §4 D)**
5. Bounded multi-turn / long-session check. **(open)**
6. Observe state + hardware throughout. **(done)**
7. Queue an equivalent start, restart the daemon, prove profile preservation. **(open)**
8. Stop normally; verify drain, exit, port closure, NVML recovery. **(done)**
9. Crash-detection and reclaim-timeout negative paths. **(partial: start-time crash done;
   post-HEALTHY death and reclaim-timeout open)**
10. Raise context only after the prior tier is safe; 64K is the final target.
11. Record requests, flags, timings, VRAM prediction/observation, draft evidence, transitions,
    cleanup.

Final gate: classify as `LIVE PASS`, `VERIFIED REFUSAL`, or `BLOCKED`. **Current standing:
`LIVE PASS` for managed start/infer/monitor/stop/reclaim at 8K with q4_0 KV and no MTP;
`BLOCKED` for the MTP + 64K q8/q4 profile** pending §4 C/D/E. Direct llama.cpp evidence alone
remains insufficient.

## 6. Work order

`A (closed bar gofmt) → D2 backend detection + D6 capability selection → B provenance →
C durable profile → D1/D3/D4/D5 MTP + (K,V) → E estimator/knife-edge → F monitoring →
G negative paths → H defaults/docs → acceptance`

**D2/D6 moved to the front:** until selection stops ignoring config order, every start for this
model either uses the wrong binary or depends on deleting the other binaries (which breaks TQ3).

Do not persist 64K as a default until admission calibration and the managed lifecycle pass at
that context.

## 7. Decisions

- Prefer pinned b10106; retain older builds only as **capability-gated** fallbacks. The current
  `binary_paths: []` workaround is a test-rig expedient and must not ship.
- Draft max 2 and one slot apply **only** to the pinned, tested profile.
- No projector/multimodal promise without a separate live gate.
- No MTP acceptance metric unless the signal is stable and tested.
- **Open question for the owner:** fix `has_cuda` detection for modern llama.cpp, or drop
  `HasCUDA` as a selection criterion in favour of explicit capability requirements? The second is
  more robust; the first is smaller. (Supersedes the old TODO(agent) "retire b8833 or
  capability-gate" with the measured reason.)

## 8. Out of scope

- Cross-engine VRAM preemption / fair-share scheduling (ADR-001).
- Forking llama.cpp or linking CUDA into the control plane (ADR-005).
- Automatic Safetensors→GGUF conversion (ADR-004).
- Chat UI, gateway features, training, Kubernetes, cross-box execution.

## 9. Test-rig state left behind

- `configs/config.yaml` — **new, untracked, not committed.** Adds the models dir, pins b10106,
  `binary_paths: []` (⚠ disables the TQ3 fork globally), `health_timeout_seconds: 300`.
  Delete it to return to built-in defaults.
- One `FAILED` and one `STOPPED` instance row + two logs under `logs/`.
- No GPU memory held, no orphan engine process. The daemon may still be running on
  `127.0.0.1:39876` (`taskkill /IM localhub.exe /F` to stop).
