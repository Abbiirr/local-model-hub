# Qwen 64K standalone runtime runbook

## Canonical configuration

The canonical launcher is `scripts/run-qwen-64k-q4q4.ps1`; the adjacent `.cmd`
file is a double-click/Command Prompt wrapper.

The verified profile is:

- Qwen3.6 27B mini IQ4_XS GGUF
- llama.cpp CUDA build b10106
- 65,536-token context
- K cache `q4_0`, V cache `q4_0`
- all 99 layers requested on GPU
- flash attention enabled
- one slot
- MTP speculation with maximum draft length 2
- reasoning enabled with no server-side reasoning budget
- agent-compatible native Qwen tool template from
  `configs/qwen3.6-agent-tools.jinja`; it preserves Qwen's XML tool-call format
  while merging multiple client system messages into the required leading
  system block
- listener on `0.0.0.0:8080` since 2026-07-28 (previously LAN-only at
  `192.168.0.73:8080`); Windows Firewall program rules for `llama-server.exe`
  allow inbound TCP on Private-profile networks only (trusted LAN + Tailscale
  adapter) and block Public-profile networks

The 2026-07-26 sustained check generated 2,048 tokens at 37.27 server
tokens/second with 89.8% MTP acceptance. A separate historical 2,685-token
coding run measured 24.82 tokens/second. Throughput varies with prompt length,
output type, MTP acceptance, GPU load, and context occupancy.

## Commands

```powershell
scripts\run-qwen-64k-q4q4.cmd
scripts\run-qwen-64k-q4q4.cmd Status
scripts\run-qwen-64k-q4q4.cmd Restart
scripts\run-qwen-64k-q4q4.cmd Stop
```

`Start` is idempotent. It recognizes a healthy matching server and refuses to
launch a duplicate. `Stop` refuses to terminate an unrelated process occupying
port 8080.

## Evidence and logs

Runtime evidence is written under the Git-ignored `logs/` directory:

- `qwen-runtime-events.jsonl`: append-only structured launcher lifecycle,
  preflight, refusal, health, stop, and error events.
- `qwen-64k-q4q4-<timestamp>.engine.log`: llama.cpp engine events with wall-clock
  timestamps, request timing, MTP acceptance, context messages, and errors.
- `qwen-64k-q4q4-<timestamp>.out.log`: captured process standard output.
- `qwen-64k-q4q4-<timestamp>.err.log`: captured process standard error.

The launcher enables `/metrics` and `/slots` for live telemetry. It does not log
full prompt or response bodies by default because those may contain credentials,
private code, or other sensitive content.

Useful checks:

```powershell
Invoke-RestMethod http://192.168.0.73:8080/health
Invoke-RestMethod http://192.168.0.73:8080/slots
Invoke-WebRequest http://192.168.0.73:8080/metrics
Get-Content logs\qwen-runtime-events.jsonl -Tail 20
Get-Content (Get-ChildItem logs\*.engine.log |
  Sort-Object LastWriteTime -Descending |
  Select-Object -First 1) -Tail 100
```

## Limits and failure scenarios

### Client output cap

An API request's `max_tokens` or equivalent is an independent output cap. The
observed client used 8,192. Removing the server reasoning budget does not
override a client cap. If reasoning consumes the entire request allowance, the
response can end without final-answer content.

### Context exhaustion

The 65,536-token slot is shared by prompt, retained conversation, reasoning, and
final answer. Context shifting is disabled.

- A prompt/history that already exceeds the slot is rejected.
- Generation reaching the remaining slot capacity stops at the context limit.
- The server normally remains running; context exhaustion is not itself a GPU
  OOM.
- A literal 64K reasoning trace cannot coexist with a non-empty prompt and final
  answer inside this 64K slot.

### EOS and length finishes

With unrestricted server prediction and reasoning, normal completion still
depends on the model producing EOS before the client or context limit. Record
the API `finish_reason` and correlate it with the engine log:

