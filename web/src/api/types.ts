// API types — mirror the daemon's JSON shapes EXACTLY as documented in:
//   temp-research/wiring-p12.md  (§5 hardware snapshot)
//   temp-research/wiring-p13.md  (GET /api/engines projection)
//   temp-research/wiring-p14.md  (GET /api/models inventory)
//   temp-research/spec-digest.md §3 (API surface), §2 (models DDL)
//
// Assumptions that need confirming against the wiring agent's FINAL shapes are
// flagged with `// ASSUMPTION:` — see temp-research/wiring-p15.md §"open questions".

// ── Hardware (wiring-p12 §5) ──────────────────────────────────────────────────
// VRAM values are MiB (nvidia-smi --nounits). util/temp = -1 means "unavailable".
export interface GPU {
  index: number;
  name: string;
  vram_total: number; // MiB
  vram_free: number; // MiB
  vram_used?: number; // MiB (added by probe; may be absent)
  driver: string;
  cc: string; // compute capability, e.g. "8.9"
  cuda?: string; // usually empty (omitempty on the Go side)
  util: number; // percent, or -1 if N/A
  temp: number; // °C, or -1 if N/A
  power: number; // watts, or -1 if N/A

  // ── Extended telemetry (PLAN-ui-gpu-rework.md §2.1) ────────────────────────
  // EVERY numeric field below uses -1 to mean "nvidia-smi reported N/A". They MUST render as
  // "n/a", never as 0: a 0 %-fan or a 0 MHz clock is a legitimate reading on an idle card, so
  // conflating the sentinel with zero would state something untrue. Use `formatNa` from
  // lib/format, which maps every negative/absent value to "n/a".
  //
  // They are optional here (not on the Go side) so an OLDER daemon that omits them degrades to
  // "n/a" rather than rendering `undefined` — same honesty rule, one code path.
  clock_sm_mhz?: number;
  clock_mem_mhz?: number;
  clock_sm_max_mhz?: number;
  fan_pct?: number;
  /** PCIe link generation NEGOTIATED right now. It downtrains to 1 at idle, which is why the
   *  max is always shown beside it — a bare "gen 1" reads as broken hardware otherwise. */
  pcie_gen?: number;
  pcie_gen_max?: number;
  pcie_width?: number;
  pcie_width_max?: number;
  power_limit_w?: number;
  power_max_limit_w?: number;
  /** Decoded clocks_throttle_reasons.active, e.g. "idle" or "sw_power_cap,hw_thermal_slowdown".
   *  Empty/absent means unthrottled. */
  throttle?: string;
  /** The budget ADMISSION compares a model against: floor(vram_free × 0.92). Deliberately
   *  SMALLER than vram_free — showing only raw free VRAM makes a model look like it fits when
   *  admission disagrees, which is the exact confusion this field exists to remove. -1 = not
   *  computed. */
  usable_vram_mib?: number;
}

/** One process holding a CUDA context (`nvidia-smi --query-compute-apps`).
 *
 *  `used_mib` is ALWAYS -1 on Windows: in WDDM mode the driver does not attribute VRAM per
 *  process. The LIST is still useful (knowing *who* holds the card is actionable), but the usage
 *  column must render "n/a" — never 0, and never an implied number we do not have.
 *  `name` may be "" when Windows denies access to the image path; show the pid alone then. */
export interface GPUProcess {
  pid: number;
  name: string;
  used_mib: number;
}

export interface CPU {
  model: string;
  cores: number;
  threads: number;
  util: number; // -1 sentinel = not measured
}

export interface RAM {
  total: number; // MiB
  free: number; // MiB
  used: number; // MiB
  /** Residency RAM floor (config residency.min_free_ram_mib, default 2048). A start is REFUSED
   *  while `free < floor_mib`. On a 16 GB box RAM binds BEFORE VRAM, so this is a frequently-hit
   *  condition, not an edge case — without the floor on screen next to free RAM, a refusal with
   *  14 GiB of VRAM sitting idle looks arbitrary. Absent/0 = unset. */
  floor_mib?: number;
}

