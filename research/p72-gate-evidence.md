# P7.2 Gate Evidence — aggregated inventory / hardware / union-deduped /v1/models

Phase P7.2 (ADR-008, docs/cluster-plan.md §2.5/§5b/§8 P7.2). Gate: TESTING.md#P72. Captured 2026-07-08
on the reference box (Windows 10). **GPU present but NO model loads / no engines** — cluster
aggregation machinery only (engines/residency/jobs disabled in both rig configs). Two same-box daemons
of the ONE `localhub` binary:

- **Coordinator** on `:39921`, scratch DB `coord.db`, model_dir = an **empty** scratch dir (`coord-models`).
- **Node** on `:39922`, scratch DB `node.db`, model_dir = `K:/projects/llm_models/models` (11 real GGUFs).

Shared `cluster_token: p72-shared-token`, `heartbeat_interval_seconds: 3` (ONLINE ≤ 6s, STALE ≤ 12s,
UNREACHABLE > 12s). The disjoint model_dirs prove attribution: the coordinator's own inventory is empty,
the node's has 11 models — so a correctly-attributed aggregate shows exactly which box owns what.

Verdict: **PASS** — every P72 criterion demonstrated live.

| # | Criterion (TESTING.md#P72 / cluster-plan §8 P7.2) | Result |
|---|---|---|
| 1 | `/api/cluster/models` fans out to all nodes + self, attributed by {node_id, node_name} | PASS |
| 2 | `/api/cluster/hardware` fans out with per-node attribution | PASS |
| 3 | Coordinator's OWN local data included as a node entry WITHOUT an HTTP round-trip (it is also a node, §2.2) | PASS |
| 4 | `/api/cluster/v1/models` = union-deduped OpenAI list (dedup by id; OpenAI shape pure) | PASS |
| 5 | Node heartbeat carries a compact `hw_summary` (free/total VRAM, GPU name, free RAM); coordinator stores + returns it verbatim | PASS |
| 6 | Kill node mid-run → partial result: node marked with an EXPLICIT error entry within the timeout, coordinator unaffected, no hang | PASS |
| 7 | After >4× interval the node is UNREACHABLE → tagged gap with NO pointless HTTP wait (245 ms) | PASS |
| 8 | Teardown: both daemons killed, no orphans, ports free | PASS |

---

## Startup — both roles up, node joins, hw_summary flowing (criteria 3, 5)

Coordinator (empty dir → 0 models) and node (11 models scanned):
```
coord: msg="cluster coordinator role enabled" node_id=6681b12c-9cfe-42c1-a993-1e247212bb71 node_name=coord-a heartbeat_interval=3s
coord: msg="initial inventory scan complete" ... model_count=0
coord: msg="http request" method=POST path=/api/cluster/nodes/join status=200
node:  msg="cluster node role enabled" node_id=d5170d37-c912-4204-87a1-edc195e4029c node_name=node-b coordinator=http://127.0.0.1:39921 base_url=http://127.0.0.1:39922
node:  msg="cluster node heartbeat starting" ... interval=3s
node:  msg="initial inventory scan complete" inserted=11 ... model_count=11
```

`GET /api/cluster/nodes` — node-b ONLINE, hw_summary populated from the node's own probe (verbatim,
coordinator never parsed it beyond passthrough):
```json
[
  {
    "name": "node-b",
    "base_url": "http://127.0.0.1:39922",
    "status": "ONLINE",
    "hw_summary": "{\"gpus\":[{\"name\":\"NVIDIA GeForce RTX 4060 Ti\",\"vram_free_mib\":15266,\"vram_total_mib\":16380}],\"ram_free_mib\":8938,\"ram_total_mib\":16331,\"captured_at\":\"2026-07-08T04:11:03Z\"}"
  }
]
```

## Aggregated inventory — attribution correct (criteria 1, 3)

