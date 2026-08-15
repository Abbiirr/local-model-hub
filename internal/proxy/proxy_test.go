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

func TestNormalizeResponsesInput_RoleTypedItems(t *testing.T) {
	in := []byte(`{"model":"m","input":[
		{"type":"user","content":"task"},
		{"type":"assistant","content":[{"type":"text","text":"answer"}]},
		{"type":"message","role":"assistant","content":[
			{"type":"input_text","text":"prior answer"},
			{"type":"encrypted_content","encrypted_content":"opaque"}
		]},
		{"type":"message","role":"assistant","content":[
			{"type":"output_text","text":"Message Type: NEW_TASK\nPayload:\nRead README.md"}
		]},
		{"type":"function_call_output","call_id":"c1","output":"ok"}
	]}`)
	out := NormalizeResponsesInput(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("normalized body: %v", err)
	}
	items := root["input"].([]any)
	user := items[0].(map[string]any)
	if user["type"] != "message" || user["role"] != "user" {
		t.Errorf("user item not normalized: %#v", user)
	}
	assistant := items[1].(map[string]any)
	assistantContent := assistant["content"].([]any)[0].(map[string]any)
	if assistantContent["type"] != "output_text" {
		t.Errorf("assistant content not normalized: %#v", assistantContent)
	}
	canonical := items[2].(map[string]any)
	canonicalContent := canonical["content"].([]any)[0].(map[string]any)
	if canonicalContent["type"] != "output_text" {
		t.Errorf("canonical assistant content not normalized: %#v", canonicalContent)
	}
	if len(canonical["content"].([]any)) != 1 {
		t.Errorf("opaque encrypted content was not removed: %#v", canonical["content"])
	}
	delegated := items[3].(map[string]any)
	if delegated["role"] != "user" {
		t.Errorf("NEW_TASK envelope was not changed to a user turn: %#v", delegated)
	}
	delegatedContent := delegated["content"].([]any)[0].(map[string]any)
	if delegatedContent["type"] != "input_text" {
		t.Errorf("NEW_TASK content was not changed to input_text: %#v", delegatedContent)
	}
	output := items[4].(map[string]any)
	if output["type"] != "function_call_output" {
		t.Errorf("non-message item was changed: %#v", output)
	}
}

func TestNormalizeResponsesInput_MovesSystemAndDeveloperMessagesToLeadingInstructions(t *testing.T) {
	in := []byte(`{"model":"m","instructions":"base rules","input":[
		{"type":"user","content":"prior task"},
		{"type":"developer","content":[{"type":"text","text":"repo rules"}]},
		{"type":"message","role":"system","content":"safety rules"},
		{"type":"user","content":"current task"}
	]}`)

	out := NormalizeResponsesInput(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("normalized body: %v", err)
	}
	if got, want := root["instructions"], "base rules\n\nrepo rules\n\nsafety rules"; got != want {
		t.Fatalf("instructions = %#v, want %#v", got, want)
	}
	items := root["input"].([]any)
	if len(items) != 2 {
		t.Fatalf("input length = %d, want 2: %#v", len(items), items)
	}
	for _, raw := range items {
		item := raw.(map[string]any)
		if role, _ := item["role"].(string); role == "system" || role == "developer" {
			t.Errorf("late instruction message remained in input: %#v", item)
		}
	}
}

func TestNormalizeResponsesInput_PreservesNonTextDeveloperMessage(t *testing.T) {
	in := []byte(`{"model":"m","input":[
		{"type":"developer","content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]},
		{"type":"user","content":"task"}
	]}`)

	out := NormalizeResponsesInput(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("normalized body: %v", err)
	}
	items := root["input"].([]any)
	if len(items) != 2 {
		t.Fatalf("input length = %d, want 2: %#v", len(items), items)
	}
	developer := items[0].(map[string]any)
	if developer["type"] != "message" || developer["role"] != "developer" {
		t.Errorf("non-text developer message was not preserved canonically: %#v", developer)
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
