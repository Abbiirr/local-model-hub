package compat

import (
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/compat/gguf"
)

// nglValue returns the token following "-ngl" in flags, or "" if absent.
func nglValue(flags []string) string {
	for i, f := range flags {
		if f == "-ngl" && i+1 < len(flags) {
			return flags[i+1]
		}
	}
	return ""
}

// TestPlanLlamaLaunch_NGLDefaultVsCPUOffload is the core regression for the full-GPU directive:
// the default (allowCPUOffload=false) must emit "-ngl all" (never offload to CPU); opting in
// (allowCPUOffload=true) must emit "-ngl auto" (llama.cpp offloads as many layers as fit, rest on
// CPU). Verified against the local llama-server --help (2026-07-07): -ngl accepts an exact number,
// 'auto', or 'all' (default: auto).
func TestPlanLlamaLaunch_NGLDefaultVsCPUOffload(t *testing.T) {
	d := ModelDescriptor{ID: "m", DisplayName: "M", Format: FormatGGUF, Source: "local_path", GGUFSource: gguf.LocalFile("x.gguf")}
	avail := availWith(winget)

	// Default: full GPU.
	full, err := PlanLlamaLaunch(d, avail, LaunchProfile{Ctx: 8192})
	if err != nil {
		t.Fatalf("PlanLlamaLaunch full-gpu: %v", err)
	}
	if got := nglValue(full.Flags); got != "all" {
		t.Errorf("default -ngl: got %q want \"all\" (full GPU, never CPU)", got)
	}

	// Opt-in: allow CPU offload.
	split, err := PlanLlamaLaunch(d, avail, LaunchProfile{Ctx: 8192, AllowCPUOffload: true})
	if err != nil {
		t.Fatalf("PlanLlamaLaunch cpu-offload: %v", err)
	}
	if got := nglValue(split.Flags); got != "auto" {
		t.Errorf("allow_cpu_offload -ngl: got %q want \"auto\" (GPU/CPU split)", got)
	}
	if !strings.Contains(split.Reason, "CPU offload allowed") {
		t.Errorf("cpu-offload plan reason should note the split; got %q", split.Reason)
	}
}

// TestGGUFRunFlags_NGL asserts the low-level flag builder directly (both branches), independent of
// binary selection.
func TestGGUFRunFlags_NGL(t *testing.T) {
	d := ModelDescriptor{ID: "m", Format: FormatGGUF}
	bin := winget

	full := ggufRunFlags(d, bin, LaunchProfile{Ctx: 4096})
	if got := nglValue(full); got != "all" {
		t.Errorf("ggufRunFlags default -ngl: got %q want \"all\"", got)
	}
	split := ggufRunFlags(d, bin, LaunchProfile{Ctx: 4096, AllowCPUOffload: true})
	if got := nglValue(split); got != "auto" {
		t.Errorf("ggufRunFlags allowCPUOffload -ngl: got %q want \"auto\"", got)
	}
}

// hasFlagPair reports whether flags contains name immediately followed by val.
func hasFlagPair(flags []string, name, val string) bool {
	for i, f := range flags {
		if f == name && i+1 < len(flags) && flags[i+1] == val {
			return true
		}
	}
	return false
}

// TestGGUFRunFlags_Speculative asserts the draft-free self-speculative opt-in: off (default) emits
// no --spec-type; "ngram-cache" appends exactly `--spec-type ngram-cache` (the value verified from
// the local b8833 --help, research §0). It must never change -ngl (adds ~zero VRAM, flags only).
func TestGGUFRunFlags_Speculative(t *testing.T) {
	d := ModelDescriptor{ID: "m", Format: FormatGGUF}
	bin := winget

	off := ggufRunFlags(d, bin, LaunchProfile{Ctx: 4096})
	for i, f := range off {
		if f == "--spec-type" {
			t.Fatalf("speculative off must not emit --spec-type; flags=%v (at %d)", off, i)
		}
	}
	on := ggufRunFlags(d, bin, LaunchProfile{Ctx: 4096, SpecMode: SpeculativeNgramCache})
	if !hasFlagPair(on, "--spec-type", "ngram-cache") {
		t.Errorf("speculative ngram-cache must emit `--spec-type ngram-cache`; flags=%v", on)
	}
	if got := nglValue(on); got != "all" {
		t.Errorf("speculative must not change -ngl; got %q want \"all\"", got)
	}
}

