# API Specification

Two surfaces, kept separate:

1. **Control API** — LocalHub's own admin/ops API (models, compatibility, instances,
   queue, jobs). Exposed over REST, SSE, WebSocket, and JSON-RPC 2.0 — all thin transports
   over one service core.
2. **Inference proxy** — a thin OpenAI-compatible passthrough (Phase 6, shipped). Routes by
   model name through admission control to the right engine's OpenAI endpoint.

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
```

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
                                            kv_quant?, allow_cpu_offload?, speculative? }
                                          -> Instance (async start). A queued start returns
                                             202 + { queue_id, position, blocked_reason }.
GET    /api/instances/{id}                -> Instance (+state, +health)
DELETE /api/instances/{id}                -> stop (drains active requests → DRAINING, not killed)
POST   /api/instances/{id}/completions    -> counted completion relay to the engine (streaming
                                             intact). Increments the in-flight counter so
                                             eviction never selects a streaming instance. Also
                                             the Phase 6 proxy's routing substrate.
```

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
proxy **only ever relays `/v1/chat/completions` and `/v1/completions`** — it never forwards
vLLM dev endpoints (`/sleep`, `/wake_up`, `/collective_rpc`, `/reset_prefix_cache`).

```
GET  /v1/models                 -> OpenAI-shaped list. Union of PRESENT + runnable inventory
                                   (runs / runs_degraded; a wont_run model is never listed).
                                   { object:"list", data:[{id, object:"model", created, owned_by:"localhub"}] }
POST /v1/chat/completions       -> resolve model -> admit (warm / load / evict+load) -> proxy+stream
POST /v1/completions            -> same
```

`/v1/embeddings` is not in scope for P6.1 (add later behind the same resolution path if an
embedding model is served).

Rules:

- **Byte-passthrough streaming.** Response chunks are relayed exactly as the engine emits
  them (`data: … \n\n` SSE lines including native `timings` blocks); LocalHub never re-frames
  or re-serializes them. The *request* body may be decode-remarshaled (small, lossless) to
  read the `model` field; the *response* is never touched.
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
