package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/manifest"
)

const testCommit = "e3a1c0f1234567890abcdef1234567890abcdef1"

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fakeHFServer serves the revision + tree + resolve endpoints for a small fake GGUF repo.
func fakeHFServer(t *testing.T, gguf []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/owner/tiny-GGUF/revision/main", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"sha":"` + testCommit + `","gated":false,"cardData":{"license":"apache-2.0"},` +
			`"siblings":[{"rfilename":"tiny-Q4_K_M.gguf"},{"rfilename":"README.md"}]}`))
	})
	mux.HandleFunc("/api/models/owner/tiny-GGUF/tree/"+testCommit, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"path":"tiny-Q4_K_M.gguf","type":"file","size":` + strconv.Itoa(len(gguf)) +
			`,"lfs":{"oid":"sha256:` + sha256hex(gguf) + `","size":` + strconv.Itoa(len(gguf)) + `}}]`))
	})
	mux.HandleFunc("/owner/tiny-GGUF/resolve/"+testCommit+"/tiny-Q4_K_M.gguf", func(w http.ResponseWriter, r *http.Request) {
		w.Write(gguf)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// supplyService builds a core.Service wired for the supply chain against a temp DB + download
// root, with the fake HF host installed.
func supplyService(t *testing.T, root string) *Service {
	t.Helper()
	d := coreTestDB(t)
	store := manifest.NewStore(d)
	dl := manifest.NewDownloader(root, manifest.GuardConfig{FreeDiskBytes: 100 << 30}, false, "")
	cfg := config.Default()
	cfg.Data.ModelDirs = []string{root}
	return New(Deps{
		Cfg:        cfg,
		DB:         d,
		Events:     NewEventBus(),
		Manifests:  store,
		Downloader: dl,
	})
}

// TestHF_TwoStepFlow proves the show-before-commit gate: without confirm, StartDownload
// resolves and STOPS at awaiting_ok with the pinned commit; with confirm, it downloads,
// verifies, writes an immutable manifest, and links a models row.
func TestHF_TwoStepFlow(t *testing.T) {
	gguf := []byte(strings.Repeat("TINYGGUF-", 4000)) // ~36 KB
	srv := fakeHFServer(t, gguf)
	old := manifest.HFHost
	manifest.HFHost = srv.URL
	defer func() { manifest.HFHost = old }()

	root := t.TempDir()
	svc := supplyService(t, root)
	ctx := context.Background()

	// Step 1: no confirm → resolve + show, do NOT download.
	job, err := svc.StartDownload(ctx, DownloadRequest{Kind: "hf", Repo: "owner/tiny-GGUF"})
	if err != nil {
		t.Fatalf("resolve step: %v", err)
	}
	if job.State != DownloadAwaitingOK {
		t.Fatalf("expected awaiting_ok, got %s (%s)", job.State, job.Reason)
	}
	if job.Commit != testCommit {
		t.Fatalf("commit not pinned in show step: %q", job.Commit)
	}
	if job.Resolved == nil || len(job.Resolved.Files) == 0 {
		t.Fatal("expected a resolved file listing in the show step")
	}
	// Nothing downloaded yet.
	if _, err := os.Stat(filepath.Join(root, "owner__tiny-GGUF", "tiny-Q4_K_M.gguf")); !os.IsNotExist(err) {
		t.Fatal("no bytes should be downloaded before confirm")
	}

	// Step 2: confirm → download.
	job2, err := svc.StartDownload(ctx, DownloadRequest{Kind: "hf", Repo: "owner/tiny-GGUF", Confirm: true})
	if err != nil {
		t.Fatalf("confirm step: %v", err)
	}
	waitJob(t, svc, job2.ID)

	final, _ := svc.DownloadStatus(job2.ID)
	if final.State != DownloadSucceeded {
		t.Fatalf("download did not succeed: %s (%s)", final.State, final.Reason)
	}
	if final.ManifestID == "" || final.ModelID == "" {
		t.Fatalf("expected manifest+model linkage, got manifest=%q model=%q", final.ManifestID, final.ModelID)
	}

	// The manifest is immutable, pins the commit, and verified the sha.
	m, err := svc.GetManifest(ctx, final.ManifestID)
	if err != nil || m == nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if m.Revision != testCommit {
		t.Fatalf("manifest revision not pinned: %q", m.Revision)
	}
	if len(m.Files) != 1 || m.Files[0].SHA256 != sha256hex(gguf) {
		t.Fatalf("manifest hash wrong: %+v", m.Files)
	}

	// The model row is linked to the manifest (gate #p33: provenance present).
	models, _ := svc.Models(ctx)
	var found bool
	for _, mm := range models {
		if mm.ID == final.ModelID {
			found = true
		}
	}
	if !found {
		t.Fatal("downloaded model not in inventory")
	}
	mf, err := svc.ManifestForModel(ctx, final.ModelID)
	if err != nil || mf == nil {
		t.Fatalf("downloaded model has no linked manifest — violates gate #p33: %v", err)
	}
}

// TestReproduce_HF verifies the reproduce export names the exact commit + hashes.
func TestReproduce_HF(t *testing.T) {
	gguf := []byte(strings.Repeat("X", 8192))
	srv := fakeHFServer(t, gguf)
	old := manifest.HFHost
	manifest.HFHost = srv.URL
	defer func() { manifest.HFHost = old }()

	root := t.TempDir()
	svc := supplyService(t, root)
	ctx := context.Background()
	job, _ := svc.StartDownload(ctx, DownloadRequest{Kind: "hf", Repo: "owner/tiny-GGUF", Confirm: true})
	waitJob(t, svc, job.ID)
	final, _ := svc.DownloadStatus(job.ID)

	b, err := svc.Reproduce(ctx, final.ModelID)
	if err != nil {
		t.Fatalf("Reproduce: %v", err)
	}
	if b.Source != "hf" || b.Commit != testCommit || b.Repo != "owner/tiny-GGUF" {
		t.Fatalf("reproduce bundle wrong: %+v", b)
	}
	if len(b.Files) != 1 || b.Files[0].SHA256 != sha256hex(gguf) {
		t.Fatalf("reproduce hashes wrong: %+v", b.Files)
	}
}

// TestLocalImport_ManifestAndWarning proves a local import writes a manifest with the
// origin-unverifiable warning and links inventory, and that hashing finalizes in the
// background.
func TestLocalImport_ManifestAndWarning(t *testing.T) {
	root := t.TempDir()
	svc := supplyService(t, root)
	ctx := context.Background()

	// Create a file INSIDE the model dir (containment).
	p := filepath.Join(root, "imported-Q8_0.gguf")
	content := []byte("this-is-an-imported-gguf-file")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}

	job, err := svc.StartDownload(ctx, DownloadRequest{Kind: "local_import", Path: p})
	if err != nil {
		t.Fatalf("local import: %v", err)
	}
	if job.ManifestID == "" || job.ModelID == "" {
		t.Fatal("local import should link a manifest + model immediately (provenance present)")
	}
	// Manifest exists immediately with the origin warning (even before hashing completes).
	m, _ := svc.GetManifest(ctx, job.ManifestID)
	if m == nil {
		t.Fatal("import manifest missing")
	}
	var hasOrigin bool
	for _, w := range m.SecurityWarnings {
		if w == manifest.WarnLocalImportOrigin {
			hasOrigin = true
		}
	}
	if !hasOrigin {
		t.Fatalf("import manifest missing origin warning: %v", m.SecurityWarnings)
	}
	// Wait for the background hash to finalize.
	waitJob(t, svc, job.ID)
	m2, _ := svc.GetManifest(ctx, job.ManifestID)
	if m2.HashPending {
		t.Fatal("expected hash finalized after background job")
	}
	if len(m2.Files) == 0 || m2.Files[0].SHA256 != sha256hex(content) {
		t.Fatalf("import hash not finalized correctly: %+v", m2.Files)
	}
}

// TestLocalImport_ContainmentRefused proves a path OUTSIDE the model dirs is refused.
func TestLocalImport_ContainmentRefused(t *testing.T) {
	root := t.TempDir()
	svc := supplyService(t, root)
	// A file outside the model dir.
	outside := filepath.Join(t.TempDir(), "outside.gguf")
	_ = os.WriteFile(outside, []byte("x"), 0o644)
	_, err := svc.StartDownload(context.Background(), DownloadRequest{Kind: "local_import", Path: outside})
	if err == nil {
		t.Fatal("expected containment refusal for a path outside configured model dirs")
	}
}

func TestOllamaPullIsRetired(t *testing.T) {
	svc := supplyService(t, t.TempDir())
	_, err := svc.StartDownload(context.Background(), DownloadRequest{Kind: "ollama", Name: "mistral:latest"})
	if err == nil || !strings.Contains(err.Error(), "ollama downloads are retired") {
		t.Fatalf("StartDownload(ollama) error = %v, want clear retirement refusal", err)
	}
}

// TestReproduce_DiscoveredModelUnknownProvenance proves the gate-#p33 distinction: a model
// discovered on disk with NO manifest reports provenance "unknown (pre-existing local file)".
func TestReproduce_DiscoveredModelUnknownProvenance(t *testing.T) {
	root := t.TempDir()
	svc := supplyService(t, root)
	ctx := context.Background()
	// Insert a discovered model with no manifest_id (as the inventory scanner would).
	if _, err := svc.deps.DB.ExecContext(ctx,
		`INSERT INTO models (id, display_name, source, format, local_path) VALUES ('disc','Discovered','local_path','gguf',?)`,
		filepath.Join(root, "disc.gguf")); err != nil {
		t.Fatalf("seed discovered model: %v", err)
	}
	b, err := svc.Reproduce(ctx, "disc")
	if err != nil {
		t.Fatalf("Reproduce discovered: %v", err)
	}
	if b.Source != "unknown" || !strings.Contains(b.Provenance, "pre-existing local file") {
		t.Fatalf("discovered model should have unknown provenance, got %+v", b)
	}
	// And it has no linked manifest.
	mf, _ := svc.ManifestForModel(ctx, "disc")
	if mf != nil {
		t.Fatal("discovered model should have no manifest")
	}
}

// waitJob blocks until a job reaches a terminal state or times out.
func waitJob(t *testing.T, svc *Service, id string) {
	t.Helper()
	for i := 0; i < 400; i++ { // up to ~4s
		if j, ok := svc.DownloadStatus(id); ok {
			if j.State == DownloadSucceeded || j.State == DownloadFailed {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach a terminal state in time", id)
}
