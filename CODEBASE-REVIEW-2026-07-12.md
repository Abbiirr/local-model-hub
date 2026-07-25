# LocalHub Codebase Review — 2026-07-12

**Goal of this review:** assess LocalHub as a cross-platform control-plane ("wrapper") over **llama.cpp and vLLM**, with a hard requirement that it run correctly on **Linux + NVIDIA**. Ollama, the web UI, supply chain, and the compat estimator were reviewed as supporting surfaces.

**Method:** eight specialized read-only sub-agents (four Opus on the critical engine/residency/platform paths, four Sonnet on compat/API/supply/UI), each briefed on the project's ADR hard-constraints and correctness bars. No files were modified during the review.

---

## TL;DR — is it a working Linux+NVIDIA wrapper today?

**Mostly ready, with real gaps to close.** The architecture is sound and honors every ADR hard-constraint: engines run as external supervised processes (no CUDA linking, no forking), residency is coarse whole-model load/unload with live VRAM re-measurement, and the anti-OOM machinery (safety margins, per-eviction NVML re-reads, degrade-to-queue) is genuinely well built. The non-Windows process/hardware layer is **really implemented**, not stubbed — group-kill, SIGTERM→SIGKILL escalation, procfs/statfs all work on Linux.

But three themes of defect matter for the stated target:

1. **False "VRAM freed" / false "fits" verdicts** — the exact OOM-inducing class the project bans. A swallowed VRAM-probe error fabricates a "verified" stop in *both* the llama.cpp and vLLM adapters; the vLLM stop marks STOPPED even when the container isn't confirmed gone; and the compat calibration fast-path replays a stale measurement at the wrong context length.
2. **Linux/NVIDIA environmental fragility** — a single unsupported `nvidia-smi` field (`compute_cap`) blanks the entire GPU list on older LTS drivers; `--runtime nvidia` is hardcoded and breaks `docker run` on modern container-toolkit setups; native `vllm serve` on Linux is an unimplemented stub (Docker is the only working vLLM launch mode); GPU 0 is hardcoded everywhere; MIG/`[Not Supported]` memory reads as 0.
3. **Security posture vs. the "reach it over my phone" deployment** — the WebSocket disables Origin checking and no JSON handler validates `Content-Type`, so in the default (`admin_token: null`) config a malicious web page can drive real state changes (spawn GPU instances, start downloads, submit inference).

None require an architectural rewrite. The findings below are contained and individually fixable.

### Severity tally

| Severity | Count | Where |
|---|---|---|
| CRITICAL | 3 | Compat calibration (1), API WebSocket + CSRF (2) |
| HIGH | 9 | llama.cpp (2), vLLM (1), cross-platform (2), core/residency (2), compat (1), supply (1) + 1 HIGH "dead code" (Ollama) |
| MEDIUM | ~18 | spread across all layers |
| LOW | ~15 | spread across all layers |

---

## Priority fix list (before trusting it on a real Linux+NVIDIA box)

Ordered by "will this cause an OOM, a wrong kill, or a breach on the target platform."

1. **Stop swallowing the "before" VRAM probe error** — llama.cpp `internal/engines/llamacpp/stop.go:62`, vLLM `internal/engines/vllm/runner.go:346-349`. A failed baseline read defaults to 0 and fabricates a huge "verified reclaim." Treat a failed before-probe as *unverified*, never as a delta. Directly defeats the stop/evict correctness bar.
2. **vLLM stop must not mark STOPPED when the container isn't confirmed gone** — `internal/engines/vllm/runner.go:356-372`. An orphaned container keeps holding `gpu_memory_utilization × VRAM` while LocalHub thinks it's free → next admission OOMs.
3. **Gate the compat calibration fast-path on ctx/KV/GPU** — `internal/compat/gguf/gguf.go:262-275`, `internal/compat/calibration.go:56-70`. A peak measured at ctx=4096 is replayed as high-confidence "fits" at ctx=32768. A false "runs."
4. **Make the `nvidia-smi` query resilient to unsupported fields** — `internal/hardware/nvidia_smi.go:22,98-118`. `compute_cap` fails the whole query on 450.x/CUDA-11 LTS drivers → **zero GPUs → refuse every model** on a working box. `scripts/nvidia-preflight.sh` passes while the daemon fails.
5. **Wire the drain-timeout + close the classify→evict TOCTOU** — core `internal/core/instances.go:506-545`, residency `internal/residency/admission.go:474-539`; `internal/residency/support.go`'s `EvaluateDrain` has no production caller. Explicit stops and a narrow eviction race can kill instances mid-stream — the non-waivable "never kill active" bar.
6. **Fix the WebSocket Origin bypass + validate `Content-Type` on JSON handlers** — `internal/api/ws.go:38-40`; `instances.go`, `supply.go`, `handlers.go`, proxy. Drive-by CSRF against the default no-auth localhost/tailnet daemon.
7. **Don't hardcode `--runtime nvidia`** — `internal/engines/vllm/command.go:80-82`. Breaks `docker run` on modern nvidia-container-toolkit/CDI hosts; make it opt-in, rely on `--gpus all`.
8. **Enforce the download size cap during streaming** — `internal/manifest/download.go` (`streamCopy`). Currently only pre-flight (trusts declared size) or post-hoc (already on disk) — an arbitrary `gguf_url` can wedge the disk.
9. **Fix `DownloadHF` "biggest weight file" selection** — `internal/manifest/downloader.go:104-112`. It records the *last* weight file, not the biggest, so multi-quant/sharded repos get `LocalPath` pointing at the wrong file → engine loads wrong quant or misses shard 1.
10. **Linux-appropriate defaults** — config `Default()` bakes in `K:\...` paths and `.exe`; `db_path` is relative → breaks under systemd with cwd `/`. Ship XDG/`/var/lib` defaults or a documented Linux example config.

---

## Cross-cutting theme: the "false freed / false fits" family

Four independent findings across three packages are the *same underlying mistake* — trusting a value that wasn't actually measured, then reporting high confidence:

- llama.cpp `stop.go:62` — swallowed before-probe → fabricated verified stop
- vLLM `runner.go:346-349` — swallowed before-probe → fabricated verified reclaim
- vLLM `runner.go:356-372` — STOPPED state without confirming container gone
- compat `gguf.go` calibration — replays a stale peak regardless of requested ctx

The residency layer's own re-measurement (which *is* correct) partly backstops the first two, but each adapter reports its own verdict upward, and the calibration one feeds the admission estimate directly. Fixing these four closes most of the OOM risk on the target.

---

# Detailed findings by area

The eight sub-agent reports follow verbatim. Each finding carries `**File:** path:line`, the issue, the impact, and a suggested fix. Areas that are solid are called out as such.

---

## llama.cpp adapter (reviewed by Opus)

### [HIGH] A failed "before" VRAM probe can produce a false VERIFIED stop
- **File:** `internal/engines/llamacpp/stop.go:62` (with `waitReclaim`, lines 114-147)
- **Issue:** `freeBefore, _ := r.vram.FreeVRAMMiB(ctx)` swallows the probe error. If NVML/nvidia-smi errors transiently at this call, `freeBefore` is `0`. In `waitReclaim`, `freeAfter` starts at `freeBefore` (0); the first *successful* post-probe (e.g. 13000 MiB) makes `freed = 13000 - 0 = 13000 >= bar`, so the stop is reported `Verified: true` with a fabricated ~13 GiB reclaim. The instance is persisted STOPPED with a bogus `vram_freed_mib`.
- **Impact on Linux+NVIDIA target:** Directly violates the CLAUDE.md correctness bar "`stop`/`evict` must verify VRAM is actually freed (NVML delta), not trust an API 200." NVML read failures are plausible on Linux (driver hiccup, `nvidia-smi` contention).
- **Suggested fix:** Treat a "before" probe error as fatal to verification: if `freeBefore` could not be read, force `Verified=false`/record the probe error in the reason rather than defaulting to 0. In `waitReclaim`, only count a delta when *both* endpoints were successfully measured.

