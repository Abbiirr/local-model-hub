package cluster

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Node status values (cluster-plan §2.4). Derived from heartbeat age at query time; the stored
// `nodes.status` column is only the last-computed cache. These are the canonical strings the plan
// and the migration DDL (DEFAULT 'UNREACHABLE') use.
const (
	StatusOnline      = "ONLINE"      // heartbeat within 2x interval
	StatusStale       = "STALE"       // 2x..4x interval — still known, placement will deprioritize
	StatusUnreachable = "UNREACHABLE" // > 4x interval (or never heartbeated since coordinator restart)
)

// JoinRequest is the node→coordinator upsert payload (cluster-plan §5b). Join and heartbeat are
// the SAME idempotent call — one code path, so a coordinator restart needs no special node logic.
// HWSummary is opaque JSON the node reports (free VRAM, gpus, engines); P7.1 stores it verbatim and
// P7.2 aggregates it — the coordinator never parses it for a fit decision (ADR-008, one-core-per-node).
type JoinRequest struct {
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	Version   string `json:"version"`
	HWSummary string `json:"hw_summary,omitempty"`
}

// Node is a registry entry as returned to callers (GET /api/cluster/nodes). Status is derived
// live from LastSeen; FirstJoined/LastSeen are coordinator-clock timestamps.
type Node struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	BaseURL     string `json:"base_url"`
	Version     string `json:"version"`
	Status      string `json:"status"`
	LastSeen    string `json:"last_seen,omitempty"`
	FirstJoined string `json:"first_joined,omitempty"`
	HWSummary   string `json:"hw_summary,omitempty"`
}

// Registry is the coordinator-side node registry. It is DB-backed soft state: rows in the `nodes`
// table (migration 0008) persist identity across a coordinator restart, and liveness is refreshed
// from heartbeats and derived from LastSeen age (cluster-plan §2.4, §7). Backing the registry with
// the DB directly (rather than an in-memory map) is what makes the soft-state rebuild automatic —
// after a coordinator restart the persisted row is still there, and the node's next heartbeat
// refreshes last_seen back to ONLINE within one interval, with no special rebuild code.
//
// clock is injectable so the status-derivation state machine is testable without sleeps.
type Registry struct {
	db       *sql.DB
	interval time.Duration
	clock    func() time.Time
}

// NewRegistry builds a coordinator registry over db. interval is the node heartbeat cadence
// (cluster.heartbeat_interval_seconds) — the unit ONLINE/STALE/UNREACHABLE are measured in.
func NewRegistry(db *sql.DB, interval time.Duration) *Registry {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	return &Registry{db: db, interval: interval, clock: time.Now}
}

// Upsert records a join/heartbeat (cluster-plan §5b). Idempotent by node_id: a first call inserts
// (stamping first_joined), a repeat refreshes name/base_url/version/hw_summary and last_seen. Rejoin
// after a config change (new base_url/IP) is handled here — the row is keyed by the stable node_id,
// so the address is simply refreshed (cluster-plan §7 "node rejoins after config change"). last_seen
// is ALWAYS the coordinator's clock at receive time (never a node-reported timestamp), which is what
// makes clock skew structurally irrelevant. Returns the resulting Node with derived status.
func (r *Registry) Upsert(ctx context.Context, req JoinRequest) (Node, error) {
	if strings.TrimSpace(req.NodeID) == "" {
		return Node{}, fmt.Errorf("node_id is required")
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.BaseURL) == "" {
		return Node{}, fmt.Errorf("name and base_url are required")
	}
	now := r.clock().UTC().Format(time.RFC3339Nano)
	// ONLINE is the status a fresh heartbeat implies; it is also re-derived on read, so the stored
	// value is only ever a cache. ON CONFLICT preserves first_joined (the row's original insert time).
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO nodes (id, name, base_url, version, status, last_seen, first_joined, hw_summary_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name            = excluded.name,
			base_url        = excluded.base_url,
			version         = excluded.version,
			status          = excluded.status,
			last_seen       = excluded.last_seen,
			hw_summary_json = excluded.hw_summary_json`,
		req.NodeID, req.Name, req.BaseURL, nullable(req.Version), StatusOnline, now, now, nullable(req.HWSummary))
	if err != nil {
		return Node{}, fmt.Errorf("upsert node %s: %w", req.NodeID, err)
	}
	return r.Get(ctx, req.NodeID)
}

// Get returns a single node by id with its status derived live from last_seen.
func (r *Registry) Get(ctx context.Context, id string) (Node, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, name, base_url, version, last_seen, first_joined, hw_summary_json
		FROM nodes WHERE id = ?`, id)
	n, err := r.scanNode(row)
	if err == sql.ErrNoRows {
		return Node{}, ErrNodeNotFound
	}
	return n, err
}

// List returns all registered nodes, status derived live from heartbeat age, ordered by name.
func (r *Registry) List(ctx context.Context) ([]Node, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, base_url, version, last_seen, first_joined, hw_summary_json
		FROM nodes ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		n, err := r.scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Remove deletes a node from the registry (DELETE /api/cluster/nodes/{id}). It does NOT touch the
// node itself — it just forgets it (cluster-plan §2.4, §5b). A still-alive node will simply rejoin
// on its next heartbeat.
func (r *Registry) Remove(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("remove node %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNodeNotFound
	}
	return nil
}

// ErrNodeNotFound is returned when a node id is absent from the registry (mapped to 404 by the API).
var ErrNodeNotFound = fmt.Errorf("node not found")

// rowScanner is the shared surface of *sql.Row and *sql.Rows so scanNode serves Get and List.
type rowScanner interface {
	Scan(dest ...any) error
}

func (r *Registry) scanNode(sc rowScanner) (Node, error) {
	var n Node
	var version, lastSeen, firstJoined, hw sql.NullString
	if err := sc.Scan(&n.ID, &n.Name, &n.BaseURL, &version, &lastSeen, &firstJoined, &hw); err != nil {
		return Node{}, err
	}
	n.Version = version.String
	n.LastSeen = lastSeen.String
	n.FirstJoined = firstJoined.String
	n.HWSummary = hw.String
	n.Status = r.deriveStatus(lastSeen)
	return n, nil
}

// deriveStatus computes ONLINE/STALE/UNREACHABLE from the age of last_seen against the heartbeat
// interval (cluster-plan §2.4): ONLINE within 2x, STALE within 4x, else UNREACHABLE. A row with no
// last_seen (e.g. persisted identity the coordinator has not heard from since restarting) is
// UNREACHABLE until the next heartbeat — the soft-state "known but not yet live" case (§7). A
// last_seen that cannot be parsed is treated as UNREACHABLE (fail-safe: never report a bad clock as
// live).
func (r *Registry) deriveStatus(lastSeen sql.NullString) string {
	if !lastSeen.Valid || lastSeen.String == "" {
		return StatusUnreachable
	}
	t, err := time.Parse(time.RFC3339Nano, lastSeen.String)
	if err != nil {
		return StatusUnreachable
	}
	age := r.clock().UTC().Sub(t)
	switch {
	case age <= 2*r.interval:
		return StatusOnline
	case age <= 4*r.interval:
		return StatusStale
	default:
		return StatusUnreachable
	}
}

// nullable maps an empty string to a SQL NULL so optional columns store NULL rather than "".
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
