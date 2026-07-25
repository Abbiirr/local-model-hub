# C1 Master Gate Audit — LocalHub Compass

Prepared as the C1 close-out staging doc so the master gate is fast to walk once Phase 6
(P6.1/P6.2/P6.3) and P5.3 (live) land. **This is a read-only audit** — no code, docs, or
gates were changed by producing it. Status values follow docs/14: `PASS` / `PENDING` /
`BLOCKED-user` (+ `PARTIAL` where evidence exists but a live demonstration is still owed).

Evidence base (all dated 2026-07-06):
- research/phase-0-friction-log.md — P0 field trial, GO_STANDALONE rationale.
- research/phase-2-validation-matrix.md — 21-row compat matrix, zero false-positive `runs`.
- research/phase-5-load-storm.md — P5.3 real-hardware storm (PARTIAL PASS, honest gaps).
- research/lmwrapper-reuse-notes.md — ported algorithms + documented engine lessons.
- docs/23-phase-0-field-validation.md — machine + tools + Decision.
- docs/residency-priority-queue.md — P5.0 algorithm (Accepted).
- docs/decisions.md — ADRs (003 resolved custom-residency, 006 Accepted Go).
- temp-research/wiring-p2{1,2}.md — estimator internals + calibration.

---

## Part A — The five master success criteria (docs/14 "Master project close gate")

