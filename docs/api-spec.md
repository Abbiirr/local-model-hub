# API Specification

Two surfaces, kept separate:

1. **Control API** — LocalHub's own admin/ops API (models, compatibility, instances,
   metrics). REST + SSE.
2. **Inference proxy** — a thin OpenAI-compatible passthrough (Phase 6 only). Do not
   build this until residency works.

All endpoints bind to `127.0.0.1` by default; remote access is via Tailscale (see
[security.md](security.md)). Auth: see the security doc — MVP can trust localhost +
Tailscale identity, with a token option.

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
GET  /api/models/{id}           -> ModelDetail (+manifest)
POST /api/models/import         { source, path|repo|url, ref? } -> starts import job
DELETE /api/models/{id}         -> remove from inventory (opt: delete files)
```

### Compatibility

```
GET  /api/models/{id}/compat?ctx=8192&batch=4   -> Verdict (see compatibility-engine.md)
POST /api/compat/preview        { repo|url, file?, ctx? }  -> Verdict, WITHOUT downloading
                                                             # remote metadata read
```

`POST /api/compat/preview` is the money endpoint: verdict before download.

### Instances (run/stop)

```
GET  /api/instances             -> [ Instance ]               # running + recent
POST /api/instances             { model_id, engine?, ctx?, priority?, pin? } -> Instance (async start)
GET  /api/instances/{id}        -> Instance (+state, +health)
DELETE /api/instances/{id}      -> stop (drains active requests first unless ?force=true)
PATCH /api/instances/{id}       { pin?, priority?, ttl_seconds? }
GET  /api/instances/{id}/logs   -> SSE log tail
```

### Queue

```
GET  /api/queue                 -> [ QueueEntry {status, blocked_reason, vram_est} ]
DELETE /api/queue/{id}          -> cancel a waiting entry
```

### Metrics

```
GET  /api/metrics               -> current snapshot (gpu + per-instance)
GET  /api/metrics/live          -> SSE stream
GET  /metrics                   -> Prometheus exposition (text/plain) for Grafana users
```

Offer **both** an SSE stream (for the built-in UI) and a Prometheus `/metrics` endpoint
(so people who already run Grafana get it free). Cheap to do, widens adoption.

## Realtime: prefer SSE over WebSocket for MVP

The UI needs server→client streams (metrics, logs, instance state). That's one-directional
→ **SSE** is simpler than WebSocket, works through Tailscale Serve/proxies cleanly, and
needs no special server. Use WebSocket only if you later need bidirectional low-latency
control. Don't over-build here.

## Inference proxy (Phase 6 — thin, optional)

Only after residency works. This is a passthrough that routes by model to the right
engine's OpenAI endpoint, going through admission control on the way.

```
GET  /v1/models                 -> OpenAI-shaped list (union across engines)
POST /v1/chat/completions       -> resolve model -> admit (warm/evict/start) -> proxy+stream
POST /v1/completions            -> same
POST /v1/embeddings             -> if an embedding model is served
```

Rules:

- Streaming must pass through untouched (SSE chunks relayed as-is).
- On admission block, return `503` with a JSON body carrying `blocked_reason` — do not
  hang forever.
- **Do not** reimplement LiteLLM. If a user wants multi-provider routing, rate limits,
  budgets, fallbacks, key management — point them at LiteLLM in front of LocalHub. This
  proxy exists only so a single OpenAI base URL can reach any local model with residency
  handled. Keep it thin. (See [ADR-002](decisions.md#adr-002).)

## Error contract

Every error returns JSON: `{ "error": { "code", "message", "reason"? } }`. `reason` is
the human-readable "why" (mirrors the verdict/blocked-reason philosophy). No opaque 500s.
