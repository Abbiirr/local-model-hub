# LocalHub Compass — Complete Builder Handoff

Written 2026-07-08 at the owner's stop-point. This is the ONE document a builder —
human or AI agent, with zero prior context — reads to take this project anywhere:
finish it, extend it, or maintain it. It supersedes
`LOCALHUB_PROJECT_START_TO_END_HANDOFF.md` (the original pre-build brief, kept for
history).

**The product in one sentence:** *which local models run on this box (or these boxes),
on what engine, at what flags, still running, over my phone* — a single-binary Go
control plane + embedded React dashboard over llama.cpp, Ollama, and vLLM, with honest
VRAM-verified answers and a numeric reason on every no.

---

## 1. Read order for a fresh builder

1. `CLAUDE.md` — agent working rules (correctness bars, scope test, engine
   re-verification rule). Binding.
2. THIS file — orientation + the complete forward plan.
3. `docs/product-brief.md` (wedge + non-goals) → `docs/decisions.md` (ADR-001..008).
4. `TODO.md` (authoritative checklist) + `TESTING.md` (every gate's criteria).
5. `docs/future-work.md` — per-item pickup briefs for all open work.
6. The area doc for whatever you touch (map in §7).

Resume mechanics: `.planflow/HANDOFF.md` is the machine checkpoint;
`temp-research/context-summary.md` the narrative one. Both current as of the stop-point.

## 2. True current state (all verified, evidence cited)

**Everything below is SHIPPED and gate-passed** (gates in TESTING.md; live evidence in
`research/`):

| Area | What ships | Key evidence |
|---|---|---|
| Inventory + hardware | GGUF/safetensors/Ollama scan; nvidia-smi probe (GPU/CPU/RAM/disk) | P1 gates |
| Compatibility engine | gguf-parser-go wrapped + TQ3 fallback + iSWA KV floor + safetensors/GQA estimator; verdict-before-download; measured-peak calibration; **zero false `runs` across 21+ real models** | phase-2 matrix; optimization research §7 |
| Supply chain | Immutable manifests, HF commit pinning, sha256, resumable, ollama pull, local import, disk hard-cap (refuses before bytes), over-cap deletion | phase-3 gates; campaign #4 |
| Run/stop | State machine + Job Objects + startup reconciliation; llama.cpp (spawn), Ollama (warm/unload), vLLM (docker/attach); **stops verified by NVML delta, never HTTP-200 trust** | P4 gates |
| Residency | Custom priority queue (classes, aging, coalescing, persisted seq), admission with per-eviction VRAM re-reads, idle-only eviction, **never-kill-active proven live incl. non-pinned streams under 91% GPU** | P5 + Run 3 + campaign drill |
| Admission guards | RAM floor (`min_free_ram_mib`), GPU temp/util ceilings (`gpu_hot`/`gpu_busy`), all engines unified through one `admitGate` | admission_*_test.go |
| OpenAI proxy | `/v1/models|chat/completions|completions`, byte-passthrough streaming, blocked-503 in ~1.4 s with numeric reason, warm-route self-shadow fixed, Ollama tag rewrite | #p61 + amendments |
| Async jobs | Durable queue, restart-safe (`queued` survives, `running`→failed honestly), WS events, honest cancel semantics | #p62 |
| JSON-RPC 2.0 | 18 methods over HTTP + WS, spec-compliant framing/batches/id-echo | #p63 |
| GPU placement control | Full-GPU default everywhere (`-ngl all`, Ollama `num_gpu` forced) + `allow_cpu_offload` opt-in (flags incl. MoE `-ncmoe`, never relaxes admission) + `speculative:"ngram-cache"` opt-in | api-spec §instances |
| Dashboard | Minimal/modern, Run-per-row, fit-annotated Start picker, WS-live invalidations, queued-start surfacing, mobile-clean, 79 tests | screenshots + click-through evidence |
| Security | Enforced bearer middleware (`admin_token` + cluster token), loopback bind default, relay path allow-list, Tailscale story | security.md |
| Cluster (partial) | ADR-008 LAN federation: P7.1 node identity/join/heartbeat/registry + P7.2 aggregated node-attributed views with partial-failure semantics; `role: off` byte-identical | p71/p72 gate evidence |
| Field proof | Six-model campaign 6/6 — gemma-12B ran 49/49 layers on GPU end-to-end (verdict→download→manifest→run→NVML-verified unload); four models **correctly refused** with numeric reasons | model-campaign file |

Suites at stop-point: `go vet/build/test ./...` fully green; web 79/79; dist embedded.
Three Codex review cycles: 15 findings, all fixed with tests.

## 3. The complete forward plan

Three tiers. Tier A is specified to the gate level — build it as-is. Tier B is wanted
and lightly specified — write the small spec, then build. Tier C needs an owner
decision (ADR or scope call) before code.

### Tier A — specified + gated, ready to build (briefs: docs/future-work.md §1–4)

| # | Item | Spec / gate |
|---|---|---|
| A1 | **P7.3 placement + routed completions** — the cluster's money milestone | cluster-plan §P7.3+§6; TESTING#p73; future-work §1 has the P7.2 handoff notes |
| A2 | **P7.4 WS fan-in + jobs place-and-forward** | cluster-plan §P7.4; TESTING#p74 |
| A3 | **P7.5 cluster security hardening** (+ ADR-008 → Accepted after owner review) | cluster-plan §P7.5; TESTING#p75 |
| A4 | **Optimization live demos**: ngram A/B t/s, iSWA floor calibration vs NVML, derivable-MoE live unlock (needs one non-TQ3 MoE download) | optimization research §6/§7 |
| A5 | **Draft-model speculative decoding** — the one remaining engine feature: draft picker + vocab-compat check + two-model VRAM admission | research §5(iii); local b8833 flag appendix |

### Tier B — should-haves (wanted; small spec then build)

| # | Item | Why / notes |
|---|---|---|
| B1 | **Observability endpoints from docs/21** — the spec'd `/api/metrics`-class surfaces were never built (docs/21 predates the final API); reconcile the spec to shipped reality, then implement counters/timings (requests, admissions, evictions, tokens/s per instance) | docs/21-observability-spec.md |
| B2 | **`/v1/embeddings` proxy route** — deliberately excluded from P6.1 scope; the embedding models are already in inventory (nomic, Qwen3-VL-Embedding); same resolution path as chat | design §1.1 note |
| B3 | **Dashboard cluster view** — nodes panel (status/last_seen/hw_summary) + node attribution when clustered; API already exists (P7.2) | existing design system |
| B4 | **UI: speculative toggle + GPU-threshold readout** (gpu_hot/gpu_busy state surfaced); mirrors the `allow_cpu_offload` toggle pattern | InstancesCard/GpuPanel |
| B5 | **WS/browser auth** when `admin_token` set (query-param ticket or cookie) — today header-only, so a tokened dashboard can't open WS | KNOWN-LIMITATIONS #13 |
| B6 | **Per-model RAM estimates + RAM-aware victim selection** — the floor is global-only by design; the estimate needs llama.cpp mmap accounting research first | KNOWN-LIMITATIONS #2 |
| B7 | **vram_obs concurrency guard** — don't record whole-GPU samples as exact per-model peaks under concurrency (calibration poisoning) | KNOWN-LIMITATIONS #11 |
| B8 | **vLLM through-proxy live depth test** — a real safetensors model through `/v1` on vLLM docker (P6 used a stub per recorded decision); needs ≥8 GB free RAM | future-work §6 |
| B9 | **Engine upgrade protocol** — Ollama upgrade (unblocks ornith-class models); llama.cpp b8833→newer (spec-flag renames on master; re-run the --help diff; keep/refresh the TQ3 fork kernels) | future-work §6 |
| B10 | **docs/22 known-good version matrix + standalone troubleshooting.md** | values exist in CHANGELOG/docs-23/KNOWN-LIMITATIONS |
| B11 | **Job list pagination/retention policy** — jobs table grows unbounded; add retention config + list paging | data-model note |

### Tier C — vision items (owner decision / ADR required first)

| # | Item | Decision needed |
|---|---|---|
| C1 | **Linux support + native NVML probe; ROCm/AMD probe** — the probe interface was built for this; everything else is Windows-validated only | scope/test-hardware ADR |
| C2 | **Cluster v2**: mTLS/per-node tokens (recorded upgrade path), model weight transfer between nodes (excluded from v1), coordinator HA | extends ADR-008 |
| C3 | **Multi-GPU single box** — probe reports GPU array already, but admission/placement assume GPU 0 | ADR + hardware to test on |
| C4 | **Auto-calibration sweeps** — proactively load+measure models at low-traffic times to convert estimates→exact verdicts | GPU-time policy decision |
| C5 | **DSpark/DeepSpec-class decoding accelerations** — watch-list until llama.cpp/vLLM/Ollama ship support (verified absent 2026-07-07) | engine-driven timing |
| C6 | **Packaging/release**: signed binaries, winget manifest, auto-update channel — docs/19 release runbook exists; distribution is a product decision | docs/19-release-runbook.md |

**Permanent non-goals (still binding, ADR-008 amendment noted):** chat UI, multi-
provider gateway (LiteLLM's job), k8s/container orchestration, cross-engine OR
cross-node VRAM preemption, auto Safetensors→GGUF conversion in core, training,
multi-tenant auth. "Cluster" means LAN federation of autonomous daemons — nothing more.

## 4. Build protocol (what made this project work — keep it)

- **Gates before checkmarks.** Every task has a TESTING.md gate; nothing is "done"
  without evidence (unit + live where the claim is about hardware). A false `runs`
  that OOMs is a correctness bug; stops are verified by NVML delta, not HTTP 200.
- **One core.** Transports (REST/SSE/WS/JSON-RPC/proxy/cluster) stay thin over
  `core.Service`; per-node core never delegated (cluster coordinator ASKS nodes).
- **Never invent engine behavior.** Verify against the LOCAL binaries' `--help` and
  current primary docs, cite in code comments, log in docs/decisions.md revalidation.
- **Hardware safeguards binding on every agent** (PLAN.md): RAM ≥2.5 GB pre-flight,
  VRAM baseline ±200 teardown, small models, ≤2 resident, nothing big to C:, never
  `wsl --shutdown`, orphan checks after every live session.
- **Orchestration that worked:** Opus subagents with strictly disjoint file
  territories; the orchestrator independently re-verifies every gate; planflow
  checkpoint after each close; Codex review after every major wave (15/15 findings
  fixed to date); package-scoped tests while agents run concurrently, one full-tree
  suite at each landing.
- **Git:** local-only, NO remote ever; agents never commit — the owner runs
  `git add -A && git commit -F COMMIT_MSG.txt` (message current as of stop-point).

## 5. Owner actions outstanding

1. The commit (above). 2. P1.6 phone check: admin PowerShell →
`Restart-Service Tailscale -Force` → `scripts/tailscale-serve.ps1` → open ts.net URL
on the phone. 3. Sign-offs: PLAN.md#decisions + ADR-008/cluster-plan §0 checklist.

## 6. Reference box (all gates validated here)

Win10, RTX 4060 Ti 16 GB (CC 8.9, driver 576.40), Ryzen 5 2600, 16 GB RAM (RAM binds
before VRAM — hence the floor), K: models dir, C: nearly full. Engines: llama.cpp
b8833 (winget Vulkan + tq3 CUDA fork — fork is the CUDA path), Ollama 0.21.2 (old —
see B9), vLLM via Docker/WSL2. Go 1.23.4; node via nvm (`K:\Softwares\nvm\v24.11.1`).

## 7. Document map

| Doc | Role |
|---|---|
| CLAUDE.md / AGENTS.md | Agent rules (binding) |
| TODO.md / TESTING.md / PLAN.md | Checklist / gates / plan+safeguards+decisions |
| docs/future-work.md | Pickup briefs for ALL open work (Tier A detail) |
| docs/cluster-plan.md | Full cluster design + ADR-008 + P7 phases |
| docs/decisions.md | ADR-001..008 + source revalidation log |
| docs/api-spec.md · architecture.md · security.md · data-model.md | Shipped-truth specs |
| docs/compatibility-engine.md · residency-*.md · engine-adapters.md | Subsystem designs |
| docs/21-observability-spec.md | Spec ahead of implementation (Tier B1) |
| docs/19-release-runbook.md · 22-engine-version-policy.md | Release path (Tier C6/B10) |
| KNOWN-LIMITATIONS.md / CHANGELOG.md / README.md | Honest limits / history / front door |
| research/ | All live gate evidence (storms, campaign, cluster rigs) |
| temp-research/ | Design contracts + research (phase-6, optimizations, spec digests) |
