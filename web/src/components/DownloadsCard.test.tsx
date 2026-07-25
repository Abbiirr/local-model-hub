import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup } from '@testing-library/react';
import { DownloadsCard } from './DownloadsCard';
import {
  awaitingJobFixture,
  doneJobFixture,
  reproduceFixture,
} from '../test/fixtures';

vi.mock('../api/client', async () => {
  const actual = await vi.importActual<typeof import('../api/client')>('../api/client');
  return {
    ...actual,
    api: {
      createDownload: vi.fn(),
      downloadStatus: vi.fn(),
      reproduce: vi.fn(),
    },
  };
});

import { api } from '../api/client';

describe('DownloadsCard', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup(); // unmount so the polling interval + DOM don't leak across tests
  });

  it('two-step HF: resolves and shows the pinned commit + files before downloading', async () => {
    (api.createDownload as ReturnType<typeof vi.fn>).mockResolvedValue(awaitingJobFixture);
    render(<DownloadsCard />);

    fireEvent.change(screen.getByPlaceholderText(/Qwen2.5-0.5B-Instruct-GGUF/i), {
      target: { value: 'owner/model-GGUF' },
    });
    fireEvent.click(screen.getByRole('button', { name: /resolve commit/i }));

    await waitFor(() => {
      // Shows the pinned commit (short form) and the confirm button — bytes NOT yet fetched.
      expect(screen.getByText(/e3a1c0f12345/i)).toBeInTheDocument();
    });
    expect(screen.getByRole('button', { name: /confirm download/i })).toBeInTheDocument();
    // First call was a resolve (confirm:false).
    expect(api.createDownload).toHaveBeenCalledWith({ kind: 'hf', repo: 'owner/model-GGUF', confirm: false });
  });

  it('confirm sends confirm:true to actually download', async () => {
    (api.createDownload as ReturnType<typeof vi.fn>)
      .mockResolvedValueOnce(awaitingJobFixture)
      .mockResolvedValueOnce({ ...awaitingJobFixture, state: 'running' });
    render(<DownloadsCard />);
    fireEvent.change(screen.getByPlaceholderText(/Qwen2.5-0.5B-Instruct-GGUF/i), {
      target: { value: 'owner/model-GGUF' },
    });
    fireEvent.click(screen.getByRole('button', { name: /resolve commit/i }));
    await waitFor(() => screen.getByRole('button', { name: /confirm download/i }));
    fireEvent.click(screen.getByRole('button', { name: /confirm download/i }));
    await waitFor(() => {
      expect(api.createDownload).toHaveBeenLastCalledWith({
        kind: 'hf',
        repo: 'owner/model-GGUF',
        confirm: true,
      });
    });
  });

  it('renders a pickle warning with the critical styling', async () => {
    (api.createDownload as ReturnType<typeof vi.fn>).mockResolvedValue({
      ...awaitingJobFixture,
      resolved: {
        ...awaitingJobFixture.resolved!,
        warnings: [
          'Contains pickle-format weights (.bin/.pt/.pth) and no safetensors alternative. Pickle can execute arbitrary code when the weights are loaded. Prefer a safetensors version of this model.',
        ],
      },
    });
    render(<DownloadsCard />);
    fireEvent.change(screen.getByPlaceholderText(/Qwen2.5-0.5B-Instruct-GGUF/i), {
      target: { value: 'owner/pickle-model' },
    });
    fireEvent.click(screen.getByRole('button', { name: /resolve commit/i }));
    await waitFor(() => {
      expect(screen.getByText(/pickle can execute arbitrary code/i)).toBeInTheDocument();
    });
  });

  it('local import switches the form and sends a path', async () => {
    (api.createDownload as ReturnType<typeof vi.fn>).mockResolvedValue({
      ...doneJobFixture,
      kind: 'local_import',
      state: 'hashing',
    });
    render(<DownloadsCard />);
    fireEvent.click(screen.getByRole('button', { name: /local import/i }));
    fireEvent.change(screen.getByPlaceholderText(/llm_models/i), {
      target: { value: 'K:\\projects\\llm_models\\models\\GLM-OCR-Q8_0.gguf' },
    });
    fireEvent.click(screen.getByRole('button', { name: /^start$/i }));
    await waitFor(() => {
      expect(api.createDownload).toHaveBeenCalledWith({
        kind: 'local_import',
        path: 'K:\\projects\\llm_models\\models\\GLM-OCR-Q8_0.gguf',
      });
    });
  });

  it('shows the reproduce JSON on a completed job', async () => {
    (api.createDownload as ReturnType<typeof vi.fn>).mockResolvedValue(doneJobFixture);
    (api.reproduce as ReturnType<typeof vi.fn>).mockResolvedValue(reproduceFixture);
    render(<DownloadsCard />);
    fireEvent.change(screen.getByPlaceholderText(/Qwen2.5-0.5B-Instruct-GGUF/i), {
      target: { value: 'owner/model-GGUF' },
    });
    fireEvent.click(screen.getByRole('button', { name: /resolve commit/i }));
    await waitFor(() => screen.getByRole('button', { name: /reproduce/i }));
    fireEvent.click(screen.getByRole('button', { name: /reproduce/i }));
    await waitFor(() => {
      expect(screen.getByText(/reproducible — Hugging Face repo pinned/i)).toBeInTheDocument();
    });
    expect(api.reproduce).toHaveBeenCalledWith('hf-owner-model-gguf');
  });

  it('requires a repo before resolving', () => {
    render(<DownloadsCard />);
    fireEvent.click(screen.getByRole('button', { name: /resolve commit/i }));
    expect(screen.getByText(/Enter a Hugging Face repo/i)).toBeInTheDocument();
    expect(api.createDownload).not.toHaveBeenCalled();
  });
});
