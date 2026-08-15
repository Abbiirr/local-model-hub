import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { StartupPanel } from './StartupPanel';
import { api } from '../api/client';

vi.mock('../api/client', async () => {
  const actual = await vi.importActual<typeof import('../api/client')>('../api/client');
  return { ...actual, api: { ...actual.api, startupConfig: vi.fn(), saveStartupConfig: vi.fn() } };
});

const initial = { version: 1, start_localhub: true, open_web_ui: false, restore_last_session: true, startup_delay_seconds: 0, health_check_seconds: 60, models: [] };

describe('StartupPanel', () => {
  beforeEach(() => {
    vi.mocked(api.startupConfig).mockResolvedValue(initial);
    vi.mocked(api.saveStartupConfig).mockImplementation(async (config) => config);
  });

  it('adds a model and saves complete startup intent', async () => {
    render(<StartupPanel models={[{ id: 'gguf-test', display_name: 'Test model', source: 'local_path', format: 'gguf', presence: 'present' }]} />);
    await screen.findByText(/Configure what the native tray app does/i);
    fireEvent.change(screen.getByLabelText('Model to add'), { target: { value: 'gguf-test' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add to startup' }));
    expect(screen.getByText('gguf-test')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Save startup settings' }));
    await waitFor(() => expect(api.saveStartupConfig).toHaveBeenCalled());
    expect(vi.mocked(api.saveStartupConfig).mock.calls[0][0].models[0].model_id).toBe('gguf-test');
  });
});
