// Package proxy hosts the transport-shaped helpers for LocalHub's thin OpenAI-compatible proxy
// (Phase 6 / P6.1): the byte-passthrough relay core, the OpenAI-dialect error writer, and the
// small request-body head parser used to read the `model` field before relaying. All BUSINESS
// logic (model resolution → admission) lives in internal/core (the one-core rule, PLAN.md
// decision 7); this package only shapes bytes on the wire.
//
// ---------------------------------------------------------------------------------------------
// WHAT THIS PACKAGE DELIBERATELY DOES **NOT** DO (grep-auditable, gate #p61; ADR-002):
//
// LocalHub's OpenAI proxy is a SINGLE local base URL that reaches any local model with residency
// handled. It is NOT a gateway. The following are LiteLLM's job, not ours, and are intentionally
// absent here (do not add them — route them to LiteLLM downstream instead):
//
//   - API-key management / virtual keys
//   - per-key or per-model budgets / spend tracking / cost accounting
//   - rate limiting
//   - provider fallback / retry-across-providers
//   - multi-provider routing (OpenAI / Anthropic / etc.)
//   - load-balancing across duplicate deployments
//   - request/response caching
//   - prompt logging by default
//
// The proxy only ever relays /v1/chat/completions and /v1/completions to a local engine that
// LocalHub's own residency admitted. It never forwards vLLM dev endpoints
// (/sleep, /wake_up, /collective_rpc, /reset_prefix_cache) — security.md.
// ---------------------------------------------------------------------------------------------
package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// OpenAIError is the OpenAI-dialect error envelope. OpenAI SDKs parse {"error":{message,type,
// code,param}}, which differs from LocalHub's internal {"error":{code,message,reason}}. The /v1/*
// routes emit THIS shape so off-the-shelf clients work — but the human-readable reason is always
// carried in `message` so the mandatory-reason gate survives the dialect switch.
type OpenAIError struct {
	Error OpenAIErrorBody `json:"error"`
}

// OpenAIErrorBody is the inner error object of the OpenAI error envelope.
type OpenAIErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Code    string  `json:"code"`
	Param   *string `json:"param"`
}

// WriteOpenAIError emits the OpenAI-dialect error envelope with the given HTTP status. The
// human-readable reason MUST be passed as message (never lose the reason — the mandatory-reason
// gate). param is always null (LocalHub does not do per-parameter validation errors).
func WriteOpenAIError(w http.ResponseWriter, status int, oaiType, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(OpenAIError{Error: OpenAIErrorBody{
		Message: message,
		Type:    oaiType,
		Code:    code,
		Param:   nil,
	}})
}

// RequestHead is the minimal set of fields the proxy must read from an OpenAI request body before
// relaying: the model to resolve, and whether the client asked to stream (informational — the
// relay is byte-identical either way). The rest of the body passes through verbatim.
type RequestHead struct {
	Model  string
	Stream bool
	// Body is the re-marshaled request body to send upstream. Per the design (decode-then-remarshal,
	// §1.5) the body is decoded into a map and re-marshaled so every field the client sent is
	// preserved; the response is what must be byte-identical, not the request.
	Body []byte
}

// ParseRequestHead decodes an OpenAI request body, reads model + stream, and returns the
// re-marshaled body for the upstream relay. It is decode-then-remarshal (design §1.5): OpenAI
// bodies are small (KB), so decoding into a map and re-marshaling is simpler and safer than
// tee-reading a head, and it losslessly preserves every field the client sent. A body with no
// (or an empty) `model` yields ok=false so the caller can return a 400 invalid_body.
func ParseRequestHead(r io.Reader) (RequestHead, bool, error) {
	var m map[string]any
	dec := json.NewDecoder(r)
	if err := dec.Decode(&m); err != nil {
		return RequestHead{}, false, err
	}
	model, _ := m["model"].(string)
	stream, _ := m["stream"].(bool)
	if strings.TrimSpace(model) == "" {
		return RequestHead{}, false, nil
	}
	body, err := json.Marshal(m)
	if err != nil {
		return RequestHead{}, false, err
	}
	return RequestHead{Model: model, Stream: stream, Body: body}, true, nil
}

// SetJSONField sets key=value on a raw JSON object body and returns the re-marshaled bytes. Used by
// the async-job worker to force `stream:false` on the upstream body (an async job returns one final
// result, not a stream). It is a no-op-safe helper: a malformed body returns the input unchanged.
func SetJSONField(body []byte, key string, value any) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m[key] = value
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// RelayResult reports the upstream status returned by Relay (for callers that record it, e.g. the
// async job path). For the streaming HTTP proxy the bytes are written straight to the client and
// only the status is surfaced.
type RelayResult struct {
	StatusCode  int
	ContentType string
}

// Relay performs the byte-identical passthrough of an upstream OpenAI response to w. It mirrors the
// proven loop in internal/api/instances.go:handleInstanceCompletion:
//
//   - builds POST {targetBaseURL}{path} with ctx (so a client disconnect cancels the upstream
//     generation — the deliberate "cancel upstream on client disconnect" contract, design §1.6),
//   - streams the request body verbatim,
//   - copies the upstream status + Content-Type, then loops read→write→flush over a 32 KiB buffer
//     writing EXACTLY the bytes read from upstream (no SSE re-parse, no JSON re-serialization —
//     the byte-identity property the gate tests).
//
// The caller owns in-flight accounting (BeginInstanceRequest/Done) around this call. Relay never
// touches residency or the instance lifecycle; it only moves bytes. It returns a *RelayError when
// the upstream is unreachable so the caller can map it to a 502 (the response is not yet written in
// that case); once bytes have been written it returns nil even on a mid-stream break (the client
// already got a 2xx + partial stream, matching the existing relay).
func Relay(ctx context.Context, w http.ResponseWriter, targetBaseURL, path string, accept string, body io.Reader) (RelayResult, error) {
	target := strings.TrimRight(targetBaseURL, "/") + path
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return RelayResult{}, &RelayError{Kind: RelayBuildFailed, Err: err}
	}
	upReq.Header.Set("Content-Type", "application/json")
	if accept != "" {
		upReq.Header.Set("Accept", accept)
	}

	client := &http.Client{Timeout: 0} // streaming; no overall timeout
	resp, err := client.Do(upReq)
	if err != nil {
		return RelayResult{}, &RelayError{Kind: RelayUpstreamUnreachable, Err: err}
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return RelayResult{StatusCode: resp.StatusCode, ContentType: ct}, nil // client gone
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			// EOF or a mid-stream break: nothing more to send. The client already has a 2xx +
			// whatever streamed; matches the existing instance relay.
			return RelayResult{StatusCode: resp.StatusCode, ContentType: ct}, nil
		}
	}
}

// RelayError classifies a relay failure that occurred BEFORE any response bytes were written, so
// the caller can emit an OpenAI error envelope with the right status.
type RelayError struct {
	Kind RelayErrorKind
	Err  error
}

// RelayErrorKind enumerates the pre-write relay failures.
type RelayErrorKind int

const (
	// RelayBuildFailed: the upstream request could not be constructed (bad URL, ctx). Maps to 500.
	RelayBuildFailed RelayErrorKind = iota
	// RelayUpstreamUnreachable: the engine did not answer. Maps to 502 upstream_unreachable.
	RelayUpstreamUnreachable
)

func (e *RelayError) Error() string {
	if e == nil || e.Err == nil {
		return "relay error"
	}
	return e.Err.Error()
}

func (e *RelayError) Unwrap() error { return e.Err }
