package vllm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/db"
	"github.com/localhub/localhub/internal/instances"
)

// --- fakes ---

// fakeDocker records every docker command and returns scripted output. It NEVER runs docker.
type fakeDocker struct {
	mu       sync.Mutex
	calls    [][]string
	psOutput string // what `docker ps` returns (the container name = still running; "" = gone)
	runErr   error  // error to return from `docker run`
	// removedAt records the call index after which `docker ps` reports gone (simulate rm).
	goneAfterRm bool
}

func (f *fakeDocker) Run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	if len(args) > 0 && args[0] == "run" {
		if f.runErr != nil {
			return "", f.runErr
		}
		return "containerid123\n", nil
	}
	if len(args) > 0 && (args[0] == "rm") {
		if f.goneAfterRm {
			f.psOutput = "" // container removed
		}
		return "", nil
	}
	if len(args) > 0 && args[0] == "ps" {
		return f.psOutput, nil
	}
	if len(args) > 0 && args[0] == "logs" {
		return "INFO: loading model...\nINFO: Uvicorn running\n", nil
	}
	return "", nil
}

func (f *fakeDocker) commandStrings() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}

func (f *fakeDocker) sawRunOrPull() bool {
	for _, c := range f.commandStrings() {
		if strings.HasPrefix(c, "run ") || strings.HasPrefix(c, "pull ") {
			return true
		}
	}
	return false
}

// scriptedVRAM rises after `after` reads (simulating VRAM freeing post-stop).
type scriptedVRAM struct {
	mu           sync.Mutex
	low, high    int
	after, calls int
}

func (s *scriptedVRAM) FreeVRAMMiB(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls >= s.after {
		return s.high, nil
	}
	return s.low, nil
}

// vllmStub serves /health (delayed-ready) + /v1/models + a minimal /v1/completions.
type vllmStub struct {
	mu         sync.Mutex
	healthAt   int // number of /health calls after which it returns 200 (delayed ready)
	calls      int
	neverReady bool
}

func (s *vllmStub) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls++
		ready := !s.neverReady && s.calls >= s.healthAt
		s.mu.Unlock()
		if ready {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "test-model", "object": "model"}},
		})
	})
	mux.HandleFunc("/v1/completions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"text": "ok"}},
		})
	})
	return httptest.NewServer(mux)
}

