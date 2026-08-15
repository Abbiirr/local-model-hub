// Test fixtures matching the real-box shapes from wiring-p12/p13/p14.
import type {
  DownloadJob,
  Engine,
  HardwareSnapshot,
  Manifest,
  ModelSummary,
  ReproduceBundle,
  Verdict,
} from '../api/types';

// Extended telemetry values are the REAL readings from the reference box (RTX 4060 Ti, driver
// 576.40): PCIe downtrained to gen 1 of a max 3, fan genuinely at 0 %, throttle "idle". Keeping
// them real is the point — a fixture with tidy numbers would not exercise the two cases that
// actually bite (a legitimate 0 vs the -1 "not reported" sentinel).
export const hardwareFixture: HardwareSnapshot = {
  gpus: [
    {
      index: 0,
      name: 'NVIDIA GeForce RTX 4060 Ti',
      vram_total: 16380,
      vram_free: 13287,
      vram_used: 2823,
      driver: '576.40',
      cc: '8.9',
      util: 0,
      temp: 55,
      power: 14.76,
      clock_sm_mhz: 210,
      clock_mem_mhz: 405,
      clock_sm_max_mhz: 3105,
      fan_pct: 0,
      pcie_gen: 1,
      pcie_gen_max: 3,
      pcie_width: 8,
      pcie_width_max: 8,
      power_limit_w: 165,
      power_max_limit_w: 165,
      throttle: 'idle',
      usable_vram_mib: 12224,
    },
  ],
  cpu: {
    model: 'AMD Ryzen 5 2600 Six-Core Processor',
    cores: 6,
    threads: 12,
    util: -1,
  },
  ram: { total: 16331, free: 4145, used: 12186, floor_mib: 2048 },
  disk: [{ path: 'K:\\', total_gib: 1863.01, free_gib: 24.93, used_gib: 1838.08 }],
  // used_mib is -1 on EVERY row on Windows (WDDM does not attribute VRAM per process), and an
  // empty name is what nvidia-smi returns when Windows denies access to the image path.
  gpu_processes: [
    { pid: 18908, name: 'K:\\WindowsApps\\Claude\\app\\claude.exe', used_mib: -1 },
    { pid: 1836, name: '', used_mib: -1 },
  ],
  captured_at: '2026-07-06T07:05:33Z',
};

export const enginesFixture: Engine[] = [
  {
    name: 'llamacpp',
    installed: true,
    version: '8833 (45cac7ca7)',
    running: true,
    base_url: '',
    reason: 'winget llama-server on PATH',
    details: {
      resolved_path: 'llama-server',
      backends: ['RPC', 'Vulkan', 'CPU'],
      caps: ['fit', 'sleep-idle', 'flash-attn'],
      has_tq3: false,
    },
  },
  {
    name: 'vllm',
    installed: true,
    version: '',
    running: false,
    base_url: '',
    reason: 'image present (vllm/vllm-openai:latest), not started; launches in P4.5',
    details: { images: ['vllm/vllm-openai:latest'] },
  },
];

// Shapes mirror the daemon's ACTUAL GET /api/models projection (core.ModelSummary):
// nullable columns are OMITTED (omitempty) rather than sent as null, and `presence` is a
// dedicated always-present field.
export const modelsFixture: ModelSummary[] = [
  {
    id: 'gguf-qwen3-6-27b-tq3-4s',
    display_name: 'Qwen3.6-27B-TQ3_4S',
    source: 'local_path',
    format: 'gguf',
    quantization: 'tq3_4s',
    local_path: 'K:\\projects\\llm_models\\models\\Qwen3.6-27B-TQ3_4S.gguf',
    size_bytes: 14979000000,
    presence: 'present',
  },
];