// TestPlanLlamaLaunch_SpeculativeReason confirms the launch plan threads the speculative value into
// its emitted flags and notes it in the human reason (CLAUDE.md: verdicts carry a reason).
func TestPlanLlamaLaunch_SpeculativeReason(t *testing.T) {
	d := ModelDescriptor{ID: "m", DisplayName: "M", Format: FormatGGUF, Source: "local_path", GGUFSource: gguf.LocalFile("x.gguf")}
	avail := availWith(winget)

	plan, err := PlanLlamaLaunch(d, avail, LaunchProfile{Ctx: 8192, SpecMode: SpeculativeNgramCache})
	if err != nil {
		t.Fatalf("PlanLlamaLaunch speculative: %v", err)
	}
	if !hasFlagPair(plan.Flags, "--spec-type", "ngram-cache") {
		t.Errorf("plan flags should include `--spec-type ngram-cache`; got %v", plan.Flags)
	}
	if !strings.Contains(plan.Reason, "self-speculative") {
		t.Errorf("plan reason should note self-speculative decoding; got %q", plan.Reason)
	}
}

// TestValidateSpeculative covers the allow-list: "" and "ngram-cache" pass; anything else is
// rejected with a reason naming the valid values.
func TestValidateSpeculative(t *testing.T) {
	for _, ok := range []string{SpeculativeOff, SpeculativeNgramCache} {
		if got, err := ValidateSpeculative(ok); err != nil || got != ok {
			t.Errorf("ValidateSpeculative(%q) = (%q,%v), want (%q,nil)", ok, got, err, ok)
		}
	}
	for _, bad := range []string{"ngram", "ngram-simple", "draft-eagle3", "on", "true"} {
		if _, err := ValidateSpeculative(bad); err == nil {
			t.Errorf("ValidateSpeculative(%q) should be rejected", bad)
		}
	}
}

// flagIndex returns the index of name in flags, or -1 if absent. Used to assert flag ORDER
// (e.g. --embedding must come after -np), not just presence.
func flagIndex(flags []string, name string) int {
	for i, f := range flags {
		if f == name {
			return i
		}
	}
	return -1
}

// TestGGUFRunFlags_Embedding pins the exact --embedding/--pooling emission contract documented in
// ggufRunFlags's doc comment. This matters because a wrong emission here either (a) silently starts
// a NORMAL generation server as byte-different from before (regressing every existing non-embedding
// launch) or (b) starts an embedding server whose /v1/embeddings behavior is wrong because --pooling
// was dropped or misplaced — and the proxy has no way to detect either from the outside.
func TestGGUFRunFlags_Embedding(t *testing.T) {
	d := ModelDescriptor{ID: "m", Format: FormatGGUF}
	bin := winget

	t.Run("off emits neither flag", func(t *testing.T) {
		// Regression guard: Embedding=false must be byte-identical to the pre-feature flag set, i.e.
		// no --embedding and no --pooling leak in from a zero-value field.
		flags := ggufRunFlags(d, bin, LaunchProfile{Ctx: 4096})
		if flagIndex(flags, "--embedding") != -1 {
			t.Errorf("embedding off must not emit --embedding; flags=%v", flags)
		}
		if flagIndex(flags, "--pooling") != -1 {
			t.Errorf("embedding off must not emit --pooling; flags=%v", flags)
		}
	})

	t.Run("on with no pooling omits --pooling", func(t *testing.T) {
		// Pooling="" means "use the model's own default" (tools/server/README.md); the flag must be
		// omitted entirely, not emitted with an empty value.
		flags := ggufRunFlags(d, bin, LaunchProfile{Ctx: 4096, Embedding: true})
		if flagIndex(flags, "--embedding") == -1 {
			t.Errorf("embedding on must emit --embedding; flags=%v", flags)
		}
		if flagIndex(flags, "--pooling") != -1 {
			t.Errorf("embedding on with unset Pooling must not emit --pooling; flags=%v", flags)
		}
	})

	t.Run("on with pooling mean emits adjacent pair", func(t *testing.T) {
		flags := ggufRunFlags(d, bin, LaunchProfile{Ctx: 4096, Embedding: true, Pooling: PoolingMean})
		if !hasFlagPair(flags, "--pooling", "mean") {
			t.Errorf("embedding on with Pooling=mean must emit `--pooling mean` adjacent; flags=%v", flags)
		}
		// --pooling must immediately follow --embedding per the doc-commented emission order.
		ei, pi := flagIndex(flags, "--embedding"), flagIndex(flags, "--pooling")
		if ei == -1 || pi != ei+1 {
			t.Errorf("--pooling must immediately follow --embedding; flags=%v (embedding@%d pooling@%d)", flags, ei, pi)
		}
	})

	t.Run("embedding flag comes after -np", func(t *testing.T) {
		// Pins the documented flag order in ggufRunFlags's doc comment: -np is emitted before the
		// [--embedding [--pooling ...]] block. If a future edit reorders this, replayed queued starts
		// (M2.5) could still be equivalent in effect, but this test exists precisely to make an
		// unintentional reorder visible rather than silently drifting from the documented shape.
		flags := ggufRunFlags(d, bin, LaunchProfile{Ctx: 4096, Embedding: true})
		npIdx, embIdx := flagIndex(flags, "-np"), flagIndex(flags, "--embedding")
		if npIdx == -1 || embIdx == -1 || embIdx < npIdx {
			t.Errorf("--embedding must appear after -np; flags=%v (-np@%d --embedding@%d)", flags, npIdx, embIdx)
		}
	})
}

