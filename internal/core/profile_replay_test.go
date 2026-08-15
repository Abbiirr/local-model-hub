package core

import (
	"context"
	"errors"
	"testing"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/compat/gguf"
	"github.com/localhub/localhub/internal/residency"
)

// M2.4 / M2.GATE gate tests: a QUEUED start must replay with the profile it was admitted with.
//
// THE DEFECT THESE PIN (observed live 2026-07-25, KNOWN-LIMITATIONS #21): the queue persisted only
// ctx/batch/pin, so a start queued with kv_quant=true replayed with an f16 KV cache. For the
// Qwen3.6-27B MTP model that prices at ~15602 MiB against ~14168 MiB usable — the entry sat WAITING
// forever, unable to fit a request that WOULD have fit at q4_0 (~14107 MiB). The queue silently
// rewrote the user's request into an impossible one, exactly the class of dishonesty CLAUDE.md
// forbids.

// drainOf simulates the durable round-trip: a request's ProfileSpec is persisted with the queue row
// and read back into a DrainRequest. The SQL persistence itself is covered by the residency store
// tests; this pins the core-side TRANSLATION, which is where the fields were being dropped.
func drainOf(spec residency.ProfileSpec, ctx int) residency.DrainRequest {
	return residency.DrainRequest{ModelID: "m1", Engine: "llamacpp", Ctx: ctx, Profile: spec}
}

// TestQueueReplay_LegacyKVQuantSurvives is the regression test for the live bug: a legacy
// kv_quant=true start must come back as q4_0/q4_0, NOT f16.
func TestQueueReplay_LegacyKVQuantSurvives(t *testing.T) {
	orig := StartInstanceRequest{ModelID: "m1", Engine: "llamacpp", Ctx: 8192, KVQuant: true}

	spec := profileSpecFromRequest(orig)
	if spec.KVTypeK != "q4_0" || spec.KVTypeV != "q4_0" {
		t.Fatalf("kv_quant=true must persist as the pair it MEANS; got %q/%q, want q4_0/q4_0",
			spec.KVTypeK, spec.KVTypeV)
	}

	replayed := startRequestFromDrain(drainOf(spec, orig.Ctx))
	p := startProfile(replayed, replayed.Ctx)
	if p.EffectiveKVTypeK() != gguf.KVCacheQ4_0 || p.EffectiveKVTypeV() != gguf.KVCacheQ4_0 {
		t.Fatalf("REGRESSION (KNOWN-LIMITATIONS #21): replayed profile KV = %s/%s, want q4_0/q4_0. "+
			"An f16 replay prices the 27B model ~1.5 GiB higher and can never be admitted",
			p.EffectiveKVTypeK(), p.EffectiveKVTypeV())
	}
	if !p.KVQuantized() {
		t.Error("replayed profile must still report a quantized KV cache")
	}
}

// TestQueueReplay_FullProfileRoundTrip covers EVERY behaviour-affecting field, including the
// asymmetric q8_0/q4_0 MTP profile that no API caller could even express before M2.1.
func TestQueueReplay_FullProfileRoundTrip(t *testing.T) {
	orig := StartInstanceRequest{
		ModelID:         "qwen3.6-mtp",
		Engine:          "llamacpp",
		Ctx:             8192,
		Batch:           1,
		Priority:        1,
		Pin:             true,
		KVTypeK:         "q8_0",
		KVTypeV:         "q4_0",
		Slots:           1,
		Speculative:     compat.SpeculativeDraftMTP,
		SpecDraftMax:    2,
		AllowCPUOffload: true,
	}

	replayed := startRequestFromDrain(residency.DrainRequest{
		ModelID:  orig.ModelID,
		Engine:   orig.Engine,
		Ctx:      orig.Ctx,
		Batch:    orig.Batch,
		Priority: orig.Priority,
		Pin:      orig.Pin,
		Profile:  profileSpecFromRequest(orig),
	})

	// Compare the NORMALIZED launch profiles: that is the object the verdict and the launch plan
	// both consume, so equality here is the real "identical normalized plan" guarantee (M2.GATE).
	want := startProfile(orig, orig.Ctx).Normalize()
	got := startProfile(replayed, replayed.Ctx).Normalize()
	if want != got {
		t.Fatalf("profile changed across queue replay:\n before: %+v\n  after: %+v", want, got)
	}

	// And the fields the queue row carries outside the profile.
	if replayed.Ctx != orig.Ctx || replayed.Pin != orig.Pin || replayed.Priority != orig.Priority {
		t.Errorf("start context changed across replay: ctx %d→%d, pin %v→%v, priority %d→%d",
			orig.Ctx, replayed.Ctx, orig.Pin, replayed.Pin, orig.Priority, replayed.Priority)
	}
}

