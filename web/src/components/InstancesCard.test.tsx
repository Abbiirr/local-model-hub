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
      // §3.3: the picker now fetches a real verdict per startable row. Stubbed as "rejects" by
      // default so these tests exercise the fallback path (cached status / "unchecked"); the
      // verdict-specific tests below override it.
      modelCompat: vi.fn(),
    },
  };
});

import { api } from '../api/client';
import { resetCompatCache } from '../api/useCompatVerdicts';

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

/** Open the collapsed "Launch options" disclosure (§3.4 moved the CPU-offload toggle, the
 *  advanced profile and the attach form behind it so the model list leads the panel). */
function openLaunchOptions() {
  fireEvent.click(screen.getByRole('button', { name: /Launch options/ }));
}

describe('InstancesCard', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // The verdict cache is module-scoped; without this a verdict from one test leaks into the next.
    resetCompatCache();
    (api.modelCompat as ReturnType<typeof vi.fn>).mockRejectedValue(
      new ApiError('unavailable', 'compat unavailable', 'no compat in this test', 503),
    );
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
    // force:false is the important half — a normal (LocalHub-spawned) instance is stopped WITHOUT the
    // adopted-process override, so the server-side guard stays armed for anything LocalHub didn't start.
    await waitFor(() =>
      expect(api.stopInstance).toHaveBeenCalledWith('i1', { force: false }),
    );
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

  it('falls back to "unchecked" when the live fit check fails, never to a manufactured verdict', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    render(<InstancesCard models={[uncheckedModel]} instancesInvalidation={0} {...offloadOff} />);
    await waitFor(() => screen.getByText('Mystery 7B Q4_K_M'));
    // The fetch rejects (see beforeEach). A check that could NOT be made must degrade to
    // "unchecked" — inventing "won't fit" from a failed request would be a claim about the model
    // that the daemon never made.
    await waitFor(() => expect(screen.getByText('unchecked')).toBeInTheDocument());
    expect(screen.queryByText("won't fit")).toBeNull();
    expect(screen.getByRole('button', { name: /^Start$/ })).toBeEnabled();
  });

  it('replaces "unchecked" with the live verdict, its estimate, and its reason as a tooltip', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (api.modelCompat as ReturnType<typeof vi.fn>).mockResolvedValue({
      model: 'Mystery 7B Q4_K_M',
      status: 'runs',
      reason: 'GGUF needs ~4123 MiB at 8K ctx and fits in 13647 MiB usable, with headroom.',
      recommended_engine: 'llamacpp',
      estimated_vram_mb: 4123,
      confidence: 'exact',
    });
    render(<InstancesCard models={[uncheckedModel]} instancesInvalidation={0} {...offloadOff} />);
    await waitFor(() => expect(screen.getByText('fits')).toBeInTheDocument());
    // The estimate is rendered beside the badge…
    expect(screen.getByText('~4.0 GiB')).toBeInTheDocument();
    // …and the mandatory reason rides along as the tooltip (never dropped).
    expect(
      screen.getByTitle(/needs ~4123 MiB at 8K ctx and fits in 13647 MiB usable/),
    ).toBeInTheDocument();
    expect(screen.queryByText('unchecked')).toBeNull();
  });

  it('fetches one verdict per startable model and does not refetch on a poll tick', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (api.modelCompat as ReturnType<typeof vi.fn>).mockResolvedValue({
      model: 'x',
      status: 'runs',
      reason: 'fits',
      recommended_engine: 'llamacpp',
      estimated_vram_mb: 1000,
      confidence: 'exact',
    });
    const { rerender } = render(
      <InstancesCard
        models={[runnableModel, uncheckedModel]}
        instancesInvalidation={0}
        {...offloadOff}
      />,
    );
    await waitFor(() =>
      expect(api.modelCompat).toHaveBeenCalledTimes(2),
    );
    // A re-render with an equal (but not identical) models array must NOT trigger new requests —
    // this endpoint does GGUF header reads, so re-rendering it into a fetch loop is a real cost.
    rerender(
      <InstancesCard
        models={[{ ...runnableModel }, { ...uncheckedModel }]}
        instancesInvalidation={1}
        {...offloadOff}
      />,
    );
    await waitFor(() => screen.getByText('GLM OCR Q8_0'));
    expect(api.modelCompat).toHaveBeenCalledTimes(2);
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

  it('leads with the model list and keeps launch options collapsed (§3.4)', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    render(<InstancesCard models={[runnableModel]} instancesInvalidation={0} {...offloadOff} />);
    await waitFor(() => screen.getByText('GLM OCR Q8_0'));
    // The primary action is present without any disclosure being opened…
    expect(screen.getByRole('button', { name: /^Start$/ })).toBeInTheDocument();
    // …while the secondary settings are behind one collapsed disclosure.
    expect(screen.queryByRole('checkbox')).toBeNull();
    expect(screen.queryByLabelText('Attach port')).toBeNull();
    expect(
      screen.getByRole('button', { name: /Launch options/ }),
    ).toHaveAttribute('aria-expanded', 'false');
  });

  it('names an armed non-default option in the collapsed summary so it is never silent', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    render(
      <InstancesCard
        models={[runnableModel]}
        instancesInvalidation={0}
        allowCpuOffload
        onAllowCpuOffloadChange={() => {}}
      />,
    );
    await waitFor(() => screen.getByText('GLM OCR Q8_0'));
    expect(screen.getByText(/CPU offload allowed/)).toBeInTheDocument();
  });

  it('renders the CPU-offload toggle unchecked by default with the muted hint', async () => {
    (api.instances as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    render(<InstancesCard models={[runnableModel]} instancesInvalidation={0} {...offloadOff} />);
    await waitFor(() => screen.getByText('GLM OCR Q8_0'));
    openLaunchOptions();
    expect(screen.getByText('Allow CPU offload')).toBeInTheDocument();
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
    openLaunchOptions();
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