// TestGGUFRunFlags_EmbeddingSuppressesSpeculative pins the invariant that an embedding-only server
// never receives --spec-type or --spec-draft-n-max, regardless of what SpecMode/SpecDraftMax the
// profile requests: an encoder-only server has no autoregressive decode loop for a draft head or an
// n-gram pool to accelerate, so forwarding those flags would either be rejected by llama.cpp or
// silently ignored — neither of which we want to depend on. The suppression happens twice
// (Normalize zeroes the fields; ggufRunFlags also gates on p.Embedding directly) so this test uses
// profiles that a caller could construct WITHOUT going through Normalize first, to prove the
// ggufRunFlags-level guard (not just Normalize) is what's doing the work.
func TestGGUFRunFlags_EmbeddingSuppressesSpeculative(t *testing.T) {
	d := ModelDescriptor{ID: "m", Format: FormatGGUF}

	t.Run("ngram-cache suppressed under embedding", func(t *testing.T) {
		flags := ggufRunFlags(d, winget, LaunchProfile{Ctx: 4096, Embedding: true, SpecMode: SpeculativeNgramCache})
		if flagIndex(flags, "--spec-type") != -1 {
			t.Errorf("embedding+ngram-cache must not emit --spec-type; flags=%v", flags)
		}
	})

	t.Run("draft-mtp suppressed under embedding even with a capable binary", func(t *testing.T) {
		flags := ggufRunFlags(d, mtpBuild, LaunchProfile{Ctx: 4096, Embedding: true, SpecMode: SpeculativeDraftMTP, SpecDraftMax: 2})
		if flagIndex(flags, "--spec-type") != -1 {
			t.Errorf("embedding+draft-mtp must not emit --spec-type; flags=%v", flags)
		}
		if flagIndex(flags, "--spec-draft-n-max") != -1 {
			t.Errorf("embedding+draft-mtp must not emit --spec-draft-n-max; flags=%v", flags)
		}
	})

	t.Run("same profiles without embedding still emit --spec-type (fixture sanity)", func(t *testing.T) {
		// Proves the suppression above is caused by Embedding, not by a broken/non-speculative
		// fixture: the identical SpecMode values DO emit --spec-type once Embedding is false.
		ngram := ggufRunFlags(d, winget, LaunchProfile{Ctx: 4096, SpecMode: SpeculativeNgramCache})
		if !hasFlagPair(ngram, "--spec-type", "ngram-cache") {
			t.Errorf("non-embedding ngram-cache must emit --spec-type ngram-cache; flags=%v", ngram)
		}
		mtp := ggufRunFlags(d, mtpBuild, LaunchProfile{Ctx: 4096, SpecMode: SpeculativeDraftMTP, SpecDraftMax: 2})
		if !hasFlagPair(mtp, "--spec-type", "draft-mtp") {
			t.Errorf("non-embedding draft-mtp must emit --spec-type draft-mtp; flags=%v", mtp)
		}
	})
}

