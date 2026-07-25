// Live-connection abstraction (PLAN.md decision 7).
//
// Phase 1 ships SSE for /api/hardware/live (the daemon offers it now). A WebSocket
// client (/api/ws) is coded here too but DISABLED by default — the WS server side
// lands later (P1.5 Go follow-up or P6.2). Enabling WS is a single config flip
// (LiveConfig.transport = 'ws' | 'auto'); the contract it expects is documented in
// temp-research/wiring-p15.md.
//
// Both transports feed the SAME reducer via typed LiveEvent callbacks, so the UI is
// transport-agnostic. WS 'auto' mode probes /api/ws once; if it doesn't upgrade, it
// falls back to SSE cleanly with no lost frames on the SSE side.
//
// This file is pure DOM (EventSource/WebSocket) + timers — no React. It is unit-
// tested with a fake EventSource (connection.test.ts) for the reconnect state machine.

import type { Engine, HardwareSnapshot, ModelSummary } from '../api/types';

// ── Event vocabulary (research/lmwrapper-reuse-notes.md event framing) ────────
// Server pushes framed events; we normalize SSE `data:` frames and WS messages
// into this discriminated union. The Phase-1 SSE endpoint /api/hardware/live emits
// bare hardware snapshots (no `type` field) — we treat an untyped frame that looks
// like a HardwareSnapshot as `hardware.snapshot`.
// The server publishes these framed `type` strings over /api/ws (internal/core/events.go). SSE
// (/api/hardware/live) carries ONLY hardware; every OTHER invalidation arrives over WS, which is why
// WS is now the default transport (see DEFAULT_LIVE_CONFIG) — an SSE-only client would miss
// models/instances/downloads/queue/jobs updates and show stale inventory.
export type LiveEvent =
  | { type: 'heartbeat'; ts?: string }
  | { type: 'hardware.snapshot'; data: HardwareSnapshot }
  | { type: 'models.updated'; data: ModelSummary[] }
  | { type: 'engines.updated'; data: Engine[] }
  | { type: 'instances.updated' }
  | { type: 'queue.update' }
  | { type: 'jobs.updated' }
  | { type: 'download.progress' }
  | { type: 'download.done' };

export type ConnectionState =
  | 'idle'
  | 'connecting'
  | 'connected'
  | 'reconnecting'
  | 'closed';

export type Transport = 'sse' | 'ws';

export interface ConnectionStatus {
  state: ConnectionState;
  /** which transport is currently active/attempted. */
  transport: Transport;
  /** last time we received ANY frame (RFC3339 or ms epoch as ISO); null if never. */
  lastEventAt: string | null;
  /** consecutive failed attempts since the last successful open. */
  attempts: number;
  /** human-readable reason for the current non-connected state (mandatory-reason rule). */
  reason: string;
}

export interface LiveConfig {
  /**
   * 'sse'  — SSE only (Phase 1 default; WS server not shipped yet).
   * 'ws'   — WS only (flip when the Go /api/ws endpoint lands).
   * 'auto' — try WS once, fall back to SSE if it fails to open.
   */
  transport: 'sse' | 'ws' | 'auto';
  /** SSE URL (default /api/hardware/live). */
  sseUrl: string;
  /** WS URL (default derived from location: ws(s)://host/api/ws). */
  wsUrl: string;
  /** backoff schedule in ms (capped exponential); index clamps to last. */
  backoffMs: number[];
  /** if no frame arrives within this window, treat the link as stalled and reconnect. */
  stallTimeoutMs: number;
}

export const DEFAULT_LIVE_CONFIG: LiveConfig = {
  // WS is now the default: the Go /api/ws endpoint ships (internal/api/ws.go) and is the ONLY
  // transport that carries models/instances/downloads/queue/jobs invalidations — SSE
  // (/api/hardware/live) carries hardware only. A page loaded before the startup scan finishes would,
  // under SSE, miss models.updated and show stale inventory. 'auto' prefers WS and falls back to SSE
  // (hardware still flows) if the socket won't upgrade, so there is no regression on WS-less setups.
  transport: 'auto',
  sseUrl: '/api/hardware/live',
  wsUrl: defaultWsUrl(),
  backoffMs: [1000, 2000, 4000, 8000, 15000, 30000],
  stallTimeoutMs: 15000,
};

