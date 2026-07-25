package manifest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeHF serves the HF revision + tree metadata endpoints (and the resolve download URL) for
// a small fake repo. The commit SHA it returns for "main" is fixed so tests can assert pinning.
const fakeCommit = "e3a1c0f1234567890abcdef1234567890abcdef1"

func fakeHF(t *testing.T, gguf []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// revision → sha + siblings + license.
	mux.HandleFunc("/api/models/owner/model-GGUF/revision/main", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"sha":"` + fakeCommit + `","gated":false,` +
			`"cardData":{"license":"apache-2.0"},` +
			`"siblings":[{"rfilename":"model-Q4_K_M.gguf"},{"rfilename":"README.md"}]}`))
	})
	// tree → size + lfs sha.
	mux.HandleFunc("/api/models/owner/model-GGUF/tree/"+fakeCommit, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"path":"model-Q4_K_M.gguf","type":"file","size":` + itoa(len(gguf)) +
			`,"lfs":{"oid":"sha256:` + sha256hex(gguf) + `","size":` + itoa(len(gguf)) + `}},` +
			`{"path":"README.md","type":"file","size":10}]`))
	})
	// resolve download URL (Range-capable).
	mux.HandleFunc("/owner/model-GGUF/resolve/"+fakeCommit+"/model-Q4_K_M.gguf", func(w http.ResponseWriter, r *http.Request) {
		if rg := r.Header.Get("Range"); rg != "" {
			w.WriteHeader(http.StatusPartialContent)
		}
		w.Write(gguf)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestResolveRevision_PinsCommit(t *testing.T) {
	gguf := []byte(strings.Repeat("G", 2048))
	srv := fakeHF(t, gguf)
	oldHost := HFHost
	HFHost = srv.URL
	defer func() { HFHost = oldHost }()

	c := &HFClient{}
	info, err := c.ResolveRevision(context.Background(), "owner/model-GGUF", "main")
	if err != nil {
		t.Fatalf("ResolveRevision: %v", err)
	}
	if info.CommitSHA != fakeCommit {
		t.Fatalf("commit not pinned: got %q want %q", info.CommitSHA, fakeCommit)
	}
	if info.License != "apache-2.0" {
		t.Fatalf("license not read: %q", info.License)
	}
	// The GGUF file should carry its size + LFS sha from the tree listing.
	var found bool
	for _, f := range info.Files {
		if f.Path == "model-Q4_K_M.gguf" {
			found = true
			if f.LFSSHA256 != sha256hex(gguf) {
				t.Fatalf("lfs sha not enriched: %q", f.LFSSHA256)
			}
			if f.SizeBytes != int64(len(gguf)) {
				t.Fatalf("size not enriched: %d", f.SizeBytes)
			}
		}
	}
	if !found {
		t.Fatal("expected the gguf file in the listing")
	}
}

// TestDownloadHF_EndToEnd exercises resolve → download → verify → manifest with the fake HF
// server. It asserts the manifest pins the commit, has per-file hashes, and its GGUF hash
// matches the LFS sha (verification happened).
func TestDownloadHF_EndToEnd(t *testing.T) {
	gguf := []byte(strings.Repeat("GGUFDATA-", 3000)) // ~27 KB
	srv := fakeHF(t, gguf)
	oldHost := HFHost
	HFHost = srv.URL
	defer func() { HFHost = oldHost }()

	root := t.TempDir()
	d := NewDownloader(root, GuardConfig{FreeDiskBytes: 100 << 30}, false, "")
	info, err := d.ResolveHF(context.Background(), "owner/model-GGUF", "main")
	if err != nil {
		t.Fatalf("ResolveHF: %v", err)
	}
	m, err := d.DownloadHF(context.Background(), info, []string{"model-Q4_K_M.gguf"}, nil)
	if err != nil {
		t.Fatalf("DownloadHF: %v", err)
	}
	if m.Source != SourceHF || m.Revision != fakeCommit {
		t.Fatalf("manifest not pinned: %+v", m)
	}
	if len(m.Files) != 1 || m.Files[0].SHA256 != sha256hex(gguf) {
		t.Fatalf("manifest file hash wrong: %+v", m.Files)
	}
	if m.Format != "gguf" || m.Quantization != "q4_k_m" {
		t.Fatalf("format/quant not derived: %q %q", m.Format, m.Quantization)
	}
	// The file landed inside the download root (containment).
	if err := ValidateWithin(m.LocalPath, []string{root}); err != nil {
		t.Fatalf("downloaded file not inside root: %v", err)
	}
}

// TestDownloadHF_GuardRefusesOversize verifies the hard cap refuses a too-big download BEFORE
// fetching bytes.
func TestDownloadHF_GuardRefusesOversize(t *testing.T) {
	gguf := []byte("small")
	srv := fakeHF(t, gguf)
	oldHost := HFHost
	HFHost = srv.URL
	defer func() { HFHost = oldHost }()

	root := t.TempDir()
	// Hard cap of 1 byte → any real file is refused. Fake the size via metadata.
	d := NewDownloader(root, GuardConfig{HardCapBytes: 1, FreeDiskBytes: 100 << 30}, false, "")
	info, _ := d.ResolveHF(context.Background(), "owner/model-GGUF", "main")
	_, err := d.DownloadHF(context.Background(), info, []string{"model-Q4_K_M.gguf"}, nil)
	if err == nil || !strings.Contains(err.Error(), "hard cap") {
		t.Fatalf("expected hard-cap refusal, got %v", err)
	}
}