// vllmTestStore opens a migrated DB with a vLLM catalog model.
func vllmTestStore(t *testing.T) *instances.Store {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vllm.db")
	d, err := db.Open(p)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO models (id, display_name, source, format) VALUES ('vm','vLLM Model','huggingface','safetensors')`); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	return instances.NewStore(d.DB)
}

func portFromURL(u string) int {
	i := strings.LastIndex(u, ":")
	if i < 0 {
		return 0
	}
	n := 0
	for _, r := range u[i+1:] {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// --- tests ---

func TestDockerStart_HealthyOnStubReadiness(t *testing.T) {
	stub := &vllmStub{healthAt: 2} // ready on the 2nd /health poll
	srv := stub.server(t)
	defer srv.Close()
	store := vllmTestStore(t)
	fd := &fakeDocker{psOutput: "localhub-vllm-i1"} // container up
	r := NewRunner(ModeDocker, store, fd, nil, RunnerConfig{
		HealthTimeout: 5 * time.Second, HealthInterval: 50 * time.Millisecond,
	})
	// The stub IS the endpoint the runner health-polls; point HostPort at it.
	inst, err := r.Start(context.Background(), StartSpec{
		InstanceID: "i1", ModelID: "vm", Model: "Qwen/Qwen3-0.6B", HostPort: portFromURL(srv.URL),
		GPUMemUtil: 0.9, MaxModelLen: 8192,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if inst.State != instances.StateHealthy {
		t.Errorf("state: got %q want HEALTHY", inst.State)
	}
	// A docker run must have been issued (the container start).
	if !fd.sawRunOrPull() {
		t.Error("expected a `docker run` for docker-mode start")
	}
	// Launch flags recorded for reproducibility.
	if len(inst.LaunchFlags) == 0 || inst.LaunchFlags[0] != "docker" {
		t.Errorf("expected docker launch flags recorded, got %v", inst.LaunchFlags)
	}
}

func TestDockerStart_HealthTimeoutFailsAndKills(t *testing.T) {
	stub := &vllmStub{neverReady: true}
	srv := stub.server(t)
	defer srv.Close()
	store := vllmTestStore(t)
	fd := &fakeDocker{psOutput: "localhub-vllm-i1"}
	r := NewRunner(ModeDocker, store, fd, nil, RunnerConfig{
		HealthTimeout: 300 * time.Millisecond, HealthInterval: 50 * time.Millisecond,
	})
	_, err := r.Start(context.Background(), StartSpec{
		InstanceID: "i1", ModelID: "vm", Model: "m", HostPort: portFromURL(srv.URL),
	})
	if err == nil {
		t.Fatal("expected a health-timeout error")
	}
	// The instance must be FAILED and the container removed (`docker rm`).
	inst, _ := store.Get(context.Background(), "i1")
	if inst.State != instances.StateFailed {
		t.Errorf("state: got %q want FAILED", inst.State)
	}
	sawRm := false
	for _, c := range fd.commandStrings() {
		if strings.HasPrefix(c, "rm -f localhub-vllm-i1") {
			sawRm = true
		}
	}
	if !sawRm {
		t.Error("expected `docker rm -f` to kill the container on health timeout")
	}
}

func TestDockerStop_VerifiesContainerGoneAndVRAM(t *testing.T) {
	stub := &vllmStub{healthAt: 1}
	srv := stub.server(t)
	defer srv.Close()
	store := vllmTestStore(t)
	fd := &fakeDocker{psOutput: "localhub-vllm-i1", goneAfterRm: true}
	vram := &scriptedVRAM{low: 8000, high: 9000, after: 2}
	r := NewRunner(ModeDocker, store, fd, vram, RunnerConfig{
		HealthTimeout: 5 * time.Second, HealthInterval: 50 * time.Millisecond,
		StopVerifyTimeout: 3 * time.Second, StopVRAMDeltaSlackMiB: 500,
	})
	ctx := context.Background()
	if _, err := r.Start(ctx, StartSpec{InstanceID: "i1", ModelID: "vm", Model: "m", HostPort: portFromURL(srv.URL), VRAMEstMiB: 700}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	res, err := r.Stop(ctx, "i1")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !res.ContainerGone {
		t.Errorf("expected container gone, reason=%q", res.Reason)
	}
	if !res.Verified {
		t.Errorf("expected VRAM-delta verified, reason=%q freed=%d", res.Reason, res.FreedMiB)
	}
	// docker stop + rm must both have been issued.
	joined := strings.Join(fd.commandStrings(), "|")
	if !strings.Contains(joined, "stop localhub-vllm-i1") || !strings.Contains(joined, "rm -f localhub-vllm-i1") {
		t.Errorf("expected docker stop + rm; commands=%v", fd.commandStrings())
	}
}

func TestAttachMode_NeverIssuesDockerOrKill(t *testing.T) {
	stub := &vllmStub{healthAt: 1}
	srv := stub.server(t)
	defer srv.Close()
	store := vllmTestStore(t)
	fd := &fakeDocker{}
	// docker client present but must NOT be used in attach mode.
	r := NewRunner(ModeAttach, store, fd, nil, RunnerConfig{})
	ctx := context.Background()
	inst, err := r.Start(ctx, StartSpec{InstanceID: "a1", ModelID: "vm", AttachURL: srv.URL})
	if err != nil {
		t.Fatalf("attach Start: %v", err)
	}
	if inst.State != instances.StateHealthy {
		t.Errorf("state: got %q want HEALTHY", inst.State)
	}
	res, err := r.Stop(ctx, "a1")
	if err != nil {
		t.Fatalf("attach Stop: %v", err)
	}
	if res.ContainerGone {
		t.Error("attach stop must not claim a container was removed")
	}
	if len(fd.commandStrings()) != 0 {
		t.Errorf("attach mode must issue ZERO docker commands, got %v", fd.commandStrings())
	}
	if !strings.Contains(strings.ToLower(res.Reason), "not owned") {
		t.Errorf("attach stop reason should note the endpoint is not owned: %q", res.Reason)
	}
}

func TestSubprocessMode_WindowsNotImplemented(t *testing.T) {
	store := vllmTestStore(t)
	r := NewRunner(ModeSubprocess, store, nil, nil, RunnerConfig{})
	r.osOverride = "windows"
	_, err := r.Start(context.Background(), StartSpec{InstanceID: "s1", ModelID: "vm", Model: "m", HostPort: 40201})
	if err == nil {
		t.Fatal("expected notImplemented on Windows subprocess mode")
	}
	var nie *NotImplementedError
	if !asNotImpl(err, &nie) {
		t.Fatalf("expected *NotImplementedError, got %T: %v", err, err)
	}
	if !strings.Contains(nie.Reason, "no native Windows support") {
		t.Errorf("reason should cite no native Windows support: %q", nie.Reason)
	}
}

func TestNoDockerRunInTestsWithoutStart(t *testing.T) {
	// Sanity: constructing a runner and doing nothing issues no docker commands.
	fd := &fakeDocker{}
	_ = NewRunner(ModeDocker, vllmTestStore(t), fd, nil, RunnerConfig{})
	if fd.sawRunOrPull() {
		t.Error("no docker run/pull should occur on construction")
	}
}

// asNotImpl is errors.As for *NotImplementedError.
func asNotImpl(err error, target **NotImplementedError) bool {
	for err != nil {
		if nie, ok := err.(*NotImplementedError); ok {
			*target = nie
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
