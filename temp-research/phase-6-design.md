# Phase 6 Design Spec — Thin OpenAI Proxy · Async Jobs · JSON-RPC 2.0

Implementation spec for the P6.1 / P6.2 / P6.3 implementer agents. **Code from this; do not
re-derive decisions.** Every decision below is made and justified so implementers only write
code + tests. Citations point at the exact source of record.

Authoritative inputs read to produce this: PLAN.md (decision 7, conflict-resolutions 1/2/10,
safeguards), temp-research/spec-digest.md (§2 DDL, §3 API), docs/api-spec.md (thin-proxy rules,
error contract), research/lmwrapper-reuse-notes.md (event vocabulary, restart + client-disconnect
lessons), research/phase-5-load-storm.md (in-flight-counting gap), and the current code surface:
`internal/api/server.go`, `internal/api/instances.go` (the completion relay = the proxy
substrate), `internal/api/ws.go`, `internal/api/errors.go`, `internal/core/{core,events,proxy,
instances,residency,inventory}.go`, `internal/residency/{admission,support,eviction}.go`,
`internal/instances/{instance,state,store}.go`, `internal/db/migrations/000{4,5,6}_*.sql`,
`cmd/localhub/main.go`.

**Engine re-verification (CLAUDE.md rule, done 2026-07-06):** Ollama exposes an OpenAI-compat
surface at base `http://127.0.0.1:11434/v1` — `/v1/chat/completions`, `/v1/completions`,
`/v1/models`, `/v1/embeddings` (source: https://docs.ollama.com/api/openai-compatibility,
fetched 2026-07-06). `llama-server` and `vLLM` are natively OpenAI-shaped at
`http://127.0.0.1:PORT/v1`. So the proxy relays to `{instance.BaseURL}/v1/...` uniformly across
all three engines; there is no per-engine request/response translation.

---

## 0. What already exists (build on it, don't re-invent)

The Phase 5 completion relay in `internal/api/instances.go:handleInstanceCompletion` +
`internal/core/proxy.go:BeginInstanceRequest` **is** the proxy substrate. It already does:

- `BeginInstanceRequest(ctx, instanceID) -> (*InstanceRequestHandle, error)` — verifies HEALTHY,
  increments `residency.InFlightTracker` (never-kill-active substrate), touches `last_used_at`,
  returns `{TargetBaseURL, InstanceID, Done()}`.
- Byte passthrough: `resp.Body.Read` → `w.Write` → `flusher.Flush()` in a loop over a 32 KiB
  buffer. No SSE re-parsing. Status + Content-Type copied through.
- Client disconnect cancels upstream via `r.Context()` on the upstream request (`upReq`).
- `defer handle.Done()` releases the in-flight count on every exit path.

**P6.1 does NOT re-implement this loop.** It (a) adds a model-name→instance *resolution* step in
front of it, (b) runs *admission* when no warm instance exists, and (c) exposes it under the
OpenAI URL shape. Extract the relay core into a reusable helper (see §1.6) so P6.1 and the
existing per-instance relay share one byte-passthrough implementation.

