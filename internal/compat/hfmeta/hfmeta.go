// Package hfmeta fetches Hugging Face model METADATA (config.json and, when present,
// model.safetensors.index.json) over HTTPS WITHOUT downloading any weights. It is used by the
// compat preview endpoint's remote-safetensors path (POST /api/compat/preview with a bare
// hf_repo): the safetensors estimator needs config.json (dimensions) and prefers the index's
// metadata.total_size for exact weights size.
//
// Safety (spec §7 "metadata only, with timeout+size caps"; PLAN.md safeguards):
//   - Every fetch is bounded by a context timeout AND a hard response-body size cap
//     (config/index files are a few KB–hundreds of KB; a multi-GB response is refused).
//   - Only the two small JSON files are fetched — never a *.safetensors weight file.
//   - The HF resolve URL is pinned to a revision when supplied (never "latest" semantics
//     leak into a download; this is read-only metadata, but we still honor the pin).
//
// This package makes network calls; callers gate it behind an explicit remote-preview action
// and pass a timeout. GGUF remote metadata is handled separately by gguf-parser-go (range
// reads) in internal/compat/gguf — this package is the safetensors analogue.
package hfmeta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout bounds a single metadata fetch when the caller passes 0.
const DefaultTimeout = 15 * time.Second

// MaxBodyBytes is the hard cap on a metadata response body. config.json / index.json are
// small (KB to low-hundreds-of-KB); anything larger is refused as a safety measure so a
// misbehaving/hostile repo cannot stream gigabytes into the daemon. 8 MiB is generous.
const MaxBodyBytes = 8 << 20

// hfBase is the Hugging Face resolve host. The resolve endpoint returns the raw file at a
// given revision (or "main"). Kept a package var so tests can point it at a local server.
var hfBase = "https://huggingface.co"

// Client fetches HF metadata. The zero value is usable (uses http.DefaultClient + defaults);
// set HTTP for a custom transport (tests inject one pointing at a fake server).
type Client struct {
	HTTP    *http.Client
	Timeout time.Duration
}

// Result is the fetched metadata: the raw config.json bytes (required) and the raw index
// bytes (optional — many single-shard models have no index). Both are capped and validated
// as JSON by the caller (the safetensors package parses them).
type Result struct {
	ConfigJSON []byte // config.json (never nil on success)
	IndexJSON  []byte // model.safetensors.index.json, or nil if absent
	Repo       string
	Revision   string // the revision requested ("" => main)
}

// Fetch retrieves config.json and (best-effort) model.safetensors.index.json for repo at
// revision (revision "" => "main"). It errors if config.json cannot be fetched (that file is
// mandatory for the estimate); a missing index is NOT an error (IndexJSON stays nil).
func (c *Client) Fetch(ctx context.Context, repo, revision string) (Result, error) {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" {
		return Result{}, fmt.Errorf("hfmeta: empty repo")
	}
	if !validRepo(repo) {
		return Result{}, fmt.Errorf("hfmeta: invalid repo %q (want owner/name)", repo)
	}
	rev := strings.TrimSpace(revision)
	if rev == "" {
		rev = "main"
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cfg, err := c.get(ctx, repo, rev, "config.json")
	if err != nil {
		return Result{}, fmt.Errorf("fetch config.json for %s@%s: %w", repo, rev, err)
	}
	if cfg == nil {
		return Result{}, fmt.Errorf("config.json not found for %s@%s (is it a model repo?)", repo, rev)
	}

	// Index is optional; a fetch error other than 404 is swallowed (we still have config).
	idx, _ := c.get(ctx, repo, rev, "model.safetensors.index.json")

	return Result{ConfigJSON: cfg, IndexJSON: idx, Repo: repo, Revision: revision}, nil
}

// get fetches one file. Returns (nil, nil) on a 404 (file absent — a normal case for the
// optional index), (bytes, nil) on 200, and an error on any other failure.
func (c *Client) get(ctx context.Context, repo, rev, file string) ([]byte, error) {
	u := fmt.Sprintf("%s/%s/resolve/%s/%s", hfBase, repo, url.PathEscape(rev), file)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// Enforce the size cap: read at most MaxBodyBytes+1 and reject if it overflows.
		lr := io.LimitReader(resp.Body, MaxBodyBytes+1)
		b, err := io.ReadAll(lr)
		if err != nil {
			return nil, err
		}
		if len(b) > MaxBodyBytes {
			return nil, fmt.Errorf("%s exceeds the %d-byte metadata cap; refusing (weights are never fetched here)", file, MaxBodyBytes)
		}
		if !json.Valid(b) {
			return nil, fmt.Errorf("%s is not valid JSON", file)
		}
		return b, nil
	case http.StatusNotFound:
		return nil, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("access to %s denied (HTTP %d) — the repo may be gated; accept its terms on HF or use a public mirror", file, resp.StatusCode)
	default:
		return nil, fmt.Errorf("unexpected HTTP %d fetching %s", resp.StatusCode, file)
	}
}

// validRepo checks the "owner/name" shape (one slash, non-empty halves, no traversal).
func validRepo(repo string) bool {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, " \t\n") {
			return false
		}
	}
	return true
}
