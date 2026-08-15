package residency

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// store.go persists queue rows to the `queue` table (0001 DDL + the 0005 additive columns). The
// in-memory Queue (queue.go) is the ORDERING authority; these rows are the DURABLE mirror the
// API (GET /api/queue), the UI, and a daemon restart read. The manager writes a row when a
// request is enqueued (WAITING + numeric blocked_reason), updates it to ADMITTED/DONE/REJECTED as
// the request progresses, and on restart the WAITING rows re-seed the in-memory queue.
//
// Status vocabulary (0001 DDL): WAITING | ADMITTED | BLOCKED | DONE | REJECTED. We use WAITING for
// a queued request, ADMITTED when it is later admitted, REJECTED for queue_full, DONE when it is
// cancelled or its instance finished.

// QueueStatus mirrors the queue.status enum.
type QueueStatus string

const (
	QueueWaiting  QueueStatus = "WAITING"
	QueueAdmitted QueueStatus = "ADMITTED"
	QueueBlocked  QueueStatus = "BLOCKED"
	QueueDone     QueueStatus = "DONE"
	QueueRejected QueueStatus = "REJECTED"
)

// QueueRow is the durable projection of one queue entry (for GET /api/queue). It carries the
// numbers the UI shows: class, footprint, blocked_reason, position, age.
type QueueRow struct {
	ID            string      `json:"id"`
	ModelID       string      `json:"model_id"`
	Engine        string      `json:"engine,omitempty"`
	PriorityClass int         `json:"priority_class"` // 0 interactive|1 normal|2 background
	Priority      int         `json:"-"`              // raw priority int (drain loop restores it verbatim)
	Status        QueueStatus `json:"status"`
	BlockedReason string      `json:"blocked_reason,omitempty"`
	FootprintMiB  int         `json:"footprint_mib,omitempty"`
	Seq           int64       `json:"seq,omitempty"`
	Position      int         `json:"position,omitempty"` // 1-based score-order position (WAITING only)
	CoalesceOf    string      `json:"coalesce_of,omitempty"`
	EnqueuedAt    time.Time   `json:"enqueued_at"`
	UpdatedAt     time.Time   `json:"updated_at,omitempty"`

	// Full start context (0006 migration) — the drain loop reads these to re-Admit + start a
	// queued request faithfully. Not surfaced in the API projection (internal to draining).
	ReqCtx   int  `json:"-"`
	ReqBatch int  `json:"-"`
	ReqPin   bool `json:"-"`

	// Profile is the rest of the persisted launch profile (0009 migration): independent KV pair,
	// slots, speculative mode + draft max, CPU-offload permission. Internal to draining/restore,
	// like the 0006 fields above, so the public /api/queue body is unchanged.
	Profile ProfileSpec `json:"-"`
}

// QueueStore is the SQLite-backed durable queue mirror.
type QueueStore struct {
	db *sql.DB
}

// NewQueueStore builds a QueueStore over the daemon's *sql.DB (the queue table is created by
// migration 0001 + extended by 0005; this constructor does not create it).
func NewQueueStore(db *sql.DB) *QueueStore { return &QueueStore{db: db} }

const qTimeLayout = "2006-01-02 15:04:05"

func qFmtTime(t time.Time) string { return t.UTC().Format(qTimeLayout) }

func qParseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, l := range []string{qTimeLayout, time.RFC3339, time.RFC3339Nano} {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// InsertWaiting persists a newly-enqueued request as a WAITING row with its numeric blocked_reason.
func (s *QueueStore) InsertWaiting(ctx context.Context, r Request, blockedReason string, footprintMiB int) error {
	if s == nil || s.db == nil {
		return nil // no-op when unconfigured (tests / queue-only mode)
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO queue
		    (id, model_id, engine, priority, status, blocked_reason, vram_bytes_est,
		     priority_class, seq, footprint_mib, req_ctx, req_batch, req_pin,
		     req_kv_type_k, req_kv_type_v, req_slots, req_spec_mode, req_spec_draft_max,
		     req_allow_cpu_offload, req_embedding, req_pooling, req_reranking,
		     enqueued_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.ModelID, nullString(r.Engine), r.Priority, string(QueueWaiting),
		nullString(blockedReason), nullZeroInt(footprintMiB),
		int(r.Class), r.Seq(), nullZeroInt(footprintMiB),
		nullZeroInt(r.Ctx), nullZeroInt(r.Batch), boolToIntQ(r.Pin),
		// Launch profile (0009): persisted so the drain loop replays the profile admission priced,
		// not a default-shaped guess (KNOWN-LIMITATIONS #21).
		nullString(r.Profile.KVTypeK), nullString(r.Profile.KVTypeV),
		nullZeroInt(r.Profile.Slots), nullString(r.Profile.SpecMode),
		nullZeroInt(r.Profile.SpecDraftMax), boolToIntQ(r.Profile.AllowCPUOffload),
		// Launch MODE (0013): --embedding/--pooling/--reranking. Dropping these on replay silently
		// turns a queued embedding/reranking start into a plain generation start (2026-08-06 bge-m3
		// crash-loop defect — the mode-field twin of the 0009 sizing-field bug).
		boolToIntQ(r.Profile.Embedding), nullString(r.Profile.Pooling), boolToIntQ(r.Profile.Reranking),
		qFmtTime(now), qFmtTime(now))
	if err != nil {
		return fmt.Errorf("queue insert %s: %w", r.ID, err)
	}
	return nil
}

// SetStatus updates a queue row's status (+ optional blocked_reason) and stamps updated_at. A
// terminal status also stamps finished_at.
func (s *QueueStore) SetStatus(ctx context.Context, id string, status QueueStatus, blockedReason string) error {
	if s == nil || s.db == nil {
		return nil
	}
	now := time.Now()
	set := "status = ?, updated_at = ?"
	args := []any{string(status), qFmtTime(now)}
	if blockedReason != "" {
		set += ", blocked_reason = ?"
		args = append(args, blockedReason)
	}
	if status == QueueAdmitted {
		set += ", started_at = ?"
		args = append(args, qFmtTime(now))
	}
	if status == QueueDone || status == QueueRejected {
		set += ", finished_at = ?"
		args = append(args, qFmtTime(now))
	}
	args = append(args, id)
	_, err := s.db.ExecContext(ctx, "UPDATE queue SET "+set+" WHERE id = ?", args...)
	if err != nil {
		return fmt.Errorf("queue set status %s=%s: %w", id, status, err)
	}
	return nil
}

// ListWaiting returns the WAITING rows in arrival (seq) order — the drain scan and the restart
// re-seed set.
func (s *QueueStore) ListWaiting(ctx context.Context) ([]QueueRow, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	return s.query(ctx, `WHERE status = 'WAITING' ORDER BY seq ASC, enqueued_at ASC`)
}

// get returns one queue row by id (for the drain loop's context fallback). Returns an error when
// the store is unconfigured or the row is absent.
func (s *QueueStore) get(ctx context.Context, id string) (QueueRow, error) {
	if s == nil || s.db == nil {
		return QueueRow{}, fmt.Errorf("queue store not configured")
	}
	rows, err := s.query(ctx, `WHERE id = ? LIMIT 1`, id)
	if err != nil {
		return QueueRow{}, err
	}
	if len(rows) == 0 {
		return QueueRow{}, fmt.Errorf("queue row %q not found", id)
	}
	return rows[0], nil
}

// List returns recent queue rows (WAITING first by seq, then recently finished) for GET /api/queue.
func (s *QueueStore) List(ctx context.Context, limit int) ([]QueueRow, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	return s.query(ctx, fmt.Sprintf(
		`ORDER BY (status = 'WAITING') DESC, seq ASC, enqueued_at DESC LIMIT %d`, limit))
}

func (s *QueueStore) query(ctx context.Context, tail string, args ...any) ([]QueueRow, error) {
	// Every 0009 column is COALESCEd to a zero value so rows written before the migration (and by
	// an older daemon) read back as "no advanced knobs" rather than failing the scan.
	const cols = `SELECT id, model_id, COALESCE(engine,''), COALESCE(priority_class,priority),
		COALESCE(priority,priority_class), status, COALESCE(blocked_reason,''),
		COALESCE(footprint_mib, COALESCE(vram_bytes_est,0)),
		COALESCE(seq,0), COALESCE(coalesce_of,''),
		COALESCE(req_ctx,0), COALESCE(req_batch,0), COALESCE(req_pin,0),
		COALESCE(req_kv_type_k,''), COALESCE(req_kv_type_v,''), COALESCE(req_slots,0),
		COALESCE(req_spec_mode,''), COALESCE(req_spec_draft_max,0), COALESCE(req_allow_cpu_offload,0),
		COALESCE(req_embedding,0), COALESCE(req_pooling,''), COALESCE(req_reranking,0),
		COALESCE(enqueued_at,''), COALESCE(updated_at,'')
		FROM queue `
	rows, err := s.db.QueryContext(ctx, cols+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("queue query: %w", err)
	}
	defer rows.Close()
	var out []QueueRow
	for rows.Next() {
		var r QueueRow
		var status, enq, upd string
		var reqPin, reqOffload, reqEmbedding, reqReranking int
		if err := rows.Scan(&r.ID, &r.ModelID, &r.Engine, &r.PriorityClass, &r.Priority,
			&status, &r.BlockedReason, &r.FootprintMiB, &r.Seq, &r.CoalesceOf,
			&r.ReqCtx, &r.ReqBatch, &reqPin,
			&r.Profile.KVTypeK, &r.Profile.KVTypeV, &r.Profile.Slots,
			&r.Profile.SpecMode, &r.Profile.SpecDraftMax, &reqOffload,
			&reqEmbedding, &r.Profile.Pooling, &reqReranking,
			&enq, &upd); err != nil {
			return nil, err
		}
		r.Status = QueueStatus(status)
		r.ReqPin = reqPin != 0
		r.Profile.AllowCPUOffload = reqOffload != 0
		// Launch MODE (0013): restore --embedding/--reranking alongside the sizing fields above, so a
		// replayed start rebuilds the SAME LaunchProfile it was queued with (KNOWN-LIMITATIONS #21's
		// mode-field twin — the 2026-08-06 bge-m3 crash-loop defect).
		r.Profile.Embedding = reqEmbedding != 0
		r.Profile.Reranking = reqReranking != 0
		r.EnqueuedAt = qParseTime(enq)
		r.UpdatedAt = qParseTime(upd)
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullZeroInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func boolToIntQ(b bool) int {
	if b {
		return 1
	}
	return 0
}
