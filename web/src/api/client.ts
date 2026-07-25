// Typed fetch layer. Same-origin in production (Go serves API + static from one
// origin); Vite dev-proxy forwards /api → 127.0.0.1:39876 in dev.
//
// Every error surfaces a human-readable `reason` when the daemon provides one
// (error envelope {error:{code,message,reason}}), per the mandatory-reason rule.

import type {
  ApiErrorBody,
  CompatPreviewRequest,
  DownloadJob,
  DownloadRequest,
  Engine,
  HardwareSnapshot,
  InstanceView,
  Manifest,
  ModelSummary,
  QueuedResponse,
  QueueEntry,
  ReproduceBundle,
  RescanResult,
  StartInstanceRequest,
  Verdict,
} from './types';

/** Error carrying the daemon's code/message/reason (or a transport-level reason). */
export class ApiError extends Error {
  readonly code: string;
  /** Human-readable "why" — always populated (falls back to message). */
  readonly reason: string;
  readonly status: number;

  constructor(code: string, message: string, reason: string, status: number) {
    super(message);
    this.name = 'ApiError';
    this.code = code;
    this.reason = reason;
    this.status = status;
  }
}

const dashboardTokenKey = 'localhub.dashboard.admin-token';

/** Session-only storage: never localStorage, a URL, or a cookie. */
export function dashboardToken(): string | null {
  if (typeof window === 'undefined') return null;
  return window.sessionStorage.getItem(dashboardTokenKey);
}

export function clearDashboardToken(): void {
  if (typeof window !== 'undefined') window.sessionStorage.removeItem(dashboardTokenKey);
}

function isErrorBody(v: unknown): v is ApiErrorBody {
  return (
    typeof v === 'object' &&
    v !== null &&
    'error' in v &&
    typeof (v as { error: unknown }).error === 'object' &&
    (v as { error: unknown }).error !== null
  );
}

/**
 * Type guard for the 202 queued-start shape returned by POST /api/instances when residency queues
 * the start. Discriminates on `status === 'queued'` (an InstanceView never carries that field).
 */
export function isQueuedResponse(
  v: InstanceView | QueuedResponse,
): v is QueuedResponse {
  return (v as QueuedResponse).status === 'queued';
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response;
  try {
    const headers = new Headers(init?.headers);
    headers.set('Accept', 'application/json');
    const token = dashboardToken();
    if (token) headers.set('Authorization', `Bearer ${token}`);
    res = await fetch(path, { ...init, headers });
  } catch (e) {
    // Network / CORS / daemon-down. No envelope available — synthesize a reason.
    const detail = e instanceof Error ? e.message : String(e);
    throw new ApiError(
      'network_error',
      `Request to ${path} failed`,
      `Could not reach the daemon (${detail}). Is localhub running on 127.0.0.1:39876?`,
      0,
    );
  }

  const text = await res.text();
  let body: unknown = undefined;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = undefined;
    }
  }

  if (!res.ok) {
    if (isErrorBody(body)) {
      const { code, message, reason } = body.error;
      throw new ApiError(
        code || 'error',
        message || res.statusText,
        reason || message || res.statusText,
        res.status,
      );
    }
    throw new ApiError(
      'http_error',
      `HTTP ${res.status} ${res.statusText}`,
      text || `The daemon returned ${res.status} with no reason.`,
      res.status,
    );
  }

  return body as T;
}