| # | Criterion | Status | Evidence pointer | What C1 must still demonstrate | Verification step |
|---|---|---|---|---|---|
| 1 | Fresh machine with Ollama + GGUF folder produces correct inventory and fit verdicts | **PASS (walkable now)** | Inventory: P1.4 (`GET /api/models`) over Ollama + `K:\projects\llm_models\models\` GGUFs + HF dirs. Verdicts: research/phase-2-validation-matrix.md — 21 rows, live `/api/compat/preview`, zero false-positive `runs` (gate §a). | A single clean end-to-end walk on THIS box captured as C1 evidence: start daemon on empty scratch DB → `GET /api/models` shows Ollama + GGUF + HF rows → `/api/models/{id}/compat` returns a verdict with reason for ≥3 models (one fits, one won't, one TQ3 fallback). | Start daemon (fresh DB); `curl /api/models`; pick 3 ids; `curl /api/models/{id}/compat`; confirm each verdict has non-empty `reason`+`estimated_vram`+`max_context_that_fits`+`run_flags`. Cross-check the `wont_run` against nvidia-smi free VRAM. |
| 2 | Admission control refuses or evicts before loading something that doesn't fit; no OOM from LocalHub load decisions | **PARTIAL** | research/phase-5-load-storm.md: E refused pre-admission (409 `start_refused`, numeric wont_run reason); dupC queued 111 MiB short with `blocked_reason`; **no OOM** (min free 1086 MiB); negative test #8 (P2.5) failed fast at 4 s. Unit/property tests green (`go test ./internal/residency/...`). | The **live evict-then-load path** and its per-eviction VRAM re-read were **not triggered on real hardware** (storm resolved by queueing, not eviction — load-storm §"Eviction sequence"). C1 needs either a live storm that forces at least one real eviction with an NVML re-read between evict and load, or an explicit WAIVED note that the mechanism is proven only by unit/property tests + the negative load. Also: the **RAM ceiling** (4 llama-server procs drove free RAM to 0.24 GB → abort) means admission budgets VRAM only; C1 should record this as a known limitation, not a gate fail. | Re-run P5.3 with models sized so admission MUST evict (e.g. pin one ~7 GB model, then request a second that only fits after eviction); assert nvidia-smi delta shows evict→re-read→load, no OOM in daemon log, victim was idle/non-pinned. |
| 3 | In-flight generation is not killed by default | **PARTIAL** | research/phase-5-load-storm.md: A pinned, 300/300 chunks intact, never a victim; failure-criteria audit all PASS. Eviction policy never selects pinned/active by default (P5.2 property tests). lmwrapper lesson: client disconnect does NOT cancel upstream generation. | **Honest gap (load-storm §"A stream integrity"):** A's stream finished ~15 s BEFORE the storm fired, so "active stream survives concurrent eviction" was NOT exercised as a live race. Also (§"Phase 6 notes"): `InFlightTracker` has **zero production callers** — a non-pinned streaming instance is currently invisible to the active gate; only pinned instances (or Ollama's recent-active window) are protected. **Phase 6's proxy must own stream counting** (increment on proxied start, decrement on completion). C1 cannot fully close #3 for non-pinned streams until P6.1 wires the tracker. | After P6.1: drive a slow generation through the proxy on a NON-pinned instance, fire admission pressure mid-stream, assert the stream completes intact AND the instance was protected via `ActiveTracker.ActiveCount` (not just pin). Until then, #3 holds only for pinned/Ollama-recent. |
| 4 | Phone over Tailscale shows live GPU, running models, and queue status | **BLOCKED-user** | P1.5 dashboard renders live GPU/engines/inventory (served by daemon, no CDN); WS+SSE live channels wired. scripts/tailscale-serve.ps1 + examples/tailscale-serve.md exist; loopback bind + SSE proxy-friendly headers verified. | TESTING.md#p16 STATUS: Tailscale backend wedged in NoState; **service restart needs an elevated shell the agent does not have.** Everything agent-preparable is done. C1 needs the MANUAL user action: admin `Restart-Service Tailscale -Force` → run serve script → open the ts.net URL on phone and confirm GPU + running models + queue render. This is a C2 user action. | MANUAL(user): from admin PowerShell `Restart-Service Tailscale -Force`; `scripts/tailscale-serve.ps1`; open printed ts.net URL on phone; confirm live GPU panel, running-instances list, and `/api/queue` status all render. |
| 5 | Every downloaded/imported model has a reproducible manifest | **PASS (walkable now)** | P3.1/P3.2/P3.3: HF revision→commit pin shown before download, per-file sha256, immutable manifest (source/revision/files/hashes/license/warnings), reproduce export; local-import provenance warning; pickle/trust_remote_code warn-not-block (ADR-004). Phase 3 close gate: no artifact in inventory without provenance. | A C1 spot-walk: import one artifact (or inspect the kept `Llama-3.2-1B-Instruct-Q4_K_M.gguf` from P2.5) → confirm its manifest carries source, commit/revision (or explicit `local-import` warning), file list, sha256s, license, warnings; run the reproduce export and confirm it names repo+commit+files+hashes. | `GET /api/models/{id}` for a downloaded model → assert manifest fields present; trigger reproduce export → assert repo/ref/commit/files/hashes(+flags) present; confirm no inventory row lacks provenance. |

### Master-gate tally

| Status | Count | Criteria |
|---|---|---|
| PASS (walkable now) | **2** | #1 inventory+verdicts, #5 manifests |
| PARTIAL (evidence exists; live demo owed, gated on P5.3-live / P6.1) | **2** | #2 admission/no-OOM (live eviction unproven), #3 in-flight survival (live race + non-pinned tracker unproven) |
| BLOCKED-user | **1** | #4 phone-over-Tailscale (elevated restart + phone check) |
| FAIL | 0 | — |

**One-line:** 2 PASS, 2 PARTIAL (both close once a P5.3 live-eviction run and P6.1 proxy
stream-counting land), 1 BLOCKED-user (Tailscale elevated restart + phone check).

---

## Part B — The seven non-waivable gates (docs/14 "Gates that cannot be waived")

| # | Gate | Status | Evidence pointer | What C1 must still demonstrate | Verification step |
|---|---|---|---|---|---|
| 1 | A model marked `runs` must not OOM in validation | **PASS** | phase-2-validation-matrix.md §(a): 5/5 loaded `runs` verdicts healthy, none OOM'd; the one `wont_run` load-attempt (#8) correctly failed fast; **false-positive `runs` = 0**. Reinforced in P5.3 (B/C/D `runs` all HEALTHY, no OOM). | Nothing net-new required; keep the invariant true for any model added to the matrix. If C1 re-runs any load, re-assert 0 false positives. | Re-read matrix §(a); if any new load done in C1, confirm loaded-and-healthy, no OOM in daemon log / nvidia-smi. |
| 2 | Residency must never kill an active generation by default | **PARTIAL** | P5.2 eviction never selects pinned/active (property tests green); P5.3 A pinned+intact, 0 evictions. lmwrapper §"active" rules ported. | Same gap as master #3: live race not exercised; `InFlightTracker` unwired (load-storm §Phase-6 notes). For NON-pinned streaming instances the gate holds today only via the coarse Ollama recent-active window. **P6.1 must wire stream counting** to make this fully true. | After P6.1: mid-stream admission-pressure test on a non-pinned instance (see master #3 step). |
| 3 | No download recorded without immutable provenance (source, revision/commit, file list, hashes, warnings) | **PASS** | P3.3 close gate: every download/import path writes an immutable manifest; per-file sha256; local-import carries explicit provenance warning. | Spot-check one manifest in C1 (see master #5). | `GET /api/models/{id}` manifest field assertion. |
| 4 | Must not bind admin controls to public internet by default | **PASS** | Default bind `127.0.0.1` (examples/config.example.yaml `server.bind: 127.0.0.1`). Every P0/P2/P5 load bound 127.0.0.1 (friction-log §Safeguard, matrix §Safeguard). Tailscale Serve (not Funnel) is the remote path (examples/tailscale-serve.md). | A C1 security-scan line: `netstat` shows the daemon on 127.0.0.1, not 0.0.0.0. Note the friction-log flag: the reference **.bat scripts default HOST=0.0.0.0** (overridden to localhost in every launch) — confirm the llama.cpp ADAPTER forces 127.0.0.1, not the bat default. | Start daemon; `netstat -ano | grep <port>` → 127.0.0.1 only; start one instance; confirm child llama-server bound 127.0.0.1 (P4.2 recorded this). |
| 5 | No cross-engine VRAM preemption / fair-share scheduling | **PASS** | ADR-001 Accepted + reaffirmed in source-revalidation log 2026-07-06. residency-priority-queue.md §"Why … does NOT violate ADR-001": queue orders admission REQUESTS only, never preempts/fair-shares; whole-model load/unload; free VRAM re-measured live. | Confirm no P6 transport (WS/JSON-RPC/jobs) introduced preemption. A grep-level audit in C1's final sweep. | Code review of internal/residency + internal/core: no per-layer/partial VRAM control; eviction is whole-model unload only. |
| 6 | No automatic Safetensors→GGUF conversion pipeline in core | **PASS** | ADR-004 Accepted. Routing is by format (safetensors→vLLM, GGUF→llama.cpp/Ollama) — matrix rows 18–21 route safetensors to vLLM, never convert. No conversion code path. | C1 grep audit confirming no `convert_hf_to_gguf`/conversion invocation in core. | `grep -ri "convert.*gguf\|convert_hf" internal/` → expect none in a core code path. |
| 7 | Every user-facing refusal/blocked request/compat verdict carries a human-readable `reason` | **PASS** | matrix §(c): reason mandatory + constructor-enforced, schema-tested in internal/compat. P5.3: E's 409 and dupC's 202 both carried numeric MiB reasons; failure-criteria audit "empty/vague reason = NO". api-spec.md error contract mandates `reason`. | Confirm the P6 surfaces (proxy 503, jobs errors, JSON-RPC error objects) also carry `reason` once they land (P6.1 requires 503+blocked_reason; P6.3 requires spec-compliant errors with human reasons). | After P6: assert proxy 503 body has `blocked_reason`; JSON-RPC error objects carry a human `reason`; job failure states carry reasons. |

### Non-waivable-gate tally

| Status | Count | Gates |
|---|---|---|
| PASS | **6** | #1, #3, #4, #5, #6, #7 |
| PARTIAL | **1** | #2 (never-kill-active — live race + non-pinned tracker owed to P6.1) |

---

## Part C — Release close gate checklist (docs/14 "Release close gate")

| Item | Status | What exists | What must be written / done for C1 |
|---|---|---|---|
| All phase gates through release scope are PASS | **PENDING** | P0–P4 gates PASS; P5.0/P5.1/P5.2 PASS; P5.3 **PARTIAL** (live eviction/stream-race unproven); P1.6 **BLOCKED-user**; Phase 6 (P6.1/P6.2/P6.3) **not started**. | Land P6.1–P6.3; upgrade P5.3 to full PASS (live eviction run) or record an explicit WAIVED rationale for the unexercised live-eviction path; resolve P1.6 via the user's Tailscale action. Record all gate statuses in one place. |
| docs/18-source-validation-matrix.md updated within 30 days | **PARTIAL (stale rows)** | File dated 2026-07-06 (fresh on date), but rows are generic "plan decision" claims with **no per-source verified-on citations** for the things actually exercised this cycle. | Add dated citations for what P0–P5 verified: Ollama `/api/ps`/`keep_alive` field shapes verified 2026-07-06; vLLM docker run + `/health`/`/v1/models` verified 2026-07-06 (command.go RE-VERIFY note); gguf-parser-go **v0.24.1** TQ3 gap (GGMLType 46); llama.cpp fork-vs-upstream flag facts (tq3_0 KV + TQ3 kernels + CUDA fork-only; `--fit`/`--sleep-idle-seconds` now upstream). See docs-sync-plan.md for exact row edits. |
| Install path tested from clean machine/container | **PENDING** | Go single-binary target (ADR-006); `scripts/dev.ps1`; examples/docker-compose.dev.yml. | A clean-box (or clean-container) install walk documented: build/download binary → run → daemon serves. Not yet evidenced. **Note constraint:** C: has ~6 GB free; a clean-container test must not write model data to C:. |
| Security scan confirms localhost default bind | **PASS (evidence exists, restate for release)** | Default bind 127.0.0.1 (config.example.yaml); every load bound localhost. | Capture one `netstat` line as release evidence (non-waivable gate #4). Note the bat-script HOST=0.0.0.0 default as a documented caveat. |
| Changelog written | **MISSING** | No CHANGELOG file exists (confirmed: repo root has none). | **Draft provided** in temp-research/draft-changelog-and-limitations.md (v0.1.0). Promote to `CHANGELOG.md` at release. |
| Known limitations written | **MISSING** | No KNOWN-LIMITATIONS file exists. | **Draft provided** in temp-research/draft-changelog-and-limitations.md. Promote to `KNOWN-LIMITATIONS.md` (or a README section) at release. |
| Example config current | **PASS** | examples/config.example.yaml + configs/config.example.yaml exist and match the current knobs (residency, queue caps 64/256/512, safety_factor 0.92, safety_margin_mb 1024, vllm util 0.90). | Verify the two copies stay in sync; confirm they reflect any P6 additions (jobs/WS/JSON-RPC settings) once those land. |
| Tailscale instructions current | **PASS** | examples/tailscale-serve.md (Serve not Funnel, localhost bind, identity-header trust rule) + scripts/tailscale-serve.ps1. | Add the P1.6 elevated-restart caveat (backend can wedge in NoState; admin restart needed) to the doc so a fresh user hits the same wall gracefully. |
| Troubleshooting current | **MISSING** | No troubleshooting doc exists. | **Write one.** Seed it from the real evidence: TQ3 fallback caveat; three-disagreeing-VRAM-numbers → measured NVML is truth; 4-process RAM ceiling on 16 GB; WSL2 9p slow weight load; Tailscale NoState needs admin restart; winget llama-server has NO CUDA (route GPU work to the fork); DRAINING-with-dead-pid reconciliation lag (load-storm §gap 4). |

### Release-gate tally

| Status | Count | Items |
|---|---|---|
| PASS / evidence-exists | **3** | example config, tailscale instructions, localhost-bind scan |
| PARTIAL | **1** | source-validation-matrix freshness (rows need dated citations) |
| PENDING (gated on P6 / clean-box test) | **2** | all-gates-PASS, install-path-from-clean |
| MISSING (must be written) | **3** | changelog, known-limitations, troubleshooting |

---

## Inconsistencies found (flag, do not decide — for user/orchestrator)

1. **api-spec.md actively recommends SSE OVER WebSocket and says build WS "only if you
   later need bidirectional"** (api-spec.md §"Realtime: prefer SSE over WebSocket"). But
   PLAN.md decision 7 + P1.5 already shipped WS, and P6.2/P6.3 mandate WS job-events +
   JSON-RPC-over-WS. The doc contradicts the shipped/planned reality. api-spec.md also
   lacks the WS, JSON-RPC, `/api/jobs`, and `/api/queue`-cancel surfaces. **Decision
   needed:** does api-spec.md get rewritten to first-class WS+JSON-RPC+jobs (per PLAN
   spec-conflict resolution 1), and when — before or after P6 lands? (docs-sync-plan.md
   §1 has the exact edit.)

2. **architecture.md hardware-probe wording says "NVML (`go-nvml` or `pynvml`)"** but the
   actual implementation is the **nvidia-smi subprocess** probe (PLAN.md decision 5;
   friction-log built entirely on nvidia-smi). Every doc that says "NVML" means "the probe
   interface" per PLAN spec-conflict resolution 3, but architecture.md reads as if go-nvml
   is linked — which ADR-005 forbids on this box. **Flag:** reword to name the nvidia-smi
   impl as current, NVML-on-Linux as later. Non-load-bearing but misleading for a reader.

3. **P5.3 is checked `[ ]` (open) in TODO.md but the phase-5-load-storm.md evidence is a
   self-declared PARTIAL PASS**, and it documents a **real unimplemented gap**: there is
   **no production queue-drain loop** (`queue.Next()` never called outside tests; queued
   dupC would never auto-start when VRAM frees — load-storm §gap 2). This is "unimplemented,
   not merely untested." **Flag:** the master gate #2/#4 (queue status shown, admission
   works) is materially incomplete until a `core.Service.DrainQueue` exists. Decide whether
   that is Phase 5 residual work or folded into P6.2 (which builds the persisted job queue
   and would naturally own draining).

4. **`vram_obs_mib` sampled the whole-GPU used figure under concurrency** (load-storm §gap
   5: C showed 11059 instead of its ~7 GB footprint) — the observed-peak calibration
   sampler may record cumulative used rather than a per-model delta when >1 instance is
   resident. **Flag for P2.5/calibration review:** a bad peak persisted to `observed_peaks`
   would poison later "exact" verdicts. Doesn't affect admission today (uses estimates) but
   is a correctness risk for the measured-beats-estimate loop.

5. **vLLM adapter passes an HF repo id to `--model`** (internal/engines/vllm/command.go),
   which triggers an **in-container HF download on first start with no LocalHub-side
   progress surfacing** — a STARTING-UX gap for remote models (the P4.5-class finding). And
   `--model` itself is a vLLM flag whose form has drifted across versions. **Flag:** both
   are real forward-compat/UX risks the version-policy (docs/22) should track; neither
   blocks the gate but both belong in known-limitations + the version matrix.

6. **decisions.md ADR-003 and ADR-006 are confirmed updated** (ADR-003 = "build custom
   residency, do not drive llama-swap," Resolved 2026-07-06 with trial evidence; ADR-006 =
   Accepted Go, 2026-07-06). No inconsistency here — this verifies the TODO/P0.3 claim.
   The one thing to keep honest: ADR-003's trial evidence lives in the friction log, which
   the ADR correctly cites.
