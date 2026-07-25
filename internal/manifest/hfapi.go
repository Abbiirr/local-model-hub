package manifest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// This file wraps the Hugging Face Hub API for the two read-only metadata operations the HF
// download path needs BEFORE any weight bytes are fetched (the "show before committing" gate,
// spec-digest §7 / TESTING.md#p31):
//
//   1. Resolve a branch/tag ref → exact commit SHA (the core rule: pin the revision).
//   2. List the repo's files at that commit (paths + LFS sha256 + size) so the UI can show
//      what will be downloaded and the estimator can total the size.
//
// API endpoints (re-verified against https://huggingface.co/docs/hub/api, 2026-07-06):
//
//   GET /api/models/{repo}/revision/{ref}
//     → { "sha": "<commit sha>", "siblings": [ {"rfilename": "..."} ],
//         "gated": false|"auto"|"manual", "cardData": {"license": "..."} }
//     `sha` is the resolved commit for the ref (this pins "main"/a tag/a branch to bytes).
//
//   The per-file LFS sha256 + exact size are NOT in the revision payload's siblings; they
//   come from the tree listing with ?expand or from each file's HEAD. We use the paths from
//   the revision call and enrich size/lfs-sha via GET /api/models/{repo}/tree/{ref}?recursive=1
//   which returns [{ "path","size","lfs":{"oid":"<sha256>","size":N} }, ...].
//
// All calls are metadata-only, context-timeout-bounded, and size-capped. The download of
// actual weight bytes is a SEPARATE step (download.go) gated behind an explicit confirm.

// HFHost is the Hugging Face API/resolve host. A package var so tests point it at a fake.
var HFHost = "https://huggingface.co"

// HFClient performs HF Hub metadata calls. Zero value is usable (http.DefaultClient +
// DefaultTimeout); set HTTP/Token for a custom transport or gated-repo auth.
type HFClient struct {
	HTTP    *http.Client
	Token   string // optional HF access token for gated/private repos (Authorization: Bearer)
	Timeout time.Duration
}

// hfMetaTimeout bounds a single metadata call when Timeout is unset.
const hfMetaTimeout = 20 * time.Second

// hfMetaMaxBytes caps a metadata response body (siblings/tree can be large for big repos, but
// still small vs weights). 32 MiB is generous for a file listing.
const hfMetaMaxBytes = 32 << 20

// RepoInfo is the resolved metadata for a repo at a ref: the pinned commit SHA, the file
// list (enriched with size + lfs sha256 where available), the gated flag, and the license.
type RepoInfo struct {
	Repo      string
	Ref       string // the ref that was requested (main/tag/branch/sha)
	CommitSHA string
	Gated     bool
	License   string
	Files     []HFFile
}

// HFFile is one file in a repo at a commit: its repo-relative path, size (bytes, 0 if
// unknown), and LFS sha256 (lowercase hex; "" for small non-LFS files like config.json).
type HFFile struct {
	Path      string
	SizeBytes int64
	LFSSHA256 string
}

// revisionResponse is the subset of GET /api/models/{repo}/revision/{ref} we read.
type revisionResponse struct {
	SHA      string `json:"sha"`
	Gated    any    `json:"gated"` // false | "auto" | "manual"
	Siblings []struct {
		RFilename string `json:"rfilename"`
	} `json:"siblings"`
	CardData struct {
		License string `json:"license"`
	} `json:"cardData"`
}

// treeEntry is one entry of GET /api/models/{repo}/tree/{ref}?recursive=1.
type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"` // "file" | "directory"
	Size int64  `json:"size"`
	LFS  *struct {
		OID  string `json:"oid"` // "sha256:..." or bare hex
		Size int64  `json:"size"`
	} `json:"lfs"`
}

