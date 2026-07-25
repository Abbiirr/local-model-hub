# Future Work — everything not yet implemented, specified for pickup

> Orientation for a builder with zero context: start at **[../BUILDER-HANDOFF.md](../BUILDER-HANDOFF.md)**
> (the complete handoff — current state, the full A/B/C-tier forward plan, build
> protocol). This file is the Tier-A detail layer it references.

Written 2026-07-08 on the owner's instruction: "alright stop all work and only write
docs for all future work." All in-flight implementation was stopped cleanly (no partial
edits left in the tree; suite green at the stop point). This document is the single
entry point for resuming ANY of the remaining work — each item says what it is, why,
where its full spec lives, what exists already, and how its gate is proven.

State at the stop point: Phases 0–6 + C1 closed; post-1.0 extensions X1–X3, X7 done;
X4 campaign complete (6/6); X5 optimizations implemented (live demos pending); cluster
P7.1 + P7.2 shipped and gate-passed. `go vet/build/test ./...` fully green; web 79/79.
TODO.md is the authoritative checklist; TESTING.md holds every gate's criteria.

---

## 1. Cluster P7.3 — placement + routed completions (the money milestone)

**What:** the coordinator answers `POST /v1/chat/completions` for any model any node
can serve: local-first, then placement, then relay to the chosen node with streaming
byte-passthrough. Full spec: docs/cluster-plan.md §P7.3 + §6; gate: TESTING.md#p73.

**Already in place (P7.1/P7.2 shipped):** node registry with live status +
`hw_summary` per heartbeat (free VRAM for the tiebreak); node-attributed
`/api/cluster/models` fan-out (presence filter); the host-agnostic
`internal/proxy.Relay` (routing is composition, not new machinery); cluster token
accepted as a bearer for coordinator→node calls.

**Build notes (from the P7.2 handoff — follow unless concretely wrong):**
- Warm signal: extend `internal/cluster.Aggregator` with an `/api/instances` fan-out
  helper (HEALTHY instance for the model = warm). No new node endpoint.
- Cold-candidate fit: ask each candidate LIVE via `GET {base_url}/api/models/{id}/compat`
  — never cache verdicts (the false-`runs` correctness bar). The coordinator never
  computes fits itself (one-core-per-node).
- Policy order (binding, plan §6.2): already-warm > has-model-on-disk > runs beats
  runs_degraded > most-free-VRAM > round-robin. Human-readable placement reason trail
  ("node-b: warm"); blocked-everywhere → node-prefixed aggregate reason; no-node-has-it
  → 404 carrying per-node status.
- Promote the coordinator's own `GET /v1/models` to the union (P7.2 deliberately
  deferred this until routing exists — the deferral note is in `handleClusterV1Models`).
- Failure modes: mid-stream node death = truncated stream / 502-before-first-byte,
  NO transparent retry (mirrors RelayError + the jobs-restart honesty rule).
- role != coordinator stays byte-identical (extend the P7.1/P7.2 regression pattern).

**Gate evidence (live, one real 1B spawn):** coordinator with an EMPTY model dir +
node with K:/projects/llm_models/models; completion POSTed to the coordinator must
cold-start the 1B on the node and stream back through it; second call hits the warm
rung; 404-with-per-node-status; fast node-prefixed blocked reason for the 32B.
Two-daemon rig pattern: ports 39921/39922, fresh scratch DBs, safeguards (RAM ≥2.5GB
pre-flight, VRAM baseline ±200 teardown, no orphans).

## 2. Cluster P7.4 — WS fan-in + jobs place-and-forward

Spec: docs/cluster-plan.md §P7.4; gate: TESTING.md#p74. Coordinator subscribes to each
ONLINE node's `/api/ws` and republishes `{node_id, event}` on its own socket
(reconnect/backoff per node; node-attributed `instances.updated` proves it). Jobs
submitted to the coordinator are placed (same engine as P7.3) and forwarded; the job
record must be durable on the OWNING node (coordinator restart loses nothing; node
restart honors that node's own ReconcileJobs semantics). The dashboard needs no changes
for v1 (it already refetches on invalidation; node attribution display is optional
polish).

## 3. Cluster P7.5 — security hardening

Spec: docs/cluster-plan.md §P7.5 + §security; gate: TESTING.md#p75. Startup REFUSES a
clustered role on a non-loopback bind without the token trio (validation-time, clear
reason — partially enforced since P7.1; P7.5 completes per-endpoint scoping both
sides); negative-auth tests on every cluster endpoint incl. WS join; token rotation
runbook; Tailscale cluster guide in examples/; dev-endpoint allow-list audit extended
to the coordinator relay (never forwards vLLM dev endpoints cross-node). ADR-008 is
Proposed — owner review of docs/cluster-plan.md §0 checklist upgrades it to Accepted.

## 4. Optimization live demos (implementation is DONE; these are GPU sessions)

Everything below is spec'd in temp-research/optimization-support-research.md (§6 test
plan; §7 iSWA audit numbers). Code shipped 2026-07-08: `speculative:"ngram-cache"`
per-start opt-in, iSWA KV floor (internal/compat/gguf/iswa.go), MoE `-ncmoe` under
`allow_cpu_offload` (internal/compat/gguf/moe.go).

