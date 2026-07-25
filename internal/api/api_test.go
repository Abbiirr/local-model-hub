package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
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

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestService(t *testing.T) *core.Service {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return core.New(core.Deps{Cfg: config.Default(), DB: d})
}

func TestErrorEnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusNotFound, "not_found", "model missing", "no model with id foo")

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if env.Error.Code != "not_found" {
		t.Errorf("code = %q, want not_found", env.Error.Code)
	}
	if env.Error.Reason == "" {
		t.Error("reason is empty; spec-digest §3 mandates a human-readable reason")
	}
	// The top-level key MUST be "error" per the mandatory contract.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["error"]; !ok {
		t.Errorf("envelope missing top-level \"error\" key: %s", rec.Body.String())
	}
}

func TestHealthHandlerOK(t *testing.T) {
	svc := newTestService(t)
	srv := &Server{svc: svc, logger: quietLogger()}

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	srv.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var res core.HealthResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.Status != "ok" {
		t.Errorf("status = %q, want ok", res.Status)
	}
	if !res.DBOK {
		t.Error("db_ok = false, want true")
	}
	if res.Version == "" {
		t.Error("version empty")
	}
}

// TestServerBindsLoopbackOnly starts a real server on 127.0.0.1:0 and asserts the listen
// address is a loopback address — the core security invariant (TESTING.md#p11).
func TestServerBindsLoopbackOnly(t *testing.T) {
	svc := newTestService(t)
	srv, err := New(svc, quietLogger(), "127.0.0.1", 0) // port 0 = OS picks a free port
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	host, _, err := splitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("addr %q: %v", srv.Addr(), err)
	}
	if !isLoopbackHost(host) {
		t.Errorf("server bound to %q, which is not loopback", host)
	}

	// Bring it up and hit the health endpoint over the real socket.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not shut down within 5s")
		}
	}()

	url := "http://" + srv.Addr() + "/api/health"
	var resp *http.Response
	// Small retry loop while the goroutine starts serving.
	for i := 0; i < 50; i++ {
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want 200", resp.StatusCode)
	}
}

// splitHostPort is a tiny wrapper so the test does not import net directly for one call.
func splitHostPort(hostport string) (string, string, error) {
	i := strings.LastIndex(hostport, ":")
	if i < 0 {
		return hostport, "", nil
	}
	host := strings.Trim(hostport[:i], "[]")
	return host, hostport[i+1:], nil
}

func isLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
}
