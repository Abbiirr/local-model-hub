package instances

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Store persists instances and their transitions to SQLite (the `instances`,
// `instance_transitions`, and `port_pool` tables from migrations 0001 + 0004). It is the ONLY
// writer of the instance state machine: every state change goes through Transition, which
// enforces legality (illegal → typed error, nothing persisted) and writes an audit row + the
// denormalized instances.state in one transaction.
//
// A process-wide mutex serializes writes. A single-box daemon has low instance churn (a
// handful of models), so a coarse lock is simpler and safer than per-row locking, and it makes
// the "read current state → check legality → write" sequence atomic against concurrent
// transitions on the same instance without relying on SQLite row locks.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// NewStore builds a Store over the daemon's *sql.DB. The tables are created by migrations
// 0001/0004; this constructor does not create them.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// timeLayout is the text form SQLite's datetime('now') writes (UTC, no zone).
const timeLayout = "2006-01-02 15:04:05"

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, l := range []string{timeLayout, time.RFC3339, time.RFC3339Nano} {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// CreateParams is the input to Create: the initial REQUESTED instance.
type CreateParams struct {
	ID         string
	ModelID    string
	Engine     Engine
	ModelPath  string
	BinaryPath string
	Ctx        int
	VRAMEstMiB int
	Pinned     bool
	Priority   int
	TTLSeconds int
	Reason     string // why this instance was requested (mandatory)
}

// Create inserts a new instance in REQUESTED state and records the first transition
// (from="" → REQUESTED). The reason is mandatory (every transition carries a why). It returns
// the created Instance.
func (s *Store) Create(ctx context.Context, p CreateParams) (Instance, error) {
	if p.ID == "" || p.ModelID == "" || p.Engine == "" {
		return Instance{}, fmt.Errorf("create instance: id, model_id, and engine are required")
	}
	if p.Reason == "" {
		return Instance{}, fmt.Errorf("create instance %s: a reason is required (every transition carries a why)", p.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Instance{}, err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO instances
		    (id, model_id, engine, state, model_path, binary_path, ctx, vram_bytes_est,
		     pinned, priority, ttl_seconds, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.ModelID, string(p.Engine), string(StateRequested),
		nullStr(p.ModelPath), nullStr(p.BinaryPath), nullInt(p.Ctx), nullInt(p.VRAMEstMiB),
		boolToInt(p.Pinned), p.Priority, nullInt(p.TTLSeconds), p.Reason, fmtTime(now))
	if err != nil {
		return Instance{}, fmt.Errorf("insert instance %s: %w", p.ID, err)
	}
	if err := insertTransition(ctx, tx, p.ID, "", StateRequested, p.Reason, now); err != nil {
		return Instance{}, err
	}
	if err := tx.Commit(); err != nil {
		return Instance{}, err
	}
	return Instance{
		ID: p.ID, ModelID: p.ModelID, Engine: p.Engine, State: StateRequested,
		ModelPath: p.ModelPath, BinaryPath: p.BinaryPath, Ctx: p.Ctx, VRAMEstMiB: p.VRAMEstMiB,
		Pinned: p.Pinned, Priority: p.Priority, TTLSeconds: p.TTLSeconds,
		Reason: p.Reason, CreatedAt: now,
	}, nil
}

// TransitionParams carries the state change plus any column updates that accompany it (e.g.
// PID+port+base_url on STARTING, observed VRAM on HEALTHY, freed VRAM on STOPPED).
type TransitionParams struct {
	To     State
	Reason string // mandatory

	// Optional column updates applied atomically with the transition. A nil pointer leaves the
	// column unchanged; a non-nil pointer sets it (including to zero).
	PID          *int
	Port         *int
	BaseURL      *string
	LaunchFlags  []string // sets launch_flags_json when non-nil
	VRAMObsMiB   *int
	VRAMFreedMiB *int
	LastUsedAt   *time.Time
}

// Transition moves an instance to a new state, enforcing legality. It reads the current state,
// checks CanTransition, and — only if legal — writes the new state + an audit row + any
// accompanying column updates, all in one transaction. An illegal transition returns a typed
// *IllegalTransitionError and persists NOTHING. A mandatory reason is enforced.
//
// State-derived timestamps are set automatically: HEALTHY stamps started_at (first time),
// STOPPED/FAILED stamp stopped_at.
func (s *Store) Transition(ctx context.Context, id string, p TransitionParams) (Instance, error) {
	if p.Reason == "" {
		return Instance{}, fmt.Errorf("transition instance %s → %s: a reason is required", id, p.To)
	}
	if !p.To.Valid() {
		return Instance{}, fmt.Errorf("transition instance %s: unknown target state %q", id, p.To)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Instance{}, err
	}
	defer func() { _ = tx.Rollback() }()

	cur, err := scanInstanceTx(ctx, tx, id)
	if err != nil {
		return Instance{}, err
	}
	if !CanTransition(cur.State, p.To) {
		// Typed programming-error. Persist nothing.
		return Instance{}, &IllegalTransitionError{InstanceID: id, From: cur.State, To: p.To}
	}

	now := time.Now()

	// Build the UPDATE dynamically so unset optional columns are left untouched.
	set := []string{"state = ?", "reason = ?"}
	args := []any{string(p.To), p.Reason}
	if p.PID != nil {
		set = append(set, "pid = ?")
		args = append(args, nullInt(*p.PID))
	}
	if p.Port != nil {
		set = append(set, "port = ?")
		args = append(args, nullInt(*p.Port))
	}
	if p.BaseURL != nil {
		set = append(set, "base_url = ?")
		args = append(args, nullStr(*p.BaseURL))
	}
	if p.LaunchFlags != nil {
		set = append(set, "launch_flags_json = ?")
		args = append(args, nullStr(launchFlagsJSON(p.LaunchFlags)))
	}
	if p.VRAMObsMiB != nil {
		set = append(set, "vram_bytes_obs = ?")
		args = append(args, nullInt(*p.VRAMObsMiB))
	}
	if p.VRAMFreedMiB != nil {
		set = append(set, "vram_freed_mib = ?")
		args = append(args, nullInt(*p.VRAMFreedMiB))
	}
	if p.LastUsedAt != nil {
		set = append(set, "last_used_at = ?")
		args = append(args, fmtTime(*p.LastUsedAt))
	}
	// State-derived timestamps.
	if p.To == StateHealthy && cur.StartedAt.IsZero() {
		set = append(set, "started_at = ?")
		args = append(args, fmtTime(now))
	}
	if p.To.terminal() {
		set = append(set, "stopped_at = ?")
		args = append(args, fmtTime(now))
	}

	q := "UPDATE instances SET " + joinComma(set) + " WHERE id = ?"
	args = append(args, id)
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return Instance{}, fmt.Errorf("update instance %s: %w", id, err)
	}
	if err := insertTransition(ctx, tx, id, cur.State, p.To, p.Reason, now); err != nil {
		return Instance{}, err
	}
	if err := tx.Commit(); err != nil {
		return Instance{}, err
	}
	return s.getLocked(ctx, id)
}

// SetObservedPeak records the observed VRAM peak on a HEALTHY instance WITHOUT a state change
// (the calibration write is separate from the state machine). It also touches last_used_at.
func (s *Store) SetObservedPeak(ctx context.Context, id string, peakMiB int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`UPDATE instances SET vram_bytes_obs = ? WHERE id = ?`, nullInt(peakMiB), id)
	return err
}

// TouchLastUsed stamps last_used_at on an instance WITHOUT a state change (the completion-proxy
// path, Phase 5 fix 3). Keeps the eviction LRU + Ollama recent-active window accurate. No-op on a
// missing row.
func (s *Store) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`UPDATE instances SET last_used_at = ? WHERE id = ?`, fmtTime(at), id)
	return err
}