// TestLaunchProfileNormalize_EmbeddingZeroesSpeculative pins LaunchProfile.Normalize's documented
// contract: Embedding forces SpecMode/SpecDraftMax off, Normalize is idempotent (required for M2.5
// queue-replay byte-identity — replaying an already-normalized profile must not drift), and the
// pre-existing draft-mtp-forces-Slots=1 rule still holds for a NON-embedding profile (proving this
// change didn't clobber the older invariant).
func TestLaunchProfileNormalize_EmbeddingZeroesSpeculative(t *testing.T) {
	p := LaunchProfile{Ctx: 4096, Embedding: true, SpecMode: SpeculativeDraftMTP, SpecDraftMax: 2}

	n := p.Normalize()
	if n.SpecMode != SpeculativeOff {
		t.Errorf("Normalize() on Embedding=true must zero SpecMode; got %q", n.SpecMode)
	}
	if n.SpecDraftMax != 0 {
		t.Errorf("Normalize() on Embedding=true must zero SpecDraftMax; got %d", n.SpecDraftMax)
	}

	// Idempotency: normalizing an already-normalized profile must be a no-op.
	nn := n.Normalize()
	if nn != n {
		t.Errorf("Normalize() must be idempotent; Normalize(p)=%+v Normalize(Normalize(p))=%+v", n, nn)
	}

	// Existing rule unbroken: a non-embedding draft-mtp profile still forces Slots=1.
	nonEmbedding := LaunchProfile{Ctx: 4096, SpecMode: SpeculativeDraftMTP, SpecDraftMax: 2}.Normalize()
	if nonEmbedding.Slots != 1 {
		t.Errorf("non-embedding draft-mtp must still force Slots=1; got %d", nonEmbedding.Slots)
	}
}

// TestGGUFRunFlags_Reranking pins the exact --embedding/--pooling rank/--reranking emission
// contract documented in ggufRunFlags's doc comment and LaunchProfile.Reranking's doc comment.
// Primary source (tools/server/README.md, master, fetched 2026-08-05):
//
//	"--rerank, --reranking : enable reranking endpoint on server (default: disabled)"
//	"Requires a reranker model (such as bge-reranker-v2-m3) and the --embedding --pooling rank
//	 options."
//
// So Reranking=true must emit all three flags TOGETHER, in order, even when the caller set neither
// Embedding nor Pooling — that coupling is Normalize's job (also covered directly by
// TestLaunchProfileNormalize_RerankingImpliesEmbeddingRank below), and ggufRunFlags calls Normalize
// itself, so this test exercises the full path a real caller goes through.
func TestGGUFRunFlags_Reranking(t *testing.T) {
	d := ModelDescriptor{ID: "m", Format: FormatGGUF}
	bin := winget

	t.Run("off emits no --reranking", func(t *testing.T) {
		// Regression guard: Reranking=false must not leak --reranking in from a zero-value field,
		// whether or not Embedding is separately on.
		flags := ggufRunFlags(d, bin, LaunchProfile{Ctx: 4096})
		if flagIndex(flags, "--reranking") != -1 {
			t.Errorf("reranking off must not emit --reranking; flags=%v", flags)
		}
		embOnly := ggufRunFlags(d, bin, LaunchProfile{Ctx: 4096, Embedding: true})
		if flagIndex(embOnly, "--reranking") != -1 {
			t.Errorf("plain embedding (reranking off) must not emit --reranking; flags=%v", embOnly)
		}
	})

	t.Run("on with neither Embedding nor Pooling set still emits all three, in order", func(t *testing.T) {
		flags := ggufRunFlags(d, bin, LaunchProfile{Ctx: 4096, Reranking: true})
		embIdx := flagIndex(flags, "--embedding")
		poolIdx := flagIndex(flags, "--pooling")
		rerankIdx := flagIndex(flags, "--reranking")
		if embIdx == -1 {
			t.Errorf("reranking on must emit --embedding (llama.cpp requires it); flags=%v", flags)
		}
		if !hasFlagPair(flags, "--pooling", "rank") {
			t.Errorf("reranking on must emit `--pooling rank` even with Pooling unset by the caller; flags=%v", flags)
		}
		if rerankIdx == -1 {
			t.Errorf("reranking on must emit --reranking; flags=%v", flags)
		}
		if !(embIdx < poolIdx && poolIdx < rerankIdx) {
			t.Errorf("emission order must be --embedding, --pooling rank, --reranking; flags=%v (embedding@%d pooling@%d reranking@%d)",
				flags, embIdx, poolIdx, rerankIdx)
		}
	})

	t.Run("on with explicit Embedding/Pooling still produces the same coupled shape", func(t *testing.T) {
		flags := ggufRunFlags(d, bin, LaunchProfile{Ctx: 4096, Embedding: true, Pooling: PoolingRank, Reranking: true})
		if !hasFlagPair(flags, "--pooling", "rank") || flagIndex(flags, "--reranking") == -1 {
			t.Errorf("reranking on with matching explicit embedding/pooling must still emit the full triple; flags=%v", flags)
		}
	})
}

