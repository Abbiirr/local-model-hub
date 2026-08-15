# PLAN — GPU telemetry, Free-GPU control, and dashboard rework

**Status:** backend §2 COMPLETE and verified live (2026-08-09); UI §3 in progress
**Created:** 2026-08-09
**Box:** RTX 4060 Ti 16 GB (cc 8.9, driver 576.40) · Ryzen 5 2600 · 16 GB RAM · Windows 10
**Driver of this work:** operator feedback — *"fix UI this is very inconvenient"*, *"I want to see
more details of gpu"*, *"there should be a free gpu / offload all models option"*,
*"localhub.exe should be the no console windows native app"*.

---

## 0. Why

The dashboard answers "what is installed" but not the two questions actually asked at the
keyboard: **"what can I start right now?"** and **"how do I get my GPU back?"**

Every model but one renders an `unchecked` compat badge, so fit is unknowable without clicking
each row. There is no way to free VRAM in one action. And GPU state is five numbers, none of
which explain *slow* — only *full*.

## 1. Constraints discovered (verified, not assumed)

| Finding | Evidence | Consequence |
|---|---|---|
| **Per-process VRAM is unavailable on Windows** | `nvidia-smi --query-compute-apps=pid,used_memory,name` returns `[N/A]` for `used_memory` on every row; some names return `[Insufficient Permissions]`. WDDM mode does not attribute VRAM per process. | Ship the process **list** (pid + name) and render usage as `n/a`. Never render `-1` as `0`. Per-process bytes need Linux or a TCC-mode datacenter card. |
| **Card runs PCIe gen 3 ×8, not gen 4** | `pcie.link.gen.current=3`, `gen.max=3`, `width=8/8`. Ryzen 5 2600 platform is PCIe 3.0. | Halves host→VRAM load bandwidth. Surface it — it explains slow model loads that VRAM numbers cannot. |
| **RAM binds before VRAM on this box** | Start refused at free RAM 1720 MiB vs floor 2048 while 14873 MiB VRAM sat free. Free RAM hit 478 MiB with one 4B loaded. | The dashboard must show the RAM floor alongside VRAM, or refusals look inexplicable. |
| **Free VRAM is non-deterministic** | Three identical compat previews seconds apart reported usable 13591 / 13812 / 13923 MiB. | Show *usable-after-margin* next to raw free, so the UI agrees with why admission refused. |
| **The usable-VRAM reason text misstates its own formula** | `compat/gguf_path.go` prints `(%d total × %.2f safety − %d in use)`. With total 16380, safety 0.92, in-use 1607 that reads as 13462 — but the value printed alongside was **13591**, because `usableVRAM` is actually `free × SafetyFactor` (`engine.go:130`), i.e. `(total − in_use) × 0.92`. | Formula is right, the parenthetical is wrong. 4 call sites, each with a different arg list, so reordering needs care. Tracked as bug #9. |
| **`localhub.exe` was console-subsystem** | Closing the shell killed the daemon. | **DONE** — rebuilt `-H=windowsgui` (Subsystem=2), added `logging.file` since stderr is discarded with no console. |

## 2. Backend

### 2.1 Extended GPU telemetry
Widen the existing single `nvidia-smi` invocation — no extra process spawn.

Add to `hardware.GPU`: `clock_sm_mhz`, `clock_mem_mhz`, `clock_sm_max_mhz`, `fan_pct`,
`pcie_gen`, `pcie_gen_max`, `pcie_width`, `pcie_width_max`, `power_limit_w`,
`power_max_limit_w`, `throttle`.

`-1` means "nvidia-smi said N/A". `throttle` decodes the `clocks_throttle_reasons.active`
bitmask to readable text (`idle`, `sw_power_cap`, `hw_thermal_slowdown`, …); empty means
unthrottled.

**Invariant:** field order in `nvidiaSmiQueryFields` must match the CSV parser's column layout.
Adding fields means updating both together.

