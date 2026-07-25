import { describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { InventoryPanel } from './InventoryPanel';
import { modelsFixture } from '../test/fixtures';
import type { ModelSummary } from '../api/types';

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
      />,
    );
    const table = screen.getByRole('table');
    const rows = within(table).getAllByRole('row');
    // header + 2 data rows
    expect(rows.length).toBe(3);
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
      />,
    );
    const table = screen.getByRole('table');
    expect(within(table).getAllByText('present').length).toBe(2);
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
      />,
    );
    expect(
      screen.getByText(/Could not load inventory:.*database is locked/i),
    ).toBeInTheDocument();
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
      />,
    );
    const table = screen.getByRole('table');
    // "ollama" appears as both source badge and format cell for the ollama model
    expect(within(table).getAllByText('ollama').length).toBe(2);
    // gguf appears as both the source badge and the format cell
    expect(within(table).getAllByText('gguf').length).toBe(2);
  });
});
