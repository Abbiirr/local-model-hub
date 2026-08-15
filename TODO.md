# Todo

Work top-to-bottom. One task = one delegated unit of work. Do not start a phase before
the prior phase's gate task is closed. Non-waivable gates in PLAN.md apply to every task.

## Qwen3.6-27B MTP run/stop/monitor integration

Source: `PLAN-run-stop-monitor.md`. Work top-to-bottom; do not close a later gate while
an earlier one is red.

### M0 — Baseline — CLOSED except M0.3 (verified 2026-07-25)

- [x] ~~M0.1 Reproduce/profile `TestRealFile_EvaluateUnder2s` in the full Go suite.~~
  **NOT A DEFECT.** 4 consecutive `go test ./...` runs all exit 0. The asserted `Evaluate`
  call takes 169–188 ms against a 2 s ceiling. The "3.12 s" in the prior plan revision is the
  `internal/compat` **package** wall time (`ok … internal/compat 3.111s`), misread as the test
  duration. See PLAN-run-stop-monitor.md §1.
- [x] ~~M0.2 Fix the implementation/fixture/contention cause…~~ **Nothing to fix** (see M0.1).
- [ ] M0.3 Resolve the four `gofmt -l` findings when in scope. (Confirmed still exactly 4:
  `internal/cluster/registry_test.go`, `internal/jsonrpc/{jobs.go,dispatcher_test.go,cluster_test.go}`.)
- [x] M0.GATE Build, vet, full Go tests, web 80/80, typecheck, production build — **ALL PASS**
  (verified 2026-07-25).

### M1 — Provenance and configuration

- [x] M1.1 Scan the target GGUF through the normal configured inventory path. (Done via
  `data.model_dirs += …\local-model-hub\models`; scanned as
  `gguf-qwen3-6-27b-16gb-vram-mtp-mini-iq4-xs`, `model_count=13`.)