`GET /api/cluster/models` — self entry (coord-a, 0 models, no HTTP) + node-b (11 models via fan-out):
```json
[
  { "node_name": "coord-a", "node_id": "6681b12c", "model_count": 0,  "error": "" },
  { "node_name": "node-b",  "node_id": "d5170d37", "model_count": 11, "error": "" }
]
```
node-b first 5 model display names (proves the node's real inventory rode through attributed):
```
['gemma4-v2-Q4_K_M', 'GLM-OCR-Q8_0', 'Llama-3.2-1B-Instruct-Q4_K_M', 'nomic-embed-text-v2-moe.Q4_K_M', 'PaddleOCR-VL-1.5']
```

## Aggregated hardware — per-node attribution (criterion 2)

`GET /api/cluster/hardware` — both nodes present (same physical GPU on this one box; attribution, not
divergence, is what P7.2 proves — divergent GPUs are the MANUAL(user) real-second-box item):
```json
[
  { "node_name": "coord-a", "gpu": "NVIDIA GeForce RTX 4060 Ti", "error": "" },
  { "node_name": "node-b",  "gpu": "NVIDIA GeForce RTX 4060 Ti", "error": "" }
]
```

## Union-deduped /v1/models (criterion 4)

`GET /api/cluster/v1/models` — union of coord (0 present) + node-b (11) = 11 unique ids, OpenAI shape pure:
```
object= list count= 11
ids[:5]= ['gguf-glm-ocr-glm-ocr-q8-0', 'gguf-llama-3-2-1b-instruct-...', 'gguf-nomic-embed-text-v2-moe-...', 'gguf-paddleocr-vl-1-5-...', 'gguf-qwen3-5-27b-tq3-1s']
```

## Kill node mid-run → partial result, coordinator unaffected (criterion 6)

Node process on `:39922` killed. Node still ONLINE (heartbeat age < 6s) so the fan-out HTTP is refused
FAST → an explicit per-node error entry; the coordinator keeps serving; `/v1/models` degrades to the
coord-only slice with no hang:
```json
[
  { "node_name": "coord-a", "model_count": 0, "error": "" },
  { "node_name": "node-b",  "model_count": 0, "error": "GET http://127.0.0.1:39922/api/models: Get \"http://127.0.0.1:39922/api/models\": dial tcp 1..." }
]
```
```
$ curl -s http://127.0.0.1:39921/api/health
{"status":"ok","version":"0.1.0-dev","db_ok":true}          # coordinator unaffected
$ curl -s http://127.0.0.1:39921/api/cluster/v1/models | jq '.data|length'
0                                                            # union degrades, no hang
```

## After >4× interval → UNREACHABLE tagged gap, no HTTP wait (criterion 7)

```
$ curl -s .../api/cluster/nodes    -> [{"name":"node-b","status":"UNREACHABLE"}]
$ curl -s .../api/cluster/models:
[
  { "node_name": "coord-a", "error": "" },
  { "node_name": "node-b",  "error": "node \"node-b\" is UNREACHABLE (no heartbeat since 2026-07-08T04:11:35.3..." }
]
elapsed_ms=245        # the UNREACHABLE node is skipped WITHOUT a pointless per-node timeout wait
```

## Teardown — no orphans (criterion 8)

```
=== orphan check (localhub.exe processes) ===
NO_LOCALHUB_PROCESSES
=== ports free ===
PORTS_FREE
```

## Unit coverage backing the gate

```
go test ./internal/cluster/... ./internal/jsonrpc/... ./internal/api/... -count=1   # all ok
```
- `internal/cluster/aggregate_test.go`:
  - `TestAggregatorFanoutPartial` — 2 live fake nodes + 1 dead → self + both + explicit error entry, no hang.
  - `TestAggregatorTimeoutPath` — a node that sleeps 5s with a 200ms per-node timeout errors within the bound (aggregate < 3s).
  - `TestAggregatorV1ModelsUnionDedupe` — a model on 2 nodes appears once; failed node omitted from the union.
  - `TestHWSummaryRoundTrip` — HWSummary marshals, rides JoinRequest.HWSummary, stored verbatim, reads back byte-identical + re-parseable.
- `internal/api/cluster_test.go`:
  - `TestClusterRoleOff_NoRoutes` (extended) — role=off exposes NONE of `/api/cluster/{nodes,models,hardware,v1/models}`.
  - `TestClusterRoleNode_NoCoordinatorRoutes` — role=node exposes no coordinator routes and no cluster.* RPC mirrors.
  - `TestClusterAggregatedModelsSelfEntry` — with no registered nodes the aggregate still returns exactly the coordinator's own self entry.
- `internal/jsonrpc/cluster_test.go`:
  - `TestClusterMirrorsRegisteredOnlyWhenWired` — `cluster.nodes.list/models.list/hardware.get` registered only when a ClusterService is wired (coordinator); absent on the default path (role off/node). The `TestMethodCoverageMirrorsRESTCore` exact-set gate stays green.

## /v1/models decision followed (deviation from cluster-plan §5b, recorded)

The task note and cluster-plan §5b conflict: §5b lists the coordinator's own **`/v1/models`** as the
union. **Followed the task decision** (and it is forced by the P7.2 territory that keeps
`internal/api/proxy.go` untouched): the coordinator's own `/v1/models` stays **local-only** in P7.2; the
union-deduped list is served on the NEW path **`/api/cluster/v1/models`**. Rationale (in code at
`handleClusterV1Models`): P7.2 ships NO routing, so a unioned `/v1/models` would advertise models the
coordinator cannot yet serve (a request for a node-B-only model would hit the coordinator's LOCAL
resolver and 404). P7.3 wires routed completions; the union is promoted to `/v1/models` THEN, when the
coordinator can serve every advertised model. The plan's INTENT (a union OpenAI list at the coordinator)
is met today without the premature-advertisement correctness bug.

## Real-second-box additions (deferred, MANUAL(user), cluster-plan §8 P7.2)

Aggregate latency over LAN; a genuinely different GPU model showing distinct `vram_total` in
`/api/cluster/hardware`. Not exercisable on this single box (both daemons see the same physical GPU).
