package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/localhub/localhub/internal/core"
	"github.com/localhub/localhub/internal/db"
	"github.com/localhub/localhub/internal/manifest"
)

// routedServer builds a full routed test server (needed for the {id} path routes) over the
// given service.
func routedServer(t *testing.T, svc *core.Service) *httptest.Server {
	t.Helper()
	srv := &Server{svc: svc, logger: quietLogger()}
	mux := http.NewServeMux()
	srv.routes(mux)
	chained := recoverMiddleware(quietLogger(), logMiddleware(quietLogger(), mux))
	ts := httptest.NewServer(chained)
	t.Cleanup(ts.Close)
	return ts
}

// TestDownloadCreate_BadKind returns a readable error envelope (no opaque 500).
func TestDownloadCreate_BadKind(t *testing.T) {
	var database *db.DB
	svc, d := serviceWith(t, func(dp *core.Deps) {
		dp.Manifests = manifest.NewStore(dp.DB)
	})
	database = d
	_ = database
	ts := routedServer(t, svc)

	body, _ := json.Marshal(core.DownloadRequest{Kind: "nonsense"})
	resp, err := http.Post(ts.URL+"/api/downloads", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var env ErrorEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Error.Reason == "" {
		t.Fatal("expected a human-readable reason on the error envelope")
	}
}

// TestManifestGet_And_Reproduce exercises the manifest + reproduce routes over a seeded
// manifest+model.
func TestManifestGet_And_Reproduce(t *testing.T) {
	var dbh *db.DB
	svc, d := serviceWith(t, func(dp *core.Deps) {
		dp.Manifests = manifest.NewStore(dp.DB)
	})
	dbh = d
	ctx := context.Background()

	// Seed a manifest via the store (immutable Put) + a linked model row.
	store := manifest.NewStore(dbh)
	m := &manifest.Manifest{
		ID: "man-1", Source: manifest.SourceHF, Repo: "owner/model",
		Revision: "e3a1c0f1234567890abcdef1234567890abcdef1", Format: "gguf",
		Files: []manifest.File{{Path: "m.gguf", SHA256: "abc", SizeBytes: 100}},
	}
	if err := store.Put(ctx, m); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO models (id, display_name, source, format, manifest_id) VALUES ('mdl','Model','huggingface','gguf','man-1')`); err != nil {
		t.Fatalf("seed model: %v", err)
	}

	ts := routedServer(t, svc)

	// GET /api/manifests/{id}
	resp, _ := http.Get(ts.URL + "/api/manifests/man-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest get status = %d", resp.StatusCode)
	}
	var got manifest.Manifest
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Revision != m.Revision {
		t.Fatalf("manifest round-trip wrong: %+v", got)
	}

	// GET /api/models/{id}/reproduce
	resp2, _ := http.Get(ts.URL + "/api/models/mdl/reproduce")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("reproduce status = %d", resp2.StatusCode)
	}
	var b manifest.ReproduceBundle
	_ = json.NewDecoder(resp2.Body).Decode(&b)
	resp2.Body.Close()
	if b.Commit != m.Revision || b.Source != "hf" {
		t.Fatalf("reproduce bundle wrong: %+v", b)
	}

	// GET /api/models/{id}/manifest
	resp3, _ := http.Get(ts.URL + "/api/models/mdl/manifest")
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("model manifest status = %d", resp3.StatusCode)
	}
	resp3.Body.Close()
}
