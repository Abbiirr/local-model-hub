package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/localhub/localhub/internal/proxy"
)

// jobs.go is P6.2: async inference jobs (design §2, PLAN.md decision 7b + conflict-resolution 10).
// Submit an OpenAI request, get a job id back IMMEDIATELY, poll or receive the result over
// WebSocket, cancel it, and survive a daemon restart (the lmwrapper restart lesson: queued jobs
// MUST persist — the `jobs` table makes them durable, migration 0007).
//
// A job runs through the SAME residency admission as the proxy and the instance API (StartInstance
// → Admit; the one-core rule + ADR-001). The worker is single-flighted (one job admitted at a time,
// matching the single-box safety posture and the residency lock). All business logic lives here;
// the REST handlers (internal/api/jobs.go) stay thin.
//
// State machine (mirrors the migration comment):
//   queued    -> accepted, waiting for admission/VRAM (durable; survives restart)
//   running   -> admitted, relaying to the engine (non-streaming; one final result)
//   succeeded -> engine returned a complete result (result_json holds the OpenAI response)
//   failed    -> engine/admission error, or "daemon restarted mid-inference" on reconcile
//   cancelled -> user cancelled (mid-queue removal or mid-inference server-side abort)

// Job states (string constants matching the migration DEFAULT + CHECK-free text column).
const (
	JobStateQueued    = "queued"
	JobStateRunning   = "running"
	JobStateSucceeded = "succeeded"
	JobStateFailed    = "failed"
	JobStateCancelled = "cancelled"
)

// JobSubmit is the input to SubmitJob (POST /api/jobs). RequestJSON is the OpenAI chat/completions
// (or completions) request body verbatim; Model is read from it by the handler (or supplied). The
// job returns the FINAL result — the worker forces stream:false upstream (design §2.3).
type JobSubmit struct {
	// Model is the requested model slug (resolved like the proxy). Required.
	Model string
	// RequestJSON is the OpenAI request body as submitted (stored verbatim so a restart re-runs the
	// exact request). Required.
	RequestJSON []byte
	// Priority is 0 interactive | 1 normal | 2 background (design §2.7). Default 1 when omitted;
	// NOT auto-derived from the endpoint (kept explicit).
	Priority int
	// Endpoint selects which OpenAI path the worker relays to: "chat" → /v1/chat/completions,
	// "completions" → /v1/completions. Default "chat".
	Endpoint string
}

// JobFilter is the input to ListJobs (GET /api/jobs?state=&model=&limit=).
type JobFilter struct {
	State string // optional exact state match
	Model string // optional exact model_id match
	Limit int    // optional cap (<=0 → default 100)
}