Error envelope is uniform (`internal/api/errors.go`): `writeError(w, status, code, message,
reason)` → `{"error":{code,message,reason}}`. **The OpenAI proxy is the one exception** — see
§1.7 (OpenAI clients expect OpenAI's own error shape). WS frames are `{"type","data","ts?"}`
(`internal/api/ws.go:wsFrame`). The event bus (`internal/core/events.go:EventBus`) is
best-effort fan-out with typed `EventType` constants.

---

# 1. P6.1 — Thin OpenAI Proxy

**Package:** new `internal/proxy` (relay helper + resolution logic that is transport-shaped) with
the core method on `internal/core` (business logic per the one-core rule). Routes registered in
`internal/api/server.go`. Owner: **Agent A** (see §4).

## 1.1 Routes (register in `server.go:routes`)

```
GET  /v1/models              -> handleV1Models
POST /v1/chat/completions    -> handleV1ChatCompletions
POST /v1/completions         -> handleV1Completions
```

Do **not** add `/v1/embeddings` in P6.1 (api-spec lists it "if an embedding model is served";
TESTING.md#p61 does not require it; keep scope minimal — add later behind the same resolution
path if needed). Mount these as literal patterns; Go 1.22 ServeMux gives them precedence over the
`/` static catch-all already mounted last.

**Auth/bind:** same middleware as the admin API (loopback bind + optional bearer token). These
are inference endpoints, not the dangerous admin dev endpoints — never forward vLLM
`/sleep`,`/wake_up`,`/collective_rpc`,`/reset_prefix_cache` (security.md; the proxy only ever
relays `/v1/chat/completions`, `/v1/completions`).

## 1.2 `GET /v1/models` — OpenAI-shaped inventory list

Return the union of **runnable** inventory models in OpenAI list shape:

```json
{ "object": "list",
  "data": [ { "id": "<model_id>", "object": "model", "created": <unix>, "owned_by": "localhub" } ] }
```

**Which models appear:** call `svc.Models(ctx)` (`internal/core/inventory.go`, returns
`[]ModelSummary`). Include a row iff `Presence == "present"` AND `CompatStatus` is empty or one of
`runs`/`runs_degraded` (never list a `wont_run`). `id` = `ModelSummary.ID` (the stable slug the
client passes back in the completion `model` field). `created` = derive from `UpdatedAt` if
parseable, else `0`. This is a read-only projection — add a core method
`svc.OpenAIModels(ctx) ([]OpenAIModel, error)` so the handler stays thin.

Rationale for filtering on compat: the proxy is a single OpenAI base URL that "can reach any
local model with residency handled" (ADR-002) — advertising a model that can never load would
produce a guaranteed 503 later; filter it out up front but keep `runs_degraded` (it loads at
reduced ctx).

## 1.3 Model resolution rule (`model` field → instance)

`resolveTarget(ctx, modelName, priorityClass) -> (*InstanceRequestHandle, *ProxyBlock, error)`:

1. **Warm hit → route.** List active instances (`InstanceStore.List(ctx, true)`); if a `HEALTHY`
   instance serves `modelName`, `BeginInstanceRequest(ctx, inst.ID)` and relay. (This mirrors
   `residency.warmInstance`; reuse admission's warm check by going through Admit, see step 2 —
   Admit returns `DecisionRouteLoaded` with the warm instance, avoiding a duplicate check.)
2. **Not warm → admission.** Call the existing start path: `svc.StartInstance(ctx,
   StartInstanceRequest{ModelID: modelName, Engine: "", Priority: <mapped>, Ctx: 0})`. This runs
   the full P5 flow (compat verdict gate → `Admit` → route_loaded / load_new / evict_then_load /
   queue / reject) and returns a `HEALTHY` `InstanceView` or a `*QueuedError` or a refusal error.
   - Engine defaulting: `Engine: ""` lets `StartInstance` pick `llamacpp` (its default). A model
     that is an Ollama-registry ref resolves to the Ollama engine inside the descriptor/compat
     path already; do not hard-code engine in the proxy.
   - `Ctx: 0` → `StartInstance` uses `Cfg.Compatibility.DefaultContext`. The OpenAI request body
     has no context-length field; the model's default context is correct.
3. On a `HEALTHY` result → `BeginInstanceRequest(ctx, inst.ID)` and relay (§1.6).
4. On `*QueuedError` or a `REFUSED:`/`wont_run` error → **fast 503** (§1.4). Do not wait.

**Model-not-found:** if `modelName` is not in inventory, `StartInstance` returns a resolve error;
map to **404** with OpenAI error shape (`code:"model_not_found"`).

## 1.4 Admission-block semantics: **blocks return fast** (503, no wait)

**Decision: fast 503 + `blocked_reason`, NO wait-briefly.** Justification:

- api-spec.md is explicit: "On admission block, return `503` with a JSON body carrying
  `blocked_reason` — **do not hang forever**."
- TESTING.md#p61 gate wording: "blocked admission → fast 503 + blocked_reason (**measured
  <2s**)". A wait-briefly loop cannot guarantee <2s under a real storm (P5 load-storm showed
  health-waits of 15–85s per spawn). Returning fast is the only way to meet the measured gate.
- The async-jobs API (P6.2) is the designated path for "submit and wait for VRAM" — a caller who
  wants queueing semantics uses `POST /api/jobs`, not the synchronous OpenAI proxy. Keeping the
  proxy non-blocking preserves the clean split (sync proxy = must-be-warm-or-fast-fail; async
  jobs = durable queue).

The one bounded wait that *is* allowed: when admission returns `load_new`/`evict_then_load`,
`StartInstance` already health-waits the spawn to `HEALTHY` before returning (that is a cold-start
load, not a queue block — the request is being served, just starting). That wait is inherent to a
cold start and is not the "block" the gate measures. The <2s measurement is specifically for the
**queue/reject** path, which returns without spawning.

**503 body** (OpenAI error shape so clients parse it, with our reason preserved):
```json
{ "error": { "message": "<blocked_reason — the numeric residency reason>",
             "type": "insufficient_vram", "code": "admission_blocked",
             "param": null } }
```
Set HTTP 503. Source the `message` from `QueuedError.Reason()` (queued) or the refusal error
string (reject / wont_run) — both already carry numeric reasons (P5 guarantees the mandatory
reason). A queued proxy request should **not** leave a durable queue row occupying a slot: pass a
flag so this admission path does not enqueue, OR immediately cancel the queue entry it created.
**Decision: cancel-on-block.** After `StartInstance` returns `*QueuedError`, call
`svc.CancelQueueEntry(ctx, qErr.QueueID)` before returning the 503 (the sync proxy caller is gone;
leaving the row would let a phantom entry auto-start a model nobody is waiting for). Document this
in the handler.

## 1.5 Streaming byte-passthrough (Go, no SSE re-parse)

Reuse the existing loop shape from `handleInstanceCompletion`:

- Build upstream `POST {handle.TargetBaseURL}/v1/chat/completions` (or `/v1/completions`) with
  `http.NewRequestWithContext(r.Context(), ...)` and the **original request body streamed
  through** (`r.Body` as the upstream body — do not buffer/parse it; the client's `stream:true`,
  `model`, messages all pass verbatim). Exception: you must read `model` to resolve the target.
  Read it **without consuming the stream** by using `io.TeeReader` into a small buffered head, or
  decode into a `struct{ Model string; Stream bool }` with `json.Decoder` and then re-marshal +
  send. **Decision: decode-then-remarshal.** OpenAI bodies are small (KB); decode into a
  `map[string]any` (or a typed struct with the fields you need + `json.RawMessage` passthrough for
  the rest), read `model`/`stream`, re-marshal the *same map* as the upstream body. This is
  simpler and safer than TeeReader head-parsing and preserves every field the client sent
  (byte-identity of the *response* is what the gate tests, not the request).
- `client := &http.Client{Timeout: 0}` (streaming; no overall timeout).
- Copy `resp.StatusCode` + `Content-Type`, then the read→write→flush loop. **Preserve chunks
  exactly** — write exactly the bytes read from upstream; never re-frame SSE, never re-serialize
  JSON chunks. `io.Copy(flushWriter{w}, resp.Body)` where `flushWriter` flushes after each Write
  is the idiomatic form; the explicit buffered loop already in `instances.go` is equivalent and
  proven — reuse it.

