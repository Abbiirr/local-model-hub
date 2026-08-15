// Package compat is the LocalHub compatibility rule engine (P2.3): it fuses the two
// estimators — internal/compat/gguf (GGUF path, wraps gguf-parser-go) and
// internal/compat/safetensors (safetensors/vLLM path, own math) — with the live hardware
// snapshot (internal/hardware) and read-only engine detection (internal/engines) to produce
// a single structured Verdict (runs | runs_degraded | wont_run | unknown) with a MANDATORY,
// specific, human-readable reason, per docs/compatibility-engine.md (spec-digest §4) and the
// PLAN.md resolutions.
//
// Design rules enforced here:
//   - Every Verdict carries a non-empty Reason (CLAUDE.md correctness bar; TESTING.md#p23
//     "schema-enforced"). This is made structurally hard by the newVerdict constructor:
//     callers do NOT build Verdict struct literals; they call the constructor which panics
//     on an empty reason (caught by a schema-level test). See verdict.go.
//   - A false "runs" that OOMs is a blocking correctness bug — the engine prefers
//     conservative estimates + margins (supply-side safety_factor 0.92, PLAN res. 9).
//   - Format routing is strict: GGUF never goes to vLLM (ADR-004); safetensors goes to
//     vLLM only (never GGUF->vLLM); TQ3_* models REQUIRE a has_tq3 llama.cpp binary.
//   - This package does NOT link CUDA and does NOT mutate any engine. It only reads
//     estimates, the probe, and detection results.
package compat

// MiB is one mebibyte in bytes (matches internal/hardware + internal/compat/gguf).
const MiB = 1024 * 1024

// SafetyFactor is the multiplicative estimator fit margin (spec §4; PLAN.md resolution 9,
// pinned to 0.92). USABLE VRAM = free_vram_now * SafetyFactor. The margin lives on the
// SUPPLY side (we shrink usable VRAM); the requirement is never inflated. This is the
// estimator's own margin — residency adds its own additive SAFETY_MARGIN_MIB downstream
// (PLAN res. 9), which is NOT applied here.
//
// PLAN res. 9 asks for "one constants file, documented, in internal/compat". This is that
// file. The sibling safetensors package keeps a package-private copy of the same 0.92 value
// (it cannot import this package without a cycle); the value is identical and cross-checked
// by TestSafetyFactorMatchesSafetensors.
const SafetyFactor = 0.92

// DegradedBoundaryFraction is the ±band around the fit boundary within which a
// FALLBACK-confidence estimate (the estimator could not parse the model — e.g. TQ3_*) is
// downgraded to runs_degraded rather than a confident runs. Resolution: a low-confidence
// estimate that lands within ±15% of the usable-VRAM boundary is not trustworthy enough to
// call a clean "runs" (spec §4 correctness bar + PLAN safeguards). 0.15 = ±15%.
const DegradedBoundaryFraction = 0.15

// MinDegradedContext is the smallest context (tokens) at which a model still counts as
// runs_degraded rather than wont_run. If a model fits ONLY below this context it is not
// usefully runnable, so the verdict is wont_run. Spec §4 "Context-length realism": surface
// "fits at 8K, not 32K"; but a model that fits only at, say, 512 tokens is effectively
// won't-run for real use. 2048 is the floor (matches the safetensors wiring note's example).
const MinDegradedContext = 2048

