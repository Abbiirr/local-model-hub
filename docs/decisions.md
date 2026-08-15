# Architecture Decision Records

Each ADR captures a load-bearing decision and the evidence behind it, so contributors (and
agents) understand *why* and don't quietly reverse it. Status: Accepted unless noted.

---

## ADR-001 — No cross-engine VRAM preemption scheduler

**Decision:** LocalHub does process-level residency (load/unload whole models) only. It
does **not** attempt weighted-fair VRAM scheduling or live preemption across engines.

**Why (evidence):**

- **vLLM** exposes sleep/wake, but only under `VLLM_SERVER_DEV_MODE=1 --enable-sleep-mode`,
  and vLLM's own docs say those endpoints must not be exposed to users / only in trusted
  networks. Level-1 sleep also needs CPU RAM ≥ model weights. It's a fast A↔B switch aid,
  not a general external preemption API.
- **Ollama** offers only coarse controls (`keep_alive`, `OLLAMA_MAX_LOADED_MODELS`,
  `OLLAMA_NUM_PARALLEL`) and its FAQ notes a new model must fully fit in VRAM to co-load.
  No external priority/preempt API.
- **llama.cpp** residency is "process is up or not." No fine-grained external VRAM control.
- **No shared allocator:** three engines = three CUDA contexts, each with its own
  allocator/KV cache. There is no cross-process VRAM pool to fairly divide.
- **MPS/MIG don't solve it:** MPS memory-limit priority is a performance hint and faults
  can affect co-tenant clients; MIG is *static* partitions with fixed profiles, not a
  dynamic eviction mechanism.

**Consequence:** the scheduler is admission control + eviction (whole models), always
re-measuring real free VRAM via NVML. This is buildable and honest. See
[residency-scheduler.md](residency-scheduler.md).

**This is the decision most likely to be wrongly "improved" back into fantasy. Don't.**

---

## ADR-002 — Don't reinvent the gateway or the estimator

**Decision:** Wrap existing best-in-class pieces instead of rebuilding them.

- **VRAM estimation (GGUF):** use `gguf-parser-go` (Apache-2.0). It's what GPUStack uses,
  reads metadata remotely without full download, and tracks upstream llama.cpp quant
  layouts. Rebuilding it means getting block sizes subtly wrong forever.
- **Gateway/routing:** the thin proxy is passthrough-only. Multi-provider routing, budgets,
  fallbacks, key management → that's **LiteLLM**, which users can put in front. Don't
  duplicate it.

**Why:** the wedge is the *bundle for single-box users*, not any one reinvented component.
Every wheel reinvented is maintenance debt against a faster-moving upstream.

---

## ADR-003 — Consider llama-swap as the swap executor before writing our own

**Decision (default):** In Phase 0/5, prototype residency by **driving llama-swap** rather
than hand-rolling process supervision. Build a custom residency manager only if a concrete
limitation forces it.

**Why:** llama-swap already does reliable process hot-swapping for any OpenAI/Anthropic-
compatible local server (llama.cpp, vLLM, …), with unload endpoints, running-model status,
TTL, groups, logs, and a web UI — i.e. most of Phase 4–5's mechanics. Reusing it cuts a
whole phase.

**Trade-off:** an external dependency vs. a single self-contained binary. If single-binary
distribution is paramount (see ADR-006) and llama-swap's eviction policy is too limited,
implement the minimal manager in [residency-scheduler.md](residency-scheduler.md).

**Status:** **Resolved 2026-07-06 (Phase 0 trial): build the custom residency manager;
do not drive llama-swap.**

**Trial outcome (P0.2, this box):** llama-swap v235 works exactly as advertised as an
*executor* — swap-on-request, `/running`, manual unload, TTL auto-unload all verified
with two local models. But it has no VRAM-aware admission, no fit estimation, no
priority queue, no verdict/`reason`, no margins — the entire safety layer LocalHub
exists to provide would still have to be built *above* it.

**Why custom wins here:** Phase 4 must build Ollama (shared daemon) and vLLM (docker)
adapters regardless — llama-swap only covers the llama.cpp slice, so the saved work is
small. Meanwhile driving it splits process-state ownership across two brains, which
conflicts with the Phase 4 state machine + restart-reconciliation requirements, and the
user-required custom priority queue (aging/size-aware; PLAN.md decision 7a) and async
job persistence need scheduler internals llama-swap doesn't expose. llama-swap's swap
semantics are retained as reference behavior (see research/phase-0-friction-log.md and
research/lmwrapper-reuse-notes.md). Revisit only if the custom manager's maintenance
burden proves the trade wrong.

