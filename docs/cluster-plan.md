# Cluster Plan — LAN Federation of Autonomous LocalHub Daemons (P7)

**Status: DRAFT — plan-first deliverable for X6 (TODO.md). No code exists yet; nothing in
this document is implemented. Implementation may not start until the owner has reviewed
the checklist below and ADR-008 has been appended to `docs/decisions.md`.**

This document is the master design for "cluster support" as re-scoped by the project
owner on 2026-07-08. It amends ADR-007 (single-box-only); the amending ADR-008 draft is
at the end of this file, marked for appending to `docs/decisions.md` on review.

---

## 0. Review checklist — decisions the owner should eyeball before any code

Each item: the question, the options considered, and the recommendation in bold.
Implementation follows the bolded recommendation unless the owner overrides during review.

1. **Coordinator in the same binary, or a separate binary?**
   Options: (a) a `cluster` section in config with `role: coordinator | node | off` in the
   one `localhub` binary; (b) a separate `localhub-coordinator` binary.
   **Recommendation: same binary, config-selected role (`cluster.role`).** One artifact to
   install, one version to keep in sync across boxes, and the coordinator needs most of the
   daemon anyway (config, logging, HTTP server, auth middleware, SQLite). A coordinator
   node still runs its full local daemon — the coordinator is an *additional role*, not a
   replacement. See §2.2.

2. **Topology: hub-and-spoke or peer-to-peer?**
   **Recommendation: hub-and-spoke v1.** One coordinator, N nodes. No gossip, no leader
   election, no consensus. Justification in §2.3. Peer-to-peer/HA coordinator is explicitly
   deferred until a real deployment demonstrates the need.

3. **Who dials whom (join direction)?**
   Options: (a) coordinator config statically lists node URLs and polls them; (b) nodes are
   configured with the coordinator URL and push-register + heartbeat.
   **Recommendation: nodes dial the coordinator (push join + periodic heartbeat), and the
   coordinator's registry is soft state rebuilt from those heartbeats.** Survives node DHCP
   address changes, makes coordinator restart trivially recoverable (§7), and adding a box
   means editing config on the *new* box only. The coordinator additionally health-probes
   registered nodes on demand. See §2.4.

4. **Node↔coordinator auth mechanism?**
   Options: (a) a single shared `cluster.token` (bearer) in every member's config; (b)
   per-node tokens issued by the coordinator; (c) mTLS.
   **Recommendation: shared cluster bearer token v1, distinct from `admin_token`, mandatory
   whenever clustering is enabled on a non-loopback bind; Tailscale as the recommended
   transport; mTLS deferred.** Rationale and threat model in §4.

5. **Placement policy order when more than one node can serve a model?**
   **Recommendation: already-warm > model present on disk (verdict `runs`) > `runs` beats
   `runs_degraded` > most free VRAM > round-robin tiebreak.** Justification in §6.2.

6. **Model weight transfer between nodes: in or out for v1?**
   **Recommendation: OUT for v1 (explicit non-goal).** Each node serves what is on its own
   disk; placement only routes to nodes that HAVE the model, or the operator triggers that
   node's own Phase-3 supply chain (`POST /api/downloads` on that node, proxied through the
   coordinator UI at most). No LocalHub-to-LocalHub weight copying. §3.

7. **Distributed state store: any?**
   **Recommendation: none.** The coordinator holds soft state (node registry, cached
   inventories) rebuilt from node heartbeats/polls; the only durable addition is a small
   additive `nodes` table in the coordinator's existing SQLite file. No raft, no etcd, no
   shared DB. §5.

8. **Mid-stream node failure semantics?**
   **Recommendation: truncated stream, no transparent retry v1.** If a node dies while
   streaming a completion through the coordinator, the client sees the stream break (or a
   502 if no bytes were written yet — matching today's `internal/proxy.RelayError`
   semantics). Transparent replay would silently re-run inference (same reason jobs are
   never silently re-run after a daemon restart — see docs/api-spec.md restart semantics).
   §7.

---

## 1. Motivation and decision trail

### 1.1 The owner decision (verbatim, dated)

On **2026-07-08** the project owner directed:

> "also in future we shall be able to run it in cluster, add support for it, we shall
> implement and plan for it, write down the plans first and then start implementing"

and clarified the meaning of "cluster":

> "cluster means not necessarily k8s or anything like that simple over the network gpu
> clusters"

This is recorded in TODO.md item X6 and temp-research/context-summary.md §X6. It **amends
ADR-007** ("Single box first; multi-node is not an MVP shape"), which recorded multi-node
as deferred-until-real-demand. The demand is now real and comes from the owner. ADR-007
also said: *"add nodes later behind the same adapter contract if warranted"* — this plan is
exactly that additive step, not a reversal of the reasoning that produced ADR-007.

### 1.2 What "cluster" means here — and what it does not

**Means:** two or more ordinary boxes, each with its own GPU(s), on the same LAN or
tailnet, each running the *same fully autonomous LocalHub daemon it runs today*, with one
thin coordination layer on top so the owner gets one pane of glass and one OpenAI base URL
for all of them.

