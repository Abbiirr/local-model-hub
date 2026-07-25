import { useState } from 'react';
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import {
  render,
  screen,
  within,
  fireEvent,
  waitFor,
  cleanup,
} from '@testing-library/react';
import { InventoryPanel } from './InventoryPanel';
import { modelsFixture } from '../test/fixtures';
import type { ModelSummary } from '../api/types';

// Mock the typed client so Run/Preview calls are observable. isQueuedResponse + ApiError are
// real (we discriminate + throw with them); only the network methods are stubbed. Render-only
// tests below don't touch these methods, so the mock is inert for them.
vi.mock('../api/client', async () => {
  const actual =
    await vi.importActual<typeof import('../api/client')>('../api/client');
  return {
    ...actual,
    api: {
      rescanModels: vi.fn(),
      modelCompat: vi.fn(),
      startInstance: vi.fn(),
    },
  };
});

import { api, ApiError } from '../api/client';

// jsdom renders both the table and the mobile cards (CSS media queries don't hide
// them in jsdom); we scope assertions to the table via role to avoid dupes.

describe('InventoryPanel', () => {
  const noop = () => {};

  it('renders model rows with source badge, format, quant, size', () => {
    render(
      <InventoryPanel
        models={modelsFixture}
        loading={false}
        error={null}
        onRescanned={noop}
        allowCpuOffload={false}
        onAllowCpuOffloadChange={() => {}}
      />,
    );
    const table = screen.getByRole('table');
    const rows = within(table).getAllByRole('row');
    // header + one direct-GGUF row; retired Ollama rows are not listed.
    expect(rows.length).toBe(2);
    expect(within(table).getByText('Qwen3.6-27B-TQ3_4S')).toBeInTheDocument();
    expect(within(table).getByText('tq3_4s')).toBeInTheDocument();
    // "gguf" appears twice (source badge + format column)
    expect(within(table).getAllByText('gguf').length).toBe(2);
    expect(within(table).getByText('14.0 GiB')).toBeInTheDocument();
  });

  it('shows a "present" presence badge by default', () => {
    render(
      <InventoryPanel
        models={modelsFixture}
        loading={false}
        error={null}
        onRescanned={noop}
        allowCpuOffload={false}
        onAllowCpuOffloadChange={() => {}}
      />,
    );
    const table = screen.getByRole('table');
    expect(within(table).getAllByText('present')).toHaveLength(1);
  });

  it('shows a "missing" presence badge when presence=missing', () => {
    const withMissing: ModelSummary[] = [
      { ...modelsFixture[0], presence: 'missing', missing_since: '2026-07-06T00:00:00Z' },
    ];
    render(
      <InventoryPanel
        models={withMissing}
        loading={false}
        error={null}
        onRescanned={noop}
        allowCpuOffload={false}
        onAllowCpuOffloadChange={() => {}}
      />,
    );
    const table = screen.getByRole('table');
    expect(within(table).getByText('missing')).toBeInTheDocument();
    expect(within(table).queryByText('present')).not.toBeInTheDocument();
  });

  it('renders a rescan button (control-plane action)', () => {
    render(
      <InventoryPanel
        models={modelsFixture}
        loading={false}
        error={null}
        onRescanned={noop}
        allowCpuOffload={false}
        onAllowCpuOffloadChange={() => {}}
      />,
    );
    expect(screen.getByRole('button', { name: /rescan/i })).toBeEnabled();
  });

  it('shows an empty-state reason when no models found', () => {
    render(
      <InventoryPanel
        models={[]}
        loading={false}
        error={null}
        onRescanned={noop}
        allowCpuOffload={false}
        onAllowCpuOffloadChange={() => {}}
      />,
    );
    expect(screen.getByText(/No models found/i)).toBeInTheDocument();
    expect(screen.getByText(/found no .* models/i)).toBeInTheDocument();
  });

  it('surfaces a load error reason', () => {
    render(
      <InventoryPanel
        models={null}
        loading={false}
        error="database is locked"
        onRescanned={noop}
        allowCpuOffload={false}
        onAllowCpuOffloadChange={() => {}}
      />,
    );
    expect(
      screen.getByText(/Could not load inventory:.*database is locked/i),
    ).toBeInTheDocument();
  });
});