---

## ADR-004 — Auto Safetensors→GGUF conversion is OUT of core; pickle is a warning, not a block

**Decision:** No automatic conversion pipeline in core. Safetensors→GGUF is an advanced,
manual, clearly-labeled action only. Pickle/`trust_remote_code` produce **warnings** by
default; hard-blocking is opt-in "strict mode."

**Why (conversion):**

- Good low-bit GGUF quant needs **imatrix** (importance-matrix) calibration; naive
  conversion yields worse models. That's a real workflow, not a button.
- `convert_hf_to_gguf.py` **lags and breaks** on new architectures; auto-conversion at
  scale is an endless upstream-chasing maintenance sink.
- Tokenizer/chat-template drift during conversion causes subtle wrong outputs.
- Storing both formats **doubles disk** per model.
- **vLLM's own GGUF support is experimental/under-optimized and moved out of core**, with
  an RFC toward removing GGUF/bitsandbytes from core — so "convert to GGUF to run on vLLM"
  is a dead end anyway. Route by format instead: safetensors→vLLM, GGUF→llama.cpp/Ollama.

**Why (pickle warning not block):** many popular models still ship pickle or need
`trust_remote_code`; a default hard block pushes users to disable safety wholesale. Loud
warning + optional strict mode preserves both usability and safety. See
[security.md](security.md) and [model-supply-chain.md](model-supply-chain.md).

---

## ADR-005 — Engines are supervised external processes, not forked/patched libraries

**Decision:** Start/stop/health-check/observe engines via their CLIs and HTTP APIs. Never
fork or patch them; never link into their memory internals.

**Why:** the three engines move fast and independently. Coupling to internals means
breaking on every upstream release. The stable contract is their process lifecycle + HTTP
API. This is also what keeps the control plane CUDA-free and portable.

**Consequence:** capabilities are bounded by what the engines expose externally — which is
exactly why ADR-001 holds.

---

## ADR-006 — Control-plane language: bias to Go for the product, Python only to prototype UX fast

**Decision:** For the shippable product, prefer a **Go** control plane (single static
binary, imports `gguf-parser-go` natively, embeds `tsnet`, bundles the frontend). Use
**Python/FastAPI** only if you need the fastest path to a UX-validating demo, treating a Go
daemon as a real milestone, not "someday."

**Why:** for a FOSS tool, install friction is the primary adoption lever. A single binary
(`curl | tar | run`, no Python/venv/CUDA in the control plane) beats a Python app with
dependencies. The best estimator is already a Go library. Frontend = React/Vite, DB =
SQLite(WAL) regardless.

**Status:** **Accepted 2026-07-06 — Go.** (Recorded in Phase 0 close-out; decision made
by orchestrating agent under project-owner directive, per PLAN.md decision 1. Go 1.23.4
verified on the reference box; frontend React/Vite via Node 24.11.1/nvm; SQLite WAL.)

---

## ADR-007 — Single box first; multi-node is not an MVP shape

**Decision:** The data model, API, and scheduler assume one host. Multi-node is additive
and deferred until real demand.

**Why:** GPUStack/KubeAI/Xinference-distributed already own the cluster case and are heavy
for it. LocalHub's wedge is the single-box user. Baking cluster assumptions in early would
turn it into a worse GPUStack. Keep the single-box model clean; add nodes later behind the
same adapter contract if warranted.

---

## How to add a new ADR

Copy the format: **Decision / Why (evidence) / Consequence / Status**. If you're reversing
an accepted ADR, write a new one that supersedes it and link both — don't edit history.
Re-verify time-sensitive claims (engine capabilities) before relying on an old ADR; this
space changes monthly.


---

## Source revalidation log — 2026-07-06

