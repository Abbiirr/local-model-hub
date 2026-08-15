# API Specification

Two surfaces, kept separate:

1. **Control API** — LocalHub's own admin/ops API (models, compatibility, instances,
   queue, jobs). Exposed over REST, SSE, WebSocket, and JSON-RPC 2.0 — all thin transports
   over one service core.
2. **Inference API** — the shipped thin OpenAI-compatible passthrough plus a shipped,
   experimental Anthropic Messages compatibility adapter for Claude Code. Both route by
   model name through the same admission control and counted engine relay.

All endpoints bind to `127.0.0.1` by default; remote access is via Tailscale (see
[security.md](security.md)).

### Auth (shipped)

Browser dashboard note: `GET /api/auth/dashboard` publicly reports only whether an
admin token is required. The dashboard keeps a submitted token in sessionStorage
and sends it as a Bearer header for REST. `POST /api/auth/dashboard` validates it
and creates a session-only, HttpOnly, SameSite=Strict cookie accepted only by the
read-only live endpoints (`GET /api/ws` and `GET /api/hardware/live`), since native
browser WebSocket/EventSource APIs cannot attach Authorization. That cookie is never
accepted for REST or inference.

Auth is enforced by `authMiddleware` (`internal/api/middleware.go`). When
`server.admin_token` is set, **every `/api/*` and `/v1/*` request must carry
`Authorization: Bearer <admin_token>`** (constant-time compare). Two paths stay open:
`GET /api/health` (liveness probes) and the static UI assets on `/` (useless without API
access; a browser cannot inject the header — headless/API clients are the audience for the
token). When `admin_token` is null (the default), the chain is untouched and the trust model
is the loopback bind + Tailscale identity described in [security.md](security.md).

## Control API

### Hardware

```
GET  /api/hardware              -> { cpu, ram, disk, gpus:[{index,name,vram_total,vram_free,driver,cuda,cc,temp,power,util}] }
GET  /api/hardware/live         -> SSE stream of the same, ~1 Hz
```

### Engines

```
GET  /api/engines               -> [ { name, installed, version, running, base_url } ]
POST /api/engines/detect        -> re-probe now
```

### Models (inventory)

```
GET  /api/models                -> [ ModelSummary ]           # normalized inventory
POST /api/models/rescan         -> re-scan model dirs + engines (idempotent) -> RescanResult
GET  /api/models/{id}/manifest  -> immutable supply-chain manifest for the model
GET  /api/models/{id}/reproduce -> reproduce bundle (repo + commit + files + hashes [+ flags])
PUT  /api/models/{id}/launch-state { "state": string } -> ModelSummary   # four-state residency policy (migration 0015)
PUT  /api/models/{id}/auto-load    { "enabled": bool }  -> ModelSummary   # DEPRECATED alias (migration 0014) — see below
```

#### Four-state residency policy (`launch_state`, migration 0015)

`ModelSummary.launch_state` is a per-model RESIDENCY/EAGERNESS policy — whether the model sits
loaded in VRAM, not a visibility setting. Set it with `PUT /api/models/{id}/launch-state
{"state": "..."}`; a value outside the four below returns 400 listing the valid ones, an unknown
model id returns 404. `available_on_request` is the **default** for every existing and new model —
it reproduces the pre-0015 behavior (cold until requested, normal idle-TTL, normal eviction)
unchanged.