// TestQueueReplay_EmbeddingSurvives is the regression test for the live bug reproduced 2026-08-06:
// a start requested with {"model_id":"...bge-m3...","embedding":true} was queued (ram_blocked) and,
// when later admitted, launched WITHOUT --embedding (a plain generation start). bge-m3 is
// encoder-only, so llama-server crashed instantly and flapped in a retry loop
// (KNOWN-LIMITATIONS #21's mode-field twin). This pins that a queued embedding:true start replays
// with Embedding still true — which compat's ggufRunFlags (pinned by TestGGUFRunFlags_Embedding)
// turns into the literal --embedding flag.
func TestQueueReplay_EmbeddingSurvives(t *testing.T) {
	orig := StartInstanceRequest{
		ModelID: "gguf-gpustack-bge-m3-gguf-bge-m3-fp16", Engine: "llamacpp", Ctx: 8192,
		Embedding: true, Pooling: compat.PoolingMean,
	}

	spec := profileSpecFromRequest(orig)
	if !spec.Embedding {
		t.Fatal("profileSpecFromRequest dropped Embedding — the queue row would persist a plain generation start")
	}
	if spec.Pooling != compat.PoolingMean {
		t.Errorf("profileSpecFromRequest dropped Pooling: got %q, want %q", spec.Pooling, compat.PoolingMean)
	}

	replayed := startRequestFromDrain(drainOf(spec, orig.Ctx))
	if !replayed.Embedding {
		t.Fatal("REGRESSION: replayed start lost embedding:true and would spawn a plain generation " +
			"llama-server against an encoder-only model (the bge-m3 crash-loop defect)")
	}
	got := startProfile(replayed, replayed.Ctx).Normalize()
	if !got.Embedding {
		t.Errorf("normalized replayed profile has Embedding=false, want true (would omit --embedding)")
	}
	if got.Pooling != compat.PoolingMean {
		t.Errorf("normalized replayed profile Pooling = %q, want %q (would omit/mismatch --pooling)", got.Pooling, compat.PoolingMean)
	}
}

// TestQueueReplay_RerankingSurvives mirrors TestQueueReplay_EmbeddingSurvives for reranking:true —
// llama.cpp's rerank endpoints require the process be started with --reranking (plus --embedding
// --pooling rank, which compat.LaunchProfile.Normalize couples automatically).
func TestQueueReplay_RerankingSurvives(t *testing.T) {
	orig := StartInstanceRequest{ModelID: "bge-reranker-v2-m3", Engine: "llamacpp", Ctx: 8192, Reranking: true}

	spec := profileSpecFromRequest(orig)
	if !spec.Reranking {
		t.Fatal("profileSpecFromRequest dropped Reranking — the queue row would persist a plain generation start")
	}

	replayed := startRequestFromDrain(drainOf(spec, orig.Ctx))
	if !replayed.Reranking {
		t.Fatal("REGRESSION: replayed start lost reranking:true and would spawn a plain generation " +
			"llama-server instead of one serving /v1/rerank")
	}
	got := startProfile(replayed, replayed.Ctx).Normalize()
	if !got.Reranking {
		t.Errorf("normalized replayed profile has Reranking=false, want true (would omit --reranking)")
	}
	if !got.Embedding || got.Pooling != compat.PoolingRank {
		t.Errorf("Normalize() must couple Reranking with Embedding+Pooling=rank on the replayed profile; got %+v", got)
	}
}

