import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import App from './App';
import { enginesFixture, hardwareFixture, modelsFixture } from './test/fixtures';

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
      expect(screen.getByText('Ollama')).toBeInTheDocument(),
    );
    expect(screen.getByText('llama.cpp')).toBeInTheDocument();
    expect(screen.getByText('vLLM')).toBeInTheDocument();

    // Inventory fetched.
    await waitFor(() =>
      expect(
        screen.getAllByText('Qwen3.6-27B-TQ3_4S').length,
      ).toBeGreaterThan(0),
    );
  });
});