// JobView is the API projection of a `jobs` row (design §2.2). ResultJSON is populated ONLY by
// GetJob (never inlined into the list view — keeps list responses small, design §2.2).
type JobView struct {
	ID         string          `json:"id"`
	ModelID    string          `json:"model_id"`
	Engine     string          `json:"engine,omitempty"`
	State      string          `json:"state"`
	Priority   int             `json:"priority"`
	Endpoint   string          `json:"endpoint,omitempty"`
	ResultJSON json.RawMessage `json:"result_json,omitempty"` // present only on GetJob for a succeeded job
	Error      string          `json:"error,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	QueueID    string          `json:"queue_id,omitempty"`
	InstanceID string          `json:"instance_id,omitempty"`
	CreatedAt  string          `json:"created_at,omitempty"`
	StartedAt  string          `json:"started_at,omitempty"`
	FinishedAt string          `json:"finished_at,omitempty"`
}

// jobWorkerState holds the worker's wake signal + the per-running-job cancel funcs. The cancel map
// lets CancelJob abort an in-flight relay server-side (design §2.5): the worker registers a
// context.CancelFunc keyed by job id while running; CancelJob looks it up and calls it.
type jobWorkerState struct {
	wake      chan struct{}
	mu        sync.Mutex
	cancelFns map[string]context.CancelFunc
	once      sync.Once
}

func (s *Service) jobs() *jobWorkerState {
	s.jobStateOnce.Do(func() {
		s.jobState = &jobWorkerState{
			wake:      make(chan struct{}, 1),
			cancelFns: make(map[string]context.CancelFunc),
		}
	})
	return s.jobState
}

// signalWake nudges the worker to scan now (non-blocking; a full buffer means a scan is already
// pending, which is what we want).
func (js *jobWorkerState) signalWake() {
	select {
	case js.wake <- struct{}{}:
	default:
	}
}

func (js *jobWorkerState) registerCancel(id string, cancel context.CancelFunc) {
	js.mu.Lock()
	js.cancelFns[id] = cancel
	js.mu.Unlock()
}

func (js *jobWorkerState) unregisterCancel(id string) {
	js.mu.Lock()
	delete(js.cancelFns, id)
	js.mu.Unlock()
}

// takeCancel looks up and removes the cancel func for a job (so a double-cancel is a no-op).
func (js *jobWorkerState) takeCancel(id string) (context.CancelFunc, bool) {
	js.mu.Lock()
	defer js.mu.Unlock()
	fn, ok := js.cancelFns[id]
	if ok {
		delete(js.cancelFns, id)
	}
	return fn, ok
}

// SubmitJob inserts a durable `queued` row and returns its id IMMEDIATELY (the whole point — no
// waiting for inference, design §2.2). It wakes the worker so a scan happens promptly.
func (s *Service) SubmitJob(ctx context.Context, req JobSubmit) (JobView, error) {
	if s.deps.DB == nil {
		return JobView{}, fmt.Errorf("submit job: database not configured")
	}
	if strings.TrimSpace(req.Model) == "" {
		return JobView{}, fmt.Errorf("submit job: model is required")
	}
	if len(req.RequestJSON) == 0 {
		return JobView{}, fmt.Errorf("submit job: request body is required")
	}
	priority := req.Priority
	if priority < 0 {
		priority = 0
	}
	if priority > 2 {
		priority = 2
	}
	endpoint := req.Endpoint
	if endpoint != "completions" {
		endpoint = "chat"
	}

	id := uuid.NewString()
	_, err := s.deps.DB.ExecContext(ctx,
		`INSERT INTO jobs (id, model_id, request_json, priority, state, endpoint)
		 VALUES (?, ?, ?, ?, 'queued', ?)`,
		id, req.Model, string(req.RequestJSON), priority, endpoint)
	if err != nil {
		return JobView{}, fmt.Errorf("submit job: insert: %w", err)
	}

	view, gerr := s.getJobRow(ctx, id, false)
	if gerr != nil {
		return JobView{}, gerr
	}
	s.publishJobEvent(id, JobStateQueued, req.Model, "")
	s.jobs().signalWake()
	return view, nil
}

// GetJob returns one job by id INCLUDING result_json when present (GET /api/jobs/{id}, design §2.2).
func (s *Service) GetJob(ctx context.Context, id string) (JobView, error) {
	if s.deps.DB == nil {
		return JobView{}, fmt.Errorf("get job: database not configured")
	}
	return s.getJobRow(ctx, id, true)
}

// ListJobs returns a filterable list of jobs (GET /api/jobs?state=&model=&limit=, design §2.2).
// result_json is NEVER inlined into the list (keep responses small; fetch it via GetJob).
func (s *Service) ListJobs(ctx context.Context, filter JobFilter) ([]JobView, error) {
	if s.deps.DB == nil {
		return nil, fmt.Errorf("list jobs: database not configured")
	}
	q := `SELECT id, model_id, COALESCE(engine,''), state, priority, COALESCE(endpoint,''),
	             COALESCE(error,''), COALESCE(reason,''), COALESCE(queue_id,''), COALESCE(instance_id,''),
	             COALESCE(created_at,''), COALESCE(started_at,''), COALESCE(finished_at,'')
	      FROM jobs`
	var args []any
	var conds []string
	if strings.TrimSpace(filter.State) != "" {
		conds = append(conds, "state = ?")
		args = append(args, filter.State)
	}
	if strings.TrimSpace(filter.Model) != "" {
		conds = append(conds, "model_id = ?")
		args = append(args, filter.Model)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY created_at DESC, id"
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	q += " LIMIT ?"
	args = append(args, limit)

	rows, err := s.deps.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: query: %w", err)
	}
	defer rows.Close()
	out := []JobView{}
	for rows.Next() {
		var v JobView
		if err := rows.Scan(&v.ID, &v.ModelID, &v.Engine, &v.State, &v.Priority, &v.Endpoint,
			&v.Error, &v.Reason, &v.QueueID, &v.InstanceID,
			&v.CreatedAt, &v.StartedAt, &v.FinishedAt); err != nil {
			return nil, fmt.Errorf("list jobs: scan: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// getJobRow reads one job. withResult controls whether result_json is loaded (list views skip it).
func (s *Service) getJobRow(ctx context.Context, id string, withResult bool) (JobView, error) {
	var v JobView
	var result sql.NullString
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT id, model_id, COALESCE(engine,''), state, priority, COALESCE(endpoint,''),
		        COALESCE(error,''), COALESCE(reason,''), COALESCE(queue_id,''), COALESCE(instance_id,''),
		        COALESCE(created_at,''), COALESCE(started_at,''), COALESCE(finished_at,''), result_json
		 FROM jobs WHERE id = ?`, id).
		Scan(&v.ID, &v.ModelID, &v.Engine, &v.State, &v.Priority, &v.Endpoint,
			&v.Error, &v.Reason, &v.QueueID, &v.InstanceID,
			&v.CreatedAt, &v.StartedAt, &v.FinishedAt, &result)
	if err == sql.ErrNoRows {
		return JobView{}, fmt.Errorf("job %q not found", id)
	}
	if err != nil {
		return JobView{}, fmt.Errorf("get job: %w", err)
	}
	if withResult && result.Valid && result.String != "" {
		v.ResultJSON = json.RawMessage(result.String)
	}
	return v, nil
}

