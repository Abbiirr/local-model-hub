package manifest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file is the file-download engine shared by the HF and single-GGUF-URL paths
// (spec-digest §7 / TESTING.md#p31). Each download:
//   - streams to a TEMP file (<dest>.part) and atomically renames to <dest> on success, so a
//     killed daemon never leaves a truncated file masquerading as complete;
//   - computes sha256 as bytes stream in (no second read pass), verifying against an
//     HF-published LFS sha256 when one is available (mismatch => refuse + delete);
//   - RESUMES a partial <dest>.part via an HTTP Range request when the server supports it
//     (verified against the resumed 206 response), continuing the running hash from the bytes
//     already on disk. If the server ignores Range (200 not 206), it restarts cleanly.
//
// Progress is reported through a ProgressFunc callback the core forwards onto the event bus
// (download.progress). Cancellation is via ctx.

// ProgressFunc is called during a download with cumulative bytes for one file. total is the
// expected size (0 if unknown). It must be cheap and non-blocking (the core publishes to a
// best-effort event bus).
type ProgressFunc func(p Progress)

// Progress is one progress update for a file within a download.
type Progress struct {
	File       string `json:"file"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"` // 0 if unknown
	FileIndex  int    `json:"file_index"`  // 1-based
	FileCount  int    `json:"file_count"`
	Resumed    bool   `json:"resumed"` // true when this file resumed a .part
	Phase      string `json:"phase"`   // "downloading" | "verifying" | "done" | "hashing"
}

// FetchResult is the outcome of downloading one file.
type FetchResult struct {
	Path      string // absolute destination path
	SHA256    string // computed lowercase hex
	SizeBytes int64
	Resumed   bool
}

// FileFetcher downloads individual files with resume + streamed hashing.
type FileFetcher struct {
	HTTP  *http.Client
	Token string // optional bearer token (HF gated repos)
}

// tmpSuffix is appended to the destination for the in-progress temp file. Resume looks for it.
const tmpSuffix = ".part"

// Fetch downloads url → dest with resume + streamed sha256. expectedSHA (lowercase hex, or
// "") is verified on completion when non-empty. It creates parent dirs as needed. On any
// error the temp file is left in place (so a retry can resume it) UNLESS the error is a hash
// mismatch, in which case the corrupt temp file is removed.
func (f *FileFetcher) Fetch(ctx context.Context, url, dest, expectedSHA string, prog ProgressFunc, idx, count int) (FetchResult, error) {
	return f.fetch(ctx, url, dest, expectedSHA, prog, idx, count, 0)
}

// FetchWithLimit is Fetch with a hard cumulative byte limit. It aborts before
// writing a chunk that would exceed limitBytes.
func (f *FileFetcher) FetchWithLimit(ctx context.Context, url, dest, expectedSHA string, prog ProgressFunc, idx, count int, limitBytes int64) (FetchResult, error) {
	return f.fetch(ctx, url, dest, expectedSHA, prog, idx, count, limitBytes)
}

