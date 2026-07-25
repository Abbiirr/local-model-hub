package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/core"
	"github.com/localhub/localhub/internal/db"
	"github.com/localhub/localhub/internal/engines/llamacpp"
	"github.com/localhub/localhub/internal/hardware"
	"github.com/localhub/localhub/internal/instances"
	"github.com/localhub/localhub/internal/residency"
)

// apiProxyProbe adapts a FakeProbe into the residency FreeVRAMProbe seam.
type apiProxyProbe struct{ p *hardware.FakeProbe }

func (a apiProxyProbe) FreeVRAMMiB(ctx context.Context) (int, error) {
	snap, err := a.p.Snapshot(ctx)
	if err != nil || len(snap.GPUs) == 0 {
		return 0, err
	}
	return snap.GPUs[0].VRAMFreeMiB, nil
}

// stubLlamaRunner satisfies the core LlamaRunnerIface so StartInstance passes its nil-check. The
// warm route returns route_loaded BEFORE Start is ever called; the block/not-found paths return
// before it too. Start is a fail-stub — a proxy test that reaches an actual spawn is a bug (the
// harness only exercises warm-route + fast-block + not-found, never a cold start).
type stubLlamaRunner struct{}

func (stubLlamaRunner) Start(context.Context, llamacpp.StartSpec) (instances.Instance, error) {
	return instances.Instance{}, context.Canceled // never expected in these tests
}
func (stubLlamaRunner) Stop(_ context.Context, id string) (llamacpp.StopResult, error) {
	return llamacpp.StopResult{Instance: instances.Instance{ID: id, State: instances.StateStopped}}, nil
}
func (stubLlamaRunner) TinyCompletion(context.Context, string, int) error        { return nil }
func (stubLlamaRunner) RecordPeakVRAM(context.Context, string, int) (int, error) { return 0, nil }

// proxyResidencyService builds a residency-wired Service for the /v1/* proxy handlers, seeding a
// runnable model row + (optionally) a HEALTHY warm instance pointing at the fake engine upstream.
// It returns the Server plus the in-flight tracker so tests can assert never-kill-active counting.
func proxyResidencyService(t *testing.T, upstream string, freeVRAM int, warm bool, verdict compat.Verdict) (*Server, *residency.InFlightTracker) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "apiproxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format, quantization, local_path, compat_status, presence)
		VALUES ('m1','Model One','local_path','gguf','q4_k_m','/tmp/m1.gguf','runs','present')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO engines (name, installed) VALUES ('llamacpp',1)`); err != nil {
		t.Fatal(err)
	}
	store := instances.NewStore(d.DB)
	if warm {
		if _, err := store.Create(context.Background(), instances.CreateParams{
			ID: "warm1", ModelID: "m1", Engine: instances.EngineLlamaCPP, VRAMEstMiB: 4000, Reason: "seed",
		}); err != nil {
			t.Fatal(err)
		}
		port := 40100
		if _, err := store.Transition(context.Background(), "warm1", instances.TransitionParams{
			To: instances.StateStarting, Reason: "seed", Port: &port, BaseURL: &upstream,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Transition(context.Background(), "warm1", instances.TransitionParams{
			To: instances.StateHealthy, Reason: "seed",
		}); err != nil {
			t.Fatal(err)
		}
	}

	snap := hardware.DefaultFakeSnapshot()
	snap.GPUs[0].VRAMFreeMiB = freeVRAM
	probe := &hardware.FakeProbe{Snap: snap}
	inflight := residency.NewInFlightTracker()
	clock := residency.RealClock()
	mgr := residency.NewManager(residency.ManagerConfig{
		Queue:           residency.NewQueue(residency.QueueConfig{}, clock),
		QueueStore:      residency.NewQueueStore(d.DB),
		Probe:           apiProxyProbe{p: probe},
		Lister:          residency.StoreLister{Store: store},
		Tracker:         inflight,
		Clock:           clock,
		SafetyMarginMiB: 1024,
	})
	svc := core.New(core.Deps{
		DB: d, Hardware: probe, Compat: &fakeProxyCompat{v: verdict},
		InstanceStore: store, Events: core.NewEventBus(), Cfg: config.Default(),
		Residency: mgr, InFlight: inflight, LlamaRunner: stubLlamaRunner{},
	})
	return &Server{svc: svc, logger: quietLogger()}, inflight
}

// fakeProxyCompat returns a canned verdict for the proxy admission path.
type fakeProxyCompat struct{ v compat.Verdict }

func (f *fakeProxyCompat) Evaluate(context.Context, compat.Request, hardware.Snapshot, compat.EngineAvailability) compat.Verdict {
	return f.v
}

// TestV1ChatCompletions_StreamsByteIdentity is the #p61 streaming gate at the HTTP boundary: a fake
// engine emits a fixed SSE byte sequence; the bytes the client receives through /v1/chat/completions
// MUST equal the upstream bytes exactly (bytes.Equal).
func TestV1ChatCompletions_StreamsByteIdentity(t *testing.T) {
	upstreamBytes := []byte(
		"data: {\"choices\":[{\"delta\":{\"content\":\"A\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"B\"}}]}\n\n" +
			"data: [DONE]\n\n")
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The proxy must relay to the completion endpoint (never a dev endpoint).
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			t.Errorf("engine hit unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write(upstreamBytes)
		if fl != nil {
			fl.Flush()
		}
	}))
	defer engine.Close()

	srv, _ := proxyResidencyService(t, engine.URL, 15200, true, compat.Verdict{Status: compat.StatusRuns, Reason: "ok", EstimatedVRAMMiB: 4000})
	rec := httptest.NewRecorder()
	req := jsonReq(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","stream":true,"messages":[]}`))
	srv.handleV1ChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(upstreamBytes, rec.Body.Bytes()) {
		t.Errorf("byte-identity FAILED.\nupstream: %q\nclient:   %q", upstreamBytes, rec.Body.Bytes())
	}
}