function defaultWsUrl(): string {
  if (typeof window === 'undefined' || !window.location) return '/api/ws';
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}/api/ws`;
}

type StatusListener = (s: ConnectionStatus) => void;
type EventListener = (e: LiveEvent) => void;

/**
 * LiveConnection owns one live link (SSE or WS) with auto-reconnect + capped
 * exponential backoff. It is deliberately framework-free: React binds to it via a
 * hook (useLive.ts). Call start() once; stop() on unmount.
 */
export class LiveConnection {
  private cfg: LiveConfig;
  private status: ConnectionStatus;
  private statusListeners = new Set<StatusListener>();
  private eventListeners = new Set<EventListener>();

  private es: EventSource | null = null;
  private ws: WebSocket | null = null;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private stallTimer: ReturnType<typeof setTimeout> | null = null;
  private stopped = false;
  /** in 'auto' mode, set true once WS proved unavailable so we stop retrying it. */
  private wsGaveUp = false;

  constructor(cfg?: Partial<LiveConfig>) {
    this.cfg = { ...DEFAULT_LIVE_CONFIG, ...cfg };
    this.status = {
      state: 'idle',
      transport: this.initialTransport(),
      lastEventAt: null,
      attempts: 0,
      reason: 'not started',
    };
  }

  getStatus(): ConnectionStatus {
    return { ...this.status };
  }

  onStatus(fn: StatusListener): () => void {
    this.statusListeners.add(fn);
    fn(this.getStatus());
    return () => this.statusListeners.delete(fn);
  }

  onEvent(fn: EventListener): () => void {
    this.eventListeners.add(fn);
    return () => this.eventListeners.delete(fn);
  }

  start(): void {
    this.stopped = false;
    this.open();
  }

  stop(): void {
    this.stopped = true;
    this.clearTimers();
    this.teardownTransport();
    this.setStatus({ state: 'closed', reason: 'stopped by client', attempts: 0 });
  }

  // ── internal ───────────────────────────────────────────────────────────────

  private initialTransport(): Transport {
    return this.cfg.transport === 'ws' || this.cfg.transport === 'auto'
      ? 'ws'
      : 'sse';
  }

  /** which transport this attempt should use, honoring 'auto' fallback. */
  private chooseTransport(): Transport {
    if (this.cfg.transport === 'ws') return 'ws';
    if (this.cfg.transport === 'sse') return 'sse';
    // auto: prefer WS until it gives up, then SSE.
    return this.wsGaveUp ? 'sse' : 'ws';
  }

  private open(): void {
    if (this.stopped) return;
    this.teardownTransport();
    const transport = this.chooseTransport();
    const connecting =
      this.status.attempts === 0 ? 'connecting' : 'reconnecting';
    this.setStatus({
      state: connecting,
      transport,
      reason:
        connecting === 'connecting'
          ? `opening ${transport.toUpperCase()} connection`
          : `reconnecting (attempt ${this.status.attempts + 1}) via ${transport.toUpperCase()}`,
    });
    if (transport === 'ws') this.openWs();
    else this.openSse();
  }

  private openSse(): void {
    let es: EventSource;
    try {
      es = new EventSource(this.cfg.sseUrl);
    } catch (e) {
      this.onTransportError('sse', `EventSource construction failed: ${errMsg(e)}`);
      return;
    }
    this.es = es;
    es.onopen = () => this.onOpen('sse');
    es.onmessage = (ev: MessageEvent) => this.onFrame(ev.data);
    es.onerror = () => {
      // EventSource auto-reconnects internally, but its cadence isn't ours and it
      // gives no reason. We take control: close and schedule our own backoff.
      this.onTransportError('sse', 'SSE stream error or server closed the connection');
    };
    this.armStallTimer();
  }

  private openWs(): void {
    let ws: WebSocket;
    try {
      ws = new WebSocket(this.cfg.wsUrl);
    } catch (e) {
      this.onWsUnavailable(`WebSocket construction failed: ${errMsg(e)}`);
      return;
    }
    this.ws = ws;
    ws.onopen = () => this.onOpen('ws');
    ws.onmessage = (ev: MessageEvent) => this.onFrame(ev.data);
    ws.onerror = () => {
      // Distinguish "WS never worked" (auto → fall back) from a mid-session drop.
      if (this.status.state !== 'connected') {
        this.onWsUnavailable('WebSocket failed to open');
      } else {
        this.onTransportError('ws', 'WebSocket error');
      }
    };
    ws.onclose = () => {
      if (this.status.state === 'connected') {
        this.onTransportError('ws', 'WebSocket closed by server');
      }
    };
    this.armStallTimer();
  }

  private onOpen(transport: Transport): void {
    this.setStatus({
      state: 'connected',
      transport,
      attempts: 0,
      reason: `${transport.toUpperCase()} connected`,
    });
    this.armStallTimer();
  }

  private onFrame(raw: unknown): void {
    this.status.lastEventAt = new Date().toISOString();
    // Refresh connected status timestamp + reset stall timer on every frame.
    this.emitStatus();
    this.armStallTimer();
    const evt = parseFrame(raw);
    if (!evt) return;
    for (const fn of this.eventListeners) fn(evt);
  }

  /** WS proved unavailable. In 'auto', give up on WS and fall back to SSE now. */
  private onWsUnavailable(reason: string): void {
    if (this.cfg.transport === 'auto' && !this.wsGaveUp) {
      this.wsGaveUp = true;
      this.teardownTransport();
      this.setStatus({
        state: this.status.attempts === 0 ? 'connecting' : 'reconnecting',
        transport: 'sse',
        reason: `WebSocket unavailable (${reason}); falling back to SSE`,
      });
      this.openSse();
      return;
    }
    this.onTransportError('ws', reason);
  }

  private onTransportError(transport: Transport, reason: string): void {
    if (this.stopped) return;
    this.teardownTransport();
    const attempts = this.status.attempts + 1;
    const delay = this.backoffFor(attempts);
    this.setStatus({
      state: 'reconnecting',
      transport,
      attempts,
      reason: `${reason}. Retrying in ${Math.round(delay / 1000)}s (attempt ${attempts}).`,
    });
    this.scheduleReconnect(delay);
  }

  private backoffFor(attempts: number): number {
    const arr = this.cfg.backoffMs;
    const idx = Math.min(attempts - 1, arr.length - 1);
    return arr[Math.max(0, idx)];
  }

  private scheduleReconnect(delayMs: number): void {
    this.clearReconnect();
    this.reconnectTimer = setTimeout(() => this.open(), delayMs);
  }

  private armStallTimer(): void {
    this.clearStall();
    if (this.cfg.stallTimeoutMs <= 0) return;
    this.stallTimer = setTimeout(() => {
      // No frame in stallTimeoutMs → assume a dead link and force a reconnect.
      this.onTransportError(
        this.status.transport,
        `no data for ${Math.round(this.cfg.stallTimeoutMs / 1000)}s (stalled)`,
      );
    }, this.cfg.stallTimeoutMs);
  }

  private teardownTransport(): void {
    this.clearStall();
    if (this.es) {
      this.es.onopen = null;
      this.es.onmessage = null;
      this.es.onerror = null;
      try {
        this.es.close();
      } catch {
        /* ignore */
      }
      this.es = null;
    }
    if (this.ws) {
      this.ws.onopen = null;
      this.ws.onmessage = null;
      this.ws.onerror = null;
      this.ws.onclose = null;
      try {
        this.ws.close();
      } catch {
        /* ignore */
      }
      this.ws = null;
    }
  }

  private clearTimers(): void {
    this.clearReconnect();
    this.clearStall();
  }

  private clearReconnect(): void {
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  private clearStall(): void {
    if (this.stallTimer) {
      clearTimeout(this.stallTimer);
      this.stallTimer = null;
    }
  }

  private setStatus(patch: Partial<ConnectionStatus>): void {
    this.status = { ...this.status, ...patch };
    this.emitStatus();
  }

  private emitStatus(): void {
    const snap = this.getStatus();
    for (const fn of this.statusListeners) fn(snap);
  }
}

/**
 * Parse a raw SSE `data:` string or WS message into a LiveEvent.
 * - Bare hardware snapshot (Phase-1 /api/hardware/live, no `type`) → hardware.snapshot.
 * - Framed `{type, data|...}` → the matching event.
 * Returns null for heartbeats-with-no-payload we don't need to fan out, or junk.
 */
export function parseFrame(raw: unknown): LiveEvent | null {
  let obj: unknown;
  if (typeof raw === 'string') {
    const s = raw.trim();
    if (!s || s === ':' || s.startsWith(':')) return null; // SSE comment / keep-alive
    try {
      obj = JSON.parse(s);
    } catch {
      return null;
    }
  } else if (raw && typeof raw === 'object') {
    obj = raw;
  } else {
    return null;
  }

  if (!obj || typeof obj !== 'object') return null;
  const rec = obj as Record<string, unknown>;

  // Framed event with explicit type (WS / future SSE).
  if (typeof rec.type === 'string') {
    switch (rec.type) {
      case 'heartbeat':
        return { type: 'heartbeat', ts: asString(rec.ts) };
      case 'hardware.snapshot':
        return { type: 'hardware.snapshot', data: unwrap(rec) as HardwareSnapshot };
      case 'models.updated':
        return { type: 'models.updated', data: (rec.data as ModelSummary[]) ?? [] };
      case 'engines.updated':
        return { type: 'engines.updated', data: (rec.data as Engine[]) ?? [] };
      case 'instances.updated':
        return { type: 'instances.updated' };
      // Signal-only invalidations (no payload; the subscriber refetches the matching REST list).
      // These arrive over WS only — the SSE hardware stream never carries them.
      case 'queue.update':
        return { type: 'queue.update' };
      case 'jobs.updated':
        return { type: 'jobs.updated' };
      case 'download.progress':
        return { type: 'download.progress' };
      case 'download.done':
        return { type: 'download.done' };
      default:
        return null;
    }
  }

  // Untyped frame that looks like a hardware snapshot (Phase-1 SSE endpoint).
  if (looksLikeHardware(rec)) {
    return { type: 'hardware.snapshot', data: rec as unknown as HardwareSnapshot };
  }
  return null;
}

function unwrap(rec: Record<string, unknown>): unknown {
  // Support both {type, data:{...}} and {type, ...snapshotFields}.
  if ('data' in rec && rec.data && typeof rec.data === 'object') return rec.data;
  const { type: _type, ...rest } = rec;
  void _type;
  return rest;
}

function looksLikeHardware(rec: Record<string, unknown>): boolean {
  return Array.isArray(rec.gpus) && 'captured_at' in rec;
}

function asString(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
