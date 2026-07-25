# Plan — LocalHub Compass, Phases 0–6 end-to-end

## Goal

Build LocalHub Compass to the master project close gate in `docs/14-closing-gates.md`:
a single-box control plane (Go daemon + React dashboard + SQLite) that inventories local
models across Ollama / llama.cpp / vLLM, gives pre-flight *runs / runs-degraded /
won't-run* verdicts with reasons and flags, downloads with immutable manifests, runs and
stops models conservatively, manages coarse residency with no OOM and no killed streams,
and serves a phone-accessible dashboard over Tailscale. All six roadmap phases
(`docs/roadmap.md`), each closed against its gate in `docs/14-closing-gates.md`.

## Context And References

Source of truth for intent (read before implementing any area):

- `CLAUDE.md` — agent rules; hard constraints from ADRs.
- `docs/product-brief.md` — wedge + non-goals (permanent; scope test lives here).
- `docs/decisions.md` — ADR-001..007. ADR-001 (no cross-engine preemption) and
  ADR-004 (no auto conversion) are non-negotiable.
- `docs/roadmap.md` + `docs/14-closing-gates.md` — phase order, DoD, close gates.
- `docs/16-implementation-playbook.md` — stack + repository shape (mirrored below).
- Area specs: `docs/architecture.md`, `docs/data-model.md` (SQLite DDL),
  `docs/compatibility-engine.md`, `docs/engine-adapters.md`,
  `docs/residency-scheduler.md`, `docs/model-supply-chain.md`, `docs/api-spec.md`,
  `docs/security.md`, `docs/15-test-strategy.md`, `docs/20-ui-ux-spec.md`,
  `docs/21-observability-spec.md`, `docs/22-engine-version-policy.md`.
- Phase prompts: `prompts/01..07` — per-phase working prompts for implementers.

## Target machine (the reference box — all gates close on this hardware)

- Windows 10 Home 10.0.19045; PowerShell + Git Bash; **git-initialized 2026-07-06,
  local-only (no remote ever)** — commits remain the user's per global agent rules
  (C2); everything staged with COMMIT_MSG.txt ready.
- GPU: NVIDIA RTX 4060 Ti **16 GB** (compute capability 8.9), driver 576.40.
- System RAM: **16 GB** (hard constraint — see safeguards).
- Engines present: Ollama (`K:\Softwares\ollama\ollama.exe`), llama.cpp
  (`llama-server` via winget **and** a local CUDA build at
  `K:\projects\llm_models\llama.cpp-tq3\build\bin\llama-server.exe` — verify which
  flags are upstream vs fork-only before relying on them). **vLLM absent; vLLM has no
  native Windows support** — Linux-only via Docker Desktop/WSL2 (docker-desktop present).
- Model/download storage: `K:` drive, budget ≤ ~30 GB of NEW downloads.

## Existing assets to reuse (discovered 2026-07-06, per user)