// TestVerifyReplayedLaunchMode is the SAFETY-GUARD unit test (migration 0013): if a replayed
// start's ACTUAL spawned flags (the authoritative record) disagree with the mode the restored
// request asked for, the drain loop must FAIL loudly rather than accept a silently-wrong-mode
// instance as "started". This is deliberately independent of the round-trip fix above — it must
// catch ANY future way the mode fails to survive replay, not just the one just fixed.
func TestVerifyReplayedLaunchMode(t *testing.T) {
	cases := []struct {
		name    string
		sreq    StartInstanceRequest
		flags   []string
		wantErr bool
	}{
		{"generation replayed as generation: ok", StartInstanceRequest{Engine: "llamacpp"}, nil, false},
		{"embedding replayed with --embedding: ok",
			StartInstanceRequest{Engine: "llamacpp", Embedding: true}, []string{"-ngl", "all", "--embedding"}, false},
		{"reranking replayed with --reranking: ok",
			StartInstanceRequest{Engine: "llamacpp", Reranking: true},
			[]string{"-ngl", "all", "--embedding", "--pooling", "rank", "--reranking"}, false},
		{"THE BUG: embedding requested but replayed flags lack --embedding",
			StartInstanceRequest{Engine: "llamacpp", Embedding: true}, []string{"-ngl", "all", "-c", "8192"}, true},
		{"reranking requested but replayed as plain embedding",
			StartInstanceRequest{Engine: "llamacpp", Reranking: true}, []string{"-ngl", "all", "--embedding"}, true},
		{"non-llamacpp engine is exempt (Embedding is a llama.cpp-only field)",
			StartInstanceRequest{Engine: "ollama", Embedding: true}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyReplayedLaunchMode("q1", "m1", tc.sreq, InstanceView{LaunchFlags: tc.flags})
			if tc.wantErr && err == nil {
				t.Fatal("expected the safety guard to refuse this mode mismatch, got nil error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tc.wantErr && len(err.Error()) < 20 {
				t.Errorf("refusal reason too terse to be actionable: %q", err.Error())
			}
		})
	}
}

func TestWrongModeReplayIsStopped(t *testing.T) {
	fr := &fakeRunner{}
	starter := residencyStarter{s: &Service{deps: Deps{LlamaRunner: fr}}}
	mismatch := errors.New("replayed in generation mode but embedding was requested")

	err := starter.rejectWrongModeReplay(context.Background(), InstanceView{ID: "wrong-mode-1"}, mismatch)
	if !errors.Is(err, mismatch) {
		t.Fatalf("cleanup must preserve the original mismatch: %v", err)
	}
	if fr.stopped != "wrong-mode-1" {
		t.Fatalf("wrong-mode instance was left resident; stopped=%q", fr.stopped)
	}
}

// TestQueueReplay_NoFieldSilentlyDropped guards against the NEXT instance of this bug: every
// non-zero profile field must survive. If a field is added to LaunchProfile/ProfileSpec but not to
// the translations, one of these sub-cases fails.
func TestQueueReplay_NoFieldSilentlyDropped(t *testing.T) {
	cases := []struct {
		name string
		req  StartInstanceRequest
	}{
		{"kv K only (genuinely asymmetric)", StartInstanceRequest{KVTypeK: "q8_0"}},
		{"kv V only (genuinely asymmetric)", StartInstanceRequest{KVTypeV: "q4_0"}},
		{"asymmetric pair", StartInstanceRequest{KVTypeK: "q8_0", KVTypeV: "q4_0"}},
		{"legacy kv_quant", StartInstanceRequest{KVQuant: true}},
		{"slots", StartInstanceRequest{Slots: 4}},
		{"ngram-cache", StartInstanceRequest{Speculative: compat.SpeculativeNgramCache}},
		{"draft-mtp + draft max", StartInstanceRequest{Speculative: compat.SpeculativeDraftMTP, SpecDraftMax: 2}},
		{"cpu offload", StartInstanceRequest{AllowCPUOffload: true}},
		{"embedding", StartInstanceRequest{Embedding: true}},
		{"embedding + pooling", StartInstanceRequest{Embedding: true, Pooling: compat.PoolingMean}},
		{"reranking", StartInstanceRequest{Reranking: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.ModelID, tc.req.Engine, tc.req.Ctx = "m1", "llamacpp", 8192
			spec := profileSpecFromRequest(tc.req)
			if spec.IsZero() {
				t.Fatalf("%s produced a ZERO ProfileSpec — the field is not being persisted at all", tc.name)
			}
			want := startProfile(tc.req, tc.req.Ctx).Normalize()
			got := startProfile(startRequestFromDrain(drainOf(spec, tc.req.Ctx)), tc.req.Ctx).Normalize()
			if want != got {
				t.Errorf("%s did not survive replay:\n before: %+v\n  after: %+v", tc.name, want, got)
			}
		})
	}
}