// Get returns the current Instance by id.
func (s *Store) Get(ctx context.Context, id string) (Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(ctx, id)
}

func (s *Store) getLocked(ctx context.Context, id string) (Instance, error) {
	return scanInstanceDB(ctx, s.db, id)
}

// List returns instances, newest first. If activeOnly, terminal (STOPPED/FAILED) rows are
// excluded — the set the residency manager and reconciler care about.
func (s *Store) List(ctx context.Context, activeOnly bool) ([]Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := instanceSelect + ` ORDER BY created_at DESC, rowid DESC`
	if activeOnly {
		q = instanceSelect + ` WHERE state NOT IN ('STOPPED','FAILED') ORDER BY created_at DESC, rowid DESC`
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Instance
	for rows.Next() {
		inst, err := scanInstanceRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

// Transitions returns the audit history for an instance, oldest first.
func (s *Store) Transitions(ctx context.Context, id string) ([]Transition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx,
		`SELECT from_state, to_state, reason, at FROM instance_transitions WHERE instance_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transition
	for rows.Next() {
		var from sql.NullString
		var to, reason, at string
		if err := rows.Scan(&from, &to, &reason, &at); err != nil {
			return nil, err
		}
		out = append(out, Transition{
			InstanceID: id, From: State(from.String), To: State(to), Reason: reason, At: parseTime(at),
		})
	}
	return out, rows.Err()
}

// insertTransition writes one instance_transitions row. from may be "" (first row).
func insertTransition(ctx context.Context, tx *sql.Tx, id string, from, to State, reason string, at time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO instance_transitions (instance_id, from_state, to_state, reason, at) VALUES (?, ?, ?, ?, ?)`,
		id, nullStr(string(from)), string(to), reason, fmtTime(at))
	if err != nil {
		return fmt.Errorf("record transition %s→%s for %s: %w", from, to, id, err)
	}
	return nil
}

// instanceSelect is the column list shared by all instance reads (kept in sync with scanInto).
const instanceSelect = `SELECT id, model_id, engine, state,
	COALESCE(pid,0), COALESCE(port,0), COALESCE(base_url,''),
	COALESCE(binary_path,''), COALESCE(model_path,''), COALESCE(ctx,0),
	COALESCE(vram_bytes_est,0), COALESCE(vram_bytes_obs,0), COALESCE(vram_freed_mib,0),
	pinned, priority, COALESCE(ttl_seconds,0), COALESCE(launch_flags_json,''),
	COALESCE(reason,''), COALESCE(created_at,''), COALESCE(started_at,''),
	COALESCE(last_used_at,''), COALESCE(stopped_at,'')
	FROM instances`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanInto(sc rowScanner) (Instance, error) {
	var (
		inst                                               Instance
		engine, state                                      string
		baseURL, binaryPath, modelPath, launchJSON, reason string
		createdAt, startedAt, lastUsedAt, stoppedAt        string
		pinnedInt                                          int
	)
	err := sc.Scan(
		&inst.ID, &inst.ModelID, &engine, &state,
		&inst.PID, &inst.Port, &baseURL,
		&binaryPath, &modelPath, &inst.Ctx,
		&inst.VRAMEstMiB, &inst.VRAMObsMiB, &inst.VRAMFreedMiB,
		&pinnedInt, &inst.Priority, &inst.TTLSeconds, &launchJSON,
		&reason, &createdAt, &startedAt, &lastUsedAt, &stoppedAt,
	)
	if err != nil {
		return Instance{}, err
	}
	inst.Engine = Engine(engine)
	inst.State = normalizeState(state)
	inst.BaseURL = baseURL
	inst.BinaryPath = binaryPath
	inst.ModelPath = modelPath
	inst.LaunchFlags = parseLaunchFlags(launchJSON)
	inst.Reason = reason
	inst.Pinned = pinnedInt != 0
	inst.CreatedAt = parseTime(createdAt)
	inst.StartedAt = parseTime(startedAt)
	inst.LastUsedAt = parseTime(lastUsedAt)
	inst.StoppedAt = parseTime(stoppedAt)
	return inst, nil
}

func scanInstanceDB(ctx context.Context, db *sql.DB, id string) (Instance, error) {
	inst, err := scanInto(db.QueryRowContext(ctx, instanceSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Instance{}, fmt.Errorf("instance %q not found", id)
	}
	return inst, err
}

func scanInstanceTx(ctx context.Context, tx *sql.Tx, id string) (Instance, error) {
	inst, err := scanInto(tx.QueryRowContext(ctx, instanceSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Instance{}, fmt.Errorf("instance %q not found", id)
	}
	return inst, err
}

func scanInstanceRow(rows *sql.Rows) (Instance, error) { return scanInto(rows) }

// --- small SQL helpers ---

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
