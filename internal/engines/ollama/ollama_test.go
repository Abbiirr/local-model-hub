package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// canned CURRENT-shape responses, from the doc verification (2026-07-06). See the package
// doc comment for provenance.
const (
	versionJSON = `{"version":"0.21.2"}`
	tagsJSON    = `{"models":[
	  {"name":"deepseek-r1:latest","model":"deepseek-r1:latest","modified_at":"2025-05-10T08:06:48.6-07:00","size":4683075271,"digest":"0a8c2669","details":{"parent_model":"","format":"gguf","family":"qwen2","families":["qwen2"],"parameter_size":"7.6B","quantization_level":"Q4_K_M"}},
	  {"name":"qwen2.5:3b","model":"qwen2.5:3b","modified_at":"2025-05-10T08:06:48.6-07:00","size":1929922048,"digest":"abc123","details":{"parent_model":"","format":"gguf","family":"qwen2","families":["qwen2"],"parameter_size":"3.1B","quantization_level":"Q4_K_M"}}
	]}`
	psJSON = `{"models":[
	  {"name":"mistral:latest","model":"mistral:latest","size":5137025024,"digest":"2ae6f6dd","details":{"parent_model":"","format":"gguf","family":"llama","families":["llama"],"parameter_size":"7.2B","quantization_level":"Q4_0"},"expires_at":"2024-06-04T14:38:31.8-07:00","size_vram":5137025024}
	]}`
)

// fakeOllama serves the canned endpoints. Any unexpected path 404s.
func fakeOllama(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("mutating call to /api/version: %s", r.Method)
		}
		w.Write([]byte(versionJSON))
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("mutating call to /api/tags: %s", r.Method)
		}
		w.Write([]byte(tagsJSON))
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("mutating call to /api/ps: %s", r.Method)
		}
		w.Write([]byte(psJSON))
	})
	return httptest.NewServer(mux)
}

func TestDetect_DaemonUp(t *testing.T) {
	srv := fakeOllama(t)
	defer srv.Close()

	c := New(srv.URL, "") // no binary; daemon up is what matters
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st := c.Detect(ctx)

	if !st.Detected || !st.Reachable {
		t.Fatalf("want detected+reachable, got detected=%v reachable=%v reason=%q", st.Detected, st.Reachable, st.Reason)
	}
	if st.Version != "0.21.2" {
		t.Errorf("version: got %q want 0.21.2", st.Version)
	}
	if st.Reason == "" {
		t.Error("reason must never be empty")
	}
	if got := st.Details["installed_model_count"]; got != 2 {
		t.Errorf("installed_model_count: got %v want 2", got)
	}
	if got := st.Details["running_model_count"]; got != 1 {
		t.Errorf("running_model_count: got %v want 1", got)
	}
}

func TestListModels_CurrentShape(t *testing.T) {
	srv := fakeOllama(t)
	defer srv.Close()
	c := New(srv.URL, "")
	ctx := context.Background()

	models, err := c.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models want 2", len(models))
	}
	m := models[0]
	if m.Name != "deepseek-r1:latest" || m.Size != 4683075271 {
		t.Errorf("model[0] fields wrong: %+v", m)
	}
	if m.Details.Format != "gguf" || m.Details.QuantizationLevel != "Q4_K_M" {
		t.Errorf("details parsed wrong: %+v", m.Details)
	}
	if len(m.Details.Families) != 1 || m.Details.Families[0] != "qwen2" {
		t.Errorf("families parsed wrong: %+v", m.Details.Families)
	}
}

func TestListRunning_VRAMandExpiry(t *testing.T) {
	srv := fakeOllama(t)
	defer srv.Close()
	c := New(srv.URL, "")
	ctx := context.Background()

	ps, err := c.ListRunning(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 {
		t.Fatalf("got %d running want 1", len(ps))
	}
	m := ps[0]
	// size_vram and expires_at are the residency source-of-truth fields.
	if m.SizeVRAM != 5137025024 {
		t.Errorf("size_vram: got %d want 5137025024", m.SizeVRAM)
	}
	if m.ExpiresAt == "" {
		t.Error("expires_at not parsed")
	}
}

// TestDefensiveParsing: extra unknown fields ignored, missing optional fields stay zero.
func TestDefensiveParsing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"9.9.9","new_future_field":123}`))
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		// model missing size + details; extra field present.
		w.Write([]byte(`{"models":[{"name":"x","extra":true}],"future":"ok"}`))
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "")
	st := c.Detect(context.Background())
	if !st.Detected || st.Version != "9.9.9" {
		t.Fatalf("defensive parse failed: %+v", st)
	}
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].Name != "x" || models[0].Size != 0 {
		t.Errorf("missing fields should be zero: %+v", models)
	}
}

// TestDetect_DaemonDown_BinaryPresent: daemon unreachable but binary on disk ->
// Detected=true, Reachable=false, non-empty Reason. Uses this test file itself as a stand-in
// "binary" that exists on disk.
func TestDetect_DaemonDown_BinaryPresent(t *testing.T) {
	// point at a closed port so the HTTP call fails fast.
	c := New("http://127.0.0.1:1", currentTestFile())
	// short context so we don't wait on connection timeouts long.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st := c.Detect(ctx)

	if !st.Detected {
		t.Errorf("binary present should yield Detected=true, got %+v", st)
	}
	if st.Reachable {
		t.Errorf("daemon down should yield Reachable=false, got %+v", st)
	}
	if st.Reason == "" {
		t.Error("Reason must be non-empty on unreachable path (non-waivable)")
	}
	if !strings.Contains(st.Reason, "read-only") {
		t.Errorf("Reason should note read-only (no daemon start): %q", st.Reason)
	}
}

// TestDetect_DaemonDown_NoBinary: daemon unreachable AND binary absent -> Detected=false,
// non-empty Reason.
func TestDetect_DaemonDown_NoBinary(t *testing.T) {
	c := New("http://127.0.0.1:1", `Z:\does\not\exist\ollama.exe`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st := c.Detect(ctx)

	if st.Detected || st.Reachable {
		t.Errorf("no daemon + no binary should be not-detected, got %+v", st)
	}
	if st.Reason == "" {
		t.Error("Reason must be non-empty (non-waivable)")
	}
}

// currentTestFile returns a path known to exist on disk (this source file), used as a
// stand-in binary for the "installed but stopped" test.
func currentTestFile() string {
	// runtime.Caller path is absolute to this file.
	return testFilePath
}