// CancelJob cancels a job (DELETE /api/jobs/{id}, design §2.5). Honest per the lmwrapper lesson:
//   - terminal (succeeded/failed/cancelled) → returns the row unchanged; the handler maps to 409.
//   - queued (mid-queue)  → remove the residency queue row (if any) + mark cancelled. Clean.
//   - running (mid-inference) → abort the worker's upstream request context (if a live cancel func
//     exists) + mark cancelled. The ENGINE may still finish the generation internally (llama.cpp
//     runs a decode to completion); "cancelled" means LocalHub stopped waiting and will not deliver
//     the result — NOT that the GPU instantly stopped. The instance is NOT killed (never-kill gate).
func (s *Service) CancelJob(ctx context.Context, id string) (JobView, error) {
	if s.deps.DB == nil {
		return JobView{}, fmt.Errorf("cancel job: database not configured")
	}
	cur, err := s.getJobRow(ctx, id, false)
	if err != nil {
		return JobView{}, err
	}

	switch cur.State {
	case JobStateSucceeded, JobStateFailed, JobStateCancelled:
		// Terminal → no-op; the handler returns 409. Return the row as-is (isTerminal flag via state).
		return cur, nil

	case JobStateQueued:
		// Remove the residency queue row (if we recorded one) so no phantom entry auto-starts later.
		if cur.QueueID != "" {
			_ = s.CancelQueueEntry(context.Background(), cur.QueueID)
		}
		reason := "cancelled by user before admission"
		// Conditional on state='queued': if the worker admitted + completed this job between our read
		// above and here, the row is now succeeded/running — do NOT clobber its result with cancelled.
		ok, ferr := s.finishJobIfState(ctx, id, JobStateQueued, JobStateCancelled, "cancelled", reason)
		if ferr != nil {
			return JobView{}, ferr
		}
		if !ok {
			// Lost the race: the job left `queued`. Return the current (possibly terminal) row so the
			// handler's 409 reflects the real state, and fire no cancelled event (nothing was cancelled).
			return s.getJobRow(ctx, id, false)
		}
		s.publishJobEvent(id, JobStateCancelled, cur.ModelID, reason)
		return s.getJobRow(ctx, id, false)

	case JobStateRunning:
		// Abort the worker's upstream relay server-side (if a live cancel func exists). Whether or
		// not one does, mark cancelled durably; the worker observes the terminal state on its own
		// completion and discards the result (design §2.5 fallback).
		if cancel, ok := s.jobs().takeCancel(id); ok {
			cancel()
		}
		reason := "cancelled by user mid-inference; upstream stream aborted, instance not killed"
		// Conditional on state='running': if the relay finished (succeeded) or failed between our read
		// and here, preserve that terminal row + result rather than overwriting it as cancelled.
		ok, ferr := s.finishJobIfState(ctx, id, JobStateRunning, JobStateCancelled, "cancelled", reason)
		if ferr != nil {
			return JobView{}, ferr
		}
		if !ok {
			// Lost the race: the job completed first. Return its terminal row (with result) so the
			// handler 409s on an already-finished job instead of hiding the result behind "cancelled".
			return s.getJobRow(ctx, id, false)
		}
		s.publishJobEvent(id, JobStateCancelled, cur.ModelID, reason)
		return s.getJobRow(ctx, id, false)
	}
	return cur, nil
}