// Run action — the PRIMARY product flow surfaced per inventory row.
describe('InventoryPanel Run action', () => {
  const noop = () => {};
  beforeEach(() => vi.clearAllMocks());
  afterEach(() => cleanup());

  it('renders a Run button on a runnable row and calls startInstance with the row model id', async () => {
    (api.startInstance as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: 'i9',
      model_id: 'gguf-qwen3-6-27b-tq3-4s',
      engine: 'llamacpp',
      state: 'STARTING',
      pinned: false,
      priority: 0,
      reason: 'spawned llama-server; awaiting /health',
    });
    render(
      <InventoryPanel
        models={modelsFixture}
        loading={false}
        error={null}
        onRescanned={noop}
        allowCpuOffload={false}
        onAllowCpuOffloadChange={() => {}}
      />,
    );
    const table = screen.getByRole('table');
    // The GGUF row is runnable; its Run button lives in the Actions column.
    const runButtons = within(table).getAllByRole('button', { name: /^Run$/ });
    expect(runButtons.length).toBeGreaterThan(0);
    fireEvent.click(runButtons[0]);
    await waitFor(() =>
      expect(api.startInstance).toHaveBeenCalledWith(
        { model_id: 'gguf-qwen3-6-27b-tq3-4s' },
        { allowCpuOffload: false },
      ),
    );
    // 200 → started note surfaces inline.
    await waitFor(() =>
      expect(within(table).getByText(/Starting — STARTING/)).toBeInTheDocument(),
    );
  });

  it('renders the queued position + blocked reason when a start is queued (202)', async () => {
    (api.startInstance as ReturnType<typeof vi.fn>).mockResolvedValue({
      status: 'queued',
      queue_id: 'q1',
      position: 2,
      blocked_reason:
        'needs 14900 MiB, only 3000 MiB free and 0 MiB reclaimable — short 11900 MiB',
    });
    render(
      <InventoryPanel
        models={modelsFixture}
        loading={false}
        error={null}
        onRescanned={noop}
        allowCpuOffload={false}
        onAllowCpuOffloadChange={() => {}}
      />,
    );
    const table = screen.getByRole('table');
    fireEvent.click(within(table).getAllByRole('button', { name: /^Run$/ })[0]);
    await waitFor(() =>
      expect(
        within(table).getByText(/Queued \(position 2\): needs 14900 MiB/),
      ).toBeInTheDocument(),
    );
  });

  it('surfaces the human-readable refusal reason when a start is REFUSED (409)', async () => {
    (api.startInstance as ReturnType<typeof vi.fn>).mockRejectedValue(
      new ApiError(
        'start_refused',
        'could not start instance',
        "REFUSED: verdict is wont_run — model needs 14900 MiB, budget is 3000 MiB",
        409,
      ),
    );
    render(
      <InventoryPanel
        models={modelsFixture}
        loading={false}
        error={null}
        onRescanned={noop}
        allowCpuOffload={false}
        onAllowCpuOffloadChange={() => {}}
      />,
    );
    const table = screen.getByRole('table');
    fireEvent.click(within(table).getAllByRole('button', { name: /^Run$/ })[0]);
    await waitFor(() =>
      expect(
        within(table).getByText(/REFUSED: verdict is wont_run/),
      ).toBeInTheDocument(),
    );
  });

  it('Run sends allow_cpu_offload:true after the shared mode tag is toggled ON', async () => {
    (api.startInstance as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: 'i9',
      model_id: 'gguf-qwen3-6-27b-tq3-4s',
      engine: 'llamacpp',
      state: 'STARTING',
      pinned: false,
      priority: 0,
      reason: 'spawned llama-server; awaiting /health',
    });
    // Controlled wrapper mimics App owning the lifted toggle state (shared with the picker).
    function Harness() {
      const [allow, setAllow] = useState(false);
      return (
        <InventoryPanel
          models={modelsFixture}
          loading={false}
          error={null}
          onRescanned={noop}
          allowCpuOffload={allow}
          onAllowCpuOffloadChange={setAllow}
        />
      );
    }
    render(<Harness />);
    // Default reads "full GPU"; click the click-to-toggle tag to allow CPU offload.
    const tag = screen.getByRole('button', { name: /full GPU/ });
    fireEvent.click(tag);
    expect(
      screen.getByRole('button', { name: /CPU offload allowed/ }),
    ).toBeInTheDocument();
    const table = screen.getByRole('table');
    fireEvent.click(within(table).getAllByRole('button', { name: /^Run$/ })[0]);
    await waitFor(() =>
      expect(api.startInstance).toHaveBeenCalledWith(
        { model_id: 'gguf-qwen3-6-27b-tq3-4s' },
        { allowCpuOffload: true },
      ),
    );
  });

  it('does not render a Run button for a non-runnable (missing) model', () => {
    const missing: ModelSummary[] = [
      { ...modelsFixture[0], presence: 'missing' },
    ];
    render(
      <InventoryPanel
        models={missing}
        loading={false}
        error={null}
        onRescanned={noop}
        allowCpuOffload={false}
        onAllowCpuOffloadChange={() => {}}
      />,
    );
    const table = screen.getByRole('table');
    expect(within(table).queryByRole('button', { name: /^Run$/ })).toBeNull();
  });
});

// Source badge classification is covered indirectly above; assert ollama vs gguf
// labels explicitly here.
describe('InventoryPanel source badges', () => {
  it('labels ollama and local gguf distinctly', () => {
    render(
      <InventoryPanel
        models={modelsFixture}
        loading={false}
        error={null}
        onRescanned={vi.fn()}
        allowCpuOffload={false}
        onAllowCpuOffloadChange={() => {}}
      />,
    );
    const table = screen.getByRole('table');
    // gguf appears as both the source badge and the format cell
    expect(within(table).getAllByText('gguf').length).toBe(2);
  });
});
