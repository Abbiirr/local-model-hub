# Phase 6 Wiring Handoff — Agent A (P6.1 Proxy + P6.2 Jobs)

Integrator: this file lists the EXACT edits to the three shared files (`server.go`, `ws.go`,
`main.go`) needed to wire Agent A's work. Agent A did NOT touch those files (they are yours). All
of Agent A's own packages compile and their tests pass in isolation (see the final report).

## 1. `internal/api/server.go` — route registrations (in `routes(mux *http.ServeMux)`)

Add these `mux.HandleFunc` lines. Put the `/v1/*` routes and `/api/jobs*` routes alongside the
existing `/api/instances` block. Go 1.22 ServeMux gives literal patterns precedence over the `/`
static catch-all (mounted last), so ordering among these does not matter.

```go
// --- P6.1 thin OpenAI proxy (internal/api/proxy.go). Inference endpoints; auth middleware +
// loopback bind cover them. Never forwards vLLM dev endpoints (docs/security.md). ---
mux.HandleFunc("GET /v1/models", s.handleV1Models)
mux.HandleFunc("POST /v1/chat/completions", s.handleV1ChatCompletions)
mux.HandleFunc("POST /v1/completions", s.handleV1Completions)

// --- P6.2 async inference jobs (internal/api/jobs.go). ---
mux.HandleFunc("POST /api/jobs", s.handleJobSubmit)
mux.HandleFunc("GET /api/jobs", s.handleJobList)
mux.HandleFunc("GET /api/jobs/{id}", s.handleJobGet)
mux.HandleFunc("DELETE /api/jobs/{id}", s.handleJobCancel)
```

Note: the `/api/` catch-all (`s.handleAPINotFound`) already exists and still fires for genuinely
unregistered `/api/*` paths — no change there.

## 2. `internal/api/ws.go` — NO CHANGE NEEDED for jobs events

The WS handler already fans out EVERY bus event generically as
`wsFrame{Type: string(ev.Type), Data: ev.Data}` (ws.go:87). Agent A's `EventJobUpdated`
(`jobs.updated`) with a `core.JobEvent` payload flows through with zero WS-handler changes. A
subscriber sees `{"type":"jobs.updated","data":{id,state,model_id,reason}}` on every job state
change and refetches `GET /api/jobs/{id}` for the terminal `result_json`.

(Agent B may still edit ws.go for JSON-RPC inbound frames — that is orthogonal to Agent A.)

## 3. `cmd/localhub/main.go` — reconcile at boot + start the worker

### (a) ReconcileJobs at boot — next to the existing `ReconcileInstances` call

Immediately AFTER the existing `svc.ReconcileInstances(ctx)` call (and BEFORE starting the job
worker), add:

```go
// P6.2: fail any job left `running` by a crash/restart (design §2.6) BEFORE the worker starts,
// so the worker never sees a stale running row. Queued jobs are left for the worker to re-admit.
if err := svc.ReconcileJobs(ctx); err != nil {
    logger.Warn("reconcile jobs failed", "err", err)
}
```

(Use whatever logger/error-handling idiom the surrounding reconcile code already uses; a reconcile
failure is non-fatal — log and continue.)

### (b) Start the job worker — next to `go svc.StartPeriodicDrain(...)`

Alongside the existing `go svc.StartPeriodicDrain(ctx, ...)` goroutine, gated on the config flag:

```go
// P6.2: the async-job worker drains queued jobs oldest/highest-priority-first, admits each through
// the SAME residency path as the proxy, relays non-streaming, and records the result. Gated on
// cfg.Jobs.Enabled (default true; mirrors residency.enabled).
if cfg.Jobs.Enabled {
    go svc.RunJobWorker(ctx)
}
```

`ctx` is the same daemon-lifetime context used for `StartPeriodicDrain`; the worker returns when it
is cancelled.

### (c) No new `core.Deps` fields

Jobs use the existing `DB` + `Residency` + `InstanceStore` + `InFlight` + `Events` deps already
wired for Phase 5. The worker's wake-signal + per-job cancel map are lazily created inside the
Service (`Service.jobState`, guarded by `jobStateOnce`). **Do not add Deps fields for jobs.**

## 4. Config

`internal/config/config.go` gained a `Jobs JobsConfig { Enabled bool }` block, defaulting to
`Enabled: true` in `Default()`. A YAML file that omits `jobs:` inherits the default true; a file
with `jobs.enabled: false` turns the worker off. No validation rule was added (a bool needs none).
Optionally document it in `examples/config.example.yaml` / `configs/config.example.yaml`:

```yaml
jobs:
  enabled: true   # P6.2 async inference jobs worker (default true)
```

## 5. Migration

`internal/db/migrations/0007_jobs.sql` is picked up automatically by the existing
`//go:embed migrations/*.sql` + `DB.Migrate()` — no wiring. It is additive (new `jobs` table only).

## 6. Deviations from the design doc (for your awareness)

- **`0007_jobs.sql` has an extra `endpoint TEXT NOT NULL DEFAULT 'chat'` column** beyond design
  §2.1's DDL. It records whether the job relays to `/v1/chat/completions` (`chat`) or
  `/v1/completions` (`completions`) so a completions-style async job hits the right upstream path.
  This was already present in the pre-existing migration fragment and is a strict superset of the
  design DDL — additive, no conflict.
- Everything else follows the design contract (§0–§6) exactly.

## 7. Post-wiring verification (integrator)

After the three edits: `go build ./... && go vet ./... && go test ./...` should be green. Then the
live checks (real llama.cpp small model within PLAN.md safeguards) for #p61 chat/completions routing
and #p62 end-to-end job succeed/cancel/restart are the integrator's phase (Agent A used fakes +
httptest only, no GPU/model loads).
