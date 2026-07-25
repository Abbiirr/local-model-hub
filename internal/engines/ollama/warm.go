// This file adds the P4.3 MUTATION path to the Ollama adapter. Ollama is a SHARED DAEMON
// LocalHub does not own or spawn (engine-adapters.md §5): we never spawn an ollama process and
// NEVER kill the daemon. "Running a model" on Ollama is a request to the daemon to load it into
// VRAM and keep it there (warm); "stopping" it is a request to unload it (keep_alive:0). Both
// go over HTTP; the daemon decides when memory is actually reclaimed, so Unload POLLS /api/ps
// until the model is gone rather than trusting the 200 (lmwrapper lesson: wait ~5s+ for VRAM
// release; the caller then confirms the VRAM delta via the hardware probe).
//
// [RE-VERIFY 2026-07-06] Warm/unload semantics re-verified against the current Ollama API doc
// (https://github.com/ollama/ollama/blob/main/docs/api.md, fetched 2026-07-06):
//
//   - LOAD (warm) a model without generating: POST /api/generate with ONLY {"model":M}
//     (empty/omitted prompt). The doc: "If the messages array is empty, the model will be
//     loaded into memory." The response is a single JSON object with "done":true and
//     "done_reason":"load". keep_alive controls how long it stays resident: a duration string
//     ("10m"), a number of seconds, or -1 to keep it resident indefinitely (pin).
//     Verbatim request:  {"model":"llama3.2"}                         → done_reason:"load"
//     Verbatim w/ pin:   {"model":"llama3.2","keep_alive":-1}
//
//   - UNLOAD a model: POST /api/generate with {"model":M,"keep_alive":0}. The doc response:
//     {"model":"llama3.2",...,"response":"","done":true,"done_reason":"unload"}. This is a
//     REQUEST — the daemon unloads on its own schedule; poll /api/ps to confirm VRAM freed.
//
//   - EMBEDDING models: /api/embed accepts keep_alive as an advanced parameter, but the doc
//     gives no empty-input load example (unlike /api/generate). For warming, /api/generate with
//     just {"model":M} works for embedding models too (the daemon loads the model regardless of
//     the endpoint that references it) and is the documented load path, so Warm uses it
//     uniformly. Unload via keep_alive:0 on /api/generate likewise unloads an embedding model.
//
// The field shapes of /api/ps (expires_at, size_vram) were verified in P1.3 (see the package
// doc in ollama.go) and re-confirmed here 2026-07-06.
package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DaemonUnreachableError is returned by Warm/Unload when the Ollama daemon does not answer. It is
// a typed error carrying a human-readable Reason (CLAUDE.md correctness bar: every user-facing
// error carries a why) so the core can surface it verbatim and callers can assert on the type.
type DaemonUnreachableError struct {
	BaseURL string
	Op      string // "warm" | "unload"
	Err     error
}

func (e *DaemonUnreachableError) Error() string {
	return fmt.Sprintf("ollama daemon at %s did not answer during %s (%v); the shared Ollama daemon appears stopped — start it and retry (LocalHub never starts or kills the ollama daemon)",
		e.BaseURL, e.Op, e.Err)
}

func (e *DaemonUnreachableError) Unwrap() error { return e.Err }

// generateResponse is the single-object response of a non-streaming POST /api/generate. Parsed
// defensively: unknown fields ignored, missing fields zero.
type generateResponse struct {
	Model      string `json:"model"`
	Response   string `json:"response"`
	Done       bool   `json:"done"`
	DoneReason string `json:"done_reason"` // "load" on warm, "unload" on unload
}

// WarmResult reports the outcome of a Warm call.
type WarmResult struct {
	Model      string // the model as echoed by the daemon
	DoneReason string // "load" when the daemon loaded the model into memory
	KeepAlive  string // the keep_alive we sent (for the audit reason)
}

