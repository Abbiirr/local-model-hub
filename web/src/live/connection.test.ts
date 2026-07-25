import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  LiveConnection,
  parseFrame,
  type ConnectionStatus,
  type LiveEvent,
} from './connection';
import { hardwareFixture } from '../test/fixtures';

// ── Fake EventSource ──────────────────────────────────────────────────────────
// Minimal controllable double: tracks instances, lets tests drive open/message/error.
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  url: string;
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  closed = false;

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }
  close() {
    this.closed = true;
  }
  // test helpers
  emitOpen() {
    this.onopen?.(new Event('open'));
  }
  emitMessage(data: unknown) {
    this.onmessage?.(
      new MessageEvent('message', {
        data: typeof data === 'string' ? data : JSON.stringify(data),
      }),
    );
  }
  emitError() {
    this.onerror?.(new Event('error'));
  }
  static reset() {
    FakeEventSource.instances = [];
  }
  static get last(): FakeEventSource {
    return FakeEventSource.instances[FakeEventSource.instances.length - 1];
  }
}

describe('parseFrame', () => {
  it('parses a bare hardware snapshot (Phase-1 SSE, no type field)', () => {
    const evt = parseFrame(JSON.stringify(hardwareFixture));
    expect(evt).not.toBeNull();
    expect(evt?.type).toBe('hardware.snapshot');
    if (evt?.type === 'hardware.snapshot') {
      expect(evt.data.gpus[0].vram_free).toBe(13287);
    }
  });

  it('parses a framed hardware.snapshot with data envelope', () => {
    const evt = parseFrame({ type: 'hardware.snapshot', data: hardwareFixture });
    expect(evt?.type).toBe('hardware.snapshot');
  });

  it('parses heartbeat', () => {
    expect(parseFrame({ type: 'heartbeat', ts: 'x' })).toEqual({
      type: 'heartbeat',
      ts: 'x',
    });
  });

  it('parses models.updated / engines.updated', () => {
    expect(parseFrame({ type: 'models.updated', data: [] })?.type).toBe(
      'models.updated',
    );
    expect(parseFrame({ type: 'engines.updated', data: [] })?.type).toBe(
      'engines.updated',
    );
  });

  // FINDING-6 gate: the server publishes queue/jobs/download invalidations over /api/ws
  // (internal/core/events.go). parseFrame must recognize each so the subscriber can refetch — an
  // unrecognized frame is dropped and the UI goes stale.
  it('parses the WS-only invalidation frames (queue/jobs/downloads)', () => {
    expect(parseFrame({ type: 'queue.update' })?.type).toBe('queue.update');
    expect(parseFrame({ type: 'jobs.updated' })?.type).toBe('jobs.updated');
    expect(parseFrame({ type: 'download.progress' })?.type).toBe(
      'download.progress',
    );
    expect(parseFrame({ type: 'download.done' })?.type).toBe('download.done');
  });

  it('ignores SSE comments, blanks, and junk', () => {
    expect(parseFrame(':keep-alive')).toBeNull();
    expect(parseFrame('')).toBeNull();
    expect(parseFrame('not json')).toBeNull();
    expect(parseFrame({ type: 'unknown' })).toBeNull();
    expect(parseFrame(42)).toBeNull();
  });
});