### 2.2 GPU-holder process list
New `hardware.GPUProcess{pid, name, used_mib}` from `--query-compute-apps`, exposed on the
hardware payload. `used_mib` is `-1` on Windows (see §1).

### 2.3 Usable-after-margin
Expose the number admission actually uses — `floor(free × SafetyFactor)`, `SafetyFactor` = 0.92
(`compat/constants.go`, applied at `compat/engine.go:130`) — plus the residency RAM floor, so a
refusal reason and the dashboard cannot disagree.

Populated in `core.Hardware`, not the probe: `internal/hardware` must not depend on
`internal/compat`. Doing it once on the way out means REST, SSE, WS and JSON-RPC all quote the
same number instead of each re-deriving it and drifting.

### 2.4 History ring buffer
Server-side, fixed-size, in-memory (no DB — this is disposable telemetry). Samples
`{t, vram_used, util, temp, power}` at the existing metrics cadence. Sized for a few minutes.
Exposed for sparklines. Lost on restart by design.

### 2.5 `POST /api/gpu/free`
Stop every non-pinned instance and report reclaimed VRAM.

Reuses the existing verified-stop path — `DRAINING` first, then NVML-delta verification
(`stop.go` + `runner.go:53`). **Must not** kill instances with in-flight requests by default
(CLAUDE.md). Body: `{"include_pinned": false}`. Response: per-instance outcome + total MiB
reclaimed, measured not assumed.

## 3. UI

### 3.1 GPU panel
Detail rows for clocks / fan / PCIe / power-limit / throttle; sparklines from §2.4; the
GPU-holder process list with honest `n/a` usage; usable-after-margin beside raw free VRAM;
RAM floor shown with RAM.

### 3.2 Free-GPU control
Button wired to §2.5 with a confirm step, showing what will be stopped and what was reclaimed.

### 3.3 Inline compat verdicts
Replace `unchecked` with real per-model verdicts, computed against current hardware and cached,
refreshed when VRAM changes materially. This is the single biggest usability win: it turns the
model list from a filename inventory into an answer to "what can I start?"

### 3.4 Promote "Start a model"
Model list moves above `Allow CPU offload` / `Advanced launch profile` / `Attach a running
server`; those collapse behind a disclosure. On a GPU-only box (task #5) the CPU-offload
checkbox is the wrong thing to lead with.

### 3.5 Layout and clipping
Wrap/scroll the ENGINES panel (currently clipped mid-sentence). Mark which of the three
detected llama.cpp builds is actually used for a start. Reduce cross-panel scrolling.

## 4. Order

1. §2.1 extended fields → 2. §2.2 process list → 3. §2.3 usable-after-margin →
4. §2.5 free-GPU endpoint → 5. §2.4 history → 6. §3.1/3.2 GPU panel + button →
7. §3.3 verdicts → 8. §3.4/3.5 layout

Backend first: the UI cannot render data that is not collected.

## 5. Definition of done

- `go vet ./... && go build ./... && go test ./...` green
- `GET /api/hardware` carries every §2 field; `-1` renders as `n/a`, never `0`
- `POST /api/gpu/free` reclaims VRAM **verified by NVML delta**, never trusting a 200
- Pinned and in-flight instances survive a free-GPU call by default
- No model row shows `unchecked` when hardware and model metadata are both available
- ENGINES panel text is fully readable at the default window width
- Dashboard's usable-VRAM figure matches the number in an admission refusal reason

## 6. Out of scope

Per-process VRAM bytes on Windows (§1 — platform limit, not a bug). Multi-GPU layout (single
GPU on this box). The five-part desktop-app / startup-model / compat-CLI work in tasks #1–#6.

## 7. Related

`PLAN-run-stop-monitor.md` (stop/verify path reused by §2.5) · `KNOWN-LIMITATIONS.md` §25
(`always_available` models are not re-loaded at boot) · task #6 (8 verified bugs incl. nested
`text_config` and duplicate inventory rows).