// Warm loads a model into the Ollama daemon's memory WITHOUT generating. It POSTs /api/generate
// with an empty prompt + keep_alive (the documented load path for generative models). keepAlive
// controls residency: pass "" for the daemon default, a duration string ("10m"), a whole-second
// count ("300"), or "-1" to pin it resident. This is a mutation (POST) — the ONLY mutation the
// Ollama adapter performs besides Unload; it never spawns or kills a process.
//
// EMBEDDING-MODEL FALLBACK (verified on the reference box 2026-07-06): embedding models reject
// /api/generate with HTTP 400 "<model> does not support generate". When Warm sees that error it
// RETRIES on /api/embed with an empty input + keep_alive, which loads the embedding model into
// VRAM and returns {"embeddings":[]}. So a single Warm call warms both generative and embedding
// models without the caller needing to pre-classify the model.
//
// It returns a WarmResult (with done_reason "load" for generative models; embed loads return an
// empty done_reason but the caller confirms residency via /api/ps) or a *DaemonUnreachableError
// if the daemon is down.
// numGPU controls layer placement (Ollama api/types.go Runner.NumGPU, json "num_gpu"): the number
// of model layers to offload to the GPU. nil => omit the option so the daemon uses its dynamic
// default (-1 = auto-split as many layers as fit, rest on CPU). A large value (LocalHub sends
// forceFullGPULayers) forces ALL layers onto the GPU (full offload, never CPU). Verified against
// https://raw.githubusercontent.com/ollama/ollama/main/api/types.go (fetched 2026-07-07):
// `NumGPU int json:"num_gpu,omitempty"` with a documented default of -1 = "set dynamically".
func (c *Client) Warm(ctx context.Context, model, keepAlive string, numGPU *int) (WarmResult, error) {
	if strings.TrimSpace(model) == "" {
		return WarmResult{}, fmt.Errorf("ollama warm: model is required")
	}
	body := warmBody(model, keepAlive, false, numGPU)
	var resp generateResponse
	err := c.postJSON(ctx, "/api/generate", body, &resp)
	if err == nil {
		return WarmResult{Model: resp.Model, DoneReason: resp.DoneReason, KeepAlive: keepAlive}, nil
	}
	if isTransport(err) {
		return WarmResult{}, &DaemonUnreachableError{BaseURL: c.BaseURL, Op: "warm", Err: err}
	}
	// Embedding models 400 on /api/generate: fall back to /api/embed (empty input warms it).
	if isNotGenerateError(err) {
		embedBody := embedWarmBody(model, keepAlive, numGPU)
		var eresp struct {
			Model string `json:"model"`
		}
		if eerr := c.postJSON(ctx, "/api/embed", embedBody, &eresp); eerr != nil {
			if isTransport(eerr) {
				return WarmResult{}, &DaemonUnreachableError{BaseURL: c.BaseURL, Op: "warm", Err: eerr}
			}
			return WarmResult{}, fmt.Errorf("ollama warm %q (embed fallback): %w", model, eerr)
		}
		return WarmResult{Model: model, DoneReason: "load", KeepAlive: keepAlive}, nil
	}
	return WarmResult{}, fmt.Errorf("ollama warm %q: %w", model, err)
}

// isNotGenerateError reports whether err is the Ollama "does not support generate" 400 (an
// embedding-only model). Matched on the message substring the daemon returns.
func isNotGenerateError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "does not support generate")
}

// embedWarmBody builds the /api/embed load body: empty input + keep_alive (same keep_alive
// encoding rules as warmBody) + an optional num_gpu option (nil => omitted).
func embedWarmBody(model, keepAlive string, numGPU *int) string {
	esc := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	opts := numGPUOptions(numGPU)
	if strings.TrimSpace(keepAlive) == "" {
		return fmt.Sprintf(`{"model":%s,"input":""%s}`, esc(model), opts)
	}
	if isNumeric(keepAlive) {
		return fmt.Sprintf(`{"model":%s,"input":"","keep_alive":%s%s}`, esc(model), keepAlive, opts)
	}
	return fmt.Sprintf(`{"model":%s,"input":"","keep_alive":%s%s}`, esc(model), esc(keepAlive), opts)
}

// numGPUOptions renders the trailing `,"options":{"num_gpu":N}` fragment for a warm body, or ""
// when numGPU is nil (omit → daemon dynamic default -1). num_gpu is a LOAD-time runner option
// (Ollama api/types.go Runner.NumGPU); it must be nested under "options" on /api/generate and
// /api/embed. The fragment starts with a comma so it appends after the last existing field.
func numGPUOptions(numGPU *int) string {
	if numGPU == nil {
		return ""
	}
	return fmt.Sprintf(`,"options":{"num_gpu":%d}`, *numGPU)
}