// ReconcileJobs runs at daemon boot (called before RunJobWorker, next to ReconcileInstances). It
// fails any job left `running` by a crash/restart with reason "daemon restarted mid-inference"
// (design §2.6): a running job's process was killed; we cannot know how far it got or whether the
// caller already saw a partial, so silently re-running risks a duplicate inference. `queued` jobs
// are left untouched — RunJobWorker re-scans and re-admits them (a queued job never started
// inference, so re-admitting is exactly what would have happened without the restart).
func (s *Service) ReconcileJobs(ctx context.Context) error {
	if s.deps.DB == nil {
		return nil
	}
	// Collect the ids first so we can publish an event + carry the reason per job.
	rows, err := s.deps.DB.QueryContext(ctx, `SELECT id, model_id FROM jobs WHERE state = 'running'`)
	if err != nil {
		return fmt.Errorf("reconcile jobs: query running: %w", err)
	}
	type stale struct{ id, model string }
	var staleJobs []stale
	for rows.Next() {
		var st stale
		if err := rows.Scan(&st.id, &st.model); err != nil {
			rows.Close()
			return fmt.Errorf("reconcile jobs: scan: %w", err)
		}
		staleJobs = append(staleJobs, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reconcile jobs: iterate: %w", err)
	}

	const reason = "daemon restarted mid-inference"
	for _, st := range staleJobs {
		if err := s.finishJob(ctx, st.id, JobStateFailed, "daemon_restart", "daemon_restart", reason); err != nil {
			return fmt.Errorf("reconcile jobs: fail %s: %w", st.id, err)
		}
		s.publishJobEvent(st.id, JobStateFailed, st.model, reason)
	}
	return nil
}

// RunJobWorker is the single background worker loop (started in main.go next to StartPeriodicDrain,
// gated on cfg.Jobs.Enabled). It drains `queued` jobs highest-priority-oldest-first, admits each
// through StartInstance (same path as the proxy), relays the request NON-STREAMING to the engine,
// captures the final result into result_json, and records the terminal state. It wakes on a tick
// (reusing the drain cadence) AND on a SubmitJob signal.
func (s *Service) RunJobWorker(ctx context.Context) {
	interval := 3 * time.Second
	if s.deps.Residency != nil {
		if d := s.deps.Residency.DrainInterval(); d > 0 {
			interval = d
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	wake := s.jobs().wake
	for {
		// Drain all currently-runnable queued jobs before waiting again (one at a time — serialized,
		// design §2.3). processNextJob returns false when there is nothing left to do this pass.
		for {
			if ctx.Err() != nil {
				return
			}
			did, requeued := s.processNextJob(ctx)
			if !did || requeued {
				// Nothing admitted, or the head job re-queued (still no VRAM) → stop this pass and
				// wait for the next tick/wake (the drain loop frees room; we re-attempt then).
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		}
	}
}

// processNextJob picks the highest-priority oldest queued job and attempts to run it. It returns
// (did, requeued): did=true when it touched a job (ran/failed/requeued), requeued=true when the job
// stayed queued (still no VRAM) so the caller stops the pass. did=false means no queued job exists.
func (s *Service) processNextJob(ctx context.Context) (did bool, requeued bool) {
	if s.deps.DB == nil {
		return false, false
	}
	// priority 0 (interactive) first, then oldest. queue_id is read too so runJob can hold AT MOST
	// ONE live residency row per blocked job (fix 1): a job already holding a WAITING queue row is
	// left for the drain loop, not re-admitted (which would mint a duplicate row).
	var id, modelID, requestJSON, endpoint, queueID string
	var priority int
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT id, model_id, request_json, priority, COALESCE(endpoint,'chat'), COALESCE(queue_id,'')
		 FROM jobs WHERE state = 'queued' ORDER BY priority ASC, created_at ASC, id ASC LIMIT 1`).
		Scan(&id, &modelID, &requestJSON, &priority, &endpoint, &queueID)
	if err == sql.ErrNoRows {
		return false, false
	}
	if err != nil {
		return false, false
	}

	// Fix 1: if this job already holds a LIVE (still WAITING) residency queue row, the drain loop
	// owns re-admission — do NOT call StartInstance again (it would create a NEW WAITING row + a NEW
	// durable mirror while only the latest queue_id is stored on the job, orphaning earlier rows that
	// can later drain into duplicate model starts). Leave it queued and stop the pass; the drain loop
	// admits it when room frees and the worker's NEXT tick finds the model warm (route_loaded) and
	// relays. Only when the row is gone/terminal do we (below) retry StartInstance.
	if s.queueEntryWaiting(queueID) {
		return true, true // touched a job, still queued → caller stops the pass
	}

	s.runJob(ctx, id, modelID, requestJSON, endpoint, priority)
	// If the job is still queued after runJob (re-queued on *QueuedError), signal the caller to stop.
	var state string
	if qerr := s.deps.DB.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, id).Scan(&state); qerr == nil {
		return true, state == JobStateQueued
	}
	return true, false
}

// runJob admits + relays a single queued job to a terminal state (design §2.3). Admission uses
// StartInstance (the one-core rule). On HEALTHY it transitions queued→running, relays non-streaming
// (forcing stream:false upstream), captures result_json, transitions running→succeeded. A
// *QueuedError leaves it queued (records queue_id; the drain loop frees room later). A refusal
// fails it with the numeric reason.
func (s *Service) runJob(ctx context.Context, id, modelID, requestJSON, endpoint string, priority int) {
	inst, err := s.StartInstance(ctx, StartInstanceRequest{
		ModelID:  modelID,
		Engine:   "",
		Priority: priority,
		Ctx:      0,
	})
	if err != nil {
		var qErr *QueuedError
		if isQueuedErr(err, &qErr) {
			// Leave queued; record the residency queue id so cancel-mid-queue can remove it and the
			// drain loop can re-admit it. Because processNextJob skips a job whose queue_id is still
			// WAITING (fix 1), this UPDATE runs only when the job had NO live row (first block, or the
			// prior row went terminal) — so the job holds exactly ONE live residency row at a time.
			// Best-effort UPDATE.
			_, _ = s.deps.DB.ExecContext(ctx, `UPDATE jobs SET queue_id = ? WHERE id = ? AND state = 'queued'`, qErr.QueueID, id)
			return
		}
		// Refusal / wont_run / other → fail with the numeric reason (mandatory-reason gate).
		reason := err.Error()
		_ = s.finishJob(ctx, id, JobStateFailed, "admission", "admission", reason)
		s.publishJobEvent(id, JobStateFailed, modelID, reason)
		return
	}

	// HEALTHY: transition queued→running (record instance_id + engine + started_at).
	if err := s.startJobRunning(ctx, id, inst.ID, inst.Engine); err != nil {
		// Could not mark running (e.g. the job was cancelled between the pick and here). If it is now
		// terminal (cancelled), abort — do not relay a cancelled job.
		return
	}
	s.publishJobEvent(id, JobStateRunning, modelID, "")

	// Open an in-flight request against the instance (never-kill-active accounting, design §2.3).
	handle, herr := s.BeginInstanceRequest(ctx, inst.ID)
	if herr != nil {
		reason := fmt.Sprintf("instance %s became unservable before relay: %v", inst.ID, herr)
		_ = s.finishJob(ctx, id, JobStateFailed, "unservable", "unservable", reason)
		s.publishJobEvent(id, JobStateFailed, modelID, reason)
		return
	}
	defer handle.Done()

	// Register a per-job cancel func so CancelJob can abort this relay server-side (design §2.5).
	relayCtx, cancel := context.WithCancel(ctx)
	s.jobs().registerCancel(id, cancel)
	defer func() {
		s.jobs().unregisterCancel(id)
		cancel()
	}()

	// Force stream:false — an async job returns ONE final result, not a stream (design §2.3).
	upstreamBody := proxy.SetJSONField([]byte(requestJSON), "stream", false)
	// Upstream model rewrite (same contract as the /v1/* proxy, internal/api/proxy.go:111-112): the
	// shared Ollama daemon routes by the `model` field and knows the model by its TAG, not LocalHub's
	// catalog id — so a job targeting an ollama-source model must relay the tag or the daemon 404s
	// "model not found". handle.UpstreamModel carries the tag ONLY for Ollama instances (P6.1 fix);
	// it is empty for llama.cpp/vLLM, which own a single loaded model and ignore the field, so the
	// body relays unchanged (byte-identical to the prior behavior).
	if handle.UpstreamModel != "" {
		upstreamBody = proxy.SetJSONField(upstreamBody, "model", handle.UpstreamModel)
	}
	path := "/v1/chat/completions"
	if endpoint == "completions" {
		path = "/v1/completions"
	}

	result, relErr := relayNonStreaming(relayCtx, handle.TargetBaseURL, path, upstreamBody)
	// If the job was cancelled mid-inference, CancelJob already wrote the terminal row + fired the
	// event; do not overwrite it. Detect by re-reading state.
	if s.jobIsTerminal(ctx, id) {
		return
	}
	if relErr != nil {
		reason := fmt.Sprintf("engine relay failed: %v", relErr)
		_ = s.finishJob(ctx, id, JobStateFailed, "engine_error", "engine_error", reason)
		s.publishJobEvent(id, JobStateFailed, modelID, reason)
		return
	}
	// Success: store the full non-streaming OpenAI response body as result_json.
	if err := s.succeedJob(ctx, id, result); err != nil {
		return
	}
	s.publishJobEvent(id, JobStateSucceeded, modelID, "")
}

// relayNonStreaming POSTs the (stream:false) body to {baseURL}{path} and returns the full response
// body. Unlike proxy.Relay (which streams to an http.ResponseWriter), the async job path buffers the
// ONE final result into result_json. A non-2xx upstream status is an error carrying the body so the
// job's reason explains the engine failure.
func relayNonStreaming(ctx context.Context, baseURL, path string, body []byte) (json.RawMessage, error) {
	target := strings.TrimRight(baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 0} // a long generation must be allowed to complete
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach engine: %w", err)
	}
	defer resp.Body.Close()
	respBody, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return nil, fmt.Errorf("read engine response: %w", rerr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("engine returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return json.RawMessage(respBody), nil
}

// startJobRunning transitions queued→running ONLY if the job is still queued (a concurrent cancel
// wins). Records instance_id, engine, started_at. Returns an error if it was no longer queued.
func (s *Service) startJobRunning(ctx context.Context, id, instanceID, engine string) error {
	res, err := s.deps.DB.ExecContext(ctx,
		`UPDATE jobs SET state='running', instance_id=?, engine=?, started_at=datetime('now')
		 WHERE id=? AND state='queued'`, instanceID, engine, id)
	if err != nil {
		return fmt.Errorf("start job running: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("job %q no longer queued (cancelled or gone)", id)
	}
	return nil
}

// succeedJob transitions running→succeeded with the result body ONLY if still running (a concurrent
// cancel wins).
func (s *Service) succeedJob(ctx context.Context, id string, result json.RawMessage) error {
	res, err := s.deps.DB.ExecContext(ctx,
		`UPDATE jobs SET state='succeeded', result_json=?, finished_at=datetime('now')
		 WHERE id=? AND state='running'`, string(result), id)
	if err != nil {
		return fmt.Errorf("succeed job: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("job %q no longer running", id)
	}
	return nil
}

// finishJob writes a terminal state (failed/cancelled/succeeded) with error/reason + finished_at.
// It is unconditional (used by cancel/reconcile/refusal paths where the transition is authoritative).
// Every failed/cancelled row MUST carry a non-empty reason (the mandatory-reason gate).
func (s *Service) finishJob(ctx context.Context, id, state, errClass, _ /*machineReason*/, reason string) error {
	_, err := s.deps.DB.ExecContext(ctx,
		`UPDATE jobs SET state=?, error=NULLIF(?,''), reason=NULLIF(?,''), finished_at=datetime('now')
		 WHERE id=?`, state, errClass, reason, id)
	if err != nil {
		return fmt.Errorf("finish job %q: %w", id, err)
	}
	return nil
}

// finishJobIfState writes a terminal state ONLY when the row is still in fromState (a CAS guard for
// the cancel path). It returns ok=true when the transition applied (RowsAffected==1) and ok=false
// when the row had already moved on (a concurrent worker completion won the race) — the caller then
// preserves the existing terminal row instead of clobbering its result. Every cancelled row carries
// a non-empty reason (the mandatory-reason gate).
func (s *Service) finishJobIfState(ctx context.Context, id, fromState, toState, errClass, reason string) (bool, error) {
	res, err := s.deps.DB.ExecContext(ctx,
		`UPDATE jobs SET state=?, error=NULLIF(?,''), reason=NULLIF(?,''), finished_at=datetime('now')
		 WHERE id=? AND state=?`, toState, errClass, reason, id, fromState)
	if err != nil {
		return false, fmt.Errorf("finish job %q (from %s): %w", id, fromState, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// jobIsTerminal reports whether the job is now in a terminal state (used to avoid overwriting a row
// CancelJob already wrote while the relay was in flight).
func (s *Service) jobIsTerminal(ctx context.Context, id string) bool {
	var state string
	if err := s.deps.DB.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=?`, id).Scan(&state); err != nil {
		return false
	}
	return state == JobStateSucceeded || state == JobStateFailed || state == JobStateCancelled
}

// publishJobEvent fires an EventJobUpdated on the bus (design §2.4). The WS handler forwards it
// generically; a subscriber refetches GET /api/jobs/{id} for the terminal result. Best-effort/nil-safe.
func (s *Service) publishJobEvent(id, state, modelID, reason string) {
	if s.deps.Events != nil {
		s.deps.Events.Publish(Event{Type: EventJobUpdated, Data: JobEvent{
			ID: id, State: state, ModelID: modelID, Reason: reason,
		}})
	}
}

// isQueuedErr is a small errors.As wrapper kept local so jobs.go does not import errors just for one
// call (proxy.go already imports errors; this mirrors the residency starter's pattern).
func isQueuedErr(err error, target **QueuedError) bool {
	for err != nil {
		if qe, ok := err.(*QueuedError); ok {
			*target = qe
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
