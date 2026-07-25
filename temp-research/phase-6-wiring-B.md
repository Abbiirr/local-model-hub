# Phase 6 Wiring Handoff — Agent B (P6.3 JSON-RPC 2.0 surface)

Serial-tail wiring for the integrator. Agent B's package (`internal/jsonrpc`) + `internal/api/rpc.go`
compile and pass tests standalone. This doc lists the exact edits to make the JSON-RPC surface live
over both `POST /api/rpc` and `/api/ws`. Every edit is mechanical (add a field, paste a route, paste a
read loop). Design contract: `temp-research/phase-6-design.md` §3, gate #p63.

Architecture: ONE `jsonrpc.Dispatcher`, constructed once, shared by the HTTP handler and the WS frame
router (so a method behaves identically on both transports — gate #p63 "works over HTTP POST and WS").
The dispatcher is a thin layer over `*core.Service`: every method is a one-line core call + marshal
(verified by `TestPackageImportsOnlyCoreAndStdlib` — the package imports only `core` + stdlib).

---

## 1. Construct the dispatcher and store it on `Server` (`internal/api/server.go`)

`internal/api/rpc.go` is written as FREE functions (`rpcHTTPHandler`, `handleWSFrame`) so it compiles
without touching `server.go`/`ws.go`. To make them reachable, add one field to `Server` and set it in
`New`.

**1a. Add the field to the `Server` struct** (top of `server.go`):

```go
import "github.com/localhub/localhub/internal/jsonrpc"   // add to the import block

type Server struct {
	svc    *core.Service
	logger *slog.Logger
	http   *http.Server
	ln     net.Listener
	addr   string
	rpc    *jsonrpc.Dispatcher   // <-- ADD: shared JSON-RPC dispatcher (P6.3)
}
```

**1b. Build it in `New`** (in `func New(...)`, right after `s := &Server{...}`):

```go
	s := &Server{svc: svc, logger: logger, addr: addr}
	s.rpc = jsonrpc.NewDispatcher(svc)   // <-- ADD (see §4 for the jobs-wired variant)
	s.routes(mux)
```

No `main.go` change is needed for the RPC dispatcher — it is derived from `svc`, which `New` already
receives. (`api.New` signature is unchanged: `New(svc, logger, bind, port, adminToken)`.)

---

## 2. Register the HTTP route (`internal/api/server.go` → `func (s *Server) routes`)

Paste this next to the other transport routes (near the `GET /api/ws` line):

```go
	// JSON-RPC 2.0 control surface (P6.3, PLAN.md decision 7d): a thin layer over the same core
	// as REST. Single request object OR batch array; errors ride in the body (200) except a
	// non-parseable body (400 + -32700). Loopback-bound + token-gated by the same middleware.
	mux.HandleFunc("POST /api/rpc", rpcHTTPHandler(s.rpc, s.logger))
```

`rpcHTTPHandler` framing (already implemented + tested in `rpc.go` / `rpc_test.go`):
- well-formed single/batch → 200 with JSON-RPC body
- notification / all-notification batch → 204 empty
- non-parseable JSON → 400 with a `-32700` Parse error object

---

## 3. Route inbound WS frames to the dispatcher (`internal/api/ws.go`)

Currently `handleWS` does `ctx = conn.CloseRead(ctx)` (drains + ignores all client frames). Replace that
single line with a read loop that dispatches JSON-RPC frames and lets everything else be ignored (the
push channel stays server→client for non-RPC frames). The push select-loop below is UNCHANGED.

**Replace this line:**
```go
	// Drain incoming control/data frames in the background so pings are answered and a
	// client close is observed promptly (it cancels ctx via conn.CloseRead).
	ctx = conn.CloseRead(ctx)
```

**With this block:**
```go
	// Read inbound frames in the background. A frame carrying "jsonrpc":"2.0" is a JSON-RPC
	// request (dispatch it, write the response frame back — P6.3, design §3.1); any other client
	// frame is ignored (the push channel is server→client only). A read error / client close
	// cancels rpcCtx, which we propagate to end the push loop.
	rpcCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	go func() {
		defer cancelRead()
		for {
			typ, data, err := conn.Read(rpcCtx)
			if err != nil {
				return // client closed / ctx cancelled / read error: stop reading, end the connection
			}
			if typ != websocket.MessageText {
				continue // JSON-RPC frames are text; ignore binary/control frames here
			}
			if _, werr := handleWSFrame(rpcCtx, conn, s.rpc, data, s.logger); werr != nil {
				return // a response-write failure means the socket is gone; stop
			}
		}
	}()
	ctx = rpcCtx   // the push select-loop below now ends when the reader ends
```

Notes:
- `handleWSFrame` returns `(isRPC bool, err error)`. `isRPC=false` = a non-RPC client frame (ignored);
  a JSON-RPC notification is `isRPC=true` with no response written. You only need the `err` here.
- Do NOT keep the old `conn.CloseRead(ctx)` — you cannot both `CloseRead` and `conn.Read` (CloseRead
  spawns its own reader). The loop above replaces CloseRead's drain-and-observe-close behavior.
- `context` and `github.com/coder/websocket` are already imported by `ws.go`.

**WS parity test plan (integrator live check, gate #p63 "works over both HTTP POST and WS"):**
1. Start the daemon; open a WS client to `ws://127.0.0.1:<port>/api/ws`.
2. Send text frame `{"jsonrpc":"2.0","method":"engines.list","id":1}`; assert a response frame arrives
   with `"id":1` and a `result` array — and that it byte-matches the `POST /api/rpc` reply for the same
   body (the shared dispatcher guarantees this; the test just confirms the transport wiring).
3. Send a notification `{"jsonrpc":"2.0","method":"engines.list"}`; assert NO response frame arrives
   (but push frames like heartbeat still do).
4. Send a non-RPC frame `{"type":"noise"}`; assert it is ignored (no response, connection stays up).
5. Confirm server→client push frames (hardware.snapshot, heartbeat, models.updated) still arrive
   throughout — the reader loop must not suppress them.

---

## 4. Jobs.* wiring (P6.2 dependency — DEFERRED until Agent A lands `internal/core/jobs.go`)

STATUS at handoff: `internal/core/jobs.go` does NOT yet exist. The four `jobs.*` RPC methods are
registered and bound to an **unwired stub** (`unwiredJobService`) that returns a `-32000` with a clear
"jobs subsystem not wired" reason (never a panic, never a silent nil). The package compiles and all
tests pass WITHOUT the P6.2 core methods.

Once Agent A lands these core methods (design §2.2 signatures):
```go
func (s *Service) SubmitJob(ctx context.Context, req core.JobSubmit) (core.JobView, error)
func (s *Service) GetJob(ctx context.Context, id string) (core.JobView, error)
func (s *Service) ListJobs(ctx context.Context, filter core.JobFilter) ([]core.JobView, error)
func (s *Service) CancelJob(ctx context.Context, id string) (core.JobView, error)
```

...swap the dispatcher construction in §1b for the jobs-wired variant. Add this shim (a new small file,
e.g. `internal/api/rpc_jobs.go`, so `internal/jsonrpc` stays core+stdlib only — the shim lives in the
api package, which already imports core):

```go
package api

import (
	"context"
	"encoding/json"

	"github.com/localhub/localhub/internal/core"
	"github.com/localhub/localhub/internal/jsonrpc"
)

// coreJobService adapts *core.Service (P6.2) to jsonrpc.JobService: decode raw params into the
// core job types, call core, return the marshalable JobView(s). This is the ONE place jobs params
// are interpreted, keeping internal/jsonrpc free of the core jobs types (design §3.2 note).
type coreJobService struct{ svc *core.Service }

func (j coreJobService) SubmitJob(ctx context.Context, params json.RawMessage) (any, error) {
	var req core.JobSubmit
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, err
		}
	}
	return j.svc.SubmitJob(ctx, req)
}
func (j coreJobService) GetJob(ctx context.Context, id string) (any, error) {
	return j.svc.GetJob(ctx, id)
}
func (j coreJobService) ListJobs(ctx context.Context, params json.RawMessage) (any, error) {
	var f core.JobFilter
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &f); err != nil {
			return nil, err
		}
	}
	return j.svc.ListJobs(ctx, f)
}
func (j coreJobService) CancelJob(ctx context.Context, id string) (any, error) {
	return j.svc.CancelJob(ctx, id)
}
```

Then change §1b to:
```go
	s.rpc = jsonrpc.NewDispatcherWithJobs(svc, coreJobService{svc: svc})
```

That is the ONLY change to wire jobs.* — the four RPC method bindings already exist in the registry.

CAVEAT: verify Agent A's actual `JobSubmit`/`JobFilter` field names/JSON tags match what a JSON-RPC
caller sends (`{model, priority?, ...}` per design §2.2). If `SubmitJob` expects the raw OpenAI body
merged with `priority`, adjust the shim's decode accordingly (it is the one interpretation point).

---

## 5. Method registry (what the surface exposes) — for reference

All bound to existing `core.Service` methods (verified 2026-07-07 against the code):

| RPC method | core call | notes / deviations from design §3.2 table |
|---|---|---|
| `hardware.get` | `Hardware(ctx)` | |
| `engines.list` | `Engines(ctx)` | returns `[]EngineStatusView`, **no error** (not `[]EngineStatus`) |
| `engines.detect` | `DetectEngines(ctx)` | design said `EnginesDetect`; real name is `DetectEngines`; no error |
| `models.list` | `Models(ctx)` | |
| `models.rescan` | `RescanModels(ctx)` | |
| `models.preview` | `CompatPreview(ctx, CompatRequest)` | params IS a `core.CompatRequest` |
| `models.compat` | `CompatPreview(ctx, CompatRequest{ModelID,Ctx,Batch})` | **no separate `ModelCompat` method exists**; mirrors REST `GET /api/models/{id}/compat` |
| `models.manifest` | `ManifestForModel(ctx, id)` | design said `ModelManifest`; real name is `ManifestForModel` |
| `models.reproduce` | `Reproduce(ctx, id)` | |
| `instances.list` | `Instances(ctx)` | |
| `instances.get` | `Instance(ctx, id)` | |
| `instances.start` | `StartInstance(ctx, StartInstanceRequest)` | `*QueuedError` → `-32000` + `data.{reason,queue_id,position,code}` |
| `instances.stop` | `StopInstanceAsync(ctx, id)` | |
| `queue.list` | `Queue(ctx)` | |
| `queue.cancel` | `CancelQueueEntry(ctx, id)` | core returns only `error`; RPC result is `{ok:true,id}` on success |
| `jobs.submit` | `SubmitJob` (P6.2) | unwired stub until §4 |
| `jobs.get` | `GetJob` (P6.2) | unwired stub until §4 |
| `jobs.list` | `ListJobs` (P6.2) | unwired stub until §4 |
| `jobs.cancel` | `CancelJob` (P6.2) | unwired stub until §4 |

Not exposed (design §3.3): downloads/imports mutations, vLLM dev endpoints, raw process kill, DB access,
the `/v1/*` OpenAI proxy (a passthrough surface, not a control-plane RPC).

---

## 6. Post-wiring verification

```
go build ./...          # whole tree compiles with the wiring
go test ./internal/api/... ./internal/jsonrpc/...   # api + jsonrpc suites green
```
Then run the WS parity live check (§3) once against the running daemon.
```