export interface Disk {
  path: string;
  total_gib: number;
  free_gib: number;
  used_gib: number;
}

// `disk` is a LIST (a box keeps models on 1..N volumes) — wiring-p12 §5.
export interface HardwareSnapshot {
  gpus: GPU[];
  cpu: CPU;
  ram: RAM;
  disk: Disk[];
  /** Processes currently holding the GPU. OMITTED (not []) when nothing has a CUDA context. */
  gpu_processes?: GPUProcess[];
  captured_at: string; // RFC3339 UTC
}

// ── Engines (wiring-p13) ──────────────────────────────────────────────────────
// The bare api-spec tuple is {name, installed, version, running, base_url}, but the
// wiring agent adds `reason` (ALWAYS present, non-waivable) and `details`.
export interface EngineDetails {
  binary_path?: string;
  resolved_path?: string;
  extra_path?: string[];
  backends?: string[]; // e.g. ["CUDA"] | ["RPC","Vulkan","CPU"]
  caps?: string[]; // e.g. ["fit","sleep-idle","flash-attn","tq3"]
  has_tq3?: boolean;
  /** The llama.cpp adapter's per-binary capability map (has_tq3, has_spec_mtp, has_flash_attn,
   *  …). It is what binary SELECTION keys off, so the Engines panel reads it to say WHICH of
   *  several detected builds a given start would actually use. */
  capabilities?: Record<string, boolean>;
  commit?: string;
  images?: string[]; // vLLM docker images present
  running_models?: number;
  models?: number; // installed count
  [k: string]: unknown; // tolerate additional detail fields
}

export interface Engine {
  name: string; // ollama | llamacpp | vllm  (llamacpp may appear MULTIPLE times)
  installed: boolean; // = Detected
  version?: string; // omitempty on the Go side — absent when unknown
  running: boolean; // = Reachable (daemon/endpoint answering now)
  base_url: string; // "" for llamacpp (spawned, no daemon)
  reason: string; // ALWAYS present
  details?: EngineDetails;
}

// ── Models / inventory (GET /api/models — core.ModelSummary) ──────────────────
// Mirrors the daemon's ACTUAL P1.4 projection (internal/core/inventory.go), which is a
// trimmed subset of the models DDL. The Go side marshals nullable columns with
// `omitempty`, so an absent value is UNDEFINED (key omitted), not JSON `null`. Presence is
// a DEDICATED field (`present` | `missing`), NOT derived from compat_status — the daemon
// tracks vanished-from-disk rows in migration 0002 without deleting them.
// ── Launch state (residency — GET/PUT /api/models/{id}/launch-state) ──────────
// Whether the model is kept loaded in VRAM, NOT whether it's visible in any list. Four
// values; an absent/unknown value from an older daemon must degrade to the safe default
// (available_on_request) rather than being treated as unavailable.
export type LaunchState =
  | 'always_available'
  | 'available'
  | 'available_on_request'
  | 'unavailable';

export interface ModelSummary {
  id: string;
  display_name: string;
  source: string; // ollama | huggingface | local_path | direct_url
  format: string; // gguf | safetensors | pytorch | ollama
  quantization?: string; // e.g. q4_k_m, tq3_4s (omitted when unknown)
  repo?: string;
  local_path?: string;
  size_bytes?: number; // raw bytes (omitted when unknown)
  sha256?: string;
  compat_status?: string; // runs | runs_degraded | wont_run | unknown (omitted when unset)
  presence: string; // present | missing (ALWAYS present)
  missing_since?: string; // RFC3339, set only when presence=missing
  updated_at?: string;
  // Residency control. `launch_state` is the four-value source of truth; `auto_load_disabled`
  // is the older boolean the daemon keeps deriving (`launch_state === 'unavailable'`) for
  // back-compat. Both are omitted by an older daemon — callers must default the state, not
  // read absence as `unavailable`.
  launch_state?: LaunchState;
  auto_load_disabled?: boolean;
}

