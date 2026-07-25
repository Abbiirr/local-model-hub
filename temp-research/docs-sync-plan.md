# Docs Sync Plan — stale docs vs code reality (for a docs agent to execute mechanically)

Prepared for the C1 close-out. Each entry names the doc, the exact staleness, and the
precise edit to make. A docs agent should be able to execute these without re-deriving
context. **This is a plan only — no docs were edited.** Grounded in: current code tree
(internal/, web/src/), PLAN.md spec-conflict resolutions, and the dated evidence files.

Priority key: **P6-gated** (do after Phase 6 code lands) · **now** (safe to do today) ·
**release** (part of the release close gate).

---

## 1. docs/api-spec.md — missing WS / JSON-RPC / jobs / queue-cancel; SSE-vs-WS advice contradicts shipped code  · **P6-gated (partly now)**

**Staleness:**
- §"Realtime: prefer SSE over WebSocket for MVP" tells the reader to build WS "only if you
  later need bidirectional." But WS already shipped in P1.5 (`internal/api/ws.go`,
  `web/src/live/`), and PLAN.md decision 7 + P6.2/P6.3 mandate WS job-events and
  JSON-RPC-over-WS. This is now **wrong advice**, not just incomplete.
- No `POST /api/jobs`, `GET /api/jobs/{id}`, job cancellation, or the WS job-events channel
  (P6.2).
- No JSON-RPC 2.0 surface (P6.3) — method coverage mirroring the REST core over HTTP POST +
  WS.
- Queue section has `GET /api/queue` and `DELETE /api/queue/{id}` in the doc; verify these
  match the shipped handlers (`internal/api/instances.go` / handlers).

**Exact edits:**
1. (now) Rewrite the "prefer SSE over WebSocket" paragraph to state the shipped reality per
   PLAN spec-conflict resolution 1: "SSE remains the spec'd server→client stream; WebSocket
   is an **additional** thin transport over the same core (shipped P1.5), and JSON-RPC 2.0
   is a further thin transport (P6.3). All four (REST/SSE/WS/JSON-RPC) sit over one service
   core — no business logic is duplicated per transport."
2. (P6-gated) Add a "Realtime channels" subsection documenting the WS channel(s): the
   live-state channel (P1.5) and the job-events channel (P6.2), using the lmwrapper event
   vocabulary (`request.accepted` / `queue.update` / `model.loading` / `model.ready` /
   `response.delta` / `response.completed` / `heartbeat`) — this is the agreed vocabulary
   (research/lmwrapper-reuse-notes.md §"API endpoint checklist").
3. (P6-gated) Add an "Async jobs" section: `POST /api/jobs {inference request} -> {job_id}`
   (queued through admission + P5.0 ordering), `GET /api/jobs/{id}`, cancellation
   (mid-queue and mid-inference: stream aborted server-side, instance NOT killed), and
   "jobs survive daemon restart (persisted queue)". Every failure state carries a `reason`.
4. (P6-gated) Add a "JSON-RPC 2.0" section: over HTTP POST + WebSocket; methods mirror the
   REST core (hardware/engines/models/compat/instances/residency/jobs); spec-compliant
   error objects each carrying a human-readable `reason` (id echo, error codes, batch
   requests).
5. (now) Confirm the queue-cancel + `blocked_reason` fields in the doc match the actual
   `GET /api/queue` payload (P5.3 showed `blocked_reason` with numeric MiB — keep that
   shape).

---

## 2. docs/architecture.md — hardware-probe wording says NVML/go-nvml; reality is nvidia-smi subprocess  · **now**

**Staleness:** §"Hardware probe" reads *"NVIDIA-first via NVML (`go-nvml` or `pynvml`)"* and
the component diagram labels the probe `(NVML/ROCm/psutil)`. The shipped probe is a
**`nvidia-smi` subprocess** (PLAN.md decision 5; the entire P0/P2/P5 evidence base is
nvidia-smi). Linking go-nvml on this Windows box would violate ADR-005 (control plane must
not link CUDA). Per PLAN spec-conflict resolution 3, "NVML" in docs always means "the
hardware-probe interface."

**Exact edits:**
1. Reword §"Hardware probe" to: "Reads GPU/CPU/RAM/disk behind a probe **interface** with
   swappable implementations. Current impl: **`nvidia-smi` subprocess** (XML/CSV) — the
   control plane deliberately does not link CUDA/NVML (ADR-005). Native NVML (Linux) and
   ROCm/`amd-smi` are alternate implementations of the same interface, not yet shipped.
   CPU/RAM/disk via gopsutil."
