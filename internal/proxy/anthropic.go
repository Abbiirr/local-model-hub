package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AnthropicRequest is the translated head of an Anthropic Messages request.
// OpenAIBody contains the equivalent OpenAI chat-completions request accepted
// by LocalHub's local engines.
type AnthropicRequest struct {
	Model      string
	Stream     bool
	OpenAIBody []byte
}

type anthropicWireRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	Messages    []anthropicMessage `json:"messages"`
	System      json.RawMessage    `json:"system"`
	Stream      bool               `json:"stream"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	TopK        *int               `json:"top_k,omitempty"`
	Stop        json.RawMessage    `json:"stop_sequences,omitempty"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	ToolChoice  json.RawMessage    `json:"tool_choice,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ParseAnthropicRequest translates the subset of Anthropic Messages used by
// Claude Code into an OpenAI chat-completions request. Unsupported content
// blocks fail closed with a human-readable reason instead of being discarded.
func ParseAnthropicRequest(r io.Reader) (AnthropicRequest, error) {
	var in anthropicWireRequest
	dec := json.NewDecoder(r)
	if err := dec.Decode(&in); err != nil {
		return AnthropicRequest{}, fmt.Errorf("could not parse Anthropic request JSON: %w", err)
	}
	in.Model = strings.TrimSpace(in.Model)
	if in.Model == "" {
		return AnthropicRequest{}, errors.New(`the request body must include a non-empty "model" field naming a local model`)
	}
	if in.MaxTokens <= 0 {
		return AnthropicRequest{}, errors.New(`the request body must include a positive "max_tokens" value`)
	}
	if len(in.Messages) == 0 {
		return AnthropicRequest{}, errors.New(`the request body must include at least one message`)
	}

	messages := make([]openAIMessage, 0, len(in.Messages)+1)
	if len(bytes.TrimSpace(in.System)) > 0 && string(bytes.TrimSpace(in.System)) != "null" {
		system, err := anthropicText(in.System)
		if err != nil {
			return AnthropicRequest{}, fmt.Errorf("unsupported Anthropic system content: %w", err)
		}
		if system != "" {
			messages = append(messages, openAIMessage{Role: "system", Content: system})
		}
	}
	for _, msg := range in.Messages {
		converted, err := convertAnthropicMessage(msg)
		if err != nil {
			return AnthropicRequest{}, err
		}
		messages = append(messages, converted...)
	}

	out := map[string]any{
		"model":      in.Model,
		"messages":   messages,
		"max_tokens": in.MaxTokens,
		"stream":     in.Stream,
	}
	if in.Stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if in.TopK != nil {
		out["top_k"] = *in.TopK
	}
	if len(bytes.TrimSpace(in.Stop)) > 0 && string(bytes.TrimSpace(in.Stop)) != "null" {
		var stop any
		if err := json.Unmarshal(in.Stop, &stop); err != nil {
			return AnthropicRequest{}, fmt.Errorf("invalid stop_sequences: %w", err)
		}
		out["stop"] = stop
	}
	if len(in.Tools) > 0 {
		tools := make([]map[string]any, 0, len(in.Tools))
		for _, tool := range in.Tools {
			var schema any
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				return AnthropicRequest{}, fmt.Errorf("tool %q has invalid input_schema: %w", tool.Name, err)
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tool.Name,
					"description": tool.Description,
					"parameters":  schema,
				},
			})
		}
		out["tools"] = tools
		if choice, err := convertToolChoice(in.ToolChoice); err != nil {
			return AnthropicRequest{}, err
		} else if choice != nil {
			out["tool_choice"] = choice
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return AnthropicRequest{}, fmt.Errorf("could not encode translated OpenAI request: %w", err)
	}
	return AnthropicRequest{Model: in.Model, Stream: in.Stream, OpenAIBody: body}, nil
}

func convertAnthropicMessage(msg anthropicMessage) ([]openAIMessage, error) {
	role := strings.TrimSpace(msg.Role)
	if role != "user" && role != "assistant" && role != "system" {
		return nil, fmt.Errorf("unsupported Anthropic message role %q", role)
	}
	var plain string
	if err := json.Unmarshal(msg.Content, &plain); err == nil {
		return []openAIMessage{{Role: role, Content: plain}}, nil
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil, fmt.Errorf("invalid Anthropic %s content: %w", role, err)
	}

	var textParts []string
	var calls []openAIToolCall
	var toolResults []openAIMessage
	for _, block := range blocks {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "tool_use":
			if role != "assistant" {
				return nil, errors.New("tool_use blocks are only valid in assistant messages")
			}
			args := strings.TrimSpace(string(block.Input))
			if args == "" || args == "null" {
				args = "{}"
			}
			calls = append(calls, openAIToolCall{
				ID:   block.ID,
				Type: "function",
				Function: openAIFunctionCall{
					Name:      block.Name,
					Arguments: args,
				},
			})
		case "tool_result":
			if role != "user" {
				return nil, errors.New("tool_result blocks are only valid in user messages")
			}
			content, err := anthropicText(block.Content)
			if err != nil {
				return nil, fmt.Errorf("unsupported tool_result content for %q: %w", block.ToolUseID, err)
			}
			toolResults = append(toolResults, openAIMessage{
				Role:       "tool",
				Content:    content,
				ToolCallID: block.ToolUseID,
			})
		default:
			return nil, fmt.Errorf("unsupported Anthropic content block type %q", block.Type)
		}
	}

	text := strings.Join(textParts, "")
	if role == "system" {
		if len(calls) > 0 || len(toolResults) > 0 {
			return nil, errors.New("system messages may only contain text blocks")
		}
		return []openAIMessage{{Role: "system", Content: text}}, nil
	}
	if role == "assistant" {
		content := any(text)
		if text == "" && len(calls) > 0 {
			content = nil
		}
		return []openAIMessage{{Role: role, Content: content, ToolCalls: calls}}, nil
	}
	result := make([]openAIMessage, 0, 1+len(toolResults))
	if text != "" {
		result = append(result, openAIMessage{Role: "user", Content: text})
	}
	result = append(result, toolResults...)
	if len(result) == 0 {
		result = append(result, openAIMessage{Role: "user", Content: ""})
	}
	return result, nil
}

func anthropicText(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", err
	}
	var parts []string
	for _, block := range blocks {
		if block.Type != "text" {
			return "", fmt.Errorf("content block %q cannot be represented as text", block.Type)
		}
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, ""), nil
}

func convertToolChoice(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil, fmt.Errorf("invalid tool_choice: %w", err)
	}
	switch choice.Type {
	case "", "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "none":
		return "none", nil
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}, nil
	default:
		return nil, fmt.Errorf("unsupported Anthropic tool_choice type %q", choice.Type)
	}
}

// WriteAnthropicError emits the error envelope consumed by Anthropic SDKs and
// Claude Code. message must always contain the human-readable LocalHub reason.
func WriteAnthropicError(w http.ResponseWriter, status int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errorType,
			"message": message,
		},
	})
}

// RelayAnthropic sends the translated request to an OpenAI-compatible local
// engine and converts its response into Anthropic Messages format.
func RelayAnthropic(ctx context.Context, w http.ResponseWriter, targetBaseURL string, req AnthropicRequest) error {
	target := strings.TrimRight(targetBaseURL, "/") + "/v1/chat/completions"
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(req.OpenAIBody))
	if err != nil {
		return &RelayError{Kind: RelayBuildFailed, Err: err}
	}
	upReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		upReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := (&http.Client{Timeout: 0}).Do(upReq)
	if err != nil {
		return &RelayError{Kind: RelayUpstreamUnreachable, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = fmt.Sprintf("local engine returned HTTP %d", resp.StatusCode)
		}
		WriteAnthropicError(w, resp.StatusCode, "api_error", message)
		return nil
	}
	if req.Stream {
		return relayAnthropicStream(w, resp.Body, req.Model)
	}
	return relayAnthropicJSON(w, resp.Body, req.Model)
}

type openAIResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   any              `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage openAIUsage `json:"usage"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func relayAnthropicJSON(w http.ResponseWriter, r io.Reader, requestedModel string) error {
	var upstream openAIResponse
	if err := json.NewDecoder(r).Decode(&upstream); err != nil {
		return fmt.Errorf("could not decode local engine response: %w", err)
	}
	if len(upstream.Choices) == 0 {
		return errors.New("local engine response contained no choices")
	}
	choice := upstream.Choices[0]
	content := make([]map[string]any, 0, 1+len(choice.Message.ToolCalls))
	if text := openAIText(choice.Message.Content); text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, call := range choice.Message.ToolCalls {
		var input any = map[string]any{}
		if strings.TrimSpace(call.Function.Arguments) != "" {
			if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
				input = map[string]any{"_raw": call.Function.Arguments}
			}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input,
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	model := requestedModel
	if upstream.Model != "" {
		model = upstream.Model
	}
	id := upstream.ID
	if id == "" {
		id = fmt.Sprintf("msg_localhub_%d", time.Now().UnixNano())
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": anthropicStopReason(choice.FinishReason),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens": upstream.Usage.PromptTokens, "output_tokens": upstream.Usage.CompletionTokens,
		},
	})
}

func openAIText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}

func anthropicStopReason(reason string) string {
	switch reason {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

type openAIStreamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   any `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
}

