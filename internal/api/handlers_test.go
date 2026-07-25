package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/core"
	"github.com/localhub/localhub/internal/db"
	"github.com/localhub/localhub/internal/hardware"
	"github.com/localhub/localhub/internal/inventory"
)

// serviceWith builds a service core over a fresh migrated DB plus whatever deps the test
// supplies via the mutator. The FakeProbe gives deterministic hardware without a real GPU.
func serviceWith(t *testing.T, mutate func(*core.Deps)) (*core.Service, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	deps := core.Deps{
		Cfg:      config.Default(),
		DB:       d,
		Hardware: &hardware.FakeProbe{},
	}
	if mutate != nil {
		mutate(&deps)
	}
	return core.New(deps), d
}

func TestHandleHardware(t *testing.T) {
	svc, _ := serviceWith(t, nil)
	srv := &Server{svc: svc, logger: quietLogger()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/hardware", nil)
	srv.handleHardware(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var snap hardware.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(snap.GPUs) != 1 || snap.GPUs[0].VRAMTotalMiB != 16380 {
		t.Errorf("unexpected GPU snapshot: %+v", snap.GPUs)
	}
	// Spec §3 key shape: top-level gpus/cpu/ram/disk must be present.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"gpus", "cpu", "ram", "disk"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("hardware response missing key %q", k)
		}
	}
}

// TestHandleHardwareLiveSSE asserts SSE framing and at least 2 ticks under a short interval.
func TestHandleHardwareLiveSSE(t *testing.T) {
	cfg := config.Default()
	cfg.Hardware.PollIntervalSeconds = 1 // ticker floor is 1s; we count the immediate + >=1 tick
	svc := core.New(core.Deps{
		Cfg:      cfg,
		DB:       migratedDB(t),
		Hardware: &hardware.FakeProbe{},
	})
	// Route through the FULL middleware chain (recover -> log -> mux) so the test catches a
	// wrapped ResponseWriter that doesn't forward Flush — the real failure mode that a
	// handler-only test misses. handlerWithMiddleware mirrors Server.New's chain.
	srv := &Server{svc: svc, logger: quietLogger()}
	mux := http.NewServeMux()
	srv.routes(mux)
	chained := recoverMiddleware(quietLogger(), logMiddleware(quietLogger(), mux))
	ts := httptest.NewServer(chained)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/hardware/live", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET SSE: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}

	// Read frames until we have collected 2 complete "data: ...\n\n" events or time out.
	sc := bufio.NewScanner(resp.Body)
	frames := 0
	deadline := time.After(2500 * time.Millisecond)
	lines := make(chan string, 64)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
loop:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				break loop
			}
			if strings.HasPrefix(line, "data: ") {
				payload := strings.TrimPrefix(line, "data: ")
				var snap hardware.Snapshot
				if err := json.Unmarshal([]byte(payload), &snap); err != nil {
					t.Errorf("frame payload not valid Snapshot JSON: %v (%q)", err, payload)
				}
				frames++
				if frames >= 2 {
					break loop
				}
			}
		case <-deadline:
			break loop
		}
	}
	cancel() // disconnect the client to trigger handler teardown
	if frames < 2 {
		t.Errorf("received %d SSE frames, want >= 2 (immediate + >=1 tick)", frames)
	}
}

func migratedDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