// TestV1Models_ListsRunnableOnly is the #p61 /v1/models gate at the HTTP boundary.
func TestV1Models_ListsRunnableOnly(t *testing.T) {
	srv, _ := proxyResidencyService(t, "http://127.0.0.1:1", 15200, false, compat.Verdict{Status: compat.StatusRuns})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	srv.handleV1Models(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}
	found := false
	for _, m := range resp.Data {
		if m.ID == "m1" {
			found = true
			if m.Object != "model" || m.OwnedBy != "localhub" {
				t.Errorf("bad model shape: %+v", m)
			}
		}
	}
	if !found {
		t.Error("runnable model m1 should appear in /v1/models")
	}
}

// TestV1ChatCompletions_Fast503OnBlock is the #p61 fast-503 gate: an un-fittable model returns 503
// with a numeric reason in the OpenAI error message, measured under 2s.
func TestV1ChatCompletions_Fast503OnBlock(t *testing.T) {
	// free 2000, footprint 9000 → queue → fast 503 (no warm instance, no idle victim).
	srv, _ := proxyResidencyService(t, "http://127.0.0.1:1", 2000, false,
		compat.Verdict{Status: compat.StatusRuns, Reason: "big", EstimatedVRAMMiB: 9000})
	rec := httptest.NewRecorder()
	req := jsonReq(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","messages":[]}`))
	start := time.Now()
	srv.handleV1ChatCompletions(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if elapsed >= 2*time.Second {
		t.Errorf("block took %v — must be < 2s (fast 503 gate)", elapsed)
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "admission_blocked" || env.Error.Type != "insufficient_vram" {
		t.Errorf("OpenAI error type/code = %q/%q", env.Error.Type, env.Error.Code)
	}
	if env.Error.Message == "" || !strings.Contains(env.Error.Message, "MiB") {
		t.Errorf("503 message must carry the numeric blocked_reason: %q", env.Error.Message)
	}
}

// TestV1ChatCompletions_404ModelNotFound is the #p61 model-not-found gate.
func TestV1ChatCompletions_404ModelNotFound(t *testing.T) {
	srv, _ := proxyResidencyService(t, "http://127.0.0.1:1", 15200, false, compat.Verdict{Status: compat.StatusRuns})
	rec := httptest.NewRecorder()
	req := jsonReq(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"no-such","messages":[]}`))
	srv.handleV1ChatCompletions(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "model_not_found" || env.Error.Type != "invalid_request_error" {
		t.Errorf("want model_not_found/invalid_request_error, got %q/%q", env.Error.Code, env.Error.Type)
	}
}

// TestV1ChatCompletions_400BadBody is the #p61 invalid-body gate (missing model).
func TestV1ChatCompletions_400BadBody(t *testing.T) {
	srv, _ := proxyResidencyService(t, "http://127.0.0.1:1", 15200, false, compat.Verdict{Status: compat.StatusRuns})
	rec := httptest.NewRecorder()
	req := jsonReq(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	srv.handleV1ChatCompletions(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_body") {
		t.Errorf("want invalid_body code: %s", rec.Body.String())
	}
}

// TestV1ChatCompletions_InFlightCountedDuringStream is the #p61 in-flight gate: while the proxied
// stream is open, the serving instance's ActiveCount > 0; after it completes, == 0.
func TestV1ChatCompletions_InFlightCountedDuringStream(t *testing.T) {
	midStream := make(chan struct{})
	proceed := make(chan struct{})
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"delta\":\"x\"}\n\n"))
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
	defer engine.Close()

	srv, inflight := proxyResidencyService(t, engine.URL, 15200, true, compat.Verdict{Status: compat.StatusRuns, Reason: "ok", EstimatedVRAMMiB: 4000})

	rec := httptest.NewRecorder()
	req := jsonReq(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","stream":true,"messages":[]}`))
	done := make(chan struct{})
	go func() {
		srv.handleV1ChatCompletions(rec, req)
		close(done)
	}()

	<-midStream
	during := inflight.ActiveCount("warm1")
	close(proceed)
	<-done
	after := inflight.ActiveCount("warm1")

	if during != 1 {
		t.Errorf("in-flight during proxied stream = %d, want 1 (never-kill-active gate)", during)
	}
	if after != 0 {
		t.Errorf("in-flight after stream = %d, want 0", after)
	}
}
