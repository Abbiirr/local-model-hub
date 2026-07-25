package manifest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// This file implements the Ollama pull passthrough (P3.2). `ollama pull` is a Phase-3-
// sanctioned mutation of Ollama's own store — it is a DOWNLOAD, not a model LOAD (no VRAM is
// touched), so it is allowed even though LocalHub otherwise never mutates a shared engine
// before Phase 4 (engine-adapters.md ownership asymmetry: Ollama owns its blob store).
//
// We drive the pull over Ollama's HTTP API (POST /api/pull, streaming NDJSON progress) rather
// than shelling the CLI, because the daemon is already the source of truth and the streamed
// {"status","total","completed","digest"} lines give clean progress + the final digest. This
// is the same operation `ollama pull` performs. After a successful pull we read the model's
// digest from GET /api/tags (or /api/show) to record on the manifest.

// OllamaPuller pulls a model into the shared Ollama daemon and reports progress. It talks
// ONLY to the local daemon (loopback base URL).
type OllamaPuller struct {
	BaseURL string
	HTTP    *http.Client
}

// NewOllamaPuller builds a puller against baseURL (e.g. http://127.0.0.1:11434).
func NewOllamaPuller(baseURL string) *OllamaPuller {
	return &OllamaPuller{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{}, // no global timeout; ctx governs (a pull can take minutes)
	}
}

// pullLine is one NDJSON line from POST /api/pull (re-verified against Ollama API docs,
// 2026-07-06): {"status":"pulling ...","digest":"sha256:...","total":N,"completed":M} then a
// final {"status":"success"}.
type pullLine struct {
	Status    string `json:"status"`
	Digest    string `json:"digest"`
	Total     int64  `json:"total"`
	Completed int64  `json:"completed"`
	Error     string `json:"error"`
}

// PullResult is the outcome of an Ollama pull: the model name and its resolved digest (the
// reproduce key), plus the total size the daemon reported.
type PullResult struct {
	Name      string
	Digest    string
	SizeBytes int64
}

// Pull streams `ollama pull <name>` progress and returns the resolved digest + size. prog is
// called with cumulative progress (BytesTotal from the manifest layer's Progress shape).
// Cancellation is via ctx (aborts the HTTP stream). An {"error":...} line fails the pull.
func (p *OllamaPuller) Pull(ctx context.Context, name string, prog ProgressFunc) (PullResult, error) {
	body := strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":true}`, name))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/api/pull", body)
	if err != nil {
		return PullResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return PullResult{}, fmt.Errorf("POST /api/pull %q: %w (is the Ollama daemon running?)", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return PullResult{}, fmt.Errorf("POST /api/pull %q returned HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var lastTotal, lastDone int64
	lastReport := time.Now()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var pl pullLine
		if err := json.Unmarshal([]byte(line), &pl); err != nil {
			continue // tolerate a malformed progress line
		}
		if pl.Error != "" {
			return PullResult{}, fmt.Errorf("ollama pull %q failed: %s", name, pl.Error)
		}
		if pl.Total > 0 {
			lastTotal = pl.Total
			lastDone = pl.Completed
		}
		if prog != nil && time.Since(lastReport) > 250*time.Millisecond {
			prog(Progress{File: name, BytesDone: pl.Completed, BytesTotal: pl.Total, FileIndex: 1, FileCount: 1, Phase: "downloading"})
			lastReport = time.Now()
		}
	}
	if err := sc.Err(); err != nil {
		return PullResult{}, fmt.Errorf("read pull stream for %q: %w", name, err)
	}

	// Read the resolved digest + size from /api/tags (the pulled model now appears there).
	digest, size := p.lookupDigest(ctx, name)
	if size == 0 {
		size = lastTotal
	}
	if prog != nil {
		prog(Progress{File: name, BytesDone: lastDone, BytesTotal: lastTotal, FileIndex: 1, FileCount: 1, Phase: "done"})
	}
	return PullResult{Name: name, Digest: digest, SizeBytes: size}, nil
}

// lookupDigest reads the pulled model's digest + size from GET /api/tags. Best-effort:
// returns ("", 0) if the daemon can't be read (the pull itself already succeeded).
func (p *OllamaPuller) lookupDigest(ctx context.Context, name string) (string, int64) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/api/tags", nil)
	if err != nil {
		return "", 0
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return "", 0
	}
	defer resp.Body.Close()
	var tags struct {
		Models []struct {
			Name   string `json:"name"`
			Model  string `json:"model"`
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&tags); err != nil {
		return "", 0
	}
	// Match by name, tolerating an implicit ":latest".
	want := name
	if !strings.Contains(want, ":") {
		want = want + ":latest"
	}
	for _, m := range tags.Models {
		if m.Name == name || m.Name == want || m.Model == name || m.Model == want {
			return m.Digest, m.Size
		}
	}
	return "", 0
}