2. In the component diagram, change the probe label from `(NVML/ROCm/psutil)` to
   `(nvidia-smi now; NVML/ROCm later)`.
3. (Optional, now) Add a one-line pointer that stop/evict VRAM verification uses the same
   probe delta (NVML/nvidia-smi delta, never HTTP 200).

**Note:** §"Stack decision" still presents Go-vs-Python as an open choice ("Option A/B",
"Decide explicitly in ADR-006"). ADR-006 is now **Accepted = Go**. Add a one-line banner at
the top of that section: "**Resolved: Go (ADR-006, Accepted 2026-07-06).** The options below
are retained as the rationale." Do not delete the rationale.

---

## 3. docs/18-source-validation-matrix.md — rows need today's per-source verified-on citations  · **release**

**Staleness:** file header says `Last checked: 2026-07-06` but the table rows are generic
"plan decision" statements with **no per-source verified-on-date citations** for the things
actually exercised this cycle. The release gate wants this current with real citations.

**Exact edits (add/annotate rows; keep existing rows, append a "Verified" note per row that
was exercised):**
1. **Ollama residency controls / concurrent behavior / `/api/ps` fields** — append:
   "Verified 2026-07-06: `/api/version`, `/api/tags`, `/api/ps` (`size_vram`, ctx),
   `keep_alive: 0` unload confirmed by polling; `/api/embed` + `/api/generate` warmed on
   the reference box (research/phase-2-validation-matrix.md rows 6–7, friction-log #8)."
2. **vLLM CUDA requirement / GGUF / sleep mode** — append a new row or note:
   "vLLM docker run + `GET /health` + `GET /v1/models` launch contract verified against
   docs.vllm.ai 2026-07-06 (internal/engines/vllm/command.go RE-VERIFY note). Docker mode
   canonical invocation confirmed; `--model` passed after image name. GGUF still NOT routed
   to vLLM (requires out-of-tree `vllm-gguf-plugin`, ADR-004)."
3. **llama.cpp server routes** — append: "Verified 2026-07-06: `llama-server` fork
   (turbo-tan/llama.cpp-tq3 commit 1cfa891) is fork-only for **`tq3_0` KV cache + TQ3
   weight kernels + CUDA backend**; `--fit`, `--sleep-idle-seconds`, `--models-dir`,
   `--reasoning-budget` are **now upstream too** (winget b8833). winget build has **no
   CUDA** (Vulkan/CPU only) — route GPU work to the fork (friction-log §Fork-vs-upstream)."
4. **gguf-parser (add a row if absent)** — "gguf-parser-go **v0.24.1**: reads local + remote
   HF GGUF by range request; **cannot parse TQ3** (`GGMLType(46)`), even metadata → LocalHub
   uses a labelled fallback heuristic (research/phase-2-validation-matrix.md rows 9–12;
   wiring-p21). Verified 2026-07-06."
5. **llama-swap** — append: "Trialled 2026-07-06 (v235): swap/`/running`/unload/TTL all
   work; NO VRAM-aware admission, estimate, priority, or reason → ADR-003 resolved to build
   custom residency above it (friction-log §llama-swap trial)."
6. Update the header date only when the edits are actually made (keep it honest).

---

## 4. docs/22-engine-version-policy.md — add the known-good matrix actually validated  · **release**

**Staleness:** §"Release requirement" lists the fields the known-good matrix must document
(OS/GPU/driver/CUDA/Ollama/llama.cpp/vLLM/gguf-parser-go versions) but the **matrix itself
is not filled**. All the values exist in docs/23 + the evidence files.

**Exact edit:** add a "Known-good matrix (2026-07-06 reference box)" table:
| OS | Windows 10 Home 10.0.19045 | | GPU | RTX 4060 Ti 16 GB (CC 8.9) | | driver | 576.40 (CUDA 12.9 runtime) |
| Ollama | 0.21.2 | | llama.cpp | fork turbo-tan/llama.cpp-tq3 @1cfa891 (CUDA) + winget b8833 (Vulkan) | | vLLM | vllm/vllm-openai:latest via Docker (WSL2) | | gguf-parser-go | v0.24.1 |
Add a note: winget llama.cpp is **Detected-untested for CUDA work** (no CUDA backend);
vLLM `--model` form is version-sensitive (pin the image, re-verify on upgrade).

---

## 5. docs/decisions.md — ADR-003 / ADR-006 already updated (VERIFIED — no edit needed)  · **now (verify only)**

**Verified against the file:**
- ADR-003 Status = **"Resolved 2026-07-06 (Phase 0 trial): build the custom residency
  manager; do not drive llama-swap."** with trial-outcome evidence. ✅ correct, no edit.
- ADR-006 Status = **"Accepted 2026-07-06 — Go."** ✅ correct, no edit.
- §"Source revalidation log — 2026-07-06" is present and current. ✅

**One optional addition (now):** ADR-004's note that "vLLM GGUF moved out of core" is
sharpened in the revalidation log to "requires the out-of-tree `vllm-gguf-plugin`" — the
main ADR-004 body still says "experimental/under-optimized and moved out of core." Consider
aligning the ADR-004 body wording with the more precise revalidation-log phrasing. Low
priority; not load-bearing.

---

## 6. docs/residency-scheduler.md — "no starvation guarantees in v1" superseded by P5.0  · **now**

**Staleness:** per PLAN spec-conflict resolution 2 and residency-priority-queue.md, the
scheduler doc's "Queue semantics" section (which reportedly says "no starvation guarantees
in v1") is **superseded** — P5.0 shipped a documented anti-starvation aging algorithm.

**Exact edit:** in residency-scheduler.md §"Queue semantics", replace the "no starvation
guarantees" language with: "**Superseded by docs/residency-priority-queue.md (P5.0,
Accepted).** The queue provides bounded-wait anti-starvation via age-based class promotion;
see that doc for the algorithm, worked examples, and property tests." Keep the rest of the
section (victim ordering etc.) intact and cross-link `internal/residency/queue.go`.
*(Verify the exact current wording before editing — this entry is derived from the PLAN
spec-conflict note, not a direct read of residency-scheduler.md.)*

---

## 7. docs/architecture.md + api-spec.md — state machine / instances surface (verify parity)  · **now**

**Check (not necessarily an edit):** architecture.md shows
`REQUESTED → STARTING → HEALTHY → (DRAINING) → STOPPED` with `FAILED`. Confirm this matches
the shipped `internal/instances/` state machine (P4.1) exactly, including the DRAINING-with-
dead-pid reconciliation the load-storm flagged (§gap 4: a stop whose HTTP caller disconnects
mid-drain leaves the row stuck in DRAINING). If reconciliation-on-tick is not yet documented,
add a line to architecture.md §"Process/state model": "DRAINING instances whose process is
already dead are reconciled to STOPPED on the restart/tick reconcile pass."

---

## 8. README.md — "Status: Pre-alpha … engineering handoff docs only"  · **release**

**Staleness:** README §Status says *"This repository currently contains the engineering
handoff docs only."* That is now false — Phases 0–5 are implemented (Go daemon, React UI,
adapters, compat engine, residency). 

