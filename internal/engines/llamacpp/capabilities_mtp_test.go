package llamacpp

import "testing"

// M3.1 gate: the spec/MTP and independent-K/V capability bits must DISCRIMINATE between the real
// binaries on the reference box. A false has_spec_mtp is the dangerous direction — it would let a
// start emit `--spec-type draft-mtp` at a binary that rejects the value — so the fixtures below are
// verbatim-shaped excerpts of the binaries' actual --help output (probed 2026-07-25).

// b10106HelpExcerpt reproduces the tokens b10106 prints. Note draft-mtp appears only as a VALUE
// inside the --spec-type enumeration, never as its own flag — which is why the probe greps for the
// bare token rather than a "--draft-mtp" flag that does not exist.
const b10106HelpExcerpt = `
-ctk,  --cache-type-k TYPE              KV cache data type for K
-ctv,  --cache-type-v TYPE              KV cache data type for V
--spec-draft-n-max N                    number of tokens to draft for speculative decoding (default: 3)
--spec-draft-n-min N                    minimum number of draft tokens
--spec-type none,draft-simple,draft-eagle3,draft-mtp,draft-dflash,ngram-simple,ngram-map-k,ngram-map-k4v,ngram-mod,ngram-cache
-np,   --parallel N                     number of server slots (default: -1, -1 = auto)
--flash-attn, -fa
--fit
`

// forkHelpExcerpt reproduces the llama.cpp-tq3 fork / winget build shape: --spec-type and the KV
// type flags EXIST, but there is no draft-mtp value and no --spec-draft-n-max. This is the case
// that must come back has_spec_mtp=false — confirmed by grepping both binaries (0 draft-mtp hits).
const forkHelpExcerpt = `
-ctk,  --cache-type-k TYPE              KV cache data type for K (also accepts tq3_0)
-ctv,  --cache-type-v TYPE              KV cache data type for V
--spec-type ngram-cache,ngram-simple
-np,   --parallel N                     number of server slots
--flash-attn, -fa
--fit
`

func TestParseCapabilities_MTPBuild(t *testing.T) {
	caps := parseCapabilities(b10106HelpExcerpt)
	if !caps.HasSpecMTP {
		t.Error("has_spec_mtp must be true: --spec-type advertises draft-mtp")
	}
	if !caps.HasSpecDraftMax {
		t.Error("has_spec_draft_max must be true: --spec-draft-n-max is present")
	}
	if !caps.HasKVCacheTypes {
		t.Error("has_kv_cache_types must be true: both --cache-type-k and --cache-type-v are present")
	}
	if !caps.HasParallelSlots {
		t.Error("has_parallel_slots must be true: -np/--parallel is present")
	}
	if caps.HasTQ3 {
		t.Error("has_tq3 must be false for the upstream build (no tq3 token)")
	}
}

func TestParseCapabilities_ForkHasNoMTP(t *testing.T) {
	caps := parseCapabilities(forkHelpExcerpt)
	if caps.HasSpecMTP {
		t.Error("has_spec_mtp must be FALSE: --spec-type exists but does not list draft-mtp. " +
			"Inferring MTP from the presence of --spec-type would emit a value this binary rejects")
	}
	if caps.HasSpecDraftMax {
		t.Error("has_spec_draft_max must be false: the fork has no --spec-draft-n-max")
	}
	// The fork DOES support independent K/V (and additionally tq3_0), so this stays true.
	if !caps.HasKVCacheTypes {
		t.Error("has_kv_cache_types must be true: the fork exposes -ctk/-ctv")
	}
	if !caps.HasTQ3 {
		t.Error("has_tq3 must be true for the fork (tq3_0 appears in its --help)")
	}
}

// TestParseCapabilities_IndependentBits proves the bits are probed SEPARATELY: a build exposing
// --spec-draft-n-max without draft-mtp (or the reverse) must not have one inferred from the other.
// That is what lets the flag builder emit the mode but omit the count, and vice versa.
func TestParseCapabilities_IndependentBits(t *testing.T) {
	onlyMax := parseCapabilities("--spec-draft-n-max N\n--spec-type ngram-cache\n")
	if onlyMax.HasSpecMTP {
		t.Error("--spec-draft-n-max alone must NOT imply draft-mtp support")
	}
	if !onlyMax.HasSpecDraftMax {
		t.Error("--spec-draft-n-max must be detected on its own")
	}

	onlyMTP := parseCapabilities("--spec-type draft-mtp,ngram-cache\n")
	if !onlyMTP.HasSpecMTP {
		t.Error("draft-mtp must be detected on its own")
	}
	if onlyMTP.HasSpecDraftMax {
		t.Error("draft-mtp alone must NOT imply --spec-draft-n-max support")
	}
}

// TestParseCapabilities_KVNeedsBothSides: independent K/V requires BOTH flags. A build exposing
// only one cannot express `-ctk q8_0 -ctv q4_0`, so it must be launched with an f16 cache.
func TestParseCapabilities_KVNeedsBothSides(t *testing.T) {
	if parseCapabilities("--cache-type-k TYPE\n").HasKVCacheTypes {
		t.Error("has_kv_cache_types must be false when only the K side exists")
	}
	if parseCapabilities("--cache-type-v TYPE\n").HasKVCacheTypes {
		t.Error("has_kv_cache_types must be false when only the V side exists")
	}
	if !parseCapabilities("-ctk TYPE\n-ctv TYPE\n").HasKVCacheTypes {
		t.Error("the short -ctk/-ctv spellings must also be recognised")
	}
}

// TestBuildReason_SurfacesMTPCaps: the human-readable engine reason must name the new caps, since
// /api/engines is where an operator checks whether MTP is available at all.
func TestBuildReason_SurfacesMTPCaps(t *testing.T) {
	reason := buildReason("10106", []string{"CUDA"}, parseCapabilities(b10106HelpExcerpt))
	for _, want := range []string{"spec-mtp", "spec-draft-n-max", "kv-types"} {
		if !capsContains(reason, want) {
			t.Errorf("engine reason must mention %q; got %q", want, reason)
		}
	}
}

func capsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
