package manifest

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
)

// rangeServer serves `payload` and HONORS a Range request (206 with the remaining bytes),
// exactly as HF's resolve endpoint does. It records whether a Range header was received.
func rangeServer(t *testing.T, payload []byte) (*httptest.Server, *bool) {
	t.Helper()
	sawRange := new(bool)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rg := r.Header.Get("Range"); rg != "" {
			*sawRange = true
			// Parse "bytes=<off>-".
			off := 0
			if strings.HasPrefix(rg, "bytes=") {
				spec := strings.TrimPrefix(rg, "bytes=")
				spec = strings.TrimSuffix(spec, "-")
				if n, err := strconv.Atoi(spec); err == nil {
					off = n
				}
			}
			if off < 0 || off > len(payload) {
				off = 0
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)-off))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[off:])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv, sawRange
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestFetch_FullDownloadAndVerify(t *testing.T) {
	payload := []byte(strings.Repeat("localhub-download-test-", 5000)) // ~115 KB
	srv, _ := rangeServer(t, payload)
	dest := filepath.Join(t.TempDir(), "model.gguf")

	f := &FileFetcher{}
	res, err := f.Fetch(context.Background(), srv.URL+"/model.gguf", dest, sha256hex(payload), nil, 1, 1)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.SHA256 != sha256hex(payload) {
		t.Fatalf("hash mismatch: %s", res.SHA256)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatal("downloaded content differs")
	}
	// The .part temp file must be gone (renamed).
	if _, err := os.Stat(dest + tmpSuffix); !os.IsNotExist(err) {
		t.Fatal("expected .part temp to be renamed away")
	}
}

// TestFetch_Resume simulates a killed download: pre-seed a .part file with a prefix of the
// payload, then Fetch must Range-resume, append the rest, and produce the correct full hash.
// This is the P3.1 resume evidence at unit level.
func TestFetch_Resume(t *testing.T) {
	payload := []byte(strings.Repeat("abcdefghij", 10000)) // 100 KB
	srv, sawRange := rangeServer(t, payload)
	dir := t.TempDir()
	dest := filepath.Join(dir, "model.gguf")

	// Pre-seed a partial .part with the first 40 KB.
	prefix := payload[:40000]
	if err := os.WriteFile(dest+tmpSuffix, prefix, 0o644); err != nil {
		t.Fatalf("seed part: %v", err)
	}

	f := &FileFetcher{}
	res, err := f.Fetch(context.Background(), srv.URL+"/model.gguf", dest, sha256hex(payload), nil, 1, 1)
	if err != nil {
		t.Fatalf("Fetch (resume): %v", err)
	}
	if !*sawRange {
		t.Fatal("expected a Range request to be sent for resume")
	}
	if !res.Resumed {
		t.Fatal("expected FetchResult.Resumed=true")
	}
	if res.SHA256 != sha256hex(payload) {
		t.Fatalf("resumed hash mismatch: got %s want %s", res.SHA256, sha256hex(payload))
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("resumed content differs (len got=%d want=%d)", len(got), len(payload))
	}
}

// TestDownloadGGUFURL_OverCapDeletesFile is the FINDING-1 gate: a direct-URL GGUF has no trusted
// size up front, so the hard cap can only be enforced AFTER Fetch finalized the file on disk. The
// post-hoc check must DELETE the over-cap artifact (not just claim it in the reason) so it cannot
// fill the model volume. Assert the finalized file is absent afterward AND the reason is honest.
func TestDownloadGGUFURL_OverCapDeletesFile(t *testing.T) {
	payload := []byte(strings.Repeat("x", 4096)) // 4 KiB — small, but we set a tiny cap below it.
	srv, _ := rangeServer(t, payload)
	dir := t.TempDir()

	d := &Downloader{
		Fetcher:  &FileFetcher{},
		DestRoot: dir,
		Guard:    GuardConfig{HardCapBytes: 1024}, // 1 KiB cap → the 4 KiB download blows it.
	}
	_, err := d.DownloadGGUFURL(context.Background(), GGUFURLRequest{URL: srv.URL + "/model.gguf", Filename: "model.gguf"}, nil)
	if err == nil {
		t.Fatal("expected an over-cap error")
	}
	if !strings.Contains(err.Error(), "hard cap") || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("expected an honest over-cap + removed reason, got %v", err)
	}
	// The finalized file must be gone — not left to wedge the disk.
	dest := filepath.Join(dir, "model.gguf")
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("expected the over-cap file %s to be deleted; it is still on disk", dest)
	}
	// And no orphaned .part either.
	if _, statErr := os.Stat(dest + tmpSuffix); !os.IsNotExist(statErr) {
		t.Fatal("expected no orphaned .part after over-cap removal")
	}
}

// TestFetch_HashMismatchDeletesPartial verifies a bad expected-hash removes the temp file so a
// retry re-downloads cleanly.
func TestFetch_HashMismatchDeletesPartial(t *testing.T) {
	payload := []byte("some-bytes-here")
	srv, _ := rangeServer(t, payload)
	dest := filepath.Join(t.TempDir(), "m.gguf")
	f := &FileFetcher{}
	_, err := f.Fetch(context.Background(), srv.URL+"/m.gguf", dest, "deadbeefwronghash", nil, 1, 1)
	if err == nil {
		t.Fatal("expected a hash-mismatch error")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected sha256 mismatch error, got %v", err)
	}
	if _, statErr := os.Stat(dest + tmpSuffix); !os.IsNotExist(statErr) {
		t.Fatal("expected the corrupt .part to be deleted on hash mismatch")
	}
}
