# Full Project Review - 2026-07-06

Review requested: "review everything here specially the tech stack and PLAN.md ... with references, use web search, do deep research."

Reviewer: Codex  
Repository: `K:\projects\ai\local-model-hub`  
Date: 2026-07-06

## Executive Verdict

LocalHub Compass has moved well past a planning bundle. The repository now contains a substantial Go daemon, SQLite migrations, engine adapters, compatibility estimators, manifest/download logic, a React/Vite dashboard, residency/admission code, and a large evidence trail. The chosen core stack - Go control plane, React/Vite UI, SQLite WAL, external engine processes, `nvidia-smi` hardware probe on Windows, `gguf-parser-go` for GGUF estimation - is directionally correct for the product constraints in `PLAN.md` and the ADRs.

The project is not complete. The true current state is:

- P0-P4 are marked complete and have credible supporting evidence.
- P5.0-P5.2 are marked complete.
- P5.3 is explicitly open: "NEARLY DONE" with two remaining live demos.
- P6.1-P6.3 are not implemented.
- P1.6 phone-over-Tailscale is blocked on a user/elevated-admin action.
- C1/C2 close-out is not complete.

The biggest technical risks are not "wrong stack." They are release-gate honesty and a few sharp mismatches between docs and implementation:

1. `admin_token`/Tailscale identity auth is documented in config and security docs, but there is no request-auth middleware enforcing it.
2. `web/dist` is required by `go:embed`, but `web/.gitignore` ignores it while root `.gitignore` says it is intentionally committed. A clean checkout can fail `go build` unless `npm run build` is run first.
3. `README.md`, `docs/architecture.md`, and `docs/api-spec.md` are materially stale against shipped code and `PLAN.md`.
4. P5 real-hardware validation exposed a system RAM ceiling. Admission is VRAM-centered, but this 16 GB RAM box can become RAM-bound before VRAM-bound.
5. The non-pinned active-stream safety story is only complete once P6 proxy stream counting is the normal ingress path.

## Verification Performed In This Review

Commands run locally:

```powershell
go vet ./...
go test ./...
go build ./...
$env:Path = 'K:\Softwares\nvm\v24.11.1;' + $env:Path; npm test
$env:Path = 'K:\Softwares\nvm\v24.11.1;' + $env:Path; npm run build
git status --short
git check-ignore -v web\dist\index.html web\dist\assets\index-CxhkZPto.js
```

Results:

- `go vet ./...`: PASS.
- `go test ./...`: PASS across backend packages.
- `go build ./...`: PASS in the current working tree.
- `npm test`: PASS, 9 test files, 62 tests.
- `npm run build`: PASS, Vite produced `web/dist`.
- Default shell did not have `npm`/`node` on PATH. Frontend commands pass only after prepending `K:\Softwares\nvm\v24.11.1`.
- `web/dist` and `web/node_modules` are ignored. `web/dist` exists locally, so `go build` passes here, but it is not protected as a tracked build input.

## Repo/Worktree State

This is a very large staged project, not a small patch:

- `rg --files` reports 304 files.
- `git status --short` showed 307 entries at review time.
- Most files are staged as added; several working-tree changes/untracked/staged additions reflect active in-progress work.
- `PLAN.md` still says the target machine is "not a git repository" and says the user must `git init` later, but `git rev-parse --show-toplevel` returns `K:/projects/ai/local-model-hub`.
- `TODO.md`/`TESTING.md` still include user action "git init + initial commit", which is stale at least for `git init`.

References: `PLAN.md:30-33`, `PLAN.md:180-181`, `TODO.md:56-59`, `TESTING.md:224-227`.

## Tech Stack Review

### Backend: Go

Verdict: good choice for the stated product.

The code uses Go as a single process control-plane daemon:

- Module: `github.com/localhub/localhub`.
- Key dependencies: `github.com/gpustack/gguf-parser-go`, `modernc.org/sqlite`, `github.com/coder/websocket`, `gopkg.in/yaml.v3`, `golang.org/x/sys`.
- `cmd/localhub/main.go` is mostly a wiring layer over `internal/*`, matching the "thin transports over one service core" intent.