// POST /api/models/rescan → core.RescanResult. The client uses this only as a success
// signal (it refetches /api/models), but the shape is typed for forward use.
export interface RescanResult {
  inserted: number;
  updated: number;
  vanished: string[]; // catalog rows not found this pass (marked missing)
  newly_missing: string[]; // subset that transitioned present -> missing this pass
  skipped_count: number;
  skipped?: unknown[];
  scan_errors?: string[];
  ollama_scanned: boolean;
  model_count: number;
}

// ── Compatibility verdict (compat.Verdict — GET /api/models/{id}/compat, POST
//    /api/compat/preview) ─────────────────────────────────────────────────────
// Every verdict ALWAYS carries a non-empty `reason` (schema-enforced on the Go side by the
// newVerdict constructor). status is the four-value vocabulary; run_flags is the exact launch
// command for a runnable verdict.
export type CompatStatus = 'runs' | 'runs_degraded' | 'wont_run' | 'unknown';
export type CompatConfidence = 'exact' | 'approximate' | 'fallback';

export interface AlternativeVerdict {
  engine: string; // llamacpp | ollama | vllm
  detail?: string; // e.g. "CUDA fork (has_tq3)"
  status: CompatStatus;
  estimated_vram_mb: number;
  max_context_that_fits?: number;
  run_flags?: string[];
  confidence: CompatConfidence;
  reason: string; // ALWAYS present
}

export interface Verdict {
  model: string;
  status: CompatStatus;
  reason: string; // MANDATORY, specific — names the numbers
  recommended_engine: string;
  estimated_vram_mb: number;
  max_context_that_fits?: number;
  requested_context?: number;
  run_flags?: string[];
  confidence: CompatConfidence;
  estimator?: string;
  engine_choices?: AlternativeVerdict[];
  warnings?: string[];
  vram_free_mb?: number;
  vram_total_mb?: number;
}

// POST /api/compat/preview request body: exactly one selector.
export interface CompatPreviewRequest {
  model_id?: string;
  hf_repo?: string;
  file?: string;
  revision?: string;
  ctx?: number;
  batch?: number;
}

// ── Supply chain / downloads (Phase 3, spec-digest §7) ────────────────────────
// A download job's lifecycle. `awaiting_ok` is the two-step HF gate: the daemon resolved the
// commit + file list and is WAITING for the user to confirm before fetching any bytes.
export type DownloadState =
  | 'resolving'
  | 'awaiting_ok'
  | 'running'
  | 'hashing'
  | 'succeeded'
  | 'failed';

export interface DownloadProgress {
  file: string;
  bytes_done: number;
  bytes_total: number; // 0 if unknown
  file_index: number;
  file_count: number;
  resumed?: boolean;
  phase: string; // downloading | verifying | done | hashing
}

export interface ResolvedFile {
  path: string;
  size_bytes: number;
  selected: boolean;
}

// The show-before-commit payload: pinned commit + files, shown before the user confirms.
export interface ResolvedDownload {
  repo: string;
  ref: string;
  commit: string;
  gated: boolean;
  license?: string;
  files: ResolvedFile[];
  total_size_bytes: number;
  warnings: string[];
}

export interface DownloadJob {
  id: string;
  kind: string; // hf | gguf_url | ollama | local_import
  state: DownloadState;
  repo?: string;
  ref?: string;
  commit?: string;
  name?: string;
  reason?: string;
  progress?: DownloadProgress;
  manifest_id?: string;
  model_id?: string;
  resolved?: ResolvedDownload;
  created_at: string;
  updated_at: string;
}

// POST /api/downloads request body: exactly one kind's fields.
export interface DownloadRequest {
  kind: string; // hf | gguf_url | ollama | local_import
  repo?: string;
  ref?: string;
  files?: string[];
  url?: string;
  name?: string;
  path?: string;
  confirm?: boolean;
}

// A manifest file entry.
export interface ManifestFile {
  path: string;
  sha256: string;
  size_bytes: number;
}

