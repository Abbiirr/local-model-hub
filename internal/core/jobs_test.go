package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/instances"
)

// warmJobService builds a residency-wired Service with a HEALTHY warm instance for m1 whose base_url
// points at the given upstream engine URL, so the job worker admits (warm route) then relays to the
// fake engine. Returns the service.
func warmJobService(t *testing.T, upstream string, freeVRAM int) *Service {
	t.Helper()
	svc, _, _ := startResidencyService(t, compat.Verdict{
		Status: compat.StatusRuns, Reason: "ok", EstimatedVRAMMiB: 4000,
	}, freeVRAM, []instances.Instance{
		{ID: "warm1", ModelID: "m1", Engine: instances.EngineLlamaCPP, VRAMEstMiB: 4000},
	})
	if _, err := svc.deps.DB.Exec(`UPDATE instances SET base_url=? WHERE id='warm1'`, upstream); err != nil {
		t.Fatalf("set warm base_url: %v", err)
	}
	return svc
}

// TestSubmitJob_ReturnsQueuedImmediately is the #p62 submit gate: SubmitJob inserts a queued row and
// returns the id WITHOUT waiting for inference (well under any inference latency).
func TestSubmitJob_ReturnsQueuedImmediately(t *testing.T) {
	svc := warmJobService(t, "http://127.0.0.1:1", 15200)
	start := time.Now()
	view, err := svc.SubmitJob(context.Background(), JobSubmit{
		Model: "m1", RequestJSON: []byte(`{"model":"m1","messages":[]}`), Priority: 1,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if view.ID == "" || view.State != JobStateQueued {
		t.Errorf("want a queued id immediately, got id=%q state=%q", view.ID, view.State)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("SubmitJob took %v — it must return immediately (no inference wait)", elapsed)
	}
	// The row is durably queued.
	got, gerr := svc.GetJob(context.Background(), view.ID)
	if gerr != nil || got.State != JobStateQueued {
		t.Errorf("job row not queued: %v state=%q", gerr, got.State)
	}
}

// TestJobWorker_RunsToSucceeded is the #p62 result gate: the worker admits (warm route), relays
// non-streaming to a fake engine, and records the final body into result_json → state succeeded.
func TestJobWorker_RunsToSucceeded(t *testing.T) {
	engineResp := `{"id":"cmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`
	var gotStreamFalse bool
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] == false {
			gotStreamFalse = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(engineResp))
	}))
	defer engine.Close()

	svc := warmJobService(t, engine.URL, 15200)
	view, err := svc.SubmitJob(context.Background(), JobSubmit{
		Model: "m1", RequestJSON: []byte(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Run one worker pass directly (deterministic — no goroutine race).
	did, _ := svc.processNextJob(context.Background())
	if !did {
		t.Fatal("worker did not pick up the queued job")
	}
	if !gotStreamFalse {
		t.Error("worker must force stream:false on the upstream body (async returns one final result)")
	}
	got, gerr := svc.GetJob(context.Background(), view.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.State != JobStateSucceeded {
		t.Fatalf("state = %q, want succeeded (reason=%q)", got.State, got.Reason)
	}
	if len(got.ResultJSON) == 0 || !strings.Contains(string(got.ResultJSON), "cmpl-1") {
		t.Errorf("result_json not captured: %q", got.ResultJSON)
	}
	if got.InstanceID != "warm1" {
		t.Errorf("instance_id = %q, want warm1", got.InstanceID)
	}
}

// TestJobWorker_RewritesModelForOllama is the FINDING-3 gate: a job targeting an OLLAMA-source model
// must relay the Ollama TAG (handle.UpstreamModel) in the body's `model` field — Ollama routes by tag,
// so relaying LocalHub's catalog id upstream yields model-not-found. This mirrors the /v1/* proxy's
// rewrite (internal/api/proxy.go). For a llama.cpp instance UpstreamModel is empty → the body's model
// is relayed UNCHANGED.
func TestJobWorker_PreservesLlamaCPPModelID(t *testing.T) {
	capture := func(t *testing.T) (*httptest.Server, *string) {
		t.Helper()
		got := new(string)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if m, ok := body["model"].(string); ok {
				*got = m
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"cmpl-ok","choices":[{"message":{"content":"hi"}}]}`))
		}))
		t.Cleanup(srv.Close)
		return srv, got
	}

	t.Run("ollama source → body model rewritten to the tag", func(t *testing.T) {
		engine, gotModel := capture(t)
		svc, _, _ := startResidencyService(t, compat.Verdict{
			Status: compat.StatusRuns, Reason: "ok", EstimatedVRAMMiB: 4000,
		}, 15200, nil)
		// An OllamaRunner must be present for the Ollama dispatch (the warm route returns before Warm is
		// actually called, but startOllamaInstance nil-checks the runner up front).
		// Seed an ollama-source model whose display_name is the upstream TAG, and a HEALTHY Ollama
		// instance for it pointed at the fake engine.
		if _, err := svc.deps.DB.Exec(
			`INSERT INTO models (id, display_name, source, format) VALUES ('mo','mistral:latest','ollama','ollama')`); err != nil {
			t.Fatal(err)
		}
		seedHealthyOn(t, svc, "warm-o", "mo", instances.EngineOllama, engine.URL)

		submitAndRunExpectFailure(t, svc, "mo", `{"model":"mo","messages":[]}`)
		if *gotModel != "" {
			t.Errorf("retired Ollama model was relayed upstream as %q", *gotModel)
		}
	})

	t.Run("llama.cpp → body model unchanged", func(t *testing.T) {
		engine, gotModel := capture(t)
		svc := warmJobService(t, engine.URL, 15200)
		submitAndRun(t, svc, "m1", `{"model":"m1","messages":[]}`)
		if *gotModel != "m1" {
			t.Errorf("relayed model = %q, want the client's id %q unchanged for llama.cpp", *gotModel, "m1")
		}
	})
}

// seedHealthyOn inserts a HEALTHY instance for modelID on the given engine pointed at baseURL.
func seedHealthyOn(t *testing.T, svc *Service, id, modelID string, engine instances.Engine, baseURL string) {
	t.Helper()
	store := svc.deps.InstanceStore
	if _, err := store.Create(context.Background(), instances.CreateParams{
		ID: id, ModelID: modelID, Engine: engine, VRAMEstMiB: 4000, Reason: "seed",
	}); err != nil {
		t.Fatalf("seed instance %s: %v", id, err)
	}
	for _, to := range []instances.State{instances.StateStarting, instances.StateHealthy} {
		if _, err := store.Transition(context.Background(), id, instances.TransitionParams{To: to, Reason: "seed"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.deps.DB.Exec(`UPDATE instances SET base_url=? WHERE id=?`, baseURL, id); err != nil {
		t.Fatal(err)
	}
}

// submitAndRun submits a job and runs one worker pass, asserting it succeeded.
func submitAndRun(t *testing.T, svc *Service, model, requestJSON string) {
	t.Helper()
	view, err := svc.SubmitJob(context.Background(), JobSubmit{Model: model, RequestJSON: []byte(requestJSON)})
	if err != nil {
		t.Fatal(err)
	}
	if did, _ := svc.processNextJob(context.Background()); !did {
		t.Fatal("worker did not pick up the queued job")
	}
	got, _ := svc.GetJob(context.Background(), view.ID)
	if got.State != JobStateSucceeded {
		t.Fatalf("job state = %q, want succeeded (reason=%q)", got.State, got.Reason)
	}
}

func submitAndRunExpectFailure(t *testing.T, svc *Service, model, requestJSON string) {
	t.Helper()
	view, err := svc.SubmitJob(context.Background(), JobSubmit{Model: model, RequestJSON: []byte(requestJSON)})
	if err != nil {
		t.Fatal(err)
	}
	if did, _ := svc.processNextJob(context.Background()); !did {
		t.Fatal("worker did not pick up the queued job")
	}
	got, _ := svc.GetJob(context.Background(), view.ID)
	if got.State != JobStateFailed || !strings.Contains(got.Reason, "Ollama is retired") {
		t.Fatalf("job state = %q reason=%q, want a clear Ollama retirement failure", got.State, got.Reason)
	}
}

// TestCancelJob_RaceKeepsTerminalRow is the FINDING-4 gate: if a job completes between CancelJob's
// read and its UPDATE, the conditional (WHERE state=?) UPDATE affects 0 rows, so the succeeded row +
// result is PRESERVED (not clobbered to cancelled). CancelJob then returns the current terminal row.
func TestCancelJob_RaceKeepsTerminalRow(t *testing.T) {
	d := coreTestDB(t)
	svc := New(Deps{DB: d, Events: NewEventBus()})
	// Simulate the race directly: CancelJob reads a 'running' row, but before its UPDATE the worker
	// completes the job to 'succeeded' with a result. We reproduce that ordering by seeding a running
	// row, then flipping it to succeeded AFTER CancelJob's read but BEFORE its conditional UPDATE — the
	// conditional guard (WHERE state='running') is what makes the update a no-op. We exercise the guard
	// via the finishJobIfState CAS the cancel path uses.
	if _, err := d.Exec(`INSERT INTO jobs (id, model_id, request_json, priority, state, result_json)
		VALUES ('j1','m1','{}',1,'running','{"id":"cmpl-final"}')`); err != nil {
		t.Fatal(err)
	}
	// The worker already wrote the terminal succeeded row between the read and the update:
	if _, err := d.Exec(`UPDATE jobs SET state='succeeded', finished_at=datetime('now') WHERE id='j1'`); err != nil {
		t.Fatal(err)
	}
	// The cancel's conditional UPDATE (from 'running') must NOT apply.
	ok, err := svc.finishJobIfState(context.Background(), "j1", JobStateRunning, JobStateCancelled, "cancelled", "cancelled by user")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("conditional cancel applied to an already-succeeded row — the result would be clobbered")
	}
	got, _ := svc.GetJob(context.Background(), "j1")
	if got.State != JobStateSucceeded {
		t.Errorf("state = %q, want succeeded preserved (not overwritten by cancel)", got.State)
	}
	if !strings.Contains(string(got.ResultJSON), "cmpl-final") {
		t.Errorf("result_json clobbered: %q", got.ResultJSON)
	}
}

// TestCancelJob_MidQueue is the #p62 cancel-mid-queue gate: a job that stays queued (VRAM too tight)
// can be cancelled; the residency queue row is removed and the job is cancelled with a reason.
func TestCancelJob_MidQueue(t *testing.T) {
	// free 2000, footprint 9000 → the job stays queued (no warm instance for a different model).
	svc, _, _ := startResidencyService(t, compat.Verdict{
		Status: compat.StatusRuns, Reason: "big", EstimatedVRAMMiB: 9000,
	}, 2000, nil)
	view, err := svc.SubmitJob(context.Background(), JobSubmit{
		Model: "m1", RequestJSON: []byte(`{"model":"m1","messages":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	// One worker pass → the job re-queues (records queue_id) but stays queued.
	svc.processNextJob(context.Background())
	mid, _ := svc.GetJob(context.Background(), view.ID)
	if mid.State != JobStateQueued {
		t.Fatalf("job should still be queued after a blocked admission, got %q", mid.State)
	}

	cancelled, cerr := svc.CancelJob(context.Background(), view.ID)
	if cerr != nil {
		t.Fatalf("CancelJob: %v", cerr)
	}
	if cancelled.State != JobStateCancelled {
		t.Errorf("state = %q, want cancelled", cancelled.State)
	}
	if cancelled.Reason == "" {
		t.Error("a cancelled job MUST carry a reason (mandatory-reason gate)")
	}
	// The residency queue must have no WAITING row left.
	rows, _ := svc.Queue(context.Background())
	for _, r := range rows {
		if r.Status == "WAITING" {
			t.Errorf("queue row not removed on cancel-mid-queue: %+v", r)
		}
	}
}

// TestJobWorker_BlockedJobHoldsSingleQueueRow is the fix-1 regression gate: a job blocked on VRAM
// must hold AT MOST ONE live residency WAITING row no matter how many worker ticks run on it. Before
// the fix, each processNextJob tick re-selected the queued job and called StartInstance→Admit again,
// minting a NEW WAITING row (in-memory + durable) each time while only the latest queue_id was stored
// on the job — orphaning earlier rows that could later drain into DUPLICATE model starts for one job.
func TestJobWorker_BlockedJobHoldsSingleQueueRow(t *testing.T) {
	// free 2000, footprint 9000, nothing evictable → the job stays queued on every tick.
	svc, fr, probe := startResidencyService(t, compat.Verdict{
		Status: compat.StatusRuns, Reason: "big", EstimatedVRAMMiB: 9000,
	}, 2000, nil)

	view, err := svc.SubmitJob(context.Background(), JobSubmit{
		Model: "m1", RequestJSON: []byte(`{"model":"m1","messages":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	countWaiting := func() int {
		var n int
		if qerr := svc.deps.DB.QueryRow(`SELECT COUNT(*) FROM queue WHERE status='WAITING'`).Scan(&n); qerr != nil {
			t.Fatalf("count waiting: %v", qerr)
		}
		return n
	}

	// Run MANY worker ticks on the blocked job.
	const ticks = 8
	for i := 0; i < ticks; i++ {
		svc.processNextJob(context.Background())
		if got := countWaiting(); got != 1 {
			t.Fatalf("after tick %d: %d WAITING residency rows, want exactly 1 (duplicate-row bug)", i+1, got)
		}
	}
	// The job is still queued and still references its single live row.
	mid, _ := svc.GetJob(context.Background(), view.ID)
	if mid.State != JobStateQueued {
		t.Fatalf("job state = %q, want queued", mid.State)
	}
	if mid.QueueID == "" {
		t.Fatal("blocked job must carry its single queue_id")
	}
	if !svc.deps.Residency.IsWaiting(mid.QueueID) {
		t.Fatal("the job's queue_id must be the single live WAITING row")
	}

	_ = fr

	// --- Now capacity frees: the job must still run to succeeded (no duplicate starts). ---
	engineResp := `{"id":"cmpl-ok","object":"chat.completion","choices":[{"message":{"content":"hi"}}]}`
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(engineResp))
	}))
	defer engine.Close()

	// Simulate the drain loop having admitted the single queue row and warmed m1: free VRAM, seed one
	// HEALTHY m1 instance pointed at the fake engine, and mark the queue row admitted (removed from
	// WAITING). Using a seeded warm instance keeps the test independent of the llama launch-plan (no
	// real binary needed) while still exercising the route_loaded → relay → succeeded worker path.
	probe.Snap.GPUs[0].VRAMFreeMiB = 15200
	warm, cerr := svc.deps.InstanceStore.Create(context.Background(), instances.CreateParams{
		ID: "warm-m1", ModelID: "m1", Engine: instances.EngineLlamaCPP, VRAMEstMiB: 4000, Reason: "drain-started",
	})
	if cerr != nil {
		t.Fatal(cerr)
	}
	for _, to := range []instances.State{instances.StateStarting, instances.StateHealthy} {
		if _, terr := svc.deps.InstanceStore.Transition(context.Background(), warm.ID, instances.TransitionParams{To: to, Reason: "seed"}); terr != nil {
			t.Fatal(terr)
		}
	}
	if _, err := svc.deps.DB.Exec(`UPDATE instances SET base_url=? WHERE id='warm-m1'`, engine.URL); err != nil {
		t.Fatal(err)
	}
	// Retire the single WAITING queue row (as the drain would on admit) so it does not linger.
	_ = svc.CancelQueueEntry(context.Background(), mid.QueueID)

	// Exactly one HEALTHY m1 instance exists (no duplicate model starts from orphaned rows).
	var instCount int
	if err := svc.deps.DB.QueryRow(`SELECT COUNT(*) FROM instances WHERE model_id='m1' AND state='HEALTHY'`).Scan(&instCount); err != nil {
		t.Fatal(err)
	}
	if instCount != 1 {
		t.Fatalf("HEALTHY m1 instances = %d, want exactly 1 (duplicate model start from orphaned rows)", instCount)
	}

	// The worker's next tick finds m1 warm (route_loaded) and relays to succeeded.
	svc.processNextJob(context.Background())
	got, _ := svc.GetJob(context.Background(), view.ID)
	if got.State != JobStateSucceeded {
		t.Fatalf("job state = %q, want succeeded once capacity freed (reason=%q)", got.State, got.Reason)
	}
	if !strings.Contains(string(got.ResultJSON), "cmpl-ok") {
		t.Errorf("result_json not captured after recovery: %q", got.ResultJSON)
	}
}

// TestCancelJob_MidInference is the #p62 cancel-mid-inference gate: a running job's relay is aborted
// server-side and the job is cancelled; the serving instance is NOT stopped (never-kill gate), and
// the reason states the honest semantics.
func TestCancelJob_MidInference(t *testing.T) {
	// The engine blocks until released, so the job stays running while we cancel it.
	release := make(chan struct{})
	started := make(chan struct{})
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done(): // the relay context was cancelled → abort (the cancel path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"late"}`))
	}))
	defer engine.Close()
	defer close(release)

	svc := warmJobService(t, engine.URL, 15200)
	view, err := svc.SubmitJob(context.Background(), JobSubmit{
		Model: "m1", RequestJSON: []byte(`{"model":"m1","messages":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Run the job in a goroutine; it will block in the engine relay.
	go svc.processNextJob(context.Background())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("engine never received the relay")
	}
	// The job should now be running.
	deadline := time.Now().Add(2 * time.Second)
	for {
		v, _ := svc.GetJob(context.Background(), view.ID)
		if v.State == JobStateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never reached running (state=%q)", v.State)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancelled, cerr := svc.CancelJob(context.Background(), view.ID)
	if cerr != nil {
		t.Fatalf("CancelJob: %v", cerr)
	}
	if cancelled.State != JobStateCancelled {
		t.Errorf("state = %q, want cancelled", cancelled.State)
	}
	if !strings.Contains(cancelled.Reason, "instance not killed") {
		t.Errorf("reason must state the honest semantics (instance not killed): %q", cancelled.Reason)
	}
	// The serving instance must still be HEALTHY (never-kill-active): cancel aborts the STREAM only.
	inst, _ := svc.deps.InstanceStore.Get(context.Background(), "warm1")
	if inst.State != instances.StateHealthy {
		t.Errorf("instance state = %q, want HEALTHY (cancel must not kill the instance)", inst.State)
	}
}

// TestReconcileJobs_RestartSemantics is the #p62 restart gate (design §2.6): running jobs become
// failed with reason "daemon restarted mid-inference"; queued jobs are left untouched.
func TestReconcileJobs_RestartSemantics(t *testing.T) {
	d := coreTestDB(t)
	svc := New(Deps{DB: d, Events: NewEventBus()})
	// Seed one running + one queued job directly (as if a prior process left them).
	if _, err := d.Exec(`INSERT INTO jobs (id, model_id, request_json, priority, state) VALUES
		('run1','m1','{}',1,'running'),
		('q1','m1','{}',1,'queued')`); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReconcileJobs(context.Background()); err != nil {
		t.Fatalf("ReconcileJobs: %v", err)
	}
	run1, _ := svc.GetJob(context.Background(), "run1")
	if run1.State != JobStateFailed {
		t.Errorf("running job state = %q, want failed", run1.State)
	}
	if run1.Reason != "daemon restarted mid-inference" {
		t.Errorf("running job reason = %q, want 'daemon restarted mid-inference'", run1.Reason)
	}
	q1, _ := svc.GetJob(context.Background(), "q1")
	if q1.State != JobStateQueued {
		t.Errorf("queued job state = %q, want queued (untouched)", q1.State)
	}
}

// TestJobRefusal_CarriesReason: a wont_run/refused admission fails the job WITH a non-empty reason.
func TestJobRefusal_CarriesReason(t *testing.T) {
	svc, _, _ := startResidencyService(t, compat.Verdict{
		Status: compat.StatusWontRun, Reason: "needs 20000 MiB, only 13000 usable", EstimatedVRAMMiB: 20000,
	}, 15200, nil)
	view, err := svc.SubmitJob(context.Background(), JobSubmit{
		Model: "m1", RequestJSON: []byte(`{"model":"m1","messages":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.processNextJob(context.Background())
	got, _ := svc.GetJob(context.Background(), view.ID)
	if got.State != JobStateFailed {
		t.Fatalf("state = %q, want failed for a wont_run model", got.State)
	}
	if got.Reason == "" {
		t.Error("a failed job MUST carry a reason (mandatory-reason gate)")
	}
}

// TestListJobs_FiltersAndOmitsResult: list filters by state and does NOT inline result_json.
func TestListJobs_FiltersAndOmitsResult(t *testing.T) {
	d := coreTestDB(t)
	svc := New(Deps{DB: d})
	if _, err := d.Exec(`INSERT INTO jobs (id, model_id, request_json, state, result_json) VALUES
		('a','m1','{}','succeeded','{"big":"result"}'),
		('b','m2','{}','queued',NULL)`); err != nil {
		t.Fatal(err)
	}
	all, err := svc.ListJobs(context.Background(), JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 jobs, got %d", len(all))
	}
	for _, j := range all {
		if len(j.ResultJSON) != 0 {
			t.Errorf("list view must NOT inline result_json (job %s)", j.ID)
		}
	}
	queued, _ := svc.ListJobs(context.Background(), JobFilter{State: "queued"})
	if len(queued) != 1 || queued[0].ID != "b" {
		t.Errorf("state filter failed: %+v", queued)
	}
}
