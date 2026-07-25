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
- [ ] M1.3 **REWRITTEN — setting `binary_path` is NOT sufficient and was the wrong diagnosis.**
  `compat.bestLlamaBinary` (`internal/compat/input.go:137-150`) picks the first binary with
  `HasCUDA` and **never consults config order**. b10106 reports `has_cuda=false` (its `--version`
  prints no backend line; modern llama.cpp loads `ggml-cuda.dll` dynamically), so the old TQ3
  fork wins and then dies on this model with
  `missing tensor 'blk.64.ssm_conv1d.weight'` (fork predates Qwen3.6's hybrid SSM arch).
  Real fix = M3.7 + M3.6. Config-only workaround (test rig only): b10106 as the sole candidate,
  which ⚠ globally disables TQ3 support.
- [ ] M1.4 Record b10106 version/commit (`10106` / `1425386fd`), SHA-256, re-verification cadence.
- [ ] M1.5 Keep older binaries only as capability-gated fallbacks.
- [ ] M1.GATE Clean restart selects b10106 for this model **without deleting the other
  binaries**, and TQ3 models still resolve to the fork.

### M2 — Durable launch profile

- [ ] M2.1 Define/version a profile with context, independent K/V types, speculative
  mode, MTP draft maximum, slots, flash attention, CPU offload, projector, pin, priority.
- [ ] M2.2 Thread it through REST, core, `LaunchPlan`, instances, and reproducibility.
- [ ] M2.3 Add model-default persistence and a UI advanced start form.
- [ ] M2.4 Persist every behavior-affecting field in the queue schema.
- [ ] M2.5 Preserve the profile through jobs and JSON-RPC admission.
- [ ] M2.GATE Queue replay yields the identical normalized plan after daemon restart.

### M3 — Capabilities, flags, and selection

**DO M3.7 AND M3.6 FIRST** — they block every other managed start for this model (see M1.3).

- [ ] M3.7 **NEW (root cause, verified 2026-07-25):** fix llama.cpp backend detection for builds
  whose `--version` advertises no backends, so a modern CUDA build is not misread as
  `has_cuda=false`; or drop `HasCUDA` as a selection criterion in favour of explicit capability
  requirements. **Owner decision needed** (PLAN-run-stop-monitor.md §7).
- [ ] M3.1 Probe `draft-mtp`, `--spec-draft-n-max`, and independent K/V support. (Confirmed the
  capability object is exactly `{has_fit, has_flash_attn, has_sleep_idle, has_tq3}` — no
  spec/MTP/KV field exists yet.)
- [ ] M3.2 Detect the model's embedded MTP tensors.
- [ ] M3.3 Gate MTP on both model and selected binary.
- [ ] M3.4 Emit q8_0/q4_0, draft max 2, and `-np 1` for the pinned profile. **Note:** asymmetric
  KV is not greenfield — `internal/compat/flags.go:80-89` already emits `-ctk <type> -ctv tq3_0`
  when `bin.HasTQ3`, driven by a capability bit rather than the request. Replace that, don't
  add alongside it. `kv_quant` is a bare `bool` in the API today
  (`internal/core/instances.go:48-69`), so q8_0/q4_0 is currently inexpressible.
- [ ] M3.5 Return human-readable reasons for unsupported combinations. (Already the norm: every
  refusal triggered in testing carried one, incl. the `draft-mtp` 400.)
- [ ] M3.6 Select a capable binary deterministically — capabilities first, configured preference
  second.
- [ ] M3.GATE Table tests cover combinations, exact flags, and selection.

### M4 — Admission and calibration

- [ ] M4.1 Estimate K and V independently at the requested context.
- [ ] M4.2 Include MTP tensors and runtime overhead.
- [x] M4.3 Preserve live NVML reads and all safety floors/margins. (Verified live: admission
  re-reads free VRAM; stop reclaim is an NVML delta, not an HTTP-200 claim.)
- [ ] M4.4 Calibrate conservatively before increasing context.
- [ ] M4.5 Record predicted and observed baseline/peak/recovered VRAM. (First data point
  2026-07-25: 8K q4_0 KV predicted **14107 MiB**, observed **13537 MiB** (+570 MiB pessimism),
  freed **13518 MiB** on stop.)
- [ ] M4.6 **NEW:** close the ~50 MiB admission knife-edge at 8K — est 14107 MiB vs 14153–14168
  MiB usable means the same request flips between HTTP 201 and 409 with ordinary desktop VRAM
  noise. (This, not a hard refusal, explains the prior revision's "rejected with 409" claim.)
- [ ] M4.GATE Zero false-positive `runs`; refusals carry measured actionable reasons. (No false
  `runs` observed; estimator errs conservative.)

### M5 — Truthful monitoring and stop

- [ ] M5.1 Add/verify periodic process and health reconciliation after startup. (Still open: only
  a **start-time** crash was observed, never a post-HEALTHY death.)
- [~] M5.2 Surface later exit/health failure as `FAILED` with reason. (Start-time crash → `FAILED`
  with the engine log tail in `reason`, verified. Post-HEALTHY exit untested.)
- [ ] M5.3 Expose MTP acceptance only if a stable tested signal exists. (Per-instance logs are
  captured to `logs/llamacpp-<id>.log`, so scraping is plausible — but nothing emits a draft
  line without MTP flags. Do not promise the metric.)
- [x] M5.4 Preserve `DRAINING` while requests are active. (DELETE → 202 `DRAINING`, async and
  detached from the HTTP caller, then `STOPPED`.)
- [ ] M5.5 Distinguish reclaim-unverified/timeouts from verified `STOPPED`. (Happy path proven —
  reason reads "stopped and VERIFIED: free VRAM rose by 13518 MiB … NVML delta, not an HTTP-200
  claim". The **timeout/inconclusive** branches remain untested.)
- [x] M5.6 Verify exit, port closure, NVML recovery, and no orphans. (VRAM 14596 → 1105 MiB used;
  no `llama-server` process; port 40100 `TIME_WAIT` only. Also clean after the crash path.)
- [ ] M5.GATE Stop, drain, crash, and reclaim-timeout tests yield truthful API/UI states.
  (Remaining: active-stream drain, post-HEALTHY crash, reclaim timeout.)

### M6 — Defaults, docs, and live acceptance

- [ ] M6.1 Save the default only after lower-context live gates pass.
- [ ] M6.2 Tie it visibly to the pinned artifact and engine build.
- [ ] M6.3 Update engine, compatibility, API, data-model, migration, and version docs.
- [x] M6.4 Run LocalHub-managed non-streaming and streaming inference. (Verified 2026-07-25 at 8K
  q4_0 KV, no MTP: non-stream returned `content: "INTEGRATION OK"`, `finish_reason: stop`;
  streaming relayed SSE intact with `system_fingerprint: "b10106-1425386fd"`. Note Qwen3.6 is a
  reasoning model — output arrives in `reasoning_content` plus `content`, so a small `max_tokens`
  yields an empty `content` and that is not a relay defect. Re-run per profile change.)
- [ ] M6.5 Run MTP-on/off sanity/determinism and bounded long-session checks.
- [ ] M6.6 Prove exact queue profile replay across daemon restart.
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
