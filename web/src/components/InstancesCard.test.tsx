import { useState } from 'react';
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

// A present GGUF that has never been compat-checked (no cached verdict) — must still appear in
// the picker, annotated "unchecked".
const uncheckedModel: ModelSummary = {
  id: 'mystery-gguf',
  display_name: 'Mystery 7B Q4_K_M',
  source: 'local_path',
  format: 'gguf',
  presence: 'present',
};

// A present GGUF cached as wont_run — still submittable; the daemon is the authority.
const wontRunModel: ModelSummary = {
  id: 'huge-70b',
  display_name: 'Huge 70B Q8_0',
  source: 'local_path',
  format: 'gguf',
  compat_status: 'wont_run',
  presence: 'present',
};

// Default offload props for renders that don't exercise the toggle. A no-op onChange keeps
// the checkbox controlled; allowCpuOffload defaults OFF (full GPU — never split to CPU).
const offloadOff = { allowCpuOffload: false, onAllowCpuOffloadChange: () => {} };

describe('InstancesCard', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });
  afterEach(() => cleanup());

  it('renders a HEALTHY instance with its state badge and reason', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([healthyInstance]);
    render(<InstancesCard models={[]} instancesInvalidation={0} {...offloadOff} />);
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
    render(<InstancesCard models={[]} instancesInvalidation={0} {...offloadOff} />);
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
    render(<InstancesCard models={[runnableModel]} instancesInvalidation={0} {...offloadOff} />);
    await waitFor(() => screen.getByText('GLM OCR Q8_0'));
    fireEvent.click(screen.getByRole('button', { name: /^Start$/ }));
    await waitFor(() =>
      expect(api.startInstance).toHaveBeenCalledWith(
        { model_id: 'glm-ocr' },
        { allowCpuOffload: false },
      ),
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
    render(<InstancesCard models={[runnableModel]} instancesInvalidation={0} {...offloadOff} />);
    await waitFor(() => screen.getByRole('button', { name: /^Start$/ }));
    fireEvent.click(screen.getByRole('button', { name: /^Start$/ }));
    await waitFor(() => screen.getByText(/Refused: REFUSED: estimated 14000 MiB/));
  });

  it('lists an unchecked model in the picker with an "unchecked" annotation', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    render(<InstancesCard models={[uncheckedModel]} instancesInvalidation={0} {...offloadOff} />);
    await waitFor(() => screen.getByText('Mystery 7B Q4_K_M'));
    // Annotated as unchecked (not filtered out for lacking a cached verdict).
    expect(screen.getByText('unchecked')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^Start$/ })).toBeEnabled();
  });

  it("allows submitting a won't-fit model and surfaces the daemon's numeric refusal", async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (api.startInstance as ReturnType<typeof vi.fn>).mockRejectedValue(
      new ApiError(
        'start_refused',
        'could not start instance',
        'REFUSED: verdict is wont_run — needs 40000 MiB, budget 3000 MiB',
        409,
      ),
    );
    render(<InstancesCard models={[wontRunModel]} instancesInvalidation={0} {...offloadOff} />);
    await waitFor(() => screen.getByText('Huge 70B Q8_0'));
    // Annotated "won't fit" but the Start button is still present + enabled.
    expect(screen.getByText("won't fit")).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /^Start$/ }));
    await waitFor(() =>
      expect(api.startInstance).toHaveBeenCalledWith(
        { model_id: 'huge-70b' },
        { allowCpuOffload: false },
      ),
    );
    await waitFor(() => screen.getByText(/Refused: REFUSED: verdict is wont_run/));
  });

  it('shows a Downloads-panel pointer when inventory is empty', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    render(<InstancesCard models={[]} instancesInvalidation={0} {...offloadOff} />);
    await waitFor(() =>
      expect(screen.getByText(/No models in inventory yet/)).toBeInTheDocument(),
    );
    expect(screen.getByText(/Downloads panel/)).toBeInTheDocument();
  });

  it('renders the CPU-offload toggle unchecked by default with the muted hint', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    render(<InstancesCard models={[runnableModel]} instancesInvalidation={0} {...offloadOff} />);
    await waitFor(() => screen.getByText('Allow CPU offload'));
    expect(screen.getByRole('checkbox')).not.toBeChecked();
    expect(
      screen.getByText(/default: full GPU — never split to CPU/),
    ).toBeInTheDocument();
  });

  it('toggling CPU offload ON sends allow_cpu_offload:true on a picker start', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (api.startInstance as ReturnType<typeof vi.fn>).mockResolvedValue({
      ...healthyInstance,
      id: 'i3',
      model_id: 'glm-ocr',
    });
    // Controlled wrapper mimics App owning the lifted toggle state.
    function Harness() {
      const [allow, setAllow] = useState(false);
      return (
        <InstancesCard
          models={[runnableModel]}
          instancesInvalidation={0}
          allowCpuOffload={allow}
          onAllowCpuOffloadChange={setAllow}
        />
      );
    }
    render(<Harness />);
    await waitFor(() => screen.getByText('GLM OCR Q8_0'));
    fireEvent.click(screen.getByRole('checkbox'));
    expect(screen.getByRole('checkbox')).toBeChecked();
    fireEvent.click(screen.getByRole('button', { name: /^Start$/ }));
    await waitFor(() =>
      expect(api.startInstance).toHaveBeenCalledWith(
        { model_id: 'glm-ocr' },
        { allowCpuOffload: true },
      ),
    );
  });

  it('does not offer to start a model that already has an active instance', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([
      { ...healthyInstance, model_id: 'glm-ocr' },
    ]);
    render(<InstancesCard models={[runnableModel]} instancesInvalidation={0} {...offloadOff} />);
    await waitFor(() => screen.getByText('HEALTHY'));
    // No Start button because the only runnable model is already running.
    expect(screen.queryByRole('button', { name: /^Start$/ })).toBeNull();
  });
});
