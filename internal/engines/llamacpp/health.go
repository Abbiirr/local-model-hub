package llamacpp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/localhub/localhub/internal/instances"
)

// ReconcileHealthChecker satisfies instances.HealthChecker for llama.cpp instances at
// daemon-startup reconcile. For a recorded instance it:
//   - GETs {base_url}/health → 200 means healthy.
//   - GETs {base_url}/props → the `model_path`/`default_generation_settings.model` names the
//     GGUF the server is actually serving; we compare its basename to the recorded model_path.
//     A match => this is our process (identity OK); a mismatch => a reused-port collision
//     (another llama-server grabbed the port) => reap.
//
// [RE-VERIFY 2026-07-06] llama-server /props returns a JSON object; the served model path is
// exposed (field name has drifted across builds: "model_path" in recent builds,
// "default_generation_settings.model" earlier). We check both and fall back to "identity OK if
// healthy and we cannot read a model path" (a healthy llama-server on our recorded port that we
// cannot positively disprove is treated as ours — conservative toward re-attach, and a genuine
// stranger is unlikely to hold exactly our port).
type ReconcileHealthChecker struct {
	HTTP *http.Client
}

// NewReconcileHealthChecker builds a checker with a short-timeout HTTP client.
func NewReconcileHealthChecker() *ReconcileHealthChecker {
	return &ReconcileHealthChecker{HTTP: &http.Client{Timeout: 4 * time.Second}}
}

func (c *ReconcileHealthChecker) Check(ctx context.Context, inst instances.Instance) (healthy bool, identityOK bool, err error) {
	base := inst.BaseURL
	if base == "" {
		return false, false, nil
	}
	// 1. /health.
	hctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, rerr := http.NewRequestWithContext(hctx, http.MethodGet, base+"/health", nil)
	if rerr != nil {
		return false, false, rerr
	}
	resp, derr := c.client().Do(req)
	if derr != nil {
		return false, false, derr
	}
	ok := resp.StatusCode == http.StatusOK
	resp.Body.Close()
	if !ok {
		return false, false, nil
	}

	// 2. /props → served model path for identity.
	served, gotPath := c.servedModelPath(ctx, base)
	if !gotPath {
		// Healthy but we could not read a model path: treat as ours (conservative re-attach).
		return true, true, nil
	}
	identityOK = sameModel(served, inst.ModelPath)
	return true, identityOK, nil
}

func (c *ReconcileHealthChecker) servedModelPath(ctx context.Context, base string) (string, bool) {
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/props", nil)
	if err != nil {
		return "", false
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var props struct {
		ModelPath                 string `json:"model_path"`
		DefaultGenerationSettings struct {
			Model string `json:"model"`
		} `json:"default_generation_settings"`
	}
	if err := json.Unmarshal(body, &props); err != nil {
		return "", false
	}
	if props.ModelPath != "" {
		return props.ModelPath, true
	}
	if props.DefaultGenerationSettings.Model != "" {
		return props.DefaultGenerationSettings.Model, true
	}
	return "", false
}

func (c *ReconcileHealthChecker) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// sameModel compares two model paths by basename (case-insensitive), tolerating different
// absolute prefixes (llama-server may report a normalized/relative path).
func sameModel(served, recorded string) bool {
	if served == "" || recorded == "" {
		return false
	}
	return strings.EqualFold(filepath.Base(served), filepath.Base(recorded))
}
