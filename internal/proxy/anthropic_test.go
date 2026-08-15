package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseAnthropicRequest_TextAndTools(t *testing.T) {
	body := `{
		"model":"qwen-local",
		"max_tokens":512,
		"stream":true,
		"system":[{"type":"text","text":"You are a coding agent.","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"system","content":[{"type":"text","text":"Mid-conversation policy."}]},
			{"role":"user","content":[{"type":"text","text":"Read the file."}]},
			{"role":"assistant","content":[
				{"type":"text","text":"I'll inspect it."},
				{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"README.md"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"hello"}
			]}
		],
		"tools":[{"name":"Read","description":"Read a file","input_schema":{
			"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]
		}}],
		"tool_choice":{"type":"auto"}
	}`
	req, err := ParseAnthropicRequest(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseAnthropicRequest: %v", err)
	}
	if req.Model != "qwen-local" || !req.Stream {
		t.Fatalf("head = model %q stream %v", req.Model, req.Stream)
	}
	var got map[string]any
	if err := json.Unmarshal(req.OpenAIBody, &got); err != nil {
		t.Fatalf("translated body: %v", err)
	}
	if got["model"] != "qwen-local" || got["stream"] != true {
		t.Errorf("translated model/stream = %v/%v", got["model"], got["stream"])
	}
	if _, ok := got["stream_options"]; !ok {
		t.Error("stream_options.include_usage was not added")
	}
	messages := got["messages"].([]any)
	if len(messages) != 5 {
		t.Fatalf("translated messages = %d, want top-level system, message system, user, assistant, tool", len(messages))
	}
	assistant := messages[3].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if fn["name"] != "Read" || !strings.Contains(fn["arguments"].(string), "README.md") {
		t.Errorf("bad translated tool call: %#v", call)
	}
	toolResult := messages[4].(map[string]any)
	if toolResult["role"] != "tool" || toolResult["tool_call_id"] != "toolu_1" {
		t.Errorf("bad translated tool result: %#v", toolResult)
	}
}

func TestParseAnthropicRequest_RejectsUnsupportedBlock(t *testing.T) {
	_, err := ParseAnthropicRequest(strings.NewReader(`{
		"model":"m","max_tokens":10,
		"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}]
	}`))
	if err == nil || !strings.Contains(err.Error(), `unsupported Anthropic content block type "image"`) {
		t.Fatalf("expected explicit image rejection, got %v", err)
	}
}

func TestRelayAnthropic_NonStreamingToolUse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl_1","model":"qwen-local",
			"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{
				"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"README.md\"}"}
			}]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":40,"completion_tokens":12}
		}`))
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	err := RelayAnthropic(context.Background(), rec, upstream.URL, AnthropicRequest{
		Model: "qwen-local", OpenAIBody: []byte(`{"model":"qwen-local","stream":false}`),
	})
	if err != nil {
		t.Fatalf("RelayAnthropic: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response JSON: %v", err)
	}
	if got["type"] != "message" || got["stop_reason"] != "tool_use" {
		t.Errorf("type/stop_reason = %v/%v", got["type"], got["stop_reason"])
	}
	content := got["content"].([]any)
	block := content[0].(map[string]any)
	if block["type"] != "tool_use" || block["name"] != "Read" {
		t.Errorf("bad tool_use block: %#v", block)
	}
	usage := got["usage"].(map[string]any)
	if usage["input_tokens"] != float64(40) || usage["output_tokens"] != float64(12) {
		t.Errorf("bad usage: %#v", usage)
	}
}

func TestRelayAnthropic_StreamingTextAndTool(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"id":"chatcmpl_2","model":"qwen-local","choices":[{"delta":{"content":"Checking "},"finish_reason":null}]}`,
			`{"id":"chatcmpl_2","model":"qwen-local","choices":[{"delta":{"content":"now."},"finish_reason":null}]}`,
			`{"id":"chatcmpl_2","model":"qwen-local","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_2","type":"function","function":{"name":"Read","arguments":"{\"file_"}}]},"finish_reason":null}]}`,
			`{"id":"chatcmpl_2","model":"qwen-local","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"path\":\"README.md\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"id":"chatcmpl_2","model":"qwen-local","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":20}}`,
		}
		for _, chunk := range chunks {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	err := RelayAnthropic(context.Background(), rec, upstream.URL, AnthropicRequest{
		Model: "qwen-local", Stream: true,
		OpenAIBody: []byte(`{"model":"qwen-local","stream":true}`),
	})
	if err != nil {
		t.Fatalf("RelayAnthropic: %v", err)
	}
	body := rec.Body.String()
	for _, required := range []string{
		"event: message_start",
		`"text":"Checking "`,
		`"type":"text_delta"`,
		`"id":"call_2"`,
		`"name":"Read"`,
		`"type":"tool_use"`,
		`"partial_json":"{\"file_"`,
		`"type":"input_json_delta"`,
		`"stop_reason":"tool_use"`,
		`"output_tokens":20`,
		"event: message_stop",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("stream missing %q:\n%s", required, body)
		}
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
}

func TestRelayAnthropic_ConvertsUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"unsupported thinking field"}}`))
	}))
	defer upstream.Close()
	rec := httptest.NewRecorder()
	err := RelayAnthropic(context.Background(), rec, upstream.URL, AnthropicRequest{
		Model: "m", OpenAIBody: []byte(`{"model":"m"}`),
	})
	if err != nil {
		t.Fatalf("RelayAnthropic: %v", err)
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsupported thinking field") {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
}
