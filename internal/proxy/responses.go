package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// ResponsesToolTarget is the canonical Codex identity for a function nested in
// a Responses API namespace tool.
type ResponsesToolTarget struct {
	Namespace string
	Name      string
}

// ResponsesNamespaceMap maps the flat function name understood by local
// Responses implementations back to the namespace/name pair Codex routes.
type ResponsesNamespaceMap map[string]ResponsesToolTarget

// NormalizeResponsesRequest performs the existing Codex input normalization
// and flattens namespace tools for local Responses implementations such as
// llama.cpp. Codex's namespace wrapper is not part of the public Responses API,
// and llama.cpp intentionally skips it.
//
// A nested tool named browser_navigate in namespace mcp__playwright becomes the
// ordinary function mcp__playwright__browser_navigate. The returned map lets
// RelayResponses restore the namespace on function-call output so Codex can
// dispatch the call to the original MCP server.
func NormalizeResponsesRequest(body []byte) ([]byte, ResponsesNamespaceMap) {
	normalized := NormalizeResponsesInput(body)

	var root map[string]any
	if err := json.Unmarshal(normalized, &root); err != nil {
		return normalized, nil
	}

	namespaceMap := ResponsesNamespaceMap{}
	changed := false

	if tools, ok := root["tools"].([]any); ok {
		flattened := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok || tool["type"] != "namespace" {
				flattened = append(flattened, raw)
				continue
			}

			namespace, _ := tool["name"].(string)
			children, childrenOK := tool["tools"].([]any)
			if strings.TrimSpace(namespace) == "" || !childrenOK {
				flattened = append(flattened, raw)
				continue
			}

			namespaceDescription, _ := tool["description"].(string)
			for _, childRaw := range children {
				child, ok := childRaw.(map[string]any)
				if !ok || child["type"] != "function" {
					continue
				}
				childName, _ := child["name"].(string)
				if strings.TrimSpace(childName) == "" {
					continue
				}

				flatName := namespace + "__" + childName
				flat := make(map[string]any, len(child)+1)
				for key, value := range child {
					flat[key] = value
				}
				flat["name"] = flatName
				if _, hasDescription := flat["description"]; !hasDescription &&
					strings.TrimSpace(namespaceDescription) != "" {
					flat["description"] = namespaceDescription
				}
				flattened = append(flattened, flat)
				namespaceMap[flatName] = ResponsesToolTarget{
					Namespace: namespace,
					Name:      childName,
				}
			}
			changed = true
		}
		if changed {
			root["tools"] = flattened
		}
	}

	// Previous assistant function calls are replayed in input on later turns.
	// Restore the flat upstream name before llama.cpp sees them.
	if input, ok := root["input"].([]any); ok {
		filtered := make([]any, 0, len(input))
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				filtered = append(filtered, raw)
				continue
			}
			// Codex may prepend an opaque encrypted compaction checkpoint when a
			// child is forked from an already-compacted parent. Local providers
			// cannot decrypt it and llama.cpp rejects the unknown item type before
			// generation. The delegated task and usable messages remain in input.
			if item["type"] == "compaction" {
				changed = true
				continue
			}
			filtered = append(filtered, raw)
			if item["type"] != "function_call" {
				continue
			}
			namespace, _ := item["namespace"].(string)
			name, _ := item["name"].(string)
			if namespace == "" || name == "" {
				continue
			}
			flatName := namespace + "__" + name
			item["name"] = flatName
			delete(item, "namespace")
			namespaceMap[flatName] = ResponsesToolTarget{
				Namespace: namespace,
				Name:      name,
			}
			changed = true
		}
		if len(filtered) != len(input) {
			root["input"] = filtered
		}
	}

	if !changed {
		return normalized, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return normalized, nil
	}
	return out, namespaceMap
}