**Does not mean:** Kubernetes, containers orchestration, a datacenter scheduler, tensor
parallelism across boxes, cross-node VRAM management, or anything GPUStack/KubeAI already
own. The product-brief line still holds: *"If you have Kubernetes and 40 GPUs, use
GPUStack or KubeAI."* This feature targets the person who has a *second* (or third)
workstation with a GPU, not a fleet.

### 1.3 Why this shape does not break the single-box wedge

Every guarantee LocalHub 1.0 makes is **per-node and untouched**: per-node residency,
admission control, never-kill-active, NVML-verified VRAM truth, manifests, verdicts with
reasons. A node that never hears of a coordinator behaves byte-for-byte as today. The
cluster layer is strictly additive: a registry, an aggregator, a placement picker, and a
relay. If the coordinator vanishes, every node keeps serving locally (§7). This is the
architectural property that keeps ADR-001/ADR-005/one-core intact.

---

## 2. Architecture

### 2.1 The shape

```
                        Phone / Browser / OpenAI client
                                     │
                        Tailscale / LAN  (one base URL)
                                     │
              ┌──────────────────────▼──────────────────────┐
              │  COORDINATOR ROLE (any one LocalHub daemon) │
              │  • node registry (join / heartbeat / health)│
              │  • aggregated inventory + hardware view     │
              │  • placement (ask nodes, pick by policy)    │
              │  • routing (/v1/* relayed to owning node)   │
              │  • WS fan-in (node events, tagged node_id)  │
              │  — plus its own full local daemon, unchanged│
              └──────┬──────────────┬──────────────┬────────┘
                     │ HTTP+WS      │ HTTP+WS      │ HTTP+WS
                     │ (cluster     │              │
                     │  token)      │              │
              ┌──────▼─────┐ ┌──────▼─────┐ ┌──────▼─────┐
              │  NODE  A   │ │  NODE  B   │ │  NODE  C   │
              │ full local │ │ full local │ │ full local │
              │ LocalHub   │ │ LocalHub   │ │ LocalHub   │
              │ (residency,│ │ daemon,    │ │ daemon,    │
              │  admission,│ │ untouched  │ │ untouched  │
              │  NVML, …)  │ │            │ │            │
              └────────────┘ └────────────┘ └────────────┘
```

Key invariants:

- **A node is a complete LocalHub.** Nothing is removed or thinned on nodes. Each node's
  residency manager, queue, estimator, engine adapters, supply chain, and UI keep working
  when addressed directly.
- **The coordinator never computes fits.** Verdicts are functions of a node's *own*
  hardware and *its* live free VRAM; the coordinator asks nodes (`POST /api/compat/preview`
  / `GET /api/models/{id}/compat` on the node) and aggregates answers. The one-core rule
  holds *per node*; the coordinator is a thin aggregator with its own small core
  (`internal/cluster`), not a second brain about VRAM.
- **The coordinator never runs admission for a node.** Routing a completion to node B
  means relaying the OpenAI request to node B's `/v1/chat/completions`; node B's own
  `ResolveForProxy` does admission, in-flight counting, and never-kill-active exactly as
  today. The coordinator only chooses *which node* to relay to.
- **Routing reuses the existing relay.** `internal/proxy.Relay(ctx, w, targetBaseURL,
  path, accept, body)` is already host-agnostic (it takes any base URL). Coordinator
  routing is "resolve node → Relay to `node.base_url`" — the streaming, cancel-on-client-
  disconnect, and error-before-first-byte semantics come for free from the code that
  shipped in P6.1.

### 2.2 Coordinator: a role of the same binary (Decision 1)

Recommended: `cluster.role: coordinator | node | off` (default `off`) in config, one
binary.

Why same-binary wins:

- **Install friction is the product** (ADR-006). Two binaries doubles the distribution
  and version-skew surface for a FOSS tool whose wedge is a 5-minute install.
- **The coordinator needs ~80% of the daemon anyway:** config loading, structured logging,
  the HTTP server + middleware chain, SQLite, the embedded UI. Only
  `internal/cluster` is new.
- **The coordinator box usually has a GPU too.** In a 2–3 box home lab, dedicating a box
  to pure coordination is absurd; the coordinator role runs *alongside* the local daemon
  role on the same process, and the coordinator's own node participates in placement like
  any other (it registers with itself over loopback, so one code path serves all nodes).
- A separate binary buys only isolation that config-gating already provides.

Cost acknowledged: role-conditional route registration in `internal/api/server.go`
(`routes()` gains a `cluster.role == coordinator` block) and role-conditional config
validation. Small, contained.

### 2.3 Hub-and-spoke, not peer-to-peer (Decision 2)

Recommended: hub-and-spoke v1.

- **State lives in one place.** With one coordinator there is no consistency protocol at
  all: the registry is a map, placement is a loop over cached node snapshots plus a live
  confirm. Peer-to-peer means gossip/consensus — precisely the "distributed scheduler"
  the owner scoped out.
- **The failure story is legible.** Coordinator down = no aggregated view and no single
  base URL, but *every node keeps serving locally* (§7). Users can point clients directly
  at a node's own `/v1/*` as the manual fallback. Peer-to-peer failure modes (split
  brain, divergent registries) are not legible.
- **It matches the deployment reality:** 2–4 boxes owned by one person. There is no
  availability requirement that justifies electing coordinators.