type streamToolState struct {
	ContentIndex int
	ID           string
	Name         string
	Started      bool
}

func relayAnthropicStream(w http.ResponseWriter, r io.Reader, requestedModel string) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("response writer does not support streaming")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	messageID := fmt.Sprintf("msg_localhub_%d", time.Now().UnixNano())
	model := requestedModel
	started := false
	textStarted := false
	textIndex := 0
	nextIndex := 0
	tools := map[int]*streamToolState{}
	inputTokens := 0
	outputTokens := 0
	finishReason := "stop"

	emit := func(event string, payload any) error {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	startMessage := func() error {
		if started {
			return nil
		}
		started = true
		return emit("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": messageID, "type": "message", "role": "assistant", "model": model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
			},
		})
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("could not decode local engine SSE chunk: %w", err)
		}
		if chunk.ID != "" {
			messageID = chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.Usage != nil {
			inputTokens = chunk.Usage.PromptTokens
			outputTokens = chunk.Usage.CompletionTokens
		}
		if err := startMessage(); err != nil {
			return err
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				finishReason = *choice.FinishReason
			}
			if text := openAIText(choice.Delta.Content); text != "" {
				if !textStarted {
					textIndex = nextIndex
					nextIndex++
					textStarted = true
					if err := emit("content_block_start", map[string]any{
						"type": "content_block_start", "index": textIndex,
						"content_block": map[string]any{"type": "text", "text": ""},
					}); err != nil {
						return err
					}
				}
				if err := emit("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": textIndex,
					"delta": map[string]any{"type": "text_delta", "text": text},
				}); err != nil {
					return err
				}
			}
			for _, call := range choice.Delta.ToolCalls {
				state := tools[call.Index]
				if state == nil {
					state = &streamToolState{ContentIndex: nextIndex}
					nextIndex++
					tools[call.Index] = state
				}
				if call.ID != "" {
					state.ID += call.ID
				}
				if call.Function.Name != "" {
					state.Name += call.Function.Name
				}
				if !state.Started {
					state.Started = true
					if err := emit("content_block_start", map[string]any{
						"type": "content_block_start", "index": state.ContentIndex,
						"content_block": map[string]any{
							"type": "tool_use", "id": state.ID, "name": state.Name, "input": map[string]any{},
						},
					}); err != nil {
						return err
					}
				}
				if call.Function.Arguments != "" {
					if err := emit("content_block_delta", map[string]any{
						"type": "content_block_delta", "index": state.ContentIndex,
						"delta": map[string]any{
							"type": "input_json_delta", "partial_json": call.Function.Arguments,
						},
					}); err != nil {
						return err
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := startMessage(); err != nil {
		return err
	}
	if textStarted {
		if err := emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": textIndex}); err != nil {
			return err
		}
	}
	for _, state := range tools {
		if state.Started {
			if err := emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": state.ContentIndex}); err != nil {
				return err
			}
		}
	}
	if err := emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": anthropicStopReason(finishReason), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outputTokens},
	}); err != nil {
		return err
	}
	return emit("message_stop", map[string]any{"type": "message_stop"})
}