This matches ADR-006 and the install-friction argument in `PLAN.md`.

References: `go.mod:1-17`, `docs/decisions.md:128-142`, `PLAN.md:61-63`, `cmd/localhub/main.go`.

### Database: SQLite WAL

Verdict: good fit.

The implementation uses `modernc.org/sqlite`, a pure-Go driver, with WAL, busy timeout, foreign keys, and `synchronous=NORMAL` in the DSN. That is aligned with single-box state, avoids cgo friction on Windows, and follows the "control plane must not link CUDA/native internals" posture.

One caveat: the code comment says `MaxOpenConns` is pinned, but the visible `internal/db/db.go` implementation does not actually call `SetMaxOpenConns`. That is not necessarily a bug, but the comment should be made true or removed.

References: `internal/db/db.go:1-55`, `internal/db/migrations/0001_initial_schema.sql:6-9`, SQLite WAL documentation.

### Frontend: React/Vite

Verdict: good enough, but build packaging needs cleanup.

The frontend is React 18 + Vite + TypeScript/Vitest. Tests and production build pass when Node is on PATH. The daemon embeds `web/dist` via Go `embed.FS`.

The packaging mismatch is important:

- `web/embed.go` says `web/dist` must exist and says "committed dist keeps a plain go build ./... working."
- root `.gitignore` says `web/dist` is deliberately committed.
- `web/.gitignore` ignores `dist/`.
- `git check-ignore` confirms `web/dist/index.html` is ignored by `web/.gitignore`.

This means a clean checkout does not reliably satisfy `go:embed all:dist`. Either commit `web/dist` by removing/overriding the ignore, or update the build contract to state that `scripts/dev.ps1 build/check` must run before `go build`.

References: `web/embed.go:6-21`, `.gitignore:10`, `web/.gitignore:2`, `scripts/dev.ps1:12-22`.

### Hardware Probe

Verdict: implementation choice is correct; docs are stale.

`PLAN.md` resolves "NVML" to the probe interface, with `nvidia-smi` subprocess on Windows and native NVML later. The implementation follows that by using an `internal/hardware` probe and `nvidia-smi` parsing.

`docs/architecture.md` still says "NVIDIA-first via NVML (`go-nvml` or `pynvml`)" and labels the component `(NVML/ROCm/psutil)`. That is stale and potentially misleading because ADR-005 says engines are external and the control plane should not link CUDA/native internals.

References: `PLAN.md:105-108`, `docs/architecture.md:63-66`, `internal/hardware/nvidia_smi.go`, `docs/decisions.md:114-124`.

### Engine Adapters

Verdict: architecture is sound; keep source revalidation strict.

The repo implements separate adapters for Ollama, llama.cpp, and vLLM. This matches the design:

- Ollama is treated as a shared daemon via HTTP and `/api/ps`.
- llama.cpp is a supervised child process.
- vLLM supports docker/attach/subprocess modes, with Windows routed through Docker/WSL2 or attach.

External source check supports the current direction:

- vLLM's current GPU install docs still say Linux is required and Windows is not natively supported; WSL/community forks are the path on Windows. They also list NVIDIA compute capability >= 7.5.
- vLLM sleep APIs still require explicit sleep-mode setup and are dangerous control endpoints, not a public scheduler primitive.
- vLLM GGUF support remains experimental and moved to the out-of-tree `vllm-gguf-plugin`.
- Ollama documents `keep_alive`, `/api/ps`, and coarse queue/concurrency environment variables.
- llama.cpp server documents OpenAI-compatible routes for completions/chat/embeddings and server monitoring.

References: `internal/engines/*`, `docs/decisions.md:168-176`, vLLM GPU docs, vLLM sleep docs, vLLM GGUF docs, Ollama FAQ/API docs, llama.cpp server README.

### Realtime/API Surfaces

Verdict: implementation is ahead of some docs.

The API currently has REST, SSE hardware live stream, WebSocket `/api/ws`, instance start/stop/list, queue list/cancel, supply-chain routes, compat routes, and a counted instance completion relay.