// RewriteResponsesOutput restores Codex namespace metadata in a non-streaming
// Responses JSON body. When no mapped function call is present, it returns the
// original bytes unchanged.
func RewriteResponsesOutput(body []byte, namespaceMap ResponsesNamespaceMap) []byte {
	if len(namespaceMap) == 0 {
		return body
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return body
	}
	if !rewriteResponsesFunctionCalls(value, namespaceMap) {
		return body
	}
	out, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return out
}

func rewriteResponsesFunctionCalls(value any, namespaceMap ResponsesNamespaceMap) bool {
	changed := false
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if rewriteResponsesFunctionCalls(item, namespaceMap) {
				changed = true
			}
		}
	case map[string]any:
		if typed["type"] == "function_call" {
			if flatName, ok := typed["name"].(string); ok {
				if target, found := namespaceMap[flatName]; found {
					typed["name"] = target.Name
					typed["namespace"] = target.Namespace
					changed = true
				}
			}
		}
		for _, child := range typed {
			if rewriteResponsesFunctionCalls(child, namespaceMap) {
				changed = true
			}
		}
	}
	return changed
}

// RelayResponses relays a Responses API request while translating only mapped
// namespace function calls in the response. All other SSE lines remain
// byte-identical, and each complete event line is flushed as it arrives.
func RelayResponses(
	ctx context.Context,
	w http.ResponseWriter,
	targetBaseURL, path, accept string,
	body io.Reader,
	namespaceMap ResponsesNamespaceMap,
) (RelayResult, error) {
	if len(namespaceMap) == 0 {
		return Relay(ctx, w, targetBaseURL, path, accept, body)
	}

	target := strings.TrimRight(targetBaseURL, "/") + path
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return RelayResult{}, &RelayError{Kind: RelayBuildFailed, Err: err}
	}
	upReq.Header.Set("Content-Type", "application/json")
	if accept != "" {
		upReq.Header.Set("Accept", accept)
	}

	resp, err := (&http.Client{Timeout: 0}).Do(upReq)
	if err != nil {
		return RelayResult{}, &RelayError{Kind: RelayUpstreamUnreachable, Err: err}
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)

	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		responseBody, readErr := io.ReadAll(resp.Body)
		if len(responseBody) > 0 {
			_, _ = w.Write(RewriteResponsesOutput(responseBody, namespaceMap))
		}
		if flusher != nil {
			flusher.Flush()
		}
		if readErr != nil {
			return RelayResult{StatusCode: resp.StatusCode, ContentType: contentType}, nil
		}
		return RelayResult{StatusCode: resp.StatusCode, ContentType: contentType}, nil
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			rewritten := rewriteResponsesSSELine(line, namespaceMap)
			if _, writeErr := w.Write(rewritten); writeErr != nil {
				return RelayResult{StatusCode: resp.StatusCode, ContentType: contentType}, nil
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return RelayResult{StatusCode: resp.StatusCode, ContentType: contentType}, nil
		}
	}
}

func rewriteResponsesSSELine(line []byte, namespaceMap ResponsesNamespaceMap) []byte {
	newline := []byte{}
	payloadLine := line
	switch {
	case bytes.HasSuffix(payloadLine, []byte("\r\n")):
		newline = []byte("\r\n")
		payloadLine = payloadLine[:len(payloadLine)-2]
	case bytes.HasSuffix(payloadLine, []byte("\n")):
		newline = []byte("\n")
		payloadLine = payloadLine[:len(payloadLine)-1]
	}

	const prefix = "data:"
	if !bytes.HasPrefix(payloadLine, []byte(prefix)) {
		return line
	}
	payload := bytes.TrimSpace(payloadLine[len(prefix):])
	if bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}
	rewritten := RewriteResponsesOutput(payload, namespaceMap)
	if bytes.Equal(rewritten, payload) {
		return line
	}
	out := make([]byte, 0, len(rewritten)+len(newline)+len(prefix)+1)
	out = append(out, prefix...)
	out = append(out, ' ')
	out = append(out, rewritten...)
	out = append(out, newline...)
	return out
}
