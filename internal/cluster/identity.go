// Package cluster implements the P7.1 LAN-federation control plane (ADR-008,
// docs/cluster-plan.md): a coordinator-side node registry (soft state, rebuilt from
// heartbeats) and a node-side join/heartbeat client. It is strictly additive — a role=off
// daemon never constructs anything here, so single-box 1.0 behavior is untouched
// (cluster-plan §1.3).
//
// Scope note: P7.1 is identity + registry + join/heartbeat ONLY. Aggregation (P7.2),
// placement (P7.3), routing, and WS fan-in (P7.4) are deliberately absent here.
package cluster

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// LoadOrCreateNodeID returns this daemon's stable node UUID, minting and persisting one on
// first call (cluster-plan §2.4: "a UUID minted once and persisted in the node's own SQLite").
// It survives restarts and box renames so the coordinator keys a node by identity, not address.
// Backed by the single-row cluster_identity table (migration 0008). Safe to call for any role;
// a coordinator uses its own id to participate in placement as a node too (§2.2).
func LoadOrCreateNodeID(ctx context.Context, db *sql.DB) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, `SELECT node_id FROM cluster_identity WHERE singleton = 1`).Scan(&id)
	switch {
	case err == nil:
		return id, nil
	case err == sql.ErrNoRows:
		// First boot for this daemon: mint once and persist. INSERT OR IGNORE so a concurrent
		// racer (there is only one writer conn, but be explicit) does not error; re-read after.
		newID := uuid.NewString()
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO cluster_identity (singleton, node_id) VALUES (1, ?)`, newID); err != nil {
			return "", fmt.Errorf("persist node identity: %w", err)
		}
		if err := db.QueryRowContext(ctx, `SELECT node_id FROM cluster_identity WHERE singleton = 1`).Scan(&id); err != nil {
			return "", fmt.Errorf("read back node identity: %w", err)
		}
		return id, nil
	default:
		return "", fmt.Errorf("read node identity: %w", err)
	}
}
