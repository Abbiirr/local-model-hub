import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import {
  render,
  screen,
  fireEvent,
  waitFor,
  cleanup,
} from '@testing-library/react';
import { InstancesCard } from './InstancesCard';
import { ApiError } from '../api/client';
import type { InstanceView, ModelSummary } from '../api/types';

vi.mock('../api/client', async () => {
  const actual =
    await vi.importActual<typeof import('../api/client')>('../api/client');
  return {
    ...actual,
    api: {
      instances: vi.fn(),
      startInstance: vi.fn(),
      stopInstance: vi.fn(),
    },
  };
});

import { api } from '../api/client';

const healthyInstance: InstanceView = {
  id: 'i1',
  model_id: 'llama-3.2-1b',
  engine: 'llamacpp',
  state: 'HEALTHY',
  port: 40100,
  base_url: 'http://127.0.0.1:40100',
  ctx: 4096,
  vram_est_mib: 900,
  vram_obs_mib: 950,
  pinned: false,
  priority: 0,
  reason: 'llama-server pid 4242 answered /health OK',
};

const runnableModel: ModelSummary = {
  id: 'glm-ocr',
  display_name: 'GLM OCR Q8_0',
  source: 'local_path',
  format: 'gguf',
  compat_status: 'runs',
  presence: 'present',
};

describe('InstancesCard', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });
  afterEach(() => cleanup());

  it('renders a HEALTHY instance with its state badge and reason', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([healthyInstance]);
    render(<InstancesCard models={[]} instancesInvalidation={0} />);
    await waitFor(() => screen.getByText('HEALTHY'));
    expect(screen.getByText('llama-3.2-1b')).toBeInTheDocument();
    expect(screen.getByText(/answered \/health OK/)).toBeInTheDocument();
  });

  it('shows a Stop button for active instances and calls stopInstance', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([healthyInstance]);
    (api.stopInstance as ReturnType<typeof vi.fn>).mockResolvedValue({
      ...healthyInstance,
      state: 'STOPPED',
      reason: 'stopped and VERIFIED: free VRAM rose by 950 MiB',
    });
    render(<InstancesCard models={[]} instancesInvalidation={0} />);
    await waitFor(() => screen.getByRole('button', { name: /^Stop$/ }));
    fireEvent.click(screen.getByRole('button', { name: /^Stop$/ }));
    await waitFor(() => expect(api.stopInstance).toHaveBeenCalledWith('i1'));
    await waitFor(() => screen.getByText(/VERIFIED: free VRAM rose/));
  });

  it('offers a Start button per runnable GGUF model and calls startInstance', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (api.startInstance as ReturnType<typeof vi.fn>).mockResolvedValue({
      ...healthyInstance,
      id: 'i2',
      model_id: 'glm-ocr',
    });
    render(<InstancesCard models={[runnableModel]} instancesInvalidation={0} />);
    await waitFor(() => screen.getByText('GLM OCR Q8_0'));
    fireEvent.click(screen.getByRole('button', { name: /^Start$/ }));
    await waitFor(() =>
      expect(api.startInstance).toHaveBeenCalledWith({ model_id: 'glm-ocr' }),
    );
  });

  it('surfaces the numeric refusal reason when a start is REFUSED (409)', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (api.startInstance as ReturnType<typeof vi.fn>).mockRejectedValue(
      new ApiError(
        'start_refused',
        'could not start instance',
        'REFUSED: estimated 14000 MiB exceeds the Phase-4 budget of 13700 MiB',
        409,
      ),
    );
    render(<InstancesCard models={[runnableModel]} instancesInvalidation={0} />);
    await waitFor(() => screen.getByRole('button', { name: /^Start$/ }));
    fireEvent.click(screen.getByRole('button', { name: /^Start$/ }));
    await waitFor(() => screen.getByText(/Refused: REFUSED: estimated 14000 MiB/));
  });

  it('does not offer to start a model that already has an active instance', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([
      { ...healthyInstance, model_id: 'glm-ocr' },
    ]);
    render(<InstancesCard models={[runnableModel]} instancesInvalidation={0} />);
    await waitFor(() => screen.getByText('HEALTHY'));
    // No Start button because the only runnable model is already running.
    expect(screen.queryByRole('button', { name: /^Start$/ })).toBeNull();
  });
});
