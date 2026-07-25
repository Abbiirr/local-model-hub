// Package ollama is the read-only Ollama detection adapter (P1.3). Ollama is a shared
// daemon LocalHub does not own or spawn (engine-adapters.md §5) — this package only
// OBSERVES it over HTTP. It makes ONLY GET requests: /api/version, /api/tags, /api/ps.
// It never POSTs (no warm, no keep_alive, no unload) — that mutation is Phase 4 work.
//
// Response field shapes are time-sensitive engine behavior (CLAUDE.md; PLAN.md resolution
// 8). They were RE-VERIFIED against the current Ollama API docs
// (https://github.com/ollama/ollama/blob/main/docs/api.md, fetched 2026-07-06) and the
// live daemon on the reference box (Ollama 0.21.2). Verified shapes, verbatim from the doc:
//
//	GET /api/version -> {"version":"0.5.1"}   (single string field "version")
//
//	GET /api/tags -> {"models":[{
//	    "name": "deepseek-r1:latest", "model": "deepseek-r1:latest",
//	    "modified_at": "2025-05-10T08:06:48.6-07:00", "size": 4683075271,
//	    "digest": "0a8c2669...", "details": {
//	      "parent_model":"", "format":"gguf", "family":"qwen2",
//	      "families":["qwen2"], "parameter_size":"7.6B", "quantization_level":"Q4_K_M"}}]}
//
//	GET /api/ps -> {"models":[{
//	    "name":"mistral:latest", "model":"mistral:latest", "size":5137025024,
//	    "digest":"2ae6f6dd...", "details":{...same shape as tags...},
//	    "expires_at":"2024-06-04T14:38:31.8-07:00", "size_vram":5137025024}]}
//
// Note: /api/ps carries `size_vram` (bytes resident in VRAM) and `expires_at` (idle-unload
// time) per running model — the source of truth for Ollama residency (engine-adapters.md).
// Parsing is DEFENSIVE: unknown fields are ignored (encoding/json default) and missing
// fields decode to their zero value, so a future Ollama that adds/removes optional fields
// does not break detection.
package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/localhub/localhub/internal/engines"
)

// DefaultBaseURL is the Ollama daemon's default local endpoint (engine-adapters.md §5).
const DefaultBaseURL = "http://127.0.0.1:11434"

// DefaultBinaryPath is where the Ollama binary lives on the reference box. Used as a
// detection fallback: if the daemon does not answer but the binary exists, the engine is
// Detected=true/Reachable=false (installed but stopped) rather than Detected=false. We do
// NOT start the daemon (read-only).
const DefaultBinaryPath = `K:\Softwares\ollama\ollama.exe`

// Client is a read-only Ollama HTTP client. Construct with New. It holds no mutable state
// beyond configuration, so it is safe for concurrent use.
type Client struct {
	BaseURL    string
	BinaryPath string
	HTTP       *http.Client
}

// New builds a Client. An empty baseURL falls back to DefaultBaseURL; an empty binaryPath
// falls back to DefaultBinaryPath. The http.Client has no global timeout — per-call
// timeouts come from the context the caller passes (so a slow /api/version cannot hang the
// detection sweep).
func New(baseURL, binaryPath string) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	if strings.TrimSpace(binaryPath) == "" {
		binaryPath = DefaultBinaryPath
	}
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		BinaryPath: binaryPath,
		HTTP:       &http.Client{},
	}
}

// Kind implements engines.Detector.
func (c *Client) Kind() engines.EngineKind { return engines.KindOllama }

// versionResponse is GET /api/version. Only `version` is defined by the doc.
type versionResponse struct {
	Version string `json:"version"`
}