`docs/api-spec.md` still frames realtime as "prefer SSE over WebSocket for MVP." That contradicts shipped P1.5 WebSocket code and `PLAN.md` decision 7, which makes WebSocket and JSON-RPC first-class additional thin transports.

References: `internal/api/server.go:63-112`, `internal/api/ws.go:28-37`, `internal/api/instances.go:153-162`, `docs/api-spec.md:78-85`, `PLAN.md:86-98`.

## PLAN.md Review

### What PLAN.md Gets Right

- It preserves the correct product boundary: single-box, not GPUStack/Kubernetes, not a chat UI, not LiteLLM, no cross-engine preemption, no automatic Safetensors-to-GGUF conversion.
- It documents the exact reference box and real constraints.
- It records important user-directed additions: priority queue, async inference, WebSocket, JSON-RPC.
- It forces primary-source revalidation for engine behavior.
- It includes hardware safeguards that were actually useful during P5 testing.
- It honestly identifies vLLM-on-Windows as Docker/WSL2/attach, not native.

References: `PLAN.md:3-11`, `PLAN.md:30-41`, `PLAN.md:59-92`, `PLAN.md:94-123`, `PLAN.md:127-149`.

### PLAN.md Issues

1. Git state is stale. `PLAN.md` says not a git repository; this is now false.
2. Phase order has a real exception. `PLAN.md` and `TODO.md` say do not start a phase before the prior gate is closed, but later phases proceeded while P1.6 remained user-blocked. That may be pragmatic, but it needs an explicit recorded exception: "P1.6 is user/manual and does not block agent-side P2-P6 work."
3. The user-directed decisions were made by an orchestrating agent and flagged for user review. That review/signoff remains in C2.
4. Async jobs are explicitly "net-new design"; P6.2 is still unimplemented.
5. JSON-RPC is planned but not implemented. Some comments imply it is part of the transport architecture, but actual routes do not exist yet.

References: `PLAN.md:32-33`, `PLAN.md:59-60`, `PLAN.md:86-92`, `PLAN.md:122-123`, `TODO.md:19`, `TODO.md:52-54`, `TESTING.md:226-227`.

## Phase/Feature Status

### Phase 0 - Field Validation

Status: credible and useful.

The friction log is strong. It validated the project wedge using real local models, gguf-parser, llama-server, Ollama, llama-swap, and `nvidia-smi`. It found the real differentiators:

- TQ3 GGUFs break `gguf-parser-go` v0.24.1 metadata parsing.
- Live free VRAM matters more than nominal VRAM.
- mmproj/projector memory can be missed.
- Ollama, gguf-parser, and NVML disagree on VRAM figures.
- llama-swap is a good executor but not a VRAM-aware admission controller.
- vLLM is not native on Windows.

References: `research/phase-0-friction-log.md`, `docs/23-phase-0-field-validation.md:34-77`.

### Phase 1 - Observability

Status: mostly done; phone path blocked.

P1.1-P1.5 are implemented. Dashboard, API, SSE, and WebSocket exist. P1.6 is blocked on Tailscale service state and a manual phone check.

Risk: the security/auth layer is weaker than docs suggest. `AdminToken` is configured, but this review found no middleware enforcing Bearer auth. Comments in handlers say auth is enforced by middleware, but middleware only logs and recovers panics.

References: `TODO.md:12-19`, `TESTING.md:63-76`, `internal/api/middleware.go`, `internal/config/config.go:34-44`, `internal/api/instances.go:13-16`.

### Phase 2 - Compatibility Engine

Status: strong unit/evidence base, with one calibration risk.

The code includes GGUF and safetensors estimators, rule engine verdicts, fallback behavior, and calibration storage. The evidence claims 20+ model validation rows and zero false-positive `runs`.

Risk: P5 evidence flagged `vram_obs_mib` possibly recording whole-GPU used memory under concurrency rather than per-model delta. If persisted as "exact" observed peaks, it can poison future fit verdicts.

