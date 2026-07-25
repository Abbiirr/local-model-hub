# Todo

Work top-to-bottom. One task = one delegated unit of work. Do not start a phase before
the prior phase's gate task is closed. Non-waivable gates in PLAN.md apply to every task.

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

- [ ] P6.1 /v1/models + /v1/chat/completions + /v1/completions routed by model→engine through admission; streaming relayed untouched; fast 503+blocked_reason; explicitly no LiteLLM features. Phase 6 close gate. Reference: PLAN.md#approach. Gate: TESTING.md#p61.
- [ ] P6.2 Async inference jobs (user requirement): POST /api/jobs (inference request → job id, queued through admission + P5.0 ordering), GET /api/jobs/{id}, results pushed over WebSocket job-events channel; cancellation; jobs survive daemon restart (persisted queue). Reference: PLAN.md#decisions item 7. Gate: TESTING.md#p62.
- [ ] P6.3 JSON-RPC 2.0 surface (user requirement): thin layer over the same service core (hardware, engines, models, compat, instances, residency, jobs); over HTTP POST + WebSocket; spec-compliant error objects with human-readable reasons. Reference: PLAN.md#decisions item 7. Gate: TESTING.md#p63.

## Close-out

- [ ] C1 Master close gate: verify all five product success criteria (docs/14 master gate) on this box; update docs/18-source-validation-matrix.md; write changelog + known limitations; final code review sweep. Reference: PLAN.md#risks-and-decisions-hazards. Gate: TESTING.md#c1.
- [ ] C2 USER ACTIONS (blocked on user): git init + first commit; phone-side Tailscale check confirmation; review agent-made decisions in PLAN.md#decisions. Reference: PLAN.md#risks-and-decisions-hazards. Gate: TESTING.md#c2.
