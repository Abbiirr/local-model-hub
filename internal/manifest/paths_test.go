package manifest

import (
	"path/filepath"
	"testing"
)

func TestValidateWithin_InsideRoot(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "sub", "model.gguf")
	if err := ValidateWithin(target, []string{root}); err != nil {
		t.Fatalf("expected target inside root to pass, got %v", err)
	}
}

func TestValidateWithin_Traversal(t *testing.T) {
	root := t.TempDir()
	// A path that resolves outside root via ..
	target := filepath.Join(root, "..", "escape.gguf")
	if err := ValidateWithin(target, []string{root}); err == nil {
		t.Fatalf("expected traversal outside root to be refused")
	}
}

func TestValidateWithin_SiblingPrefixNotContained(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "models")
	sibling := filepath.Join(base, "models-evil", "x.gguf")
	// "models-evil" must NOT be considered inside "models" (separator boundary).
	if err := ValidateWithin(sibling, []string{root}); err == nil {
		t.Fatalf("expected models-evil to be refused as not inside models")
	}
}

func TestValidateWithin_NoRoots(t *testing.T) {
	if err := ValidateWithin("/anything", nil); err == nil {
		t.Fatalf("expected refusal when no roots configured")
	}
}

func TestSafeJoin_OK(t *testing.T) {
	root := t.TempDir()
	got, err := SafeJoin(root, "owner__repo/config.json")
	if err != nil {
		t.Fatalf("SafeJoin failed: %v", err)
	}
	want := filepath.Join(root, "owner__repo", "config.json")
	if got != want {
		t.Fatalf("SafeJoin = %q, want %q", got, want)
	}
}

func TestSafeJoin_Traversal(t *testing.T) {
	root := t.TempDir()
	if _, err := SafeJoin(root, "../../etc/passwd"); err == nil {
		t.Fatalf("expected SafeJoin to refuse a traversal path")
	}
}

func TestSafeJoin_AbsoluteRel(t *testing.T) {
	root := t.TempDir()
	if _, err := SafeJoin(root, "/absolute/path"); err == nil {
		t.Fatalf("expected SafeJoin to refuse an absolute rel path")
	}
}
