import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { GpuPanel } from './GpuPanel';
import { hardwareFixture } from '../test/fixtures';
import { ApiError } from '../api/client';
import type { HardwareSnapshot } from '../api/types';

vi.mock('../api/client', async () => {
  const actual =
    await vi.importActual<typeof import('../api/client')>('../api/client');
  return { ...actual, api: { freeGPU: vi.fn() } };
});

import { api } from '../api/client';

/** hardwareFixture with one GPU field overridden. */
function withGpu(patch: Partial<HardwareSnapshot['gpus'][0]>): HardwareSnapshot {
  return {
    ...hardwareFixture,
    gpus: [{ ...hardwareFixture.gpus[0], ...patch }],
  };
}

describe('GpuPanel — extended telemetry (§3.1)', () => {
  afterEach(() => cleanup());

  it('shows the admission budget beside raw free VRAM, labelled as what a start is checked against', () => {
    render(<GpuPanel hardware={hardwareFixture} live />);
    // Raw free VRAM (13287 MiB) and the SMALLER admission budget (12224 MiB) are BOTH on screen.
    // Showing only the free figure is what makes a refusal look like a bug.
    expect(screen.getByText(/13\.0 GiB free · \d+%/)).toBeInTheDocument();
    expect(screen.getByText('Usable for a start')).toBeInTheDocument();
    expect(screen.getAllByText('11.9 GiB').length).toBeGreaterThan(0);
    // The note names the gap explicitly, so the two numbers cannot be mistaken for a discrepancy.
    expect(
      screen.getByText(/admission budget — 13\.0 GiB free minus a 1\.0 GiB safety reserve/),
    ).toBeInTheDocument();
  });

  it('renders clocks, fan, PCIe (current AND max) and the power limit', () => {
    render(<GpuPanel hardware={hardwareFixture} live />);
    expect(screen.getByText('210 MHz / 3105 MHz')).toBeInTheDocument();
    expect(screen.getByText('405 MHz')).toBeInTheDocument();
    // A REAL 0 reading — fan 0 % and util 0 % must render as 0%, not as n/a.
    expect(screen.getAllByText('0%').length).toBe(2);
    // PCIe downtrains at idle: gen 1 of a max 3. Both numbers, always.
    expect(screen.getByText('gen 1 / 3 · ×8 / 8')).toBeInTheDocument();
    expect(screen.getByText('15 W / 165 W')).toBeInTheDocument();
  });

  it('renders every -1 field as n/a and never as 0', () => {
    render(
      <GpuPanel
        hardware={withGpu({
          util: -1,
          temp: -1,
          power: -1,
          clock_sm_mhz: -1,
          clock_mem_mhz: -1,
          clock_sm_max_mhz: -1,
          fan_pct: -1,
          pcie_gen: -1,
          pcie_gen_max: -1,
          pcie_width: -1,
          pcie_width_max: -1,
          power_limit_w: -1,
        })}
        live
      />,
    );
    expect(screen.getAllByText('n/a').length).toBeGreaterThan(0);
    expect(screen.getByText('gen n/a · ×n/a')).toBeInTheDocument();
    // Nothing may claim a 0 MHz clock or a 0 % fan from a sentinel.
    expect(screen.queryByText('0 MHz')).toBeNull();
    expect(screen.queryByText('0%')).toBeNull();
    expect(screen.queryByText('0 W')).toBeNull();
  });

  it('distinguishes a benign idle throttle from an active limit', () => {
    render(<GpuPanel hardware={hardwareFixture} live />);
    expect(screen.getByText('idle')).toBeInTheDocument();

    cleanup();
    render(
      <GpuPanel
        hardware={withGpu({ throttle: 'sw_power_cap,hw_thermal_slowdown' })}
        live
      />,
    );
    expect(
      screen.getByText('sw_power_cap, hw_thermal_slowdown'),
    ).toBeInTheDocument();

    cleanup();
    render(<GpuPanel hardware={withGpu({ throttle: '' })} live />);
    expect(screen.getByText('none')).toBeInTheDocument();
  });

  it('lists GPU-holder processes with honest n/a usage and the pid alone when the name is withheld', () => {
    render(<GpuPanel hardware={hardwareFixture} live />);
    fireEvent.click(screen.getByRole('button', { name: /GPU holders \(2\)/ }));
    expect(screen.getByText('claude.exe')).toBeInTheDocument();
    expect(screen.getByText('1836')).toBeInTheDocument();
    // Per-process VRAM is unavailable under WDDM: every row reads n/a, never 0 MiB.
    expect(screen.getAllByText('n/a').length).toBeGreaterThanOrEqual(2);
    expect(screen.queryByText('0 MiB')).toBeNull();
    expect(screen.getByText(/Per-process VRAM is not available on Windows/)).toBeInTheDocument();
  });
});