func (f *FileFetcher) fetch(ctx context.Context, url, dest, expectedSHA string, prog ProgressFunc, idx, count int, limitBytes int64) (FetchResult, error) {
	tmp := dest + tmpSuffix
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return FetchResult{}, fmt.Errorf("create dir for %s: %w", dest, err)
	}

	// Determine resume offset from an existing temp file.
	var offset int64
	h := sha256.New()
	if fi, err := os.Stat(tmp); err == nil && fi.Size() > 0 {
		// Re-hash the bytes already on disk so the running hash matches after resume.
		if err := rehashExisting(tmp, h); err != nil {
			// Can't trust the partial; start over.
			_ = os.Remove(tmp)
			offset = 0
			h = sha256.New()
		} else {
			offset = fi.Size()
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return FetchResult{}, err
	}
	if f.Token != "" {
		req.Header.Set("Authorization", "Bearer "+f.Token)
	}
	resumed := false
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	client := f.HTTP
	if client == nil {
		client = &http.Client{Timeout: 0} // no global timeout; ctx governs cancellation
	}
	resp, err := client.Do(req)
	if err != nil {
		return FetchResult{}, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	// Open the temp file for append (resume) or create/truncate (fresh).
	var out *os.File
	switch resp.StatusCode {
	case http.StatusPartialContent: // 206 — server honored Range; append.
		resumed = offset > 0
		out, err = os.OpenFile(tmp, os.O_WRONLY|os.O_APPEND, 0o644)
	case http.StatusOK: // 200 — server ignored Range (or fresh); restart cleanly.
		if offset > 0 {
			// Range was ignored: discard the running hash and start from zero.
			h = sha256.New()
			offset = 0
		}
		out, err = os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return FetchResult{}, fmt.Errorf("GET %s returned HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err != nil {
		return FetchResult{}, fmt.Errorf("open temp %s: %w", tmp, err)
	}

	total := offset + resp.ContentLength // ContentLength is remaining bytes on a 206
	if resp.ContentLength < 0 {
		total = 0
	}

	written, err := streamCopy(ctx, out, resp.Body, h, offset, total, prog, fileName(dest), resumed, idx, count, limitBytes)
	closeErr := out.Close()
	if err != nil {
		if limitBytes > 0 && strings.Contains(err.Error(), "hard cap") {
			_ = os.Remove(tmp)
			return FetchResult{}, fmt.Errorf("%w; removed the partial file", err)
		}
		return FetchResult{}, err // temp left in place for resume
	}
	if closeErr != nil {
		return FetchResult{}, fmt.Errorf("close temp %s: %w", tmp, closeErr)
	}

	sum := hex.EncodeToString(h.Sum(nil))
	if expectedSHA != "" && !strings.EqualFold(sum, strings.TrimPrefix(expectedSHA, "sha256:")) {
		_ = os.Remove(tmp) // corrupt; remove so a retry re-downloads cleanly
		return FetchResult{}, fmt.Errorf(
			"sha256 mismatch for %s: got %s, expected %s (deleted the partial; re-download to retry)",
			fileName(dest), sum, expectedSHA)
	}

	if err := os.Rename(tmp, dest); err != nil {
		return FetchResult{}, fmt.Errorf("finalize %s: %w", dest, err)
	}
	if prog != nil {
		prog(Progress{File: fileName(dest), BytesDone: written, BytesTotal: written, FileIndex: idx, FileCount: count, Phase: "done", Resumed: resumed})
	}
	return FetchResult{Path: dest, SHA256: sum, SizeBytes: written, Resumed: resumed}, nil
}

// streamCopy copies src→dst while updating hash h and reporting progress, honoring ctx
// cancellation between chunks. startOffset is the bytes already present (for progress
// accounting on resume). Returns the FINAL total size on disk.
func streamCopy(ctx context.Context, dst io.Writer, src io.Reader, h hash.Hash, startOffset, total int64, prog ProgressFunc, name string, resumed bool, idx, count int, limitBytes int64) (int64, error) {
	buf := make([]byte, 1<<20) // 1 MiB chunks
	done := startOffset
	mw := io.MultiWriter(dst, h)
	lastReport := time.Now()
	for {
		select {
		case <-ctx.Done():
			return done, ctx.Err()
		default:
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if limitBytes > 0 && done+int64(n) > limitBytes {
				return done, fmt.Errorf("download %s exceeds the %d byte hard cap; aborted before writing beyond the limit", name, limitBytes)
			}
			if _, werr := mw.Write(buf[:n]); werr != nil {
				return done, fmt.Errorf("write %s: %w", name, werr)
			}
			done += int64(n)
			if prog != nil && time.Since(lastReport) > 250*time.Millisecond {
				prog(Progress{File: name, BytesDone: done, BytesTotal: total, FileIndex: idx, FileCount: count, Phase: "downloading", Resumed: resumed})
				lastReport = time.Now()
			}
		}
		if rerr == io.EOF {
			return done, nil
		}
		if rerr != nil {
			return done, fmt.Errorf("read %s: %w", name, rerr)
		}
	}
}

// rehashExisting streams an existing temp file through h so a resumed download continues the
// same running sha256.
func rehashExisting(path string, h hash.Hash) error {
	fp, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fp.Close()
	_, err = io.Copy(h, fp)
	return err
}

func fileName(p string) string { return filepath.Base(p) }