// An immutable manifest (GET /api/manifests/{id}, GET /api/models/{id}/manifest).
export interface Manifest {
  id: string;
  source: string; // hf | gguf_url | ollama | local_import
  repo?: string;
  revision?: string;
  format?: string;
  quantization?: string;
  license?: string;
  files: ManifestFile[];
  security_warnings: string[];
  downloaded_at: string;
  name?: string;
  digest?: string;
  url?: string;
  local_path?: string;
  hash_pending?: boolean;
  supersedes_id?: string;
}

// The reproduce export (GET /api/models/{id}/reproduce).
export interface ReproduceBundle {
  model_id: string;
  display_name?: string;
  source: string; // hf | gguf_url | ollama | local_import | unknown
  provenance: string;
  repo?: string;
  ref?: string;
  commit?: string;
  files?: ManifestFile[];
  url?: string;
  ollama_name?: string;
  ollama_digest?: string;
  local_path?: string;
  hash_pending?: boolean;
  primary_sha256?: string;
  license?: string;
  size_bytes?: number;
  warnings?: string[];
  engine?: string;
  launch_flags?: string[];
  reproduce_hint?: string;
}

// ── Instances (Phase 4 — run/stop, GET/POST/DELETE /api/instances) ────────────
// An instance is a spawned model process (llama.cpp in Phase 4). state is the six-value
// lifecycle vocabulary; every instance carries a mandatory `reason` for its current state.
export type InstanceState =
  | 'REQUESTED'
  | 'STARTING'
  | 'HEALTHY'
  | 'DRAINING'
  | 'STOPPED'
  | 'FAILED';

export interface InstanceView {
  id: string;
  model_id: string;
  engine: string; // llamacpp | ollama | vllm
  state: InstanceState;
  pid?: number;
  port?: number;
  base_url?: string;
  binary_path?: string;
  model_path?: string;
  ctx?: number;
  vram_est_mib?: number;
  vram_obs_mib?: number;
  vram_freed_mib?: number;
  pinned: boolean;
  priority: number;
  launch_flags?: string[];
  reason?: string; // current state reason
  created_at?: string;
  started_at?: string;
  stopped_at?: string;
  // ── Truthful monitoring (migration 0010, M5) ────────────────────────────────
  /** Last SUCCESSFUL health probe by the periodic monitor. A HEALTHY instance with a stale
   *  `last_health_at` is meaningfully different from a freshly-confirmed one. */
  last_health_at?: string;
  /** CONSECUTIVE failed health probes. Non-zero on a HEALTHY instance means DEGRADED: the monitor
   *  is seeing failures but has not yet reached the reap threshold. */
  health_failures?: number;
  /** Whether the VRAM reclaim on stop was PROVEN by an NVML delta. `undefined` = no stop attempted;
   *  `false` = the process is gone but reclamation was never proven (timeout, or the baseline probe
   *  failed). Never read `state === 'STOPPED'` alone as "VRAM is back" (M5.5) — the paired `reason`
   *  spells out which case it was. */
  reclaim_verified?: boolean;
  /** True when LocalHub ATTACHED to a process it did not spawn (e.g. a launcher script's server).
   *  The UI must surface this: Stop on an adopted row would kill a process owned by whatever started
   *  it, so the API refuses without `?force=true`, and residency never auto-evicts it. */
  adopted?: boolean;
  /** How the adopted instance was discovered, e.g. "attach: port 8080, PID 20352". */
  adopted_source?: string;
}

/** POST /api/instances/attach body — ADOPT an already-running llama-server rather than spawning one.
 *
 *  No admission runs (nothing is being allocated); the launch profile and context are READ from the
 *  live process's own command line. `vram_mib` is optional and exists because per-process VRAM is not
 *  readable under Windows WDDM (nvidia-smi reports `[N/A]`): supply the figure you measured and it is
 *  recorded as a calibration peak, or omit it and the footprint is honestly reported as unknown. */
export interface AttachInstanceRequest {
  port: number;
  host?: string;
  model_id?: string;
  pin?: boolean;
  vram_mib?: number;
}

