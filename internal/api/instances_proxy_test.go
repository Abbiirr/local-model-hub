package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/core"
	"github.com/localhub/localhub/internal/db"
	"github.com/localhub/localhub/internal/hardware"
	"github.com/localhub/localhub/internal/instances"
	"github.com/localhub/localhub/internal/residency"
)

// proxyTestService builds a Service with a real instance store + in-flight tracker + a HEALTHY
// llama.cpp instance whose base_url points at the given upstream URL, so the completion relay has a
// target and the in-flight counter has something to count.
func proxyTestService(t *testing.T, upstream string) (*core.Service, *instances.Store, *residency.InFlightTracker) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format) VALUES ('m1','M1','local_path','gguf')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO engines (name, installed) VALUES ('llamacpp',1)`); err != nil {
		t.Fatal(err)
	}
	store := instances.NewStore(d.DB)
	if _, err := store.Create(context.Background(), instances.CreateParams{
		ID: "i1", ModelID: "m1", Engine: instances.EngineLlamaCPP, Reason: "seed",
	}); err != nil {
		t.Fatal(err)
	}
	port := 40100
	if _, err := store.Transition(context.Background(), "i1", instances.TransitionParams{
		To: instances.StateStarting, Reason: "seed", Port: &port, BaseURL: &upstream,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), "i1", instances.TransitionParams{
		To: instances.StateHealthy, Reason: "seed",
	}); err != nil {
		t.Fatal(err)
	}
	inflight := residency.NewInFlightTracker()
	svc := core.New(core.Deps{
		Cfg: config.Default(), DB: d, InstanceStore: store, InFlight: inflight,
		Hardware: &hardware.FakeProbe{Snap: hardware.DefaultFakeSnapshot()},
	})
	return svc, store, inflight
}

// TestCompletionProxy_StreamsAndCountsInFlight is the fix-3 API gate: the completion relay streams
// the upstream response byte-for-byte AND holds the in-flight count for the FULL duration of the
// stream (observed via a mid-stream check), releasing it on completion.
func TestCompletionProxy_StreamsAndCountsInFlight(t *testing.T) {
	// Upstream engine stub: streams two SSE chunks with a gate between them so the test can observe
	// the in-flight count WHILE the stream is open.
	midStream := make(chan struct{})
	proceed := make(chan struct{})
	var inflightDuringStream int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"delta\":\"Hello\"}\n\n"))
		if fl != nil {
			fl.Flush()
		}
		close(midStream)
		<-proceed
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()

	svc, _, inflight := proxyTestService(t, upstream.URL)
	srv := &Server{svc: svc, logger: quietLogger()}

	// Fire the relay in a goroutine; observe in-flight mid-stream.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/instances/i1/completions", strings.NewReader(`{"stream":true}`))
	req.SetPathValue("id", "i1")
	done := make(chan struct{})
	go func() {
		srv.handleInstanceCompletion(rec, req)
		close(done)
	}()

	<-midStream // upstream has sent the first chunk; the relay is mid-stream
	atomic.StoreInt32(&inflightDuringStream, int32(inflight.ActiveCount("i1")))
	close(proceed)
	<-done

	if got := atomic.LoadInt32(&inflightDuringStream); got != 1 {
		t.Errorf("in-flight during stream = %d, want 1 (the never-kill-active signal)", got)
	}
	// Released on completion.
	if c := inflight.ActiveCount("i1"); c != 0 {
		t.Errorf("in-flight after completion = %d, want 0", c)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("relay status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Hello") || !strings.Contains(body, "[DONE]") {
		t.Errorf("relay body did not stream through intact: %q", body)
	}
}

// TestCompletionProxy_RejectsNonHealthy: a relay to a non-HEALTHY / unknown instance is 409, and
// nothing is counted.
func TestCompletionProxy_RejectsUnknown(t *testing.T) {
	svc, _, inflight := proxyTestService(t, "http://127.0.0.1:1")
	srv := &Server{svc: svc, logger: quietLogger()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/instances/nope/completions", strings.NewReader(`{}`))
	req.SetPathValue("id", "nope")
	srv.handleInstanceCompletion(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 for an unknown instance", rec.Code)
	}
	if c := inflight.ActiveCount("nope"); c != 0 {
		t.Errorf("in-flight leaked for a rejected relay: %d", c)
	}
}

// TestInstanceStop_Returns202Draining is the fix-4 API gate: DELETE returns 202 with a DRAINING
// view immediately (the stop is detached). We seed an instance WITHOUT a runner so the background
// stop is a no-op that cannot write STOPPED synchronously — proving the 202 is returned before the
// stop completes.
func TestInstanceStop_Returns202Draining(t *testing.T) {
	svc, _, _ := proxyTestService(t, "http://127.0.0.1:1")
	srv := &Server{svc: svc, logger: quietLogger()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/instances/i1", nil)
	req.SetPathValue("id", "i1")
	srv.handleInstanceStop(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("stop status = %d, want 202 Accepted (draining), body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "DRAINING") {
		t.Errorf("202 body should carry the DRAINING view: %s", rec.Body.String())
	}
	// give the detached goroutine a moment; nothing to assert beyond no panic (no runner wired).
	time.Sleep(50 * time.Millisecond)
}
