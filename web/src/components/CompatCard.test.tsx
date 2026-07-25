import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { CompatCard } from './CompatCard';
import { verdictFixture } from '../test/fixtures';

// Mock the api client so the card's network call is controlled.
vi.mock('../api/client', async () => {
  const actual = await vi.importActual<typeof import('../api/client')>('../api/client');
  return {
    ...actual,
    api: {
      compatPreview: vi.fn(),
    },
  };
});

import { api } from '../api/client';

describe('CompatCard', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('previews a remote repo and shows the verdict (no download)', async () => {
    (api.compatPreview as ReturnType<typeof vi.fn>).mockResolvedValue(verdictFixture);
    render(<CompatCard />);

    fireEvent.change(screen.getByPlaceholderText(/bartowski/i), {
      target: { value: 'bartowski/Llama-3.2-1B-Instruct-GGUF' },
    });
    fireEvent.change(screen.getByPlaceholderText(/Q4_K_M/i), {
      target: { value: 'Llama-3.2-1B-Instruct-Q4_K_M.gguf' },
    });
    fireEvent.click(screen.getByRole('button', { name: /check fit/i }));

    await waitFor(() => {
      expect(screen.getByText(/FALLBACK estimate/i)).toBeInTheDocument();
    });
    expect(api.compatPreview).toHaveBeenCalledWith({
      hf_repo: 'bartowski/Llama-3.2-1B-Instruct-GGUF',
      file: 'Llama-3.2-1B-Instruct-Q4_K_M.gguf',
    });
  });

  it('requires a repo before checking', () => {
    render(<CompatCard />);
    fireEvent.click(screen.getByRole('button', { name: /check fit/i }));
    expect(screen.getByText(/Enter a Hugging Face repo/i)).toBeInTheDocument();
    expect(api.compatPreview).not.toHaveBeenCalled();
  });

  it('sends only hf_repo (safetensors path) when file is blank', async () => {
    (api.compatPreview as ReturnType<typeof vi.fn>).mockResolvedValue({
      ...verdictFixture,
      status: 'wont_run',
      recommended_engine: 'vllm',
    });
    render(<CompatCard />);
    fireEvent.change(screen.getByPlaceholderText(/bartowski/i), {
      target: { value: 'NousResearch/Meta-Llama-3.1-8B-Instruct' },
    });
    fireEvent.click(screen.getByRole('button', { name: /check fit/i }));
    await waitFor(() => {
      expect(api.compatPreview).toHaveBeenCalledWith({
        hf_repo: 'NousResearch/Meta-Llama-3.1-8B-Instruct',
      });
    });
  });

  it('surfaces the error reason on failure', async () => {
    const { ApiError } = await vi.importActual<typeof import('../api/client')>('../api/client');
    (api.compatPreview as ReturnType<typeof vi.fn>).mockRejectedValue(
      new ApiError('gated', 'denied', 'repo is gated — accept its terms on HF', 403),
    );
    render(<CompatCard />);
    fireEvent.change(screen.getByPlaceholderText(/bartowski/i), {
      target: { value: 'meta-llama/Llama-3.1-8B-Instruct' },
    });
    fireEvent.click(screen.getByRole('button', { name: /check fit/i }));
    await waitFor(() => {
      expect(screen.getByText(/repo is gated/i)).toBeInTheDocument();
    });
  });
});