// POST /api/instances request body.
export interface StartInstanceRequest {
  model_id: string;
  engine?: string;
  ctx?: number;
  batch?: number;
  priority?: number;
  pin?: boolean;
  /** LEGACY single-bool KV description: true = q4_0 on BOTH cache sides. Kept for back-compat; it is
   *  a shorthand for kv_type_k/kv_type_v, and an explicit pair WINS over it. */
  kv_quant?: boolean;
  /** INDEPENDENT `-ctk` key-cache dtype: 'f16' | 'q8_0' | 'q4_0'.
   *
   *  The Qwen3.6-27B MTP profile PROVEN at 64K on the reference box is SYMMETRIC —
   *  kv_type_k='q4_0' with kv_type_v='q4_0' (scripts/run-qwen-64k-q4q4.ps1). Independence still
   *  matters: an ASYMMETRIC pair like q8_0/q4_0 keeps the quality-critical K cache at higher
   *  precision, and was inexpressible through this API before M2.1 when `kv_quant` was a single bool
   *  meaning q4_0 on both sides. Both the VRAM estimate and the emitted flags follow these fields.
   *  'tq3_0' is rejected: the estimator cannot price it, and an unpriced footprint must not be
   *  admitted. */
  kv_type_k?: string;
  /** INDEPENDENT `-ctv` value-cache dtype. Setting only ONE side is a genuine asymmetric request,
   *  not a mistake — the other side stays f16. */
  kv_type_v?: string;
  /** llama.cpp `-np` (parallel server slots). Omitted/0 = 1. Admission-relevant, not cosmetic: each
   *  slot multiplies the KV cache. A draft-mtp start FORCES 1 regardless of this value. */
  slots?: number;
  /** `--spec-draft-n-max` for a speculative mode. Omitted/0 = use the engine default. The validated
   *  Qwen3.6 MTP profile uses 2. Rejected when `speculative` is off (it would never be emitted, so
   *  accepting it would let a caller believe MTP was on). */
  spec_draft_max?: number;
  /** Speculative decoding mode: '' (off) | 'ngram-cache' (draft-free self-speculation, ~zero extra
   *  VRAM) | 'draft-mtp' (the model's EMBEDDED multi-token-prediction head). 'draft-mtp' requires
   *  BOTH a model with MTP tensors AND a `has_spec_mtp` binary; otherwise the start is refused with
   *  a reason naming which side failed. */
  speculative?: string;
  // GPU/CPU layer placement. Omitted/false = full GPU, NEVER split to CPU (default:
  // llama.cpp `-ngl all`, Ollama `num_gpu=999`). true = allow a GPU/CPU split
  // (`-ngl auto` / Ollama auto). Relaxes launch flags only — NOT the admission VRAM
  // verdict (docs/api-spec.md §Instances). The client omits it entirely when false to
  // keep the wire clean (default behavior needs no field).
  allow_cpu_offload?: boolean;
}

// ── Saved per-model launch default (M2.3 — /api/models/{id}/launch-default) ───
// A launch profile that has been PROVEN on this box, stored so the advanced start form can be
// pre-filled with values known to work.
//
// ADVISORY, by design: `POST /api/instances` does NOT read this. The UI pre-fills the form and the
// operator confirms. An unspecified start field keeps meaning "the daemon default", never "whatever
// was saved" — auto-applying it would be a silent behaviour change (TODO.md M6.1).
export interface ModelLaunchDefault {
  model_id: string;
  ctx?: number;
  kv_type_k?: string;
  kv_type_v?: string;
  slots?: number;
  spec_mode?: string;
  spec_draft_max?: number;
  allow_cpu_offload?: boolean;
  // Provenance (M6.2): WHAT this was validated against. A profile validated on one llama.cpp build
  // is not evidence about another — the older CUDA fork hard-failed on this model's hybrid SSM
  // architecture, which is exactly why capability-first binary selection exists.
  engine_version?: string;
  engine_commit?: string;
  binary_path?: string;
  artifact_sha256?: string;
  validated_note?: string;
  /** Set by the server on READ (never stored) when the recorded provenance no longer matches what is
   *  detected now. Non-empty = "proven somewhere else; re-validate before relying on it". */
  stale_reason?: string;
  created_at?: string;
  updated_at?: string;
}

