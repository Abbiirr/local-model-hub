package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/core"
	"github.com/localhub/localhub/internal/hardware"
)

// testMux builds the full routed + middleware-wrapped handler exactly as Server.New does,
// so tests exercise route precedence (specific API routes vs /api/ catch-all vs "/" static)
// the same way production does.
func testMux(t *testing.T) http.Handler {
	t.Helper()
	svc := core.New(core.Deps{
		Cfg:      config.Default(),
		DB:       migratedDB(t),
		Hardware: &hardware.FakeProbe{},
		Events:   core.NewEventBus(),
	})
	srv := &Server{svc: svc, logger: quietLogger()}
	mux := http.NewServeMux()
	srv.routes(mux)
	return recoverMiddleware(quietLogger(), logMiddleware(quietLogger(), mux))
}

// TestStaticServesIndexAtRoot asserts GET / returns the embedded dashboard HTML (the app
// root div, no CDN references) with a no-cache header.
func TestStaticServesIndexAtRoot(t *testing.T) {
	h := testMux(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="root"`) {
		t.Errorf("GET / body missing app root div; got: %s", truncate(body, 200))
	}
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Errorf("GET / body contains an external URL (CDN?); dashboard must be self-contained")
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("index.html Cache-Control = %q, want no-cache", cc)
	}
}

// TestStaticServesSPARouteButNotAPI asserts an unknown extension-less path (a client route)
// falls back to index.html, while /api/* NEVER falls back to the SPA shell.
func TestStaticServesSPARouteButNotAPI(t *testing.T) {
	h := testMux(t)

	// SPA route (no extension) → index.html.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/some/spa/route", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /some/spa/route status = %d, want 200 (SPA fallback)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Errorf("SPA route did not serve index.html shell")
	}

	// /api/* unknown path → JSON 404 envelope, NOT the SPA shell.
	arec := httptest.NewRecorder()
	h.ServeHTTP(arec, httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil))
	if arec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/does-not-exist status = %d, want 404", arec.Code)
	}
	if strings.Contains(arec.Body.String(), `id="root"`) {
		t.Errorf("/api/* fell back to the SPA shell; it must return the JSON envelope")
	}
}

// TestStaticServesHashedAssetImmutable asserts an existing hashed asset is served with the
// immutable cache header. It discovers the asset name from index.html so it stays valid as
// the hash changes across builds.
func TestStaticServesHashedAssetImmutable(t *testing.T) {
	h := testMux(t)

	// Read index.html to find a real hashed asset path (./assets/index-<hash>.js).
	irec := httptest.NewRecorder()
	h.ServeHTTP(irec, httptest.NewRequest(http.MethodGet, "/", nil))
	assetPath := extractAssetPath(irec.Body.String())
	if assetPath == "" {
		t.Skip("no hashed asset reference found in index.html; skipping asset cache check")
	}

	arec := httptest.NewRecorder()
	h.ServeHTTP(arec, httptest.NewRequest(http.MethodGet, assetPath, nil))
	if arec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", assetPath, arec.Code)
	}
	if cc := arec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("asset %s Cache-Control = %q, want immutable", assetPath, cc)
	}
}

// extractAssetPath pulls the first "/assets/....js" or ".css" path out of index.html's
// relative refs ("./assets/index-<hash>.js"), returning an absolute request path.
func extractAssetPath(html string) string {
	for _, marker := range []string{`./assets/`, `/assets/`} {
		i := strings.Index(html, marker)
		if i < 0 {
			continue
		}
		rest := html[i+len(marker):]
		end := strings.IndexAny(rest, `"'`)
		if end < 0 {
			continue
		}
		return "/assets/" + rest[:end]
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