- `stop`: the model emitted a normal stop/EOS condition.
- `length`: a client token limit or available context ended generation.

### VRAM admission and OOM

The launcher refuses a fresh load below 15,000 MiB free VRAM. The 64K profile
allocates almost the entire RTX 4060 Ti 16 GB; observed idle headroom has varied
from about 168 to 543 MiB. Successful startup proves the profile loaded, not that
another GPU application cannot later consume the remaining memory.

Do not lower the preflight or start another GPU model to force admission. Close
competing GPU workloads, confirm free memory with `nvidia-smi`, then rerun
`Start`. If llama.cpp exits during loading, the launcher records the exit code
and recent error log before failing.

### Model appears down

Check `Status` and `/health` first. The standalone model is on port 8080.
LocalHub on port 39876 is a separate process and does not proxy to this
standalone server.

For another LAN machine, use:

```text
http://192.168.0.73:8080/v1
```

For another Tailscale host on this tailnet (this box is `100.111.28.50`):

```text
http://100.111.28.50:8080/v1
```

The API is unauthenticated; its safety boundary is the network: the Private-only
firewall scoping plus the tailnet's own device authentication. Never enable
`tailscale funnel` for port 8080 — that would publish the API to the open
internet. `tailscale serve` is currently unconfigured and not required.

The process can be healthy locally while Windows Firewall blocks inbound
traffic. Keep the `llama-server` program rules as: Allow inbound TCP on the
Private profile, Block on Public.

### Shell closure, sign-out, and reboot

The launcher uses a detached hidden process, so closing the launching shell does
not stop it. A Windows sign-out or reboot does stop it. This launcher does not
install a scheduled task or watchdog; automatic boot recovery must be configured
separately and must still use this canonical launcher and its VRAM preflight.

### LocalHub

LocalHub is intentionally stopped in the current standalone setup. Port 39876
should be closed. Starting LocalHub does not adopt or expose the standalone model
on port 8080.

## Incident reporting

For a failure report, capture:

1. Exact local time and client host.
2. Request endpoint and `finish_reason` (do not paste secrets).
3. `Status`, `/slots`, and `nvidia-smi`.
4. The matching structured journal entries.
5. The last 100 lines of the matching engine log.
6. Whether LocalHub or another GPU workload was running.

Keep partial evidence labeled partial: health proves availability, not successful
generation; process creation does not prove model load; a completed short request
does not prove sustained throughput.

## Observed scenario evidence — 2026-07-26

The following events were directly observed during configuration and validation:

| Scenario | Observed evidence | Verdict |
|---|---|---|
| Mixed `q8_0/q4_0` KV profile | Two live samples measured 7.79 and 12.93 tokens/second | Did not reproduce the claimed 25 tokens/second |
| Restored `q4_0/q4_0` profile | 512-token run measured 35.69 server tokens/second; 2,048-token run measured 37.27 | Proven above the 25 tokens/second target for those workloads |
| Finite reasoning plus small client cap | A 256-token chat request used the allowance in reasoning, returned empty final content, and ended with `finish_reason=length` | Demonstrates that a reasoning budget larger than the client allowance does not ensure a final answer |
| Unrestricted reasoning | A client request with `max_tokens=16000` stopped naturally after 12,913 decoded tokens, leaving 3,087 unused | Demonstrates EOS completion before the client cap for that request; not a universal completion guarantee |
| VRAM headroom | Idle readings after successful 64K loads ranged from about 168 to 543 MiB free | Very narrow margin; competing GPU workloads remain an OOM risk |
| Model reported down | PID, listener, `/health`, and `/v1/models` were healthy on port 8080 while LocalHub remained separate on 39876 | The apparent outage was an endpoint/firewall-path distinction, not a dead model |
| LocalHub shutdown | Verified LocalHub PID 21724 was stopped and port 39876 closed | Standalone llama.cpp remains the only intended model endpoint |
| Preflight parser failure | A restart stopped the prior process, then failed closed while parsing `nvidia-smi`; the JSONL journal records the error and stack | Parser changed to capture complete native output before selecting the first GPU value |
| Context overflow | Not destructively forced against the active model | Expected behavior is documented from the installed help and llama.cpp server contract; remains unexecuted in this runtime |

