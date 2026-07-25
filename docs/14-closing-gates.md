# 14 — Closing Gates

These are the project-closing gates. A phase is not done until its gate is closed with evidence. Do not replace these with subjective “looks good” reviews.

## Gate status values

| Status | Meaning |
|---|---|
| `PASS` | Evidence attached; acceptance criteria met. |
| `FAIL` | Criteria not met; phase cannot close. |
| `BLOCKED` | External blocker recorded with owner and next action. |
| `WAIVED` | Allowed only for non-safety, non-correctness criteria, with written rationale. |

## Gates that cannot be waived

The following are release blockers and cannot be waived:

1. A model marked `runs` must not OOM in validation.
2. Residency must never kill an active generation by default.
3. A model download must never be recorded without immutable provenance: source, revision/commit when available, file list, hashes, and warnings.
4. LocalHub must not bind admin controls to public internet by default.
5. No phase may introduce cross-engine VRAM preemption or fair-share scheduling.
6. No automatic Safetensors-to-GGUF conversion pipeline may enter core.
7. Every user-facing refusal, blocked request, or compatibility verdict must include a human-readable `reason`.

## Master project close gate

The full MVP closes only when all five product success criteria are met:

1. Fresh machine with Ollama + GGUF folder produces correct inventory and fit verdicts.
2. Admission control refuses or evicts before loading something that does not fit; no OOM from LocalHub load decisions.
3. In-flight generation is not killed by default.
4. Phone over Tailscale shows live GPU, running models, and queue status.
5. Every downloaded/imported model has a reproducible manifest.

## Phase gates

### Phase 0 — Existing-tool validation

**Entry:** One real GPU workstation and at least ten candidate models selected.

**Exit evidence:**

- `research/phase-0-friction-log.md` filled with at least ten model attempts.
- llama-swap, gguf-parser, Open WebUI, Tailscale Serve, Ollama, llama.cpp, and vLLM were tried where applicable.
- One explicit decision: `GO_STANDALONE`, `CONTRIBUTE_UPSTREAM`, or `STOP`.

**Close gate:** continue only if existing tools fail the actual single-box bundle in concrete, documented ways.

### Phase 1 — Read-only observability

**Entry:** stack/language decision recorded in ADR-006.

**Exit evidence:**

- Hardware probe returns CPU/RAM/disk and GPU name, total/free VRAM, driver, compute capability, utilization, power, temperature.
- Engine detection works for Ollama, llama.cpp, and vLLM without mutating state.
- Inventory table contains Ollama models, GGUF folder models, and configured HF/safetensors folders.
- Phone can view live dashboard through Tailscale Serve.
- No engine mutation endpoints are implemented or enabled.

**Close gate:** UI and API match real hardware/engine state; zero writes to engines.

### Phase 2 — Compatibility engine

**Entry:** Phase 1 gate passed; estimator test set selected.

**Exit evidence:**

- GGUF estimates come from gguf-parser-go/library or binary, not a hand-written estimator.
- Safetensors/vLLM estimates include weights, GQA-aware KV cache, and overhead.
- 20-model validation matrix completed: GGUF + safetensors, GQA + MHA, fits + does-not-fit.
- False-positive `runs` rate is zero for the validation set.
- Estimation error is recorded for each model.
- Every verdict includes `reason`, `estimated_vram`, `max_context_that_fits`, and `run_flags`.

**Close gate:** no model predicted as `runs` causes OOM during validation.

### Phase 3 — Supply chain

**Entry:** Phase 2 gate passed for remote preview.

**Exit evidence:**

- Hugging Face downloads resolve branch/tag to exact commit SHA before download.
- Single GGUF URL, Ollama pull, local import, and HF snapshot paths are implemented.
- Every completed download/import writes an immutable manifest.
- Per-file SHA-256 is computed and stored.
- Pickle/trust_remote_code/no-license/gated-repo warnings are shown before load and stored in manifest.
- Reproduce export includes repo/ref/commit/files/hashes and launch flags if run.

**Close gate:** no model artifact enters inventory without provenance or explicit local-import warning.

### Phase 4 — Controlled run/stop

**Entry:** Phase 3 manifest/repro path works; mutation review completed.

**Exit evidence:**

- llama.cpp adapter can spawn `llama-server`, health-check it, stop it, and verify VRAM reclaimed.
- vLLM adapter can spawn/health/stop a model server and handles slow `STARTING` state.
- Ollama adapter can mirror `/api/ps`, warm a model, request unload with `keep_alive: 0`, and verify state by polling.
- State machine transitions are recorded: REQUESTED → STARTING → HEALTHY → DRAINING → STOPPED or FAILED.
- Control-plane restart reconciles old instances: reattach, mark stopped, or reap orphaned processes.

**Close gate:** start/stop works for all three engines and VRAM reclamation is verified from live hardware, not inferred from HTTP 200.

### Phase 5 — Coarse residency

**Entry:** Phase 4 run/stop gate passed.

**Exit evidence:**

- Admission path: warm check → estimate → read free VRAM → evict if safe → start or reject/queue.
- Victim order implemented: expired TTL, idle lowest priority + LRU, largest reclaimable.
- Pinned and active instances are never evicted by default.
- After every eviction, free VRAM is re-read from NVML/driver source.
- Queue entries always include `blocked_reason`.
- Load-storm test requests more models than fit and finishes with no OOM, no active-stream kill, no crash, and sensible blocked reasons.

**Close gate:** load-storm test passes on the reference GPU.

### Phase 6 — Thin OpenAI proxy, optional

**Entry:** Phase 5 gate passed; proxy still confirmed as needed.

**Exit evidence:**

- `/v1/models`, `/v1/chat/completions`, `/v1/completions` route through admission control.
- Streaming passes through unchanged.
- Blocked admission returns fast `503` with `blocked_reason`; it does not hang.
- Proxy does not implement LiteLLM features such as budgets, keys, fallback providers, or multi-provider routing.

**Close gate:** one OpenAI base URL reaches all local engines while preserving streaming and residency safety.

## Release close gate

Before tagging a release:

- All phase gates through the release scope are `PASS`.
- `docs/18-source-validation-matrix.md` is updated within the last 30 days.
- Install path tested from clean machine or clean container.
- Security scan confirms localhost default bind.
- Changelog and known limitations are written.
- Example config, Tailscale instructions, and troubleshooting are current.