- [ ] M1.2 Create/link an immutable manifest with path, size, SHA-256, and MTP metadata.
- [x] M1.3 **REWRITTEN — setting `binary_path` is NOT sufficient and was the wrong diagnosis.**
  `compat.bestLlamaBinary` (`internal/compat/input.go`) picked the first binary with
  `HasCUDA` and **never consulted config order**. b10106 reports `has_cuda=false` (its `--version`
  prints no backend line; modern llama.cpp loads `ggml-cuda.dll` dynamically), so the old TQ3
  fork won and then died on this model with
  `missing tensor 'blk.64.ssm_conv1d.weight'` (fork predates Qwen3.6's hybrid SSM arch).
  Real fix = M3.7 + M3.6 — **LANDED 2026-07-25** (see below). The config-only workaround
  (`binary_paths: []`, which ⚠ globally disables TQ3 support) is no longer needed once the
  fallback binaries are restored.
- [~] M1.4 Record b10106 version/commit (`10106` / `1425386fd`), SHA-256, re-verification cadence.
  **PARTIAL 2026-07-25:** version + commit are recorded and now LIVE-reported by `/api/engines`
  (`10106 (1425386fd)`, backends CPU/CUDA/RPC), and `model_launch_defaults` (migration 0011) has
  `engine_version`/`engine_commit`/`binary_path`/`artifact_sha256` columns so a saved profile is bound
  to the build that validated it. Re-verification cadence is documented as "re-run the `--help` diff
  after any engine upgrade" in the `Capabilities` doc comment. **Still open:** the SHA-256 of the
  binary and of the GGUF artifact are not computed/stored yet.
- [x] M1.5 Keep older binaries only as capability-gated fallbacks. **DONE + LIVE-VERIFIED 2026-07-25:**
  all three binaries are configured and detected; selection is capability-first, configured-order
  second. `/api/engines` shows the fork retaining `has_tq3=true` (so TQ3 models still resolve to it)
  while only b10106 reports `has_spec_mtp=true` (so a draft-mtp profile can only select b10106).
- [ ] M1.GATE Clean restart selects b10106 for this model **without deleting the other
  binaries**, and TQ3 models still resolve to the fork.
  **STATUS 2026-07-25:** selection logic LANDED + table-test PASS (M3.6/M3.7): b10106 configured
  first wins for a non-TQ3 model even with a has_cuda fork present; TQ3 models still resolve to
  the fork (`TestBestLlamaBinary_*`, `TestPlanLlamaLaunch_SelectsFirstConfiguredNonTQ3`,
  `TestEvaluateGGUF_NonTQ3_SelectsFirstConfigured`).
  **LIVE-VERIFIED 2026-07-25 (M1.GATE closed):** with the fork + winget build restored as
  fallbacks in `configs/config.yaml`, `/api/engines` reports b10106 as
  `backends: [CPU, CUDA, RPC]` (was `null` — the M3.7(B) `backendsFromDir` inference works),
  the non-TQ3 Qwen3.6 MTP model resolves to engine choice **"CUDA build"** (b10106, chosen by
  configured order, NOT by deleting anything), and the TQ3_4S model still resolves to
  **"CUDA fork (has_tq3)"**. The `binary_paths: []` workaround is gone.

### M2 — Durable launch profile

- [x] M2.1 Define/version a profile with context, independent K/V types, speculative mode, MTP draft
  maximum, slots, flash attention, CPU offload, projector, pin, priority. **DONE 2026-07-25:**
  `compat.LaunchProfile` + `StartInstanceRequest.{kv_type_k,kv_type_v,slots,spec_draft_max}`.
  `kv_quant` keeps working as a documented SHORTHAND for `q4_0/q4_0`; an explicit pair wins. All
  values validated by `validateStartProfile` BEFORE engine dispatch (so a bad value is refused
  identically for every engine and nothing spawns/queues); `tq3_0` refused because the estimator
  cannot price it. Flash-attn stays derived from the binary capability, not a profile field (v1).
- [x] M2.2 Thread it through REST, core, `LaunchPlan`, instances, and reproducibility. **DONE** — one
  profile per start feeds BOTH the verdict and `PlanLlamaLaunch`, so the admitted footprint and the
  spawned flags cannot diverge; `LaunchPlan.Profile` carries the normalized profile back out.
- [x] M2.3 Add model-default persistence and a UI advanced start form. **DONE 2026-07-25:**
  migration `0011_model_launch_defaults` + `GET/PUT/DELETE /api/models/{id}/launch-default`, and
  `web/src/components/AdvancedStartForm.tsx` (independent K/V selects, `-np`, spec mode + draft max,
  Load saved / Save as default / Clear). Collapsed by default and summarising an armed profile so it
  is never silently in effect; blank fields are OMITTED from the request so an untouched form sends
  the byte-identical body it sent before. **A saved default is ADVISORY** — the start path never
  reads it (M6.1 forbids saving one until live gates pass, and auto-apply would change what an
  unspecified field means). Defaults are provenance-bound and report `stale_reason` when the
  validated engine build is no longer detected (M6.2). Web 101/101, typecheck + prod build clean.
- [x] M2.4 Persist every behavior-affecting field in the queue schema. **DONE 2026-07-25:** additive
  migration `0009_queue_launch_profile` (`req_kv_type_k`, `req_kv_type_v`, `req_slots`,
  `req_spec_mode`, `req_spec_draft_max`, `req_allow_cpu_offload`; every read COALESCEd so
  pre-migration rows load as "no advanced knobs"). `residency.ProfileSpec` threads them through
  `Request`/`AdmitSpec`/`QueueRow`/`DrainRequest` + `RestoreWaiting`, and
  `residencyStarter.StartQueued` — the actual site of the live bug, which dropped kv_quant,
  allow_cpu_offload AND speculative — now rebuilds via the single `startRequestFromDrain`
  translation. The legacy bool is resolved to the (K,V) pair ONCE at enqueue, so the queue never
  holds two representations that can disagree after a restart.
- [x] M2.5 Preserve the profile through jobs and JSON-RPC admission. **DONE** — JSON-RPC
  `instances.start` unmarshals straight into `StartInstanceRequest`, so the new fields flow through;
  jobs/proxy deliberately start with default profiles (they accept no profile from the caller).
- [ ] M2.6 **NOT DONE — the launch profile cannot express the proven script's full flag set.**
  `scripts/run-qwen-64k-q4q4.ps1` is the profile that actually runs 64K on this box, and LocalHub can
  reproduce only PART of it. Until these land, a UI-launched instance is **not** byte-identical to the
  script's, so "one unified system" is not yet true. Each needs a `compat.LaunchProfile` field, a
  `StartInstanceRequest` field, flag emission in `internal/compat/flags.go`, a UI control in
  `web/src/components/AdvancedStartForm.tsx`, and — for anything behaviour-affecting — a column in
  migration 0009 (queue replay) and 0011 (saved defaults).

  - [ ] **M2.6a `--reasoning on` / `--reasoning-budget N`.** Controls whether the model emits its
    reasoning section and where it is cut off. **Behaviour-affecting, not VRAM-affecting.** This
    matters for Qwen3.6 specifically: M6.4 already recorded that its output arrives in
    `reasoning_content` plus `content`, so a client that gets no reasoning section — or a truncated
    one — sees materially different output. The script runs `--reasoning on --reasoning-budget -1`
    (unrestricted). LocalHub emits neither, so it inherits the engine default rather than the proven
    setting.
  - [ ] **M2.6b `--metrics` and `--slots`.** Enable llama-server's Prometheus `/metrics` and `/slots`
    endpoints. Not VRAM-affecting. These are observability surfaces LocalHub would benefit from
    consuming directly (real per-slot state instead of inference from `/health`), so the right scope is
    probably "emit the flags AND read the endpoints", not just emit the flags.
  - [x] **M2.6c `-lv 3`, `--log-prefix`, `--log-timestamps`, `--log-file <path>` — NOT NEEDED.**
    Investigated 2026-07-27 and closed as a non-gap, correcting an earlier claim on this line that
    said these flags were "the likely reason the script observes an MTP acceptance rate and LocalHub
    did not". **That was false.** Evidence:
    - `-lv 3` is already b10106's DEFAULT — a LocalHub-spawned instance logged
      `verbosity = 3 (adjust with the -lv N CLI arg)` while LocalHub passes no `-lv`. The script's
      2.7 MB log contains only `I` and `W` lines (22,245 / 704) and zero `D` lines, so `-lv 3`
      unlocked nothing.
    - `--log-file` would CONFLICT, not help: `cmp` shows the script's `.engine.log` and `.err.log` are
      byte-identical (2,757,316 bytes each) — it merely duplicates stderr, which LocalHub already
      captures to `logs/llamacpp-<id>.log` (8 MiB cap).
    - `--log-prefix` / `--log-timestamps` are cosmetic formatting of a stream LocalHub already owns.
  - [ ] **M2.6d `-ngl 99` vs LocalHub's `-ngl all`.** Functionally equivalent for a 65-layer model
    (both mean "offload everything"), so this is cosmetic, NOT behavioural. Listed only so a
    flag-by-flag diff of the two command lines does not read as an unexplained mismatch.
  - **NOT a gap — deliberate:** `--host`. The script binds `192.168.0.73` (LAN) while LocalHub binds
    engine processes to `127.0.0.1` on purpose and fronts them with its own authenticated surface.
    Changing that would widen the attack surface of every spawned engine, so it should stay a LocalHub
    policy decision rather than a profile field.

- [x] M2.GATE Queue replay yields the identical normalized plan after daemon restart.
  **LIVE PASS 2026-07-25** — see M6.6 for the run (202 queued with a full non-default profile → daemon
  restart → `restored durable residency queue waiting=1` → row byte-identical → re-derived
  blocked_reason proves the profile feeds admission). Backed by **UNIT/TABLE PASS:** `TestQueueReplay_LegacyKVQuantSurvives` (the exact live defect —
  kv_quant=true must replay as q4_0/q4_0, never f16; an f16 replay prices the 27B ~1.5 GiB higher and
  can never be admitted), `TestQueueReplay_FullProfileRoundTrip`, and
  `TestQueueReplay_NoFieldSilentlyDropped` (per-field, so a future field added without threading
  fails the suite). Migrations 0009–0011 are now applied to the live `localhub.db` (schema_migrations
  1→11), and the 165 pre-existing rows written by the OLD binary read back cleanly under the new
  COALESCEd query — backward compatibility proven on production data.

### M3 — Capabilities, flags, and selection

**DO M3.7 AND M3.6 FIRST** — they block every other managed start for this model (see M1.3).

- [x] M3.7 **(root cause, verified 2026-07-25)** — DONE (A)+(B). **(A)** `HasCUDA` dropped as a
  selection criterion: `bestLlamaBinary` now takes `BinaryRequirements` (capability filter) and
  picks the first DETECTED binary in CONFIGURED order (`internal/compat/input.go`).
  **(B)** backend detection fixed: `llamacpp.detectOne` infers backends from the co-located
  `ggml-*.dll/.so/.dylib` when `--version` advertises none (`backendsFromDir`) — verified against
  the real b10106 (its `--version` prints no banner; `ggml-cuda.dll` sits beside the exe). Tests:
  `TestDetect_ModernBuildNoBackendBanner_InfersCUDA`, `TestBackendsFromDir_*`. `HasCUDA` stays as
  an accurate REPORTING field only.
- [x] M3.1 Probe `draft-mtp`, `--spec-draft-n-max`, and independent K/V support. **DONE 2026-07-25.**
  `llamacpp.Capabilities` gained `has_spec_mtp`, `has_spec_draft_max`, `has_kv_cache_types`,
  `has_parallel_slots`, all published on `/api/engines` + the engine reason, and carried into
  selection by `core.llamaBinaryFromView` (which also stops hardcoding `SupportsKVQuant: true` —
  it now reflects the probed `has_kv_cache_types`). **Re-verified against the real binaries' `--help`
  BEFORE relying on it:** b10106 prints `--spec-type none,draft-simple,draft-eagle3,draft-mtp,
  draft-dflash,ngram-simple,ngram-map-k,ngram-map-k4v,ngram-mod,ngram-cache`, `--spec-draft-n-max N`
  (default 3), `-ctk/--cache-type-k` + `-ctv/--cache-type-v`, `-np/--parallel N`; the TQ3 fork AND the
  winget build have `--spec-type` and `-ctk/-ctv` but **0 hits** for `draft-mtp` and no
  `--spec-draft-n-max`. Each bit is probed INDEPENDENTLY — nothing is inferred from `--spec-type`
  being present, because that would emit a value the fork rejects. Tests:
  `TestParseCapabilities_{MTPBuild,ForkHasNoMTP,IndependentBits,KVNeedsBothSides}`,
  `TestBuildReason_SurfacesMTPCaps`.
- [x] M3.2 Detect the model's embedded MTP tensors. **DONE:** `gguf.InspectMTP` reads the header +
  tensor-info only (no model load, no GPU) and matches the verified Qwen3 `.nextn.` convention
  (`blk.64.nextn.eh_proj.weight` ≈53 MiB + `nextn.enorm/hnorm/shared_head_norm`), accepting
  `mtp`/`draft` markers for forward compat. It also returns the head BYTES, which M4.2 adds to the
  footprint. `ModelDescriptor.HasMTP` short-circuits when a caller already knows.
- [x] M3.3 Gate MTP on both model and selected binary. **DONE:** `requirementsForProfile` adds a
  `SpecMTP` binary requirement, so selection can only return a `has_spec_mtp` binary; the MODEL side
  is checked separately in BOTH the verdict path (`evaluateGGUF`) and the launch path
  (`PlanLlamaLaunch`). Test: `TestPlanLlamaLaunch_MTPRefusals` (both directions),
  `TestPlanLlamaLaunch_MTPSelectsCapableBinary` (capability outranks configured order, the inverse of
  the non-MTP case).
- [x] M3.4 Emit q8_0/q4_0, draft max 2, and `-np 1` for the pinned profile. **DONE 2026-07-25.** The
  capability-bit-driven `-ctv tq3_0` is REPLACED (not added alongside): `-ctk`/`-ctv` now come from the
  profile's independent (K,V) pair. `TestPlanLlamaLaunch_MTPExactFlags` pins the whole surface as
  name/value PAIRS — `-ngl all -c 8192 --fit off -fa on -ctk q8_0 -ctv q4_0 -np 1 --spec-type
  draft-mtp --spec-draft-n-max 2` — and asserts `-ctv tq3_0` is gone. `--spec-draft-n-max` is emitted
  only when the SELECTED binary advertises it (`TestGGUFRunFlags_SpecDraftMaxGatedOnBinary`); a binary
  without `-ctk/-ctv` is forced to f16 and the plan REPORTS f16 rather than emitting flags it cannot
  honour (`TestPlanLlamaLaunch_KVForcedF16OnIncapableBinary`); draft-mtp forces `-np 1` even when more
  slots are requested (`TestPlanLlamaLaunch_MTPForcesSingleSlot`). q8_0/q4_0 is now expressible from
  the API and the UI (M2.1/M2.3), closing the second half of KNOWN-LIMITATIONS #21.
- [x] M3.5 Return human-readable reasons for unsupported combinations. **DONE:** each MTP refusal names
  WHICH side failed and what to do instead (binary: "needs a has_spec_mtp llama-server … none of the
  detected binaries (%s) support it"; model: "does not embed an MTP / next-n draft head (no `.nextn.`
  tensors) … pick a different speculative mode"). `draft-mtp` is now a VALID `speculative` value
  (it used to be a flat 400), while an unvalidated mode such as `draft-eagle3` is still refused even
  though b10106 advertises it — the value set stays pinned to what has been tested.
- [x] M3.6 Select a capable binary deterministically — capabilities first, configured preference
  second. **DONE 2026-07-25:** `requirementsForModel` + `bestLlamaBinary(BinaryRequirements)`
  (`internal/compat/input.go`), threaded through `evaluateGGUF` + `PlanLlamaLaunch`. Tests:
  `TestBestLlamaBinary_ConfigOrderBeatsHasCUDA`, `TestBestLlamaBinary_TQ3RequirementPicksFork`,
  `TestBestLlamaBinary_TQ3RequirementNoForkAvailable`, `TestBestLlamaBinary_SkipsNotDetected`,
  `TestRequirementsForModel`.
- [x] M3.GATE Table tests cover combinations, exact flags, and selection.
  **CLOSED 2026-07-25 at the unit/table level:** selection order, exact emitted flags, and the
  `has_spec_mtp`/`--spec-draft-n-max`/independent-K/V capability probe all have table tests, and the
  full `go test ./...` is green (19/19 packages). Evidence per sub-item above; gate detail in
  TESTING.md#m3. **Not claimed:** no MTP start has been executed live — that is M6.5.

### M4 — Admission and calibration

- [x] M4.1 Estimate K and V independently at the requested context. **DONE 2026-07-25 — and it
  surfaced a real defect.** The gguf-parser path already honoured `KVCacheTypeK/V`, but the canonical
  calibration key was being smuggled through `Options.KVCacheType`, which is ALSO consumed as a real
  dtype by the TQ3 file-size fallback (`kvBytesForType`) and the iSWA floor (`kvBytesPerElemX8`). For an
  asymmetric profile that field held `"q8_0/q4_0"` — not a dtype — so both fell through to their
  `default` branch, priced the cache as f16/f16, and printed a note DISAGREEING with the emitted
  `-ctk/-ctv`. Fixed by separating the concerns: new explicit `Options.CalibKey`,
  `Options.CalibrationKey()` as the single derivation point, and `kvCacheBytes`/`iswaKVFloorBytes`
  pricing K and V separately. A SYMMETRIC pair reproduces the previous numbers EXACTLY (asserted), so
  no stored estimate moved. The mis-pricing erred toward over-counting, so it could not have caused a
  false `runs` — but the stated reasoning was wrong, which the "every verdict carries a reason" bar
  does not tolerate.
- [x] M4.2 Include MTP tensors and runtime overhead. **DONE:** `InspectMTP` sums the `.nextn.*` tensor
  bytes (~53 MiB on the target) and the verdict adds them as an honest additive surcharge when
  `draft-mtp` is on — applied consistently at the requested ctx AND across every `ggufMaxContext`
  probe, so `max_context_that_fits` cannot disagree with the requested-ctx estimate.
- [x] M4.3 Preserve live NVML reads and all safety floors/margins. (Verified live: admission
  re-reads free VRAM; stop reclaim is an NVML delta, not an HTTP-200 claim.)
- [ ] M4.4 Calibrate conservatively before increasing context.
- [ ] M4.5 Record predicted and observed baseline/peak/recovered VRAM. (First data point
  2026-07-25: 8K q4_0 KV predicted **14107 MiB**, observed **13537 MiB** (+570 MiB pessimism),
  freed **13518 MiB** on stop.)
- [x] M4.6 Close the ~50 MiB admission knife-edge at 8K. **DONE 2026-07-25 as an HONESTY fix, not a
  policy loosening.** New `compat.AdmissionNoiseMiB = 256` (measured: est 14107 MiB vs usable
  14153–14168 MiB is a 46–61 MiB margin — smaller than ordinary desktop VRAM churn, which is exactly
  why the SAME request flipped 201↔409). A fit INSIDE that band now reports `runs_degraded` with a
  numeric reason naming the remaining headroom and what makes the decision deterministic: record a
  measured peak for this exact (K,V) pair (which on this profile moves the margin from ~50 MiB to
  ~600 MiB), free VRAM, or lower the context. Deliberate design points: `withinAdmissionNoise` is an
  ABSOLUTE-MiB check, distinct from the relative ±15% `nearBoundary`; it returns false for a
  non-fitting estimate so it can never soften a refusal; and it is ordered so a MEASURED (`exact`)
  verdict is never downgraded — a measurement IS the resolution of the knife-edge. Admission itself is
  unchanged: no new refusals, and no `runs` that would OOM.
- [ ] M4.GATE Zero false-positive `runs`; refusals carry measured actionable reasons. (No false
  `runs` observed; estimator errs conservative.)

### M5 — Truthful monitoring and stop

- [x] M5.1 Add/verify periodic process and health reconciliation after startup. **DONE 2026-07-25
  (unit-verified; live kill still owed).** New `instances.Monitor` + `Service.StartPeriodicMonitor`,
  wired in `main` alongside the drain loop and reusing the SAME health/identity checker as startup
  reconcile. **Documented interval: 15 s** (`core.DefaultMonitorInterval`), so a dead engine is FAILED
  within one tick and an unresponsive one is reaped after 3 × 15 s ≈ 45 s. Migration
  `0010_instance_health_reclaim` persists `last_health_at` + `health_failures` (persisted, not
  in-memory, so a restart cannot silently forgive a long-failing instance). Deliberately NO new
  instance state — these add observability to existing states rather than threading an `UNHEALTHY`
  state through every transport.
- [x] M5.2 Surface later exit/health failure as `FAILED` with reason. **DONE.** A post-HEALTHY death is
  `FAILED`, never `STOPPED`: a requested stop goes through `DRAINING`, so reporting a vanished engine
  as STOPPED would claim a graceful shutdown AND imply a verified reclaim that was never measured.
  Killing is deliberately conservative — PID-gone is unconditional (nothing to kill), while an
  alive-but-unresponsive instance is only reaped after N CONSECUTIVE failures and NEVER when pinned or
  when requests are in flight (never-kill-active). Tests:
  `TestMonitor_PostHealthyDeath_IsFailedNotStopped`, `TestMonitor_NeverReapsPinned` (the live 27B is
  pinned, so this is its direct guarantee), `TestMonitor_NeverReapsActiveStream` (which also proves the
  protection is scoped to ACTIVITY — once idle the same failing health does reap),
  `TestMonitor_UnresponsiveBelowThreshold_NotKilled`, `TestMonitor_HealthySuccessResetsCounter`,
  `TestMonitor_SkipsNonHealthyStates`, `TestMonitor_ReapDisabled`, `TestMonitor_WrongModelIdentity`.
- [~] M5.3 Expose MTP acceptance only if a stable tested signal exists. **REOPENED 2026-07-26 — my
  2026-07-25 "no such signal exists" conclusion was probably WRONG, for a fixable reason.**
  What I observed was real: a LocalHub-managed MTP run (M6.5) produced an engine log with **zero**
  lines matching `draft|spec|mtp|nextn|accept`. What I then concluded — that b10106 emits no
  acceptance signal — does not follow. The owner's `scripts/run-qwen-64k-q4q4.ps1` docstring records
  **MTP acceptance 89.8%** on the same binary and the same model, and that script passes
  `-lv 3 --metrics --slots --log-file <path>` while LocalHub passes none of them (M2.6b/M2.6c).
  **ROOT CAUSE FOUND 2026-07-27 — and it was neither of the two things I guessed.** The signal EXISTS
  and LocalHub can already produce it:
  - b10106 emits `I slot print_timing: id N | task N | draft acceptance = 0.87500 ( 14 accepted / 16
    generated), mean len = 2.75` whenever speculative decoding is ACTIVE. It is gated on
    `--spec-type draft-mtp` — which LocalHub already emits — NOT on `-lv`/`--metrics`/`--slots`
    (see M2.6c: `-lv 3` is b10106's default and unlocks no extra lines).
  - My original "zero matches" observation came from grepping
    `logs/llamacpp-22e2d303*.log`, a file that `wc -l` reported as **0 lines**. I grepped an EMPTY
    file and concluded the signal did not exist. The grep was sound; the inference was not.

  **What is actually missing is a parser and an API surface, not flags.** Remaining work: parse the
  `draft acceptance` line out of `logs/llamacpp-<id>.log`, expose it on the instance view, and state
  its WINDOW honestly — the line is emitted per finished request and the instance log is capped at
  8 MiB, so a long-running instance loses early lines. An exposed figure must say what span it covers
  rather than implying a lifetime average.
- [x] M5.4 Preserve `DRAINING` while requests are active. (DELETE → 202 `DRAINING`, async and
  detached from the HTTP caller, then `STOPPED`.)
- [x] M5.5 Distinguish reclaim-unverified/timeouts from verified `STOPPED`. **DONE 2026-07-25.**
  `Runner.Stop` now persists `reclaim_verified` (migration 0010: 1 verified / 0 unproven / NULL never
  attempted — three states on purpose, so "not attempted" cannot read as "failed"). The reason string
  was already truthful, but `state=STOPPED` + `vram_freed_mib` alone let a reclaim TIMEOUT or a failed
  baseline probe look like a clean stop to any programmatic consumer. `false` now covers BOTH failure
  modes (timeout, and a failed baseline where a delta is meaningless), and it is surfaced on the API
  view and in the UI as "reclaim UNVERIFIED". The monitor's own terminal transitions also set it false,
  since a monitor-detected death measures no NVML delta at all.
- [x] M5.6 Verify exit, port closure, NVML recovery, and no orphans. (VRAM 14596 → 1105 MiB used;
  no `llama-server` process; port 40100 `TIME_WAIT` only. Also clean after the crash path.)
- [x] M5.GATE Stop, drain, crash, and reclaim-timeout tests yield truthful API/UI states.
  **LIVE PASS 2026-07-25** (owner brought the 27B down): a clean API stop reported
  `reclaim_verified: true` + 13562 MiB freed by NVML delta; a disposable 1B was then killed EXTERNALLY
  and the API flipped HEALTHY → **FAILED in 4 s** (documented interval 15 s, no LocalHub restart) with
  `reclaim_verified: false` and a reason stating it was not a requested stop. No orphans. Remaining
  unit-only: the reap-after-N branch, pinned/active-stream protections, and the reclaim-TIMEOUT branch
  (a timeout needs a GPU that refuses to release VRAM, which cannot be induced on demand). Detail:
  TESTING.md#m5. Superseded note — the earlier blocker read:
  **(was) STILL OPEN — the LIVE kill.** It cannot be run right now: the pinned 27B holds ~14.6 GiB of 16 GiB,
  so there is no room for a second disposable model, and the owner's constraint is to keep it up.
  Needs either a window with the 27B down, or a tiny model plus freed VRAM. Do NOT close this gate on
  unit evidence alone — the criterion is explicitly "kill a disposable HEALTHY process and see the
  API/UI reflect it within the documented interval without a LocalHub restart".

### M6 — Defaults, docs, and live acceptance

- [ ] M6.1 Save the default only after lower-context live gates pass.
- [ ] M6.2 Tie it visibly to the pinned artifact and engine build.
- [ ] M6.3 Update engine, compatibility, API, data-model, migration, and version docs.
- [x] M6.4 Run LocalHub-managed non-streaming and streaming inference. (Verified 2026-07-25 at 8K
  q4_0 KV, no MTP: non-stream returned `content: "INTEGRATION OK"`, `finish_reason: stop`;
  streaming relayed SSE intact with `system_fingerprint: "b10106-1425386fd"`. Note Qwen3.6 is a
  reasoning model — output arrives in `reasoning_content` plus `content`, so a small `max_tokens`
  yields an empty `content` and that is not a relay defect. Re-run per profile change.)
- [~] M6.5 Run MTP-on/off sanity/determinism and bounded long-session checks. **MTP-ON PROVEN LIVE
  2026-07-25 — the first managed MTP start on this box.** `ctx 2048, -ctk q8_0 -ctv q4_0, -np 1,
  --spec-type draft-mtp --spec-draft-n-max 2` reached HEALTHY (pid 2248) and served inference
  (`"MTP OK"`, finish stop, 150 tokens, fingerprint `b10106-1425386fd`). Estimate 13775 MiB, measured
  peak 13370 MiB (+405 conservative).
  **HARD FINDING — q8_0/q4_0 does NOT fit at 8K on this card**, refused with numbers rather than an
  OOM: 8K needs 14420 MiB vs 13951 usable (469 short); 4K needs 13990 vs 13951 (**39 MiB** short — the
  knife-edge itself); 2K fits. So MTP is real but buys only 2K context here unless the safety factor is
  revisited or the ~1.2 GiB of desktop VRAM is freed.
  **Still open:** the MTP-on vs MTP-off A/B and the bounded long-session check — and note the two
  configurations are not comparable at equal context, since MTP only fits at 2K.
- [x] M6.6 Prove exact queue profile replay across daemon restart. **LIVE PASS 2026-07-25:** a start
  carrying `ctx 4096 / kv_type_k q8_0 / kv_type_v q4_0 / slots 2 / ngram-cache / allow_cpu_offload /
  priority 1` returned 202 with a numeric `ram_blocked` reason; the durable row held every field; the
  daemon was restarted (`restored durable residency queue waiting=1`) and the row read back
  BYTE-IDENTICAL. The re-derived blocked_reason (`footprint 1335 + 1024 margin`) proves the restored
  profile actually feeds admission, not just a column. The 27B reattached across the same restart with
  its MTP flags intact. Entry cancelled; no stray rows. Detail: TESTING.md#m2.
  **(historical note from the earlier partial run:)** The daemon
  was rebuilt and restarted against the live DB: migrations 9/10/11 applied cleanly, the pinned 27B was
  re-attached at the same pid, and the 165 pre-existing queue rows written by the OLD binary read back
  correctly under the new schema (backward compat proven on production data). The unit round-trip is
  proven three ways (M2.GATE). **What is still missing:** a genuinely QUEUED start to replay. It needs
  a request that PASSES the verdict but does not fit — impossible in the current VRAM state, where
  everything that passes fits in the free ~1.6 GiB (and would start a second model) and everything
  larger is refused `wont_run` before a queue row exists. Needs a window with the 27B down, or a
  temporary `residency.min_free_ram_mib` raise so a start queues on the RAM floor. See TESTING.md#m2.
- [ ] M6.7 Prove drain/stop, NVML reclaim, crash detection, and negative paths.
- [ ] M6.8 Increase context progressively; attempt 64K only after prior tiers pass.
- [ ] M6.9 Record requests, flags, timings, transitions, deltas, evidence, and cleanup.
- [ ] M6.GATE Classify as `LIVE PASS`, `VERIFIED REFUSAL`, or `BLOCKED` with reason.
  `LIVE PASS` requires managed start/infer/monitor/stop/restart/queue evidence.
  **Standing as of 2026-07-25:** `LIVE PASS` for managed start → HEALTHY → infer (stream +
  non-stream) → drain → stop → NVML-verified reclaim **at 8K with q4_0 KV and no MTP**;
  `BLOCKED` for the MTP + 64K q8/q4 profile pending M3.7/M3.6/M3.x and M4. Queue-replay evidence
  (M6.6) still missing, so the gate itself stays open.

## Phase 0 — Field validation (no product code)

- [x] P0.1 Environment audit: record machine profile (OS/CPU/RAM/GPU/VRAM/driver/CUDA, Tailscale present?), engine versions (ollama, llama-server, docker), model storage baseline; fill "Machine" + "Tools tested" in docs/23-phase-0-field-validation.md. Reference: PLAN.md#target-machine-the-reference-box--all-gates-close-on-this-hardware. Gate: TESTING.md#p01.
- [x] P0.2 Existing-tool trial: for 10 real candidate models (prefer the GGUFs already in K:\projects\llm_models\models) answer "fits? on what engine? at what ctx?" by hand using gguf-parser CLI (remote metadata where possible), Ollama, llama-server; trial llama-swap (Windows binary) for swap mechanics; note vLLM as deferred-to-Docker; write research/phase-0-friction-log.md with per-model rows + friction list; mine lmwrapper_project (queue/allocator semantics, documented failures, Windows lessons) into research/lmwrapper-reuse-notes.md; determine which llama.cpp-tq3 flags are fork-only vs upstream. Reference: PLAN.md#existing-assets-to-reuse-discovered-2026-07-06-per-user. Gate: TESTING.md#p02.
- [x] P0.3 Decisions: fill docs/23 Decision = GO_STANDALONE (user directive; rationale from friction log); resolve ADR-003 (llama-swap drive vs custom residency) from the trial and update docs/decisions.md; record ADR-006 = Go (Accepted). Reference: PLAN.md#decisions. Gate: TESTING.md#p03.

## Phase 1 — Read-only observability

- [x] P1.1 Go scaffold: module, cmd/localhub, config loading (configs/), SQLite WAL bootstrap + migrations per docs/data-model.md, localhost-only HTTP server skeleton, logging; layered architecture: one internal service core, transports (REST/SSE/WS/JSON-RPC) as thin layers per PLAN.md decision 7; scripts/dev.ps1. Reference: PLAN.md#approach. Gate: TESTING.md#p11.
- [x] P1.2 Hardware probe: internal/hardware interface + nvidia-smi implementation (GPU name, VRAM total/free, driver, CC, util, power, temp) + CPU/RAM/disk; GET /api/hardware + SSE /api/hardware/live. Reference: PLAN.md#decisions item 5. Gate: TESTING.md#p12.
- [x] P1.3 Engine detection (read-only): Ollama /api/version /api/tags /api/ps; configured llama.cpp + vLLM entries from config; GET /api/engines. Zero mutation. Reference: PLAN.md#approach. Gate: TESTING.md#p13.
- [x] P1.4 Inventory scanner: Ollama models + GGUF folder scan + HF/safetensors dirs → normalized table per docs/data-model.md; GET /api/models. Reference: PLAN.md#approach. Gate: TESTING.md#p14.
- [x] P1.5 React dashboard (web/): live GPU panel, engines, inventory; built + embedded/served by daemon; WebSocket live-state channel implemented alongside SSE (same event source). Reference: PLAN.md#approach, docs/20-ui-ux-spec.md. Gate: TESTING.md#p15.
- [ ] P1.6 (BLOCKED on user — see TESTING.md#p16: elevated Tailscale service restart + phone check; all agent-side prep done) Phone access: Tailscale Serve config + docs; verify serve endpoint programmatically; MANUAL user check from phone. Phase 1 close gate per docs/14. Reference: PLAN.md#approach. Gate: TESTING.md#p16.

## Phase 2 — Compatibility engine

- [x] P2.1 gguf-parser-go integration (library): estimates for local GGUF + remote HF GGUF without full download. Reference: PLAN.md#approach. Gate: TESTING.md#p21.
- [x] P2.2 Safetensors/vLLM estimator: weights + GQA-aware KV + overhead from config.json + safetensors index, per docs/compatibility-engine.md. Reference: PLAN.md#approach. Gate: TESTING.md#p22.
- [x] P2.3 Rule engine: hardware gates (CC, VRAM margin), engine recommendation by format (GGUF→llama.cpp/Ollama, safetensors→vLLM), verdict {runs|runs_degraded|wont_run} with mandatory reason, estimated_vram, max_context_that_fits, run_flags. Reference: PLAN.md#approach. Gate: TESTING.md#p23.
- [x] P2.4 POST /api/compat/preview (remote, no download) + verdict UI. Reference: PLAN.md#approach. Gate: TESTING.md#p24.
- [x] P2.5 Validation matrix: 20 diverse models (GGUF+safetensors, GQA+MHA, fits+doesn't) — real loads for a hardware-safe subset per PLAN.md safeguards; record estimator error; zero false-positive runs. Phase 2 close gate. Reference: PLAN.md#approach. Gate: TESTING.md#p25.

## Phase 3 — Supply chain

- [x] P3.1 HF download: revision→commit pinning shown before download, resumable, per-file sha256; single-GGUF URL path. Reference: PLAN.md#approach. Gate: TESTING.md#p31.
- [x] P3.2 Ollama pull passthrough + local import path (with explicit local-import warning). Reference: PLAN.md#approach. Gate: TESTING.md#p32.
- [x] P3.3 Immutable manifest + license/security scan (pickle/trust_remote_code/gated/no-license warnings, warn-not-block per ADR-004) + reproduce export. Phase 3 close gate. Reference: PLAN.md#approach. Gate: TESTING.md#p33.

## Phase 4 — Controlled run/stop

- [x] P4.1 Instance state machine + DB (REQUESTED→STARTING→HEALTHY→DRAINING→STOPPED/FAILED) + startup reconciliation (reattach/mark-stopped/reap orphans). Reference: PLAN.md#hardware-safeguards-binding-on-every-task-user-constraint-do-not-over-use-my-hardware-use-cpu-as-little-as-possible. Gate: TESTING.md#p41.
- [x] P4.2 llama.cpp adapter: spawn llama-server (port pool, flags recorded, localhost), health, stop, VRAM-delta verify. Reference: PLAN.md#hardware-safeguards-binding-on-every-task-user-constraint-do-not-over-use-my-hardware-use-cpu-as-little-as-possible. Gate: TESTING.md#p42.
- [x] P4.3 Ollama adapter: warm via API, unload via keep_alive:0, mirror /api/ps, poll-verified state. Reference: PLAN.md#hardware-safeguards-binding-on-every-task-user-constraint-do-not-over-use-my-hardware-use-cpu-as-little-as-possible. Gate: TESTING.md#p43.
- [x] P4.4 vLLM adapter: spawn/health/stop against documented contract + STARTING UX; contract tests vs stub on Windows. Reference: PLAN.md#hardware-safeguards-binding-on-every-task-user-constraint-do-not-over-use-my-hardware-use-cpu-as-little-as-possible. Gate: TESTING.md#p44.
- [x] P4.5 vLLM real E2E (one session): Docker/WSL2, tiny model, start/health/infer-one-token/stop, VRAM + teardown verified. Phase 4 close gate (all three engines). Reference: PLAN.md#hardware-safeguards-binding-on-every-task-user-constraint-do-not-over-use-my-hardware-use-cpu-as-little-as-possible. Gate: TESTING.md#p45.

## Phase 5 — Coarse residency

- [x] P5.0 Priority-queue design + implementation (user requirement): custom documented ordering algorithm for queued admission requests — priority classes, aging (anti-starvation), size/cost-aware scoring; design note in docs/ + property tests. Whole-model residency only (ADR-001). Reference: PLAN.md#decisions item 7. Gate: TESTING.md#p50.
- [x] P5.1 Admission control: warm? → estimate → read free VRAM (probe, re-read after every evict) → evict-if-safe → start | queue (ordered by P5.0) / reject with blocked_reason. Per ADR-001: whole-model load/unload only. Reference: PLAN.md#hardware-safeguards-binding-on-every-task-user-constraint-do-not-over-use-my-hardware-use-cpu-as-little-as-possible. Gate: TESTING.md#p51.
- [x] P5.2 Eviction policy: expired TTL → idle+lowest-priority+LRU → largest reclaimable; never pinned, never active/DRAINING-with-streams by default. Reference: PLAN.md#hardware-safeguards-binding-on-every-task-user-constraint-do-not-over-use-my-hardware-use-cpu-as-little-as-possible. Gate: TESTING.md#p52.
- [x] P5.3 Load-storm test on the reference GPU with small models: more requests than fit; no OOM, no killed active stream, no crash, sensible blocked_reasons. Phase 5 close gate. Reference: PLAN.md#hardware-safeguards-binding-on-every-task-user-constraint-do-not-over-use-my-hardware-use-cpu-as-little-as-possible. Gate: TESTING.md#p53.

## Phase 6 — Thin OpenAI proxy

- [x] P6.1 /v1/models + /v1/chat/completions + /v1/completions routed by model→engine through admission; streaming relayed untouched; fast 503+blocked_reason; explicitly no LiteLLM features. Phase 6 close gate. Reference: PLAN.md#approach. Gate: TESTING.md#p61.
- [x] P6.2 Async inference jobs (user requirement): POST /api/jobs (inference request → job id, queued through admission + P5.0 ordering), GET /api/jobs/{id}, results pushed over WebSocket job-events channel; cancellation; jobs survive daemon restart (persisted queue). Reference: PLAN.md#decisions item 7. Gate: TESTING.md#p62.
- [x] P6.3 JSON-RPC 2.0 surface (user requirement): thin layer over the same service core (hardware, engines, models, compat, instances, residency, jobs); over HTTP POST + WebSocket; spec-compliant error objects with human-readable reasons. Reference: PLAN.md#decisions item 7. Gate: TESTING.md#p63.

## Close-out

- [x] C1 Master close gate: verify all five product success criteria (docs/14 master gate) on this box; update docs/18-source-validation-matrix.md; write changelog + known limitations; final code review sweep. (Closed agent-side 2026-07-07; master #4 phone check rides on C2.) Reference: PLAN.md#risks-and-decisions-hazards. Gate: TESTING.md#c1.
- [ ] C2 USER ACTIONS (blocked on user): initial commit (repo already git-initialized; `git add -A && git commit -F COMMIT_MSG.txt`); phone-side Tailscale check confirmation; review agent-made decisions in PLAN.md#decisions. Reference: PLAN.md#risks-and-decisions-hazards. Gate: TESTING.md#c2.

## Post-1.0 user-directed extensions (2026-07-07/08 directives)

- [x] X1 UI overhaul: minimal/modern redesign; Run-per-row + fit-annotated Start picker; homepage recents collapsed behind History toggles; mobile 390px overflow fixed. Evidence: web suite (79 tests), live click-throughs, screenshots in session scratchpad.
- [x] X2 Full-GPU default + allow_cpu_offload opt-in (llama.cpp -ngl all/auto; Ollama num_gpu forced-full default — BEHAVIOR CHANGE from silent auto-split) end-to-end incl. UI toggle. Gate: unit tests green (flags/warm/api decode); docs/api-spec.md §Instances.
- [x] X3 Admission thresholds: residency.min_free_ram_mib (2048), max_gpu_temp_c (87), max_gpu_util_pct (0=off) — fresh-probe admission guards w/ numeric reasons, warm-route exempt, never kills running models. Gate: admission_ram_test.go + admission_gpu_test.go.
- [ ] X4 Model campaign — five of six resolved 2026-07-07 (evidence research/model-campaign-2026-07-07.md): ASR/OCR/30B-BF16/ornith/vLLM-GGUF all honest verdicts or refusals; residency drill (unload/queue/auto-drain/evict/never-kill-active, 17/17 layers full-GPU) ALL PASS. REMAINING: gemma-4-12B live run + full-GPU proof once its throttled HF download completes (campaign agent auto-resumes; verify teardown + append evidence).
- [ ] X4.1 Comprehensive local-model acceptance campaign — close the gap from the 2026-07-10 smoke pass: exercise every currently local large, TQ3, vision, multimodal, embedding, and Ollama model through **LocalHub's** start/proxy path (not a direct-engine-only call). Run one model at a time only after RAM/VRAM/temperature safeguards pass; preserve the configured WSL/Docker headroom. Cover at minimum: the four TQ3 GGUFs (Qwen3.5 27B, Qwen3.6 27B, Qwen3.6 35B-A3B MoE, Qwopus vision), Qwen3-VL-32B plus projector, Qwen3-VL-Embedding-8B, GLM-OCR plus projector, PaddleOCR-VL plus projector, Gemma-4-12B, the remaining local Ollama models (GLM-OCR, Qianfan-OCR, MiniCPM-O4.5, DeepSeek-R1/Qwen3 8B), and a real Safetensors/vLLM route. For every row, record the LocalHub compatibility verdict/reason, exact engine and flags, health, one suitable inference call (chat, embedding, or multimodal), NVML baseline/peak/recovery, and cleanup. An unsafe or unsupported model is complete only with a reproducible LocalHub refusal carrying its reason; never bypass admission to force a load. Append results and estimator deltas to research/model-campaign-2026-07-07.md and research/phase-2-validation-matrix.md. Done when every locally present model is classified as current LIVE PASS, VERIFIED REFUSAL, or BLOCKED with an actionable reason; zero false-positive `runs`; no orphan process/container; and all three LocalHub-routed engines have a current E2E result. Reference: TESTING.md#p25,p42-p45,p61; docs/compatibility-engine.md; docs/engine-adapters.md.
  - 2026-07-11 partial live evidence: `research/x41-live-campaign-2026-07-11.md`. This stays open: embedding and multimodal relay coverage, correct projector association, a safe 32B VL verdict, and a current LocalHub-routed Safetensors/vLLM E2E are still missing.
  - Full-VRAM calibration, 2026-07-11: Qwen3.6-27B TQ3 and Qwopus reached health at ctx 512 with their correct projectors and ~15.96 GiB VRAM, but both stalled on a 4-token completion; Qwen3.5-27B did not reach health before host-RAM exhaustion. Add a low-context degraded profile only after the completion stall has a reproducible cause and usable latency proof.
  - **Owner constraint, 2026-07-11:** do not attempt to load or benchmark any model of 30B parameters or larger on this box. Keep 32B VL and 35B-A3B as estimate/refusal-only rows; no forced load, no full-VRAM calibration, and no retry unless the owner explicitly reverses this constraint.
- [ ] X5 Optimization support — IMPLEMENTATION DONE 2026-07-08 (suite green, orchestrator-verified): (a) `speculative:"ngram-cache"` per-start opt-in shipped; (b) iSWA audit found gguf-parser UNDERPRICING gemma-4 KV to 0 MiB (no gemma4 arch + SkipLargeMetadata dropping per-layer kv heads) → conservative iSWA floor added in internal/compat/gguf/iswa.go (+1024@4K/+1088@8K/+1472@32K MiB, numbers in research doc §7, KNOWN-LIMITATIONS #16); (c) MoE -ncmoe under allow_cpu_offload with metadata-derived honest sizing (TQ3 MoEs not derivable → safe fallback, KNOWN-LIMITATIONS #19). REMAINING (GPU-gated): ngram t/s A/B demo; iSWA floor calibration vs real NVML load; derivable-MoE live unlock (needs a non-TQ3 MoE download); draft-model spec-dec feature (2-model VRAM admission, research §5).
- [x] X6a Cluster PLAN: docs/cluster-plan.md written 2026-07-08 (architecture, security, data model, failure modes, P7.1–P7.5 with two-daemon-rig gates, §0 review checklist); ADR-008 appended to docs/decisions.md (Proposed — owner review of §0 upgrades to Accepted).
- [x] X7 Proxy warm-route self-shadowing BUG — FIXED 2026-07-08: root cause was the compat verdict-status gate running BEFORE residency's warm-route check; StartInstance now warm-short-circuits (HEALTHY instance for the model → route, zero fit math) with cold path unchanged. Tests: TestResolveForProxy_WarmRoutesDespiteSelfShadowVerdict + ColdSelfShadowStillBlocks. Ctx-mismatch v1 semantics documented (KNOWN-LIMITATIONS #18). Suite green (orchestrator-verified).

- [ ] X8 **Origin-aware auth — two operating modes (owner directive 2026-07-25).** Desired end
  state: LAN reachable **without** a password, Tailscale access **requiring** a token/password.
  **Blocker (verified):** `server.admin_token` is a single GLOBAL switch — `authMiddleware`
  (`internal/api/middleware.go`) applies it to every `/api/*` and `/v1/*` request regardless of
  origin, so the two modes cannot coexist by config; enabling the token for Tailscale also locks
  out passwordless LAN clients. Needs request-origin classification (loopback / LAN / tailnet —
  the last via the Tailscale identity headers `trust_tailscale_headers` already governs) plus a
  per-origin policy, with negative tests per origin class. Documented in docs/security.md
  "Two operating modes" + configs/config.example.yaml. **Current live state: Mode A** (bind
  0.0.0.0, allow_nonlocal true, admin_token null) — full admin to anyone who can reach
  port 39876, on every interface including the two ZeroTier overlays.

## Phase 7 — Cluster (LAN federation, ADR-008; plan: docs/cluster-plan.md)

- [x] P7.1 Node identity + cluster config (cluster.role coordinator|node|off, cluster_token) + push join/heartbeat + nodes registry (additive table, soft state) + GET/POST /api/cluster/nodes + role:off byte-identical regression. (PASS 2026-07-08, live two-daemon rig — research/p71-gate-evidence.md; 0008_nodes.sql; deviations logged in the report: join subpath /join, heartbeat_interval_seconds=10, ONLINE/STALE/UNREACHABLE, node-token required unconditionally, JSON-RPC cluster mirrors deferred to P7.2.) Reference: docs/cluster-plan.md §P7.1. Gate: TESTING.md#p71.
- [x] P7.2 Aggregated views: fan-out /api/cluster/models + /api/cluster/hardware + union-deduped /api/cluster/v1/models; partial-result semantics on dead nodes. (PASS 2026-07-08 live rig — research/p72-gate-evidence.md; recorded deviation: union NOT promoted onto the coordinator's own /v1/models until P7.3 routing exists; JSON-RPC cluster.* mirrors shipped coordinator-gated; suite green orchestrator-verified.) Reference: docs/cluster-plan.md §P7.2. Gate: TESTING.md#p72.
- [ ] P7.3 Placement + routed completions (money milestone): warm > has-model > runs>degraded > free-VRAM > round-robin; routed /v1/chat|completions via internal/proxy.Relay; node-prefixed block reasons. STOPPED-BY-OWNER 2026-07-08 before any code (clean tree); full pickup brief incl. P7.2 handoff notes: docs/future-work.md §1. Reference: docs/cluster-plan.md §P7.3. Gate: TESTING.md#p73.
- [ ] P7.4 WS fan-in ({node_id, event} republish) + jobs place-and-forward (node-durable job records). Pickup: docs/future-work.md §2. Reference: docs/cluster-plan.md §P7.4. Gate: TESTING.md#p74.
- [ ] P7.5 Security hardening: cluster-token validation trio mandatory when clustered non-loopback, rotation runbook, Tailscale cluster guide, negative-auth tests, dev-endpoint allow-list audit on coordinator relay. Pickup: docs/future-work.md §3. Reference: docs/cluster-plan.md §P7.5. Gate: TESTING.md#p75.

> **2026-07-08 owner stop-point:** "alright stop all work and only write docs for all
> future work." All remaining open items above and every deferred/live-demo/polish item
> are consolidated with pickup briefs in **docs/future-work.md** (the single resume
> entry point). Tree left clean: go vet/build/test ./... green, web 79/79, no partial
> edits, no orphan processes.