## Reproducing this profile through LocalHub — 2026-07-27

This section records how much of `scripts/run-qwen-64k-q4q4.ps1` LocalHub can
reproduce today, and exactly what still requires the script. The script itself is
unchanged and remains the canonical launcher.

### The profile is now saved in LocalHub

The script's profile is stored as the model's saved launch default
(`PUT /api/models/gguf-qwen3-6-27b-16gb-vram-mtp-mini-iq4-xs/launch-default`,
migration 0011). Verified round-trip on 2026-07-27:

```json
{"model_id":"gguf-qwen3-6-27b-16gb-vram-mtp-mini-iq4-xs","ctx":65536,
 "kv_type_k":"q4_0","kv_type_v":"q4_0","slots":1,"spec_mode":"draft-mtp",
 "spec_draft_max":2,"engine_version":"10106","engine_commit":"1425386fd",
 "binary_path":"K:\\projects\\ai\\local-model-hub\\tools\\llama.cpp\\llama-server.exe"}
```

`stale_reason` is absent, because the recorded `binary_path` is still among the
detected llama-server binaries. A saved default is **advisory**: `POST
/api/instances` never applies it automatically; the UI pre-fills the advanced
start form from it.

To start the expressible part of the profile from LocalHub:

```json
POST /api/instances
{"model_id":"gguf-qwen3-6-27b-16gb-vram-mtp-mini-iq4-xs","ctx":65536,
 "kv_type_k":"q4_0","kv_type_v":"q4_0","slots":1,
 "speculative":"draft-mtp","spec_draft_max":2}
```

### Flag-by-flag comparison

