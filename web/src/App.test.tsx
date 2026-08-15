import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import App from './App';
import {
  enginesFixture,
  hardwareFixture,
  modelsFixture,
  verdictFixture,
} from './test/fixtures';
import { resetCompatCache } from './api/useCompatVerdicts';

// Smoke test: App mounts end-to-end with mocked fetch + EventSource (no daemon).
// Proves the panels wire together and render real data without runtime errors.

class FakeEventSource {
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(public url: string) {
    // Deliver the first hardware frame on next tick, like a real SSE open+message.
    setTimeout(() => {
      this.onopen?.(new Event('open'));
      this.onmessage?.(
        new MessageEvent('message', { data: JSON.stringify(hardwareFixture) }),
      );
    }, 0);
  }
  close() {}
}

describe('App (smoke)', () => {
  beforeEach(() => {
    // The compat cache is MODULE-scoped (shared by the Instances picker and the Inventory table),
    // so it must be cleared between tests or a verdict fetched by one leaks into the next.
    resetCompatCache();
    // @ts-expect-error test double
    globalThis.EventSource = FakeEventSource;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        const body =
          url.includes('/api/instances')
            ? []
            : url.includes('/api/engines')
              ? enginesFixture
              : // Must precede the /api/models branch: the per-model compat URL is
                // /api/models/{id}/compat and would otherwise match the list route.
                url.includes('/compat')
                ? verdictFixture
                : url.includes('/api/models')
                  ? modelsFixture
                  : url.includes('/api/hardware')
                    ? hardwareFixture
                    : {};
        return new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }),
    );
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    // @ts-expect-error cleanup
    delete globalThis.EventSource;
  });

  it('renders header, GPU, engines, and inventory with live + fetched data', async () => {
    render(<App />);

    // Header brand always present (logo splits LocalHub / Compass into spans).
    expect(screen.getAllByText(/LocalHub/).length).toBeGreaterThan(0);

    // GPU panel paints once the SSE frame arrives.
    await waitFor(() =>
      expect(
        screen.getByText('NVIDIA GeForce RTX 4060 Ti'),
      ).toBeInTheDocument(),
    );
    // Header shows a live connection badge.
    expect(screen.getByText(/SSE live/i)).toBeInTheDocument();

    // Engines fetched.
    await waitFor(() =>
      expect(screen.queryByText('Ollama')).not.toBeInTheDocument(),
    );
    expect(screen.getByText('llama.cpp')).toBeInTheDocument();
    expect(screen.getByText('vLLM')).toBeInTheDocument();

    // Inventory fetched.
    await waitFor(() =>
      expect(
        screen.getAllByText('Qwen3.6-27B-TQ3_4S').length,
      ).toBeGreaterThan(0),
    );

    // §3.3: rows carry a REAL verdict fetched from /api/models/{id}/compat, not "unchecked".
    await waitFor(() =>
      expect(screen.getAllByText('degraded').length).toBeGreaterThan(0),
    );

    // §3.1: the admission budget is on screen beside raw free VRAM — this is the number a start
    // is checked against, and the whole reason a refusal can otherwise look arbitrary.
    expect(screen.getByText(/Usable for a start/i)).toBeInTheDocument();
  });
});