- Any node *can* be the coordinator (it's a config flag), so there is no special
  hardware; if the coordinator box dies, the owner flips the flag on another box and
  points nodes at it. That is the v1 "HA story," documented, honest, manual.

### 2.4 Join, heartbeat, and health (Decision 3)

- A node with `cluster.role: node` has `cluster.coordinator_url` and `cluster.token` in
  config. On startup (and on a retry loop while unreachable) it calls
  `POST {coordinator}/api/cluster/nodes` with its identity: `{node_id, name, base_url,
  version}`. `node_id` is a UUID minted once and persisted in the node's own SQLite
  (survives restarts; renaming a box does not re-identity it). `base_url` is how the
  coordinator should reach the node back (the node's non-loopback bind or tailnet name) —
  explicitly configured, never guessed from the socket peer address (NAT/proxy safe).
- **Heartbeat:** the node re-POSTs the same registration every `heartbeat_seconds`
  (default 15s) with a small status payload (free VRAM, instance count, engine health
  summary). Registration is idempotent upsert — join and heartbeat are the same call, so
  there is one code path and coordinator restart needs no special node logic.
- **Health states** on the coordinator: `ONLINE` (heartbeat within 2× interval),
  `STALE` (2–4× interval; still routable but placement deprioritizes), `UNREACHABLE`
  (>4× interval or an active probe failed; never placed, shown red in UI). The
  coordinator may also actively probe `GET {node}/api/health` before committing a
  placement (that path is auth-exempt by design — see middleware — so it works even
  before token wiring, but placement confirms with an authed call).
- **Leave** is just heartbeat absence; `DELETE /api/cluster/nodes/{id}` exists for
  explicit removal from the registry.
- **Clock skew is a non-issue by construction:** `last_seen` is stamped with the
  *coordinator's* clock at receive time. Node-reported timestamps are never used for
  liveness (§7).

### 2.5 What the coordinator adds, precisely

| Concern | Coordinator behavior |
|---|---|
| Node registry | In-memory map + additive `nodes` table (durable names/ids only); rebuilt from heartbeats |
| Aggregated inventory | Periodic + on-demand `GET {node}/api/models`, cached with node_id tags; `models.updated` WS events trigger refresh |
| Aggregated hardware | Cached last heartbeat payload + on-demand `GET {node}/api/hardware` |
| Placement | For a requested model: filter nodes that HAVE it → ask each candidate for a live verdict/free-VRAM → pick by policy (§6.2) |
| Routing | Relay the OpenAI request to the chosen node's `/v1/*` via the existing `internal/proxy.Relay` |
| WS fan-in | Client WS connection to each node's `/api/ws`; re-publish frames on its own `/api/ws` wrapped `{node_id, event}` |
| Jobs (P7.4) | Accept a job, place it, submit to the owning node's `/api/jobs`, mirror terminal state via fan-in |

---

## 3. Explicit non-goals carried forward

These remain non-goals **inside the cluster feature**, not just outside it. Every one of
them is an attractive "improvement" that must be refused in review:

- **No k8s / container orchestration.** No operators, no CRDs, no Docker requirement.
  (Product-brief non-goals; unchanged by ADR-008.)
- **No cross-node VRAM preemption or fair-share scheduling.** ADR-001 applies with more
  force across a network than within a box. Placement is admission-time selection only;
  once placed, a request's fate is governed entirely by that node's local residency rules.
- **No model weight transfer between nodes v1.** Each node serves what its own disk
  holds. Placement routes only to nodes that HAVE the model; getting a model onto another
  node is that node's own Phase-3 supply chain (owner-triggered), preserving the manifest/
  provenance story per node. (Decision 6.)
- **No distributed state store v1.** Coordinator soft state, rebuilt from node
  heartbeats. Losing the coordinator's cache loses nothing durable. (Decision 7.)
- **No tensor/pipeline parallelism across boxes.** One model instance always lives
  entirely on one node. (llama.cpp RPC-server-style distributed inference is explicitly
  out; it is experimental upstream and violates the "engines are external supervised
  processes" simplicity of ADR-005 deployments.)
- **No multi-tenant auth/RBAC.** Same single-operator posture as security.md; the
  cluster token is machine-to-machine, not a user system.
- **No gateway feature creep.** The coordinator's `/v1/*` stays a thin passthrough
  (ADR-002); LiteLLM still owns budgets/keys/fallback if wanted, now simply pointed at the
  coordinator.

---

## 4. Security

This is the first feature whose *purpose* is leaving localhost, so it gets the longest
section.

### 4.1 Current posture being disturbed

Today (verified in code, 2026-07-08):

- `internal/config/config.go` `validateBind` **hard-fails any non-loopback bind** unless
  `server.allow_nonlocal: true` (lines ~348, ~413–425), with a DANGEROUS warning.
- `internal/api/middleware.go` `authMiddleware` enforces `server.admin_token` as a
  mandatory bearer on all `/api/*` and `/v1/*` when set (constant-time compare), with
  exactly two exemptions: `GET /api/health` and the static UI assets.
- docs/security.md's threat #2 is *accidental public exposure of an admin-capable
  service*; the mitigation is the loopback default.

Clustering requires nodes to listen where the coordinator can reach them, and the
coordinator to listen where clients and nodes can reach it. That means non-loopback binds
(LAN IP) **or** loopback binds fronted by Tailscale Serve / tsnet. Both paths must keep
threat #2 mitigated.

### 4.2 Node↔coordinator auth (Decision 4)

**v1: a shared cluster bearer token (`cluster.token`), distinct from `admin_token`.**

- Every cluster-plane call (node→coordinator join/heartbeat; coordinator→node inventory,
  verdict, relay, WS subscribe) carries `Authorization: Bearer <cluster.token>`, verified
  with the same constant-time pattern as `authMiddleware`.
- **Distinct from `admin_token` on purpose:** the admin token is the human operator's
  credential for a single box; the cluster token is a machine credential shared by the
  federation. Rotating one must not break the other, and a leaked node config must not
  hand out coordinator admin rights beyond what the cluster plane exposes.
- On the **node side**, coordinator-originated calls hit the node's ordinary API. v1
  keeps this simple: when `cluster.role: node`, the node accepts `cluster.token` as a
  valid bearer alongside `admin_token` (both compared constant-time; either passes the
  existing middleware). Per-endpoint scoping (cluster token can't call, say,
  `POST /api/downloads`) is a hardening item in P7.5, not a v1 blocker — the token holder
  is the coordinator the owner installed.
- **Mandatory when clustered + non-loopback:** config validation rejects
  `cluster.role != off` on a non-loopback bind with an empty `cluster.token` — the same
  hard-fail philosophy as `validateBind`. There is no unauthenticated cluster mode outside
  loopback (the two-daemon dev rig on 127.0.0.1 may run tokenless).
- Why not per-node tokens v1: an issuance/enrollment flow (approve node, store hashes,
  revoke) is real machinery for marginal gain at 2–4 owner-controlled boxes. Why not mTLS
  v1: cert generation/distribution/rotation is exactly the install friction security.md
  warns against, and Tailscale (below) already provides mutual authentication + WireGuard
  encryption at the transport layer for free. mTLS remains the documented upgrade path if
  a deployment can't use Tailscale and can't trust its LAN.

### 4.3 Tailscale as the recommended transport

The existing phone story already leans on Tailscale (security.md, examples/
tailscale-serve.md). Clustering composes with it naturally:

- Every box joins the same tailnet. `cluster.coordinator_url` and node `base_url`s are
  tailnet addresses (`http://box-a.tailnet-name.ts.net:9090` or 100.x.y.z).
- Tailscale gives node-to-node **encryption + device identity + ACLs** without LocalHub
  shipping any TLS code; the cluster token then rides inside an already-authenticated,
  encrypted tunnel (defense in depth rather than sole defense).
- With Tailscale Serve fronting a loopback bind, boxes can cluster **without ever setting
  `allow_nonlocal: true`** — the strongest posture, and the documented recommendation.
- Plain-LAN clustering (no Tailscale) is supported but documented as: token mandatory,
  cleartext HTTP on your LAN, trust your LAN accordingly — mirroring the existing
  "LAN-only users can bind to a LAN IP behind their own firewall" stance.
- (ADR-006 already contemplates embedding `tsnet`; if that lands, cluster traffic can use
  it directly. Not a P7 dependency.)

### 4.4 Bind interactions, spelled out

- A **node** must be reachable by the coordinator: either `server.bind: <LAN/tailnet IP>`
  + `allow_nonlocal: true` + `cluster.token` (validation-enforced trio), or loopback +
  Tailscale Serve. Config validation gains cluster-aware messages so the failure mode is
  a readable reason, not a mystery.
- The **coordinator** must be reachable by nodes and clients; same two options.
- The static-UI auth exemption (middleware) now exposes the dashboard *assets* on the
  LAN when non-loopback — as today, the UI is inert without a token for API calls;
  documented, unchanged.
- The **dangerous endpoints** stay dangerous: vLLM dev endpoints are still never
  forwarded by any relay (the coordinator relays only `/v1/chat/completions` and
  `/v1/completions`, the same allow-list discipline as `completionUpstreamPaths` in
  `internal/api/instances.go`). Instance stop/evict on a node via the coordinator is an
  authed cluster-plane call end to end.

### 4.5 Threat model (cluster additions)

In order of realism: (1) **A hostile device on the LAN** reaching a node's now-non-local
API — mitigated by mandatory cluster/admin tokens on non-loopback binds, and best
eliminated by the Tailscale transport (nodes never listen on the raw LAN). (2) **Token
leakage** (config file copied off a box) — one shared secret compromises cluster-plane
access on all members until rotated; rotation = edit configs + rolling restart, documented
in P7.5; per-node tokens/mTLS are the recorded upgrade path. (3) **A compromised node**
feeding the coordinator false verdicts/inventory — placement misroutes, but blast radius
is bounded by the node itself: the coordinator never acts on another node based on node
A's claims, and each node's own admission still protects its GPU. (4) **Coordinator
compromise** — it holds cluster-plane reach into every node; run it like the admin box it
is. Out of scope, stated plainly: adversarial co-tenant nodes, LAN eavesdropping without
Tailscale (use Tailscale), and any guarantee against a malicious *owner-installed* model —
unchanged from security.md.

---

## 5. Data model deltas (additive only)

On the **coordinator's** existing SQLite file (no new DB, no schema changes on nodes, no
destructive migration — consistent with data-model.md "add columns additively"):

```sql
-- Cluster nodes known to this coordinator (P7.1) ---------------------------
-- Soft-state-backed: rows persist identity/naming; liveness (status/last_seen)
-- is refreshed from heartbeats and is advisory, not authoritative.
CREATE TABLE nodes (
  id            TEXT PRIMARY KEY,            -- node-minted UUID, stable across restarts
  name          TEXT NOT NULL,               -- human label ("basement-4090")
  base_url      TEXT NOT NULL,               -- how the coordinator reaches the node
  version       TEXT,                        -- localhub version string (skew visibility)
  status        TEXT NOT NULL DEFAULT 'UNREACHABLE',  -- ONLINE|STALE|UNREACHABLE
  last_seen     TEXT,                        -- coordinator-clock receive time (never node clock)
  first_joined  TEXT NOT NULL DEFAULT (datetime('now')),
  hw_summary_json TEXT                       -- last heartbeat payload (free VRAM, gpus, engines)
);
```

Notes:

- Aggregated inventory is **not** persisted into the coordinator's `models` table — that
  table means "artifacts on *this* host" and muddying it would leak cluster assumptions
  into the single-box model (the exact thing ADR-007's reasoning protects). The aggregate
  view is an in-memory cache keyed `(node_id, model_id)`, rebuilt on demand; the API
  exposes it with explicit `node_id`/`node_name` fields.
- Placement decisions are logged (structured log + optionally a small append-only
  `placements` audit table in P7.3 if debugging demands it — decision deferred, default
  no).
- Jobs routed to nodes (P7.4) keep their durable record on the **owning node's** `jobs`
  table (single source of truth per node); the coordinator keeps only a routing memo
  (job_id → node_id) so `GET /api/jobs/{id}` on the coordinator can proxy the read.

## 5b. API surface (coordinator role)

Cluster plane (new, coordinator only; all token-gated):

```
POST   /api/cluster/nodes          { node_id, name, base_url, version, hw_summary } -> upsert (join + heartbeat)
GET    /api/cluster/nodes          -> [ Node {id, name, base_url, status, last_seen, hw_summary} ]
GET    /api/cluster/nodes/{id}     -> Node detail (+ live probe option ?probe=1)
DELETE /api/cluster/nodes/{id}     -> remove from registry (does not touch the node itself)
```

Aggregated views (coordinator only; shapes reuse existing ModelSummary/hardware shapes
plus `node_id`+`node_name` fields):

```
GET  /api/cluster/models           -> [ ModelSummary + {node_id, node_name} ]   (fan-out, cached)
GET  /api/cluster/hardware         -> [ per-node hardware snapshot + node identity ]
GET  /v1/models                    -> coordinator mode: UNION of member nodes' /v1/models
                                      (deduped by model id; a model on 2 nodes appears once)
POST /v1/chat/completions          -> placement (§6.2) -> relay to owning node's /v1/chat/completions
POST /v1/completions               -> same
POST /api/jobs                     -> P7.4: place -> submit to owning node's /api/jobs -> mirror
```

Existing local endpoints (`/api/models`, `/api/hardware`, …) on the coordinator keep
their **local-only** meaning — the cluster aggregate lives under `/api/cluster/*` so no
existing client's semantics change silently. (The `/v1/*` surface is the deliberate
exception: a coordinator's OpenAI endpoint aggregating is the whole point, and single-node
behavior is the degenerate case of the same code.)

JSON-RPC mirrors (same one-core-per-surface discipline as `internal/jsonrpc`):
`cluster.nodes.list`, `cluster.nodes.remove`, `cluster.models.list`,
`cluster.hardware.get` — thin one-line calls into the coordinator core. Join/heartbeat
stays REST-only (it is machine-to-machine, not an operator surface).

WS fan-in: the coordinator maintains a client WS connection to each ONLINE node's
`/api/ws` and re-publishes received frames on its own `/api/ws` as
`{ "node_id": "...", "node_name": "...", "event": <original frame> }`, alongside its own
local frames (which carry its own node_id for uniformity). Reconnect with backoff;
a dropped node WS feeds the health state machine (§2.4). The dashboard learns a node
filter; per-node event ordering is preserved per connection, no cross-node ordering is
promised (none is needed — every event type is a refetch signal per api-spec.md).

---

## 6. Compat/estimator interplay and placement

### 6.1 One-core rule, per node

Verdicts remain a pure function of a node's own hardware, engines, and live free VRAM —
computed by that node's compat engine exactly as today. The coordinator:

- **never** imports estimator code paths for placement,
- **never** caches a verdict beyond a placement decision (verdicts embed *live* free
  VRAM; a stale cached verdict is precisely the false-`runs` the correctness bar bans),
- asks candidates at placement time: `GET {node}/api/models/{id}/compat?ctx=...` (model
  on disk) — cheap, no download, returns the full Verdict with reason.

The aggregated `/api/cluster/models` view may display each node's *cached*
`compat_status` (as the node itself caches it) for UX, clearly as advisory; placement
always re-asks live.

### 6.2 Placement policy (Decision 5)

Given a requested model, candidates = ONLINE nodes whose inventory HAS the model. Pick:

1. **Already-warm** (node reports a HEALTHY instance for the model): route there.
   Zero load latency, zero VRAM movement, and it exploits per-node LRU naturally. If
   several are warm: most free VRAM wins (headroom for the KV cache growth of this
   request), then round-robin.
2. **Verdict quality:** among cold candidates, live `runs` beats `runs_degraded`;
   `wont_run` is never placed (the coordinator returns the node reasons aggregated —
   mandatory-reason gate holds across the network).
3. **Most free VRAM** among equal verdicts: loads where it disturbs the least (fewest
   forced evictions on that node — conservative, matching the residency philosophy of
   preferring headroom over cleverness).
4. **Round-robin** as the final tiebreak, so equal boxes share warm-up load instead of
   node-list order permanently favoring one box.

Justification for warm-first over most-free-VRAM-first: swapping whole models is the most
expensive operation in the system (tens of seconds); the single-box scheduler already
treats warmth as the dominant term (`admit()` returns a warm instance before any
estimation), and the cluster policy should be the same philosophy lifted one level. A
free-VRAM-first policy would ping-pong loads across nodes and multiply cold starts.

Placement failure is never retried onto a `wont_run` node; if the chosen node's own
admission blocks at relay time (503 `admission_blocked`), v1 propagates that node's 503
(with its numeric reason, prefixed with the node name) rather than silently re-placing —
re-placement loops hide capacity problems the owner should see. Recorded as a v2
candidate (single re-placement attempt to the next candidate) after real-world feedback.

---

## 7. Failure modes

| Failure | Behavior (designed, not aspirational) |
|---|---|
| **Node unreachable mid-stream** | The coordinator's relay to the node breaks. Bytes already sent: client sees a truncated stream (same as an engine crash today); no bytes yet: 502 `upstream_unreachable` with node name in the reason — exactly `internal/proxy.RelayError` semantics, one level up. No transparent retry (Decision 8: silent re-inference is worse than a visible error). Node goes STALE→UNREACHABLE via missed heartbeats; new placements skip it. |
| **Coordinator dies / restarts** | **Nodes are unaffected**: local serving, residency, direct-to-node clients all keep working — nodes never depend on the coordinator for anything local (they just fail heartbeats and retry with backoff). On coordinator restart, the registry rebuilds within one heartbeat interval (~15s) from node re-registration; `nodes` rows persist identity so the UI shows known-but-not-yet-heartbeated boxes as UNREACHABLE rather than absent. In-flight coordinator-routed streams die with the process (clients retry; the generations on nodes finish or get cancelled by their engines on socket close — same as any proxy death). |
| **Clock skew between boxes** | Structurally irrelevant: all liveness is coordinator-clock `last_seen` at receive time; node-reported timestamps are display-only. No cross-node timestamp comparison exists anywhere in the design. Version skew is surfaced (`nodes.version` in the UI) with a documented same-minor-version support policy. |
| **Same model on 2+ nodes** | Placement policy §6.2 (warm > runs > free VRAM > round-robin). `/v1/models` dedupes by model id. |
| **Partial cluster (1 node)** | Fully functional degenerate case: coordinator + its own local node role = today's single box with extra registry rows. This is deliberately the P7.1 gate rig. |
| **Same-box dev cluster (2 daemons, 2 ports)** | The standing test rig (§8): two daemons on 127.0.0.1 with different ports, `db_path`s, `model_dirs`, and residency budgets. One physical GPU means NVML shows both daemons the same card — VRAM-truth tests that need *divergent* hardware use divergent **residency budgets** and **disjoint model_dirs** (inventory/placement divergence), not fake GPUs. Anything genuinely requiring two physical GPUs is explicitly deferred to the real-second-box checklist per gate. |
| **Node rejoins after config change (new base_url/IP)** | Upsert by `node_id` refreshes `base_url`; stale cached inventory for that node is invalidated on rejoin. |
| **Token mismatch** | Join/heartbeat gets 401 with the standard readable reason; node logs it loudly and retries with backoff (config typo is the #1 expected setup failure — the reason string must say which side rejected). |

---

## 8. Phased roadmap — P7.x

Sizing: **S** ≤ ~1 day-equivalent of focused agent work, **M** ≈ 2–4, **L** ≈ 5+.
Every gate below is provable on THIS box via the **two-daemon rig**: daemon A
(coordinator+node, port X) and daemon B (node, port Y), both 127.0.0.1, separate
`db_path`/`data_dir`, **disjoint `model_dirs`** (A: a subset dir with model α; B: a dir
with models β,γ), different residency budgets (e.g. A 12000 MiB, B 6000 MiB) to force
placement divergence on one physical GPU. Each phase lists what additionally needs a
**real second GPU box** (none attached today) — run those when one appears, recorded as
MANUAL(user) items in the TESTING.md style.

### P7.1 — Node identity, cluster config, join/heartbeat, coordinator skeleton (M)

Scope: `cluster` config block (`role`, `coordinator_url`, `token`, `heartbeat_seconds`,
node `name`/`base_url`) + validation rules (§4.2, §4.4); persisted node UUID; new
`internal/cluster` package (registry, health state machine); `nodes` migration;
`/api/cluster/nodes` join/list/detail/remove; node-side heartbeat loop; cluster-token
acceptance in middleware; JSON-RPC `cluster.nodes.*`.

DoD: two-daemon rig — B joins A, appears ONLINE with correct base_url/version; killing B
drives STALE→UNREACHABLE on schedule; restarting A rebuilds the registry from B's next
heartbeat without B restarting; `role: off` daemons are byte-identical to today (no new
routes, no loops — the regression gate).

Gate (TESTING style):
```bash
go vet ./... && go build ./... && go test ./...        # includes new registry/state-machine unit tests
# rig: start A (coordinator) + B (node); then:
curl -s A/api/cluster/nodes | jq '.[].status'           # B ONLINE
# kill B; poll until STALE then UNREACHABLE (timestamps in evidence)
# restart A; B re-listed ONLINE within one heartbeat interval, no B restart
# tokenless non-loopback cluster config -> daemon refuses to start with readable reason
```
Real-second-box additions: join over actual LAN and over tailnet; base_url reachability
from a different host; Windows firewall friction documented. MANUAL(user).

### P7.2 — Aggregated inventory, hardware, /v1/models (S–M)

Scope: coordinator fan-out client (with per-node timeouts + partial-result semantics:
an UNREACHABLE node yields a tagged gap, never a hang); `/api/cluster/models`,
`/api/cluster/hardware`; coordinator-mode `/v1/models` union+dedupe; inventory cache +
invalidation on rejoin; JSON-RPC mirrors; UI node column (minimal).

DoD: rig — `/api/cluster/models` shows α tagged node-A and β,γ tagged node-B;
`/v1/models` lists α,β,γ once each; killing B degrades the aggregate to A's slice with
B marked UNREACHABLE (partial result, not an error); node-local endpoints unchanged.

Gate:
```bash
curl -s A/api/cluster/models | jq 'group_by(.node_id) | map({n:.[0].node_name, c:length})'
curl -s A/v1/models | jq '.data | length'               # == distinct union count
# kill B -> aggregate returns A-only + B UNREACHABLE in /api/cluster/nodes; no request hangs > timeout
```
Real-second-box: aggregate latency over LAN; a genuinely different GPU model showing
distinct vram_total in /api/cluster/hardware. MANUAL(user).

### P7.3 — Placement + routed completions (M–L) — the money milestone

Scope: placement engine (§6.2) in `internal/cluster`; live per-candidate verdict asks;
coordinator `/v1/chat/completions` + `/v1/completions` relay to the owning node via
`internal/proxy.Relay`; error propagation (node 503 reasons prefixed with node name;
502 on unreachable); placement decision logging.

DoD: rig — a request for β (only on B) routes to B and streams back through A
byte-identically; a request for α (only on A) serves locally through the same code path;
a model present on both (add one shared model to both dirs for this test) prefers the
node with a warm instance, then most-free-budget; B's admission block propagates as a
503 with B's numeric reason through A; a request for a model on neither returns 404
`model_not_found` listing zero candidates in the reason.

Gate:
```bash
# β via A: response bytes (incl. SSE framing) identical to hitting B directly (diff of captures)
# warm-preference: warm β on B, place again -> B chosen (placement log line as evidence)
# block propagation: shrink B budget so β wont admit -> A returns 503 admission_blocked, reason contains "node-B" + MiB numbers
# never-kill-active x cluster: stream through A from B, request an evicting load on B directly -> B DRAINING protects the stream (existing gate, now exercised through the relay)
```
Real-second-box: streaming quality/latency over LAN; mid-stream network cut (pull the
cable — the truncated-stream contract §7); tokens/s through-coordinator vs direct
overhead measured. MANUAL(user).

### P7.4 — WS fan-in + jobs routing (M)

Scope: coordinator WS client per node with backoff (feeds health §2.4); re-publish
wrapped frames; coordinator `/api/jobs` place-and-forward + routing memo + proxied
`GET/DELETE /api/jobs/{id}`; `jobs.updated` fan-in drives job mirroring; dashboard node
filter.

DoD: rig — an instance start on B emits `instances.updated` tagged node-B on A's
`/api/ws`; a job submitted to A for β runs on B, terminal state + `result_json`
retrievable from A; job cancel through A cancels on B honestly (B's existing
cancellation semantics, relayed); coordinator restart mid-job: the job finishes on B
(node-durable) and A can still fetch it by id after rebuild.

Gate:
```bash
# ws capture on A shows {node_id: B, event: instances.updated} within 1s of B's local event
# job via A -> succeeded; GET A/api/jobs/{id} returns result_json == GET B/api/jobs/{id}
# restart A mid-run -> job unaffected on B; A proxies the read post-restart
```
Real-second-box: WS reconnect across a real network blip; event fan-in latency. MANUAL(user).

### P7.5 — Security hardening + docs (S–M)

Scope: enforce the full validation trio (clustered + non-loopback ⇒ token, both sides);
token rotation runbook; per-endpoint scoping decision for the cluster token (implement
the deny-list for supply-chain/admin endpoints, or explicitly record acceptance);
Tailscale cluster setup guide (examples/); threat-model section merged into
docs/security.md; version-skew policy in docs; negative tests (wrong token, no token,
token in URL never accepted, dev endpoints unreachable through every relay path).

DoD: every negative test red-teamed in the rig; docs updated (security.md,
api-spec.md pointer, examples/cluster-tailscale.md); a from-scratch two-box setup doc a
non-author can follow.

Gate:
```bash
go test ./internal/cluster/... ./internal/api/...       # incl. negative-auth table tests
# rig: curl without token to every /api/cluster/* and node cluster-plane path -> 401 with reason
# grep-audit: no relay path can reach /sleep|/wake_up|/collective_rpc|/reset_prefix_cache (extend the existing P6.1 allow-list test to the coordinator relay)
```
Real-second-box: full Tailscale-transport cluster with loopback binds on both boxes
(the recommended posture, actually exercised); phone → coordinator → remote node
completion. MANUAL(user).

### Effort summary

| Phase | Size | Existing code touched | New code |
|---|---|---|---|
| P7.1 | M | `internal/config/config.go` (cluster block + validation), `internal/api/server.go` (role-gated routes), `internal/api/middleware.go` (second token), `internal/db` migration, `cmd/localhub/main.go` (role wiring, heartbeat loop start) | `internal/cluster/` (registry, health, identity), `internal/api/cluster.go` (thin handlers) |
| P7.2 | S–M | `internal/api/proxy.go` (`/v1/models` coordinator branch), `internal/jsonrpc/dispatcher.go` (mirrors), web UI (node tags) | `internal/cluster/` (fan-out client, cache) |
| P7.3 | M–L | `internal/api/proxy.go` (route-or-local branch) — `internal/proxy.Relay` reused **unchanged** | `internal/cluster/placement.go` (+ property-style tests over policy ordering) |
| P7.4 | M | `internal/api/ws.go` (publish wrapped frames), `internal/api/jobs.go` (coordinator branch), web UI (node filter) | `internal/cluster/` (WS fan-in client, job router + memo) |
| P7.5 | S–M | `internal/config`, docs, examples | negative-test suites |

**Total: roughly one Phase-4-to-6-sized effort (≈ 2–3 weeks of the cadence that built
P4–P6), dominated by P7.3 and by rig-based integration testing.** The single biggest
de-risker already in hand: the relay, auth middleware, WS bus, and one-core layering were
built host-agnostic enough that the coordinator is mostly *composition*, not new
machinery.

---

## 9. ADR-008 draft — **to be appended to docs/decisions.md on review** (do not treat as accepted until it lands there)

> ## ADR-008 — LAN federation of autonomous daemons (supersedes ADR-007 in part)
>
> **Decision:** LocalHub gains an optional cluster mode: a set of fully autonomous
> LocalHub daemons on a LAN/tailnet, federated by ONE coordinator role (a config-selected
> mode of the same binary, hub-and-spoke) that adds a node registry (push join +
> heartbeat, soft state), aggregated inventory/hardware views, verdict-based placement
> (the coordinator ASKS each node for its own live verdict and free VRAM; it never
> computes fits itself), and OpenAI-request routing to the owning node via the existing
> host-agnostic relay. Per-node behavior — residency, admission, never-kill-active,
> NVML-verified VRAM truth, manifests, reasons — is untouched; a node with clustering off
> is byte-identical to 1.0.
>
> **Why:** Project-owner directive, 2026-07-08 (verbatim): "also in future we shall be
> able to run it in cluster, add support for it, we shall implement and plan for it,
> write down the plans first and then start implementing", clarified as "cluster means
> not necessarily k8s or anything like that simple over the network gpu clusters". This
> amends ADR-007's "multi-node is deferred until real demand" — the demand is the owner's.
> ADR-007's *reasoning* survives: LocalHub still refuses to become a worse GPUStack, so
> the federation is the thinnest layer that answers "which of my boxes can run this, and
> route my request there." Hub-and-spoke because at 2–4 owner-controlled boxes a single
> coordinator eliminates all consensus/split-brain machinery, keeps failure legible
> (coordinator down ⇒ nodes fully functional locally), and any box can be made
> coordinator by config.
>
> **Carried-forward non-goals (binding):** no k8s/container orchestration; no cross-node
> VRAM preemption or fair-share scheduling (ADR-001 applies a fortiori); no model weight
> transfer between nodes in v1 (placement routes only to nodes that have the model; a
> node acquires models via its own supply chain); no distributed state store (coordinator
> soft state rebuilt from heartbeats; only an additive `nodes` table); no cross-box
> tensor/pipeline parallelism; no multi-tenant auth.
>
> **Consequence:** first feature to leave localhost ⇒ security posture changes are
> explicit and validation-enforced: a distinct shared cluster bearer token mandatory
> whenever clustered on a non-loopback bind, Tailscale as the recommended transport
> (loopback binds preserved), mTLS/per-node tokens as the recorded upgrade path. Full
> design, placement policy, failure modes, and P7.x gates: docs/cluster-plan.md.
>
> **Status:** Proposed 2026-07-08 (owner-directed; pending owner review of
> docs/cluster-plan.md §0 checklist).

---

*Written 2026-07-08 as the X6 plan-first deliverable. Sources: docs/decisions.md,
docs/product-brief.md, docs/architecture.md, docs/api-spec.md, docs/security.md,
docs/data-model.md, docs/residency-scheduler.md + residency-priority-queue.md, and the
shipped code (`internal/api/server.go`, `middleware.go`, `proxy.go`, `instances.go`,
`internal/proxy/proxy.go`, `internal/config/config.go`) — all claims about current
behavior were verified against those files on this date. No external engine-capability
claims were introduced.*
