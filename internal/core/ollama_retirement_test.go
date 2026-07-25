package core

import (
	"context"
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/instances"
)

func TestOllamaRetirementHidesAndRefusesPersistedRows(t *testing.T) {
	d := coreTestDB(t)
	ctx := context.Background()
	if _, err := d.ExecContext(ctx, `INSERT INTO models (id, display_name, source, format) VALUES ('legacy-ollama','mistral:latest','ollama','ollama')`); err != nil {
		t.Fatal(err)
	}
	svc := New(Deps{Cfg: config.Default(), DB: d, InstanceStore: instances.NewStore(d.DB)})

	models, err := svc.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 0 {
		t.Fatalf("Models() returned retired Ollama rows: %+v", models)
	}
	if _, err := svc.StartInstance(ctx, StartInstanceRequest{ModelID: "legacy-ollama"}); err == nil || !strings.Contains(err.Error(), "Ollama is retired") {
		t.Fatalf("StartInstance legacy row error = %v, want clear retirement refusal", err)
	}
}
