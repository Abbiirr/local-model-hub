package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/core"
	"github.com/localhub/localhub/internal/db"
)

// jobsTestServer builds a Server whose core has only a DB wired (enough for the durable-queue REST
// surface: submit/get/list/cancel). No worker runs — the handlers are exercised in isolation.
func jobsTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "apijobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	svc := core.New(core.Deps{DB: d, Events: core.NewEventBus(), Cfg: config.Default()})
	return &Server{svc: svc, logger: quietLogger()}, d
}

// TestJobSubmit_Returns201QueuedImmediately is the #p62 submit gate at the HTTP boundary: POST
// /api/jobs returns 201 + {id, state:"queued"} without waiting for inference.
func TestJobSubmit_Returns201QueuedImmediately(t *testing.T) {
	srv, _ := jobsTestServer(t)
	rec := httptest.NewRecorder()
	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}],"priority":2}`
	req := jsonReq(http.MethodPost, "/api/jobs", strings.NewReader(body))
	start := time.Now()
	srv.handleJobSubmit(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("submit took %v — must return immediately", elapsed)
	}
	var view core.JobView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.ID == "" || view.State != "queued" {
		t.Errorf("want {id, state:queued}, got id=%q state=%q", view.ID, view.State)
	}
	if view.Priority != 2 {
		t.Errorf("priority = %d, want 2 (passed through)", view.Priority)
	}
}

// TestJobSubmit_MissingModel returns 400.
func TestJobSubmit_MissingModel(t *testing.T) {
	srv, _ := jobsTestServer(t)
	rec := httptest.NewRecorder()
	req := jsonReq(http.MethodPost, "/api/jobs", strings.NewReader(`{"messages":[]}`))
	srv.handleJobSubmit(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestJobGet_404 returns 404 for an unknown job.
func TestJobGet_404(t *testing.T) {
	srv, _ := jobsTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/jobs/nope", nil)
	req.SetPathValue("id", "nope")
	srv.handleJobGet(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestJobCancel_TerminalReturns409: cancelling an already-succeeded job is a 409 no-op.
func TestJobCancel_TerminalReturns409(t *testing.T) {
	srv, d := jobsTestServer(t)
	if _, err := d.Exec(`INSERT INTO jobs (id, model_id, request_json, state) VALUES ('j1','m1','{}','succeeded')`); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/jobs/j1", nil)
	req.SetPathValue("id", "j1")
	srv.handleJobCancel(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a terminal job", rec.Code)
	}
}

// TestJobCancel_QueuedSucceeds: cancelling a queued job returns 200 + cancelled.
func TestJobCancel_QueuedSucceeds(t *testing.T) {
	srv, d := jobsTestServer(t)
	if _, err := d.Exec(`INSERT INTO jobs (id, model_id, request_json, state) VALUES ('j2','m1','{}','queued')`); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/jobs/j2", nil)
	req.SetPathValue("id", "j2")
	srv.handleJobCancel(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var view core.JobView
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.State != "cancelled" || view.Reason == "" {
		t.Errorf("want cancelled with a reason, got state=%q reason=%q", view.State, view.Reason)
	}
	// Confirm durably cancelled.
	got, _ := srv.svc.GetJob(context.Background(), "j2")
	if got.State != "cancelled" {
		t.Errorf("job not durably cancelled: %q", got.State)
	}
}

// TestJobList_Filters exercises the list handler + state filter.
func TestJobList_Filters(t *testing.T) {
	srv, d := jobsTestServer(t)
	if _, err := d.Exec(`INSERT INTO jobs (id, model_id, request_json, state) VALUES
		('a','m1','{}','queued'),('b','m1','{}','succeeded')`); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/jobs?state=queued", nil)
	srv.handleJobList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var list []core.JobView
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "a" {
		t.Errorf("state filter failed: %+v", list)
	}
}