- `K:\projects\llm_models\models\` — ~60 GB of real GGUFs already on disk (Qwen3.x
  27B/35B TQ3 quants ~13 GB each, vision mmproj files, small OCR/embedding models).
  Use these for Phase 0 trials and the Phase 2 validation matrix — near-VRAM-limit and
  won't-fit cases WITHOUT new large downloads.
- `K:\projects\llm_models\run-*.bat` — battle-tested llama-server launch profiles
  (hot/balanced/cold idle-unload profiles, KV-cache quant tuning `-ctk q4_0 -ctv tq3_0`,
  `-fa on`, full-offload-or-fail strict GPU mode, Ollama-unload preflight before cold
  starts). Direct input to the llama.cpp adapter's flag templates and the friction log.
- `K:\projects\llm_models\lmwrapper_project\` — prior-art Python wrapper (allocator,
  restart-queue semantics, Ollama-replacement gap analysis). Mine its queue/allocator
  semantics, documented failures, and Windows process-management lessons before
  designing `internal/residency`. Findings recorded in
  `research/lmwrapper-reuse-notes.md` (produced during P0.2).

## Decisions (made 2026-07-06 by orchestrating agent under user directive "whole project done"; user dismissed the interactive question — flagged for user review)

1. **ADR-006 resolved: Go control plane.** Single binary, `gguf-parser-go` imported
   natively, React+Vite frontend embedded, SQLite (WAL). Record resolution in
   `docs/decisions.md` (status Proposed → Accepted) as part of Phase 1.
2. **Phase 0 gate: executed honestly, GO_STANDALONE pre-decided by user directive.**
   Friction log is still produced for real (it drives feature priorities). If existing
   tools genuinely cover the bundle, surface loudly in the log — but the build proceeds.
3. **vLLM verification strategy:** build the adapter fully against vLLM's documented
   CLI/HTTP contract; continuous testing via a stub/contract server on Windows; **one
   controlled real end-to-end session** in Docker (WSL2, GPU passthrough) with a tiny
   model (≤1B), then torn down. Ollama + llama.cpp are continuously verified natively.
4. **Model budget:** small models (0.5B–8B quants), ≤ ~30 GB total on `K:`. Won't-fit
   verdicts validated primarily via metadata/estimates plus a small number of controlled
   negative load attempts (see safeguards), not by bulk-downloading 30B+ models.
5. **Hardware probe on Windows:** control plane must not link CUDA (ADR-005 spirit);
   NVML via cgo is painful on Windows. Probe is an interface with two impls:
   `nvidia-smi` subprocess (XML/CSV) now, native NVML later on Linux. VRAM-delta
   verification for stop/evict uses the same probe.
6. **Orchestration:** the main session orchestrates and verifies; implementation is
   delegated to Opus subagents at xhigh effort (Workflow `effort:'xhigh'`,
   `model:'opus'`), one TODO task per unit of work. Every mutation-capable phase output
   is reviewed (code-review agents) before its gate is attempted.
7. **User-directed scope additions (2026-07-06):** the user explicitly requires, in
   addition to the roadmap: (a) a **priority queue with a custom, documented
   algorithm** for admission/queue ordering (priority classes + aging to prevent
   starvation + size/cost awareness; still whole-model residency — ADR-001 untouched);
   (b) **async inference** — submit-job API returning a job id with poll + push
   result delivery; (c) **WebSocket** transport for live dashboard state and job
   events (alongside the spec'd SSE); (d) a **JSON-RPC 2.0** control surface exposed
   as a thin layer over the same internal service core as REST. These pass the
   product's scope test (single-box phone-ops/API features, not gateway/chat/k8s) and
   are recorded here as the user's product-owner decision. Architecture rule: one
   service core; REST, SSE, WebSocket, JSON-RPC are thin transports over it.

## Spec conflict resolutions (orchestrator, 2026-07-06 — implementers follow THESE where docs disagree; full conflict list in temp-research/spec-digest.md §13)

1. **Transport:** api-spec.md says SSE-only; user directive adds WS + JSON-RPC
   (decision 7). Resolution: SSE stays as specced; WS and JSON-RPC are additional thin
   transports over the same core. Docs get updated when the code lands.
2. **Queue:** residency-scheduler.md's "no starvation guarantees in v1" is superseded
   by decision 7(a): P5.0 authors the custom algorithm (priority + aging + size/cost)
   and adds additive migration columns to the `queue` table.
3. **"NVML" in docs** always means "the hardware-probe interface" (decision 5):
   nvidia-smi subprocess on Windows now, native NVML on Linux later. Never link go-nvml
   on the Windows box.
4. **vLLM adapter launch modes:** `subprocess` (Linux) and `docker` (Windows/anywhere,
   docker CLI) + `attach` (pre-existing endpoint). On this box only `docker`/`attach`
   are usable; P4.5 verifies the docker mode once.
5. **Windows portability:** engine-adapters.md POSIX signals/paths are Linux-first;
   implementers use a process-management abstraction (kill-tree via Job Objects/
   taskkill, portable paths, localhost bind avoids firewall prompts). Follow the
   Windows lessons in research/lmwrapper-reuse-notes.md (terminate→kill after 5s,
   CREATE_NO_WINDOW, memory-free polling).
6. **ADR-003** resolved in P0.3 from the llama-swap trial; Phase 5 implementers must
   read the resolution, not assume custom-build.
7. **ADR-006** Go: bookkeeping Proposed→Accepted happens in P0.3 (moved up from Phase 1).
8. **Ollama `/api/ps` field shapes are time-sensitive:** P1.3/P4.3 implementers must
   re-verify against current Ollama docs and cite them (CLAUDE.md rule).
9. **Margins reconciled:** estimator applies multiplicative `safety_factor = 0.92`
   (compat doc); residency admission adds additive `SAFETY_MARGIN_MIB = 1024` on top of
   estimates; the dev-test "1.5 GB margin" in safeguards below is testing policy, not a
   product constant. One constants file, documented, in `internal/compat`.
10. **Async jobs are net-new design** (no table/endpoints in docs): P6.2 designs
    schema + REST + WS events; reuse lmwrapper's event vocabulary
    (request.accepted/queue.update/model.loading/model.ready/response.delta/
    response.completed/heartbeat).

## Hardware safeguards (binding on every task; user constraint: "do not over use my hardware, use CPU as little as possible")

- **VRAM margin:** never intentionally load beyond `free VRAM − 1.5 GB` during dev
  tests; at most one resident model unless the test itself requires more.
- **No CPU inference:** llama.cpp test loads use full GPU offload (`-ngl 999`) and
  small contexts; never fall back to CPU-heavy inference for verification. Generation
  tests use tiny prompts/token counts.
- **Real loads are batched** into few, explicit test sessions (Phase 0 trial, Phase 2
  validation matrix, Phase 4 run/stop, Phase 5 load-storm); everything else uses
  metadata, stubs, and unit tests. Compile/test cycles are cheap; model loads are not.
- **Negative (won't-fit) validation** uses llama.cpp fail-fast behavior with
  `--no-mmap` avoided and context oversizing rather than giant downloads: pick a model
  whose weights fit but whose KV at huge ctx cannot, so failure is quick allocation
  failure, not swap-death. Never run a negative test that can exhaust system RAM.
- **vLLM/Docker session:** verify Docker Desktop WSL2 RAM cap (`.wslconfig`) ≤ 8 GB
  before the session; tiny model only; container stopped and WSL shut down after.
- **Cleanup is part of every test:** kill spawned engine processes, verify VRAM
  returned to baseline via probe, before the task's gate may close.
- **Downloads (revised after P0.1 audit):** K: has only ~25 GB free and C: ~6 GB free.
  New downloads are capped at **~10 GB cumulative**, small models only (≤4 GB each),
  resumable + hash-verified, on `K:`; validation leans on the existing ~60 GB of GGUFs
  in `K:\projects\llm_models\models\`. Delete scratch artifacts when a phase closes;
  never write model data to C:.

## Approach

Repository shape per `docs/16-implementation-playbook.md`: `cmd/localhub/`,
`internal/{api,db,hardware,engines/{ollama,llamacpp,vllm},compat,manifest,residency,metrics}`,
`web/`, `configs/`, `scripts/`. First code milestone is read-only (playbook): hardware +
engines + models endpoints, SSE, SQLite bootstrap, minimal React page. Windows is the
primary dev/verify target; keep code portable (build tags / interfaces) for Linux.

Phase order and gates are exactly `docs/roadmap.md` / `docs/14-closing-gates.md`;
task-level breakdown lives in `TODO.md`, verification criteria per task in
`TESTING.md`. Non-waivable gates (no false `runs`, no killed streams, manifests always,
localhost bind, no preemption scheduler, no auto-conversion, always a `reason`) apply to
every task, not just phase ends.

## Risks And Decisions (hazards)

- **16 GB system RAM** is the tightest resource: Docker/WSL2 vLLM, Vite builds, and Go
  builds must not run concurrently with model loads.
- **Windows quirks:** process management (kill trees, orphan reaping), path handling,
  `nvidia-smi` parsing stability across driver versions, port pooling, firewall prompts
  on listen (mitigate: bind localhost only — also a security gate).
- **Tailscale phone verification requires the user's device** — agent can verify the
  Serve endpoint programmatically; final phone check is a MANUAL gate item for the user.
- **Engine behavior claims are time-sensitive** (CLAUDE.md): each adapter task must
  re-verify current engine docs (Ollama API, llama-server flags, vLLM serve args) and
  cite them; `docs/decisions.md` revalidation log was last run 2026-07-06 (today — fresh).
- **ADR-003 (llama-swap vs custom residency)** resolves during Phase 0 trial;
  llama-swap ships Windows binaries (Go). Whichever way it lands, ADR gets updated.
- **No agent git writes**: user's global rules forbid the agent from git writes; all
  checkpoints are file-based (this workflow + `.planflow/`). The repo is git-initialized
  (local-only); the user commits at phase boundaries — flagged in TODO as user actions.
- **Recorded phase-order exception (2026-07-07)**: P1.6 (phone-over-Tailscale check) is
  a MANUAL user action blocked on an elevated Tailscale service restart; it does not
  block agent-side P2–P6/C1 work. All agent-preparable P1.6 items are done; the user
  check closes it under C2.
