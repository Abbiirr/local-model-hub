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
}

// POST /api/instances request body.
export interface StartInstanceRequest {
  model_id: string;
  engine?: string;
  ctx?: number;
  batch?: number;
  priority?: number;
  pin?: boolean;
  kv_quant?: boolean;
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

// ── Error envelope (spec-digest §3, MANDATORY) ────────────────────────────────
// { "error": { "code", "message", "reason"? } } — surface `reason` in the UI.
export interface ApiErrorBody {
  error: {
    code: string;
    message: string;
    reason?: string;
  };
}