| `launch_state`          | loaded when idle? | evictable under VRAM pressure? | idle-TTL unload? | auto-loads on an incoming request? |
|--------------------------|:---:|:---:|:---:|:---:|
| `always_available`       | YES — kept loaded even with zero requests; PINNED (eviction's never-evict-pinned gate refuses it) | **NO** | NO | yes |
| `available`               | YES — loaded now | yes | NO | yes |
| `available_on_request` *(default)* | NO — cold until a request arrives | yes | **YES** — offloaded when idle, reloaded on next request | yes |
| `unavailable`             | NO | n/a | n/a | **NEVER** — the operator must start it manually via `POST /api/instances` |

Only `unavailable` refuses an auto-load and only `unavailable` excludes the model from
`GET /v1/models` (advertised as unavailable to OpenAI clients) — `always_available`, `available`,
and `available_on_request` all auto-load on request and are all listed identically on
`GET /v1/models`. This endpoint (`GET /api/models`) always lists every model regardless of state,
including `unavailable` ones — the operator must be able to see and reverse it. An **explicit**
`POST /api/instances` for the same model id is **never** refused by `launch_state` — even
`unavailable` means "never automatic", never "never runnable".

Refusal enforcement for `unavailable` happens at three points, each with a reason naming the model
and how to load it manually: the OpenAI proxy resolve (`/v1/*`), the residency queue drain/promotion
loop (a queued `unavailable` model is dropped rather than retried forever), and the async jobs
worker. `StartInstance` itself carries the same refusal as a belt-and-braces net for any other
implicit caller; only an explicit operator start (`POST /api/instances`, which sets `Manual=true`)
bypasses it.

`always_available` and `available` are also exempt from idle-TTL unload; `available_on_request`
(and `unavailable`, for the rare manual start) use the configured `residency.default_ttl_seconds`.
Proactive/eager loading of `always_available`/`available` models at daemon startup is **not**
implemented (deferred — see KNOWN-LIMITATIONS.md): today these states only take effect once an
instance is (re)started through the normal admission path (explicit or auto), at which point the
launch is pinned/TTL-exempted as above; nothing auto-starts a stopped `always_available` model on
its own yet.

`ModelSummary.auto_load_disabled` (bool) is **DEPRECATED** and now **DERIVED**
(`launch_state == "unavailable"`) — kept only so existing readers of the bool keep working. Toggle it
with the deprecated `PUT /api/models/{id}/auto-load {"enabled": false}` (-> `launch_state=unavailable`)
/ `{"enabled": true}` (-> `launch_state=available_on_request`; this can never grant
`always_available`/`available`); an unknown model id returns 404. Prefer
`PUT /api/models/{id}/launch-state` for new integrations.

Downloads/imports are the Phase 3 supply-chain surface (below), not a `/api/models/import`
endpoint.

### Supply chain (Phase 3)

```
POST /api/downloads             { source, repo|url|path, ref? } -> download job (two-step HF, gguf_url, ollama pull, local import)
GET  /api/downloads             -> [ Download ]
GET  /api/downloads/{id}        -> Download status
GET  /api/manifests/{id}        -> immutable manifest by id
```

`download.progress` events stream over the WebSocket channel (see Realtime channels).

### Compatibility

```
GET  /api/models/{id}/compat?ctx=8192&batch=4   -> Verdict (see compatibility-engine.md)
POST /api/compat/preview        { repo|url, file?, ctx? }  -> Verdict, WITHOUT downloading
                                                             # remote metadata read
```

`POST /api/compat/preview` is the money endpoint: verdict before download.

### Instances (run/stop)

Admin actions — they spawn/kill GPU processes; localhost-bound + token-gated by middleware.

```
GET    /api/instances                     -> [ Instance ]     # running + recent
POST   /api/instances                     { model_id, engine?, ctx?, batch?, priority?, pin?,
                                            kv_quant?, kv_type_k?, kv_type_v?, slots?,
                                            speculative?, spec_draft_max?, allow_cpu_offload? }
                                          -> Instance (async start). A queued start returns
                                             202 + { queue_id, position, blocked_reason }.
POST   /api/instances/attach              -> 410 Gone (`attach_disabled`). External-process
                                             adoption is permanently retired; LocalHub must own
                                             admission, spawn, drain, and VRAM reclaim.
GET    /api/instances/{id}                -> Instance (+state, +health)
DELETE /api/instances/{id}                -> stop (drains active requests → DRAINING, not killed)
POST   /api/instances/{id}/completions    -> counted completion relay to the engine (streaming
                                             intact). Increments the in-flight counter so
                                             eviction never selects a streaming instance. Also
                                             the Phase 6 proxy's routing substrate.
```

### Launch profile fields (M2.1) — independent K/V, slots, speculative

`kv_type_k` / `kv_type_v` (string, default unset ⇒ `f16`) are the **independent** llama.cpp `-ctk` /
`-ctv` cache dtypes. Accepted: `f16`, `q8_0`, `q4_0`. Setting only ONE side is a genuine asymmetric
request, not an error — the other side stays `f16`. The validated Qwen3.6-27B MTP profile is
`kv_type_k: "q8_0", kv_type_v: "q4_0"`.

`tq3_0` is **rejected with 400** even though the CUDA fork advertises it as a cache type: the VRAM
estimator cannot price it, and a start whose footprint cannot be computed must not be admitted (no
speculative admission). Both the estimate and the emitted flags follow these fields, so the admitted
footprint and the spawned process can never disagree.

**`kv_quant`** (bool) is the LEGACY shorthand and still works: `true` ⇒ `q4_0` on **both** sides. An
explicit `kv_type_k`/`kv_type_v` **wins** over it. The queue persists only the resolved pair, so a
restart can never surface a disagreement between the bool and the pair.

`slots` (int, default `1`) is `-np`, the parallel server slot count. It is **admission-relevant, not
cosmetic**: each slot multiplies the KV cache, so it changes the VRAM estimate. Capped at 32 — a
larger value is far more likely a typo than an intent, and a rejected value carries the bound.

`speculative` (string, default `""` = off) selects speculative decoding:

| value | meaning |
|---|---|
| `""` | off — today's flags |
| `"ngram-cache"` | draft-free n-gram self-speculation (`--spec-type ngram-cache`); ~zero extra VRAM, so it never changes admission |
| `"draft-mtp"` | the model's **embedded** multi-token-prediction head (`--spec-type draft-mtp`); no second model |

`draft-mtp` requires **both** a model whose GGUF embeds MTP/`nextn` tensors **and** a binary whose
`--spec-type` advertises `draft-mtp` (`has_spec_mtp` on `/api/engines`). If either side is missing the
start is refused with a reason naming *which* side failed and what to do instead. It also **forces
`-np 1`** — a pinned-build acceptance condition, not a general llama.cpp fact — and adds the MTP head's
weight to the VRAM estimate. Any other value (including modes b10106 advertises but LocalHub has not
validated, e.g. `draft-eagle3`) is rejected with the valid set in the reason.

`spec_draft_max` (int, default unset ⇒ engine default) is `--spec-draft-n-max`. Valid 1–16; the
validated profile uses `2`. It is **rejected when `speculative` is off** — the flag would never be
emitted, so accepting it would let a caller believe MTP was on. It is emitted only when the *selected*
binary advertises the flag (probed separately from `draft-mtp`, since a build may expose one and not
the other).

### Saved per-model launch default (M2.3)

```
GET    /api/models/{id}/launch-default   -> ModelLaunchDefault (404 when none saved — the NORMAL state)
PUT    /api/models/{id}/launch-default   { ctx?, kv_type_k?, kv_type_v?, slots?, spec_mode?,
                                           spec_draft_max?, allow_cpu_offload?,
                                           engine_version?, engine_commit?, binary_path?,
                                           artifact_sha256?, validated_note? }
                                         -> ModelLaunchDefault
DELETE /api/models/{id}/launch-default   -> 204 (idempotent)
```

A saved default is **ADVISORY**. `POST /api/instances` does **not** read it: an unspecified start field
keeps meaning "the daemon default", never "whatever was saved". The dashboard's advanced start form
pre-fills from it and the operator confirms. Auto-applying it would change the meaning of an omitted
field — a silent behaviour change of the kind that hid the queue-replay defect — and would need its own
ADR and gate (TODO.md M6.1).

`PUT` validates the profile through the **same** path a live start uses, so a profile that could never
be started cannot be saved as a default (400 with the reason). An unknown `model_id` is 404. The path
id **wins** over any `model_id` in the body.

Defaults are **provenance-bound** (M6.2): `engine_version`, `engine_commit`, `binary_path`,
`artifact_sha256` record what the profile was validated against. On read, the server computes
`stale_reason` when the recorded binary is no longer among the detected ones — a profile that runs on
one build can hard-fail on another (the older fork died on this model's hybrid SSM architecture), so a
saved profile outliving its build is a stale claim, not a recommendation.

**`allow_cpu_offload`** (bool, default `false`) controls GPU/CPU layer placement. The default is
**full GPU — never offload to CPU**: llama.cpp launches with `-ngl all` and Ollama warms with
`num_gpu=999` (all layers on GPU). Set `true` to permit a GPU/CPU split for borderline models:
llama.cpp switches to `-ngl auto` (offload as many layers as fit, remainder on CPU) and Ollama omits
`num_gpu` so the daemon uses its dynamic default (`num_gpu=-1`, auto-split). It relaxes only the
launch flags, **not admission** — the start still passes the same conservative full-GPU VRAM verdict,
so a model whose weights genuinely exceed free VRAM is still refused (correctness bar: no false
`runs` that OOMs). CPU-split therefore helps only cases the whole-model estimate is pessimistic
about, not true VRAM overflow. Note: forcing Ollama to full GPU by default is a deliberate behavior
change — previously LocalHub sent no `num_gpu`, letting the Ollama daemon auto-split. vLLM ignores
this field (its CPU-offload knob is separate).

For a **mixture-of-experts (MoE) GGUF**, `allow_cpu_offload=true` additionally moves the expert
weights of every expert layer to CPU RAM via llama.cpp `-ncmoe N`, and admission sizes against the
reduced GPU footprint (`total − expert bytes`, derived from tensor metadata). If the expert bytes are
not derivable (e.g. a TQ3_* quant gguf-parser-go cannot parse), the start is **declined** with
`"MoE offload sizing not derivable for this model"` rather than admitted on a guessed footprint — no
speculative admission. See KNOWN-LIMITATIONS #19.

**`speculative`** (string, default `""` = off) opts into **draft-free self-speculative decoding** on
the llama.cpp path. Valid values: `""` (off, today's flags) and `"ngram-cache"`, which appends
`--spec-type ngram-cache` — llama.cpp's n-gram self-speculation that needs **no second (draft)
model** and adds **~zero extra VRAM** (a small hash pool). It changes only the launch flags, so it
**does not affect admission** (the VRAM verdict is unchanged); it can speed up structured/repetitive
output. Any other value is rejected with **400** and a reason listing the valid values (e.g. draft-
model spec and the other `ngram-*` variants are future work; the value set is pinned to the local
b8833 `--spec-type` surface). Ollama/vLLM starts ignore this field. Draft-*model* speculative
decoding (a second GGUF + two-model VRAM accounting) is a separate, larger feature — not this knob.

### Native app startup policy

```
GET /api/startup  -> StartupConfig
PUT /api/startup  <- StartupConfig -> normalized StartupConfig
```

The startup document controls the native tray's login delay, LocalHub/Web UI startup,
previous-session restoration, and ordered per-model startup entries. Per-model entries choose
`canonical_qwen` or `localhub_api`, retry policy (`never`, `limited`, `keep_available`), optional
saved-profile use, and the same validated launch fields accepted by `POST /api/instances`.
Saving is atomic and strict: unknown fields, duplicate model IDs, invalid enum values, or unsafe
field combinations return a standard error envelope with a human-readable `reason`. `PUT` changes
durable intent only; the tray applies it on its next health cycle and never bypasses admission.

### Queue

```
GET    /api/queue               -> [ QueueEntry {id, model_id, engine?, priority_class, status,
                                     blocked_reason?, footprint_mib?, position?, age_seconds?} ]
DELETE /api/queue/{id}          -> cancel a WAITING entry
```

Read/cancel only — admission itself happens through `POST /api/instances` (a queued start
returns 202 + position). `blocked_reason` is the numeric MiB residency reason (e.g. "waiting
for VRAM: need N MiB … M MiB short"); shape from `core.QueueEntryView`
(`internal/core/residency.go`).

## Realtime channels

Server→client streaming is shipped over **two** transports, both thin layers over the same
service core:

- **SSE** — `GET /api/hardware/live` streams the hardware snapshot (~1 Hz). SSE remains the
  spec'd server→client stream: simple, proxies through Tailscale Serve cleanly, no special
  server needed.
- **WebSocket** — `GET /api/ws` (shipped P1.5). A server-push channel carrying hardware
  snapshots plus change signals and best-effort events. Event types observed on the bus:
  `hardware.snapshot`, `models.updated`, `engines.updated`, `instances.updated`,
  `queue.update`, `download.progress`, `jobs.updated`, and a heartbeat. The client refetches
  the relevant REST resource on a change signal. The **same `/api/ws` socket also accepts
  inbound JSON-RPC 2.0 request frames** (see JSON-RPC below); a frame carrying
  `"jsonrpc":"2.0"` is dispatched and answered, any other inbound frame is ignored.

> **Note (supersedes earlier "prefer SSE over WebSocket for MVP" advice):** WebSocket is not
> a "later if you need bidirectional" option — it shipped in P1.5, and Phase 6 adds
> JSON-RPC-over-WS and `jobs.updated` job events on the same socket. REST, SSE, WS, and
> JSON-RPC are four thin transports over **one** service core; no business logic is
> duplicated per transport (PLAN.md decision 7).

The bundled dashboard defaults to WebSocket (`auto`: WS preferred, SSE hardware-only
fallback) since 2026-07-07 — models/instances/queue/jobs/downloads invalidations flow
over `/api/ws`; a queued instance start (HTTP 202) is surfaced in the UI with its queue
position and `blocked_reason`.

## Inference proxy (Phase 6 — thin, shipped)

A passthrough that routes by model name to the right engine's OpenAI endpoint, going through
admission control on the way. Same auth middleware + loopback bind as the admin API. The
proxy only relays the allowlisted routes listed below — it never forwards
vLLM dev endpoints (`/sleep`, `/wake_up`, `/collective_rpc`, `/reset_prefix_cache`).

```
GET  /v1/models                 -> OpenAI-shaped list. Union of PRESENT + runnable inventory
                                   (runs / runs_degraded; a wont_run model is never listed;
                                   a model with launch_state=unavailable (migration 0015) is never
                                   listed either, even though GET /api/models still shows it).
                                   { object:"list", data:[{id, object:"model", created, owned_by:"localhub"}] }
POST /v1/chat/completions       -> resolve model -> admit (warm / load / evict+load) -> proxy+stream
POST /v1/completions            -> same
POST /v1/responses              -> same; normalizes Codex role-typed message items for local
                                   Responses implementations and bridges namespaced client tools
POST /v1/embeddings             -> same resolve -> admit -> proxy path; resolves ONLY to a llama.cpp
                                   instance started in embedding mode (llama-server --embedding)
POST /v1/rerank                 -> same path; resolves ONLY to a llama.cpp instance started in
                                   reranking mode (--embedding --pooling rank --reranking)
```

`/v1/embeddings` runs the same resolve → admit → proxy path as the generation routes, with one
extra constraint: it resolves **only** to a llama.cpp instance started in embedding mode. Start one
with `POST /api/instances {"model_id": …, "embedding": true}`, optionally passing
`"pooling"` (`none|mean|cls|last|rank`; omitted means the model's own default). llama.cpp documents
`--embedding` as "restrict to only support embedding use case" — an embedding-only server and a
generation server cannot be the same process — so an embeddings request that resolves to a
generation-mode instance is refused with a reason rather than relayed, and the two modes count as
distinct residents for warm-routing. Note that llama.cpp's OpenAI-compatible `/v1/embeddings`
"requires that the model uses a pooling different than type `none`" (source:
`llama.cpp/tools/server/README.md`); its native `/embedding` route is unaffected. GGUF only, on
llama.cpp only — ADR-004 rules out automatic Safetensors conversion, and vLLM has no native Windows
support on this box.

`/v1/rerank` is the third resolution path. llama.cpp gates its rerank endpoints behind
`--rerank, --reranking` ("enable reranking endpoint on server"), and that flag "requires a reranker
model (such as bge-reranker-v2-m3) and the `--embedding --pooling rank` options" (source:
`llama.cpp/tools/server/README.md`) — so a rerank start emits all three flags, and
`POST /api/instances {"model_id": …, "reranking": true}` implies `embedding` and `pooling: rank`
rather than making the caller repeat them. Launch modes are therefore three-way, not a boolean:
generation, embedding-only, and reranking. The distinction matters because a rerank server pools with
`rank` and returns a relevance score per (query, document) pair rather than a reusable vector, so
`/v1/embeddings` refuses a rerank-mode instance and `/v1/rerank` refuses an embedding-only one —
each with a reason naming both the resident and the requested mode. The request/response bodies are
relayed byte-for-byte (`{model, query, documents, top_n}` → `results[{index, relevance_score}]`);
LocalHub never reshapes them (ADR-002).

Only the `/v1/`-prefixed alias is registered. llama.cpp additionally exposes bare `/rerank`,
`/reranking` and `/v1/reranking`, but the auth middleware protects the `/api/` and `/v1/` prefixes
only — registering a bare `/rerank` would create an unauthenticated route on a token-gated daemon.
Point clients that default to `/rerank` at `/v1/rerank`.

Rules:

- **Byte-passthrough streaming.** Response chunks are relayed exactly as the engine emits
  them (`data: … \n\n` SSE lines including native `timings` blocks); LocalHub never re-frames
  or re-serializes them. The *request* body may be decode-remarshaled (small, lossless) to
  read the `model` field; the *response* is never touched.
- **Responses compatibility normalization is narrowly bidirectional.** `/v1/responses` converts Codex role-typed
  message items to canonical `type:"message"` input, removes opaque encrypted cloud-parent
  reasoning that a local provider cannot consume, and treats a spawned `NEW_TASK` envelope
  as the local child's user turn. It also flattens Codex's non-standard `type:"namespace"`
  tool wrappers into ordinary functions named `<namespace>__<tool>` for local engines, then
  restores `namespace` plus the short tool `name` on matching function-call output so Codex
  can route the call to the original MCP server. Non-tool SSE lines remain byte-identical;
  only mapped function-call events are re-serialized. Clients should scope local agents to
  the smallest useful tool allow-list because expanding hundreds of namespace tools can
  exceed a local model's context window.
- **Admission block → fast `503`, no wait.** When no warm instance exists and the model
  cannot be admitted (queue/reject), return `503` immediately with the numeric
  `blocked_reason`, in OpenAI error shape (below) — do not hang. A caller who wants
  submit-and-wait semantics uses the async jobs API instead. (A cold start that *is* being
  served still waits for `/health`; that is not the block the gate measures.)
  <br>**Resolved (2026-07-07):** an earlier live measurement showed ~10 s on the block path
  (gguf-parser doing ~2.7 s of buffered CPU parsing ×4 calls ahead of admission). Fixed via
  mmap + SkipLargeMetadata on the local-file parse (byte-identical estimates); re-measured
  live at **1.36–1.54 s over 8 trials including first-call** (research/phase-6-live-gates.md,
  Amendment).
- **Never forwards vLLM dev endpoints** (security.md).
- **In-flight counting closes the Phase-5 gap for external clients:** every proxied request
  goes through the counted relay (`BeginInstanceRequest`/`Done`), so a non-pinned instance
  serving a proxied stream is visible to the never-kill-active gate. Client disconnect cancels
  the upstream request (aborts the *stream*, not the *instance*).
- **Do not** reimplement LiteLLM. No API-key management, budgets, rate limits, provider
  fallback, multi-provider routing, or caching. Point users at LiteLLM in front of LocalHub
  for those. This proxy exists only so a single OpenAI base URL can reach any local model with
  residency handled. Keep it thin. (See [ADR-002](decisions.md#adr-002).)

### OpenAI error dialect (the one envelope exception)

The `/v1/*` routes emit OpenAI's own error shape `{"error":{message,type,code,param}}` so
off-the-shelf clients parse it — but the human-readable `reason` is preserved in `message`
(the mandatory-reason gate never breaks). Mapping (`internal/proxy`):

| Situation | HTTP | type | code |
|---|---|---|---|
| model not in inventory | 404 | `invalid_request_error` | `model_not_found` |
| verdict wont_run / reject | 503 | `insufficient_vram` | `admission_blocked` |
| admission queued (sync proxy) | 503 | `insufficient_vram` | `admission_blocked` |
| upstream engine unreachable | 502 | `api_error` | `upstream_unreachable` |
| bad JSON body / missing model | 400 | `invalid_request_error` | `invalid_body` |

## Anthropic Messages compatibility adapter (experimental — shipped)

The Anthropic-format surface is a deliberately narrow protocol adapter over local,
OpenAI-compatible engine endpoints. Its first compatibility target is Claude Code. It is
not an Anthropic service, does not contact Anthropic, and does not make a local model Claude.
Non-Claude models can differ in instruction following, context handling, tool-call
reliability, safety behavior, and output quality; Anthropic does not support them.

```
POST /v1/messages               -> translate Anthropic Message -> admit/relay OpenAI chat
                                   completion -> Anthropic Message (streamed or non-streamed)
POST /v1/messages/count_tokens  -> optional honest input-token estimate; 404/not implemented
                                   until the selected engine exposes a defensible tokenizer
```

`POST /v1/messages` is required for the feature. `count_tokens` is optional and must not
claim exact Anthropic tokenization: when implemented, its result is for the selected local
model/tokenizer and the response must identify it as an estimate in documentation and
telemetry. Returning a fabricated count is worse than leaving the endpoint unimplemented.

### Request contract

- Require `content-type: application/json` and `anthropic-version: 2023-06-01`. A missing or
  unsupported version is `400 invalid_request_error` with a human-readable reason.
- Require `model`, `max_tokens`, and `messages`. Support the top-level `system` field,
  `stream`, `temperature`, `top_p`, `top_k` when the engine supports it, `stop_sequences`,
  `tools`, and `tool_choice`. Unsupported fields are rejected or documented as ignored;
  they are never silently advertised as effective.
- Translate string content and `text` blocks to OpenAI message content. Translate Anthropic
  tool definitions (`name`, `description`, `input_schema`) to OpenAI function tools.
  Translate assistant `tool_use` blocks to assistant tool calls and user `tool_result`
  blocks to tool-result messages while preserving tool IDs.
- The minimum supported content-block set is `text`, `tool_use`, and `tool_result`.
  Images/documents, citations, thinking/redacted-thinking blocks, server-side Anthropic
  tools, and beta-only extensions are outside the initial adapter. Prompt-cache controls
  are accepted as inert compatibility metadata because the local engine does not implement
  Anthropic prompt caching; no cache benefit is advertised.
- Model resolution, cold admission, eviction safety, cancellation, and in-flight counting
  are exactly the same as the OpenAI proxy. There is no second scheduler.

### Response and streaming contract

Non-streaming success returns an Anthropic-shaped Message with `id`, `type:"message"`,
`role:"assistant"`, `content:[...]`, `model`, `stop_reason`, `stop_sequence`, and `usage`.
Text becomes `text` blocks; OpenAI function calls become `tool_use` blocks with stable IDs
and an object-valued `input`. Finish reasons map conservatively to `end_turn`, `max_tokens`,
`stop_sequence`, or `tool_use`; an unmappable reason must not be invented.

When `stream:true`, LocalHub **re-frames** the upstream OpenAI stream as named Anthropic SSE
events. The required order is:

1. `message_start`
2. one or more `content_block_start` -> `content_block_delta` ->
   `content_block_stop` sequences
3. `message_delta` carrying the final stop reason and cumulative usage
4. `message_stop`

Text deltas use `text_delta`. Tool arguments use `input_json_delta` with `partial_json`
fragments, and the completed `tool_use.input` must be valid JSON object data. Optional
`ping` events are allowed. OpenAI `data: [DONE]` is consumed, not forwarded.

Streaming is a release requirement, not a follow-up: Claude Code's long-running and tool
workflows must not be declared compatible from non-streaming text-only proof.

### Anthropic auth and version headers

When `server.admin_token` is set, this adapter accepts either
`Authorization: Bearer <admin_token>` or `x-api-key: <admin_token>` using the same
constant-time comparison. They are aliases for LocalHub's one operator credential, not a
new key-management feature. If both are present they must identify the same configured
token. The credential is consumed by LocalHub and is never forwarded to the local engine.

This lets Claude Code use its normal base-URL and auth-token configuration while preserving
LocalHub's existing loopback/Tailscale trust model. LocalHub does not issue Anthropic keys,
perform billing, or validate credentials with Anthropic.

### Anthropic error dialect

Errors from these two routes use Anthropic's envelope:

```json
{
  "type": "error",
  "error": { "type": "invalid_request_error", "message": "<human-readable reason>" }
}
```

| Situation | HTTP | `error.type` |
|---|---:|---|
| bad JSON, missing field/version, or unsupported content/feature | 400 | `invalid_request_error` |
| missing or invalid LocalHub credential | 401 | `authentication_error` |
| model absent from inventory | 404 | `not_found_error` |
| admission blocked/queued on the synchronous route | 529 | `overloaded_error` |
| upstream local engine failure/unreachable | 502 | `api_error` |
| unexpected LocalHub failure | 500 | `api_error` |

The mandatory human-readable LocalHub `reason` goes in `error.message`; no opaque 500s.
This Anthropic envelope applies only to `/v1/messages` and
`/v1/messages/count_tokens`. OpenAI inference routes keep the OpenAI error dialect.

### Boundary with LiteLLM

The adapter performs only deterministic wire translation and routes to the resolved local
model through LocalHub residency. It does **not** add per-user/API-key management, budgets,
rate limits, usage billing, multi-provider selection, provider fallback, load balancing,
or prompt/response caching. Those remain LiteLLM/gateway responsibilities (ADR-002,
ADR-010).

## Async inference jobs (Phase 6 — shipped)

Submit an OpenAI request, get a job id back **immediately**, then poll or watch the result
over WebSocket. Durable: queued jobs survive a daemon restart (the `jobs` table, migration
`0007_jobs.sql`). Every job runs through the **same** residency admission as the proxy and the
instance API — no separate admission path.

```
POST   /api/jobs      { model, priority?, endpoint?, ...openai chat/completions body }
                      -> 201 { id, state:"queued", priority, endpoint }   (returns in <1 s)
GET    /api/jobs      ?state=&model=&limit=   -> [ Job ]                  (result_json omitted)
GET    /api/jobs/{id} -> Job (state + result_json on success, or reason on failure/cancel)
DELETE /api/jobs/{id} -> cancel -> 200 {state:"cancelled"} or 409 if already terminal
```

**State machine** (`internal/core/jobs.go`): `queued → running → succeeded | failed |
cancelled`.

- `priority`: 0 interactive | 1 normal (default) | 2 background.
- `endpoint`: `chat` (default) → `/v1/chat/completions`, or `completions` →
  `/v1/completions`. The worker forces `stream:false` upstream and stores the one final
  OpenAI response body in `result_json`.
- **Cancellation is honest.** Mid-queue → the residency queue row is removed and the job is
  `cancelled` (`reason: "cancelled by user before admission"`). Mid-inference → the worker's
  upstream relay is aborted server-side and the job is `cancelled` (`reason: "cancelled by user
  mid-inference; upstream stream aborted, instance not killed"`). The instance is **not**
  killed; the engine may still finish the generation internally — "cancelled" means LocalHub
  stopped waiting, not that the GPU instantly stopped.
- **Restart semantics.** On daemon restart, `queued` jobs re-enter the drain path and run
  normally; a `running` job becomes `failed` with `reason: "daemon restarted mid-inference"`
  (never silently re-run — that would risk a duplicate inference).
- Every `failed`/`cancelled` job carries a non-empty `reason` (mandatory-reason gate).

**Job events over WebSocket:** every state change publishes a `jobs.updated` frame on
`/api/ws` carrying `{id, state, model_id, reason?}`. Subscribers fetch `GET /api/jobs/{id}`
on the terminal event to get `result_json` (large bodies stay off the WS frame).

## JSON-RPC 2.0 surface (Phase 6 — shipped)

A thin JSON-RPC 2.0 control surface over the **same** `core.Service` as REST — zero
business logic, every method is a one-line core call (`internal/jsonrpc`).

```
POST /api/rpc     -> a single request object OR a batch array. Always HTTP 200 for a
                     well-formed transport (JSON-RPC errors ride in the body's `error`
                     object); a non-parseable body is 400 + a `-32700` error object.
/api/ws           -> the same dispatcher also answers inbound JSON-RPC request frames
                     (a frame with "jsonrpc":"2.0"); identical results to POST /api/rpc.
```

**Method registry** (verified against `internal/jsonrpc/dispatcher.go`):

| Method | Core call | Params → result |
|---|---|---|
| `hardware.get` | `Hardware` | {} → hardware snapshot |
| `engines.list` | `Engines` | {} → [ EngineStatus ] |
| `engines.detect` | `DetectEngines` | {} → re-probe (+ `engines.updated`) |
| `models.list` | `Models` | {} → [ ModelSummary ] |
| `models.rescan` | `RescanModels` | {} → RescanResult |
| `models.preview` | `CompatPreview` | CompatRequest → Verdict (no download) |
| `models.compat` | `CompatPreview` | {id, ctx?, batch?} → Verdict |
| `models.manifest` | `ManifestForModel` | {id} → manifest |
| `models.reproduce` | `Reproduce` | {id} → reproduce bundle |
| `instances.list` | `Instances` | {} → [ InstanceView ] |
| `instances.get` | `Instance` | {id} → InstanceView |
| `instances.start` | `StartInstance` | StartInstanceRequest → InstanceView (or queued → error w/ data) |
| `instances.stop` | `StopInstanceAsync` | {id} → InstanceView |
| `queue.list` | `Queue` | {} → [ QueueEntryView ] |
| `queue.cancel` | `CancelQueueEntry` | {id} → {ok, id} |
| `jobs.submit` | `SubmitJob` | job body → JobView |
| `jobs.get` | `GetJob` | {id} → JobView |
| `jobs.list` | `ListJobs` | {state?, model?, limit?} → [ JobView ] |
| `jobs.cancel` | `CancelJob` | {id} → JobView |

The `/v1/*` OpenAI proxy is **not** a JSON-RPC method (it is a passthrough surface, not a
control-plane RPC). Nothing engine-mutating beyond what REST already exposes is added.

**Framing / errors** (spec-compliant): the response `id` echoes the request `id` verbatim
(int stays int, string stays string); an *absent* `id` is a notification (execute, no reply);
`"id":null` is a normal request replied with `null`. A batch array yields one response per
non-notification element; an empty `[]` batch → a single `-32600`. Error objects preserve the
human `reason` in `data.reason`:

| Condition | code | notes |
|---|---|---|
| malformed / unparseable JSON | `-32700` | transport-level 400 |
| not an object / bad batch element / empty batch | `-32600` | `data.reason` |
| unknown method | `-32601` | `data.method` |
| bad params (missing id, wrong types) | `-32602` | `data.reason` |
| core method returned an error | `-32000` | `data.reason = err.Error()` |
| admission queued (`instances.start` / `jobs.submit`) | `-32000` | `data.reason` = numeric `blocked_reason` (+ `queue_id`, `position` where available) |

## Error contract

Every error returns JSON: `{ "error": { "code", "message", "reason"? } }`. `reason` is
the human-readable "why" (mirrors the verdict/blocked-reason philosophy). No opaque 500s.
