-- 0008_nodes.sql
-- Additive migration (data-model.md: additive only; docs/cluster-plan.md §5 "additive only,
-- no destructive migration"). P7.1 cluster support (ADR-008). This adds two NEW tables and
-- touches nothing existing; a role=off daemon simply never writes to them.
--
-- Owner directive that scoped this feature (verbatim, 2026-07-08):
--   "cluster means not necessarily k8s or anything like that simple over the network gpu clusters"
--
-- `nodes` is the COORDINATOR's registry of the other daemons that have joined it. It is SOFT
-- STATE, not a source of truth: rows persist identity/naming so a restarted coordinator still
-- knows a box exists, but liveness (status/last_seen) is refreshed from heartbeats and is
-- advisory — the registry is rebuilt from node re-registration within one heartbeat interval
-- after a coordinator restart (cluster-plan §2.4, §7). last_seen is ALWAYS stamped with the
-- coordinator's own clock at receive time; node-reported timestamps are never used for liveness
-- (clock skew is structurally irrelevant, cluster-plan §7). status is derived at query time from
-- last_seen age; the stored column is just the last-computed cache. DDL kept verbatim from
-- cluster-plan §5.
CREATE TABLE nodes (
  id            TEXT PRIMARY KEY,            -- node-minted UUID, stable across restarts
  name          TEXT NOT NULL,               -- human label ("basement-4090")
  base_url      TEXT NOT NULL,               -- how the coordinator reaches the node
  version       TEXT,                        -- localhub version string (skew visibility)
  status        TEXT NOT NULL DEFAULT 'UNREACHABLE',  -- ONLINE|STALE|UNREACHABLE
  last_seen     TEXT,                        -- coordinator-clock receive time (never node clock)
  first_joined  TEXT NOT NULL DEFAULT (datetime('now')),
  hw_summary_json TEXT                       -- last heartbeat payload (free VRAM, gpus, engines); P7.2 fills it
);

-- `cluster_identity` persists THIS daemon's own stable node UUID in its own SQLite, so a restart
-- (or a box rename) does not re-identity it to the coordinator (cluster-plan §2.4: "a UUID minted
-- once and persisted in the node's own SQLite"). Single-row table (the CHECK pins it to one row).
-- Every daemon has one — a coordinator participates in placement as its own node too (§2.2), so the
-- identity is uniform across roles. A role=off daemon simply mints one lazily-or-never; it is inert.
CREATE TABLE cluster_identity (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),  -- exactly one row
  node_id   TEXT NOT NULL
);
