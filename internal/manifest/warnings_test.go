package manifest

import (
	"strings"
	"testing"
)

func TestScan_PickleNoSafetensors(t *testing.T) {
	got := Scan(ScanInput{
		Files: []File{
			{Path: "pytorch_model.bin"},
			{Path: "config.json"},
		},
		License: "apache-2.0",
	})
	if !HasPickleWarning(got) {
		t.Fatalf("expected pickle warning for a .bin-only repo, got %v", got)
	}
}

func TestScan_PickleWithSafetensorsSuppressed(t *testing.T) {
	// A repo that has BOTH a .bin and a .safetensors gets NO pickle warning (a safe
	// alternative exists — the user can load the safetensors).
	got := Scan(ScanInput{
		Files: []File{
			{Path: "pytorch_model.bin"},
			{Path: "model.safetensors"},
			{Path: "config.json"},
		},
		License: "mit",
	})
	if HasPickleWarning(got) {
		t.Fatalf("did not expect pickle warning when safetensors present, got %v", got)
	}
}

func TestScan_TrustRemoteCode_AutoMap(t *testing.T) {
	cfg := []byte(`{"model_type":"custom","auto_map":{"AutoModel":"modeling_custom.CustomModel"}}`)
	got := Scan(ScanInput{
		Files:      []File{{Path: "model.safetensors"}},
		ConfigJSON: cfg,
		License:    "apache-2.0",
	})
	if !containsWarn(got, WarnTrustRemoteCode) {
		t.Fatalf("expected trust_remote_code warning from auto_map, got %v", got)
	}
}

func TestScan_TrustRemoteCode_PyFile(t *testing.T) {
	got := Scan(ScanInput{
		Files: []File{
			{Path: "model.safetensors"},
			{Path: "modeling_custom.py"},
		},
		License: "apache-2.0",
	})
	if !containsWarn(got, WarnTrustRemoteCode) {
		t.Fatalf("expected trust_remote_code warning from modeling_*.py, got %v", got)
	}
}

func TestScan_NoLicense(t *testing.T) {
	got := Scan(ScanInput{Files: []File{{Path: "model.safetensors"}}})
	if !containsWarn(got, WarnNoLicense) {
		t.Fatalf("expected no-license warning, got %v", got)
	}
}

func TestScan_Gated(t *testing.T) {
	got := Scan(ScanInput{Files: []File{{Path: "model.safetensors"}}, License: "llama3", Gated: true})
	if !containsWarn(got, WarnGatedRepo) {
		t.Fatalf("expected gated warning, got %v", got)
	}
}

func TestScan_LocalImportOrigin(t *testing.T) {
	got := Scan(ScanInput{Files: []File{{Path: "m.gguf"}}, License: "apache-2.0", LocalImport: true})
	if !containsWarn(got, WarnLocalImportOrigin) {
		t.Fatalf("expected local-import origin warning, got %v", got)
	}
}

func TestScan_CleanRepoNoWarnings(t *testing.T) {
	got := Scan(ScanInput{
		Files:   []File{{Path: "model.safetensors"}, {Path: "config.json"}},
		License: "apache-2.0",
	})
	if len(got) != 0 {
		t.Fatalf("expected no warnings for a clean safetensors+license repo, got %v", got)
	}
}

// TestScan_OrderStable ensures the fixed emit order (pickle, trust_remote_code, license,
// gated, local-import) so UI/tests are deterministic.
func TestScan_OrderStable(t *testing.T) {
	got := Scan(ScanInput{
		Files:       []File{{Path: "pytorch_model.bin"}, {Path: "modeling_x.py"}},
		Gated:       true,
		LocalImport: true,
	})
	want := []string{WarnPickle, WarnTrustRemoteCode, WarnNoLicense, WarnGatedRepo, WarnLocalImportOrigin}
	if len(got) != len(want) {
		t.Fatalf("want %d warnings, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("warning %d: want %q, got %q", i, want[i], got[i])
		}
	}
}

func containsWarn(ws []string, want string) bool {
	for _, w := range ws {
		if strings.Contains(w, strings.SplitN(want, " —", 2)[0]) || w == want {
			return true
		}
	}
	return false
}