### [HIGH] Live-process map leaks an entry (and closed log file) on the STARTING-transition failure path
- **File:** `internal/engines/llamacpp/runner.go:200-211`
- **Issue:** Every other Start failure branch calls `r.live.remove(spec.InstanceID)` (health-fail 222-223, healthy-transition-fail 234-235), but the STARTING `store.Transition` error branch (208-211) calls `p.Kill()` + `r.failStart(...)` and returns *without* `r.live.remove`. The entry stored at line 196 stays in `r.live.m` forever, retaining the ring buffer and a now-closed `logFile`. A later `Stop(sameID)` then finds `ok==true`, calls `live.proc.Stop` on the dead process and `live.logFile.Close()` a second time.
- **Impact on Linux+NVIDIA target:** Map/goroutine-adjacent leak per failed start; the double-`Close()` is benign only because `CappedLogFile.Close` is idempotent-tolerant today — but the stale live entry is a real lifecycle-tracking bug.
- **Suggested fix:** Add `r.live.remove(spec.InstanceID)` to the STARTING-transition failure branch.

### [MEDIUM] Stop verification measures global GPU free VRAM with no concurrency guard
- **File:** `internal/engines/llamacpp/stop.go:114-147`; `internal/engines/llamacpp/runner.go:42-51`
- **Issue:** `waitReclaim` attributes *all* rise in global free VRAM (GPU 0 only) to this one stop. If another instance is loading during the window this stop can falsely `timedOut`; if another is *also* stopping it can falsely `Verified`. Nothing in the adapter serializes stops/starts, and it hard-codes `GPUs[0]`.
- **Impact on Linux+NVIDIA target:** On a busy single box the VRAM-delta gate can be fooled in both directions; on a multi-GPU Linux host the wrong device is measured.
- **Suggested fix:** Ensure residency serializes load/unload around the measured window (or use NVML per-PID accounting); key the probe to the instance's actual GPU rather than index 0.

### [MEDIUM] DRAINING is followed by an immediate signal; no in-flight-request drain in the adapter
- **File:** `internal/engines/llamacpp/stop.go:51-71`; `internal/instances/proc/proc_windows.go:139-165`, `proc_other.go:76-96`
- **Issue:** `Stop` marks DRAINING then immediately calls `proc.Stop`, which sends SIGTERM (Linux) / `taskkill` (Windows) at once and hard-kills after a 5s *process-exit* grace. That 5s is time for the process to exit, not for active generations to complete. The adapter delegates the never-kill-active gate entirely to the residency layer.
- **Impact on Linux+NVIDIA target:** A direct admin `Stop` (or any caller trusting the DRAINING label) will cut active requests.
- **Suggested fix:** Have `Stop` optionally poll llama-server `/slots`/`/metrics` before signaling, or clearly restrict `Stop` to callers that have already drained and document it.

### [MEDIUM] Kill-by-PID on a reattached instance has a PID-reuse hazard
- **File:** `internal/engines/llamacpp/stop.go:65-71`; `internal/instances/proc/proc_other.go:114-119`
- **Issue:** When no live handle exists (reattached instance), Stop calls `proc.KillTree(inst.PID)` → on Linux `syscall.Kill(-pid, SIGKILL)`, signaling the whole process *group*. If that PID was recycled between reconcile and stop, this SIGKILLs an unrelated group. Unlike `health.go`'s reconcile path, Stop performs no `/props` identity re-check first.
- **Impact on Linux+NVIDIA target:** Linux recycles PIDs aggressively; low probability, high blast radius.
- **Suggested fix:** Re-verify process identity (via `/props`, or a start-time/cmdline check) before `KillTree`.

### [MEDIUM] `--no-webui` is appended unconditionally, not capability-gated
- **File:** `internal/engines/llamacpp/runner.go:260-263`
- **Issue:** The adapter probes capabilities (`has_fit`, `has_flash_attn`, `has_tq3`) but hard-appends `--no-webui` regardless of whether the target binary supports it. An older llama-server rejects unknown args and exits non-zero.
- **Impact on Linux+NVIDIA target:** A self-built/packaged llama-server without `--no-webui` fails at startup → FAILED. Fails safe (no OOM) but is an avoidable false-negative.
- **Suggested fix:** Probe `--no-webui` in `parseCapabilities` and only add it when detected.

