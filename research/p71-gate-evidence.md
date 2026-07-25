# P7.1 Gate Evidence — cluster join/heartbeat, soft-state registry

Phase P7.1 (ADR-008, docs/cluster-plan.md). Gate: TESTING.md#P71. Captured 2026-07-08 on the
reference box (Windows 10). **No GPU / no model loads** — cluster machinery only. Two same-box
daemons of the ONE `localhub` binary: a coordinator on :39921 (scratch db `coord.db`) and a node
on :39922 (scratch db `node.db`), shared `cluster_token: shared-secret-token`,
`heartbeat_interval_seconds: 3` (so ONLINE ≤ 6s, STALE ≤ 12s, UNREACHABLE > 12s of heartbeat age).
Engines/residency/jobs disabled in the rig configs (cluster plane in isolation).

Verdict: **PASS** — every P71 criterion demonstrated live.

| # | Criterion (TESTING.md#P71 / cluster-plan §8) | Result |
|---|---|---|
| 1 | Node joins coordinator via one idempotent upsert (push join + heartbeat) | PASS |
| 2 | `GET /api/cluster/nodes` shows the node ONLINE with `last_seen` + correct `base_url`/`version` | PASS |
| 3 | Kill node → status degrades ONLINE→STALE→UNREACHABLE on schedule | PASS |
| 4 | Restart coordinator → registry rebuilt from node's next heartbeat (soft state), node NOT restarted | PASS |
| 5 | Wrong/missing cluster token on join → 401 with reason | PASS |
| 6 | Coordinator down does not affect the node's local serving | PASS |
| 7 | Node UUID stable across node restart | PASS |
| 8 | Teardown: both daemons killed, no orphans | PASS |

---

## Coordinator startup + empty registry (before node joins)

```
level=INFO msg="cluster coordinator role enabled" node_id=4b118afb-a522-4648-be57-38f2b3f82521 node_name=coord-a heartbeat_interval=3s
level=INFO msg="http server listening" addr=127.0.0.1:39921

$ curl -s http://127.0.0.1:39921/api/health
{"status":"ok","version":"0.1.0-dev","db_ok":true}

$ curl -s http://127.0.0.1:39921/api/cluster/nodes
[]
```

## Node joins → ONLINE (criteria 1, 2)

Node log (started once):
```
level=INFO msg="cluster node role enabled" node_id=e1742bf7-922c-4351-a959-465a38453735 node_name=node-b coordinator=http://127.0.0.1:39921 base_url=http://127.0.0.1:39922 heartbeat_interval=3s
level=INFO msg="cluster node heartbeat starting" coordinator=http://127.0.0.1:39921 node_id=e1742bf7-922c-4351-a959-465a38453735 name=node-b base_url=http://127.0.0.1:39922 interval=3s
```

`GET /api/cluster/nodes` on the coordinator (bearer = shared cluster token):
```json
[
  {
    "id": "e1742bf7-922c-4351-a959-465a38453735",
    "name": "node-b",
    "base_url": "http://127.0.0.1:39922",
    "version": "0.1.0-dev",
    "status": "ONLINE",
    "last_seen": "2026-07-08T03:30:37.7635227Z",
    "first_joined": "2026-07-08T03:30:13.5287312Z"
  }
]
```

## Wrong / missing token on join → 401 with reason (criterion 5)

```
=== join with WRONG token -> expect 401 ===
HTTP 401
{
  "error": {
    "code": "unauthorized",
    "message": "missing or invalid cluster token",
    "reason": "POST /api/cluster/nodes/join requires Authorization: Bearer <cluster.cluster_token>; the coordinator rejected the presented token (check cluster_token matches on both boxes)"
  }
}
=== join with MISSING token -> expect 401 ===
HTTP 401
```

## Kill node → status degrades on schedule (criterion 3)

Node process on :39922 killed (`taskkill /PID 23788 /F`). Coordinator polled every 2s; `last_seen`
freezes (no more heartbeats arrive) and the derived status walks ONLINE→STALE→UNREACHABLE exactly at
the 2×/4× interval thresholds:
```
t=+2s   status=ONLINE       last_seen=2026-07-08T03:31:16.986554Z
t=+4s   status=ONLINE       last_seen=2026-07-08T03:31:16.986554Z
t=+6s   status=STALE        last_seen=2026-07-08T03:31:16.986554Z
t=+8s   status=STALE        last_seen=2026-07-08T03:31:16.986554Z
t=+10s  status=UNREACHABLE  last_seen=2026-07-08T03:31:16.986554Z
t=+12s  status=UNREACHABLE  last_seen=2026-07-08T03:31:16.986554Z
```

## Node UUID stable across restart (criterion 7) + coordinator down doesn't stop local serving (criterion 6)

Node restarted; SAME `node_id` (e1742bf7-…) re-appears ONLINE — identity persisted in the node's own
SQLite (`cluster_identity` table):
```json
{ "id": "e1742bf7-922c-4351-a959-465a38453735", "status": "ONLINE", "last_seen": "2026-07-08T03:32:05.3912622Z" }
```
Coordinator killed (`taskkill /PID 23924 /F`); the node keeps serving its own API:
```
$ curl -s http://127.0.0.1:39922/api/health
{"status":"ok","version":"0.1.0-dev","db_ok":true}
```

## Restart coordinator → soft-state rebuild from heartbeats (criterion 4)

Coordinator restarted (fresh process, same `coord.db`). Node was NOT restarted (its log shows a
single "heartbeat starting"). Within one heartbeat interval the node is ONLINE again and `last_seen`
advances (new heartbeats: 03:33:31 → 03:33:34 → 03:33:37) — the registry rebuilt purely from the
node's re-registration:
```
level=INFO msg="cluster coordinator role enabled" node_id=4b118afb-... heartbeat_interval=3s
level=INFO msg="http server listening" addr=127.0.0.1:39921

t=+2s  {"id":"e1742bf7-...","status":"ONLINE","last_seen":"2026-07-08T03:33:31.684445Z"}
t=+4s  {"id":"e1742bf7-...","status":"ONLINE","last_seen":"2026-07-08T03:33:31.684445Z"}
t=+6s  {"id":"e1742bf7-...","status":"ONLINE","last_seen":"2026-07-08T03:33:34.6951421Z"}
t=+8s  {"id":"e1742bf7-...","status":"ONLINE","last_seen":"2026-07-08T03:33:37.6988902Z"}
```

## Teardown — no orphans (criterion 8)

```
=== TEARDOWN: kill both daemons ===
SUCCESS: The process with PID 19736 has been terminated.
SUCCESS: The process with PID 7024 has been terminated.
=== orphan check (should be empty) ===
NO_ORPHANS
NO_LOCALHUB_PROCESSES
```

## Role=off regression (unit)

`internal/api/cluster_test.go::TestClusterRoleOff_NoRoutes`: a role-off Server registers NO
`/api/cluster/*` route — GET/POST/DELETE all fall through to the `/api/` catch-all returning the
standard `not_found` JSON envelope. `TestClusterConfigDefaultOff` pins `config.Default().Cluster.Role
== "off"`. Both pass in `go test ./internal/api/...` / `./internal/config/...`.

## Real-second-box additions (deferred, MANUAL(user), per cluster-plan §8 P7.1)

Join over actual LAN and over tailnet; `base_url`/`advertise_url` reachability from a different host;
Windows firewall friction. Not exercisable on this single box.