// TestHandleEnginesAllDisabled uses an empty registry (all engines nil) so the sweep is
// deterministic and makes ZERO external calls: every engine reports installed=false with a
// reason. This exercises the projection + route without touching a real engine.
func TestHandleEnginesAllDisabled(t *testing.T) {
	svc, _ := serviceWith(t, func(d *core.Deps) {
		d.Engines = core.EnginesRegistry{} // all nil
	})
	srv := &Server{svc: svc, logger: quietLogger()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/engines", nil)
	srv.handleEngines(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var views []core.EngineStatusView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(views) != 3 {
		t.Fatalf("engines = %d, want 3 (ollama/llamacpp/vllm)", len(views))
	}
	names := map[string]bool{}
	for _, v := range views {
		names[v.Name] = true
		if v.Installed {
			t.Errorf("engine %q reported installed with an empty registry", v.Name)
		}
		if v.Reason == "" {
			t.Errorf("engine %q has empty reason (non-waivable)", v.Name)
		}
	}
	for _, want := range []string{"ollama", "llamacpp", "vllm"} {
		if !names[want] {
			t.Errorf("engines response missing %q", want)
		}
	}
}

// TestHandleModelsAndRescan wires a real scanner over a fixture GGUF tree (no engines, no
// hashing) and asserts: rescan inserts the fixtures, GET /api/models returns them, a partial
// download is excluded, and a re-scan is idempotent (no duplicate rows).
func TestHandleModelsAndRescan(t *testing.T) {
	root := buildFixtureModels(t)

	var repo *inventory.Repository
	svc, d := serviceWith(t, func(dp *core.Deps) {
		repo = inventory.NewRepository(dp.DB)
		dp.Inventory = inventory.New([]string{root}) // no Ollama source
		dp.InvRepo = repo
	})
	_ = d
	srv := &Server{svc: svc, logger: quietLogger()}

	// Rescan.
	rrec := httptest.NewRecorder()
	rreq := httptest.NewRequest(http.MethodPost, "/api/models/rescan", nil)
	srv.handleModelsRescan(rrec, rreq)
	if rrec.Code != http.StatusOK {
		t.Fatalf("rescan status = %d, want 200; body=%s", rrec.Code, rrec.Body.String())
	}
	var summary core.RescanResult
	if err := json.Unmarshal(rrec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	if summary.Inserted != 2 {
		t.Errorf("inserted = %d, want 2 (two standalone GGUFs)", summary.Inserted)
	}
	if summary.SkippedCount < 1 {
		t.Errorf("skipped = %d, want >=1 (the .part partial)", summary.SkippedCount)
	}
	if summary.OllamaScanned {
		t.Error("OllamaScanned = true, want false (no Ollama source wired)")
	}

	// GET /api/models.
	mrec := httptest.NewRecorder()
	mreq := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	srv.handleModels(mrec, mreq)
	if mrec.Code != http.StatusOK {
		t.Fatalf("models status = %d, want 200; body=%s", mrec.Code, mrec.Body.String())
	}
	var models []core.ModelSummary
	if err := json.Unmarshal(mrec.Body.Bytes(), &models); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2; %+v", len(models), models)
	}
	for _, m := range models {
		if m.Presence != "present" {
			t.Errorf("model %q presence = %q, want present", m.ID, m.Presence)
		}
		if m.Format != "gguf" {
			t.Errorf("model %q format = %q, want gguf", m.ID, m.Format)
		}
	}

	// Re-scan: idempotent (0 inserts, count unchanged).
	rrec2 := httptest.NewRecorder()
	srv.handleModelsRescan(rrec2, httptest.NewRequest(http.MethodPost, "/api/models/rescan", nil))
	var summary2 core.RescanResult
	if err := json.Unmarshal(rrec2.Body.Bytes(), &summary2); err != nil {
		t.Fatal(err)
	}
	if summary2.Inserted != 0 {
		t.Errorf("second rescan inserted = %d, want 0 (idempotent)", summary2.Inserted)
	}
	if summary2.ModelCount != 2 {
		t.Errorf("second rescan model_count = %d, want 2", summary2.ModelCount)
	}
}

// buildFixtureModels creates a tiny GGUF tree: two valid single-file GGUFs (with the GGUF
// magic so the scanner accepts them) and one partial download (must be skipped).
func buildFixtureModels(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// GGUF magic "GGUF" + version 3 little-endian, padded — enough for a header sniff.
	header := append([]byte("GGUF"), 0x03, 0x00, 0x00, 0x00)
	body := append(header, make([]byte, 64)...)
	for _, name := range []string{"tiny-model-q4_k_m.gguf", "small-model-q8_0.gguf"} {
		if err := os.WriteFile(filepath.Join(root, name), body, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// Partial download — must be excluded and logged.
	if err := os.WriteFile(filepath.Join(root, "big-model-q4_k_m.gguf.part"), body, 0o644); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	return root
}
