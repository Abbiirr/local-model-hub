import { useState } from 'react';
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup } from '@testing-library/react';
import {
  AdvancedStartForm,
  EMPTY_PROFILE,
  profileFromDefault,
  profileToRequest,
  summarize,
  type LaunchProfileDraft,
} from './AdvancedStartForm';
import { ApiError } from '../api/client';
import type { ModelLaunchDefault } from '../api/types';

vi.mock('../api/client', async () => {
  const actual =
    await vi.importActual<typeof import('../api/client')>('../api/client');
  return {
    ...actual,
    api: {
      modelLaunchDefault: vi.fn(),
      saveModelLaunchDefault: vi.fn(),
      deleteModelLaunchDefault: vi.fn(),
    },
  };
});

import { api } from '../api/client';

// The VALIDATED Qwen3.6-27B MTP profile — the whole reason this form exists. Before M2.1/M2.3 it was
// inexpressible from any caller: kv_quant is a single bool meaning q4_0 on BOTH sides.
const savedMTPDefault: ModelLaunchDefault = {
  model_id: 'qwen36-mtp',
  ctx: 8192,
  kv_type_k: 'q8_0',
  kv_type_v: 'q4_0',
  slots: 1,
  spec_mode: 'draft-mtp',
  spec_draft_max: 2,
  engine_version: '10106',
  engine_commit: '1425386fd',
  validated_note: 'validated at 8K',
};

beforeEach(() => {
  vi.clearAllMocks();
});
afterEach(() => cleanup());

// Harness: the form is controlled, so tests need real state to observe edits.
function Harness(props: { modelId?: string | null; initial?: LaunchProfileDraft }) {
  const [p, setP] = useState<LaunchProfileDraft>(props.initial ?? EMPTY_PROFILE);
  return (
    <AdvancedStartForm
      profile={p}
      onChange={setP}
      modelId={props.modelId === undefined ? 'qwen36-mtp' : props.modelId}
    />
  );
}

// ── profileToRequest: the wire contract ───────────────────────────────────────
// A BLANK field must be OMITTED, not sent as 0. The server treats 0/'' as unset, so sending an
// explicit 0 for slots would be indistinguishable from asking for it — and an untouched form must
// produce the byte-identical body the plain Start path sent before M2.3 existed.
describe('profileToRequest wire cleanliness', () => {
  it('omits every field for an untouched profile', () => {
    expect(profileToRequest(EMPTY_PROFILE)).toEqual({});
  });

  it('omits blanks but keeps a partially-filled profile', () => {
    const got = profileToRequest({ ...EMPTY_PROFILE, kvTypeK: 'q8_0' });
    expect(got).toEqual({ kv_type_k: 'q8_0' });
    expect(got).not.toHaveProperty('kv_type_v');
    expect(got).not.toHaveProperty('slots');
  });

  it('maps the validated MTP profile onto the exact request fields', () => {
    expect(profileToRequest(profileFromDefault(savedMTPDefault))).toEqual({
      ctx: 8192,
      kv_type_k: 'q8_0',
      kv_type_v: 'q4_0',
      slots: 1,
      speculative: 'draft-mtp',
      spec_draft_max: 2,
    });
  });

  it('never sends allow_cpu_offload (the shared toggle owns it — one control, not two)', () => {
    const got = profileToRequest({ ...EMPTY_PROFILE, allowCpuOffload: true });
    expect(got).not.toHaveProperty('allow_cpu_offload');
  });

  it('drops a non-numeric numeric field rather than sending NaN', () => {
    const got = profileToRequest({ ...EMPTY_PROFILE, slots: 'abc' });
    expect(got).not.toHaveProperty('slots');
  });
});

