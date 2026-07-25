import { describe, expect, it } from 'vitest';
import { render, screen, fireEvent, within } from '@testing-library/react';
import { CompatVerdictView, statusTone } from './CompatVerdict';
import { verdictFixture } from '../test/fixtures';
import type { Verdict } from '../api/types';

describe('statusTone', () => {
  it('maps the four statuses to tones + labels', () => {
    expect(statusTone('runs')).toEqual({ tone: 'ok', label: 'runs' });
    expect(statusTone('runs_degraded')).toEqual({ tone: 'warn', label: 'degraded' });
    expect(statusTone('wont_run')).toEqual({ tone: 'crit', label: "won't run" });
    expect(statusTone('unknown')).toEqual({ tone: 'neutral', label: 'unknown' });
  });
});

describe('CompatVerdictView', () => {
  it('always shows the reason (mandatory-reason rule), even collapsed', () => {
    render(<CompatVerdictView verdict={verdictFixture} />);
    expect(screen.getByText(/FALLBACK estimate/i)).toBeInTheDocument();
    // "degraded" appears both in the status badge and inside the reason text.
    expect(screen.getAllByText(/degraded/i).length).toBeGreaterThan(0);
  });

  it('reveals VRAM, max ctx, and run flags when expanded', () => {
    render(<CompatVerdictView verdict={verdictFixture} />);
    // Collapsed: run flags not shown yet.
    expect(screen.queryByText(/Run flags/i)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { expanded: false }));
    expect(screen.getByText('Run flags')).toBeInTheDocument();
    expect(
      screen.getByText(/-ngl all -c 8192 --fit off -fa on/),
    ).toBeInTheDocument();
    expect(screen.getByText('Max ctx that fits')).toBeInTheDocument();
  });

  it('shows a crit reason for wont_run', () => {
    const wont: Verdict = {
      ...verdictFixture,
      status: 'wont_run',
      reason: 'needs ~30000 MiB but only 11960 usable',
    };
    render(<CompatVerdictView verdict={wont} />);
    const badges = screen.getAllByText("won't run");
    expect(badges.length).toBeGreaterThan(0);
    expect(screen.getByText(/needs ~30000 MiB/)).toBeInTheDocument();
  });

  it('renders per-engine breakdown when there are multiple choices', () => {
    const multi: Verdict = {
      ...verdictFixture,
      engine_choices: [
        { engine: 'llamacpp', detail: 'CUDA fork', status: 'runs', estimated_vram_mb: 6000, confidence: 'approximate', reason: 'x' },
        { engine: 'ollama', detail: 'shared daemon', status: 'runs', estimated_vram_mb: 6000, confidence: 'approximate', reason: 'y' },
      ],
    };
    render(<CompatVerdictView verdict={multi} defaultOpen />);
    expect(screen.getByText('Per-engine')).toBeInTheDocument();
    const alts = screen.getByText('Per-engine').parentElement as HTMLElement;
    expect(within(alts).getByText('llamacpp')).toBeInTheDocument();
    expect(within(alts).getByText('ollama')).toBeInTheDocument();
  });
});
