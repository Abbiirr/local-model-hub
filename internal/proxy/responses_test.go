package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeResponsesRequestFlattensNamespacesAndPriorCalls(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"tools":[{
			"type":"namespace",
			"name":"mcp__playwright",
			"description":"Browser tools",
			"tools":[{
				"type":"function",
				"name":"browser_navigate",
				"description":"Navigate",
				"strict":true,
				"parameters":{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}
			}]
		}],
		"input":[{
			"type":"function_call",
			"namespace":"mcp__playwright",
			"name":"browser_navigate",
			"call_id":"call_1",
			"arguments":"{\"url\":\"https://example.com\"}"
		}]
	}`)

	out, namespaceMap := NormalizeResponsesRequest(in)
	if target := namespaceMap["mcp__playwright__browser_navigate"]; target != (ResponsesToolTarget{
		Namespace: "mcp__playwright",
		Name:      "browser_navigate",
	}) {
		t.Fatalf("namespace map target = %#v", target)
	}

	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("normalized request is invalid JSON: %v", err)
	}
	tools := root["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tool count = %d, want 1", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "mcp__playwright__browser_navigate" {
		t.Errorf("flattened tool = %#v", tool)
	}
	if _, ok := tool["parameters"]; !ok {
		t.Errorf("flattened tool lost its schema: %#v", tool)
	}

	call := root["input"].([]any)[0].(map[string]any)
	if call["name"] != "mcp__playwright__browser_navigate" {
		t.Errorf("prior function call was not flattened: %#v", call)
	}
	if _, exists := call["namespace"]; exists {
		t.Errorf("prior function call retained namespace: %#v", call)
	}
}

func TestNormalizeResponsesRequestDropsOpaqueCodexCompaction(t *testing.T) {
	in := []byte(`{
		"model":"local",
		"input":[
			{"type":"compaction","id":"cmp_1","encrypted_content":"opaque"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"do the task"}]}
		]
	}`)

	out, _ := NormalizeResponsesRequest(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	input := root["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input count = %d, want 1: %s", len(input), out)
	}
	item := input[0].(map[string]any)
	if item["type"] != "message" {
		t.Fatalf("remaining input = %#v, want message", item)
	}
}

func TestRewriteResponsesOutputRestoresNamespace(t *testing.T) {
	in := []byte(`{
		"output":[{
			"type":"function_call",
			"name":"mcp__playwright__browser_navigate",
			"call_id":"call_1",
			"arguments":"{\"url\":\"https://example.com\"}"
		}]
	}`)
	out := RewriteResponsesOutput(in, ResponsesNamespaceMap{
		"mcp__playwright__browser_navigate": {
			Namespace: "mcp__playwright",
			Name:      "browser_navigate",
		},
	})

	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("rewritten response is invalid JSON: %v", err)
	}
	call := root["output"].([]any)[0].(map[string]any)
	if call["name"] != "browser_navigate" || call["namespace"] != "mcp__playwright" {
		t.Errorf("function call was not restored: %#v", call)
	}
}

func TestRelayResponsesRewritesStreamingCallAndPreservesOtherLines(t *testing.T) {
	const unchanged = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\r\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"type":"namespace"`) {
			t.Errorf("upstream request still contained a namespace tool: %s", body)
		}
		if !strings.Contains(string(body), `"name":"mcp__playwright__browser_navigate"`) {
			t.Errorf("upstream request did not contain the flat tool: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"mcp__playwright__browser_navigate\",\"call_id\":\"call_1\",\"arguments\":\"\"}}\r\n")
		_, _ = io.WriteString(w, unchanged)
		_, _ = io.WriteString(w, "data: [DONE]\r\n\r\n")
	}))
	defer upstream.Close()

	request, namespaceMap := NormalizeResponsesRequest([]byte(`{
		"model":"m",
		"stream":true,
		"input":"navigate",
		"tools":[{
			"type":"namespace",
			"name":"mcp__playwright",
			"tools":[{
				"type":"function",
				"name":"browser_navigate",
				"parameters":{"type":"object","properties":{}}
			}]
		}]
	}`))

	rec := httptest.NewRecorder()
	_, err := RelayResponses(
		context.Background(),
		rec,
		upstream.URL,
		"/v1/responses",
		"text/event-stream",
		strings.NewReader(string(request)),
		namespaceMap,
	)
	if err != nil {
		t.Fatalf("RelayResponses error: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"name":"browser_navigate"`) ||
		!strings.Contains(body, `"namespace":"mcp__playwright"`) {
		t.Errorf("streamed function call was not restored: %s", body)
	}
	if !strings.Contains(body, unchanged) {
		t.Errorf("unrelated SSE line changed:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]\r\n\r\n") {
		t.Errorf("SSE terminator changed:\n%s", body)
	}
}

func TestRelayResponsesRewritesNonStreamingCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":[{"type":"function_call","name":"mcp__node_repl__js","call_id":"c1","arguments":"{}"}]}`)
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	_, err := RelayResponses(
		context.Background(),
		rec,
		upstream.URL,
		"/v1/responses",
		"application/json",
		strings.NewReader(`{"model":"m"}`),
		ResponsesNamespaceMap{
			"mcp__node_repl__js": {Namespace: "mcp__node_repl", Name: "js"},
		},
	)
	if err != nil {
		t.Fatalf("RelayResponses error: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"name":"js"`) ||
		!strings.Contains(rec.Body.String(), `"namespace":"mcp__node_repl"`) {
		t.Errorf("non-streaming function call was not restored: %s", rec.Body.String())
	}
}
