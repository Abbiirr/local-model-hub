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

	"github.com/coder/websocket"

	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/core"
	"github.com/localhub/localhub/internal/db"
	"github.com/localhub/localhub/internal/hardware"
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

func newFakeHardwareTestService(t *testing.T) *core.Service {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return core.New(core.Deps{Cfg: config.Default(), DB: d, Hardware: &hardware.FakeProbe{}})
}

// TestStartInstanceRequest_AllowCPUOffloadDecodes verifies the transport contract handleInstanceCreate
// relies on: the POST /api/instances body decodes allow_cpu_offload into StartInstanceRequest, and an
// omitted field defaults to false (full GPU — backward compatible). This is the exact json.Decode the
// handler performs before calling svc.StartInstance.
func TestStartInstanceRequest_AllowCPUOffloadDecodes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"omitted defaults false (full gpu)", `{"model_id":"m"}`, false},
		{"explicit true", `{"model_id":"m","allow_cpu_offload":true}`, true},
		{"explicit false", `{"model_id":"m","allow_cpu_offload":false}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req core.StartInstanceRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if req.AllowCPUOffload != tc.want {
				t.Errorf("AllowCPUOffload = %v, want %v", req.AllowCPUOffload, tc.want)
			}
			if req.ModelID != "m" {
				t.Errorf("ModelID = %q, want m", req.ModelID)
			}
		})
	}
}

// TestStartInstanceRequest_SpeculativeDecodes verifies the transport contract for the per-start
// speculative knob: the POST /api/instances body decodes `speculative` into StartInstanceRequest,
// and an omitted field defaults to "" (off — backward compatible). REST and JSON-RPC share this
// struct, so both transports get the field for free.
func TestStartInstanceRequest_SpeculativeDecodes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"omitted defaults off", `{"model_id":"m"}`, ""},
		{"ngram-cache", `{"model_id":"m","speculative":"ngram-cache"}`, "ngram-cache"},
		{"explicit empty", `{"model_id":"m","speculative":""}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req core.StartInstanceRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if req.Speculative != tc.want {
				t.Errorf("Speculative = %q, want %q", req.Speculative, tc.want)
			}
		})
	}
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

// TestAuthMiddlewareEnforcesAdminToken covers security.md item 3: when server.admin_token
// is set, /api/* requires `Authorization: Bearer <token>`; GET /api/health stays open for
// liveness probes; with no token configured the chain is untouched.
func TestAuthMiddlewareEnforcesAdminToken(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("no token configured: passthrough", func(t *testing.T) {
		h := authMiddleware("", next)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	h := authMiddleware("s3cret", next)

	t.Run("missing header on /api → 401 with reason", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		var env ErrorEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if env.Error.Reason == "" {
			t.Error("401 must carry a human-readable reason")
		}
	})

	t.Run("wrong token → 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/instances/abc", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("correct token → pass", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("GET /api/health exempt", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("static UI exempt", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
}

func TestDashboardAuthKeepsBearerOnRESTAndCookieOnlyOnLiveEndpoints(t *testing.T) {
	srv := &Server{adminToken: "s3cret"}
	login := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/dashboard", nil)
	loginReq.Header.Set("Authorization", "Bearer s3cret")
	srv.handleDashboardAuthLogin(login, loginReq)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != dashboardLiveCookie {
		t.Fatalf("login cookie = %#v, want one %q cookie", cookies, dashboardLiveCookie)
	}
	if cookies[0].Value == "s3cret" || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unsafe dashboard cookie: %#v", cookies[0])
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := authMiddleware("s3cret", next)

	liveReq := httptest.NewRequest(http.MethodGet, "/api/ws", nil)
	liveReq.AddCookie(cookies[0])
	liveRec := httptest.NewRecorder()
	h.ServeHTTP(liveRec, liveReq)
	if liveRec.Code != http.StatusOK {
		t.Errorf("live endpoint with dashboard cookie status = %d, want 200", liveRec.Code)
	}

	restReq := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	restReq.AddCookie(cookies[0])
	restRec := httptest.NewRecorder()
	h.ServeHTTP(restRec, restReq)
	if restRec.Code != http.StatusUnauthorized {
		t.Errorf("REST endpoint accepted dashboard cookie: status = %d, want 401", restRec.Code)
	}

	badLogin := httptest.NewRecorder()
	badLoginReq := httptest.NewRequest(http.MethodPost, "/api/auth/dashboard", nil)
	badLoginReq.Header.Set("Authorization", "Bearer cluster-token")
	srv.handleDashboardAuthLogin(badLogin, badLoginReq)
	if badLogin.Code != http.StatusUnauthorized {
		t.Errorf("non-admin bearer minted dashboard cookie: status = %d, want 401", badLogin.Code)
	}
}

func TestDashboardLoginAuthorizesRealWebSocketButNotREST(t *testing.T) {
	svc := newFakeHardwareTestService(t)
	srv, err := New(svc, quietLogger(), "127.0.0.1", 0, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	}()
	base := "http://" + srv.Addr()
	loginReq, _ := http.NewRequest(http.MethodPost, base+"/api/auth/dashboard", nil)
	loginReq.Header.Set("Authorization", "Bearer s3cret")
	loginRes, err := http.DefaultClient.Do(loginReq)
	if err != nil {
		t.Fatal(err)
	}
	defer loginRes.Body.Close()
	if loginRes.StatusCode != http.StatusOK || len(loginRes.Cookies()) != 1 {
		t.Fatalf("dashboard login = %d, cookies=%v", loginRes.StatusCode, loginRes.Cookies())
	}
	cookie := loginRes.Cookies()[0]

	// The same cookie must not authenticate an ordinary REST request.
	restReq, _ := http.NewRequest(http.MethodGet, base+"/api/models", nil)
	restReq.AddCookie(cookie)
	restRes, err := http.DefaultClient.Do(restReq)
	if err != nil {
		t.Fatal(err)
	}
	restRes.Body.Close()
	if restRes.StatusCode != http.StatusUnauthorized {
		t.Fatalf("REST with dashboard cookie = %d, want 401", restRes.StatusCode)
	}

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/api/ws"
	wsCtx, wsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wsCancel()
	conn, _, err := websocket.Dial(wsCtx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": []string{cookie.String()}}})
	if err != nil {
		t.Fatalf("dashboard cookie did not authorize websocket: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")
}

// TestServerBindsLoopbackOnly starts a real server on 127.0.0.1:0 and asserts the listen
// address is a loopback address — the core security invariant (TESTING.md#p11).
func TestServerBindsLoopbackOnly(t *testing.T) {
	svc := newTestService(t)
	srv, err := New(svc, quietLogger(), "127.0.0.1", 0, "") // port 0 = OS picks a free port
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
