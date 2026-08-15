package api

import (
	"encoding/json"
	"testing"

	"github.com/localhub/localhub/internal/core"
)

func TestCodexModelsProvidesRequiredLocalMetadata(t *testing.T) {
	models := codexModels([]core.OpenAIModel{
		{ID: "gguf-qwen3-6-27b-16gb-vram-mtp-mini-iq4-xs", Object: "model", OwnedBy: "localhub"},
		{ID: "kat", Object: "model", OwnedBy: "localhub"},
	})
	if len(models) != 2 {
		t.Fatalf("metadata count = %d, want 2", len(models))
	}
	qwen := models[0]
	if qwen.ContextWindow != 8192 || qwen.AutoCompactTokenLimit != 8000 || qwen.TruncationPolicy.Limit != 8000 {
		t.Fatalf("unexpected Qwen metadata: %#v", qwen)
	}
	got := models[1]
	if got.Slug != "kat" || got.ContextWindow != 167168 || got.AutoCompactTokenLimit != 153600 {
		t.Fatalf("unexpected KAT metadata: %#v", got)
	}
	if got.MaxContextWindow != 167168 || got.EffectiveContextWindowPercent != 100 || got.TruncationPolicy.Limit != 153600 {
		t.Fatalf("inconsistent KAT context metadata: %#v", got)
	}
	if got.ShellType != "unified_exec" || got.ToolMode == nil || *got.ToolMode != "direct" {
		t.Fatalf("unexpected tool metadata: %#v", got)
	}

	body, err := json.Marshal(openAIListResponse{
		Object: "list",
		Data: []core.OpenAIModel{
			{ID: "gguf-qwen3-6-27b-16gb-vram-mtp-mini-iq4-xs", Object: "model", OwnedBy: "localhub"},
			{ID: "kat", Object: "model", OwnedBy: "localhub"},
		},
		Models: models,
	})
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, ok := envelope["data"]; !ok {
		t.Error("OpenAI data envelope was lost")
	}
	if _, ok := envelope["models"]; !ok {
		t.Error("Codex models envelope is missing")
	}
}
