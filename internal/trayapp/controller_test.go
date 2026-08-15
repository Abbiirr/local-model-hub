package trayapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/startupconfig"
)

type fakeRunner struct {
	mu         sync.Mutex
	startName  string
	startArgs  []string
	startError error

	spawnDir   string
	spawnName  string
	spawnArgs  []string
	spawnCalls int
	spawnError error
}

func (f *fakeRunner) Start(name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startName = name
	f.startArgs = append([]string(nil), args...)
	return f.startError
}

func (f *fakeRunner) StartIn(dir, name string, args ...string) (*os.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spawnCalls++
	f.spawnDir = dir
	f.spawnName = name
	f.spawnArgs = append([]string(nil), args...)
	if f.spawnError != nil {
		return nil, f.spawnError
	}
	// A handle for a process that is not ours; nothing in these tests signals it.
	return &os.Process{Pid: -1}, nil
}

func (f *fakeRunner) spawns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawnCalls
}

// The bug that made the tray unusable: the daemon was already running, but startup unconditionally
// tried to start it anyway and never returned. Health is checked first, and an answering daemon
// means no spawn at all.
func TestStartLocalHubAdoptsAlreadyRunningDaemon(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()

	controller, err := New(t.TempDir(), hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	controller.runner = runner
	if err := controller.StartLocalHub(context.Background()); err != nil {
		t.Fatalf("adopting a healthy daemon must succeed: %v", err)
	}
	if runner.spawns() != 0 {
		t.Fatalf("spawned %d daemons; a healthy daemon must never be started again", runner.spawns())
	}
	if controller.DaemonOwned() {
		t.Error("a daemon the tray did not start must not be reported as tray-owned")
	}
}

// No PowerShell, no script: the daemon executable is launched directly, in the repo root.
func TestStartLocalHubSpawnsDaemonDirectlyWithoutPowerShell(t *testing.T) {
	root := t.TempDir()
	daemon := filepath.Join(root, "bin", "localhub.exe")
	if err := os.MkdirAll(filepath.Dir(daemon), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemon, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}

	healthy := make(chan struct{})
	var once sync.Once
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			http.NotFound(w, r)
			return
		}
		select {
		case <-healthy:
			w.WriteHeader(http.StatusOK)
		default:
			// First poll: not up yet, so the caller must wait rather than assume success.
			once.Do(func() { close(healthy) })
			http.Error(w, "starting", http.StatusServiceUnavailable)
		}
	}))
	defer hub.Close()

	controller, err := New(root, hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	controller.runner = runner
	if err := controller.StartLocalHub(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.spawnName != daemon {
		t.Fatalf("spawned %q, want the daemon at %q", runner.spawnName, daemon)
	}
	// Compare the executable itself, not the path: t.TempDir() embeds this test's name.
	if executable := strings.ToLower(filepath.Base(runner.spawnName)); executable != "localhub.exe" {
		t.Fatalf("launched %q; the tray must exec the daemon directly, not a shell", executable)
	}
	for _, arg := range runner.spawnArgs {
		if strings.Contains(strings.ToLower(arg), ".ps1") {
			t.Fatalf("argument %q references a PowerShell script", arg)
		}
	}
	if runner.spawnDir != controller.repoRoot {
		t.Fatalf("working directory = %q, want %q", runner.spawnDir, controller.repoRoot)
	}
	if !reflect.DeepEqual(runner.spawnArgs, []string{"-config", "configs/config.yaml"}) {
		t.Fatalf("args = %q", runner.spawnArgs)
	}
	if !controller.DaemonOwned() {
		t.Error("a daemon the tray started must be reported as tray-owned")
	}
}

// A daemon that never becomes healthy must fail with a bounded, readable error rather than parking
// the caller forever — that wait is what froze the tray menu.
func TestStartLocalHubGivesUpWhenDaemonNeverAnswers(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "localhub.exe"), []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer hub.Close()

	controller, err := New(root, hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	controller.runner = &fakeRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- controller.StartLocalHub(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a failure when the daemon never becomes healthy")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("StartLocalHub ignored its context deadline and hung")
	}
}

func TestStartLocalHubReportsMissingExecutable(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer hub.Close()
	controller, err := New(t.TempDir(), hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	controller.runner = &fakeRunner{}
	err = controller.StartLocalHub(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v, want a reason naming the paths searched", err)
	}
	// Every user-facing failure has to say where to look.
	for _, candidate := range controller.DaemonCandidates() {
		if strings.Contains(err.Error(), filepath.Base(candidate)) {
			return
		}
	}
	t.Fatalf("error does not name any searched path: %v", err)
}

func TestStatusReportsControlPlaneHealth(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()

	controller, err := New(t.TempDir(), hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	if status := controller.Status(context.Background()); !status.LocalHubHealthy {
		t.Fatalf("unexpected status: %+v", status)
	}

	hub.Close()
	if status := controller.Status(context.Background()); status.LocalHubHealthy {
		t.Fatalf("unreachable daemon reported healthy: %+v", status)
	}
}

// Exit stops models through the drain-aware API, then terminates the exact daemon identified by
// LocalHub health. The production terminator additionally verifies the executable path.
func TestShutdownAllStopsInstancesAndAdoptedDaemon(t *testing.T) {
	var deleted []string
	var terminatedPID int
	live := true
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/instances":
			if live {
				_, _ = w.Write([]byte(`[{"id":"live-1","state":"HEALTHY"},{"id":"old-1","state":"STOPPED"}]`))
			} else {
				_, _ = w.Write([]byte(`[{"id":"live-1","state":"STOPPED"},{"id":"old-1","state":"STOPPED"}]`))
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/health":
			_, _ = w.Write([]byte(`{"status":"ok","pid":4242}`))
		case r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			live = false
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	controller, err := New(t.TempDir(), hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	controller.runner = &fakeRunner{}
	controller.terminator = func(pid int, _ []string) error {
		terminatedPID = pid
		hub.Close()
		return nil
	}
	if err := controller.ShutdownAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deleted, []string{"/api/instances/live-1"}) {
		t.Fatalf("deleted instances = %q, want only the live one", deleted)
	}
	if terminatedPID != 4242 {
		t.Fatalf("terminated pid = %d, want health-reported daemon pid 4242", terminatedPID)
	}
}

// Exiting must not be blocked by a daemon that is already gone.
func TestShutdownAllSucceedsWhenDaemonIsAlreadyDown(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	hub.Close()

	controller, err := New(t.TempDir(), hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	controller.runner = &fakeRunner{}
	if err := controller.ShutdownAll(context.Background()); err != nil {
		t.Fatalf("shutdown with no reachable daemon must succeed: %v", err)
	}
}

func TestStartConfiguredModelUsesActionDeadlineNotHealthProbeTimeout(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/instances":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/instances":
			time.Sleep(75 * time.Millisecond)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	controller, err := New(t.TempDir(), hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	controller.client.Timeout = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = controller.StartConfiguredModel(ctx, startupconfig.StartupModelConfig{
		ModelID: "large-model", Mode: startupconfig.StartupModeLocalHubAPI,
	})
	if err != nil {
		t.Fatalf("cold model start inherited the short health-probe timeout: %v", err)
	}
}

func TestOpenWebUIUsesURLHandler(t *testing.T) {
	controller, err := New(t.TempDir(), "http://127.0.0.1:39876")
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	controller.runner = runner
	if err := controller.OpenWebUI(); err != nil {
		t.Fatal(err)
	}
	want := []string{"url.dll,FileProtocolHandler", "http://127.0.0.1:39876"}
	if runner.startName != "rundll32.exe" || !reflect.DeepEqual(runner.startArgs, want) {
		t.Fatalf("start = %q %q, want rundll32.exe %q", runner.startName, runner.startArgs, want)
	}
}

func TestModelsReturnsOnlyPresentInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[
			{"id":"gone","display_name":"Gone","presence":"missing","local_path":"C:\\\\gone.gguf"},
			{"id":"qwen","display_name":"Qwen","presence":"present","local_path":"C:\\\\qwen.gguf"}
		]`))
	}))
	defer server.Close()

	controller, err := New(t.TempDir(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	models, err := controller.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "qwen" || models[0].DisplayName != "Qwen" {
		t.Fatalf("models = %#v, want only present Qwen", models)
	}
}

func TestRunStartupMakesOneNonBlockingInitialAttempt(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/instances":
			_, _ = w.Write([]byte("[]"))
		case r.Method == http.MethodPost && r.URL.Path == "/api/instances":
			attempts++
			http.Error(w, "temporary startup failure", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	controller, err := New(t.TempDir(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := startupconfig.DefaultStartupConfig()
	cfg.HealthCheckSeconds = 1
	cfg.Models = []startupconfig.StartupModelConfig{{
		ModelID:     "x-model",
		Enabled:     true,
		Order:       0,
		RetryPolicy: startupconfig.RetryLimited,
		MaxAttempts: 3,
		Mode:        startupconfig.StartupModeLocalHubAPI,
	}}
	results := controller.RunStartup(context.Background(), cfg)
	if len(results) != 1 || results[0].Started || attempts != 1 {
		t.Fatalf("results = %#v, attempts=%d", results, attempts)
	}
}

func TestRunStartupNeverRetries(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/instances":
			_, _ = w.Write([]byte("[]"))
		case r.Method == http.MethodPost && r.URL.Path == "/api/instances":
			attempts++
			http.Error(w, "permanent startup failure", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	controller, err := New(t.TempDir(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := startupconfig.DefaultStartupConfig()
	cfg.HealthCheckSeconds = 1
	cfg.Models = []startupconfig.StartupModelConfig{{
		ModelID:     "x-model",
		Enabled:     true,
		Order:       0,
		RetryPolicy: startupconfig.RetryNever,
		MaxAttempts: 0,
		Mode:        startupconfig.StartupModeLocalHubAPI,
	}}
	results := controller.RunStartup(context.Background(), cfg)
	if len(results) != 1 || results[0].Started || attempts != 1 {
		t.Fatalf("results = %#v, attempts=%d", results, attempts)
	}
}