// TestGGUFRunFlags_RerankingSuppressesSpeculative pins that a reranking server, like a plain
// embedding server, never receives --spec-type or --spec-draft-n-max: it is encoder-only (via the
// Embedding coupling Normalize forces), so there is no autoregressive decode loop to speculate
// ahead of. Mirrors TestGGUFRunFlags_EmbeddingSuppressesSpeculative.
func TestGGUFRunFlags_RerankingSuppressesSpeculative(t *testing.T) {
	d := ModelDescriptor{ID: "m", Format: FormatGGUF}

	flags := ggufRunFlags(d, mtpBuild, LaunchProfile{Ctx: 4096, Reranking: true, SpecMode: SpeculativeDraftMTP, SpecDraftMax: 2})
	if flagIndex(flags, "--spec-type") != -1 {
		t.Errorf("reranking+draft-mtp must not emit --spec-type; flags=%v", flags)
	}
	if flagIndex(flags, "--spec-draft-n-max") != -1 {
		t.Errorf("reranking+draft-mtp must not emit --spec-draft-n-max; flags=%v", flags)
	}
}

// TestLaunchProfileNormalize_RerankingImpliesEmbeddingRank pins LaunchProfile.Normalize's
// documented Reranking coupling: Reranking=true forces Embedding=true and Pooling=PoolingRank
// regardless of what those two fields were set to, and Normalize stays idempotent (M2.5 queue-replay
// byte-identity — replaying an already-normalized rerank profile must not drift).
func TestLaunchProfileNormalize_RerankingImpliesEmbeddingRank(t *testing.T) {
	p := LaunchProfile{Ctx: 4096, Reranking: true}

	n := p.Normalize()
	if !n.Embedding {
		t.Errorf("Normalize() on Reranking=true must force Embedding=true; got %+v", n)
	}
	if n.Pooling != PoolingRank {
		t.Errorf("Normalize() on Reranking=true must force Pooling=%q; got %q", PoolingRank, n.Pooling)
	}
	// The embedding-mode speculative zeroing must still apply to a rerank profile (Reranking forces
	// Embedding before that block runs).
	spec := LaunchProfile{Ctx: 4096, Reranking: true, SpecMode: SpeculativeDraftMTP, SpecDraftMax: 2}.Normalize()
	if spec.SpecMode != SpeculativeOff || spec.SpecDraftMax != 0 {
		t.Errorf("Normalize() on Reranking=true must also zero SpecMode/SpecDraftMax like Embedding does; got SpecMode=%q SpecDraftMax=%d",
			spec.SpecMode, spec.SpecDraftMax)
	}

	// Idempotency: normalizing an already-normalized rerank profile must be a no-op.
	nn := n.Normalize()
	if nn != n {
		t.Errorf("Normalize() must be idempotent for a rerank profile; Normalize(p)=%+v Normalize(Normalize(p))=%+v", n, nn)
	}
}

// TestGGUFRunFlags_RerankingOffRegression is a pure regression guard, separate from the "off"
// sub-test above, pinning that a completely zero-value/unrelated profile never emits --reranking —
// the field must default to off exactly like every other opt-in profile field.
func TestGGUFRunFlags_RerankingOffRegression(t *testing.T) {
	d := ModelDescriptor{ID: "m", Format: FormatGGUF}
	flags := ggufRunFlags(d, winget, LaunchProfile{Ctx: 4096, KVTypeK: gguf.KVCacheQ8_0, Slots: 2})
	if flagIndex(flags, "--reranking") != -1 {
		t.Errorf("Reranking=false must never emit --reranking; flags=%v", flags)
	}
}

// TestValidatePooling covers ValidatePooling's allow-list: "" plus the five llama.cpp pooling
// types pass through unchanged; anything else (including a near-miss like "avg" and a
// case-mismatched "MEAN" — the validator is case-sensitive) is rejected with a reason that names
// the valid values (CLAUDE.md: every refusal carries a human-readable reason).
func TestValidatePooling(t *testing.T) {
	for _, ok := range []string{PoolingDefault, PoolingNone, PoolingMean, PoolingCLS, PoolingLast, PoolingRank} {
		if got, err := ValidatePooling(ok); err != nil || got != ok {
			t.Errorf("ValidatePooling(%q) = (%q,%v), want (%q,nil)", ok, got, err, ok)
		}
	}
	for _, bad := range []string{"avg", "MEAN", "Rank", "average", "sum"} {
		err := func() error {
			_, err := ValidatePooling(bad)
			return err
		}()
		if err == nil {
			t.Errorf("ValidatePooling(%q) should be rejected", bad)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "mean") || !strings.Contains(msg, "rank") {
			t.Errorf("ValidatePooling(%q) error should name the valid values (mean, rank); got %q", bad, msg)
		}
	}
}