// CalibratedMarginMiB is the ADDITIVE headroom applied instead of SafetyFactor when a verdict's
// footprint came from a MEASURED observed peak (Confidence "exact") rather than an estimate.
//
// WHY the multiplicative factor is wrong for a measurement: SafetyFactor (0.92) exists to absorb
// ESTIMATOR ERROR, and it scales with the CARD, not with the uncertainty. On a 16380 MiB card it
// reserves ~1310 MiB unconditionally — so a model deliberately sized to fill 16 GB can never be
// admitted no matter how much VRAM is actually free, even when we hold driver-truth numbers for it.
// That was blocking the Qwen3.6-27B MTP profile at 64K (~15687 MiB), which the owner has
// demonstrably run on this box with headroom.
//
// A measured peak has no estimator error left to absorb. What it still needs is a cushion for
// allocator jitter and run-to-run variation: CUDA context/workspace sizes shift slightly between
// runs, and free VRAM itself moves by tens of MiB with ordinary desktop activity (see
// AdmissionNoiseMiB). 320 MiB sits comfortably above the ~50 MiB of observed VRAM noise and above the
// run-to-run spread seen in calibration on this box (13856 → 13370 → 13152 MiB for the same model at
// different profiles), while leaving a model that genuinely fits able to start.
//
// This applies ONLY to Confidence "exact". Every estimate — approximate or fallback — keeps the full
// multiplicative SafetyFactor, so the OOM guard is untouched for any model that has not actually been
// measured on this hardware.
const CalibratedMarginMiB = 320

// AdmissionNoiseMiB is the width of the VRAM-noise band around the admission boundary (M4.6).
//
// MEASURED, not guessed: on this box the Qwen3.6-27B MTP model at 8K q4_0 estimates 14107 MiB
// while usable VRAM was observed between 14153 and 14168 MiB across runs — a margin of only
// ~46-61 MiB. Ordinary desktop VRAM churn (a browser tab, a compositor redraw) is larger than
// that, so the SAME request flipped between HTTP 201 (admitted) and HTTP 409 (refused) with
// nothing about the model or the profile changing. That instability was originally misread as a
// standing refusal (PLAN-run-stop-monitor.md §2 corrections).
//
// 256 MiB sits above the observed spread with room to spare, and well below the estimator's own
// measured pessimism at this profile (+570 MiB: predicted 14107, observed 13537).
//
// What LocalHub does with it: a fit INSIDE this band is reported as `runs_degraded` with a numeric
// reason saying the decision is within VRAM noise, and naming what makes it deterministic (record a
// measured peak for this exact (K,V) pair, free VRAM, or lower the context). It deliberately does
// NOT change admission: no new refusals, and — critically — no `runs` that would OOM. The verdict
// becomes HONEST about a coin-flip instead of presenting it as a solid yes.
const AdmissionNoiseMiB = 256

// ReducedContextThreshold is the context below which "fits only at a reduced ctx" triggers
// runs_degraded even when the estimate is confident. Per the task spec: "runs_degraded
// when: fits only at reduced ctx (< 4096)". A model whose max-fitting context is below this
// is degraded (it runs, but not at a comfortable context).
const ReducedContextThreshold = 4096

// vLLMMaxGPUMemUtil mirrors the safetensors package's practical gpu-memory-utilization
// ceiling (0.97). Above ~0.97 vLLM commonly OOMs on CUDA-graph capture / fragmentation. The
// value is package-private in internal/compat/safetensors (cannot be imported without a
// cycle); it is duplicated here for the wont_run reason text and cross-checked in tests.
// [RE-VERIFY] against the pinned vLLM version.
const vLLMMaxGPUMemUtil = 0.97

// vLLMMinComputeCapability is the hard minimum NVIDIA compute capability vLLM requires
// (spec §4 hardware-capability gate: CC >= 7.5). The reference box is 8.9 (passes), but the
// rule MUST exist so a lower-CC box is correctly gated out. [RE-VERIFY] against the pinned
// vLLM version's install docs (compatibility-engine.md §revalidation 2026-07-06).
const vLLMMinComputeCapability = 7.5

// ctxCandidates are the discrete context points the GGUF path probes to find the max
// context that fits (binary-search-ish; the task spec says "3-4 candidate ctx points
// 4K/8K/16K/32K — don't over-call remote"). The safetensors path uses a closed-form solve
// instead (MaxContextThatFits), so this list is GGUF-only.
var ctxCandidates = []int{4096, 8192, 16384, 32768}

// EngineVersion identifies this rule engine for the version-drift requirement (docs/22).
// Bump on behavioral changes to the rules.
const EngineVersion = "localhub-compat@0.1.0"
