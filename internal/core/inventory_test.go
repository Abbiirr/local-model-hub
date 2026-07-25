package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/db"
	"github.com/localhub/localhub/internal/engines/ollama"
	"github.com/localhub/localhub/internal/inventory"
)

func coreTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "core.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

func writeGGUF(t *testing.T, dir, name string) {
	t.Helper()
	body := append([]byte("GGUF"), 0x03, 0x00, 0x00, 0x00)
	body = append(body, make([]byte, 32)...)
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestRescanOllamaUnreachableDoesNotFailOrMarkMissing is the core safeguard: when the Ollama
// daemon is unreachable, the disk scan still succeeds, OllamaScanned is false, a note is
// recorded, and pre-existing Ollama rows are NOT marked missing (the ollama source is
// excluded from reconciliation).
func TestRescanOllamaUnreachableDoesNotFailOrMarkMissing(t *testing.T) {
	d := coreTestDB(t)
	root := t.TempDir()
	writeGGUF(t, root, "disk-model-q4_k_m.gguf")

	// Seed a pre-existing Ollama row directly (as if a prior scan when Ollama was up).
	if _, err := d.Exec(
		`INSERT INTO models (id, display_name, source, format) VALUES ('ollama-seed','seed','ollama','ollama')`,
	); err != nil {
		t.Fatalf("seed ollama row: %v", err)
	}

	// Ollama client pointed at a dead port → ListModels errors → adapter reports unreachable.
	deadClient := ollama.New("http://127.0.0.1:1", "") // port 1 is not listening
	ollamaInv := NewOllamaInvAdapter(deadClient)

	scanner := inventory.New([]string{root}, inventory.WithOllama(ollamaInv))
	repo := inventory.NewRepository(d)

	svc := New(Deps{
		Cfg:       config.Default(),
		DB:        d,
		Inventory: scanner,
		InvRepo:   repo,
		OllamaInv: ollamaInv,
	})

	res, err := svc.RescanModels(context.Background())
	if err != nil {
		t.Fatalf("RescanModels must not fail when Ollama is down: %v", err)
	}
	if res.OllamaScanned {
		t.Error("OllamaScanned = true, want false (daemon was down)")
	}
	if res.Inserted != 1 {
		t.Errorf("inserted = %d, want 1 (the disk GGUF)", res.Inserted)
	}
	if len(res.ScanErrors) == 0 {
		t.Error("expected a scan error/note recording the Ollama outage")
	}

	// The pre-existing Ollama row must still be present (NOT marked missing), because the
	// ollama source was excluded from reconciliation.
	var presence string
	if err := d.QueryRow(`SELECT presence FROM models WHERE id='ollama-seed'`).Scan(&presence); err != nil {
		t.Fatalf("read ollama-seed: %v", err)
	}
	if presence != "present" {
		t.Errorf("ollama-seed presence = %q, want present (unreachable source must not be reconciled)", presence)
	}
}

// TestEnginesSweepReadOnlyDisabled confirms an all-nil registry yields three not-installed
// entries each with a reason and makes no external calls.
func TestEnginesSweepReadOnlyDisabled(t *testing.T) {
	svc := New(Deps{Cfg: config.Default(), DB: coreTestDB(t)})
	views := svc.Engines(context.Background())
	if len(views) != 3 {
		t.Fatalf("engines = %d, want 3", len(views))
	}
	for _, v := range views {
		if v.Installed || v.Running {
			t.Errorf("engine %q installed/running with nil registry", v.Name)
		}
		if v.Reason == "" {
			t.Errorf("engine %q missing reason", v.Name)
		}
	}
}