## 1.6 Client-disconnect vs upstream: **cancel upstream on client disconnect**

**Decision: cancel the upstream generation when the client disconnects (via `r.Context()` on the
upstream request), matching the EXISTING relay behavior in `instances.go`.** This is a deliberate
*departure* from the lmwrapper llama.cpp reality and must be documented:

- The lmwrapper lesson (research/lmwrapper-reuse-notes.md): "Client disconnect does NOT cancel
  upstream llama.cpp generation — it runs to completion." That is a statement about
  **llama-server's** behavior when it loses its HTTP client, not about our proxy.
- What our proxy controls is the **proxy→engine** HTTP connection. Cancelling `upReq`'s context
  closes that connection. For `llama-server` this *does* abort generation server-side on current
  builds (the server observes the closed connection and stops the slot); for engines that run to
  completion regardless, the worst case is the engine finishes a few tokens into the void — no
  correctness harm, VRAM already accounted.
- **Why cancel rather than detach-and-complete:** (a) it frees the engine slot promptly for the
  next request (single-box, one slot per llama-server), (b) it releases our in-flight count
  (`handle.Done()` via `defer`) so eviction's never-kill-active gate reflects reality — a client
  that hung up is no longer "active", (c) it matches the already-shipped `instances.go` relay so
  there is one behavior, not two.
- **The instance is NOT stopped/evicted on disconnect.** Cancelling the request context aborts the
  *stream*, not the *process*. Eviction is a separate concern owned by residency/DrainQueue. This
  is the "stream aborted server-side, instance not killed" contract (also P6.2 cancel semantics).

**In-flight counting closes the Phase-5 gap for external clients** (research/phase-5-load-storm.md
"Phase 6 notes"): because every proxied request goes through `BeginInstanceRequest`
(Increment) → `defer Done()` (Decrement), a non-pinned instance serving a proxied stream now has
`ActiveCount > 0` and eviction will not select it. Before P6.1, a stream sent directly to an
instance `base_url` was invisible to the tracker. **Requirement:** the proxy MUST relay through
`BeginInstanceRequest` for every request (never build a raw upstream call that bypasses it),
including the very first request that just triggered a cold start.

## 1.7 Error shape (OpenAI dialect, the one envelope exception)

OpenAI SDKs parse `{"error":{message,type,code,param}}`. Our internal envelope is
`{"error":{code,message,reason}}`. **Decision:** on the `/v1/*` routes, emit the **OpenAI shape**
so off-the-shelf clients work, but put our human-readable `reason` in `message` (never lose the
reason — the mandatory-reason gate). Add a small `writeOpenAIError(w, status, oaiType, code,
message)` in `internal/proxy`. Map:

| Situation | HTTP | type | code |
|---|---|---|---|
| model not in inventory | 404 | invalid_request_error | model_not_found |
| verdict wont_run / reject | 503 | insufficient_vram | admission_blocked |
| admission queued (sync proxy) | 503 | insufficient_vram | admission_blocked |
| upstream engine unreachable | 502 | api_error | upstream_unreachable |
| bad JSON body / missing model | 400 | invalid_request_error | invalid_body |

## 1.8 Explicit NOT-LiteLLM list (grep-auditable per gate #p61)

The proxy MUST NOT contain any of: API-key management / virtual keys, per-key or per-model
**budgets** / spend tracking, **rate limiting**, provider **fallback** / retry-across-providers,
multi-provider routing (OpenAI/Anthropic/etc.), load-balancing across duplicate deployments,
cost accounting, request/response **caching**, prompt logging by default. The gate greps for
`budget|apikey|api_key|fallback|providers|rate.?limit|litellm` under `internal/proxy` and expects
zero hits (except in comments explicitly disclaiming them). Put a top-of-file comment listing
what this package deliberately does NOT do, pointing at LiteLLM (ADR-002).

## 1.9 P6.1 acceptance shape

- `GET /v1/models` lists present + runnable inventory (llama.cpp + Ollama models both appear).
- `POST /v1/chat/completions` with a warm model streams tokens; with a cold model triggers
  admission then streams; with an un-fittable model returns 503+reason in <2s.
- Streaming byte-identity: see §5 gate technique.

---

# 2. P6.2 — Async Inference Jobs

**Package:** migration `0007_jobs.sql` + core `internal/core/jobs.go` + a background job worker
started in `main.go` next to `StartPeriodicDrain`. REST handlers in `internal/api/jobs.go`. WS
job events over the existing `/api/ws` channel. Owner: **Agent A**.

## 2.1 Migration DDL — `internal/db/migrations/0007_jobs.sql`

Additive only (data-model.md). Follow the 0004/0005 comment style. A jobs table is **net-new**
(PLAN.md conflict-resolution 10: "Async jobs are net-net design ... P6.2 designs schema").

