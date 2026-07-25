import { useCallback, useEffect, useState } from 'react';
import type { InstanceState, InstanceView, ModelSummary } from '../api/types';
import { api, ApiError } from '../api/client';
import {
  Panel,
  Badge,
  Reason,
  EmptyState,
  ErrorState,
  Skeleton,
  type Tone,
} from './ui';
import styles from './InstancesCard.module.css';

// InstancesCard is the Phase-4 run/stop surface (P4.1/P4.2). It shows every spawned instance
// with its lifecycle state badge + mandatory reason + VRAM numbers + stop button, and offers a
// start button per runnable inventory model (GGUF, compat runs/runs_degraded). ADMIN actions —
// starting/stopping spawns/kills GPU processes; the daemon binds loopback + token-gates.

// Map an instance state to a badge tone.
const STATE_TONE: Record<InstanceState, Tone> = {
  REQUESTED: 'neutral',
  STARTING: 'accent',
  HEALTHY: 'ok',
  DRAINING: 'warn',
  STOPPED: 'neutral',
  FAILED: 'crit',
};

function isActive(s: InstanceState): boolean {
  return s === 'REQUESTED' || s === 'STARTING' || s === 'HEALTHY' || s === 'DRAINING';
}

export function InstancesCard(props: {
  models: ModelSummary[] | null;
  instancesInvalidation: number;
}) {
  const [instances, setInstances] = useState<InstanceView[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [startingModel, setStartingModel] = useState<string | null>(null);
  const [actionReason, setActionReason] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const list = await api.instances();
      setInstances(Array.isArray(list) ? list : []);
      setError(null);
    } catch (e) {
      setError(e instanceof ApiError ? e.reason : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  // Initial load + refetch when the live channel signals instances.updated. Also a slow poll
  // fallback so STARTING→HEALTHY transitions surface even without a live push.
  useEffect(() => {
    void refresh();
  }, [refresh, props.instancesInvalidation]);

  useEffect(() => {
    const id = setInterval(() => void refresh(), 4000);
    return () => clearInterval(id);
  }, [refresh]);

  const onStart = useCallback(
    async (modelId: string) => {
      setStartingModel(modelId);
      setActionReason(null);
      try {
        const inst = await api.startInstance({ model_id: modelId });
        setActionReason(`Started ${modelId}: ${inst.state}. ${inst.reason ?? ''}`);
        void refresh();
      } catch (e) {
        // A 409 REFUSED carries the numeric why in `reason` (gate).
        setActionReason(e instanceof ApiError ? `Refused: ${e.reason}` : String(e));
      } finally {
        setStartingModel(null);
      }
    },
    [refresh],
  );

  const onStop = useCallback(
    async (id: string) => {
      setBusyId(id);
      setActionReason(null);
      try {
        const inst = await api.stopInstance(id);
        setActionReason(inst.reason ?? `Stopped ${id}.`);
        void refresh();
      } catch (e) {
        setActionReason(e instanceof ApiError ? e.reason : String(e));
      } finally {
        setBusyId(null);
      }
    },
    [refresh],
  );

  // Runnable inventory models: local GGUFs with a runs / runs_degraded compat badge, and no
  // active instance already running them.
  const activeModelIds = new Set(
    (instances ?? []).filter((i) => isActive(i.state)).map((i) => i.model_id),
  );
  const runnable = (props.models ?? []).filter(
    (m) =>
      m.format === 'gguf' &&
      m.presence !== 'missing' &&
      (m.compat_status === 'runs' || m.compat_status === 'runs_degraded') &&
      !activeModelIds.has(m.id),
  );

  const active = (instances ?? []).filter((i) => isActive(i.state));
  const recent = (instances ?? []).filter((i) => !isActive(i.state)).slice(0, 5);

  return (
    <Panel title="Instances">
      {actionReason && <Reason>{actionReason}</Reason>}
      {error && <ErrorState>{error}</ErrorState>}

      {loading && !instances ? (
        <Skeleton>Loading instances…</Skeleton>
      ) : (
        <>
          {active.length === 0 && recent.length === 0 && (
            <EmptyState>No instances yet. Start a runnable model below.</EmptyState>
          )}

          {active.map((inst) => (
            <InstanceRow
              key={inst.id}
              inst={inst}
              busy={busyId === inst.id}
              onStop={() => onStop(inst.id)}
            />
          ))}

          {recent.length > 0 && (
            <div className={styles.recentHeading}>Recent</div>
          )}
          {recent.map((inst) => (
            <InstanceRow key={inst.id} inst={inst} recent />
          ))}
        </>
      )}

      {/* Start controls: one button per runnable inventory model. */}
      <div className={styles.startSection}>
        <div className={styles.startHeading}>Start a model</div>
        {runnable.length === 0 ? (
          <EmptyState>
            No runnable GGUF models. Run a compatibility check on a local GGUF first.
          </EmptyState>
        ) : (
          <ul className={styles.startList}>
            {runnable.map((m) => (
              <li key={m.id} className={styles.startItem}>
                <div className={styles.startModel}>
                  <span className={styles.startName}>{m.display_name}</span>
                  <Badge tone={m.compat_status === 'runs' ? 'ok' : 'warn'}>
                    {m.compat_status}
                  </Badge>
                </div>
                <button
                  type="button"
                  className={styles.startBtn}
                  disabled={startingModel !== null}
                  onClick={() => onStart(m.id)}
                >
                  {startingModel === m.id ? 'Starting…' : 'Start'}
                </button>
              </li>
            ))}
          </ul>
        )}
      </div>
    </Panel>
  );
}

function InstanceRow(props: {
  inst: InstanceView;
  busy?: boolean;
  recent?: boolean;
  onStop?: () => void;
}) {
  const { inst } = props;
  return (
    <div className={`${styles.row} ${props.recent ? styles.rowRecent : ''}`}>
      <div className={styles.rowHead}>
        <Badge tone={STATE_TONE[inst.state]} dot>
          {inst.state}
        </Badge>
        <span className={styles.model}>{inst.model_id}</span>
        {inst.port ? <span className={styles.port}>:{inst.port}</span> : null}
        {!props.recent && props.onStop && (
          <button
            type="button"
            className={styles.stopBtn}
            disabled={props.busy}
            onClick={props.onStop}
          >
            {props.busy ? 'Stopping…' : 'Stop'}
          </button>
        )}
      </div>

      <div className={styles.meta}>
        {inst.vram_est_mib ? <span>est {inst.vram_est_mib} MiB</span> : null}
        {inst.vram_obs_mib ? <span>peak {inst.vram_obs_mib} MiB</span> : null}
        {inst.vram_freed_mib ? <span>freed {inst.vram_freed_mib} MiB</span> : null}
        {inst.ctx ? <span>ctx {inst.ctx}</span> : null}
        {inst.pid ? <span>pid {inst.pid}</span> : null}
      </div>

      {inst.reason && <Reason crit={inst.state === 'FAILED'}>{inst.reason}</Reason>}
    </div>
  );
}
