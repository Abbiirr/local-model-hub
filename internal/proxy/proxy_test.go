package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRelay_ByteIdentity is the #p61 streaming byte-passthrough gate (design §5): a fake upstream
// emits a fixed SSE byte sequence; Relay must deliver the EXACT bytes to the client — no re-framing,
// no JSON re-serialization. bytes.Equal MUST hold.
func TestRelay_ByteIdentity(t *testing.T) {
	// The exact byte sequence the "engine" streams (several SSE chunks + terminator).
	upstreamBytes := []byte(
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
			"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
			"data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		// Write in two writes with a flush between to exercise the chunked read loop.
		half := len(upstreamBytes) / 2
		_, _ = w.Write(upstreamBytes[:half])
		if fl != nil {
			fl.Flush()
		}
		_, _ = w.Write(upstreamBytes[half:])
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	res, err := Relay(context.Background(), rec, upstream.URL, "/v1/chat/completions", "text/event-stream",
		strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatalf("Relay error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	clientBytes := rec.Body.Bytes()
	if !bytes.Equal(upstreamBytes, clientBytes) {
		t.Errorf("byte-identity FAILED.\nupstream (%d): %q\nclient   (%d): %q",
			len(upstreamBytes), upstreamBytes, len(clientBytes), clientBytes)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream (copied through)", ct)
	}
}

// TestRelay_UpstreamUnreachable returns a typed *RelayError before any bytes are written so the
// caller can emit a 502.
func TestRelay_UpstreamUnreachable(t *testing.T) {
	rec := httptest.NewRecorder()
	_, err := Relay(context.Background(), rec, "http://127.0.0.1:1", "/v1/chat/completions", "",
		strings.NewReader(`{}`))
	if err == nil {
		t.Fatal("expected an error reaching a dead upstream")
	}
	var relErr *RelayError
	if !asRelayError(err, &relErr) || relErr.Kind != RelayUpstreamUnreachable {
		t.Errorf("want RelayUpstreamUnreachable, got %#v", err)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("no bytes should be written on a pre-write failure, got %q", rec.Body.String())
	}
}

// TestRelay_CopiesNon2xxStatus: a non-2xx upstream status is copied through verbatim (the proxy is a
// passthrough; a 400 from the engine reaches the client as a 400).
func TestRelay_CopiesNon2xxStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}))
	defer upstream.Close()
	rec := httptest.NewRecorder()
	res, err := Relay(context.Background(), rec, upstream.URL, "/v1/chat/completions", "", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("Relay error: %v", err)
	}
	if res.StatusCode != http.StatusBadRequest || rec.Code != http.StatusBadRequest {
		t.Errorf("status not copied through: relay=%d rec=%d", res.StatusCode, rec.Code)
	}
}

// TestParseRequestHead reads model + stream and re-marshals losslessly (every field preserved).
func TestParseRequestHead(t *testing.T) {
	body := `{"model":"llama-3","stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":16}`
	head, ok, err := ParseRequestHead(strings.NewReader(body))
	if err != nil || !ok {
		t.Fatalf("ParseRequestHead ok=%v err=%v", ok, err)
	}
	if head.Model != "llama-3" || !head.Stream {
		t.Errorf("model=%q stream=%v, want llama-3/true", head.Model, head.Stream)
	}
	// The re-marshaled body must still carry every field (lossless round-trip).
	var m map[string]any
	if err := json.Unmarshal(head.Body, &m); err != nil {
		t.Fatalf("re-marshaled body not valid JSON: %v", err)
	}
	if _, has := m["messages"]; !has {
		t.Error("re-marshaled body lost the messages field")
	}
	if _, has := m["max_tokens"]; !has {
		t.Error("re-marshaled body lost the max_tokens field")
	}
}

// TestParseRequestHead_MissingModel yields ok=false so the caller returns 400.
func TestParseRequestHead_MissingModel(t *testing.T) {
	_, ok, err := ParseRequestHead(strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("want ok=false for a body with no model")
	}
}

// TestSetJSONField forces stream:false on an upstream body (the async-job path) losslessly.
func TestSetJSONField(t *testing.T) {
	out := SetJSONField([]byte(`{"model":"m","stream":true,"messages":[]}`), "stream", false)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("SetJSONField produced invalid JSON: %v", err)
	}
	if m["stream"] != false {
		t.Errorf("stream = %v, want false", m["stream"])
	}
	if m["model"] != "m" {
		t.Errorf("model field lost: %v", m["model"])
	}
}

// TestWriteOpenAIError emits the OpenAI-dialect envelope with the reason in message.
func TestWriteOpenAIError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteOpenAIError(rec, http.StatusServiceUnavailable, "insufficient_vram", "admission_blocked",
		"waiting for VRAM: need 9000 MiB, free 2000 MiB")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	var env OpenAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body not valid OpenAI error JSON: %v", err)
	}
	if env.Error.Type != "insufficient_vram" || env.Error.Code != "admission_blocked" {
		t.Errorf("type/code = %q/%q", env.Error.Type, env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "9000 MiB") {
		t.Errorf("reason not preserved in message: %q", env.Error.Message)
	}
	if env.Error.Param != nil {
		t.Errorf("param should be null, got %v", *env.Error.Param)
	}
}

// asRelayError is a tiny local errors.As helper (the test file avoids importing errors just for one
// call; mirrors the pattern used in core).
func asRelayError(err error, target **RelayError) bool {
	for err != nil {
		if re, ok := err.(*RelayError); ok {
			*target = re
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