```sql
-- 0007_jobs.sql
-- Additive migration (data-model.md: additive only). P6.2 async inference jobs (PLAN.md
-- decision 7b + conflict-resolution 10): submit an inference request, get a job id back
-- immediately, poll or receive the result over WebSocket, cancel it, and survive a daemon
-- restart (research/lmwrapper-reuse-notes.md restart lesson: queued jobs MUST persist — the
-- lmwrapper failure was that they did not). This is a NEW table; nothing existing is touched.
--
-- A job is the durable record of an async inference request routed through the SAME residency
-- admission + P5.0 ordering as everything else. state machine:
--   queued    -> accepted, waiting for admission/VRAM (durable; survives restart)
--   running   -> admitted, relaying to the engine (an instance is HEALTHY and serving it)
--   succeeded -> engine returned a complete result (result_json holds the OpenAI response)
--   failed    -> engine/admission error, or 'daemon restarted mid-inference' on reconcile
--   cancelled -> user cancelled (mid-queue removal or mid-inference server-side abort)
-- result_json is the full non-streaming OpenAI response body on success (nullable otherwise).
-- error/reason carry the human-readable why on failure (mandatory-reason gate).

CREATE TABLE jobs (
  id            TEXT PRIMARY KEY,               -- uuid
  model_id      TEXT NOT NULL,                  -- the requested model slug (resolved like the proxy)
  engine        TEXT,                           -- resolved engine (nullable until admission picks it)
  request_json  TEXT NOT NULL,                  -- the OpenAI request body as submitted (verbatim)
  priority      INTEGER NOT NULL DEFAULT 1,     -- raw priority (0 interactive|1 normal|2 background)
  state         TEXT NOT NULL DEFAULT 'queued', -- queued|running|succeeded|failed|cancelled
  result_json   TEXT,                           -- OpenAI response body on success (nullable)
  error         TEXT,                           -- machine-ish error class on failure (nullable)
  reason        TEXT,                           -- human-readable why (failure/cancel) (nullable)
  queue_id      TEXT,                           -- residency queue row id while queued (nullable)
  instance_id   TEXT,                           -- serving instance while running (nullable)
  created_at    TEXT NOT NULL DEFAULT (datetime('now')),
  started_at    TEXT,                           -- when it went running
  finished_at   TEXT                            -- when it reached a terminal state
);
CREATE INDEX idx_jobs_state_created ON jobs(state, created_at);  -- worker scan: queued oldest-first
```

Notes: `request_json` is the OpenAI body verbatim so a restart re-runs the exact request.
`priority` maps to `residency.ClassFromPriority` (0/1/2). Do **not** store the streamed deltas;
async jobs return the **final** result (`result_json`), streaming is the proxy's job. `queue_id`
links to the residency `queue` row so cancel-mid-queue can remove it.

## 2.2 REST endpoints (register in `server.go`, handlers in `internal/api/jobs.go`)

```
POST   /api/jobs         { model, priority?, ...openai chat/completions body } -> 201 {id, state:"queued"}
GET    /api/jobs/{id}    -> Job (state + result_json/reason as applicable)
GET    /api/jobs         ?state=&model=&limit=  -> [Job] (filterable list)
DELETE /api/jobs/{id}    -> cancel (see §2.5) -> 200 {id, state:"cancelled"} or 409 if terminal
```

`POST /api/jobs` body = an OpenAI chat/completions (or completions) request plus optional
`priority`. It returns **immediately** with the job id (the whole point — no waiting). The core
method:

```go
func (s *Service) SubmitJob(ctx context.Context, req JobSubmit) (JobView, error)   // inserts queued row, wakes worker
func (s *Service) GetJob(ctx context.Context, id string) (JobView, error)
func (s *Service) ListJobs(ctx context.Context, filter JobFilter) ([]JobView, error)
func (s *Service) CancelJob(ctx context.Context, id string) (JobView, error)
```

`JobView` mirrors the row (id, model_id, engine, state, priority, result/reason where present,
timestamps). Never inline result_json into the list view (keep list responses small); include it
only in `GET /api/jobs/{id}`.

## 2.3 Job worker (background goroutine, started in `main.go`)

A single worker loop (single-box, low churn) started next to the drain loop:

```go
// main.go, alongside: go svc.StartPeriodicDrain(ctx, ...)
if cfg.Jobs.Enabled {          // default true; a config gate mirrors residency.enabled
    go svc.RunJobWorker(ctx)    // drains queued jobs oldest-first, admits, relays, records result
}
```

`RunJobWorker`:
1. On a tick (reuse the drain cadence, e.g. 2–5s) **and** on a wake signal from `SubmitJob`
   (buffered chan), scan `jobs` for the highest-priority oldest `queued` job (`ORDER BY priority,
   created_at`; priority 0 first).
2. For that job, run the **same resolution as the proxy**: `StartInstance(ctx,
   StartInstanceRequest{ModelID: job.model_id, Priority: job.priority, ...})`.
   - `HEALTHY` result → transition job `queued→running` (set `instance_id`, `started_at`), then
     relay the request **non-streaming** to `{inst.BaseURL}/v1/chat/completions` (force
     `stream:false` in the upstream body — the async job returns one final result), capture the
     full response body into `result_json`, transition `running→succeeded`. Wrap the relay in
     `BeginInstanceRequest`/`Done` so in-flight counting protects the serving instance (same gate
     closure as the proxy).
   - `*QueuedError` → leave the job `queued`; store `queue_id`. The residency queue + DrainQueue
     will free room later; the worker re-attempts on its next tick. (The job's durability lives in
     the `jobs` row, not only the residency queue.)
   - refusal / wont_run error → transition `queued→failed` with `reason` = the numeric reason.
3. Publish a WS job event on every state change (§2.4).

**Concurrency:** serialize the worker (one job admitted at a time) — matches the single-box safety
posture and the residency lock. A future enhancement can widen this; do not in P6.2.

## 2.4 WS job events (reuse the vocabulary, `jobs.*` namespace)

**Decision: `jobs.*` namespace, aligned with the existing dotted `EventType` naming
(`models.updated`, `queue.update`, `instances.updated`).** Add to `internal/core/events.go`:

```go
EventJobUpdated EventType = "jobs.updated"   // fires on ANY job state change (queued→running→…)
```