// ── Residency queue (Phase 5 — GET/DELETE /api/queue) ─────────────────────────
// One admission queue entry. priority_class: 0 interactive | 1 normal | 2 background.
// blocked_reason always names the numbers (needed/free/reclaimable/short).
export interface QueueEntry {
  id: string;
  model_id: string;
  engine?: string;
  priority_class: number;
  status: 'WAITING' | 'ADMITTED' | 'BLOCKED' | 'DONE' | 'REJECTED';
  blocked_reason?: string;
  footprint_mib?: number;
  position?: number; // 1-based score-order position (WAITING only)
  age_seconds?: number;
}

// 202 response body when a start is queued (POST /api/instances, blocked on VRAM).
export interface QueuedResponse {
  status: 'queued';
  queue_id: string;
  position: number;
  blocked_reason: string;
}

// ── Free the GPU (POST /api/gpu/free — PLAN-ui-gpu-rework.md §2.5) ────────────
// Stop everything LocalHub is running so the card is available again, in ONE action. It is a thin
// orchestration over the per-instance stop path, so every stop still drains first and verifies the
// VRAM actually came back via an NVML delta rather than trusting an HTTP 200.

/** Per-instance outcome. `stopped:false` covers BOTH a skip and a failure — `reason` says which. */
export interface FreeGPUInstanceResult {
  id: string;
  model_id: string;
  stopped: boolean;
  reason: string;
}

/** The whole sweep. VRAM is MEASURED before and after, so `reclaimed_mib` is observed truth, not a
 *  sum of per-instance estimates. It can be NEGATIVE if another process grabbed VRAM mid-sweep;
 *  that is reported honestly rather than clamped, so the UI must not assume it is >= 0. */
export interface FreeGPUResult {
  stopped: number;
  /** Instances left alone because they are PINNED. Pinning is an explicit "keep this resident"
   *  instruction, so the convenience button never overrides it silently — the UI surfaces the
   *  count and offers the include_pinned retry. */
  skipped: number;
  failed: number;
  vram_free_before_mib: number;
  vram_free_after_mib: number;
  reclaimed_mib: number;
  results?: FreeGPUInstanceResult[];
  reason: string;
}

// ── Error envelope (spec-digest §3, MANDATORY) ────────────────────────────────
// { "error": { "code", "message", "reason"? } } — surface `reason` in the UI.
export interface ApiErrorBody {
  error: {
    code: string;
    message: string;
    reason?: string;
  };
}

// Native tray startup policy. The daemon persists this at configs/startup.json; the tray applies
// it at login and on its low-frequency recovery cycle.
export interface StartupConfig {
  version: number;
  start_localhub: boolean;
  open_web_ui: boolean;
  restore_last_session: boolean;
  startup_delay_seconds: number;
  health_check_seconds: number;
  models: StartupModelConfig[];
}

export interface StartupModelConfig {
  model_id: string;
  enabled: boolean;
  order: number;
  start_delay_seconds: number;
  retry_policy: 'never' | 'limited' | 'keep_available';
  max_attempts: number;
  use_saved_profile: boolean;
  // localhub_api is the only launch mode. The removed 'canonical_qwen' mode was executed by a
  // PowerShell supervisor the tray no longer calls; startup config v2 strips those entries.
  mode: 'localhub_api';
  engine?: '' | 'llamacpp' | 'vllm';
  ctx?: number;
  batch?: number;
  priority?: number;
  pin?: boolean;
  kv_type_k?: '' | 'f16' | 'q8_0' | 'q4_0';
  kv_type_v?: '' | 'f16' | 'q8_0' | 'q4_0';
  slots?: number;
  speculative?: '' | 'ngram-cache' | 'draft-mtp';
  spec_draft_max?: number;
  allow_cpu_offload?: boolean;
}