// A verdict fixture matching compat.Verdict — a degraded TQ3 fallback (the real-box case).
export const verdictFixture: Verdict = {
  model: 'Qwen3.6-27B-TQ3_4S',
  status: 'runs_degraded',
  reason:
    'estimated ~14900 MiB at 8K ctx fits in 11960 MiB usable (16380 total × 0.92 safety − 3093 in use), but this is a FALLBACK estimate (gguf-parser could not parse the quant) and lands within ±15% of the fit boundary — treat as degraded until a measured VRAM peak confirms it.',
  recommended_engine: 'llamacpp',
  estimated_vram_mb: 14900,
  max_context_that_fits: 8192,
  requested_context: 8192,
  run_flags: ['-ngl', 'all', '-c', '8192', '--fit', 'off', '-fa', 'on'],
  confidence: 'fallback',
  estimator: 'gguf-parser-go@v0.24.1+fallback-heuristic',
  vram_free_mb: 13287,
  vram_total_mb: 16380,
  engine_choices: [
    {
      engine: 'llamacpp',
      detail: 'CUDA fork (has_tq3)',
      status: 'runs_degraded',
      estimated_vram_mb: 14900,
      confidence: 'fallback',
      reason: 'fallback near boundary',
    },
  ],
};

// ── Supply chain (Phase 3) fixtures ───────────────────────────────────────────
// The two-step HF gate: a resolved job awaiting confirmation, with a pinned commit + a
// pickle warning (the loud critical case).
export const awaitingJobFixture: DownloadJob = {
  id: 'job-1',
  kind: 'hf',
  state: 'awaiting_ok',
  repo: 'owner/model-GGUF',
  commit: 'e3a1c0f1234567890abcdef1234567890abcdef1',
  reason: 'resolved owner/model-GGUF@main → commit e3a1c0f123; 1 file(s), 400.0 MiB. Confirm to download.',
  resolved: {
    repo: 'owner/model-GGUF',
    ref: 'main',
    commit: 'e3a1c0f1234567890abcdef1234567890abcdef1',
    gated: false,
    license: 'apache-2.0',
    files: [
      { path: 'model-Q4_K_M.gguf', size_bytes: 419430400, selected: true },
      { path: 'README.md', size_bytes: 1024, selected: false },
    ],
    total_size_bytes: 419430400,
    warnings: [],
  },
  created_at: '2026-07-06T10:00:00Z',
  updated_at: '2026-07-06T10:00:01Z',
};

export const runningJobFixture: DownloadJob = {
  id: 'job-2',
  kind: 'hf',
  state: 'running',
  repo: 'owner/model-GGUF',
  commit: 'e3a1c0f1234567890abcdef1234567890abcdef1',
  progress: {
    file: 'model-Q4_K_M.gguf',
    bytes_done: 209715200,
    bytes_total: 419430400,
    file_index: 1,
    file_count: 1,
    phase: 'downloading',
  },
  created_at: '2026-07-06T10:00:00Z',
  updated_at: '2026-07-06T10:00:05Z',
};

export const doneJobFixture: DownloadJob = {
  id: 'job-3',
  kind: 'hf',
  state: 'succeeded',
  repo: 'owner/model-GGUF',
  commit: 'e3a1c0f1234567890abcdef1234567890abcdef1',
  manifest_id: 'man-1',
  model_id: 'hf-owner-model-gguf',
  reason: 'download complete; manifest written and linked to inventory',
  created_at: '2026-07-06T10:00:00Z',
  updated_at: '2026-07-06T10:01:00Z',
};

export const manifestFixture: Manifest = {
  id: 'man-1',
  source: 'hf',
  repo: 'owner/model-GGUF',
  revision: 'e3a1c0f1234567890abcdef1234567890abcdef1',
  format: 'gguf',
  quantization: 'q4_k_m',
  license: 'apache-2.0',
  files: [
    { path: 'model-Q4_K_M.gguf', sha256: 'abc123', size_bytes: 419430400 },
  ],
  security_warnings: [],
  downloaded_at: '2026-07-06T10:01:00Z',
};

export const reproduceFixture: ReproduceBundle = {
  model_id: 'hf-owner-model-gguf',
  display_name: 'model-GGUF',
  source: 'hf',
  provenance: 'reproducible — Hugging Face repo pinned to an exact commit SHA with per-file sha256 hashes.',
  repo: 'owner/model-GGUF',
  commit: 'e3a1c0f1234567890abcdef1234567890abcdef1',
  files: [{ path: 'model-Q4_K_M.gguf', sha256: 'abc123', size_bytes: 419430400 }],
  primary_sha256: 'abc123',
  license: 'apache-2.0',
  size_bytes: 419430400,
  reproduce_hint: 'download owner/model-GGUF at revision e3a1c0f1234567890abcdef1234567890abcdef1',
};
