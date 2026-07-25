package core

import (
	"context"
	"testing"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/compat/hfmeta"
	"github.com/localhub/localhub/internal/hardware"
)

// fakeCompat records the descriptor it was asked to evaluate and returns a canned verdict.
type fakeCompat struct {
	last    compat.ModelDescriptor
	verdict compat.Verdict
}

func (f *fakeCompat) Evaluate(_ context.Context, req compat.Request, _ hardware.Snapshot, _ compat.EngineAvailability) compat.Verdict {
	f.last = req.Descriptor
	v := f.verdict
	v.Model = req.Descriptor.DisplayName
	return v
}

type fakeHFMeta struct {
	res hfmeta.Result
	err error
}

func (f *fakeHFMeta) Fetch(context.Context, string, string) (hfmeta.Result, error) {
	return f.res, f.err
}

// TestCompatPreview_InventoryModel_PersistsVerdict: a preview for an inventory model returns
// the verdict AND writes compat_status/recommended_engine onto the models row.
func TestCompatPreview_InventoryModel_PersistsVerdict(t *testing.T) {
	d := coreTestDB(t)
	// Seed a GGUF model row.
	_, err := d.Exec(`INSERT INTO models (id, display_name, source, format, quantization, local_path)
		VALUES ('m1','Model One','local_path','gguf','q4_k_m','K:\models\m1.gguf')`)
	if err != nil {
		t.Fatal(err)
	}

	fc := &fakeCompat{verdict: compat.Verdict{Status: compat.StatusRuns, Reason: "fits with headroom", RecommendedEngine: "llamacpp"}}
	svc := New(Deps{
		DB:       d,
		Hardware: &hardware.FakeProbe{},
		Engines:  EnginesRegistry{}, // all nil → disabled engine views (no llama binary)
		Compat:   fc,
	})

	v, err := svc.CompatPreview(context.Background(), CompatRequest{ModelID: "m1"})
	if err != nil {
		t.Fatalf("CompatPreview: %v", err)
	}
	if v.Status != compat.StatusRuns {
		t.Errorf("status = %q, want runs", v.Status)
	}
	// The descriptor built from the row must carry the GGUF format + local path.
	if fc.last.Format != compat.FormatGGUF || fc.last.LocalPath == "" {
		t.Errorf("descriptor from inventory wrong: %+v", fc.last)
	}
	// Verdict must be persisted onto the row.
	var status, engine string
	if err := d.QueryRow(`SELECT COALESCE(compat_status,''), COALESCE(recommended_engine,'') FROM models WHERE id='m1'`).
		Scan(&status, &engine); err != nil {
		t.Fatal(err)
	}
	if status != "runs" || engine != "llamacpp" {
		t.Errorf("persisted compat_status=%q recommended_engine=%q, want runs/llamacpp", status, engine)
	}
}

// TestCompatPreview_RemoteGGUF builds a remote GGUF descriptor (hf_repo+file) without touching
// the DB.
func TestCompatPreview_RemoteGGUF(t *testing.T) {
	fc := &fakeCompat{verdict: compat.Verdict{Status: compat.StatusRuns, Reason: "ok"}}
	svc := New(Deps{Hardware: &hardware.FakeProbe{}, Compat: fc})
	_, err := svc.CompatPreview(context.Background(), CompatRequest{
		HFRepo: "bartowski/Llama-3.2-1B-Instruct-GGUF", File: "Llama-3.2-1B-Instruct-Q4_K_M.gguf",
	})
	if err != nil {
		t.Fatalf("CompatPreview remote GGUF: %v", err)
	}
	if fc.last.Format != compat.FormatGGUF {
		t.Errorf("format = %q, want gguf", fc.last.Format)
	}
	if fc.last.Quantization != "q4_k_m" {
		t.Errorf("quant from filename = %q, want q4_k_m", fc.last.Quantization)
	}
	if fc.last.Repo != "bartowski/Llama-3.2-1B-Instruct-GGUF" {
		t.Errorf("repo not carried: %+v", fc.last)
	}
}

// TestCompatPreview_RemoteSafetensors fetches config/index via the fake HFMeta and builds a
// safetensors descriptor.
func TestCompatPreview_RemoteSafetensors(t *testing.T) {
	fc := &fakeCompat{verdict: compat.Verdict{Status: compat.StatusWontRun, Reason: "too big"}}
	hf := &fakeHFMeta{res: hfmeta.Result{
		ConfigJSON: []byte(`{"num_hidden_layers":32,"hidden_size":4096,"num_attention_heads":32,"num_key_value_heads":8,"torch_dtype":"bfloat16","max_position_embeddings":131072}`),
		IndexJSON:  []byte(`{"metadata":{"total_size":16060000000}}`),
	}}
	svc := New(Deps{Hardware: &hardware.FakeProbe{}, Compat: fc, HFMeta: hf})
	v, err := svc.CompatPreview(context.Background(), CompatRequest{HFRepo: "NousResearch/Meta-Llama-3.1-8B-Instruct"})
	if err != nil {
		t.Fatalf("CompatPreview remote safetensors: %v", err)
	}
	if fc.last.Format != compat.FormatSafetensors {
		t.Errorf("format = %q, want safetensors", fc.last.Format)
	}
	if !fc.last.STConfigResolved || fc.last.STConfig.NumHiddenLayers != 32 {
		t.Errorf("safetensors config not resolved: %+v", fc.last)
	}
	if fc.last.STWeights.TotalBytes == 0 {
		t.Error("index total_size should populate STWeights")
	}
	if v.Status != compat.StatusWontRun {
		t.Errorf("status = %q, want wont_run", v.Status)
	}
}

// TestCompatPreview_NoSelector errors clearly.
func TestCompatPreview_NoSelector(t *testing.T) {
	svc := New(Deps{Hardware: &hardware.FakeProbe{}, Compat: &fakeCompat{}})
	_, err := svc.CompatPreview(context.Background(), CompatRequest{})
	if err == nil {
		t.Fatal("empty selector must error")
	}
}

// TestCompatPreview_ modelNotFound.
func TestCompatPreview_ModelNotFound(t *testing.T) {
	d := coreTestDB(t)
	svc := New(Deps{DB: d, Hardware: &hardware.FakeProbe{}, Compat: &fakeCompat{}})
	_, err := svc.CompatPreview(context.Background(), CompatRequest{ModelID: "nope"})
	if err == nil {
		t.Fatal("unknown model id must error")
	}
}