describe('GpuPanel — RAM floor (§3.1)', () => {
  afterEach(() => cleanup());

  it('shows the floor next to free RAM without warning when it is satisfied', () => {
    render(<GpuPanel hardware={hardwareFixture} live />);
    expect(screen.getByText(/floor 2.0 GiB/)).toBeInTheDocument();
    expect(screen.queryByText(/below the residency floor/)).toBeNull();
  });

  it('warns, and names the shortfall, when free RAM is under the floor', () => {
    render(
      <GpuPanel
        hardware={{
          ...hardwareFixture,
          ram: { total: 16331, free: 1720, used: 14611, floor_mib: 2048 },
        }}
        live
      />,
    );
    // 2048 − 1720 = 328 MiB short. The message must say starts are refused regardless of VRAM,
    // because on this box that is exactly what happens with 13 GiB of the card sitting idle.
    expect(screen.getByText(/below the residency floor/)).toBeInTheDocument();
    expect(screen.getByText(/0.3 GiB more is free/)).toBeInTheDocument();
  });
});

describe('GpuPanel — Free GPU control (§3.2)', () => {
  beforeEach(() => vi.clearAllMocks());
  afterEach(() => cleanup());

  it('does not call the endpoint on the first click — it arms a confirm step', () => {
    render(<GpuPanel hardware={hardwareFixture} live />);
    fireEvent.click(screen.getByRole('button', { name: 'Free GPU' }));
    expect(api.freeGPU).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Stop all' })).toBeInTheDocument();
    // Cancel backs out without touching anything.
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(api.freeGPU).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Free GPU' })).toBeInTheDocument();
  });

  it('reports the MEASURED reclaimed VRAM and what was stopped', async () => {
    (api.freeGPU as ReturnType<typeof vi.fn>).mockResolvedValue({
      stopped: 1,
      skipped: 0,
      failed: 0,
      vram_free_before_mib: 10700,
      vram_free_after_mib: 14833,
      reclaimed_mib: 4133,
      results: [
        {
          id: 'i1',
          model_id: 'fara-4b',
          stopped: true,
          reason: 'stopped and VERIFIED: free VRAM rose by 4133 MiB',
        },
      ],
      reason: 'stopped 1 instance(s); free VRAM rose by 4133 MiB (measured, not assumed)',
    });
    const onGpuFreed = vi.fn();
    render(<GpuPanel hardware={hardwareFixture} live onGpuFreed={onGpuFreed} />);
    fireEvent.click(screen.getByRole('button', { name: 'Free GPU' }));
    fireEvent.click(screen.getByRole('button', { name: 'Stop all' }));

    // include_pinned defaults to FALSE — a one-click convenience must not override a pin.
    await waitFor(() => expect(api.freeGPU).toHaveBeenCalledWith(false));
    // The measured delta, not a sum of estimates.
    await waitFor(() => expect(screen.getByText('+4.0 GiB measured')).toBeInTheDocument());
    expect(screen.getByText(/free VRAM 10.4 GiB → 14.5 GiB/)).toBeInTheDocument();
    expect(screen.getByText('fara-4b')).toBeInTheDocument();
    // Something was stopped → free VRAM moved → cached verdicts are stale.
    expect(onGpuFreed).toHaveBeenCalledTimes(1);
  });

  it('says pinned instances were kept and offers the explicit include_pinned retry', async () => {
    (api.freeGPU as ReturnType<typeof vi.fn>).mockResolvedValue({
      stopped: 0,
      skipped: 2,
      failed: 0,
      vram_free_before_mib: 4000,
      vram_free_after_mib: 4000,
      reclaimed_mib: 0,
      results: [
        { id: 'i1', model_id: 'a', stopped: false, reason: 'skipped: instance is pinned' },
        { id: 'i2', model_id: 'b', stopped: false, reason: 'skipped: instance is pinned' },
      ],
      reason: 'stopped nothing; 2 pinned skipped',
    });
    render(<GpuPanel hardware={hardwareFixture} live />);
    fireEvent.click(screen.getByRole('button', { name: 'Free GPU' }));
    fireEvent.click(screen.getByRole('button', { name: 'Stop all' }));

    await waitFor(() =>
      expect(screen.getByText(/2 pinned instances were kept/)).toBeInTheDocument(),
    );
    // Overriding a pin is a SECOND deliberate action, not something the first click did quietly.
    fireEvent.click(screen.getByRole('button', { name: 'Stop pinned too' }));
    await waitFor(() => expect(api.freeGPU).toHaveBeenLastCalledWith(true));
  });

  it('surfaces the failure reason and claims nothing when the sweep fails', async () => {
    (api.freeGPU as ReturnType<typeof vi.fn>).mockRejectedValue(
      new ApiError('stop_failed', 'could not free the GPU', 'instance store not configured', 500),
    );
    render(<GpuPanel hardware={hardwareFixture} live />);
    fireEvent.click(screen.getByRole('button', { name: 'Free GPU' }));
    fireEvent.click(screen.getByRole('button', { name: 'Stop all' }));
    await waitFor(() =>
      expect(
        screen.getByText(/Free GPU failed: instance store not configured/),
      ).toBeInTheDocument(),
    );
    expect(screen.queryByText(/measured/)).toBeNull();
  });
});