References: `internal/compat/*`, `research/phase-2-validation-matrix.md`, `research/phase-5-load-storm.md`, `temp-research/c1-master-gate-audit.md`.

### Phase 3 - Supply Chain

Status: implemented according to TODO, but needs C1 walk.

The code has manifest/download/HF/Ollama pull/local import paths and tests. The close-out still needs a spot-walk confirming manifest fields and reproduce export for a real downloaded/imported model.

References: `internal/manifest/*`, `internal/core/supply.go`, `TODO.md:29-33`, `TESTING.md:108-124`.

### Phase 4 - Controlled Run/Stop

Status: implemented and tested, with real-box claims recorded.

The adapters and instance lifecycle exist for llama.cpp, Ollama, and vLLM. The source and code design match the external engine docs. Keep vLLM image/version pinning under the engine version policy, because vLLM CLI and model-resolution behavior drift.

References: `internal/instances/*`, `internal/engines/llamacpp/*`, `internal/engines/ollama/*`, `internal/engines/vllm/*`, `TODO.md:35-41`, `TESTING.md:126-155`.

### Phase 5 - Residency

Status: not closed.

The design and code have advanced materially. The queue algorithm is documented, P5.1/P5.2 tests are green, live runs proved several behaviors, and the review commands passed.

But P5.3 remains open in both `TODO.md` and `TESTING.md`. Remaining live demos:

- Stream temporally overlapping an eviction.
- Queue auto-drain firing live.

The blockers are real hardware/RAM constraints, not just missing unit tests. Do not mark Phase 5 closed until those are demonstrated or explicitly waived with a documented reason.

References: `TODO.md:43-48`, `TESTING.md:176-193`, `docs/residency-priority-queue.md`, `research/phase-5-load-storm.md`.

### Phase 6 - Thin Proxy, Async Jobs, JSON-RPC

Status: not implemented.

There is a counted instance completion relay that becomes useful substrate for P6, and `temp-research/phase-6-design.md` is detailed. But these are still open TODO items:

- `/v1/models`, `/v1/chat/completions`, `/v1/completions`.
- Durable async jobs.
- JSON-RPC 2.0 over HTTP POST and WebSocket.

References: `TODO.md:50-54`, `TESTING.md:195-216`, `temp-research/phase-6-design.md`, `internal/api/instances.go:153-162`.

## High-Priority Findings

### 1. Auth Is Modeled And Documented But Not Enforced

Severity: high for any non-loopback or shared-machine use.

Evidence:

- `server.admin_token` exists in config.
- `docs/security.md` describes optional bearer token and Tailscale identity.
- API comments say admin actions are auth-enforced by middleware.
- `internal/api/middleware.go` has recover/log middleware only; no token or Tailscale header enforcement was found.

Impact:

- Default loopback bind reduces exposure, but `server.allow_nonlocal: true` would expose admin-capable controls with no functional token enforcement.
- Tailscale identity headers are not translated into auth decisions.

Recommendation:

- Either implement auth middleware before release, or change docs/config comments to say token/identity auth is planned but not implemented.
- If `allow_nonlocal` remains, require `admin_token` when non-loopback is enabled.

References: `docs/security.md:23-36`, `configs/config.example.yaml:11-17`, `internal/config/config.go:34-44`, `internal/api/middleware.go`, `internal/api/instances.go:13-16`.

### 2. Clean-Checkout Build Contract Is Broken/Ambiguous

Severity: high for release.

Evidence:

- `web/embed.go` requires `web/dist` for Go build.
- root `.gitignore` says `web/dist` is committed deliberately.
- `web/.gitignore` ignores `dist/`.
- `git check-ignore` confirms `web/dist/index.html` is ignored.

Impact:

- `go build ./...` passes on this machine because ignored `web/dist` exists locally.
- A clean clone/checkout can fail unless `npm run build` is run first.

Recommendation:

- Pick one contract:
  - Commit `web/dist` by removing `dist/` from `web/.gitignore` and force-adding the current build, or
  - Do not commit dist; update `web/embed.go`, `README`, and gate commands to require `scripts/dev.ps1 check` or `npm run build` before Go build.

