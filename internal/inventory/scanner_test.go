package inventory

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeGGUF writes a minimal but header-valid GGUF file (magic "GGUF" + version 3 + some
// padding so it is non-empty). This is enough for the scanner's cheap sniff; it is NOT a
// parseable model (that's not what P1.4 does).
func writeGGUF(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf [16]byte
	copy(buf[0:4], []byte("GGUF"))
	binary.LittleEndian.PutUint32(buf[4:8], 3) // version 3
	if err := os.WriteFile(path, buf[:], 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeFile writes arbitrary content, creating parent dirs.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildFixtureTree lays out a representative model directory mirroring the reference box:
//   - a plain GGUF with a recognizable quant
//   - a partial-download litter file (.tmp) and a .part.aria2 litter file
//   - a multi-part GGUF set (2 shards)
//   - a base GGUF with an mmproj companion in the same dir
//   - a subdir that is an orphan mmproj (mmproj only, no base)
//   - a safetensors HF dir (config.json + a .safetensors)
//   - a pytorch/pickle HF dir (config.json + a .bin, no safetensors)
//   - an empty dir and a .cache dir that must be ignored
func buildFixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// Plain GGUF.
	writeGGUF(t, filepath.Join(root, "solo", "Model-Q4_K_M.gguf"))

	// Partial-download litter (must be excluded + logged).
	writeFile(t, filepath.Join(root, "solo", "Big-TQ3_4S.gguf.tmp"), "partial")
	writeFile(t, filepath.Join(root, "solo", "Big-TQ3_4S.gguf.part.aria2"), "aria2 control")

	// Multi-part GGUF set (one logical model, two shards).
	writeGGUF(t, filepath.Join(root, "split", "Big-Q8_0-00001-of-00002.gguf"))
	writeGGUF(t, filepath.Join(root, "split", "Big-Q8_0-00002-of-00002.gguf"))

	// Base GGUF + mmproj companion in the same directory.
	writeGGUF(t, filepath.Join(root, "vision", "VLM-Q6_K.gguf"))
	writeGGUF(t, filepath.Join(root, "vision", "mmproj-F16.gguf"))

	// Orphan mmproj (no base in its directory).
	writeGGUF(t, filepath.Join(root, "orphan", "mmproj.gguf"))

	// Safetensors HF model root.
	writeFile(t, filepath.Join(root, "hf-safetensors", "config.json"),
		`{"architectures":["LlamaForCausalLM"],"model_type":"llama","torch_dtype":"bfloat16"}`)
	writeFile(t, filepath.Join(root, "hf-safetensors", "model.safetensors"), "weightsweights")
	writeFile(t, filepath.Join(root, "hf-safetensors", "LICENSE"), "Apache-2.0")

	// Pickle/pytorch HF model root (config.json + .bin only).
	writeFile(t, filepath.Join(root, "hf-pickle", "config.json"),
		`{"architectures":["GPT2LMHeadModel"],"model_type":"gpt2"}`)
	writeFile(t, filepath.Join(root, "hf-pickle", "pytorch_model.bin"), "pickledata")

	// Empty dir and cache dir that must not produce records.
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".cache", "blob"), "cache junk")

	return root
}

