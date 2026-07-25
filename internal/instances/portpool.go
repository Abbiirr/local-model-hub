package instances

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"sync"
	"time"
)

// PortPool allocates TCP ports from a configured range for spawned engines (llama.cpp/vLLM:
// one port per model). Allocations are persisted to the port_pool table (migration 0004) so a
// daemon restart knows which ports were ours, and each candidate port is BIND-TESTED before
// use so a port already taken by another process (or a still-running orphan) is skipped.
//
// Freeing a port DELETEs its row (called on stop). The pool is the low port range default
// 40100-40200 (config engines.llamacpp.port_range).
//
// A mutex serializes allocation so two concurrent starts cannot bind-test the same port and
// both win the race; the persisted row is the durable record, the bind-test is the liveness
// check.
type PortPool struct {
	db     *sql.DB
	lo, hi int
	mu     sync.Mutex
	// tryListen is the bind-test seam (real net.Listen by default; a fake in tests). Returns a
	// closer to release the probe listener, or an error if the port is unavailable.
	tryListen func(port int) (func() error, error)
}

// NewPortPool builds a pool over [lo, hi] inclusive, persisted in db.port_pool. An invalid
// range (lo<=0 or hi<lo) is normalized to the reference default 40100-40200.
func NewPortPool(db *sql.DB, lo, hi int) *PortPool {
	if lo <= 0 || hi < lo {
		lo, hi = 40100, 40200
	}
	return &PortPool{db: db, lo: lo, hi: hi, tryListen: defaultTryListen}
}

// defaultTryListen bind-tests a port on 127.0.0.1 (the only interface LocalHub uses). Success
// returns a closer that releases the probe listener; the caller closes it immediately before
// the real spawn binds the same port (a tiny TOCTOU window, acceptable on a single-box daemon
// and closed in practice by the persisted allocation row + mutex).
func defaultTryListen(port int) (func() error, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	return ln.Close, nil
}

// Allocate reserves a free port for instanceID: it scans the range for a port that is neither
// already recorded in port_pool NOR currently bindable-refused (in use), persists the
// allocation, and returns the port. It errors if the whole range is exhausted.
func (pp *PortPool) Allocate(ctx context.Context, instanceID string) (int, error) {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	taken, err := pp.recordedPorts(ctx)
	if err != nil {
		return 0, fmt.Errorf("read port_pool: %w", err)
	}
	for port := pp.lo; port <= pp.hi; port++ {
		if taken[port] {
			continue
		}
		closer, terr := pp.tryListen(port)
		if terr != nil {
			// In use by something else (another process, an orphan we don't track). Skip.
			continue
		}
		// Release the probe listener right away so the real spawn can bind it, then record.
		_ = closer()
		if _, err := pp.db.ExecContext(ctx,
			`INSERT INTO port_pool (port, instance_id, allocated_at) VALUES (?, ?, ?)`,
			port, instanceID, time.Now().UTC().Format(timeLayout)); err != nil {
			// A concurrent daemon or a race recorded it first; try the next port.
			continue
		}
		return port, nil
	}
	return 0, fmt.Errorf("port pool exhausted: no free port in range %d-%d (all %d recorded or in use)",
		pp.lo, pp.hi, pp.hi-pp.lo+1)
}

// Free releases a port back to the pool (DELETE its row). Idempotent: freeing an unallocated
// port is a no-op. Called on stop.
func (pp *PortPool) Free(ctx context.Context, port int) error {
	if port == 0 {
		return nil
	}
	pp.mu.Lock()
	defer pp.mu.Unlock()
	_, err := pp.db.ExecContext(ctx, `DELETE FROM port_pool WHERE port = ?`, port)
	return err
}

// FreeByInstance releases whatever port an instance holds (used on reap/reconcile where we
// know the instance but read the port from its own row). Idempotent.
func (pp *PortPool) FreeByInstance(ctx context.Context, instanceID string) error {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	_, err := pp.db.ExecContext(ctx, `DELETE FROM port_pool WHERE instance_id = ?`, instanceID)
	return err
}

// recordedPorts returns the set of ports currently recorded as allocated.
func (pp *PortPool) recordedPorts(ctx context.Context) (map[int]bool, error) {
	rows, err := pp.db.QueryContext(ctx, `SELECT port FROM port_pool`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}
