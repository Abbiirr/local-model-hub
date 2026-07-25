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