### [LOW] `-ngl all` / `-ngl auto` is build-version-sensitive (compat-owned)
- **File:** originates in `internal/compat/flags.go:72-76`; consumed via `StartSpec.Flags`.
- **Issue:** `all`/`auto` as `-ngl` values are a relatively recent llama.cpp addition; classic `-ngl` accepts only an integer. Verified against the *local* reference binaries, but an arbitrary llama-server on the Linux target may predate keyword support.
- **Impact on Linux+NVIDIA target:** Fails safe (won't start → FAILED, not an OOM), but a portable deployment could hit it.
- **Suggested fix:** compat should gate the `all`/`auto` keyword on a `--help` probe and fall back to a large integer for older builds.

### [LOW] Miscellaneous
- `runner.go:252` — `-c` de-dup checks only `-c`, not the `--ctx-size` alias. Cosmetic today.
- `health.go:113-117` — `sameModel` compares basenames only; two different GGUFs sharing a filename would falsely match. Acceptable given conservative-reattach, but worth a comment.
- `internal/instances/proc/proc_other.go` — this is the *actual* Linux stop/kill path yet is framed as a CI "fallback." Given Linux+NVIDIA is the primary target, it deserves first-class test coverage. `otherProcess.Stop` returns `nil` unconditionally even if the process is still alive after SIGKILL+2s.

### Overall assessment
The detection/probing half (`llamacpp.go`, `exec.go`, `health.go`) is careful and genuinely cross-platform: PATH/explicit-path resolution, `os.PathListSeparator`, case-insensitive PATH handling, capability grepping, and identity-via-`/props` reconcile are all done well. The mutation half is architecturally right — it enforces the VRAM-delta gate via NVML rather than trusting an HTTP 200, uses Job Objects / process-group kills for tree teardown, and captures stderr into a ring buffer + capped log. The two HIGH findings (swallowed before-probe → fabricated VERIFIED stop; live-map leak on one Start failure) are the ones to fix before relying on this on Linux. Nothing forks/patches engines or links CUDA, so the ADR hard-constraints are respected. **Test gaps:** no test injects a failing `VRAMProbe` and none drives the Start failure paths — adding those two catches both HIGH findings.

---

## vLLM adapter (reviewed by Opus)

### [HIGH] Stop marks instance STOPPED even when the container was not confirmed gone
- **File:** `internal/engines/vllm/runner.go:356-372` (reason at `:424-427`)
- **Issue:** `stopDocker` issues `docker stop` + `docker rm -f` (both errors intentionally ignored) and then, regardless of the verify result, unconditionally transitions the row to `StateStopped` at line 363. `waitContainerGoneAndReclaim` can return `gone == false` (both stop and rm failed) and the code still records STOPPED. The reason string honestly says "container still listed… verify manually," but the *state* the rest of the system reads is STOPPED.
- **Impact on Linux+NVIDIA target:** An orphaned vLLM container keeps holding its `gpu_memory_utilization × VRAM` allocation (0.9 of the card by default) while LocalHub believes the VRAM is free. Residency will then admit another model against phantom-free VRAM → OOM.
- **Suggested fix:** When `!gone`, do not transition to STOPPED. Keep the instance in a non-terminal/FAILED-to-stop state, or retry `rm -f`, so residency accounting still treats the VRAM as occupied.

### [MEDIUM] Failed baseline VRAM probe silently produces a bogus "VERIFIED reclaimed"
- **File:** `internal/engines/vllm/runner.go:346-349`, `:388-402`
- **Issue:** `freeBefore, _ = r.vram.FreeVRAMMiB(ctx)` discards the error. If the baseline probe fails, `freeBefore` stays `0`; `freeAfter` then jumps to real free VRAM and `freed = freeAfter - 0` trivially clears `bar`, returning `verified=true` with a fabricated delta. (Same class as the llama.cpp HIGH.)
- **Impact on Linux+NVIDIA target:** A transient NVML/nvidia-smi failure at stop time yields a confident-but-false "VRAM reclaimed" verdict.
- **Suggested fix:** If the baseline read errors, treat verification as skipped/unverified rather than substituting 0; only assert a delta when both reads succeeded.

### [MEDIUM] `--runtime nvidia` is fragile on modern nvidia-container-toolkit installs
- **File:** `internal/engines/vllm/command.go:80-82`
- **Issue:** `DockerRunArgs` always emits both `--runtime nvidia` and `--gpus all`. On many current Linux installs only the CDI / `--gpus` path is configured and the legacy `nvidia` runtime is not registered in `daemon.json`; `docker run` then fails with `unknown or invalid runtime name: nvidia`. Hardcoded with no way to drop it (ExtraDockerArgs can only *add*).
- **Impact on Linux+NVIDIA target:** On the primary deployment target this can make every docker-mode start fail.
- **Suggested fix:** Make `--runtime nvidia` optional (config toggle) or rely on `--gpus all` alone; keep `--runtime nvidia` opt-in for legacy hosts.

### [MEDIUM] Native Linux `vllm serve` path is a stub — docker is the only working launch mode
- **File:** `internal/engines/vllm/runner.go:259-282`
- **Issue:** `startSubprocess` returns `*NotImplementedError` on *every* OS including Linux — it never spawns `vllm serve` (`SubprocessArgs` is built only for the reason string). Deliberate for the P4.4 contract level, but the answer to "does native Linux vLLM work?" is: no, not without docker. Stop-side subprocess is also unimplemented.
- **Impact on Linux+NVIDIA target:** Bare-metal Linux deployments wanting `vllm serve` (no container) have no functioning path; they must use docker mode.
- **Suggested fix:** Flag that subprocess mode is unimplemented; make startup logging/config validation clear that only docker/attach are runnable today.

### [MEDIUM] No `--served-model-name` — served id is the full repo/path
- **File:** `internal/engines/vllm/command.go:87-93`, `:124-129`
- **Issue:** The model is passed as `--model <HF repo id or local path>` with no `--served-model-name`. vLLM then advertises the model under that exact string and requires requests to match it; LocalHub's catalog `model_id` is a different identifier. Health check is unaffected (`probeModels` only checks `len(data) > 0`), but the proxy must route on whatever name vLLM serves.
- **Impact on Linux+NVIDIA target:** If the proxy forwards the catalog `model_id`, vLLM returns 404 "model not found."
- **Suggested fix:** Pass `--served-model-name <catalog model id>` so the served name is deterministic.

### [LOW] No default HF cache mount / HF_TOKEN — re-download per start, gated models fail
- **File:** `internal/engines/vllm/command.go:72-93`
- **Issue:** Without operator-supplied `ExtraDockerArgs`, nothing mounts `~/.cache/huggingface` and no `HF_TOKEN` is passed. Because stop does `rm -f`, each start re-downloads weights into the ephemeral layer, and gated repos 401.
- **Impact on Linux+NVIDIA target:** Slow cold starts and outright failure for gated models unless the operator remembers the mount/env.
- **Suggested fix:** Document/default a HF cache mount and optional `HF_TOKEN` passthrough for docker mode.

### [LOW] `docker ps --filter name=` is a substring match, not exact
- **File:** `internal/engines/vllm/command.go:106-108`; consumed at `runner.go:416-422`, `health.go:69-70`
- **Issue:** Docker's `name` filter is substring/regex; `name=localhub-vllm-i1` also matches `...-i10`. Instance IDs are UUIDs so real collisions are effectively impossible.
- **Suggested fix:** Anchor the filter or match the exact name in the `{{.Names}}` output.

### Overall assessment
The command-building layer is clean and faithfully matches the current vLLM docs invocation (`--model`, `--gpu-memory-utilization`, `--max-model-len`, `--dtype`, positional model for `serve` are all valid and current), and the DockerClient seam keeps tests honest. The health/readiness design (600s cold-start allowance, `/health` + `/v1/models` cross-check, per-request timeouts, ticker cleanup) is sound and appropriately conservative. No Windows path assumptions leak into container mounts — mounts are entirely config-driven. The two things that matter most for the target are the stop path marking STOPPED without confirming the container is gone and the baseline-probe false "verified" — both can leave real VRAM occupied while LocalHub believes it is free. **Test gaps:** no test for stop where the container does *not* go away, and none for a failing baseline VRAM probe.

---

## Cross-platform, process & hardware layer (reviewed by Opus)

The non-Windows path is **genuinely implemented, not stubbed** — `proc_other.go` does real `setpgid` + SIGTERM→SIGKILL escalation, `sys_other.go` reads `/proc/meminfo` and `unix.Statfs`, `nvidia_smi.go` is platform-neutral. The gaps are robustness (driver/format variance, PID reuse, multi-GPU) and Windows-shaped defaults, not missing implementation.

### [HIGH] nvidia-smi query poisoned by a single unsupported field (`compute_cap`) on older Linux drivers
- **File:** `internal/hardware/nvidia_smi.go:22, :98-118`
- **Issue:** The probe requests all ten fields in one `--query-gpu` call including `compute_cap`. `nvidia-smi` fails the *entire* query with a non-zero exit if any single field is unknown. `compute_cap` was only added around driver 465 (2021); Linux deployments commonly pin older LTS branches (450.x / CUDA 11) where it's rejected. `execNvidiaSmi` returns an error, and `probe.Snapshot` degrades to **zero GPUs**.
- **Impact on Linux+NVIDIA target:** On an older-driver box the GPU list silently goes empty; admission refuses every model and the dashboard shows no GPU. `scripts/nvidia-preflight.sh:10` queries a *different, smaller* field set without `compute_cap`, so preflight passes while the daemon fails.
- **Suggested fix:** Fall back to a reduced field set on non-zero exit (name/memory/driver/temp/util/power) before declaring zero GPUs; parse `compute_cap` from a separate best-effort call.

### [HIGH] Reconcile kill uses negative-PID group-kill with PID-reuse blast radius
- **File:** `internal/instances/reconcile.go:125`, `internal/instances/proc/proc_other.go:114-119`
- **Issue:** `reconcileOne` calls `r.kill(inst.PID)` (→ `syscall.Kill(-pid, SIGKILL)`) whenever a recorded PID is alive but the health/identity probe fails. After a daemon crash + restart the PID can be reused by an unrelated process; the identity gate is checked against the recorded port but the *kill target is the PID*, and `-pid` signals the whole group. If the reused PID is a group leader (shells, daemons under `setsid` — common on Linux), LocalHub SIGKILLs an unrelated group.
- **Impact on Linux+NVIDIA target:** A restart-time reconcile can kill unrelated host processes/services.
- **Suggested fix:** Confirm process identity beyond "PID alive" (own the recorded port via `/proc/net/tcp`, or match `/proc/<pid>/exe`/`cmdline`); prefer killing the specific PID when identity can't be confirmed; record + re-check a start-time token (`/proc/<pid>/stat` field 22).

### [MEDIUM] Admission and HW summary only ever look at GPU 0; nvidia-smi index ≠ CUDA index
- **File:** `cmd/localhub/main.go:661-703` (residencyVRAMProbe/GPUProbe), `:185` (vLLM), `internal/hardware/nvidia_smi.go:164-172`
- **Issue:** Every VRAM/temp/util probe hardcodes `snap.GPUs[0]`. Multi-GPU boxes are a primary Linux shape. The parser trusts nvidia-smi's `index` (PCI-bus order), while engines using CUDA order (default fastest-first unless `CUDA_DEVICE_ORDER=PCI_BUS_ID`) may load onto a different physical device — so admission can read free VRAM from a different GPU than the engine fills.
- **Impact on Linux+NVIDIA target:** On a 2+ GPU box, admission mis-reads capacity → can admit onto a full card (OOM) or refuse when another is free. No cross-card total accounting.
- **Suggested fix:** Make the target GPU configurable / sum-or-select across `snap.GPUs`; document/pin `CUDA_DEVICE_ORDER=PCI_BUS_ID`. Single-GPU boxes unaffected.

### [MEDIUM] Default config is Windows-shaped; a fresh Linux boot finds nothing
- **File:** `internal/config/config.go:246-285` (Default), `internal/hardware/fake.go:68`
- **Issue:** `Default()` bakes in `K:\projects\llm_models\models`, a `...\llama-server.exe` absolute candidate, and a `K:\Softwares\...\CUDA\v12.5\bin` ExtraPath. On Linux with no config file the model dir doesn't exist (empty inventory) and the `.exe` candidate is dead. It limps because `BinaryPath: "llama-server"` (bare, PATH-resolved) is folded in first, but the out-of-box experience contradicts the "5-minute install, zero edits" promise.
- **Impact on Linux+NVIDIA target:** No models discovered and confusing Windows paths until the operator writes a full config.
- **Suggested fix:** OS-appropriate defaults at runtime (XDG for data, `/var/lib/localhub` for a service, no `.exe`, no `K:\`), or ship a documented Linux example config.

### [MEDIUM] nvidia-smi `[Not Supported]` / MIG memory fields degrade to 0, reading as "no VRAM"
- **File:** `internal/hardware/nvidia_smi.go:201-217` (isNA/atoiOr)
- **Issue:** `isNA` recognizes only `""`, `N/A`, `[N/A]`. It doesn't recognize `[Not Supported]`, which nvidia-smi emits for some fields (notably `memory.free`/`memory.used` on MIG-enabled A100/H100). For memory the parse-failure default is **0** — indistinguishable from a full/empty card. MIG geometry isn't enumerated (`--query-gpu` alone can't).
- **Impact on Linux+NVIDIA target:** On a MIG datacenter box, `VRAMFreeMiB`/`VRAMTotalMiB` read 0, so admission refuses everything.
- **Suggested fix:** Add `[Not Supported]` to `isNA`; use a sentinel (-1) so admission can tell "unknown" from "full"; add a `--query-mig`/`-i` path or document MIG as unsupported.

### [LOW] Linux CPU model/core reporting is a placeholder
- **File:** `internal/hardware/sys_other.go:23-27`
- **Issue:** `CPU.Model` is hardcoded to `runtime.GOARCH + " CPU"` and physical cores equal logical; no `/proc/cpuinfo` parse. Cosmetic.
- **Suggested fix:** Parse `/proc/cpuinfo` for parity.

### [LOW] `CUDAVersion` field is declared but never populated
- **File:** `internal/hardware/hardware.go:64-67`
- **Issue:** `GPU.CUDAVersion` (`json:"cuda"`) is always empty — no code path sets it (it's a header field, not in the per-GPU CSV).
- **Suggested fix:** Parse it from a separate `nvidia-smi --query` or drop the field.

### [LOW] `!windows` build tag lumps darwin with linux; `/proc` reads fail on macOS dev machines
- **File:** `internal/hardware/sys_other.go:1, :29-30`
- **Issue:** `sys_other.go` is `//go:build !windows` but is Linux-specific (`/proc/meminfo`). On macOS RAM probing errors every snapshot. No effect on the target.
- **Suggested fix:** Rename to `sys_linux.go` (`//go:build linux`) and add a darwin stub, or document the limitation.

### [LOW] Default `data.db_path` is relative; brittle under a systemd service
- **File:** `internal/config/config.go:247`, `cmd/localhub/main.go:516-521`
- **Issue:** `DBPath` defaults to `./localhub.db` and log location derives from `filepath.Dir(DBPath)`. Under systemd without an explicit `WorkingDirectory`, cwd is `/`, so the daemon writes DB and logs under `/`.
- **Impact on Linux+NVIDIA target:** A default systemd deployment (the stated primary target) may fail to open the DB or scatter state.
- **Suggested fix:** Default to an absolute service path (XDG or `/var/lib/localhub`); ensure `examples/localhub.service` sets `WorkingDirectory` + `StateDirectory`.

### Positive notes (not findings)
- Process-group kill correctly survives daemon death for orphan reaping (`Setpgid` at spawn + `Kill(-pid, ...)`).
- Graceful SIGTERM→(5s)→SIGKILL escalation, zombie reaping via a background `cmd.Wait()` goroutine, and `signal 0` liveness probing are correct POSIX idioms.
- `filepathDir` and `expandHome` handle both `/` and `\`; log dirs use `filepath.Join`. No hardcoded separators in runtime path-join code.
- `nvidia-smi` discovery uses `exec.LookPath` with a bare name (no `.exe` assumption).

### Overall assessment — verdict: **mostly ready to run on Linux+NVIDIA**
The primitives are real Linux implementations. Blockers are robustness under real-world variance, not missing code: `compute_cap` blanks the GPU list on older LTS drivers (HIGH), the reconcile group-kill has a PID-reuse blast radius (HIGH), and multi-GPU/MIG/`[Not Supported]` plus Windows-shaped defaults will surprise datacenter and fresh-install deployments (MEDIUM). It will run and manage llama.cpp processes correctly on a single modern-driver single-GPU Linux box today; fix the two HIGH items before trusting it on older-driver or multi-GPU/MIG servers, and give the default config Linux paths for systemd.

---

## Core orchestration & residency (reviewed by Opus)

### [HIGH] Never-kill-active gate has a TOCTOU: in-flight increment is not synchronized with the eviction decision+execution
- **File:** `internal/residency/admission.go:474-539` (PickVictims + eviction loop under `m.mu`); `internal/core/proxy.go:90-92` (`IncrementActive` with no residency lock); `internal/residency/eviction.go:84-97`
- **Issue:** `admitLocked` classifies victims as idle via `PickVictims` (reads `tracker.ActiveCount`) and executes `m.evictor.Evict` while holding `m.mu`. But `BeginInstanceRequest` calls `IncrementActive(id)` **without** taking `m.mu`. A completion arriving after a victim is classified idle but before/during its `Evict` (a window spanning the multi-second stop of earlier victims) increments the counter on an instance already committed to eviction. `Evict` → `llamacpp.Stop` marks DRAINING and immediately kills; it never re-reads `ActiveCount`.
- **Impact:** Violates the non-waivable "never kill an instance with active in-flight requests" bar under a real (narrow) race.
- **Suggested fix:** Re-check `ActiveCount(victim.ID) == 0` immediately before each `Evict` inside the locked loop; or have `IncrementActive` participate in the manager lock (or a per-instance "eviction-committed" flag `BeginInstanceRequest` refuses against).

### [HIGH] Drain-timeout semantics (EvaluateDrain) are never wired; an explicit stop marks DRAINING then kills immediately
- **File:** `internal/core/instances.go:506-545` → `435-491` → `internal/engines/llamacpp/stop.go:35-108`; `internal/residency/support.go:188-211` (EvaluateDrain, only referenced by tests)
- **Issue:** `EvaluateDrain` (wait until `ActiveCount==0` or `DrainTimeout`) has **no production caller** (only `support_test.go`). `StopInstanceAsync` transitions to DRAINING for the audit trail, then the background goroutine calls `StopInstance` → `llamacpp.Stop`, which after a 5s *process* grace hard-kills — never consulting the in-flight counter. So DELETE on a busy instance terminates active streams; DRAINING is cosmetic.
- **Impact:** The documented "DRAINING, not killed" contract is not honored for user-initiated stops.
- **Suggested fix:** In the `StopInstanceAsync` goroutine, poll `EvaluateDrain(...)` and only proceed to kill when it returns `Stop:true`. This is the intended use of the already-written function.

### [MEDIUM] Manager mutex held across the slow probe + Evict sequence, blocking admission and read-only /api/queue
- **File:** `internal/residency/admission.go:328-331`, `449-539`, `763-833`
- **Issue:** `admitLocked` performs `FreeVRAMMiB` reads and one-at-a-time `Evict` calls (each bounded by ~30s VRAM-reclaim poll) while holding `m.mu`. `QueueRows` (GET /api/queue), `IsWaiting`, `Cancel`, `Admit` all serialize on the same lock, so a background eviction can wedge the queue endpoint and new admissions for tens of seconds.
- **Impact:** Availability: the observability/queue surface stalls exactly under memory pressure.
- **Suggested fix:** Snapshot the victim plan under the lock, then run `Evict` + probe re-read with the lock released (re-validating on re-acquire), or give queue-read paths a separate RWMutex/snapshot.

### [MEDIUM] In-flight counter only tracks proxied requests; direct-to-engine-port traffic is invisible to the eviction gate
- **File:** `internal/core/proxy.go:19-24, 87-108`; `internal/residency/eviction.go:84-97`
- **Issue:** `ActiveCount` is only incremented by `BeginInstanceRequest` (the relay / `/v1` proxy). A client hitting the instance's `base_url` loopback port directly is not counted, so eviction can classify a busy instance as idle and stop it.
- **Impact:** Eviction (and the drain-timeout, once wired) can kill a busy instance whose traffic bypassed the proxy.
- **Suggested fix:** Don't advertise `base_url` to clients, or scrape the engine's own busy-slots metric (llama.cpp `/metrics` `n_busy_slots`, vLLM `/metrics`) as a secondary signal.

### [MEDIUM] Relay has no connect / first-byte / idle timeout; a hung upstream pins the instance indefinitely
- **File:** `internal/proxy/proxy.go:152-182` (`&http.Client{Timeout: 0}`)
- **Issue:** Zero (infinite) client timeout, relying solely on `r.Context()`. If the engine stalls mid-stream while the client stays connected, `resp.Body.Read` blocks forever; the caller holds the in-flight handle for the full relay, so the counter never decrements and eviction never reclaims that (dead) instance.
- **Impact:** A wedged upstream leaks an in-flight count and permanently protects a hung instance.
- **Suggested fix:** Set `http.Transport.ResponseHeaderTimeout` and an inter-chunk idle deadline; keep the overall streaming timeout unbounded but bound time-to-first-byte and inter-chunk gaps.

### [LOW] Lost drain wakeup when a pass is already in flight
- **File:** `internal/residency/drain.go:104-120`; `internal/core/instances.go:462,476,489`
- **Issue:** `DrainQueue` returns immediately if `draining` is true. A stop that frees exactly enough can fire `drainAsync` while a prior pass (which read free-VRAM before the stop settled) is still running; that trigger is discarded with no re-run latch, so freed capacity waits for the periodic tick (~10s).
- **Suggested fix:** Add a `rerunRequested` bool and loop the pass once more before clearing `draining`.

### [LOW] Reservation leak in the (retired) Ollama start path
- **File:** `internal/core/ollama_instances.go:74-130` (no `ReleaseReservation`)
- **Issue:** llama.cpp and vLLM paths `defer ReleaseReservation`; `startOllamaInstance` doesn't. A never-released reservation permanently reduces admission supply. Currently unreachable (StartInstance rejects ollama/retired at `instances.go:158-160`), hence LOW.
- **Suggested fix:** Add the `defer`, or delete the unreachable path.

### [LOW] Relay forwards only Content-Type from the upstream response
- **File:** `internal/proxy/proxy.go:159-163`
- **Issue:** Other upstream headers (`Cache-Control`, `Retry-After` on a 503, etc.) are dropped. Some clients/proxies expect `Cache-Control: no-cache` on SSE.
- **Suggested fix:** Copy a small allow-list of streaming-relevant headers, or the full set minus hop-by-hop.

### Correctness-bar verification
1. **stop/evict verifies VRAM freed via NVML delta** — **MET at the residency layer.** `waitReclaim` records the honest delta; admission re-reads the probe after each eviction (admission.go:520-528) and never trusts the adapter's `FreedMiB` for the load gate. *Caveat:* the adapters swallow the before-probe error (see llama.cpp/vLLM sections) — fix those so the adapter-level verdict can't be fabricated.
2. **Never kill active (DRAIN, not kill)** — **PARTIALLY MET / at risk.** Auto-eviction correctly excludes active/pinned/DRAINING/non-HEALTHY (eviction.go:119-138), but three gaps weaken it: the classify→evict TOCTOU, the unwired drain-timeout, and untracked direct-to-port traffic.
3. **False "fits" that OOMs is a bug; prefer conservative** — **MET at residency.** Additive `SafetyMarginMiB`, per-eviction re-read with degrade-to-Queue, reject-rather-than-guess, vLLM budgeted at `util × total`. *Caveat:* the compat calibration/util findings can still feed an optimistic estimate in.
4. **No cross-engine preemption / fair-share (ADR-001)** — **MET.** Coarse whole-model load/unload; free VRAM always re-measured live; one idle victim at a time.
5. **Every verdict/error carries a reason** — **MET.** `AdmitResult.Reason` on every branch, `QueuedError.Reason`, `QueueFullError.Reason`; the OpenAI dialect preserves the reason in `message`.

### Overall assessment
The anti-OOM machinery (bars 1, 3, 4) is genuinely well built. The real weakness is the never-kill-active bar (bar 2): the in-flight signal is checked outside the eviction lock (TOCTOU), the documented drain-timeout is written but never wired, and only proxied traffic is counted. Each can terminate a live stream, which the project classifies as non-waivable. Prioritize wiring `EvaluateDrain` and closing the classify→evict race before the next storm test. Cross-engine flows are consistent (llama.cpp and vLLM both route through the one `admitGate`); the divergent Ollama path is retired/dead.

---

## Compatibility estimator (reviewed by Sonnet)

### [CRITICAL] Calibration override ignores requested context, KV type, GPU, and driver
- **File:** `internal/compat/gguf/gguf.go:262-275` (`estimateWith`), `internal/compat/calibration.go:56-70` (`ObservedPeak`)
- **Issue:** `ObservedPeak(modelKey)` returns a bare MiB number keyed only by `modelKey` — it doesn't take or check the requested context length, KV cache type, GPU, or driver, even though the `observed_peaks` row stores all of them. In `estimateWith`, if the store has *any* row for the key it's returned unconditionally as `ConfidenceExact` **regardless of `opts.ContextLen`**. `ggufMaxContext` (`gguf_path.go:211-237`) probes every candidate (4K/8K/16K/32K) through the same gate, so once a peak exists every candidate returns the identical number and the loop reports the largest (32768) as "fits."
- **Impact:** A peak measured once at ctx=4096 with f16 KV on GPU A is replayed as authoritative "fits" at ctx=32768, a different KV-quant, or after a GPU/driver change. Because KV cache scales with context, this is exactly the "false `runs` that OOMs" the correctness bar forbids.
- **Suggested fix:** Make `ObservedPeak` accept the requested `ctx`/`KVCacheType` and only return the calibrated value when they match (or are conservative relative to) the recorded row; gate on GPU/driver equality before trusting a peak as "exact," else fall back to the computed estimate.

### [HIGH] Recommended vLLM `--gpu-memory-utilization` is computed only against TOTAL VRAM, never against free VRAM
- **File:** `internal/compat/safetensors/estimate.go:230-269` (`GPUMemUtil`), `internal/compat/safetensors_path.go:77`
- **Issue:** `GPUMemUtil` takes only `totalVRAMMiB`. `MinUtil` is `RequiredMiB/TotalVRAMMiB`, and when below vLLM's default it's bumped up to 0.90 with no check against free VRAM. The "does it fit" verdict correctly gates on free VRAM, but the concrete `--gpu-memory-utilization` handed to `vllm serve` is independent of that. `docs/compatibility-engine.md:91` itself says the fit question should be "`gpu_memory_utilization × free_VRAM`" — the code only multiplies by TOTAL.
- **Impact:** On a box already hosting other resident engines, a small model can pass the free-VRAM fit check yet be launched with `--gpu-memory-utilization 0.90`, telling vLLM it may claim ~90% of the *whole* card. vLLM's memory profiling then fails to start or fights the other engine — a real OOM the verdict said would "run."
- **Suggested fix:** Thread `free` VRAM into `GPUMemUtil` and require `RecommendUtil * total` to fit inside actually-free VRAM (with safety factor), degrading to `runs_degraded`/`wont_run` with a reason when the safe ceiling is too low.

### [MEDIUM] Fixed 2 GiB vLLM overhead does not scale with requested batch/concurrency
- **File:** `internal/compat/safetensors/constants.go:37-45`, `estimate.go:141-143`
- **Issue:** `overheadMiB` is a hard-coded 2.0 GiB regardless of `Params.Batch`. CUDA-graph capture and activation buffers scale with `max_num_seqs`; the KV term scales with batch but overhead doesn't.
- **Impact:** At high concurrency the overhead component under-counts, eroding the margin.
- **Suggested fix:** Document that the constant is only validated for small batch (≤8), or add a per-extra-sequence increment once real data is available.

### [LOW] Fallback KV byte-per-element for q8_0 rounds slightly optimistic
- **File:** `internal/compat/gguf/fallback.go:118-131`
- **Issue:** On the file-size-only fallback path, q8_0 KV is priced at a flat 1 byte/element; real q8_0 is ~1.0625. The package's own `iswa.go` (`kvBytesPerElemX8`) correctly rounds q8_0 to 1.125 elsewhere.
- **Impact:** Minor — only the low-confidence fallback path's KV term; the weights term already carries a 5% margin + 500 MiB overhead.
- **Suggested fix:** Reuse `kvBytesPerElemX8`'s rounding in `kvBytesForType`, or comment the intentional coarseness.

### Overall assessment
The estimator is unusually disciplined for a from-scratch VRAM sizer: GQA-aware KV math is correct and consistent, iSWA/Gemma and MoE offload sizing err conservative and decline rather than guess, every verdict is structurally forced through `newVerdict`/`newAlternative` so an empty `Reason` is impossible, and the fallback GGUF header reader bounds string/array lengths so a malformed file can't panic or over-allocate. The two issues that matter are the same theme the project cares about — a confident "runs" that doesn't reflect *live* state: the context/hardware-blind calibration lookup, and the total-VRAM-only vLLM util recommendation. Fix both before relying on calibration and multi-engine residency in production.

---

## API layer & proxy surface (reviewed by Sonnet)

### [CRITICAL] WebSocket accepts any Origin, then dispatches state-mutating JSON-RPC methods
- **File:** `internal/api/ws.go:38-40` (`InsecureSkipVerify: true`), `internal/api/rpc.go:88-103`, `internal/jsonrpc/dispatcher.go:150-199` (registers `instances.start/stop`, `models.rescan`, `jobs.submit/cancel`, `queue.cancel`)
- **Issue:** `InsecureSkipVerify: true` disables the handshake Origin check, so `GET /api/ws` accepts a connection from *any* web page (browsers don't apply CORS/SOP to raw `new WebSocket(...)` — only the server-side Origin check does). The justifying comment ("loopback-only daemon; same-origin gate") is wrong on both counts: the same-origin check is exactly what's disabled, and the daemon isn't always loopback (`admin_token` defaults to `null`, Tailscale Serve is the documented remote path). The read loop feeds every inbound frame straight into the dispatcher's mutating methods with no per-call auth.
- **Impact:** With the default config, visiting a malicious page lets its JS open `ws://127.0.0.1:<port>/api/ws` and drive `instances.start`/`jobs.submit`/`models.rescan` — a drive-by on a localhost service, zero interaction. Over a tailnet with `admin_token` unset, any tailnet device is equally exposed. When `admin_token` IS set, auth middleware runs first and mitigates this path, but the Origin bypass remains a latent hole.
- **Suggested fix:** Set `OriginPatterns` (or validate `Origin`/`Host`) instead of blanket `InsecureSkipVerify`; at minimum require Origin to match the bind host. Independently, gate mutating methods behind the admin-token check even over the authenticated WS.

### [CRITICAL] JSON body handlers never check Content-Type — classic CSRF via "simple" cross-site requests
- **File:** `internal/api/instances.go:71-79`, `internal/api/supply.go:25-41`, `internal/api/handlers.go:107-114`, `internal/proxy/proxy.go:84-100`
- **Issue:** None of these handlers check `Content-Type` before decoding JSON. Browsers only preflight cross-origin requests with a non-"simple" Content-Type; a `text/plain` (or form-urlencoded) POST is CORS-safelisted and sent cross-origin with no preflight. Since the handlers don't validate the declared type, a plain HTML `<form>` or a third-party `fetch()` can POST a JSON body to `/api/instances`, `/api/downloads`, `/api/compat/preview`, or `/v1/chat/completions` and have it acted on as if from the dashboard.
- **Impact:** Under the default trust model with `admin_token` unset, any page the victim's browser loads can spawn a GPU instance, start a download from an attacker-chosen URL (a CSRF-to-SSRF chain — the body's `hf_repo`/`gguf_url` are attacker-controlled), or submit inference — purely by the victim visiting a page. The localhost bind doesn't stop the victim's own browser.
- **Suggested fix:** Reject requests whose `Content-Type` isn't `application/json` (forces a CORS preflight that fails without explicit `Access-Control-Allow-Origin`). Longer-term, require a non-safelisted custom header (`X-Requested-With`) for state-changing endpoints.

### [MEDIUM] Dashboard "live" cookie is the raw admin token, not an independent session credential
- **File:** `internal/api/middleware.go:20-22`, `internal/api/dashboard_auth.go:16-26`
- **Issue:** `dashboardLiveCookieValue` is `base64(adminToken)` — trivially reversible — set directly as the cookie. The cookie *is* the master credential, not a revocable session token.
- **Impact:** Any leak (XSS, reverse-proxy logs, extension, shared machine) discloses the actual `admin_token` in reversible form; no way to invalidate just the session without rotating the token and breaking every API client.
- **Suggested fix:** Derive the cookie from an HMAC of the token + a server-generated random session id (or a random server-tracked session token), so it's independently revocable.

### [MEDIUM] No request body size limit on most JSON-decoding endpoints
- **File:** `internal/api/instances.go:73`, `internal/api/supply.go:27`, `internal/api/handlers.go:109`, `internal/proxy/proxy.go:84-100`
- **Issue:** Only `POST /api/rpc` caps its body (1 MiB). Every other handler decodes with no `http.MaxBytesReader`, and `http.Server` sets no body cap (only `ReadHeaderTimeout`).
- **Impact:** A large body is read fully into memory before validation, on a control-plane process managing GPU residency — a memory-exhaustion/DoS vector, worse given the default no-auth posture.
- **Suggested fix:** Wrap `r.Body` in `http.MaxBytesReader` on every handler that buffers a body.

### [LOW] Graceful shutdown does not cover WebSocket connections
- **File:** `internal/api/server.go:230-255`
- **Issue:** `http.Server.Shutdown` doesn't close/wait for hijacked (WebSocket) connections; the `Serve` doc comment overstates what happens to live `/api/ws` connections.
- **Suggested fix:** Track open WS connections and send a close frame / cancel contexts on shutdown.

### [LOW] Fragile string-matching for HTTP error classification
- **File:** `internal/api/instances.go:232-245, 134-138, 157-158`
- **Issue:** Handlers classify core errors into status codes by `strings.Contains`/`HasPrefix` on `err.Error()` text rather than typed sentinels (the codebase already has the pattern in `core.QueuedError` via `errors.As`).
- **Suggested fix:** Introduce typed sentinel errors and switch to `errors.Is`.

### [LOW] Inconsistent status-code mapping for equivalent read failures
- **File:** `internal/api/supply.go:65-77` (500) vs `81-95` (400); `100-108` (`handleReproduce`, any error → 404)
- **Issue:** Same class of manifest read maps to different status families; `handleReproduce` maps *every* error to 404, masking real internal errors.
- **Suggested fix:** Align to caller-error → 4xx, transport/read-error → 500; distinguish genuine not-found.

### Overall assessment
The proxy/relay plumbing is solid: `Relay` streams with a 32 KiB read/flush loop and no client timeout, and the middleware chain's `statusRecorder` forwards `Flush`/`Hijack`, so SSE/streaming aren't broken by logging/recovery middleware — no buffering-kills-streaming bug. The error envelope always carries a `reason`, and static-file serving has no path-traversal bug (`embed.FS` + `fs.ValidPath` reject `..`). The serious problems all stem from one gap: the trust model assumes the browser's same-origin machinery protects the daemon, but the WS Origin check is disabled and no JSON handler validates `Content-Type` — so in the default config a malicious page can drive real state changes. Close the Content-Type/CORS gap and the WS Origin check first.

---

## Model supply chain, inventory & persistence (reviewed by Sonnet)

### [HIGH] Download size cap is never enforced during streaming, only pre-flight or post-hoc
- **File:** `internal/manifest/download.go:67-160` (`Fetch`/`streamCopy`), `internal/manifest/downloader.go:56-177`
- **Issue:** `GuardConfig.CheckDownloadSize` is consulted only in `DownloadHF` (sums HF-reported metadata sizes *before* fetching, trusting declared sizes) and `DownloadGGUFURL` (checks `res.SizeBytes > hardCap()` only *after* the file has landed — the comment admits "an over-cap artifact is now ON DISK"). `streamCopy` has no awareness of `GuardConfig` and writes unbounded bytes.
- **Impact:** For an arbitrary user-supplied `gguf_url` (no size known up front), a large response can write far past the 8 GiB cap and 5 GiB headroom before the post-hoc check deletes it — on a box with only ~22-24 GB free this can wedge the disk (failed SQLite writes) before the safeguard fires.
- **Suggested fix:** Thread the cap into `Fetch`/`streamCopy` and abort the stream (delete the partial) the moment cumulative bytes exceed `hardCap()` (and, if known, the free-disk headroom).

### [MEDIUM] Path containment is lexical only — does not resolve symlinks
- **File:** `internal/manifest/paths.go:36-119`
- **Issue:** `absClean` uses `filepath.Abs` + `Clean` (lexical), never `EvalSymlinks`. `isWithin` does a string-prefix compare. If a path component under a model dir is a symlink pointing outside the root, `SafeJoin`/`ValidateWithin` report it as "contained" while the OS resolves the write outside the store.
- **Impact:** The containment guard defends against string traversal (`..`, absolute) but not symlink redirection — exploitable if a symlink is planted inside a model dir (compromise chaining, misconfiguration, shared dirs).
- **Suggested fix:** After the lexical check, `EvalSymlinks` the destination's existing parent chain and re-verify containment against the resolved root.

### [MEDIUM] Resumed direct-URL (gguf_url) downloads have no integrity validator
- **File:** `internal/manifest/download.go:67-160`, `internal/manifest/downloader.go:133-177`
- **Issue:** `Fetch` resumes a `.part` purely via `Range: bytes=<offset>-`, with no `If-Range`/ETag/Last-Modified. For `DownloadGGUFURL`, `expectedSHA` is `""` (no HF LFS hash for an arbitrary URL), so nothing catches a resume that spliced two different versions.
- **Impact:** If the remote content changes between the partial and the resume, the file silently combines old+new bytes into a corrupted GGUF with no hash to catch it.
- **Suggested fix:** Persist a validator (ETag/Last-Modified) with the `.part` and send it as `If-Range`; treat a `200` (or mismatch) as restart-from-scratch.

### [MEDIUM] "biggest weight file" selection in `DownloadHF` is broken — records the last file, not the biggest
- **File:** `internal/manifest/downloader.go:104-112`
- **Issue:** The condition `isWeightFile(f.Path) && (m.LocalPath == "" || res.SizeBytes > 0)` never compares to a running max — `res.SizeBytes > 0` is true for every non-empty file, so it fires on *every* weight file and overwrites `m.LocalPath`/`Format`/`Quantization` with whichever was processed **last**.
- **Impact:** `Manifest.LocalPath` is persisted to `models.local_path` and used directly as the engine launch path (`instances.go:241,312`, `vllm_instances.go:60`). Any multi-quant GGUF repo, sharded GGUF (`-00001-of-000NN.gguf`), or sharded safetensors gets `LocalPath` pointing at an arbitrary file → loads the wrong quant or misses shard 1.
- **Suggested fix:** Track a real running maximum, and special-case multi-part GGUF to prefer `-00001-of-*` (mirroring `scanner.go:227-233`, which does it correctly).

### [LOW] `Model.ModTime` documented as driving change-detection but never read
- **File:** `internal/inventory/model.go:129-133`, `internal/inventory/repository.go:121-187`
- **Issue:** The doc comment says `ModTime` decides "whether a row changed," but `upsertOne` never references it; `Upsert` unconditionally bumps `updated_at` every scan.
- **Suggested fix:** Implement the change-detection (skip UPDATE when path/size/mtime match) or fix the comment.

### [LOW] `FinalizeHashes` read-modify-write has no optimistic-concurrency guard
- **File:** `internal/manifest/store.go:141-172`
- **Issue:** `Get` → mutate → `UPDATE ... WHERE id = ?` doesn't re-assert the read state, so two overlapping calls could lose an update. Low (one hasher per manifest intended) but a real TOCTOU.
- **Suggested fix:** Compare-and-swap on the previously-read JSON, or condition on a `hash_pending` column.

### Overall assessment
Well-engineered: `paths.go` correctly defeats string-based `..`/absolute/sibling-prefix escapes (verified against tests), streamed sha256 with resume is correct for the trusted commit-pinned HF path, ADR-004 warn-not-block is implemented exactly (`warnings.go`), manifest immutability is enforced, and the SQLite layer makes the right calls for this project (**pure-Go `modernc.org/sqlite` driver — good for Linux cross-compile**, WAL + busy_timeout + single-connection pool, additive-only idempotent migrations). Issues are edge cases: unenforced streaming size caps (the disk-fill risk is most serious), lexical-only symlink handling, unauthenticated resume of untrusted URLs, and the multi-file selection bug. None compromise the primary "malicious repo path" threat model, which is solid.

---

## Ollama adapter & web UI (reviewed by Sonnet)

### Ollama adapter

#### [HIGH] Entire Ollama runtime package is dead code per ADR-009, not yet removed
- **File:** `internal/config/config.go:426-427, 258-263`, `cmd/localhub/main.go:85-88`
- **Issue:** `docs/decisions.md` ADR-009 ("Retire Ollama as a LocalHub runtime," accepted 2026-07-11) retires Ollama. Config validation hard-fails startup if `engines.ollama.enabled` is `true`, and the default is `false`. Since `ollamaClient` is only built when `Enabled`, `reg.Ollama`, `NewOllamaInvAdapter`, `ollama.NewRunner`, `ollama.NewMirror` are always nil — so `internal/engines/ollama/*.go` (~800 lines + tests) is unreachable in the running product.
- **Impact:** Dead code carrying latent bugs still compiles, still has passing tests, and could bit-rot or be accidentally re-enabled.
- **Suggested fix:** Delete the package (and the warm/mirror driving code in `ollama_instances.go`, keeping any read-only detection ADR-009 still wants) or clearly quarantine it (build tag / `_disabled` suffix / remove from `main.go`).

#### [MEDIUM] `Runner.Stop` has no idempotency handling for the Mirror-vs-Runner STOPPED race
- **File:** `internal/engines/ollama/warm.go:196-256` (`Stop`) vs `154-176` (`Warm`)
- **Issue:** `Warm` handles the mirror race (catches `IllegalTransitionError`, treats already-HEALTHY as success). `Stop` has the symmetric race — `Mirror.Sync` can mark the DRAINING row STOPPED before `Runner.Stop`'s own transition — but no matching handling; it returns `fmt.Errorf("mark stopped: %w", serr)`.
- **Impact:** A successful unload can surface as an error because the mirror won the race.
- **Suggested fix:** Mirror the `Warm` pattern: on `IllegalTransitionError`, re-fetch and return success if already STOPPED.

#### [MEDIUM] `Unload` has no `/api/embed` fallback for embedding models (asymmetric with `Warm`)
- **File:** `internal/engines/ollama/warm.go:210-238` vs `97-125`
- **Issue:** `Warm` retries embedding models on `/api/embed` when `/api/generate` 400s; `Unload` sends `keep_alive:0` unconditionally via `/api/generate` with no equivalent.
- **Impact:** An embedding model warmed via the fallback would 400 on unload — warmable but never cleanly unloadable.
- **Suggested fix:** Apply the same retry-on-`/api/embed` logic to `Unload`.

#### [MEDIUM] `DefaultBinaryPath` is a hardcoded Windows dev-box path used as the cross-platform fallback
- **File:** `internal/engines/ollama/ollama.go:47-51` (`K:\Softwares\ollama\ollama.exe`), used at `69-71, 146, 167`
- **Issue:** `main.go:87` always calls `ollama.New(baseURL, "")`, so `New` always falls back to `DefaultBinaryPath` — a Windows-only path with no config knob. On Linux `binaryExists` always returns false, so `Detect` can never distinguish "installed but stopped" from "not installed" (always `Detected=false` even at `/usr/bin/ollama`).
- **Impact:** Low today (package unreachable); broken cross-platform detection if retirement is reversed.
- **Suggested fix:** Add a config binary-path field and an OS-appropriate default.

#### [LOW] No effective timeout bounds Ollama HTTP calls from long-lived goroutines
- **File:** `internal/engines/ollama/ollama.go:65-77` (`&http.Client{}`), `cmd/localhub/main.go:300-301, 340`
- **Issue:** The comment claims "per-call timeouts come from the caller's context," but the periodic mirror sync passes the root shutdown context (no deadline) and the client has no `Timeout`. A hung connection blocks the GET indefinitely, stalling all future syncs.
- **Suggested fix:** Set a sane `http.Client.Timeout` or wrap call sites with `context.WithTimeout`.

### Web UI

#### [MEDIUM] `time.Time` fields tagged `omitempty` are never actually omitted
- **File:** `internal/core/instances.go:90-92`, `internal/instances/instance.go:50-53`, consumed as `web/src/api/types.ts:311-313` (`created_at?`, `started_at?`, `stopped_at?`)
- **Issue:** Go's `omitempty` doesn't treat a zero-value `time.Time{}` as empty, so an unset `started_at`/`stopped_at` serializes as `"0001-01-01T00:00:00Z"` rather than omitted — contradicting the TS "optional" types.
- **Impact:** Latent (not rendered today), but any future `if (inst.started_at)` gets a false positive (non-empty zero-time string is truthy), and `relativeTime`/`Date.parse` shows a nonsensical multi-million-hour age.
- **Suggested fix:** Serialize as `*time.Time` (nil omits) or a custom MarshalJSON; or on the TS side check for the zero-time sentinel.

#### [LOW] Inconsistent unmount-safety pattern across data-fetching components
- **File:** `web/src/api/useResource.ts:28-37` (has a `mounted` ref) vs `InstancesCard.tsx`, `DownloadsCard.tsx`, `QueueCard.tsx` (no guard)
- **Issue:** Three components roll their own `useState`+`useCallback` fetch logic without the `mounted` guard the codebase's own `useResource` establishes.
- **Impact:** Low in this SPA, but a slow request resolving after unmount would `setState` on an unmounted component.
- **Suggested fix:** Route them through `useResource` or add the `mounted` ref guard.

#### [LOW] User-facing error hint hardcodes a loopback address that's misleading over Tailscale
- **File:** `web/src/api/client.ts:84-89`
- **Issue:** The synthesized error message always says `127.0.0.1:39876` regardless of how the dashboard was reached (the actual `fetch` correctly uses a same-origin relative path, so nothing is functionally broken).
- **Suggested fix:** Derive the hint from `window.location.host`, or drop the host/port.

### Overall assessment
The Ollama adapter is internally careful about the "verify VRAM/`/api/ps`, don't trust a 200" bar and handles the one documented race, but that care wasn't applied symmetrically to `Stop`/`Unload`, and the whole package is dead per ADR-009 — resolve by removal or explicit quarantine. The web UI is in solid shape: no hardcoded absolute URLs threaten Tailscale access, the WebSocket/SSE reconnect state machine (`live/connection.ts`) correctly tears down transports before reopening (no duplicate connections) and uses capped exponential backoff with a stall watchdog, and `useResource`/`useLive` guard post-unmount updates. The two concrete gaps worth fixing are the `omitempty`-on-`time.Time` wire mismatch and the inconsistent unmount guard.

---

## Appendix — review coverage

| Area | Reviewer | Files |
|---|---|---|
| llama.cpp adapter | Opus | `internal/engines/llamacpp/*` |
| vLLM adapter | Opus | `internal/engines/vllm/*` |
| Cross-platform / process / hardware | Opus | `internal/hardware/*`, `internal/instances/*`, `internal/instances/proc/*`, `cmd/localhub/main.go`, `internal/config/*` |
| Core orchestration & residency | Opus | `internal/core/*`, `internal/residency/*` |
| Compatibility estimator | Sonnet | `internal/compat/*` (incl. `gguf/`, `safetensors/`, `hfmeta/`) |
| API layer & proxy surface | Sonnet | `internal/api/*`, `internal/proxy/*`, `web/embed.go` |
| Supply chain, inventory, persistence | Sonnet | `internal/manifest/*`, `internal/inventory/*`, `internal/db/*` |
| Ollama adapter & web UI | Sonnet | `internal/engines/ollama/*`, `web/src/**` |

Not deeply reviewed (lower risk for the stated goal): the extensive `docs/`, `prompts/`, `research/`, `temp-research/`, and `project-management/` trees; test-only helper files beyond a quality skim. All findings are read-only observations; no code was changed.