// ModelDetails is the shared `details` sub-object of /api/tags and /api/ps entries.
type ModelDetails struct {
	ParentModel       string   `json:"parent_model"`
	Format            string   `json:"format"`
	Family            string   `json:"family"`
	Families          []string `json:"families"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
}

// TagModel is one entry of GET /api/tags (an installed model).
type TagModel struct {
	Name       string       `json:"name"`
	Model      string       `json:"model"`
	ModifiedAt string       `json:"modified_at"`
	Size       int64        `json:"size"`
	Digest     string       `json:"digest"`
	Details    ModelDetails `json:"details"`
}

type tagsResponse struct {
	Models []TagModel `json:"models"`
}

// PSModel is one entry of GET /api/ps (a currently loaded/running model). SizeVRAM and
// ExpiresAt are the residency-relevant fields (bytes resident, idle-unload time).
type PSModel struct {
	Name      string       `json:"name"`
	Model     string       `json:"model"`
	Size      int64        `json:"size"`
	Digest    string       `json:"digest"`
	Details   ModelDetails `json:"details"`
	ExpiresAt string       `json:"expires_at"`
	SizeVRAM  int64        `json:"size_vram"`
}

type psResponse struct {
	Models []PSModel `json:"models"`
}

// Detect performs the read-only detection sweep and returns a single EngineStatus. It:
//  1. GETs /api/version. If it answers, the daemon is Detected=true, Reachable=true, and
//     Detect also folds in a live /api/tags and /api/ps snapshot (counts + running list).
//  2. If the daemon does NOT answer, it checks whether the Ollama binary exists on disk.
//     Present binary -> Detected=true, Reachable=false ("installed but daemon not
//     answering"). Absent binary -> Detected=false. Both carry a Reason.
//
// It never starts the daemon and never mutates state.
func (c *Client) Detect(ctx context.Context) engines.EngineStatus {
	st := engines.EngineStatus{
		Kind:     engines.KindOllama,
		Endpoint: c.BaseURL,
		Details:  map[string]any{},
	}

	ver, err := c.version(ctx)
	if err != nil {
		// Daemon did not answer. Fall back to binary presence to distinguish
		// "installed but stopped" from "not installed".
		if binaryExists(c.BinaryPath) {
			st.Detected = true
			st.Reachable = false
			st.Details["binary_path"] = c.BinaryPath
			st.Reason = fmt.Sprintf(
				"Ollama binary present at %s but daemon at %s did not answer (%v); daemon appears stopped. Not started (detection is read-only).",
				c.BinaryPath, c.BaseURL, err)
			return st
		}
		st.Detected = false
		st.Reachable = false
		st.Reason = fmt.Sprintf(
			"Ollama daemon at %s did not answer (%v) and no Ollama binary found at %s; Ollama not detected on this box.",
			c.BaseURL, err, c.BinaryPath)
		return st
	}

	// Daemon answered.
	st.Detected = true
	st.Reachable = true
	st.Version = ver
	if binaryExists(c.BinaryPath) {
		st.Details["binary_path"] = c.BinaryPath
	}

	// Best-effort live snapshots. These are read-only and must not turn a healthy detect
	// into a failure: if either sub-call fails, record the error in Details but keep
	// Reachable=true (the daemon is clearly up — it answered /api/version).
	if tags, terr := c.listModels(ctx); terr == nil {
		st.Details["installed_model_count"] = len(tags)
	} else {
		st.Details["tags_error"] = terr.Error()
	}
	if ps, perr := c.listRunning(ctx); perr == nil {
		st.Details["running_model_count"] = len(ps)
		names := make([]string, 0, len(ps))
		for _, m := range ps {
			names = append(names, m.Name)
		}
		st.Details["running_models"] = names
	} else {
		st.Details["ps_error"] = perr.Error()
	}

	st.Reason = fmt.Sprintf("Ollama daemon reachable at %s (version %s).", c.BaseURL, ver)
	return st
}

// Version performs GET /api/version and returns the daemon version string. Exported for
// callers that only need the version. Read-only.
func (c *Client) Version(ctx context.Context) (string, error) { return c.version(ctx) }

func (c *Client) version(ctx context.Context) (string, error) {
	var v versionResponse
	if err := c.getJSON(ctx, "/api/version", &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// ListModels performs GET /api/tags and returns installed models. Read-only.
func (c *Client) ListModels(ctx context.Context) ([]TagModel, error) { return c.listModels(ctx) }

func (c *Client) listModels(ctx context.Context) ([]TagModel, error) {
	var r tagsResponse
	if err := c.getJSON(ctx, "/api/tags", &r); err != nil {
		return nil, err
	}
	return r.Models, nil
}

// ListRunning performs GET /api/ps and returns currently loaded models with their VRAM
// size and idle-expiry — the source of truth for Ollama residency. Read-only.
func (c *Client) ListRunning(ctx context.Context) ([]PSModel, error) { return c.listRunning(ctx) }

func (c *Client) listRunning(ctx context.Context) ([]PSModel, error) {
	var r psResponse
	if err := c.getJSON(ctx, "/api/ps", &r); err != nil {
		return nil, err
	}
	return r.Models, nil
}

// getJSON issues a GET and decodes the JSON body into out. Decoding is defensive: unknown
// fields are ignored and missing fields stay zero (standard encoding/json behavior), so a
// future Ollama that changes optional fields does not break detection. A non-2xx status is
// an error (its body is truncated into the message for a readable Reason).
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request %s: %w", path, err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return fmt.Errorf("GET %s returned HTTP %d: %s", path, resp.StatusCode, snippet)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

// binaryExists reports whether path names an existing file. Empty path -> false.
func binaryExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