// forceFullGPULayers is the num_gpu value LocalHub sends to force ALL layers onto the GPU (full
// offload, never CPU). Ollama offloads min(num_gpu, model_layer_count) layers, so any value >= the
// largest local model's layer count forces full residency; 999 is the well-established idiom for
// "all layers" (far above any real model's layer count). Used when allow_cpu_offload is false —
// the hard default that makes an Ollama warm behave like llama.cpp's -ngl all.
const forceFullGPULayers = 999

// FullGPULayers returns a pointer to the num_gpu value that forces ALL layers onto the GPU (full
// offload, never CPU). The core passes this as WarmSpec.NumGPU for the full-GPU default; it passes
// nil for allow_cpu_offload=true (daemon dynamic -1 auto-split). Exported so the core does not
// hardcode the sentinel.
func FullGPULayers() *int { n := forceFullGPULayers; return &n }

// UnloadResult reports the outcome of an Unload call — including whether the model actually left
// /api/ps within the poll budget (the honest "did VRAM free" signal, not a bare 200).
type UnloadResult struct {
	Model         string        // the model requested to unload
	DoneReason    string        // "unload" when the daemon accepted the unload request
	Gone          bool          // true when the model no longer appears in /api/ps
	WaitedFor     time.Duration // how long we polled before Gone (or the timeout)
	FreedVRAMHint int64         // size_vram (bytes) the model held just before unload, if observed
	Reason        string        // human-readable outcome
}

// UnloadConfig tunes the Unload poll.
type UnloadConfig struct {
	// PollTimeout bounds the /api/ps poll after the unload request (default 20s: Ollama frees
	// asynchronously, ~5s+ per the lmwrapper lesson, but a busy daemon can take longer).
	PollTimeout time.Duration
	// PollInterval is the /api/ps cadence (default 500ms).
	PollInterval time.Duration
}

