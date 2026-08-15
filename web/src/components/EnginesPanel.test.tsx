import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { EnginesPanel } from './EnginesPanel';
import { enginesFixture } from '../test/fixtures';
import type { Engine } from '../api/types';

describe('EnginesPanel', () => {
  it('renders detected/reachable badges and version', () => {
    render(
      <EnginesPanel engines={enginesFixture} loading={false} error={null} />,
    );
    expect(screen.queryByText('Ollama')).not.toBeInTheDocument();
    expect(screen.getByText('8833 (45cac7ca7)')).toBeInTheDocument();
    // ollama + llamacpp reachable → two "reachable" badges
    expect(screen.getAllByText('reachable')).toHaveLength(1);
  });

  it('shows the mandatory reason for an unreachable engine', () => {
    render(
      <EnginesPanel engines={enginesFixture} loading={false} error={null} />,
    );
    // vLLM is unreachable → its reason string must be visible
    expect(screen.getByText('unreachable')).toBeInTheDocument();
    expect(
      screen.getByText(/image present.*not started.*P4\.5/i),
    ).toBeInTheDocument();
  });

  it('renders backend + capability chips', () => {
    render(
      <EnginesPanel engines={enginesFixture} loading={false} error={null} />,
    );
    expect(screen.getByText('Vulkan')).toBeInTheDocument();
    expect(screen.getByText('flash-attn')).toBeInTheDocument();
    expect(screen.getByText('sleep-idle')).toBeInTheDocument();
  });

  it('renders a reason even when an engine is not found', () => {
    const missing: Engine[] = [
      {
        name: 'vllm',
        installed: false,
        version: '',
        running: false,
        base_url: '',
        reason: 'engine vllm disabled in config',
      },
    ];
    render(<EnginesPanel engines={missing} loading={false} error={null} />);
    expect(screen.getByText('not found')).toBeInTheDocument();
    expect(
      screen.getByText('engine vllm disabled in config'),
    ).toBeInTheDocument();
  });

  // §3.5 — three llama.cpp builds are detected on the reference box and, before this, nothing on
  // screen said which one a start would actually use. Selection walks CONFIGURED order and takes
  // the first detected build that satisfies the model (compat/input.go bestLlamaBinary).
  it('marks which of several detected llama.cpp builds is used, and why the others are not', () => {
    const threeBuilds: Engine[] = [
      {
        name: 'llamacpp',
        installed: true,
        version: '10106 (1425386fd)',
        running: true,
        base_url: '',
        reason: 'llama-server detected',
        details: {
          resolved_path: 'K:\\tools\\llama.cpp\\llama-server.exe',
          backends: ['CPU', 'CUDA', 'RPC'],
          capabilities: { has_spec_mtp: true, has_flash_attn: true, has_tq3: false },
        },
      },
      {
        name: 'llamacpp',
        installed: true,
        version: '1 (1cfa891)',
        running: true,
        base_url: '',
        reason: 'llama-server detected (fork)',
        details: {
          resolved_path: 'K:\\llama.cpp-tq3\\llama-server.exe',
          backends: ['CUDA'],
          capabilities: { has_tq3: true },
        },
      },
      {
        name: 'llamacpp',
        installed: true,
        version: '8833 (45cac7ca7)',
        running: true,
        base_url: '',
        reason: 'winget llama-server on PATH',
        details: { resolved_path: 'llama-server', backends: ['Vulkan'], capabilities: {} },
      },
    ];
    render(<EnginesPanel engines={threeBuilds} loading={false} error={null} />);

    // Exactly ONE build is marked as the default choice.
    expect(screen.getAllByText('used by default')).toHaveLength(1);
    // The TQ3 fork is reachable only for TQ3_* quants — say so rather than leaving it ambiguous.
    expect(screen.getByText(/fallback: TQ3_\* quants/)).toBeInTheDocument();
    // The third build has no capability the default lacks, so it gets no misleading role label.
    expect(screen.queryByText(/fallback: draft-mtp/)).toBeNull();
  });

  it('derives capability chips from the daemon\'s boolean capability map', () => {
    // The llama.cpp adapter reports `capabilities` (a bool map), not the `caps` string list the
    // original projection assumed — so on a real box the chip row showed backends only.
    const withCapMap: Engine[] = [
      {
        name: 'llamacpp',
        installed: true,
        running: true,
        base_url: '',
        reason: 'detected',
        details: { capabilities: { has_flash_attn: true, has_tq3: false, has_spec_mtp: true } },
      },
    ];
    render(<EnginesPanel engines={withCapMap} loading={false} error={null} />);
    expect(screen.getByText('flash-attn')).toBeInTheDocument();
    expect(screen.getByText('spec-mtp')).toBeInTheDocument();
    // A false capability is NOT advertised.
    expect(screen.queryByText('tq3')).toBeNull();
  });

  it('surfaces a load error reason', () => {
    render(
      <EnginesPanel
        engines={null}
        loading={false}
        error="Could not reach the daemon (Failed to fetch)."
      />,
    );
    expect(
      screen.getByText(/Could not load engines:.*Could not reach the daemon/i),
    ).toBeInTheDocument();
  });
});