describe('LiveConnection SSE state machine', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    FakeEventSource.reset();
    // @ts-expect-error installing test double
    globalThis.EventSource = FakeEventSource;
  });
  afterEach(() => {
    vi.useRealTimers();
    // @ts-expect-error cleanup
    delete globalThis.EventSource;
  });

  function track(conn: LiveConnection) {
    const states: ConnectionStatus[] = [];
    conn.onStatus((s) => states.push(s));
    return states;
  }

  it('transitions idle → connecting → connected on open', () => {
    const conn = new LiveConnection({ transport: 'sse' });
    const states = track(conn);
    conn.start();
    expect(conn.getStatus().state).toBe('connecting');
    FakeEventSource.last.emitOpen();
    expect(conn.getStatus().state).toBe('connected');
    expect(conn.getStatus().transport).toBe('sse');
    expect(states.map((s) => s.state)).toContain('connected');
    conn.stop();
  });

  it('delivers hardware events to onEvent listeners', () => {
    const conn = new LiveConnection({ transport: 'sse' });
    const events: LiveEvent[] = [];
    conn.onEvent((e) => events.push(e));
    conn.start();
    FakeEventSource.last.emitOpen();
    FakeEventSource.last.emitMessage(hardwareFixture);
    expect(events).toHaveLength(1);
    expect(events[0].type).toBe('hardware.snapshot');
    expect(conn.getStatus().lastEventAt).not.toBeNull();
    conn.stop();
  });

  it('on error → reconnecting with backoff, then reopens after the delay', () => {
    const conn = new LiveConnection({
      transport: 'sse',
      backoffMs: [1000, 2000, 4000],
      stallTimeoutMs: 0, // disable stall timer for deterministic backoff test
    });
    conn.start();
    FakeEventSource.last.emitOpen();
    expect(conn.getStatus().state).toBe('connected');

    // first failure → reconnecting, attempts=1, delay=1000
    FakeEventSource.last.emitError();
    expect(conn.getStatus().state).toBe('reconnecting');
    expect(conn.getStatus().attempts).toBe(1);
    expect(FakeEventSource.last.closed).toBe(true);

    const before = FakeEventSource.instances.length;
    vi.advanceTimersByTime(1000);
    expect(FakeEventSource.instances.length).toBe(before + 1); // reopened
    conn.stop();
  });

  it('applies capped exponential backoff across repeated failures', () => {
    const conn = new LiveConnection({
      transport: 'sse',
      backoffMs: [1000, 2000, 4000],
      stallTimeoutMs: 0,
    });
    conn.start();

    // fail before ever opening: attempt 1 → 1000ms
    FakeEventSource.last.emitError();
    expect(conn.getStatus().attempts).toBe(1);
    vi.advanceTimersByTime(1000);

    // attempt 2 → 2000ms
    FakeEventSource.last.emitError();
    expect(conn.getStatus().attempts).toBe(2);
    let n = FakeEventSource.instances.length;
    vi.advanceTimersByTime(1999);
    expect(FakeEventSource.instances.length).toBe(n); // not yet
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances.length).toBe(n + 1);

    // attempt 3 → 4000ms; attempt 4 stays capped at 4000
    FakeEventSource.last.emitError();
    expect(conn.getStatus().attempts).toBe(3);
    vi.advanceTimersByTime(4000);
    FakeEventSource.last.emitError();
    expect(conn.getStatus().attempts).toBe(4);
    n = FakeEventSource.instances.length;
    vi.advanceTimersByTime(4000); // capped, not 8000
    expect(FakeEventSource.instances.length).toBe(n + 1);
    conn.stop();
  });

  it('resets attempts to 0 after a successful reopen', () => {
    const conn = new LiveConnection({
      transport: 'sse',
      backoffMs: [500],
      stallTimeoutMs: 0,
    });
    conn.start();
    FakeEventSource.last.emitOpen();
    FakeEventSource.last.emitError();
    expect(conn.getStatus().attempts).toBe(1);
    vi.advanceTimersByTime(500);
    FakeEventSource.last.emitOpen(); // reconnected
    expect(conn.getStatus().state).toBe('connected');
    expect(conn.getStatus().attempts).toBe(0);
    conn.stop();
  });

  it('a stalled link (no frames) forces a reconnect', () => {
    const conn = new LiveConnection({
      transport: 'sse',
      backoffMs: [1000],
      stallTimeoutMs: 5000,
    });
    conn.start();
    FakeEventSource.last.emitOpen();
    expect(conn.getStatus().state).toBe('connected');
    // no messages for 5s → stall → reconnecting
    vi.advanceTimersByTime(5000);
    expect(conn.getStatus().state).toBe('reconnecting');
    expect(conn.getStatus().reason).toMatch(/stalled/i);
    conn.stop();
  });

  it('stop() closes the transport and cancels reconnect', () => {
    const conn = new LiveConnection({
      transport: 'sse',
      backoffMs: [1000],
      stallTimeoutMs: 0,
    });
    conn.start();
    FakeEventSource.last.emitOpen();
    FakeEventSource.last.emitError(); // schedule reconnect
    const n = FakeEventSource.instances.length;
    conn.stop();
    expect(conn.getStatus().state).toBe('closed');
    vi.advanceTimersByTime(60000);
    expect(FakeEventSource.instances.length).toBe(n); // no new connections
  });

  it('every non-connected status carries a human-readable reason', () => {
    const conn = new LiveConnection({ transport: 'sse', stallTimeoutMs: 0 });
    const states = track(conn);
    conn.start();
    FakeEventSource.last.emitError();
    for (const s of states) {
      if (s.state !== 'connected') {
        expect(s.reason.length).toBeGreaterThan(0);
      }
    }
    conn.stop();
  });
});