**Exact edit:** update §Status to reflect the real state at release: "v0.1.0 (unreleased):
Phases 0–5 implemented and validated on the reference box; Phase 6 (thin OpenAI proxy +
async jobs + JSON-RPC) in progress. See CHANGELOG.md and KNOWN-LIMITATIONS.md." Also update
the "Monitoring + phone" bullet, which says metrics are "over SSE" — add "and WebSocket."
Do this at release, after Phase 6 lands, so the status line is accurate.

---

## 9. New docs to CREATE (not sync — they don't exist)  · **release**

These are release-gate requirements with no current file:
1. **CHANGELOG.md** — draft ready in temp-research/draft-changelog-and-limitations.md.
2. **KNOWN-LIMITATIONS.md** — draft ready in the same file.
3. **Troubleshooting doc** (e.g. `docs/troubleshooting.md` or a README section) — seed from
   known-limitations: TQ3 fallback; three-disagreeing-VRAM-numbers → NVML is truth; ~3-proc
   RAM ceiling on 16 GB; WSL2 9p slow load; Tailscale NoState → admin restart; winget
   llama-server has no CUDA; DRAINING-with-dead-pid lag.
4. **examples/tailscale-serve.md** — ADD the P1.6 elevated-restart caveat (backend can wedge
   in NoState; `Restart-Service Tailscale -Force` from admin PowerShell needed) so a fresh
   user hits the wall gracefully. (This is an edit to an existing file, listed here for
   grouping.)

---

## Execution order suggestion for the docs agent

1. **now, safe today:** #2 (architecture NVML wording + Go-resolved banner), #5 (verify
   ADRs — likely no-op), #6 (scheduler starvation note), #7 (state-machine parity check),
   and the SSE-vs-WS paragraph rewrite in #1(1).
2. **release:** #3 (source-validation citations), #4 (version known-good matrix), #8
   (README status), #9 (create changelog/limitations/troubleshooting, tailscale caveat).
3. **P6-gated (after Phase 6 code lands):** #1(2–4) (WS/jobs/JSON-RPC API sections).
