import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, isQueuedResponse } from './client';
import type { InstanceView, LaunchState, ModelSummary, QueuedResponse } from './types';

// FINDING-5 gate: POST /api/instances can answer 202 {status:'queued', queue_id, position,
// blocked_reason} when residency queues a start on VRAM. startInstance must surface that shape (NOT
// cast it to a bogus InstanceView), so the UI can show the queue position + reason.

function mockFetch(status: number, body: unknown) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () =>
      new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
    ),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('startInstance 202 queued handling', () => {
  it('returns the queued shape on a 202 (not a bogus InstanceView)', async () => {
    const queued: QueuedResponse = {
      status: 'queued',
      queue_id: 'q-123',
      position: 2,
      blocked_reason: 'needs 9000 MiB, free 2000 MiB',
    };
    mockFetch(202, queued);

    const res = await api.startInstance({ model_id: 'm1' });
    expect(isQueuedResponse(res)).toBe(true);
    if (isQueuedResponse(res)) {
      expect(res.queue_id).toBe('q-123');
      expect(res.position).toBe(2);
      expect(res.blocked_reason).toContain('9000 MiB');
    }
  });

  it('returns an InstanceView on a 200 (not queued)', async () => {
    const inst: InstanceView = {
      id: 'i-1',
      model_id: 'm1',
      engine: 'llamacpp',
      state: 'HEALTHY',
      pinned: false,
      priority: 1,
    };
    mockFetch(200, inst);

    const res = await api.startInstance({ model_id: 'm1' });
    expect(isQueuedResponse(res)).toBe(false);
    if (!isQueuedResponse(res)) {
      expect(res.id).toBe('i-1');
      expect(res.state).toBe('HEALTHY');
    }
  });
});

// allow_cpu_offload wire-cleanliness: default (or explicit false) sends NO field; only an opt-in
// (options.allowCpuOffload:true) adds allow_cpu_offload:true to the POST body. The flag relaxes
// launch flags only, not admission (docs/api-spec.md §Instances).
describe('startInstance allow_cpu_offload wire', () => {
  function captureFetch() {
    const spy = vi.fn(async (_path: string, _init?: RequestInit) =>
      new Response(JSON.stringify({ id: 'i-1', model_id: 'm1', engine: 'llamacpp', state: 'HEALTHY', pinned: false, priority: 0 }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    vi.stubGlobal('fetch', spy);
    return spy;
  }
  function bodyOf(spy: ReturnType<typeof captureFetch>): Record<string, unknown> {
    const init = spy.mock.calls[0]?.[1];
    return JSON.parse(String(init?.body ?? '{}'));
  }

  it('omits allow_cpu_offload by default (no options → clean wire)', async () => {
    const spy = captureFetch();
    await api.startInstance({ model_id: 'm1' });
    expect(bodyOf(spy)).not.toHaveProperty('allow_cpu_offload');
  });

  it('omits allow_cpu_offload when the option is explicitly false', async () => {
    const spy = captureFetch();
    await api.startInstance({ model_id: 'm1' }, { allowCpuOffload: false });
    expect(bodyOf(spy)).not.toHaveProperty('allow_cpu_offload');
  });

  it('sends allow_cpu_offload:true when the option is true', async () => {
    const spy = captureFetch();
    await api.startInstance({ model_id: 'm1' }, { allowCpuOffload: true });
    expect(bodyOf(spy)).toMatchObject({ model_id: 'm1', allow_cpu_offload: true });
  });
});

// PUT /api/models/{id}/launch-state — the residency control the Inventory panel exposes.
describe('setModelLaunchState', () => {
  it('PUTs {state} to the launch-state endpoint and returns the updated summary', async () => {
    const updated: ModelSummary = {
      id: 'm1',
      display_name: 'Model One',
      source: 'local_path',
      format: 'gguf',
      presence: 'present',
      launch_state: 'unavailable',
      auto_load_disabled: true,
    };
    const spy = vi.fn(async (_path: string, _init?: RequestInit) =>
      new Response(JSON.stringify(updated), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    vi.stubGlobal('fetch', spy);

    const res = await api.setModelLaunchState('m1', 'unavailable');

    expect(spy).toHaveBeenCalledWith(
      '/api/models/m1/launch-state',
      expect.objectContaining({ method: 'PUT' }),
    );
    const init = spy.mock.calls[0]?.[1];
    expect(JSON.parse(String(init?.body ?? '{}'))).toEqual({ state: 'unavailable' });
    expect(res).toEqual(updated);
  });

  it('URL-encodes the model id', async () => {
    const spy = vi.fn(async () =>
      new Response(JSON.stringify({}), { status: 200 }),
    );
    vi.stubGlobal('fetch', spy);
    await api.setModelLaunchState('weird id/slash', 'available');
    expect(spy).toHaveBeenCalledWith(
      '/api/models/weird%20id%2Fslash/launch-state',
      expect.anything(),
    );
  });

  it('rejects with an ApiError carrying the daemon reason on a non-2xx response', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            error: {
              code: 'launch_state_invalid',
              message: 'bad state',
              reason: "REFUSED: 'bogus' is not a recognized launch state",
            },
          }),
          { status: 400, headers: { 'Content-Type': 'application/json' } },
        ),
      ),
    );
    await expect(
      api.setModelLaunchState('m1', 'bogus' as unknown as LaunchState),
    ).rejects.toMatchObject({
      reason: "REFUSED: 'bogus' is not a recognized launch state",
      status: 400,
    });
  });
});