// ── WS 'auto' fallback ────────────────────────────────────────────────────────
class FakeWebSocket {
  static instances: FakeWebSocket[] = [];
  url: string;
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  closed = false;
  constructor(url: string) {
    this.url = url;
    FakeWebSocket.instances.push(this);
  }
  close() {
    this.closed = true;
  }
  emitOpen() {
    this.onopen?.(new Event('open'));
  }
  emitMessage(data: unknown) {
    this.onmessage?.(
      new MessageEvent('message', {
        data: typeof data === 'string' ? data : JSON.stringify(data),
      }),
    );
  }
  emitError() {
    this.onerror?.(new Event('error'));
  }
  static reset() {
    FakeWebSocket.instances = [];
  }
  static get last(): FakeWebSocket {
    return FakeWebSocket.instances[FakeWebSocket.instances.length - 1];
  }
}

describe('LiveConnection auto transport WS→SSE fallback', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    FakeEventSource.reset();
    FakeWebSocket.reset();
    // @ts-expect-error test doubles
    globalThis.EventSource = FakeEventSource;
    // @ts-expect-error test doubles
    globalThis.WebSocket = FakeWebSocket;
  });
  afterEach(() => {
    vi.useRealTimers();
    // @ts-expect-error cleanup
    delete globalThis.EventSource;
    // @ts-expect-error cleanup
    delete globalThis.WebSocket;
  });

  it('falls back to SSE when WS fails to open, and connects', () => {
    const conn = new LiveConnection({
      transport: 'auto',
      wsUrl: 'ws://x/api/ws',
      sseUrl: '/api/hardware/live',
      stallTimeoutMs: 0,
    });
    conn.start();
    // first attempt is WS
    expect(FakeWebSocket.instances.length).toBe(1);
    expect(conn.getStatus().transport).toBe('ws');
    // WS errors before opening → fall back to SSE immediately
    FakeWebSocket.instances[0].emitError();
    expect(conn.getStatus().transport).toBe('sse');
    expect(FakeEventSource.instances.length).toBe(1);
    FakeEventSource.last.emitOpen();
    expect(conn.getStatus().state).toBe('connected');
    expect(conn.getStatus().transport).toBe('sse');
    conn.stop();
  });

  // FINDING-6 gate: a WS connection must deliver each new invalidation frame type to onEvent
  // subscribers (queue/jobs/downloads) — these ride WS only, so dropping them leaves stale UI.
  it('dispatches each WS invalidation frame type to onEvent', () => {
    const conn = new LiveConnection({ transport: 'ws', stallTimeoutMs: 0 });
    const events: LiveEvent[] = [];
    conn.onEvent((e) => events.push(e));
    conn.start();
    FakeWebSocket.last.emitOpen();
    expect(conn.getStatus().transport).toBe('ws');

    for (const type of [
      'models.updated',
      'instances.updated',
      'queue.update',
      'jobs.updated',
      'download.progress',
      'download.done',
    ]) {
      FakeWebSocket.last.emitMessage(
        type === 'models.updated' ? { type, data: [] } : { type },
      );
    }
    expect(events.map((e) => e.type)).toEqual([
      'models.updated',
      'instances.updated',
      'queue.update',
      'jobs.updated',
      'download.progress',
      'download.done',
    ]);
    conn.stop();
  });
});