Keep it a single invalidation signal like the other events (the client refetches `GET
/api/jobs/{id}`), **plus** carry a minimal `Data` payload so a subscriber can react without a
refetch round-trip:

```go
s.deps.Events.Publish(Event{Type: EventJobUpdated, Data: JobEvent{
    ID: id, State: state, ModelID: modelID, Reason: reason}})   // reason only on failure/cancel
```

The WS handler (`ws.go`) already fans out every bus event as `{"type":"jobs.updated",
"data":{...}}` — no WS handler change needed beyond it already forwarding all `EventType`s. The
result itself is delivered by the client fetching `GET /api/jobs/{id}` on the terminal event (its
`result_json`). This satisfies gate #p62 "delivered over WebSocket subscription" (the terminal
state + fetchable result) while keeping large result bodies off the WS frame.

Map to the lmwrapper vocabulary for provenance (research notes): `request.accepted` = job created
(`jobs.updated` state=queued), `queue.update` = still queued (already emitted by residency),
`response.completed` = `jobs.updated` state=succeeded. We use the `jobs.*` names, not the
lmwrapper literal names, to match this codebase's existing convention — one naming scheme, not
two.

## 2.5 Cancellation (honest per lmwrapper lesson)

`DELETE /api/jobs/{id}` → `CancelJob`:

- **Terminal job** (succeeded/failed/cancelled) → 409, no-op (nothing to cancel).
- **`queued` (mid-queue)** → remove the residency queue row (`CancelQueueEntry(queue_id)` if set)
  and transition job `queued→cancelled` (`reason:"cancelled by user before admission"`). Clean —
  nothing was running.
- **`running` (mid-inference)** → **abort the relay server-side** (cancel the worker's upstream
  request context for this job) and transition `running→cancelled`
  (`reason:"cancelled by user mid-inference; upstream stream aborted, instance not killed"`).
  **Honest documentation (lmwrapper lesson):** aborting our upstream request stops *us* consuming
  the stream and releases the in-flight count; the **engine may still finish** the generation
  internally (llama.cpp runs a decode to completion; mid-decode interruption is effectively
  impossible per research notes). We do NOT kill the instance to force cancellation (never-kill
  gate). So "cancelled" means "LocalHub stopped waiting and will not deliver the result", not
  "the GPU instantly stopped." State this in the `reason` and in the endpoint doc comment.

Implementation for mid-inference abort: the worker stores a per-running-job `context.CancelFunc`
in an in-memory map keyed by job id; `CancelJob` looks it up and calls it, then writes the
terminal row. If the job id has no live cancel func (worker on another tick / process boundary),
fall back to marking `cancelled` durably and let the worker observe the state on completion and
discard the result.

## 2.6 Restart semantics (persist; running→failed, NOT silently re-run)

**Decision: on daemon restart, `queued` jobs re-enter the drain path (re-admitted, run normally);
`running` jobs → `failed` with reason `"daemon restarted mid-inference"`. Running jobs are NOT
auto-re-queued/re-run.** Justification:

- **Persist requirement (hard):** research/lmwrapper-reuse-notes.md — the lmwrapper failure was
  that "queued HTTP requests and in-flight generations don't survive restarts". Our jobs table
  makes queued jobs durable; that half is non-negotiable and satisfied by the `jobs` row +
  worker re-scan on boot.