- **ngram speculation A/B:** same model (1B or gemma-12B), one run with
  `speculative:"ngram-cache"`, one without; ≥256-token completions; capture
  llama-server timings (`n_drafted`/`n_accepted`, t/s). Single-sample is fine for a
  first datapoint; label it as such.
- **iSWA floor calibration:** load gemma-4-12B for real, compare NVML-measured peak vs
  the floored estimate (9102 MiB @4K) — confirm conservative-correct; the
  observed-peak calibration store then takes over for exact verdicts.
- **Derivable-MoE live unlock:** needs a NON-TQ3 MoE GGUF (the inventory's 35B-A3B is
  TQ3 → sizing not derivable by design). Smallest useful download: any Q4-class MoE
  ≤12GB. Prove: `allow_cpu_offload:true` → `-ncmoe N` emitted, admission uses the
  expert-reduced footprint, model loads on 16GB, reason states the offload.
- **Draft-model speculative decoding (the remaining real FEATURE):** design sketch in
  the research doc §5(iii): draft-model picker + tokenizer/vocab-compat check +
  TWO-model VRAM admission (target + draft both resident) + `-md/-ngld/--draft-max`
  flags (exact names in the doc's local --help appendix). Enabler on this box: a
  same-family small draft (Qwen3 0.6B ~0.4–0.8GB for the Qwen 27Bs, or a small gemma-4
  for the 12B). Do NOT start from llama.cpp master docs — the local b8833 flag surface
  is the contract until the binary is upgraded.

## 5. Deferred docs & polish

- **docs/22-engine-version-policy.md known-good matrix** — values exist in
  CHANGELOG.md + docs/23; fill the matrix at next release.
- **Standalone docs/troubleshooting.md** — content currently folded into
  KNOWN-LIMITATIONS.md + examples/tailscale-serve.md; split out if wanted.
- **Dashboard cluster view** — P7.2's `/api/cluster/*` surfaces have no UI; a nodes
  panel (status/last_seen/hw_summary) is straightforward with the existing design
  system. Optional: node attribution on inventory rows when clustered.
- **UI for `speculative` + threshold visibility** — the Run/Start flows expose
  `allow_cpu_offload` only; the speculative toggle and a GPU-thresholds readout
  (gpu_hot/gpu_busy state) are small additions to InstancesCard/GpuPanel.

## 6. Known-limitations follow-ups (each documented in KNOWN-LIMITATIONS.md)

- Per-model RAM estimates + RAM-aware victim selection (floor is global-only, #2).
- vram_obs concurrency over-sampling → calibration poisoning guard (#11).
- WS/browser auth when admin_token is set (header-only today, #13).
- Ollama upgrade on the box (0.21.2) unblocks the ornith model (campaign #5, 412).
- vLLM through-proxy live depth test (P4 proved docker lifecycle; P6 used a stub per
  recorded decision; a real safetensors model through /v1 on vLLM is the remaining
  depth gap — needs ≥8GB free RAM for WSL2).
- llama.cpp binary upgrade path: master renames spec flags (`--spec-draft-*`,
  `draft-eagle3`, `draft-dflash`) — re-run the optimization research's --help diff
  before upgrading; the tq3 fork carries the TQ3 CUDA kernels (upgrade BOTH or keep
  the fork pinned).

## 7. Watch-list (verified out of scope today)

- **DSpark / DeepSpec** (DeepSeek-AI + PKU, 2026-06 — confidence-scheduled speculative
  decoding): not in llama.cpp/vLLM/Ollama as of 2026-07-07; targets DeepSeek-V4.
  Revisit when an engine ships it (sources + dates in the optimization research doc).
- Native NVML/ROCm probes (interface exists, implementations unshipped, #8).
- mTLS / per-node cluster tokens (recorded upgrade path in ADR-008).

## 8. User actions (unchanged, blocking final close-out)

1. **The commit:** `git add -A && git commit -F COMMIT_MSG.txt` (COMMIT_MSG.txt
   refreshed 2026-07-08 to cover the post-1.0 wave).
2. **P1.6 / master criterion #4:** admin PowerShell → `Restart-Service Tailscale
   -Force` → `scripts/tailscale-serve.ps1` → open the ts.net URL on the phone.
3. **Sign-offs:** PLAN.md#decisions + the ADR-008 / cluster-plan §0 review checklist.

---

*Resume protocol: read .planflow/HANDOFF.md (or temp-research/context-summary.md) →
pick an item here → its spec doc + TESTING gate are the contract. Orchestration
conventions that worked: opus subagents with disjoint file territories, orchestrator
independently re-verifies every gate, planflow checkpoint after each close, hardware
safeguards from PLAN.md binding on every agent, Codex review after every major wave.*