// ── summarize: a non-default profile must never be silently armed ─────────────
describe('summarize', () => {
  it('is empty for an untouched profile', () => {
    expect(summarize(EMPTY_PROFILE)).toBe('');
  });

  it('renders an asymmetric KV pair, filling f16 for the unset side', () => {
    expect(summarize({ ...EMPTY_PROFILE, kvTypeK: 'q8_0' })).toContain('kv q8_0/f16');
  });

  it('renders the full MTP profile', () => {
    const s = summarize(profileFromDefault(savedMTPDefault));
    expect(s).toContain('ctx 8192');
    expect(s).toContain('kv q8_0/q4_0');
    expect(s).toContain('-np 1');
    expect(s).toContain('draft-mtp n=2');
  });
});

// ── The form itself ──────────────────────────────────────────────────────────
describe('AdvancedStartForm', () => {
  it('is collapsed by default so the common start path is untouched', () => {
    render(<Harness />);
    expect(screen.queryByText('K cache (-ctk)')).not.toBeInTheDocument();
    expect(
      screen.getByRole('button', { name: /Advanced launch profile/ }),
    ).toHaveAttribute('aria-expanded', 'false');
  });

  it('shows the armed profile summary while collapsed', () => {
    render(
      <Harness initial={{ ...EMPTY_PROFILE, kvTypeK: 'q8_0', kvTypeV: 'q4_0' }} />,
    );
    expect(screen.getByText(/kv q8_0\/q4_0/)).toBeInTheDocument();
  });

  it('lets K and V be set INDEPENDENTLY (the pair kv_quant could not express)', () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: /Advanced launch profile/ }));

    const kSelect = screen.getByLabelText('K cache (-ctk)') as HTMLSelectElement;
    const vSelect = screen.getByLabelText('V cache (-ctv)') as HTMLSelectElement;
    fireEvent.change(kSelect, { target: { value: 'q8_0' } });
    fireEvent.change(vSelect, { target: { value: 'q4_0' } });

    expect(kSelect.value).toBe('q8_0');
    expect(vSelect.value).toBe('q4_0');
  });

  it('does not offer tq3_0 — the estimator cannot price it, so it must not be admitted', () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: /Advanced launch profile/ }));
    const kSelect = screen.getByLabelText('K cache (-ctk)') as HTMLSelectElement;
    const values = Array.from(kSelect.options).map((o) => o.value);
    expect(values).not.toContain('tq3_0');
    // '' renders as "f16 (default)"; there is deliberately no SECOND explicit 'f16' entry, since two
    // options producing an identical cache dtype differ only by emitting a redundant flag. A browser
    // check caught that duplication — the earlier version of this assertion accepted it, because it
    // only checked the values were correct, never that the list read sensibly.
    expect(values).toEqual(['', 'q8_0', 'q4_0']);
    const labels = Array.from(kSelect.options).map((o) => o.textContent);
    expect(labels.filter((l) => l && l.includes('f16'))).toHaveLength(1);
  });

  it('offers only VALIDATED speculative modes, not the full advertised set', () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: /Advanced launch profile/ }));
    const sel = screen.getByLabelText('Speculative') as HTMLSelectElement;
    const values = Array.from(sel.options).map((o) => o.value);
    expect(values).toEqual(['', 'ngram-cache', 'draft-mtp']);
    // b10106 also advertises these, but they are untested here, so they are not offered.
    expect(values).not.toContain('draft-eagle3');
    expect(values).not.toContain('draft-dflash');
  });

  it('disables the draft max until a speculative mode is chosen', () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: /Advanced launch profile/ }));
    const draftMax = screen.getByLabelText('Draft max') as HTMLInputElement;
    expect(draftMax).toBeDisabled();

    fireEvent.change(screen.getByLabelText('Speculative'), {
      target: { value: 'draft-mtp' },
    });
    expect(screen.getByLabelText('Draft max')).not.toBeDisabled();
  });

  it('pre-fills the form from a saved default and surfaces its validated note', async () => {
    vi.mocked(api.modelLaunchDefault).mockResolvedValue(savedMTPDefault);
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: /Advanced launch profile/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Load saved' }));

    await waitFor(() => {
      expect((screen.getByLabelText('K cache (-ctk)') as HTMLSelectElement).value).toBe('q8_0');
    });
    expect((screen.getByLabelText('V cache (-ctv)') as HTMLSelectElement).value).toBe('q4_0');
    expect((screen.getByLabelText('Speculative') as HTMLSelectElement).value).toBe('draft-mtp');
    expect(screen.getByText(/validated at 8K/)).toBeInTheDocument();
  });

  it('surfaces a STALE saved profile (validated on a different engine build — M6.2)', async () => {
    vi.mocked(api.modelLaunchDefault).mockResolvedValue({
      ...savedMTPDefault,
      stale_reason:
        'this profile was validated on llama.cpp build 10106 (1425386fd), which is not among the currently detected binaries',
    });
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: /Advanced launch profile/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Load saved' }));

    await waitFor(() => {
      expect(
        screen.getByText(/not among the currently detected binaries/),
      ).toBeInTheDocument();
    });
  });

  it('treats a 404 as "nothing saved yet", not an error', async () => {
    vi.mocked(api.modelLaunchDefault).mockRejectedValue(
      new ApiError('not_found', 'no saved launch default', 'none saved', 404),
    );
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: /Advanced launch profile/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Load saved' }));

    await waitFor(() => {
      expect(screen.getByText(/No saved profile for this model yet/)).toBeInTheDocument();
    });
  });

  it('sends the armed profile when saving a default', async () => {
    vi.mocked(api.saveModelLaunchDefault).mockResolvedValue(savedMTPDefault);
    render(<Harness initial={profileFromDefault(savedMTPDefault)} />);
    fireEvent.click(screen.getByRole('button', { name: /Advanced launch profile/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Save as default' }));

    await waitFor(() => {
      expect(api.saveModelLaunchDefault).toHaveBeenCalledWith('qwen36-mtp', {
        ctx: 8192,
        kv_type_k: 'q8_0',
        kv_type_v: 'q4_0',
        slots: 1,
        spec_mode: 'draft-mtp',
        spec_draft_max: 2,
        allow_cpu_offload: false,
      });
    });
  });

  it('surfaces a server refusal reason verbatim when a save is rejected', async () => {
    vi.mocked(api.saveModelLaunchDefault).mockRejectedValue(
      new ApiError(
        'bad_request',
        'could not save',
        'invalid kv_type_k value "tq3_0": valid values are f16, q8_0, or q4_0',
        400,
      ),
    );
    render(<Harness initial={{ ...EMPTY_PROFILE, kvTypeK: 'q8_0' }} />);
    fireEvent.click(screen.getByRole('button', { name: /Advanced launch profile/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Save as default' }));

    await waitFor(() => {
      expect(screen.getByText(/invalid kv_type_k value/)).toBeInTheDocument();
    });
  });

  it('disables the saved-default actions when no model is selected', () => {
    render(<Harness modelId={null} />);
    fireEvent.click(screen.getByRole('button', { name: /Advanced launch profile/ }));
    expect(screen.getByRole('button', { name: 'Load saved' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Save as default' })).toBeDisabled();
  });

  it('Clear resets every field back to the daemon defaults', () => {
    render(<Harness initial={profileFromDefault(savedMTPDefault)} />);
    fireEvent.click(screen.getByRole('button', { name: /Advanced launch profile/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Clear' }));

    expect((screen.getByLabelText('K cache (-ctk)') as HTMLSelectElement).value).toBe('');
    expect((screen.getByLabelText('V cache (-ctv)') as HTMLSelectElement).value).toBe('');
    expect((screen.getByLabelText('Context (-c)') as HTMLInputElement).value).toBe('');
    expect((screen.getByLabelText('Speculative') as HTMLSelectElement).value).toBe('');
  });
});