- GPUStack current built-in backend docs list vLLM, SGLang, MindIE, and VoxBox.
- Ollama still exposes coarse residency controls through `keep_alive`, `/api/ps`, and daemon environment variables such as `OLLAMA_MAX_LOADED_MODELS`, `OLLAMA_NUM_PARALLEL`, and `OLLAMA_MAX_QUEUE`.
- vLLM sleep/wake server endpoints remain gated behind `VLLM_SERVER_DEV_MODE=1 --enable-sleep-mode` and are not suitable as public user-facing scheduling controls.
- vLLM GGUF support remains experimental/under-optimized and requires the out-of-tree `vllm-gguf-plugin`.
- Tailscale Serve remains the preferred phone-access path because it proxies a localhost service to the tailnet, applies ACLs, and can pass identity headers.

Re-run this source revalidation before changing ADR-001, ADR-002, ADR-004, or any engine adapter behavior.

## ADR-008 — LAN federation of autonomous daemons (supersedes ADR-007 in part)

**Decision:** LocalHub gains an optional cluster mode: a set of fully autonomous
LocalHub daemons on a LAN/tailnet, federated by ONE coordinator role (a config-selected
mode of the same binary, hub-and-spoke) that adds a node registry (push join +
heartbeat, soft state), aggregated inventory/hardware views, verdict-based placement
(the coordinator ASKS each node for its own live verdict and free VRAM; it never
computes fits itself), and OpenAI-request routing to the owning node via the existing
host-agnostic relay. Per-node behavior — residency, admission, never-kill-active,
NVML-verified VRAM truth, manifests, reasons — is untouched; a node with clustering off
is byte-identical to 1.0.

**Why:** Project-owner directive, 2026-07-08 (verbatim): "also in future we shall be
able to run it in cluster, add support for it, we shall implement and plan for it,
write down the plans first and then start implementing", clarified as "cluster means
not necessarily k8s or anything like that simple over the network gpu clusters". This
amends ADR-007's "multi-node is deferred until real demand" — the demand is the owner's.
ADR-007's *reasoning* survives: LocalHub still refuses to become a worse GPUStack, so
the federation is the thinnest layer that answers "which of my boxes can run this, and
route my request there." Hub-and-spoke because at 2–4 owner-controlled boxes a single
coordinator eliminates all consensus/split-brain machinery, keeps failure legible
(coordinator down ⇒ nodes fully functional locally), and any box can be made
coordinator by config.

**Carried-forward non-goals (binding):** no k8s/container orchestration; no cross-node
VRAM preemption or fair-share scheduling (ADR-001 applies a fortiori); no model weight
transfer between nodes in v1 (placement routes only to nodes that have the model; a
node acquires models via its own supply chain); no distributed state store (coordinator
soft state rebuilt from heartbeats; only an additive `nodes` table); no cross-box
tensor/pipeline parallelism; no multi-tenant auth.

**Consequence:** first feature to leave localhost ⇒ security posture changes are
explicit and validation-enforced: a distinct shared cluster bearer token mandatory
whenever clustered on a non-loopback bind, Tailscale as the recommended transport
(loopback binds preserved), mTLS/per-node tokens as the recorded upgrade path. Full
design, placement policy, failure modes, and P7.x gates: docs/cluster-plan.md.

**Status:** Proposed 2026-07-08 (owner-directed; pending owner review of
docs/cluster-plan.md §0 checklist; implementation proceeding under the owner's
plan-then-implement directive).

---

## ADR-009 — Retire Ollama as a LocalHub runtime

**Decision:** LocalHub serves GGUF exclusively through supervised llama.cpp instances;
Ollama is not configured, detected, warmed, or used for downloads in new deployments.
Safetensors/AWQ continue to route to vLLM.

**Why:** A supervised llama.cpp process gives LocalHub deterministic lifecycle ownership,
per-model ports, and verified VRAM release. Ollama is a shared daemon with opaque
content-addressed blobs and asynchronous unload semantics, which conflicts with the
single-box residency correctness bar.

**Migration boundary:** an Ollama tag is not a llama.cpp model path. LocalHub must not
relabel it as one. Existing Ollama artifacts require a standalone GGUF import or
re-download; multi-file/modifier artifacts remain manual migration work and receive a
human-readable reason rather than a false runnable verdict.

**Status:** Accepted 2026-07-11 (project-owner directive).

---

## ADR-010 — Experimental Anthropic Messages compatibility adapter (supersedes ADR-002 in part)