References: `web/embed.go:6-21`, `.gitignore:10`, `web/.gitignore:2`, `scripts/dev.ps1:12-22`.

### 3. P5.3 Is Not Closed

Severity: high for product claims.

Evidence:

- `TODO.md` leaves P5.3 unchecked.
- `TESTING.md` says "NEARLY CLOSED - two live demos remain."
- `research/phase-5-load-storm.md` says Run 1/2 were partial and describes RAM-bound live-test gaps.

Impact:

- The core product promise "no OOM and no killed active stream under load" is not fully live-proven yet.
- The remaining proof matters because the exact risk is race/timing/resource pressure on the real box.

Recommendation:

- Finish the two micro-storms from `.planflow/HANDOFF.md` / `temp-research/context-summary.md`, or document a deliberate waiver with exact limitation language.

References: `TODO.md:43-48`, `TESTING.md:176-193`, `research/phase-5-load-storm.md`.

### 4. RAM Needs To Become A First-Class Admission Constraint

Severity: medium-high.

Evidence:

- P5 real-hardware tests aborted on system RAM floor, not VRAM.
- The reference box has 16 GB system RAM and active Docker/WSL workloads.
- Current residency narrative is mostly VRAM-focused.

Impact:

- A false "runs" may still be operationally unsafe if RAM pressure causes thrash, failed spawns, or user workload impact.
- P5 live demos may stay flaky unless RAM is explicitly accounted for.

Recommendation:

- Add RAM headroom to admission/start gates, at least for llama.cpp and vLLM Docker.
- Surface RAM-blocked reasons just like VRAM-blocked reasons.
- Add this to known limitations if not implemented before release.

References: `PLAN.md:127-149`, `research/phase-5-load-storm.md`, `TESTING.md:176-193`.

### 5. Documentation Drift Is Now Material

Severity: medium-high.

Examples:

- `README.md` says the repo contains "engineering handoff docs only"; this is false.
- `docs/architecture.md` describes NVML/go-nvml, not current `nvidia-smi`.
- `docs/api-spec.md` says SSE-first and treats WS as future optional, but WS shipped.
- `docs/api-spec.md` lacks async jobs and JSON-RPC.
- `PLAN.md` and `TESTING.md` say `git init` remains, while the repo is already git-initialized.
- `docs/18-source-validation-matrix.md` needs dated source citations for what was actually revalidated.

Recommendation:

- Execute `temp-research/docs-sync-plan.md` before C1.
- Update README status only after P6/P5 status is final, so it does not overclaim.

References: `README.md:59-64`, `docs/architecture.md:63-66`, `docs/api-spec.md:78-85`, `temp-research/docs-sync-plan.md`, `PLAN.md:32-33`.

### 6. Active Stream Safety Depends On Using The LocalHub Relay/Proxy

Severity: medium.

Evidence:

- P5 report states earlier direct-engine streams were invisible to `InFlightTracker`.
- Current code has `POST /api/instances/{id}/completions`, which counts around a relay.
- P6 proxy is not implemented yet.

Impact:

- Non-pinned direct calls to engine base URLs remain outside LocalHub's active-stream accounting.
- The "never kill active generation" invariant is strongest only when clients enter through LocalHub.

Recommendation:

- Make P6 proxy the documented inference ingress.
- For direct engine URLs, explicitly state that active-stream protection is not guaranteed unless pinned or mediated through LocalHub.

References: `internal/api/instances.go:153-162`, `research/phase-5-load-storm.md`, `temp-research/c1-master-gate-audit.md`.

## External Source Revalidation Summary

The web-researched assumptions still support the stack:

