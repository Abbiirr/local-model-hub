# Known Limitations — LocalHub Compass v0.1.0

Ordered roughly by how likely a user is to hit them. Every item cites its evidence. These are
honest limits of the shipped code, not future-work wishlist items.

## 1. TQ3 (and other unparseable quants) fall back to a conservative heuristic — a false `wont_run` is possible

gguf-parser-go v0.24.1 **cannot parse TQ3 quants** (fails with `GGMLType(46) unsupported`, even
for metadata). LocalHub degrades to a labelled file-size + KV heuristic rather than erroring,
but that heuristic is deliberately conservative and **over-counts**. Concretely, the four TQ3
flagship models on the reference box (Qwen3.x 27B/35B) get `wont_run` verdicts (e.g.
Qwen3.6-27B-TQ3_4S estimated ~16178 MiB @8K vs 12226 usable) — yet the battle-tested
`run-qwen3.6-27b-tq3_4s.bat` profile **does run them** at low context when VRAM is freer. The
verdict carries the fallback caveat and notes a measured peak may fit. **Mitigation:** load it
once for real; the observed-peak calibration then returns an exact verdict thereafter.
(research/phase-2-validation-matrix.md rows 9–12; friction-log #1; wiring-p21.)

## 2. RAM — not VRAM — binds first on a 16 GB-RAM box; a global RAM floor now guards it (no per-model RAM estimate, no RAM-based eviction)

Admission now enforces a **system-RAM floor** (`residency.min_free_ram_mib`, default **2048
MiB**) as a first-class start gate, alongside the VRAM checks. Before **every** new
process/warm across **all engines** (llama.cpp spawn, Ollama warm, vLLM docker), admission reads
**fresh** free system RAM from the hardware probe; when it is below the floor, the request is
**queued** (or rejected if the class queue is capped) with a numeric `ram_blocked:` reason
(e.g. `ram_blocked: free system RAM 1800 MiB below floor 2048 MiB …`) surfaced **exactly like
VRAM shortages** — in `/api/queue`, the proxy `503` body, and job `reason`s. The drain loop
re-admits the request once RAM recovers (e.g. a VRAM-eviction that stops a `llama-server` also
frees that process's RAM). This closes the operational-safety gap the load-storm exposed:
**four resident `llama-server` processes drove free system RAM to 0.24–0.54 GB** (16 GB box,
Docker Desktop/other apps also resident) **while VRAM never OOMed** (min free 1086 MiB).

**Limits of the shipped floor (deliberate scope):**

- It is a **global guard**, not a **per-model RAM estimate** — admission does not predict how
  much RAM a specific model will use; it only refuses to start anything new while free RAM is
  below the floor. Set the floor conservatively for your box (on 16 GB, plan for ~3 concurrent
  llama.cpp processes).
- There is **no RAM-based eviction / victim selection** — the VRAM eviction policy is unchanged
  and never selects a victim to free RAM. RAM recovery relies on the queue+drain path (an idle
  VRAM-eviction happens to free RAM, a process exits, or external RAM pressure clears).
- Warm routes to an already-resident model are **exempt** (no new process → no RAM gate).
- The floor is a fresh point-in-time probe; it does not reserve RAM against a concurrently
  starting instance, so under a burst of simultaneous starts it is a best-effort ceiling.

(fix 4; research/phase-5-load-storm.md §"Not exercised" #1; full-project-review-2026-07-06.md
finding 4; internal/residency/admission.go RAM floor; internal/config/config.go
`residency.min_free_ram_mib`.)

## 3. vLLM footprint is `gpu-memory-utilization × total VRAM`, so co-residency needs a lowered util

vLLM **pre-allocates** `--gpu-memory-utilization × TOTAL` VRAM and fills it — it does not size
to the model. The estimator reports the required VRAM and the minimum util that holds it, and
the adapter passes a recommended util (defaulting up to 0.90). But to co-reside a vLLM model
with another engine's model, you must **lower vLLM's util explicitly** or vLLM grabs its whole
configured fraction of the card regardless of the model's actual size. (wiring-p22 §3.)

## 4. vLLM on Windows is Docker/WSL2 only; WSL2 9p mount makes weight loading slow

vLLM has no native Windows support — use Docker/WSL2 or attach to a pre-existing endpoint.
Loading model weights from a Windows path into the vLLM Docker container over the WSL2 9p
filesystem is slow (one observed weight load ~79.9 s). Keep model files on a Linux-native path
inside WSL, or a mounted volume with better throughput, for faster vLLM cold starts. *(P4.5
controlled-session finding; verified 2026-07-06 against vLLM GPU install docs.)*

## 5. `--model` in the vLLM image is a version-drift risk; remote-HF start has a UX gap

The vLLM adapter's docker invocation passes `--model <repo>` after the image name. The exact
form has drifted across vLLM versions; the command is unit-tested string-exactly so a break is
caught by a failing test, but a vLLM image upgrade may require updating the flag — **pin the
image and re-verify the launch contract before upgrading**. Separately, when a vLLM model is
started from an HF repo id, vLLM **downloads the weights inside the container on first start**
and LocalHub surfaces STARTING without in-container download progress, so a first start of a
large remote model can sit in STARTING for a long time. Pre-caching the HF weights (mounted
`~/.cache/huggingface`) avoids this. (internal/engines/vllm/command.go RE-VERIFY note; docs/22.)

## 6. Cancellation mid-decode is best-effort — the engine finishes the token stream

A client disconnect does **not** cancel an upstream llama.cpp generation; it runs to
completion. Mid-decode interruption is effectively impossible on the engines LocalHub
supervises. Cancellation is honored cleanly only while a request is still **queued** (or during
prefill); once decode is underway, LocalHub stops tracking (aborts its own upstream request and
releases the in-flight count) but does not abort the engine, and does not kill the instance
(never-kill gate). This applies equally to the async-jobs cancel path — "cancelled" means
LocalHub stopped waiting, not that the GPU instantly stopped. Eviction is designed around
idle-only. (research/lmwrapper-reuse-notes.md; internal/core/jobs.go CancelJob.)

## 7. Ollama VRAM accounting is an approximation (shared daemon, no per-instance port, 30 s active window)

Ollama is a **shared daemon** LocalHub does not own, and there is no per-instance port — all
Ollama models are routed through the one Ollama daemon. VRAM for Ollama models is read from
`/api/ps` (`size_vram`), which disagreed with both gguf-parser (579 MiB) and the NVML delta
(701 MiB) for the same model — three numbers, ~30–60 % apart; **the measured NVML delta is
treated as truth**. "Active" for an Ollama model is approximated by a recent-activity window
(~30 s) rather than exact in-flight tracking, and unload is requested via `keep_alive: 0` and
confirmed by polling (never a process kill). (matrix row 6; friction-log #6/#8.)

## 8. Hardware probe is Windows/`nvidia-smi`-only today

The current hardware probe shells out to `nvidia-smi` (the control plane deliberately does not
link CUDA/NVML — ADR-005). Native NVML (Linux) and ROCm/`amd-smi` probes are designed as
alternate implementations of the same interface but **are not shipped**; this release is
validated on NVIDIA + Windows only. `nvidia-smi` parsing stability across driver versions is a
maintenance risk. (PLAN.md decision 5; docs/23 machine profile.)

## 9. Active-stream protection is guaranteed only through the LocalHub ingress (proxy/relay)

"Never kill an active generation" is enforced via the in-flight counter, which is incremented
only when a request enters through a **LocalHub-mediated path** — the Phase-6 OpenAI proxy
(`/v1/*`), the async-jobs relay, or the per-instance completion relay
(`POST /api/instances/{id}/completions`). A client that calls an engine's `base_url` **directly**
is invisible to the counter; such an instance is protected only if it is **pinned** (or within
Ollama's recent-active window). The Phase-6 proxy closes this for clients that go through
LocalHub; direct-to-engine callers should pin. Note also that the live "active stream survives a
concurrent eviction" race and the live `evict_then_load` path were proven by unit/property tests
plus a negative load, not yet as a live hardware race (the load-storm resolved by queueing, and
the one pinned stream finished ~15 s before admission pressure arrived).
(research/phase-5-load-storm.md §"A stream integrity", §"Phase 6 notes".)

## 10. RESOLVED 2026-07-07 — blocked-admission latency on the OpenAI proxy (was ~10 s)

An earlier live gate measured ~10–12.5 s on the proxy's blocked-503 path. Root cause was
`gguf-parser-go` doing ~2.7 s of buffered CPU parsing per call, ×4 calls (requested ctx + 3
context candidates), ahead of admission. Fixed by enabling mmap + SkipLargeMetadata on the
LOCAL-file parse only (`internal/compat/gguf/parse.go`) — a single parse dropped 2.33 s →
0.031 s with **byte-identical VRAM estimates** (verified 1B/8B/32B × four context sizes; the
TQ3 fallback path is unchanged). Live re-measure: **1.36–1.54 s across 8 trials including the
first call after daemon start** — the <2 s gate criterion is met. Kept here for history; the
gate evidence lives in research/phase-6-live-gates.md (Amendment).

## 11. Observed-VRAM calibration may over-sample under concurrency

The measured-peak sampler (`vram_obs_mib`) that feeds the "exact" calibration may record the
whole-GPU used figure rather than a per-model delta when more than one instance is resident (the
load-storm saw C report 11059 MiB instead of its ~7 GB footprint). This does not affect admission
today (admission uses estimates, not stored peaks), but a bad peak persisted to `observed_peaks`
could poison a later "exact" verdict. Under review for the measured-beats-estimate loop.
(research/phase-5-load-storm.md §gap 5; full-project-review-2026-07-06.md finding under Phase 2.)

## 12. No auto queue-drain of proxy/instance queue entries beyond the jobs worker

The async-jobs worker drains queued **jobs** when VRAM frees. But a plain queued **instance
start** (`POST /api/instances` that returned 202) is not auto-retried by a periodic drain of the
residency queue in the same way — a queued instance-start may require a re-POST when room frees.
The durable path for "submit and wait for VRAM" is the async-jobs API. (research/phase-5-load-storm.md
§"Not exercised" #2.)

## 13. Trust model is localhost + Tailscale + one optional admin token — no multi-user auth

There is no OAuth / multi-tenant / RBAC (explicit non-goal). Access is protected by (a) the
default `127.0.0.1` bind, (b) Tailscale Serve + tailnet ACLs / identity headers for remote
access, and (c) an **optional but enforced** single `admin_token` (bearer, constant-time; when
set, required on all `/api/*` + `/v1/*` except `GET /api/health` and static UI assets). The token
is null by default. Do not expose LocalHub directly to the public internet; there is no
Funnel-safe mode, and the single token has no per-endpoint scoping. (docs/security.md;
internal/api/middleware.go; config `admin_token: null`.)

## 14. Phone-over-Tailscale confirmation is pending a manual user action

All agent-preparable phone-access work is done (serve script, SSE/WS proxy-friendly headers,
loopback bind, docs), but on the reference box the Tailscale backend wedged in NoState and a
restart needs an **elevated shell** (`Restart-Service Tailscale -Force` from admin PowerShell).
The final phone check (open the ts.net dashboard on a phone, confirm live GPU + running models +
queue render) is a **manual user action** and has not been performed. (TESTING.md#p16.)

## 15. GPU temp/util thresholds are admission-time guards, not continuous enforcement

`residency.max_gpu_temp_c` (default 87; RTX 4060 Ti max 90C, throttles ~83C) and
`residency.max_gpu_util_pct` (default 0 = disabled) are checked only when *starting/warming a new
model* — a running model is never killed for crossing them (the never-kill-active rule). A blocked
start queues with a numeric `gpu_hot` / `gpu_busy` reason and the drain loop re-admits once the card
cools / frees; a warm route (already-loaded model) is exempt. (internal/residency/admission.go §1c.)

## 16. Gemma-4 (iSWA) KV is priced by a LocalHub correction, not the estimator

`gguf-parser-go@v0.24.1` does not model Gemma-4's interleaved sliding-window attention:
it has no `gemma4` architecture case, and under LocalHub's `SkipLargeMetadata()` local
parse the per-layer `head_count_kv` array is dropped, so the library prices the gemma-4
KV cache at **0 MiB at every context** (an under-price → false-fit OOM risk). LocalHub
therefore wraps the estimator (ADR-002) with an iSWA-aware KV floor computed from the
GGUF metadata — `internal/compat/gguf/iswa.go` — adding the shortfall to `VRAMMiB` and
recording it in `CtxVRAMMiB` with a verdict note. On the real gemma-4-12B this adds
~1024 MiB @4K / ~1088 MiB @8K / ~1472 MiB @32K (40 sliding + 8 global of 48 layers,
n_swa=1024). **v1 caveats:** (a) sliding layers are sized to `n_swa + 2048` (llama.cpp's
default logical-batch allowance) — if you launch with a much larger `-b`, the true SWA
cache is bigger and the floor could still under-state it; (b) the floor is only applied
when it EXCEEDS the library's own KV number, so a future gguf-parser-go that models
gemma-4 correctly transparently supersedes it; (c) other future SWA architectures the
library also can't model are covered generically (any model exposing
`<arch>.attention.sliding_window` + per-layer or scalar `head_count_kv`), but only
gemma-4 has been validated against a real file. Calibrate against a real NVML-measured
load when the GPU is free (research doc §7; TESTING.md). (temp-research/optimization-support-research.md §7.)

## 17. Self-speculative decoding is opt-in and draft-free only (v1)

The per-start `speculative` knob (POST /api/instances) accepts only `""` (off) and
`"ngram-cache"` (llama.cpp `--spec-type ngram-cache`, draft-free n-gram self-speculation,
~zero extra VRAM). The other `ngram-*` variants, the draft-MODEL path (`-md` + two-model
VRAM accounting), and llama.cpp *master*'s newer `--spec-draft-*`/`draft-eagle3`/
`draft-dflash` surface are NOT exposed — the value set is pinned to the local b8833
`--spec-type` help output (research §0/§1). The knob changes launch flags only and never
admission; Ollama/vLLM starts ignore it. Draft-model speculative decoding is tracked as a
separate future feature (research §5(iii)). (internal/compat/flags.go; docs/api-spec.md.)

## 18. A warm route is context-INDEPENDENT — the running instance's ctx is reused

Both the residency warm check and the StartInstance warm short-circuit match a resident model on
`model + engine + HEALTHY`, **ignoring the requested context length**. So if a client (or the OpenAI
`/v1` proxy, which resolves at the configured `DefaultContext`) asks for a model that is already
running at a *different* ctx, LocalHub routes to the running instance **at its current ctx** rather
than restart it at the requested ctx. This is the deliberate v1 behavior: it is what makes a warm
route free (zero VRAM, no reload) and it fixes the self-shadow 503 (a large resident model whose
fresh verdict would be re-evaluated against the free VRAM that already excludes its own weights, so
it "doesn't fit itself"). Consequence: a request needing a *larger* ctx than the running instance
will be served at the smaller ctx, not transparently upgraded. A ctx-aware restart (stop + reload at
the larger ctx when the client demands it) is future work. (internal/core/instances.go StartInstance
warm short-circuit; internal/residency/admission.go step 1.)

## 19. MoE CPU expert-offload (`-ncmoe`) requires a PARSEABLE GGUF and derivable expert bytes

When `allow_cpu_offload=true` and the model is a mixture-of-experts GGUF, LocalHub can move the
expert weights of every expert layer to CPU RAM via llama.cpp `-ncmoe N` (verified flag, research
§0/§4), letting an oversize MoE (e.g. Qwen3.6-35B-A3B) fit on 16 GB. Admission then sizes against the
REDUCED GPU footprint (total − expert bytes on CPU), where the expert bytes are DERIVED from tensor
metadata — never guessed (`internal/compat/gguf/moe.go`, `internal/compat/launchplan.go`
`moeOffloadDecision`). **v1 limits:** (a) if a MoE opts into offload but its expert tensors are not
measurable, the start is DECLINED with `"MoE offload sizing not derivable for this model"` rather
than admitted on a guessed footprint (no speculative admission). (b) **The current inventory MoE is
`Qwen3.6-35B-A3B-TQ3_4S`, a TQ3_* quant that gguf-parser-go cannot parse at all** (the same
`GGMLType(46)` error as the file-size fallback path) — so its expert bytes cannot be derived and the
`-ncmoe` path does not engage; the start falls back to plain `-ngl auto` with conservative
full-footprint admission (safe: it may refuse a MoE that offload would have fit, never a false fit).
A parseable MoE (Q4/Q8 GGUF) gets the full `-ncmoe` + reduced-footprint admission. (c) v1 offloads
ALL expert layers (`N = expert-layer count`); a partial/tuned N to balance GPU/CPU is future work.
(d) expert weight on CPU raises host-RAM pressure — the RAM floor guard (KNOWN-LIMITATIONS #… RAM
floor) still applies. Validate against a real NVML-measured MoE load when the GPU is free
(research §6). (internal/compat/gguf/moe.go; internal/compat/launchplan.go.)

## 20. Observed-peak calibration is keyed by ONE canonical kv_type, not the (K,V) pair

The `observed_peaks` table indexes measurements by `(model_key, ctx, kv_type)` — a single TEXT
column predating independent K/V cache dtypes. With `-ctk`/`-ctv` now independent,
`gguf.CanonicalKVKey(k, v)` collapses the pair into that one key: a **symmetric** pair renders as
the bare type (`f16`, `q4_0`) so every row recorded before independent K/V still matches, and an
**asymmetric** pair renders as `k/v` (e.g. `q8_0/q4_0`). An asymmetric profile therefore has **no
calibration data until a peak is measured for that exact pair**, and falls back to the estimate —
deliberately, because a peak measured with a q4_0 value cache must never justify a q8_0 one.

**Regression worth remembering (found 2026-07-25):** when the (K,V) refactor first landed it
populated only `KVCacheTypeK/V` and left the legacy `KVCacheType` empty, so the lookup key became
`""`, **every** recorded measurement missed, and verdicts silently degraded from `exact` to
`fallback` confidence. Caught by `TestVerdictUsesObservedPeak` /
`TestVerdictTightensAfterRecording`. If either test fails again, suspect the calibration key
before the estimator. (internal/compat/gguf/gguf.go `CanonicalKVKey`;
internal/compat/gguf_path.go; migration 0003_observed_peaks.sql.)

## 21. RESOLVED 2026-07-25 — the start API and the queue now carry the full launch profile

**Both halves are fixed.** Kept for history because the second half was a live, reproduced data-loss
bug and its regression tests are named after it.

**Was (a):** `core.StartInstanceRequest` exposed only `kv_quant bool` → q4_0 on BOTH cache sides, so
`-ctk q8_0 -ctv q4_0` (the validated Qwen3.6-27B MTP profile), the MTP draft maximum, and the slot
count were unreachable from any API or UI caller. **Now:** `kv_type_k`, `kv_type_v`, `slots`, and
`spec_draft_max` are first-class request fields (M2.1), exposed in the UI's advanced start form
(M2.3), and validated before dispatch. `kv_quant` still works, documented as a shorthand for
`q4_0/q4_0`; an explicit pair wins over it.

**Was (b), the live bug:** the `queue` table persisted `req_ctx`/`req_batch`/`req_pin` but **not**
`kv_quant`, `allow_cpu_offload`, or `speculative`, so a queued start replayed with **f16 KV** — which
for the 27B prices at ~15602 MiB against ~14168 MiB usable, leaving the entry `WAITING` forever with
`footprint 15602 + 1024 margin`. A request that WOULD have fit at q4_0 (~14107 MiB) could never
drain: the queue silently rewrote it into an impossible one. **Now:** migration
`0009_queue_launch_profile` persists the whole profile, `residency.ProfileSpec` threads it through
admission/restore/drain, and `residencyStarter.StartQueued` — where the fields were actually being
dropped — rebuilds the request through one audited translation. The legacy bool is resolved to the
(K,V) pair ONCE at enqueue, so the queue never stores two representations that can disagree. Guarded
by `TestQueueReplay_LegacyKVQuantSurvives`, `TestQueueReplay_FullProfileRoundTrip`, and
`TestQueueReplay_NoFieldSilentlyDropped` (per-field, so a future profile field added without
threading fails the suite).

**Honest remainder:** the LIVE replay — queue a start, restart the daemon, prove the normalized plan
is identical — has not been run, and migrations 0009–0011 have not yet been applied to the live
`localhub.db`. See TODO.md M6.6 / TESTING.md#m2.

## 22. LAN-open and token-enforced access cannot coexist (single global admin token)

`server.admin_token` is one global switch: `authMiddleware` applies it to every `/api/*` and
`/v1/*` request regardless of origin. The owner's intended two modes — **no password on the LAN,
password required over Tailscale** — are therefore not expressible by config; enabling the token
for tailnet clients also locks out passwordless LAN clients. Needs request-origin classification
(loopback / LAN / tailnet, the last via the Tailscale identity headers `trust_tailscale_headers`
already governs) plus a per-origin policy. See docs/security.md "Two operating modes" and
TODO.md X8.

**Current reference-box state is Mode A**: `bind: 0.0.0.0`, `allow_nonlocal: true`,
`admin_token: null` — anyone who can reach port 39876 has full admin rights (start/stop GPU
processes, trigger downloads), on **every** interface including the two ZeroTier overlays
(10.147.19.x, 172.27.25.x), not just the Ethernet LAN.

## 23. Post-HEALTHY health monitoring can kill an unresponsive instance, and its live gate is unrun

The periodic monitor (M5, `internal/instances/monitor.go`, 15 s interval) closes a real hole — an
engine that died after reaching HEALTHY used to keep reporting HEALTHY until the daemon restarted —
but it is worth knowing exactly what it will and will not do.

**It kills, under narrow conditions.** After **3 consecutive** failed `/health` probes
(`DefaultReapAfterFailures`, ≈45 s of continuous unresponsiveness) against a still-alive PID, it kills
the process tree and marks the instance `FAILED`. It will **never** do that to an instance that is
pinned, has in-flight requests (never-kill-active), or is in any state other than `HEALTHY`. The
PID-gone path is unconditional but risk-free — there is nothing left to kill. Set
`MonitorConfig.ReapAfterFailures = 0` to disable killing entirely; the PID-gone → `FAILED` path keeps
working either way. **The residual risk is a false positive:** llama-server's `/health` can stall
under sustained load or during a very long prompt-eval, and a request that entered the engine
*directly* (not through a LocalHub-mediated path) is invisible to the in-flight counter
(KNOWN-LIMITATIONS #9), so such an instance is protected only by pinning.

**The live gate is NOT closed.** Every behaviour above is proven by unit tests with injected
process/health seams; **no disposable HEALTHY process has actually been killed on this box**, because
the pinned 27B holds ~14.6 GiB of 16 GiB and the owner's constraint is to keep it up. Until that runs,
treat post-HEALTHY detection as designed-and-unit-tested, not field-proven (TESTING.md#m5).

**Monitor-detected deaths never claim a verified reclaim.** A death or reap sets
`reclaim_verified = false`, because no NVML delta was measured — only a requested stop through
`Runner.Stop` measures one. So a `FAILED` row from the monitor tells you the process is gone, NOT that
its VRAM is back; re-read `/api/hardware` to confirm.

## 24. The 64K MTP profile is still BLOCKED — the flags exist, the calibration does not

Everything needed to *express* the validated Qwen3.6-27B MTP profile now exists end-to-end: the
capability probe (`has_spec_mtp`), the model-side MTP tensor detection, both-sides gating, the exact
flags (`-ctk q8_0 -ctv q4_0 -np 1 --spec-type draft-mtp --spec-draft-n-max 2`), independent K/V
sizing, and the MTP head surcharge. **None of it has been run live.**

Specifically still missing: an MTP-on/MTP-off sanity + determinism comparison (M6.5), the
8K→16K→32K→64K admission calibration with predicted-vs-observed VRAM per tier (M4.4/M4.5), and any
measured peak for an ASYMMETRIC (K,V) pair — the calibration store is keyed by the canonical pair
(`q8_0/q4_0`), which correctly MISSES until measured, so an asymmetric profile currently runs on the
conservative estimate rather than driver truth (see #20). No 64K default is persisted, and per
TODO.md M6.1 none may be until those tiers pass. The standing classification for the MTP + 64K
profile remains **BLOCKED**, not "runs".

## 25. `always_available`/`available` do not proactively (re)load at daemon startup — DEFERRED

Migration 0015's four-state `launch_state` policy (docs/api-spec.md) implements PINNING
(`always_available` never selected by eviction) and idle-TTL EXEMPTION
(`always_available`/`available` never idle-unloaded) — both wired into the mechanisms that already
existed (`Instance.Pinned`, `Instance.TTLSeconds<=0`). What it deliberately does **NOT** do is
proactively start a STOPPED `always_available`/`available` model — at daemon boot, after a crash, or
after a manual stop. Today those states only take effect the next time an instance of that model is
(re)started through the normal admission path (explicit `POST /api/instances`, or the OpenAI proxy
auto-load for a request that happens to name it); nothing eagerly reaches out and loads it on its
own.

This was scoped out deliberately, not for lack of a plausible design. On 2026-08-06, this exact box
(16 GB system RAM, 16 GB VRAM) hit an uncontrolled auto-start loop that repeatedly loaded a 13.5 GB
model, exhausted system RAM, tripped the `ram_blocked` admission floor, and made the daemon
unusable for hours (the incident migration 0014's decommission flag — and now `unavailable` — exists
to prevent). A proactive-load-at-startup feature that does not (a) go through the SAME admission gate
as every other start (never bypassing the RAM floor or VRAM check), (b) refuse to retry in a tight
loop on failure, and (c) log a clear reason and give up gracefully, would be exactly that failure
mode again, just triggered by a config flag instead of a remote client. Implementing it correctly
needs its own admission-aware, single-attempt-per-boot startup pass with its own tests — a
meaningfully separate unit of work from the pin/TTL wiring here. Per this repo's working instructions,
a missing feature beats a resurrected thrash loop, so it is deferred rather than rushed.

**Practical implication today:** flipping a model to `always_available`/`available` while it is
STOPPED does not bring it up by itself — start it once (`POST /api/instances`) and it stays up
(pinned, TTL-exempt) until something explicitly stops it or the daemon restarts.
