package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDaemon is a stateful fake Ollama daemon: it tracks which models are "resident" and echoes
// keep_alive on warm, and simulates a DELAYED unload (the model stays in /api/ps for a couple of
// polls after keep_alive:0, mirroring the real daemon's asynchronous free — the lmwrapper lesson).
type fakeDaemon struct {
	mu             sync.Mutex
	resident       map[string]int64 // model tag -> size_vram bytes
	unloadRequests map[string]int   // model -> how many /api/ps polls remain before it disappears
	lastWarmBody   map[string]any   // last decoded /api/generate warm body
	embedOnly      map[string]bool  // models that 400 on /api/generate (embedding-only)
	genCalls       int
	psCalls        int
	embedCalls     int
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{resident: map[string]int64{}, unloadRequests: map[string]int{}, embedOnly: map[string]bool{}}
}

func (f *fakeDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"0.21.2"}`))
	})
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("/api/generate must be POST, got %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		model, _ := req["model"].(string)
		f.mu.Lock()
		// Embedding-only models 400 on /api/generate for a WARM (no keep_alive:0). An unload
		// (keep_alive:0) still succeeds for them (matches the real daemon behavior 2026-07-06).
		isUnload := false
		if ka, ok := req["keep_alive"]; ok {
			if n, isNum := ka.(float64); isNum && n == 0 {
				isUnload = true
			}
		}
		if f.embedOnly[model] && !isUnload {
			f.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": `"` + model + `" does not support generate`})
			return
		}
		f.genCalls++
		f.lastWarmBody = req
		doneReason := "load"
		// keep_alive:0 → unload request (delayed disappearance).
		if ka, ok := req["keep_alive"]; ok {
			if n, isNum := ka.(float64); isNum && n == 0 {
				doneReason = "unload"
				if _, res := f.resident[model]; res {
					f.unloadRequests[model] = 2 // stays resident for 2 more /api/ps polls
				}
			}
		}
		if doneReason == "load" {
			if _, ok := f.resident[model]; !ok {
				f.resident[model] = 700 * 1024 * 1024 // 700 MiB default footprint
			}
		}
		f.mu.Unlock()
		resp := map[string]any{"model": model, "response": "", "done": true, "done_reason": doneReason}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("/api/embed must be POST, got %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		model, _ := req["model"].(string)
		f.mu.Lock()
		f.embedCalls++
		unload := false
		if ka, ok := req["keep_alive"]; ok {
			if n, isNum := ka.(float64); isNum && n == 0 {
				unload = true
			}
		}
		if unload {
			delete(f.resident, model)
		} else if _, ok := f.resident[model]; !ok {
			f.resident[model] = 900 * 1024 * 1024
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"model": model, "embeddings": []any{}})
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.psCalls++
		type psEntry struct {
			Name      string `json:"name"`
			Model     string `json:"model"`
			SizeVRAM  int64  `json:"size_vram"`
			ExpiresAt string `json:"expires_at"`
		}
		var models []psEntry
		for tag, sz := range f.resident {
			// Decrement pending unloads; drop the model when the countdown hits zero.
			if n, ok := f.unloadRequests[tag]; ok {
				if n <= 0 {
					delete(f.resident, tag)
					delete(f.unloadRequests, tag)
					continue
				}
				f.unloadRequests[tag] = n - 1
			}
			models = append(models, psEntry{Name: tag, Model: tag, SizeVRAM: sz, ExpiresAt: "2026-07-06T20:00:00Z"})
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
	})
	return httptest.NewServer(mux)
}

func TestWarm_LoadsModel(t *testing.T) {
	f := newFakeDaemon()
	srv := f.server(t)
	defer srv.Close()

	c := New(srv.URL, "")
	res, err := c.Warm(context.Background(), "llama3.2", "10m", nil)
	if err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if res.DoneReason != "load" {
		t.Errorf("done_reason: got %q want load", res.DoneReason)
	}
	// Warm sends a duration string keep_alive → JSON string.
	f.mu.Lock()
	ka := f.lastWarmBody["keep_alive"]
	_, hasOpts := f.lastWarmBody["options"]
	f.mu.Unlock()
	if ka != "10m" {
		t.Errorf("keep_alive sent: got %v want \"10m\"", ka)
	}
	// numGPU nil → NO options object (daemon dynamic default).
	if hasOpts {
		t.Errorf("expected no options object when numGPU is nil; got %v", f.lastWarmBody["options"])
	}
}

// TestWarm_NumGPUForcesFullOffload asserts a non-nil numGPU is sent as options.num_gpu (the
// full-GPU default path forces all layers onto the GPU). Regression guard for the
// "never offload to CPU" directive.
func TestWarm_NumGPUForcesFullOffload(t *testing.T) {
	f := newFakeDaemon()
	srv := f.server(t)
	defer srv.Close()
	c := New(srv.URL, "")

	if _, err := c.Warm(context.Background(), "llama3.2", "10m", FullGPULayers()); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	f.mu.Lock()
	opts, ok := f.lastWarmBody["options"].(map[string]any)
	f.mu.Unlock()
	if !ok {
		t.Fatalf("expected an options object carrying num_gpu; got body %v", f.lastWarmBody)
	}
	n, ok := opts["num_gpu"].(float64)
	if !ok || n != 999 {
		t.Errorf("num_gpu: got %v (%T) want 999 (force all layers on GPU)", opts["num_gpu"], opts["num_gpu"])
	}
	// The model is now resident.
	resident, ps, err := c.IsResident(context.Background(), "llama3.2")
	if err != nil || !resident {
		t.Fatalf("expected model resident after warm; resident=%v err=%v", resident, err)
	}
	if ps.SizeVRAM == 0 {
		t.Error("expected non-zero size_vram for resident model")
	}
}

func TestWarm_EmbeddingModelFallsBackToEmbed(t *testing.T) {
	f := newFakeDaemon()
	f.embedOnly["nomic-embed:latest"] = true // 400s on /api/generate
	srv := f.server(t)
	defer srv.Close()
	c := New(srv.URL, "")

	res, err := c.Warm(context.Background(), "nomic-embed:latest", "5m", nil)
	if err != nil {
		t.Fatalf("Warm embed model: %v", err)
	}
	if res.DoneReason != "load" {
		t.Errorf("done_reason: got %q want load (from embed fallback)", res.DoneReason)
	}
	f.mu.Lock()
	embedCalls := f.embedCalls
	f.mu.Unlock()
	if embedCalls == 0 {
		t.Error("expected the /api/embed fallback to be called for an embedding-only model")
	}
	resident, _, _ := c.IsResident(context.Background(), "nomic-embed:latest")
	if !resident {
		t.Error("embedding model should be resident after the embed-fallback warm")
	}
}

func TestWarm_PinSendsNegativeOne(t *testing.T) {
	f := newFakeDaemon()
	srv := f.server(t)
	defer srv.Close()
	c := New(srv.URL, "")
	if _, err := c.Warm(context.Background(), "llama3.2", "-1", nil); err != nil {
		t.Fatalf("Warm pin: %v", err)
	}
	f.mu.Lock()
	ka := f.lastWarmBody["keep_alive"]
	f.mu.Unlock()
	// -1 is numeric → JSON number, decodes to float64(-1).
	if n, ok := ka.(float64); !ok || n != -1 {
		t.Errorf("keep_alive for pin: got %v (%T) want -1 number", ka, ka)
	}
}

func TestUnload_PollsUntilGone(t *testing.T) {
	f := newFakeDaemon()
	srv := f.server(t)
	defer srv.Close()
	c := New(srv.URL, "")

	// Warm first so the model is resident with a footprint.
	if _, err := c.Warm(context.Background(), "mistral:latest", "", nil); err != nil {
		t.Fatalf("Warm: %v", err)
	}

	res, err := c.Unload(context.Background(), "mistral:latest", UnloadConfig{
		PollTimeout: 5 * time.Second, PollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Unload: %v", err)
	}
	if !res.Gone {
		t.Errorf("expected Gone=true after poll, got reason=%q", res.Reason)
	}
	if res.DoneReason != "unload" {
		t.Errorf("done_reason: got %q want unload", res.DoneReason)
	}
	if res.FreedVRAMHint == 0 {
		t.Error("expected a non-zero FreedVRAMHint (footprint captured before unload)")
	}
	if res.WaitedFor <= 0 {
		t.Error("expected to have polled for a measurable duration (delayed unload)")
	}
	// The daemon must have been polled more than once (delayed disappearance).
	f.mu.Lock()
	polls := f.psCalls
	f.mu.Unlock()
	if polls < 2 {
		t.Errorf("expected multiple /api/ps polls for delayed unload, got %d", polls)
	}
}

func TestUnload_NotResidentIsImmediateGone(t *testing.T) {
	f := newFakeDaemon()
	srv := f.server(t)
	defer srv.Close()
	c := New(srv.URL, "")
	res, err := c.Unload(context.Background(), "ghost:latest", UnloadConfig{PollTimeout: time.Second})
	if err != nil {
		t.Fatalf("Unload: %v", err)
	}
	if !res.Gone {
		t.Errorf("expected Gone=true for a model that was never resident, got %q", res.Reason)
	}
}

func TestUnload_TimeoutWhenModelStays(t *testing.T) {
	// A daemon that NEVER drops the model → Unload reports Gone=false honestly (not a false 200).
	mux := http.NewServeMux()
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "stuck", "done": true, "done_reason": "unload"})
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
			{"name": "stuck", "model": "stuck", "size_vram": int64(500 * 1024 * 1024), "expires_at": "2026-07-06T20:00:00Z"},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, "")
	res, err := c.Unload(context.Background(), "stuck", UnloadConfig{PollTimeout: 300 * time.Millisecond, PollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("Unload: %v", err)
	}
	if res.Gone {
		t.Error("expected Gone=false when the model never leaves /api/ps")
	}
	if !strings.Contains(res.Reason, "still present") {
		t.Errorf("timeout reason should note the model is still present, got %q", res.Reason)
	}
}

func TestWarm_DaemonDown_TypedError(t *testing.T) {
	// Point at a closed server so the transport fails.
	srv := httptest.NewServer(http.NewServeMux())
	url := srv.URL
	srv.Close()
	c := New(url, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.Warm(ctx, "llama3.2", "", nil)
	if err == nil {
		t.Fatal("expected an error when the daemon is down")
	}
	var de *DaemonUnreachableError
	if !asDaemon(err, &de) {
		t.Fatalf("expected *DaemonUnreachableError, got %T: %v", err, err)
	}
	if de.Op != "warm" {
		t.Errorf("op: got %q want warm", de.Op)
	}
	if !strings.Contains(de.Error(), "never starts or kills") {
		t.Errorf("reason should state the never-kill-daemon policy, got %q", de.Error())
	}
}

// asDaemon is errors.As for *DaemonUnreachableError (local helper to avoid importing errors here).
func asDaemon(err error, target **DaemonUnreachableError) bool {
	for err != nil {
		if de, ok := err.(*DaemonUnreachableError); ok {
			*target = de
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
