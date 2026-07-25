# Wiring notes — P1.5 web dashboard (`web/`)

Author: P1.5 FRONTEND agent. Scope: the React + Vite + TypeScript dashboard under
`web/`. This agent touched ONLY files under `web/` plus this file (coordination rule).
No Go code, `go.mod`, `configs/`, `scripts/`, or PLAN/TODO/TESTING were modified.

The dashboard is Phase-1 read-only observability: GPU live panel, engines panel,
inventory table/cards, and a header connection indicator. No mutation UI (no
start/stop/evict). The one control-plane action present is `POST /api/models/rescan`
(walks disk; not engine mutation).

Build verified 2026-07-06 on Node 24.11.1 / npm 11.6.2 (via nvm):
`npx tsc --noEmit` clean, `npx vitest run` 42/42 pass (5 files),
`npm run build` clean → `web/dist` (~56 KB gzipped total; see §5).

---

## 1. What the Go side must do — static embed + SPA serving

The production build lands in `web/dist`. The Go daemon serves it from an
`embed.FS`. Recommended pattern (the later Go embed task owns this — it is NOT in
P1.5's scope, but this is the contract the frontend was built against):

```go
// internal/api/webui/embed.go  (or wherever the daemon prefers)
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the built dashboard rooted at dist/ (so "/" maps to dist/index.html).
func FS() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
```

**Important — the `dist/` directory must exist at compile time** or `//go:embed`
fails the build. Options: (a) commit `web/dist`; (b) run `npm run build` in the Go
build/CI step before `go build`; (c) guard the embed behind a build tag and serve a
"UI not built" stub in dev. Recommend (b) in the release build, (a) or (c) for dev
convenience. The frontend agent does NOT commit `dist` (it is gitignored in
`web/.gitignore`).

### Static handler with SPA fallback

The app is currently a single page (no client router yet), but code the handler as
an SPA so future routes work: serve the requested asset if it exists, else fall back
to `index.html`. Do NOT fall back for `/api/*` or `/ws` — those are real endpoints.

```go
func StaticHandler() (http.Handler, error) {
	sub, err := webui.FS()
	if err != nil { return nil, err }
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// never intercept API/WS — those are registered separately and should 404
		// here only if truly unregistered.
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" { p = "index.html" }
		if _, err := fs.Stat(sub, p); err != nil {
			// unknown path with no file extension → SPA route → serve index.html
			if path.Ext(p) == "" {
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/"
				fileServer.ServeHTTP(w, r2)
				return
			}
			// asset with an extension that doesn't exist → real 404
			http.NotFound(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	}), nil
}
```

Mount the static handler LAST (after `/api/*`, `/metrics`, `/ws`) so it only catches
what the API router didn't. With Go 1.22+ `http.ServeMux` pattern routing, register
API routes with method+path patterns and the UI as the bare `/` fallback.

### Asset paths are RELATIVE — mount anywhere

`vite.config.ts` sets `base: './'`, so every asset URL in `dist/index.html` is
relative (`./assets/index-*.js`, `./assets/index-*.css`). The bundle therefore
serves correctly whether mounted at `/`, under a sub-path, or behind Tailscale Serve
with a path prefix. **Do not** rewrite these to absolute paths.

### Cache headers

- Hashed assets under `dist/assets/` (content-hash in filename) are immutable:
  `Cache-Control: public, max-age=31536000, immutable`.
- `index.html` must NOT be cached hard (it references the hashed asset names):
  `Cache-Control: no-cache` (or `max-age=0, must-revalidate`). Otherwise a phone
  keeps loading stale asset names after a redeploy.
- A simple rule: if the request path starts with `/assets/` → immutable; else (incl.
  index.html) → no-cache. `http.FileServer` does not set these; wrap it or set
  headers before delegating.

### Content-Type

`http.FileServer` + `http.FS` set MIME types by extension correctly for `.js`,
`.css`, `.html`. No extra config needed. (If a future asset type mismatches, it is
the handler's concern, not the build's.)

---

## 2. WebSocket endpoint contract (`/api/ws`) — coded for, NOT required in Phase 1

Per PLAN.md decision 7, the client ships a WebSocket abstraction ALONGSIDE SSE. In
Phase 1 it is **disabled by default** — the client uses SSE only. Enabling WS is a
single config flip in `web/src/live/connection.ts`:

```ts
// DEFAULT_LIVE_CONFIG.transport: 'sse'  →  'auto'  (or 'ws')
```

`'auto'` tries WS once; if `/api/ws` doesn't upgrade/open, it falls back to SSE
cleanly (unit-tested). So the Go WS endpoint can land later (P1.5 Go follow-up or
P6.2) with zero frontend changes beyond that flip.

**Contract the client expects when `/api/ws` exists:**

- **URL:** `ws(s)://<host>/api/ws` (scheme derived from page scheme:
  `https:` → `wss:`, else `ws:`). Same-origin. Dev proxy already forwards `/ws` and
  `/api` (Vite `server.proxy`), but the client default WS URL is `/api/ws` — if the
  Go endpoint is at a different path, set `LiveConfig.wsUrl`.
- **Direction:** server→client push is all the client needs for Phase 1 (it never
  sends frames). A bidirectional control channel (JSON-RPC over WS, decision 7(d)) is
  net-new later work and does not affect this client.
- **Framing:** each WS message is ONE JSON object. The client parses it with the
  SAME `parseFrame()` used for SSE. Supported event shapes (event vocabulary from
  `research/lmwrapper-reuse-notes.md`):

  | `type`               | payload                              | client action |
  |----------------------|--------------------------------------|---------------|
  | `heartbeat`          | `{ "type":"heartbeat", "ts"?:str }`  | keep-alive; resets stall timer |
  | `hardware.snapshot`  | `{ "type":"hardware.snapshot", "data": <HardwareSnapshot> }` OR the bare snapshot fields alongside `type` | updates the GPU/system panel |
  | `models.updated`     | `{ "type":"models.updated", "data": <ModelSummary[]> }` (data optional) | triggers a refetch of `GET /api/models` |
  | `engines.updated`    | `{ "type":"engines.updated", "data": <Engine[]> }` (data optional) | triggers a refetch of `GET /api/engines` |

  Notes:
  - For `hardware.snapshot` the client accepts BOTH `{type, data:{...}}` and
    `{type, ...snapshotFields}`. Prefer the `data` envelope for consistency.
  - For `models.updated` / `engines.updated`, the client currently only uses them as
    an invalidation signal (it refetches over REST); sending `data` is optional and
    forward-compatible (the client will consume it directly later).
  - Unknown `type` values are ignored (forward-compatible).
  - The client sends a **stall reconnect** if NO frame (of any type) arrives within
    15s. If the server has no data to push, send a `heartbeat` at least every ~10s to
    keep the link marked healthy.

**SSE contract (Phase 1, live now):** `GET /api/hardware/live` streams SSE frames
where each `data:` line is a bare `HardwareSnapshot` JSON (no `type` field — the
client detects it by shape: has `gpus` array + `captured_at`). This exactly matches
the handler sketch in `wiring-p12.md` §4b. SSE comment/keep-alive lines (`:...`) are
ignored by the client. When the Go side later wants to multiplex models/engines over
SSE too, switch to typed frames (`data: {"type":"hardware.snapshot","data":{...}}`)
— the client already parses those.

---

## 3. API shapes the client consumes (must match the wiring agents' final JSON)

All typed in `web/src/api/types.ts`. Sourced from wiring-p12/p13/p14. If the Go
wiring agent's FINAL shapes differ from these, the client's `types.ts` +
fixtures must be updated (they are the only coupling point). Flagged assumptions:

- **`GET /api/hardware`** and each **`/api/hardware/live`** SSE frame →
  `HardwareSnapshot` per wiring-p12 §5: `{ gpus:[{index,name,vram_total,vram_free,
  vram_used?,driver,cc,cuda?,util,temp,power}], cpu:{model,cores,threads,util},
  ram:{total,free,used}, disk:[{path,total_gib,free_gib,used_gib}], captured_at }`.
  VRAM is **MiB**; `util`/`temp`/`power`/`cpu.util` = **-1** means unavailable (the
  UI renders "N/A"). `disk` is an **array**.
- **`GET /api/engines`** → `Engine[]` per wiring-p13:
  `{ name, installed, version, running, base_url, reason, details? }`.
  - `installed` = Detected, `running` = Reachable. Both drive badges.
  - **`reason` is ALWAYS present** and is rendered — the UI shows it for unreachable
    engines (crit-styled) and as info otherwise. If the Go side omits `reason`, the
    non-waivable mandatory-reason rule is violated; the UI degrades gracefully (no
    reason line) but this should not happen.
  - `llamacpp` may appear **multiple times** (one per binary). The client keys cards
    by `details.resolved_path || details.binary_path || base_url` + index, and shows
    `details.backends[]` + `details.caps[]` (incl. `tq3`, `flash-attn`) as chips.
  - `details` is optional and flexible (`[k:string]:unknown`) — extra fields are
    tolerated.
- **`GET /api/models`** → `ModelSummary[]` per wiring-p14 (the `models` DDL columns):
  `{ id, display_name, source, format, quantization, repo, revision, local_path,
  size_bytes, sha256, license, recommended_engine, compat_status, manifest_id?,
  created_at?, updated_at? }`. `size_bytes` is raw bytes (client formats to GiB).
  Nullable fields render "—".
- **`POST /api/models/rescan`** → any JSON (the client ignores the body on success
  and just refetches `/api/models`). A `RescanResult` `{inserted,updated,vanished,
  skipped}` is typed but not required. **This endpoint's existence is an assumption**
  — wiring-p14 §1c flags it as implied-but-not-in-api-spec and asks the orchestrator
  to confirm the path. If the daemon does not register `POST /api/models/rescan`, the
  Rescan button will surface the error's `reason` (e.g. 404) in the UI — no crash.
- **Error envelope (all endpoints):** non-2xx responses must be
  `{ "error": { "code", "message", "reason"? } }`. The client's `ApiError` extracts
  `reason` (falling back to `message`) and shows it. A non-JSON error body still
  produces a readable reason (raw text or an HTTP-status message).

### Assumptions to confirm against the Go wiring agent's FINAL shapes

1. `POST /api/models/rescan` exists and is the rescan trigger (wiring-p14 open Q).
2. Engine `reason` field is named exactly `reason` (wiring-p13 says yes; api-spec's
   bare tuple omits it — the client needs the p13 projection, not the bare tuple).
3. Engine per-binary detail lives under `details` (backends/caps/resolved_path). If
   the Go side flattens these differently, update `EngineDetails` in `types.ts`.
4. Hardware `disk` is an array and VRAM is MiB (both per wiring-p12 — confirmed
   there, restated here since they are load-bearing for the VRAM bar + disk list).
5. SSE frames on `/api/hardware/live` are bare snapshots (no `type`). If the Go side
   emits typed frames, the client already handles them — no change needed.

---

## 4. Dev proxy port assumed

`vite.config.ts` `server.proxy` forwards, in DEV ONLY:
- `/api`  → `http://127.0.0.1:39876`
- `/ws`   → `ws://127.0.0.1:39876` (ws upgrade)

**Port 39876** confirmed from `configs/config.example.yaml` (`server.port: 39876`,
`server.bind: 127.0.0.1`) and matches wiring-p12/security spec. In PRODUCTION the Go
daemon serves both the API and the static assets from the **same origin**, so no
proxy runs and same-origin `fetch('/api/...')` / `new EventSource('/api/hardware/live')`
just work. If the daemon's port is ever changed, update only the dev proxy target
(production is origin-relative and unaffected).

---

## 5. Build output (the embed payload)

`npm run build` (runs `tsc --noEmit` then `vite build`) → `web/dist`:

```
dist/index.html                 0.52 kB  (gzip 0.31 kB)   — relative asset refs, no CDN
dist/assets/index-<hash>.css   10.07 kB  (gzip 2.84 kB)
dist/assets/index-<hash>.js   164.25 kB  (gzip 53.00 kB)  — React 18 + app
────────────────────────────────────────────────────────
total raw ~177 kB · total gzipped ~56 kB
```

Verified: `dist/index.html` contains NO external host references (only relative
`./assets/...`); built JS/CSS contain no `@import`, no CSS `url()`, and no runtime
network hosts (the only `http(s)://` strings in the JS are React's internal XML
namespaces + the react error-decoder doc link string — never fetched). Fully
self-contained for phone-over-Tailscale loading.

---

## 6. File tree delivered (all under `web/`)

```
web/
├── .gitignore                       # node_modules, dist, .vite
├── .claude/launch.json              # local preview server config (npm run preview)
├── index.html                       # Vite entry (dev); build injects hashed assets
├── package.json                     # React 18, Vite 5, Vitest 2, TS 5 (no heavy UI framework)
├── tsconfig.json                    # strict; noUnusedLocals/Parameters
├── vite.config.ts                   # base:'./' (embed contract), dev proxy, vitest config
└── src/
    ├── main.tsx                     # React root
    ├── App.tsx / App.module.css     # layout: GPU+Engines (left), Inventory (right); mobile stacks GPU-first
    ├── vite-env.d.ts                # CSS-module + vite/client ambient types
    ├── api/
    │   ├── types.ts                 # HardwareSnapshot, Engine, ModelSummary, ApiErrorBody
    │   ├── client.ts                # typed fetch + ApiError(reason) + error-envelope parsing
    │   └── useResource.ts           # loading/error/refresh hook (surfaces reason)
    ├── live/
    │   ├── connection.ts            # LiveConnection: SSE now + WS abstraction, backoff+stall state machine, parseFrame
    │   ├── connection.test.ts       # fake EventSource/WebSocket; reconnect + auto-fallback
    │   └── useLive.ts               # React binding for LiveConnection
    ├── lib/
    │   ├── format.ts                # VRAM bar math, MiB→GiB, bytes→human, metric/relative-time
    │   └── format.test.ts
    ├── components/
    │   ├── ui.tsx / ui.module.css   # Panel, Badge, Chip, Bar, Reason, states, IconButton
    │   ├── Header.tsx / .module.css # brand, daemon version, last-update, connection badge
    │   ├── GpuPanel.tsx / .module.css      # VRAM bar + util/temp/power + CPU/RAM/disk
    │   ├── EnginesPanel.tsx / .module.css  # per-engine cards; detected/reachable badges; caps chips; REASON
    │   ├── EnginesPanel.test.tsx
    │   ├── SourceBadge.tsx          # source (ollama/hf/gguf) + presence badges
    │   ├── InventoryPanel.tsx / .module.css # table (desktop) / cards (≤640px) + Rescan
    │   └── InventoryPanel.test.tsx
    ├── App.test.tsx                 # smoke: full app mounts with mocked fetch+EventSource
    └── test/
        ├── setup.ts                 # jest-dom matchers
        └── fixtures.ts              # real-box shapes from wiring-p12/13/14
```

Node engine: **24.11.1** via nvm (`nvm use 24.11.1`). `node_modules` lives inside
`web/` (on `K:`). No CDN, no heavy UI framework (plain CSS modules); deps are React +
Vite + Vitest/testing-library only.

---

## 7. Tests (small but real — 42 across 5 files, all pass)

- `format.test.ts` — VRAM bar math (used-fraction, clamp-to-100%, warn/crit tiers,
  zero-total safety), MiB→GiB, bytes→human, -1 "N/A" sentinel.
- `connection.test.ts` — SSE reconnect state machine via fake EventSource:
  idle→connecting→connected, error→reconnecting with **capped exponential backoff**,
  attempts reset on reopen, stall-timeout reconnect, stop() cancels reconnect, every
  non-connected status carries a reason; plus WS `'auto'`→SSE fallback via fake
  WebSocket; plus `parseFrame` on bare/typed/junk frames.
- `EnginesPanel.test.tsx` — detected/reachable badges, **mandatory reason rendered on
  the unreachable vLLM engine**, capability chips, reason on not-found engine, load-error reason.
- `InventoryPanel.test.tsx` — table rows, source badges (ollama vs gguf), **presence
  badge (present default, missing when `compat_status='missing'`)**, rescan button,
  empty-state reason, load-error reason.
- `App.test.tsx` — smoke: whole app mounts, GPU paints from a mocked SSE frame,
  header shows "SSE live", engines + inventory render from mocked fetch.

Run: `cd web && nvm use 24.11.1 && npx vitest run` (and `npx tsc --noEmit`,
`npm run build`).
```