- vLLM: current docs say GPU installs require Linux and Python 3.10-3.13, Windows is not natively supported, and NVIDIA CUDA GPU support requires compute capability 7.5+. This supports the Docker/WSL2/attach strategy on Windows.
- vLLM sleep: sleep/wake is a special server capability requiring explicit sleep mode setup and is listed alongside dangerous/admin-style APIs. It should not become a public scheduling primitive.
- vLLM GGUF: official docs describe GGUF as experimental/under-optimized and require the out-of-tree `vllm-gguf-plugin`. Routing GGUF primarily to llama.cpp/Ollama remains correct.
- Ollama: official docs confirm `/api/ps`, `keep_alive`, unload via `keep_alive: 0`, and coarse concurrency controls such as max loaded models, parallelism, and queue size.
- llama.cpp: server README confirms OpenAI-compatible chat/completions/embeddings and monitoring endpoints, supporting the supervised-process adapter and thin proxy plan.
- gguf-parser-go: README says it can parse remote GGUF metadata with chunk/range reads and estimate memory. Local evidence still proves a TQ3 gap, so fallback and measured calibration are required.
- Tailscale Serve: docs confirm Serve proxies localhost into the tailnet, injects identity headers for tailnet traffic, strips spoofed headers, and recommends localhost binding when trusting identity/capability headers.
- SQLite WAL: docs confirm reader/writer concurrency benefits and same-host constraints, fitting this single-box daemon.
- GPUStack/LiteLLM/llama-swap: current docs confirm they own adjacent problem slices. This reinforces LocalHub's narrowed wedge and the decision not to become a GPUStack/LiteLLM clone.

## Source References

### Local

- `AGENTS.md` - agent rules and non-negotiable constraints.
- `docs/product-brief.md` - product wedge, target user, and non-goals.
- `docs/decisions.md` - ADR-001 through ADR-007 and 2026-07-06 source revalidation log.
- `PLAN.md` - current end-to-end plan, user-directed additions, safeguards, and phase approach.
- `TODO.md` - current checklist state.
- `TESTING.md` - gate criteria and current P1.6/P5.3/P6/C1 status.
- `research/phase-0-friction-log.md` - field validation and tool friction.
- `research/phase-2-validation-matrix.md` - compatibility validation matrix.
- `research/phase-5-load-storm.md` - P5 real-hardware evidence and remaining live gaps.
- `temp-research/c1-master-gate-audit.md` - close-gate staging audit.
- `temp-research/docs-sync-plan.md` - concrete stale-doc remediation plan.
- `temp-research/phase-6-design.md` - detailed P6 design.
- `go.mod`, `web/package.json`, `cmd/localhub/main.go`, `internal/api/*`, `internal/db/*`, `internal/residency/*`, `web/embed.go`.

### Web / Primary Sources

- vLLM GPU installation: https://docs.vllm.ai/en/stable/getting_started/installation/gpu/
- vLLM sleep mode: https://docs.vllm.ai/en/latest/features/sleep_mode/
- vLLM GGUF docs: https://docs.vllm.ai/en/stable/features/quantization/gguf/
- vLLM online serving: https://docs.vllm.ai/en/stable/serving/online_serving/
- Ollama FAQ: https://docs.ollama.com/faq
- Ollama `/api/ps`: https://docs.ollama.com/api/ps
- llama.cpp server README: https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md
- gguf-parser-go README: https://github.com/gpustack/gguf-parser-go
- Tailscale Serve docs: https://tailscale.com/docs/features/tailscale-serve
- SQLite WAL docs: https://sqlite.org/wal.html
- Go embed package docs: https://pkg.go.dev/embed
- GPUStack built-in inference backends: https://docs.gpustack.ai/2.0/user-guide/built-in-inference-backends/
- LiteLLM routing/load balancing docs: https://docs.litellm.ai/docs/routing-load-balancing
- llama-swap README: https://github.com/mostlygeek/llama-swap

## Recommended Next Actions

1. Finish P5.3 micro-storms or document an explicit waiver.
2. Implement or de-scope auth before any LAN/non-loopback claim.
3. Fix the `web/dist` build/commit contract.
4. Complete P6.1-P6.3 only after P5.3 status is honest.
5. Run the docs sync plan before C1.
6. Update README status, known limitations, changelog, troubleshooting, and source-validation matrix.
7. Remove/adjust stale "git init" instructions now that the repo is already initialized.