**Decision:** LocalHub will expose a narrow, experimental Anthropic Messages compatibility
adapter for Claude Code and other Anthropic-format clients. It translates
`POST /v1/messages` to the selected local engine's OpenAI-compatible chat-completions
endpoint and translates the response back to Anthropic's Messages shape. Streaming text
and client-tool use are required for the feature to count as usable. An optional
`POST /v1/messages/count_tokens` may be added when LocalHub can return an honest estimate.

This supersedes ADR-002 only where it called the inference proxy "passthrough-only."
OpenAI routes remain passthrough; the Anthropic route is necessarily a protocol translator.
ADR-002's gateway boundary remains binding: LocalHub does **not** add API-key management,
budgets, rate limits, usage billing, multi-provider routing, provider fallback, or response/
prompt caching. Users who need those features put LiteLLM or another gateway in front.

**Why:** The project owner explicitly made Anthropic-style API support a LocalHub goal on
2026-07-30 so Claude Code can use LocalHub-managed local models. This is still aligned with
the product wedge: the adapter routes to a model on this box through the existing admission,
residency, in-flight accounting, and engine relay. It does not select among commercial
providers or become a general AI gateway.

The compatibility target is the versioned Anthropic Messages wire contract, including:

- `anthropic-version: 2023-06-01`, Anthropic Message/content-block response shapes, and
  Anthropic error envelopes;
- SSE event framing (`message_start`, content-block start/delta/stop, `message_delta`,
  `message_stop`) rather than relaying OpenAI SSE bytes unchanged;
- `text`, `tool_use`, and `tool_result` content blocks, with streamed tool input emitted as
  `input_json_delta` fragments and reassembled into an object at block completion;
- LocalHub bearer authentication plus `x-api-key` as an alias for the same single admin
  token on this adapter, so standard Anthropic clients can authenticate; and
- explicit rejection, with a human-readable reason, of Anthropic features that cannot be
  represented faithfully by the selected local engine.

Primary contract references (revalidated 2026-07-30):
[Messages](https://platform.claude.com/docs/en/api/messages),
[streaming](https://platform.claude.com/docs/en/build-with-claude/streaming),
[errors](https://platform.claude.com/docs/en/api/errors),
[token counting](https://platform.claude.com/docs/en/api/messages/count_tokens), and
[versioning](https://platform.claude.com/docs/en/api/versioning).

**Consequence:** This route cannot use the OpenAI proxy's byte-passthrough implementation;
it needs a small request/response/event translation boundary over the same service core and
counted relay. Compatibility must be tested against a pinned current Claude Code release,
including a real streamed tool round trip. The adapter must label itself experimental and
must never imply that a non-Claude local model is Claude, Anthropic-hosted, Anthropic-
supported, or behaviorally equivalent to Claude.

**Status:** Accepted 2026-07-30 (project-owner directive; documentation contract recorded
before implementation).

---

## ADR-011 — Bridge Codex namespace tools for local Responses engines

**Decision:** On `POST /v1/responses`, LocalHub will flatten Codex's non-standard
`type:"namespace"` tool wrappers into ordinary Responses function tools named
`<namespace>__<tool>`. When the selected local engine emits a matching function call,
LocalHub restores the original `namespace` and short `name` fields before returning it to
Codex. Prior function calls replayed in request input are flattened in the other direction.
All unrelated response events remain passthrough.

**Why:** Codex 0.145 sends MCP/browser tools in a namespace wrapper that current local
Responses implementations such as llama.cpp skip. A request-only flatten is insufficient:
Codex routes MCP calls by the namespace/name pair, so the response must be mapped back as
well. The issue and intended flat-name routing are documented in
[openai/codex#23186](https://github.com/openai/codex/issues/23186) and
[openai/codex#26234](https://github.com/openai/codex/issues/26234).

This is a narrow compatibility exception to ADR-002. It does not discover tools, execute
them, change client approvals, add providers, or make LocalHub an agent gateway. Tool
allow-lists remain a client/agent configuration concern; they are required in practice to
keep large namespace expansions within a local model's context window.

**Verification:** Unit gates cover request flattening, prior-call replay, streaming and
non-streaming response restoration, and preservation of unrelated SSE lines. The live gate
is a real Codex parent launching a local-Qwen child with `fork_turns=1`, followed by
successful Playwright navigate/snapshot/click/back sequences routed through LocalHub.

**Status:** Accepted and implemented 2026-07-30.
