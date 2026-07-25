# Roadmap

Phased so that **each phase ships standalone value** and the risky pieces come after the
useful-but-safe pieces. Do not skip Phase 0.

Each phase lists a **Definition of Done** — objective, testable. Don't advance until DoD
is met.

---

## Phase 0 — Validate with existing tools (write ~no code)

Before building, run the target workflow with off-the-shelf tools and record where it
hurts. This is what tells you what LocalHub must actually improve.

- Stand up: Ollama + a folder of GGUFs; llama-swap in front of llama.cpp/vLLM; Open WebUI
  for chat; Tailscale Serve for phone access; `gguf-parser` CLI for estimates.
- Try to answer, by hand, for 10 real models: "does it fit, on what engine, at what ctx?"
- Note every manual/painful/impossible step. Those become LocalHub's feature list.

**DoD:** a written list of concrete friction points, AND a go/no-go decision. If
llama-swap + gguf-parser + Open WebUI already cover your needs, the honest outcome may be
"contribute to those instead." Record the decision either way.

---

## Phase 1 — Read-only observability

Pure read. No mutation, no downloads, no queue. Immediately useful and low-risk.

- Hardware detection (NVIDIA/NVML first): GPU name, VRAM total/free, driver, CUDA, CC,
  temp, power, util; CPU/RAM/disk.
- Engine detection: Ollama (`/api/version`, `/api/tags`, `/api/ps`), configured
  llama.cpp, configured vLLM.
- Normalized model inventory table across sources.
- Live GPU dashboard, **reachable from phone over Tailscale.**

**DoD:** on a real box, correct inventory + correct live GPU/engine status shown on a
phone. Zero writes to any engine.

---

## Phase 2 — Compatibility engine

The differentiator. Still read-only w.r.t. engines.

- Integrate `gguf-parser-go` (library in Go / binary+JSON in Python) for GGUF estimates.
- Implement safetensors/vLLM estimation (weights + GQA-aware KV + overhead) from
  `config.json` + safetensors index (see [compatibility-engine.md](compatibility-engine.md)).
- Hardware capability gates (vLLM CC, etc.); engine recommendation; realistic ctx.
- `POST /api/compat/preview` — verdict **without** downloading (remote metadata read).
- Verdict UI with the mandatory human-readable `reason`.

**DoD:** for 20 diverse models (GGUF + safetensors, GQA + MHA, fits + doesn't-fit),
verdicts match reality: things it says `runs` actually run; things it says `wont_run`
actually OOM. Measure and record the estimator error.

---

## Phase 3 — Supply chain (download + manifest)

- HF download with revision→commit pinning; single-GGUF URL; `ollama pull`; local import.
- Per-file sha256; immutable manifest; license + security-warning scan.
- Show verdict + resolved commit SHA **before** committing a large download.

**DoD:** every download produces a valid, immutable manifest; a "reproduce" export names
exact repo+commit+files+hashes; pickle/`trust_remote_code` models get warnings.

---

## Phase 4 — Controlled run/stop

First engine mutation. Owned processes only at first.

- llama.cpp adapter: spawn/health/stop `llama-server`; port pool; flags recorded.
- vLLM adapter: spawn/health/stop; `STARTING` UX for slow cold start.
- Ollama adapter: warm via API; unload via `keep_alive: 0`; mirror `/api/ps`.
- Instance state machine + reconciliation on control-plane restart.
- Verify `stop` frees VRAM via NVML delta.

**DoD:** start/stop each engine's model from the UI; VRAM measurably reclaimed on stop;
restart reconciles without orphaned processes.

---

## Phase 5 — Coarse residency

Wire admission control + eviction (or drive llama-swap — decide per
[ADR-003](decisions.md#adr-003)).

- Admission: warm? else estimate → maybe evict → start. Re-measure NVML after each evict.
- Eviction order (expired TTL → idle+low-priority+LRU → largest); skip pinned + active.
- Never kill active streams by default. Queue with populated `blocked_reason`.

**DoD:** the load-storm test — request more models than fit on the GPU; result is no OOM,
no killed active streams, sensible evictions, clear blocked reasons, no crash.

---

## Phase 6 — Thin OpenAI proxy (optional)

Only now, and only thin.

- `/v1/models`, `/v1/chat/completions`, `/v1/completions` routed by model → engine,
  through admission control; streaming relayed untouched; `503`+reason on block.
- Explicitly *not* LiteLLM. Document "put LiteLLM in front for gateway features."

**DoD:** a single OpenAI base URL reaches any local model; residency handled
transparently; streaming intact; blocks return fast with a reason.

---

## Explicitly deferred / never

- Multi-node (only if real demand; keep the single-box data model clean until then).
- ML-based estimation (only if rule error proves large).
- TPS/throughput prediction (label clearly as rough if added).
- Auto Safetensors→GGUF conversion in core — **never** (ADR-004).
- Chat UI, gateway, k8s, training — **never** (product-brief non-goals).

## Sequencing rationale

Value ships at Phase 1 (observability) and compounds at Phase 2 (the thing nothing else
does well for single-box). The risky mutation/residency work (4–5) comes *after* you've
already delivered something people want, so early users de-risk the hard parts for you.