- **Why running→failed, not re-run:** a `running` job's process was killed by the restart; we
  cannot know how far the generation got or whether it had side effects the caller already saw
  (the caller may have polled a partial). Silently re-running risks a **duplicate** inference the
  caller didn't ask twice for (non-idempotent from the caller's view — think a job that was about
  to succeed). Failing honestly with a clear reason lets the caller decide to resubmit. This
  mirrors the never-pretend-streams-survived rule (research notes: "reconciliation must not
  pretend streams survived").
- **Why queued CAN safely re-run:** a `queued` job never started inference — re-admitting it is
  exactly what would have happened had the daemon not restarted. No duplication risk.

Wire this into the existing startup reconciliation path (`svc.ReconcileInstances` is called at
boot in main; add a sibling `svc.ReconcileJobs(ctx)`):

```go
func (s *Service) ReconcileJobs(ctx context.Context) error {
    // UPDATE jobs SET state='failed', reason='daemon restarted mid-inference',
    //   finished_at=now WHERE state='running';
    // queued jobs are left as-is; RunJobWorker re-scans and re-admits them.
}
```

Call `ReconcileJobs` before starting `RunJobWorker` (so the worker never sees a stale `running`).

## 2.7 Admission integration + priority mapping

Jobs go through `Admit` **via `StartInstance`** exactly like the proxy and the instance API — no
separate admission path (the one-core rule + ADR-001). Priority: `job.priority` (0/1/2) →
`StartInstanceRequest.Priority` → `residency.ClassFromPriority`. Default 1 (normal). Suggested
convenience mapping if a caller omits `priority`: chat/completions → normal(1); a caller can pass
`priority:0` for interactive or `2` for background (lmwrapper derivation, research notes) — but do
NOT auto-derive from endpoint in P6.2 (keep it explicit; document the field).

## 2.8 P6.2 acceptance shape

Submit → id immediately; poll `GET /api/jobs/{id}` until succeeded + `result_json`; WS
`jobs.updated` frames observed for each transition; cancel mid-queue removes it; cancel
mid-inference marks cancelled + aborts relay (instance stays HEALTHY); restart with queued jobs →
jobs still present + runnable, running jobs → failed with the restart reason. Integration test
against a real small llama.cpp model within safeguards.

---

# 3. P6.3 — JSON-RPC 2.0 Surface

**Package:** new `internal/jsonrpc` (framing, dispatch, method registry) + transport wiring in
`internal/api` (`POST /api/rpc` and JSON-RPC frame handling on the existing `/api/ws`). **Thin
layer over the same `core.Service` — zero business logic** (PLAN.md decision 7; gate #p63 code
review confirms this). Owner: **Agent B**.

## 3.1 Transport

- `POST /api/rpc` — single request object OR a batch array (JSON-RPC 2.0 §6). Content-Type
  `application/json`. Always HTTP 200 for a well-formed transport (JSON-RPC errors ride in the
  response body's `error` object; only malformed JSON / non-JSON gets a transport-level 400 with
  a JSON-RPC `-32700 Parse error` object).
- **`/api/ws` gains request/response frames** alongside the existing push events. Frame
  disambiguation: **a frame with a `"jsonrpc":"2.0"` field is a JSON-RPC request** (dispatch it,
  write a JSON-RPC response frame back); a frame without it is ignored/logged (Phase 1 WS is
  push-only from server; the client does not otherwise send frames). The existing server→client
  push frames (`{"type","data"}`) are unaffected — they have no `jsonrpc` field, so a client
  distinguishes a push (`type` present) from an RPC response (`jsonrpc`+`id` present). Document
  this in `ws.go` where `CloseRead` currently just drains: change it to parse inbound text frames,
  and if `jsonrpc=="2.0"`, route to the shared dispatcher and write the result frame.

## 3.2 Method registry — map to actual core methods

Every method is `namespace.verb` → one `core.Service` method (named from the real code). The
dispatcher is a `map[string]func(ctx, params json.RawMessage) (any, *RPCError)`. Methods:

| JSON-RPC method | core.Service method (exists) | params → result |
|---|---|---|
| `hardware.get` | `Hardware(ctx)` | {} → `hardware.Snapshot` |
| `engines.list` | `Engines(ctx)` | {} → `[]EngineStatus` |
| `engines.detect` | `EnginesDetect(ctx)` (POST /api/engines/detect handler's core call) | {} → re-probe result |
| `models.list` | `Models(ctx)` | {} → `[]ModelSummary` |
| `models.rescan` | `RescanModels(ctx)` | {} → `RescanResult` |
| `models.preview` | `CompatPreview(ctx, req)` | {repo\|url,file?,ctx?} → `Verdict` |
| `models.compat` | `ModelCompat(ctx, id, ctx, batch)` | {id,ctx?,batch?} → `Verdict` |
| `models.manifest` | `ModelManifest(ctx, id)` | {id} → manifest |
| `models.reproduce` | `Reproduce(ctx, id)` | {id} → reproduce bundle |
| `instances.list` | `Instances(ctx)` | {} → `[]InstanceView` |
| `instances.get` | `Instance(ctx, id)` | {id} → `InstanceView` |
| `instances.start` | `StartInstance(ctx, req)` | `StartInstanceRequest` → `InstanceView` (or queued error → RPC error w/ data) |
| `instances.stop` | `StopInstanceAsync(ctx, id)` | {id} → `InstanceView` |
| `queue.list` | `Queue(ctx)` | {} → `[]QueueEntryView` |
| `queue.cancel` | `CancelQueueEntry(ctx, id)` | {id} → ok |
| `jobs.submit` | `SubmitJob(ctx, req)` (P6.2) | job body → `JobView` |
| `jobs.get` | `GetJob(ctx, id)` | {id} → `JobView` |
| `jobs.list` | `ListJobs(ctx, filter)` | {state?,model?,limit?} → `[]JobView` |
| `jobs.cancel` | `CancelJob(ctx, id)` | {id} → `JobView` |

**Verify method names against the code before wiring** — the handler names in `server.go`
(`handleEnginesDetect`, `handleModelsRescan`, `handleCompatPreview`, `handleModelCompat`,
`handleManifestGet`, `handleModelManifest`, `handleReproduce`) tell you the core methods to call;
name each RPC method after the core call, not the HTTP handler. Where a core method for a
read-only op isn't yet a named `Service` method (e.g. engines-detect logic currently inline in
the handler), Agent B extracts it into a `Service` method first (a thin, mechanical refactor —
coordinate with Agent A only if it touches `instances.go`; it does not).

## 3.3 What is NOT exposed

Nothing engine-mutating beyond what REST already exposes. Specifically NOT over JSON-RPC:
downloads/imports mutations beyond what a caller could already do (keep `models.*` read+rescan;
do not add a `downloads.start` unless REST parity is explicitly wanted — P6.3 gate says "mirror
REST core", and downloads are a REST concern; **decision: include read-only + start/stop/cancel
that already exist as REST, exclude nothing that REST has, add nothing REST lacks**). Never expose
vLLM dev endpoints, raw process kill, or DB access. The `/v1/*` OpenAI proxy is NOT a JSON-RPC
method (it is a passthrough surface, not a control-plane RPC).

## 3.4 Request/response framing (spec-compliant)

Request object: `{"jsonrpc":"2.0","method":"...","params":{...},"id":<int|string|null?>}`.

- **id echo:** the response `id` MUST equal the request `id` (JSON-RPC 2.0 §5). Echo it verbatim
  (int stays int, string stays string).
- **Notifications (no `id`):** a request without an `id` member is a notification — execute it,
  send **no response** (§4.1). A request with `"id":null` is NOT a notification per spec (null is
  a valid id) but is discouraged; treat `id:null` as a normal request and echo `null`. (Be precise:
  *absent* id = notification = no reply; *present null* id = reply with `"id":null`.)
- **Success:** `{"jsonrpc":"2.0","result":<value>,"id":<echo>}`.
- **Error:** `{"jsonrpc":"2.0","error":{"code":<int>,"message":<str>,"data":{...}},"id":<echo|null>}`.
- **Batch:** array in → array out, one response object per non-notification element, order
  independent (clients match by id); an all-notification batch yields **no** response body (return
  204/empty for HTTP). An empty batch array `[]` → single `-32600 Invalid Request` error object.

## 3.5 Error object mapping (from our envelope)

Map the internal error/reason to JSON-RPC error objects, preserving the human-readable reason in
`data.reason` (the mandatory-reason gate carries through to RPC):

| Condition | JSON-RPC code | message | data |
|---|---|---|---|
| malformed JSON / not parseable | -32700 Parse error | "parse error" | — |
| not an object / missing method / bad batch element | -32600 Invalid Request | "invalid request" | {reason} |
| unknown method | -32601 Method not found | "method not found: X" | {method} |
| bad params (missing id, wrong types) | -32602 Invalid params | "invalid params" | {reason} |
| core method returned an error | -32000 (server error, impl-defined range) | short message | {reason: err.Error(), code: our_code} |
| admission queued (instances.start / jobs.submit) | -32000 | "admission blocked" | {reason: numeric blocked_reason, queue_id, position} |

Use a single `coreErrorToRPC(err) *RPCError` that recognizes `*QueuedError` (and any typed core
errors) and folds the numeric reason into `data.reason`. Codes -32000..-32099 are the reserved
implementation-defined server-error range (JSON-RPC 2.0 §5.1) — use `-32000` for all core-surfaced
errors, disambiguated by `data.code`.

## 3.6 P6.3 acceptance shape

id echo (int + string + null-vs-absent), `-32601`/`-32602`/`-32700`/`-32600` codes, batch
(mixed request+notification), works over both `POST /api/rpc` and `/api/ws`, method coverage
mirrors REST core, no duplicated business logic (every method body is a one-line core call +
marshal). See §5.

---

# 4. File / Territory Plan (parallel implementation, minimal serial tail)

Two implementer agents work concurrently. Ownership is disjoint except for two shared files that
**one integrator** touches last.

## Agent A — Proxy + Jobs (owns)

- `internal/proxy/` — NEW pkg: relay helper (extracted byte-passthrough), `writeOpenAIError`,
  resolution glue. (transport-shaped helpers only; business logic stays in core)
- `internal/core/proxy.go` — EXTEND: add `OpenAIModels`, and the resolve-and-relay core method(s)
  the `/v1/*` handlers call. (Agent A already "owns" this file's domain.)
- `internal/core/jobs.go` — NEW: `SubmitJob`/`GetJob`/`ListJobs`/`CancelJob`/`RunJobWorker`/
  `ReconcileJobs` + `JobView`/`JobSubmit`/`JobFilter`/`JobEvent`.
- `internal/db/migrations/0007_jobs.sql` — NEW.
- `internal/api/jobs.go` — NEW: REST handlers for `/api/jobs*` (thin).
- `internal/api/proxy.go` — NEW: REST handlers for `/v1/*` (thin).
- `internal/core/events.go` — EXTEND: add `EventJobUpdated` const. **(shared-ish; append-only —
  a new const line; low conflict. Agent A adds it since jobs owns the event.)**

## Agent B — JSON-RPC (owns)

- `internal/jsonrpc/` — NEW pkg: request/response/error types, batch handling, dispatcher,
  method registry, `coreErrorToRPC`.
- `internal/api/rpc.go` — NEW: `POST /api/rpc` handler + the `/api/ws` inbound-frame RPC routing
  helper (a function `handleWSFrame(ctx, conn, raw []byte)` that `ws.go` calls).
- Extract any missing read-only core methods (e.g. `EnginesDetect`) into `Service` methods —
  these live in the relevant `internal/core/*.go` (engines.go), NOT in files Agent A owns.

## Shared files — ONE integrator wires last (serial tail, kept tiny)

1. **`internal/api/server.go`** (`routes`) — add route registrations for `/v1/models`,
   `/v1/chat/completions`, `/v1/completions` (Agent A's handlers), `/api/jobs*` (Agent A),
   `/api/rpc` (Agent B). Both agents write their handlers in their own files; the integrator adds
   the `mux.HandleFunc(...)` lines. **To parallelize:** each agent provides the exact route lines
   in a `// WIRING:` comment block at the top of their handler file; the integrator pastes them.
2. **`internal/api/ws.go`** — one edit: replace the pure `conn.CloseRead(ctx)` drain with a read
   loop that, on a text frame, calls Agent B's `handleWSFrame`. Integrator-owned; Agent B supplies
   the helper signature and the 6-line loop as a `// WIRING:` snippet.
3. **`cmd/localhub/main.go`** — wire the two new deps + one goroutine: (a) nothing new needed for
   the proxy (it reuses existing `svc`), (b) `ReconcileJobs(ctx)` call at boot next to
   `ReconcileInstances`, (c) `go svc.RunJobWorker(ctx)` next to `go svc.StartPeriodicDrain(...)`,
   (d) the JSON-RPC dispatcher construction (`jsonrpc.NewDispatcher(svc)`) passed into the
   `Server`. **`core.Deps` gets no new fields for jobs** (jobs use `DB` + existing deps) — this
   avoids a `core.go` Deps-struct conflict entirely.

**Serial tail = one integrator pass over 3 files, each edit mechanical** (paste route lines, paste
ws loop, add 2 main lines + dispatcher). Both agents can finish and test their packages in
isolation using the existing test patterns (fakes for the runners; `httptest` for handlers)
before the integrator runs. Agents A and B do **not** touch each other's files; the only file both
conceptually depend on is `core.Service`, which they only *call* (Agent A adds methods to
jobs.go/proxy.go it owns; Agent B only reads existing method signatures).

**Handoff notes for the integrator (put in a `temp-research/phase-6-wiring.md` when coding):**
route list, the ws.go loop snippet, the main.go additions, and the config flags added
(`jobs.enabled`, default true). Confirm `go build ./... && go test ./...` green after wiring.

---

# 5. Gate Mapping (#p61 / #p62 / #p63 → evidence)

## #p61 (thin OpenAI proxy)

| Criterion | Evidence technique |
|---|---|
| `/v1/models` lists inventory-runnable models | `curl /v1/models` → assert present+runnable ids appear, a `wont_run`/missing model does not; unit test on `OpenAIModels` with fake inventory. |
| chat/completions routes to each engine | integration: warm a llama.cpp model + an Ollama model, `POST /v1/chat/completions` with each `model`, assert a 200 completion; vLLM via stub or the P45 session if feasible. |
| **streaming byte-passthrough (SSE chunks unmodified)** | **Capture-and-diff:** stand up a fake upstream (`httptest.Server`) that emits a fixed SSE byte sequence (several `data: {...}\n\n` chunks + `data: [DONE]\n\n`); point an instance's `BaseURL` at it; `POST /v1/chat/completions?stream=true` through the proxy; capture the exact bytes the client receives; `bytes.Equal(upstreamBytes, clientBytes)` MUST hold. This proves no re-framing/re-serialization. Run once against a real llama.cpp stream too and assert chunk count + `[DONE]` terminator present (as P5 did in `stream_A.sse`). |
| blocked admission → fast 503+blocked_reason (<2s) | with GPU full (or a tight `vram_budget_mib`), request an un-fittable model; assert HTTP 503, body carries the numeric reason, and wall-clock < 2s (test measures `time.Since(start)`). |
| grep audit: no budgets/keys/fallback/multi-provider | `grep -rIE 'budget\|api_?key\|fallback\|providers\|rate.?limit\|litellm' internal/proxy` → only comment disclaimers; code review confirms. |
| in-flight counting closes the P5 gap | test: proxy a stream to a non-pinned instance, assert `InFlight.ActiveCount(id) > 0` during the relay and `== 0` after; assert eviction skips it while active. |

## #p62 (async jobs)

| Criterion | Evidence technique |
|---|---|
| submit → id immediately | `POST /api/jobs` returns 201 + `{id, state:"queued"}` in <100ms (no wait for inference); unit test asserts the row is `queued` and the handler returned before the worker ran. |
| result by poll AND over WS | integration (real small llama.cpp): submit; poll `GET /api/jobs/{id}` until `succeeded` + non-null `result_json`; separately, subscribe `/api/ws`, assert `jobs.updated` frames with state transitions culminating in `succeeded`. |
| cancel mid-queue | fill VRAM so a submitted job stays `queued`; `DELETE /api/jobs/{id}`; assert `cancelled` + the residency queue row removed. |
| cancel mid-inference (stream aborted, instance not killed) | submit a job that starts running (slower model / larger max_tokens); `DELETE` mid-run; assert job `cancelled`, the serving instance still `HEALTHY` (not stopped), in-flight count released. |
| restart with queued jobs → still present + runnable | submit jobs; kill daemon; restart; assert `queued` jobs still `queued` and get run; assert any `running` job is now `failed` reason `"daemon restarted mid-inference"`. |
| all failure states carry reasons | assert every `failed`/`cancelled` row has a non-empty `reason`. |

## #p63 (JSON-RPC 2.0)

| Criterion | Evidence technique |
|---|---|
| id echo | table test: int id, string id, `id:null` (echoed null), absent id (notification → no response). |
| error codes | tests for `-32700` (bad JSON), `-32600` (not an object / empty batch), `-32601` (unknown method), `-32602` (bad params), `-32000` (core error carries `data.reason`). |
| batch | mixed batch (2 requests + 1 notification) → array of 2 responses, matched by id; all-notification batch → empty/204. |
| method coverage mirrors REST core | test invokes each registered method once (with fakes) and asserts it reaches the corresponding `Service` method; a coverage list asserts hardware/engines/models/compat/instances/queue/jobs all present. |
| works over HTTP POST and WS | run the same request over `POST /api/rpc` and over a `/api/ws` frame; assert identical result. |
| no business logic duplicated (thin-transport) | code review + a structural check: every dispatcher entry is a one-liner core call; grep the `internal/jsonrpc` pkg for absence of DB/engine imports (it imports only `core` + encoding). |

---

# 6. Cross-cutting invariants (apply to all three)

- **One core.** No transport re-implements admission, estimation, eviction, or engine calls; they
  call `core.Service` methods (PLAN.md decision 7; the code already enforces this shape).
- **Mandatory reason** survives every hop: proxy 503 `message`, job `reason`, RPC `data.reason`.
- **Never-kill-active**: every proxied/job relay goes through `BeginInstanceRequest`/`Done` so the
  in-flight counter is accurate; cancel aborts the *stream*, never the *instance*.
- **Localhost bind + optional token** unchanged; no new dangerous endpoints; vLLM dev endpoints
  never forwarded.
- **Additive migrations only**; `0007_jobs.sql` touches nothing existing.
- **Byte-identity for streaming**: response chunks relayed exactly, no SSE re-parse (proxy) — the
  request body may be decode-remarshaled (small, lossless) but the response never is.