Script command line (from `logs/qwen-runtime-events.jsonl` and the adopted
instance's recorded `launch_flags`):

```text
-m <model> -ngl 99 -c 65536 -fa on -np 1 --cache-type-k q4_0 --cache-type-v q4_0
--spec-type draft-mtp --spec-draft-n-max 2 --reasoning on --reasoning-budget -1
--chat-template-file configs/qwen3.6-agent-tools.jinja
-lv 3 --log-prefix --log-timestamps --log-file <path> --metrics --slots
--host 192.168.0.73 --port 8080
```

LocalHub emits (observed on instance `bed6b54b`/`22e2d303`, `internal/compat/flags.go`
plus `internal/engines/llamacpp/runner.go` `buildArgs`):

```text
-m <model> --host 127.0.0.1 --port <pool> -ngl all -c <ctx> --fit off -fa on
-ctk <k> -ctv <v> -np <slots> [--spec-type <mode> --spec-draft-n-max <n>] --no-webui
```

| Script flag | Expressible in LocalHub today | Notes |
|---|---|---|
| `-c 65536` | Yes — `ctx` | Same value, admission-relevant. |
| `--cache-type-k q4_0` / `--cache-type-v q4_0` | Yes — `kv_type_k` / `kv_type_v` | LocalHub emits the short aliases `-ctk`/`-ctv`. |
| `-np 1` | Yes — `slots` (draft-mtp forces 1) | |
| `--spec-type draft-mtp` | Yes — `speculative` | Gated on model MTP tensors + `has_spec_mtp` binary. |
| `--spec-draft-n-max 2` | Yes — `spec_draft_max` | Emitted only when the binary advertises the flag. |
| `-fa on` | Yes — derived from binary capability, not a profile field | b10106 reports flash-attn, so LocalHub emits it. |
| `-ngl 99` | Equivalent — LocalHub emits `-ngl all` | Cosmetic for a 65-layer model; both mean full offload. `-ngl 99` additionally aborts llama.cpp's `--fit` pass ("n_gpu_layers already set by user to 99, abort"), which LocalHub achieves explicitly with `--fit off`. |
| `--reasoning on` | **No** | Behaviour-affecting, not VRAM-affecting. Qwen3.6 returns `reasoning_content` plus `content`; a LocalHub-started instance inherits the engine default instead of the proven setting. Needs a profile field (TODO M2.6a). |
| `--reasoning-budget -1` | **No** | Same field. `-1` = unrestricted; the script's default. |
| `--metrics` | **No** | Enables the Prometheus `/metrics` endpoint. Not VRAM-affecting, does not change generation. |
| `--slots` | **No** | Enables the `/slots` endpoint. Not VRAM-affecting, does not change generation. |
| `-lv 3` | **No — and it is a no-op on this build** | LocalHub-spawned llama-server logs `verbosity = 3 (adjust with the -lv N CLI arg)` even though LocalHub passes no `-lv`, so 3 is b10106's default. Passing it changes nothing. |
| `--log-prefix` / `--log-timestamps` | **No** | Cosmetic log formatting on the same stream LocalHub already captures. |
| `--log-file <path>` | **No — and would conflict** | The script's `.engine.log` and `.err.log` for run `20260726-124348` are byte-identical (`cmp` reports no difference, 2,757,316 bytes each): `--log-file` only duplicates stderr. LocalHub already captures the same stream to `logs/llamacpp-<id>.log`, size-capped at 8 MiB. |
| `--host 192.168.0.73` | **No — deliberate** | LocalHub binds every spawned engine to `127.0.0.1` and fronts it with its own authenticated surface. This is a policy decision, not a missing field. |
| `--port 8080` | **No — deliberate** | LocalHub allocates from its bind-tested port pool. |
| `--no-webui` | LocalHub adds it; the script does not | Headless control-plane default; no VRAM effect. |

### MTP acceptance: the signal exists without the script's logging flags

The `draft acceptance` figure (89.8% in the docstring) comes from llama.cpp's
`slot print_timing` block. It is an **INFO**-level line, and it is **not** gated on
`-lv 3`, `--metrics`, `--slots`, or `--log-file`:

- LocalHub-spawned instance `6615ba10` (flags: `-ngl all -c 8192 --fit off -fa on
  -ctk q4_0 -ctv q4_0 --no-webui`, i.e. none of those four flags) logged the full
  `print_timing` block, including `graphs reused`.
- The script's run logged the identical block plus one extra line:
  `draft acceptance = 0.87500 ( 14 accepted / 16 generated), mean len = 2.75`.

The only difference between the two blocks is speculative decoding being active.
So the acceptance line is gated on `--spec-type draft-mtp`, which LocalHub **can**
express, and it lands in `logs/llamacpp-<id>.log` on a LocalHub-started MTP
instance. What LocalHub lacks is a parser and an API surface for it, not the
signal. The earlier note in `PLAN-run-stop-monitor.md` §F.4 / `TODO.md` M2.6c —
that the logging flags are the likely reason the script sees an acceptance rate
and LocalHub does not — is **incorrect** on this build.

Caveats before building on this: the line is per-request, it is emitted only when
a slot finishes a task, and the instance log is capped at 8 MiB, so a long-running
instance will lose early lines. Any exposed metric must state which window it
covers.

### What still requires the script

Only three things, and only one of them is behavioural:

1. `--reasoning on --reasoning-budget -1` — the proven reasoning setting.
   Until a profile field exists, a LocalHub-started instance may produce a
   materially different response shape.
2. LAN binding on `192.168.0.73:8080` — deliberate LocalHub policy; use the
   script (or LocalHub's own authenticated surface) if a LAN listener is needed.
3. The 15,000 MiB free-VRAM preflight and the duplicate/foreign-port refusals are
   the script's own guards. LocalHub has its own admission path with different
   thresholds; the two are not interchangeable.

External-process adoption is retired. `POST /api/instances/attach` returns
`410 attach_disabled`; start models with `POST /api/instances` so LocalHub owns
admission, process creation, draining, and verified VRAM reclaim.