// ResolveRevision resolves ref (branch/tag/sha, "" => "main") to an exact commit SHA and
// returns full repo metadata (files + gated + license). This is the metadata-only call the
// two-step download flow shows the user BEFORE downloading (spec-digest §7 "show resolved
// commit SHA before downloading").
func (c *HFClient) ResolveRevision(ctx context.Context, repo, ref string) (RepoInfo, error) {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" {
		return RepoInfo{}, fmt.Errorf("empty repo")
	}
	if ref == "" {
		ref = "main"
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = hfMetaTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var rev revisionResponse
	url := fmt.Sprintf("%s/api/models/%s/revision/%s", HFHost, repo, pathEscapeRef(ref))
	if err := c.getJSON(ctx, url, &rev); err != nil {
		return RepoInfo{}, fmt.Errorf("resolve %s@%s: %w", repo, ref, err)
	}
	if rev.SHA == "" {
		return RepoInfo{}, fmt.Errorf("resolve %s@%s: HF returned no commit sha", repo, ref)
	}

	info := RepoInfo{
		Repo:      repo,
		Ref:       ref,
		CommitSHA: rev.SHA,
		Gated:     gatedFlag(rev.Gated),
		License:   strings.TrimSpace(rev.CardData.License),
	}

	// Enrich with size + LFS sha via the tree listing (best-effort; on failure we still have
	// the sibling paths from the revision call, just without size/hash).
	sizes, lfs := c.treeMeta(ctx, repo, rev.SHA)
	for _, sib := range rev.Siblings {
		f := HFFile{Path: sib.RFilename}
		if sz, ok := sizes[sib.RFilename]; ok {
			f.SizeBytes = sz
		}
		if h, ok := lfs[sib.RFilename]; ok {
			f.LFSSHA256 = h
		}
		info.Files = append(info.Files, f)
	}
	return info, nil
}

// treeMeta fetches the recursive tree listing at commit and returns per-path size + LFS
// sha256 maps. Best-effort: returns empty maps on any error.
func (c *HFClient) treeMeta(ctx context.Context, repo, commit string) (map[string]int64, map[string]string) {
	sizes := map[string]int64{}
	lfs := map[string]string{}
	url := fmt.Sprintf("%s/api/models/%s/tree/%s?recursive=1&expand=1", HFHost, repo, commit)
	var entries []treeEntry
	if err := c.getJSON(ctx, url, &entries); err != nil {
		return sizes, lfs
	}
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		if e.LFS != nil {
			if e.LFS.Size > 0 {
				sizes[e.Path] = e.LFS.Size
			} else {
				sizes[e.Path] = e.Size
			}
			lfs[e.Path] = strings.TrimPrefix(e.LFS.OID, "sha256:")
		} else {
			sizes[e.Path] = e.Size
		}
	}
	return sizes, lfs
}

// ResolveURL returns the direct download URL for a repo file at a pinned commit. This is what
// the downloader GETs (and Range-resumes). Pinning the commit in the URL guarantees the bytes
// match the manifest revision.
func (c *HFClient) ResolveURL(repo, commit, file string) string {
	return fmt.Sprintf("%s/%s/resolve/%s/%s", HFHost, repo, commit, file)
}

// getJSON GETs url and decodes JSON into out with the size cap + auth header applied.
func (c *HFClient) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("HTTP %d — the repo may be gated or private; accept its terms on HF or set an access token", resp.StatusCode)
	case http.StatusNotFound:
		return fmt.Errorf("HTTP 404 — repo or ref not found")
	default:
		return fmt.Errorf("unexpected HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, hfMetaMaxBytes+1))
	if err != nil {
		return err
	}
	if len(b) > hfMetaMaxBytes {
		return fmt.Errorf("metadata response exceeds %d-byte cap", hfMetaMaxBytes)
	}
	return json.Unmarshal(b, out)
}

// gatedFlag normalizes HF's `gated` field (false | "auto" | "manual") into a bool.
func gatedFlag(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t != "" && t != "false"
	}
	return false
}

// pathEscapeRef escapes a ref for use in a URL path segment while preserving "/" (a branch
// name can contain a slash). Only spaces and a few unsafe chars are escaped; refs are
// typically simple (main, v1.0, a sha).
func pathEscapeRef(ref string) string {
	return strings.ReplaceAll(ref, " ", "%20")
}