// TestStartProfile_KVPrecedence pins the documented back-compat contract: an explicit (K,V) beats
// the legacy bool, and setting ONE side leaves the other at f16 (a real, intentional asymmetric
// request, not a bug).
func TestStartProfile_KVPrecedence(t *testing.T) {
	// Explicit pair wins over kv_quant.
	p := startProfile(StartInstanceRequest{KVQuant: true, KVTypeK: "q8_0", KVTypeV: "q8_0"}, 8192)
	if p.EffectiveKVTypeK() != gguf.KVCacheQ8_0 || p.EffectiveKVTypeV() != gguf.KVCacheQ8_0 {
		t.Errorf("explicit kv types must win over kv_quant; got %s/%s", p.EffectiveKVTypeK(), p.EffectiveKVTypeV())
	}
	// One side only => the other stays f16.
	p = startProfile(StartInstanceRequest{KVTypeK: "q8_0"}, 8192)
	if p.EffectiveKVTypeK() != gguf.KVCacheQ8_0 || p.EffectiveKVTypeV() != gguf.KVCacheF16 {
		t.Errorf("kv_type_k alone must leave V at f16; got %s/%s", p.EffectiveKVTypeK(), p.EffectiveKVTypeV())
	}
	// Neither => f16/f16, and kv_quant=false stays off.
	p = startProfile(StartInstanceRequest{}, 8192)
	if p.KVQuantized() {
		t.Error("a request with no KV fields must not be quantized")
	}
}

// TestValidateStartProfile_Refusals: every invalid profile value is refused BEFORE anything spawns
// or queues, with a reason naming the valid set (CLAUDE.md).
func TestValidateStartProfile_Refusals(t *testing.T) {
	cases := []struct {
		name string
		req  StartInstanceRequest
	}{
		{"bad kv_type_k", StartInstanceRequest{KVTypeK: "tq3_0"}},
		{"bad kv_type_v", StartInstanceRequest{KVTypeV: "q5_1"}},
		{"slots too high", StartInstanceRequest{Slots: compat.MaxSlots + 1}},
		{"negative slots", StartInstanceRequest{Slots: -1}},
		{"draft max too high", StartInstanceRequest{Speculative: compat.SpeculativeDraftMTP, SpecDraftMax: compat.MaxSpecDraftMax + 1}},
		{"draft max without a spec mode", StartInstanceRequest{SpecDraftMax: 2}},
		{"unknown speculative", StartInstanceRequest{Speculative: "draft-eagle3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateStartProfile(tc.req); err == nil {
				t.Fatalf("%s must be refused", tc.name)
			} else if len(err.Error()) < 20 {
				t.Errorf("refusal reason too terse to be actionable: %q", err.Error())
			}
		})
	}
	// The validated Qwen3.6 MTP profile must PASS.
	ok := StartInstanceRequest{
		KVTypeK: "q8_0", KVTypeV: "q4_0", Slots: 1,
		Speculative: compat.SpeculativeDraftMTP, SpecDraftMax: 2,
	}
	if _, err := validateStartProfile(ok); err != nil {
		t.Fatalf("the validated Qwen3.6-27B MTP profile must be accepted; got %v", err)
	}
}