export const api = {
  dashboardAuthStatus: () => request<{ auth_required: boolean }>('/api/auth/dashboard'),
  dashboardLogin: async (token: string) => {
    if (typeof window !== 'undefined') window.sessionStorage.setItem(dashboardTokenKey, token);
    try {
      return await request<{ authenticated: boolean }>('/api/auth/dashboard', { method: 'POST' });
    } catch (e) {
      clearDashboardToken();
      throw e;
    }
  },
  dashboardLogout: async () => {
    try {
      await request<{ authenticated: boolean }>('/api/auth/dashboard', { method: 'DELETE' });
    } finally {
      clearDashboardToken();
    }
  },
  hardware: () => request<HardwareSnapshot>('/api/hardware'),
  engines: () => request<Engine[]>('/api/engines'),
  models: () => request<ModelSummary[]>('/api/models'),
  /** Control-plane rescan (walks disk) — NOT engine mutation. */
  rescanModels: () =>
    request<RescanResult>('/api/models/rescan', { method: 'POST' }),
  /** Compatibility verdict for one inventory model (cached onto its row). */
  modelCompat: (id: string, ctx?: number) =>
    request<Verdict>(
      `/api/models/${encodeURIComponent(id)}/compat${ctx ? `?ctx=${ctx}` : ''}`,
    ),
  /** Compatibility preview WITHOUT downloading — inventory model or remote HF ref. */
  compatPreview: (req: CompatPreviewRequest) =>
    request<Verdict>('/api/compat/preview', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(req),
    }),

  // ── Supply chain (Phase 3) ──────────────────────────────────────────────────
  /**
   * Create a download/import. For HF, omit `confirm` to RESOLVE + show the pinned commit
   * (two-step gate), then call again with the same repo/ref and `confirm:true` to download.
   */
  createDownload: (req: DownloadRequest) =>
    request<DownloadJob>('/api/downloads', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(req),
    }),
  /** List tracked download jobs. */
  downloads: () => request<DownloadJob[]>('/api/downloads'),
  /** One download job's status. */
  downloadStatus: (id: string) =>
    request<DownloadJob>(`/api/downloads/${encodeURIComponent(id)}`),
  /** The manifest linked to a model (404 for a discovered pre-existing file). */
  modelManifest: (id: string) =>
    request<Manifest>(`/api/models/${encodeURIComponent(id)}/manifest`),
  /** The reproduce export for a model (works for every source kind). */
  reproduce: (id: string) =>
    request<ReproduceBundle>(`/api/models/${encodeURIComponent(id)}/reproduce`),

  // ── Instances (Phase 4 — ADMIN: spawn/kill GPU processes) ───────────────────
  /** List running + recent instances. */
  instances: () => request<InstanceView[]>('/api/instances'),
  /**
   * Start a model instance. Three outcomes:
   *  - 200 → an {@link InstanceView} (spawned / already warm).
   *  - 202 → a {@link QueuedResponse} ({status:'queued', queue_id, position, blocked_reason}) when
   *    residency queued the start on VRAM. The UI surfaces the position + reason (NOT a bogus
   *    InstanceView — the daemon spawned nothing yet).
   *  - 409 → a safety-refused start (verdict not runs/degraded, or estimate over budget) rejects
   *    with an ApiError whose `reason` carries the numeric why — nothing is spawned.
   * Callers discriminate on the `status: 'queued'` field (present only on the 202 shape); use the
   * {@link isQueuedResponse} guard.
   *
   * `options.allowCpuOffload` maps to the body's `allow_cpu_offload`. The default is full GPU
   * (never split to CPU): we send the field ONLY when the caller opts in (true), keeping the wire
   * clean for the default. The flag relaxes launch flags only — the admission VRAM verdict is
   * unchanged (docs/api-spec.md §Instances).
   */
  startInstance: (
    req: StartInstanceRequest,
    options?: { allowCpuOffload?: boolean },
  ) =>
    request<InstanceView | QueuedResponse>('/api/instances', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(
        options?.allowCpuOffload
          ? { ...req, allow_cpu_offload: true }
          : req,
      ),
    }),
  /** Stop an instance (drain → stop with VRAM-delta verification). */
  stopInstance: (id: string) =>
    request<InstanceView>(`/api/instances/${encodeURIComponent(id)}`, {
      method: 'DELETE',
    }),

  // ── Residency queue (Phase 5) ───────────────────────────────────────────────
  /** List admission-queue entries (WAITING + recent) with blocked_reason + position. */
  queue: () => request<QueueEntry[]>('/api/queue'),
  /** Cancel a WAITING queue entry. */
  cancelQueueEntry: (id: string) =>
    request<{ status: string; id: string }>(
      `/api/queue/${encodeURIComponent(id)}`,
      { method: 'DELETE' },
    ),
};