func modelByID(res ScanResult, id string) (Model, bool) {
	for _, m := range res.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

func skippedContains(res ScanResult, substr string) bool {
	for _, s := range res.Skipped {
		if filepath.Base(s.Path) == substr || contains(s.Path, substr) {
			return true
		}
	}
	return false
}

func contains(hay, needle string) bool {
	return len(needle) > 0 && (hay == needle || (len(hay) >= len(needle) && indexOf(hay, needle) >= 0))
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestScanFixtureTree(t *testing.T) {
	root := buildFixtureTree(t)
	sc := New([]string{root})
	res := sc.Scan(context.Background())

	// Expected standalone models:
	//   solo/Model-Q4_K_M.gguf
	//   split/Big-Q8_0 (one record for the 2 shards)
	//   vision/VLM-Q6_K.gguf (with mmproj companion)
	//   hf-safetensors (safetensors)
	//   hf-pickle (pytorch)
	// = 5 models. The orphan mmproj and both partials are NOT models.
	if len(res.Models) != 5 {
		t.Fatalf("want 5 models, got %d: %+v", len(res.Models), modelIDs(res))
	}

	// Partials excluded and logged.
	if !skippedContains(res, ".tmp") {
		t.Errorf("expected .tmp partial to be skipped+logged; skipped=%+v", res.Skipped)
	}
	if !skippedContains(res, ".aria2") {
		t.Errorf("expected .aria2 partial to be skipped+logged; skipped=%+v", res.Skipped)
	}

	// Multi-part collapsed to ONE record with Parts populated.
	split, ok := modelByID(res, ggufNatKey(root, filepath.Join(root, "split", "Big-Q8_0-00001-of-00002.gguf")))
	if !ok {
		t.Fatalf("multi-part model not found; ids=%v", modelIDs(res))
	}
	if len(split.Parts) != 2 {
		t.Errorf("multi-part model should have 2 parts, got %d: %v", len(split.Parts), split.Parts)
	}
	if split.Quantization != "q8_0" {
		t.Errorf("multi-part quant = %q, want q8_0", split.Quantization)
	}

	// Vision model has the mmproj companion linked, and the mmproj is NOT a standalone row.
	vlm, ok := modelByID(res, ggufNatKey(root, filepath.Join(root, "vision", "VLM-Q6_K.gguf")))
	if !ok {
		t.Fatalf("vision model not found; ids=%v", modelIDs(res))
	}
	if len(vlm.Companions) != 1 || vlm.Companions[0].Kind != "mmproj" {
		t.Errorf("vision model should link 1 mmproj companion, got %+v", vlm.Companions)
	}
	for _, m := range res.Models {
		if isMMProj(filepath.Base(m.LocalPath)) {
			t.Errorf("mmproj file %q became a standalone model row (should be a companion)", m.LocalPath)
		}
	}

	// Orphan mmproj skipped with a reason.
	if !skippedContains(res, "mmproj.gguf") {
		t.Errorf("orphan mmproj should be skipped+logged; skipped=%+v", res.Skipped)
	}

	// Safetensors model.
	st, ok := modelByID(res, hfNatKey(root, filepath.Join(root, "hf-safetensors")))
	if !ok {
		t.Fatalf("safetensors model not found; ids=%v", modelIDs(res))
	}
	if st.Format != FormatSafetensors {
		t.Errorf("hf-safetensors format = %q, want safetensors", st.Format)
	}
	if st.ModelType != "llama" || len(st.Architectures) != 1 || st.Architectures[0] != "LlamaForCausalLM" {
		t.Errorf("safetensors config not captured: type=%q arch=%v", st.ModelType, st.Architectures)
	}
	if st.Quantization != "bf16" {
		t.Errorf("safetensors quant from torch_dtype = %q, want bf16", st.Quantization)
	}
	if st.License != "present" {
		t.Errorf("safetensors license presence = %q, want present", st.License)
	}

	// Pickle model: pytorch format + a pickle warning.
	pk, ok := modelByID(res, hfNatKey(root, filepath.Join(root, "hf-pickle")))
	if !ok {
		t.Fatalf("pickle model not found; ids=%v", modelIDs(res))
	}
	if pk.Format != FormatPyTorch {
		t.Errorf("hf-pickle format = %q, want pytorch", pk.Format)
	}
	if len(pk.Warnings) == 0 {
		t.Errorf("pickle model should carry a pickle warning")
	}
}

func TestScanExcludesEmptyAndCache(t *testing.T) {
	root := buildFixtureTree(t)
	res := New([]string{root}).Scan(context.Background())
	for _, m := range res.Models {
		if contains(m.LocalPath, string(filepath.Separator)+".cache"+string(filepath.Separator)) ||
			filepath.Base(m.LocalPath) == "blob" {
			t.Errorf("cache content became a model: %q", m.LocalPath)
		}
	}
}

func TestScanCancelledContext(t *testing.T) {
	root := buildFixtureTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	res := New([]string{root}).Scan(ctx)
	if len(res.Errors) == 0 {
		t.Errorf("cancelled scan should record a context error; errors=%v", res.Errors)
	}
}

func TestSniffGGUFRejectsBadMagic(t *testing.T) {
	root := t.TempDir()
	// A .gguf file that is not actually GGUF (wrong magic) must be skipped, not inventoried.
	writeFile(t, filepath.Join(root, "fake", "notreally.gguf"), "this is not a gguf header at all")
	res := New([]string{root}).Scan(context.Background())
	if len(res.Models) != 0 {
		t.Errorf("bad-magic .gguf should not become a model; got %+v", modelIDs(res))
	}
	if !skippedContains(res, "notreally.gguf") {
		t.Errorf("bad-magic file should be skipped+logged; skipped=%+v", res.Skipped)
	}
}

func modelIDs(res ScanResult) []string {
	out := make([]string, 0, len(res.Models))
	for _, m := range res.Models {
		out = append(out, m.ID)
	}
	return out
}
