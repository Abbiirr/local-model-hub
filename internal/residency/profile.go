package residency

import "fmt"

// ProfileSpec is the behaviour-affecting launch profile carried through admission, the durable
// queue row (migration 0009), and the drain-loop replay. It is a deliberately DUMB value type:
// plain strings and ints, no validation, no engine knowledge.
//
// WHY it is not compat.LaunchProfile: `internal/residency` must not depend on `internal/compat`.
// Residency decides WHETHER and WHEN a request may start; compat decides WHAT flags a start emits
// and what it costs. Keeping that dependency out preserves the split (and avoids an import cycle,
// since core wires both). `internal/core` owns the two small translations — profileSpecFromProfile
// and profileFromSpec in core/residency.go — covered by a round-trip test.
//
// EVERY field here changes the emitted llama-server flags and/or the VRAM estimate. That is the
// membership rule: a field that can change neither does not belong in the queue row; a field that
// CAN and is missing is a replay bug. Precisely the 2026-07-25 defect, where kv_quant,
// allow_cpu_offload and speculative were dropped, so a queued q4_0 start came back as f16, priced
// ~15602 MiB against ~14168 MiB usable, and waited forever (KNOWN-LIMITATIONS #21).
type ProfileSpec struct {
	// KVTypeK / KVTypeV are the INDEPENDENT -ctk/-ctv cache dtypes ("", "f16", "q8_0", "q4_0").
	// The legacy kv_quant bool is resolved to this pair ONCE at enqueue time, so the queue never
	// stores two representations that could disagree after a restart.
	KVTypeK string
	KVTypeV string
	// Slots is -np (parallel server slots). 0 => 1. Admission-relevant: slots multiply the KV cache.
	Slots int
	// SpecMode is the speculative mode ("", "ngram-cache", "draft-mtp").
	SpecMode string
	// SpecDraftMax is --spec-draft-n-max. 0 => omit the flag (engine default).
	SpecDraftMax int
	// AllowCPUOffload permits a GPU/CPU split (-ngl auto, plus MoE -ncmoe when derivable). It
	// changes FLAGS and the MoE footprint; it never relaxes admission.
	AllowCPUOffload bool

	// Embedding / Pooling / Reranking are the llama.cpp SERVER-MODE fields (`--embedding`,
	// `--pooling`, `--reranking`). They are not admission-relevant sizing knobs like the fields
	// above — they change which ENDPOINTS the spawned process answers — but they are just as
	// behaviour-affecting: a queued start of an encoder-only embedding model that replays without
	// `--embedding` spawns a generation-mode llama-server against a model that has no causal decode
	// path, which crashes on startup (observed live: bge-m3 queued with embedding:true replayed as
	// plain generation and flapped in a crash loop — the same class of defect as the 2026-07-25 KV
	// drop, KNOWN-LIMITATIONS #21, just for the mode fields instead of the sizing fields).
	Embedding bool
	// Pooling is llama.cpp `--pooling` (""|none|mean|cls|last|rank). Meaningful only with Embedding.
	Pooling string
	// Reranking is llama.cpp `--reranking`. Implies Embedding + Pooling=rank (compat.LaunchProfile.
	// Normalize enforces the coupling on the profile that actually gets launched).
	Reranking bool
}

// IsZero reports whether the spec carries nothing but defaults — used to keep queue rows and log
// lines clean for the common "no advanced knobs" start.
func (p ProfileSpec) IsZero() bool { return p == ProfileSpec{} }

// String renders the profile compactly for a blocked_reason / log line, e.g.
// "kv=q8_0/q4_0 np=1 spec=draft-mtp(n_max=2)". Returns "" for a zero profile so callers can append
// it unconditionally without producing noise.
func (p ProfileSpec) String() string {
	if p.IsZero() {
		return ""
	}
	s := ""
	if p.KVTypeK != "" || p.KVTypeV != "" {
		s += fmt.Sprintf("kv=%s/%s ", orF16(p.KVTypeK), orF16(p.KVTypeV))
	}
	if p.Slots > 0 {
		s += fmt.Sprintf("np=%d ", p.Slots)
	}
	if p.SpecMode != "" {
		s += "spec=" + p.SpecMode
		if p.SpecDraftMax > 0 {
			s += fmt.Sprintf("(n_max=%d)", p.SpecDraftMax)
		}
		s += " "
	}
	if p.AllowCPUOffload {
		s += "cpu_offload=on "
	}
	if len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}

// orF16 renders an unset cache dtype as the llama.cpp default it means.
func orF16(s string) string {
	if s == "" {
		return "f16"
	}
	return s
}