func (c UnloadConfig) withDefaults() UnloadConfig {
	if c.PollTimeout <= 0 {
		c.PollTimeout = 20 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	return c
}

// Unload requests immediate unload of a model (POST /api/generate {"model":M,"keep_alive":0}) and
// then POLLS /api/ps until the model is gone or the timeout elapses. It does NOT trust the 200:
// the daemon frees VRAM on its own schedule, so Gone==true (model absent from /api/ps) is the
// real signal that residency has been released. The caller still verifies the actual VRAM delta
// via the hardware probe (residency-scheduler.md re-measure rule) — this method only confirms the
// model left the daemon's resident set.
//
// FreedVRAMHint is the size_vram the model held on the last /api/ps sample before it vanished, so
// the caller has an expected-footprint number for its probe-delta bar.
func (c *Client) Unload(ctx context.Context, model string, cfg UnloadConfig) (UnloadResult, error) {
	if strings.TrimSpace(model) == "" {
		return UnloadResult{}, fmt.Errorf("ollama unload: model is required")
	}
	cfg = cfg.withDefaults()

	// Record the model's current VRAM footprint (if resident) BEFORE the unload, as the hint.
	var footprint int64
	if ps, err := c.listRunning(ctx); err == nil {
		if m, ok := findPS(ps, model); ok {
			footprint = m.SizeVRAM
		} else {
			// Already not resident — nothing to unload. Return Gone immediately.
			return UnloadResult{
				Model: model, Gone: true, FreedVRAMHint: 0,
				Reason: fmt.Sprintf("model %q was not resident in /api/ps; nothing to unload", model),
			}, nil
		}
	}

	// Send the unload request.
	body := warmBody(model, "0", true, nil) // keep_alive:0 as a JSON number; no num_gpu on unload
	var resp generateResponse
	if err := c.postJSON(ctx, "/api/generate", body, &resp); err != nil {
		if isTransport(err) {
			return UnloadResult{}, &DaemonUnreachableError{BaseURL: c.BaseURL, Op: "unload", Err: err}
		}
		return UnloadResult{}, fmt.Errorf("ollama unload %q: %w", model, err)
	}

	// Poll /api/ps until the model is gone or we time out.
	start := time.Now()
	deadline := start.Add(cfg.PollTimeout)
	tick := time.NewTicker(cfg.PollInterval)
	defer tick.Stop()
	for {
		ps, err := c.listRunning(ctx)
		if err == nil {
			if _, ok := findPS(ps, model); !ok {
				waited := time.Since(start)
				return UnloadResult{
					Model: model, DoneReason: resp.DoneReason, Gone: true, WaitedFor: waited,
					FreedVRAMHint: footprint,
					Reason: fmt.Sprintf("unload confirmed: model %q left /api/ps after %s (VRAM release requested; daemon freed it — verify the probe delta ~%d bytes)",
						model, waited.Round(time.Millisecond), footprint),
				}, nil
			}
		}
		if time.Now().After(deadline) {
			waited := time.Since(start)
			return UnloadResult{
				Model: model, DoneReason: resp.DoneReason, Gone: false, WaitedFor: waited,
				FreedVRAMHint: footprint,
				Reason: fmt.Sprintf("unload requested (done_reason=%q) but model %q still present in /api/ps after %s — Ollama frees asynchronously; residency may lag, re-poll before assuming headroom",
					resp.DoneReason, model, waited.Round(time.Millisecond)),
			}, nil
		}
		select {
		case <-ctx.Done():
			return UnloadResult{}, ctx.Err()
		case <-tick.C:
		}
	}
}

// IsResident reports whether a model currently appears in /api/ps, and its size_vram/expires_at
// if so. A read-only convenience over listRunning for the mirror sync + warm confirmation.
func (c *Client) IsResident(ctx context.Context, model string) (resident bool, ps PSModel, err error) {
	list, err := c.listRunning(ctx)
	if err != nil {
		return false, PSModel{}, err
	}
	if m, ok := findPS(list, model); ok {
		return true, m, nil
	}
	return false, PSModel{}, nil
}

// warmBody builds the /api/generate request body. When keepAliveIsNumber is true, keepAlive is
// emitted as a JSON number (used for keep_alive:0 unload and numeric-second warms). An empty
// keepAlive omits the field entirely (daemon default). A "-1" pins the model (keep_alive:-1).
// numGPU (nil => omitted) sets the load-time layer-offload count under "options".
func warmBody(model, keepAlive string, keepAliveIsNumber bool, numGPU *int) string {
	esc := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	opts := numGPUOptions(numGPU)
	if strings.TrimSpace(keepAlive) == "" {
		return fmt.Sprintf(`{"model":%s%s}`, esc(model), opts)
	}
	if keepAliveIsNumber || isNumeric(keepAlive) {
		// Numeric keep_alive (seconds, 0, or -1) → JSON number.
		return fmt.Sprintf(`{"model":%s,"keep_alive":%s%s}`, esc(model), keepAlive, opts)
	}
	// Duration string like "10m" → JSON string.
	return fmt.Sprintf(`{"model":%s,"keep_alive":%s%s}`, esc(model), esc(keepAlive), opts)
}

// isNumeric reports whether s is an integer literal (optionally negative), so a keep_alive of
// "300" or "-1" is emitted as a JSON number rather than a quoted string.
func isNumeric(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' {
		if len(s) == 1 {
			return false
		}
		i = 1
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// findPS finds a /api/ps entry matching model by exact name or model field. Ollama refers to a
// model by tag (e.g. "llama3.2" or "llama3.2:latest"); we match either the `name` or `model`
// field, and also tolerate a bare-name request against a ":latest"-tagged entry.
func findPS(list []PSModel, model string) (PSModel, bool) {
	for _, m := range list {
		if m.Name == model || m.Model == model {
			return m, true
		}
	}
	// Tolerate bare name vs ":latest".
	want := model
	if !strings.Contains(want, ":") {
		want = model + ":latest"
	}
	for _, m := range list {
		if m.Name == want || m.Model == want {
			return m, true
		}
	}
	return PSModel{}, false
}

// postJSON issues a POST with a JSON body and decodes the (non-streaming) JSON response. Ollama's
// /api/generate streams by default; we do NOT set stream:false in the body because a load/unload
// call (empty prompt) returns a single object regardless. Decoding is defensive.
func (c *Client) postJSON(ctx context.Context, path, body string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("build POST %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &transportError{err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(raw))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return fmt.Errorf("POST %s returned HTTP %d: %s", path, resp.StatusCode, snippet)
	}
	if out == nil {
		return nil
	}
	// The body may contain a single JSON object (load/unload). Decode the first object.
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

// transportError marks an error as a transport/connection failure (daemon down) so Warm/Unload
// can wrap it as a DaemonUnreachableError. HTTP-status errors are NOT transport errors.
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

func isTransport(err error) bool {
	var te *transportError
	return asTransport(err, &te)
}

// asTransport is errors.As specialized for *transportError without importing errors at each call.
func asTransport(err error, target **transportError) bool {
	for err != nil {
		if te, ok := err.(*transportError); ok {
			*target = te
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
