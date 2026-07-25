import { useCallback, useEffect, useState } from 'react';
import type { InstanceState, InstanceView, ModelSummary } from '../api/types';
import { api, ApiError, isQueuedResponse } from '../api/client';
import {
  Panel,
  Badge,
  Reason,
  ClampText,
  EmptyState,
  ErrorState,
  Skeleton,
  type Tone,
} from './ui';
import styles from './InstancesCard.module.css';

// InstancesCard is the Phase-4 run/stop surface (P4.1/P4.2). It shows every spawned instance
// with its lifecycle state badge + mandatory reason + VRAM numbers + stop button, and offers a
// start button per PRESENT startable inventory model (GGUF or Ollama), each annotated with its
// cached compat status. A "won't fit" model is still submittable — the daemon is the authority
// and refuses with the numeric reason. ADMIN actions — starting/stopping spawns/kills GPU
// processes; the daemon binds loopback + token-gates.

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

// Annotate a picker option with its CACHED compat status. This is advisory only — a "won't fit"
// option is still submittable; the daemon is the authority and refuses with the numeric reason.
function compatAnnotation(status: string | undefined): { label: string; tone: Tone } {
  switch (status) {
    case 'runs':
      return { label: 'fits', tone: 'ok' };
    case 'runs_degraded':
      return { label: 'fits (degraded)', tone: 'warn' };
    case 'wont_run':
      return { label: "won't fit", tone: 'crit' };
    default:
      return { label: 'unchecked', tone: 'neutral' };
  }
}

export function InstancesCard(props: {
  models: ModelSummary[] | null;
  instancesInvalidation: number;
  // Shared GPU/CPU placement toggle (lifted to App). false = full GPU (never split to CPU).
  allowCpuOffload: boolean;
  onAllowCpuOffloadChange: (v: boolean) => void;
}) {
  const [instances, setInstances] = useState<InstanceView[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [startingModel, setStartingModel] = useState<string | null>(null);
  const [actionReason, setActionReason] = useState<string | null>(null);
  const [showHistory, setShowHistory] = useState(false);

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
        const res = await api.startInstance(
          { model_id: modelId },
          { allowCpuOffload: props.allowCpuOffload },
        );
        if (isQueuedResponse(res)) {
          // 202: residency queued the start on VRAM — nothing spawned yet. Surface the position +
          // blocked_reason so the operator knows why it's waiting (mandatory-reason rule).
          setActionReason(
            `Queued ${modelId} (position ${res.position}): ${res.blocked_reason}`,
          );
        } else {
          setActionReason(`Started ${modelId}: ${res.state}. ${res.reason ?? ''}`);
        }
        void refresh();
      } catch (e) {
        // A 409 REFUSED carries the numeric why in `reason` (gate).
        setActionReason(e instanceof ApiError ? `Refused: ${e.reason}` : String(e));
      } finally {
        setStartingModel(null);
      }
    },
    [refresh, props.allowCpuOffload],
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

  // Startable inventory models: every PRESENT model with a spawnable engine (GGUF via llama.cpp,
  // or an Ollama model), minus any already running. We deliberately do NOT filter by cached
  // compat_status — a model may be unchecked, or the operator may want to try a wont_run one and
  // let the daemon refuse with the authoritative numeric reason (source of truth is the core, not
  // a UI filter). Each option is annotated with its cached compat status.
  const activeModelIds = new Set(
    (instances ?? []).filter((i) => isActive(i.state)).map((i) => i.model_id),
  );
  const startable = (props.models ?? []).filter(
    (m) =>
      m.format === 'gguf' &&
      m.presence !== 'missing' &&
      !activeModelIds.has(m.id),
  );
  const inventoryEmpty = (props.models ?? []).length === 0;

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

          {/* Terminal/stopped instances are kept out of the default view (the
              homepage shows only what's live). They stay reachable behind a
              quiet, collapsed-by-default History toggle. */}
          {recent.length > 0 && (
            <div className={styles.history}>
              <button
                type="button"
                className={styles.historyToggle}
                onClick={() => setShowHistory((v) => !v)}
                aria-expanded={showHistory}
              >
                {showHistory ? '▾' : '▸'} History ({recent.length})
              </button>
              {showHistory &&
                recent.map((inst) => (
                  <InstanceRow key={inst.id} inst={inst} recent />
                ))}
            </div>
          )}
        </>
      )}

      {/* Start controls: one row per startable inventory model, annotated with its cached compat
          status. Every present GGUF/Ollama model is listed (not only cached-runs ones) — the
          daemon refuses an over-budget start with the authoritative numeric reason. */}
      <div className={styles.startSection}>
        <div className={styles.startHeading}>Start a model</div>
        {/* GPU/CPU placement toggle — applies to every start from this picker AND to Inventory
            Run (shared, lifted to App). Off by default: full GPU, never split to CPU. */}
        <label className={styles.offloadRow}>
          <input
            type="checkbox"
            className={styles.offloadInput}
            checked={props.allowCpuOffload}
            onChange={(e) => props.onAllowCpuOffloadChange(e.target.checked)}
          />
          <span className={styles.offloadLabel}>Allow CPU offload</span>
          <span className={styles.offloadHint}>
            default: full GPU — never split to CPU
          </span>
        </label>
        {startable.length === 0 ? (
          <EmptyState>
            {inventoryEmpty ? (
              <>
                No models in inventory yet.
                <br />
                Add one from the Downloads panel (Hugging Face, a GGUF URL, or a local
                import), then start it here.
              </>
            ) : (
              <>
                Every present model is already running.
                <br />
                Stop one above, or add more from the Downloads panel.
              </>
            )}
          </EmptyState>
        ) : (
          <ul className={styles.startList}>
            {startable.map((m) => {
              const annot = compatAnnotation(m.compat_status);
              return (
                <li key={m.id} className={styles.startItem}>
                  <div className={styles.startModel}>
                    <span className={styles.startName}>{m.display_name}</span>
                    <Badge tone={annot.tone}>{annot.label}</Badge>
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
              );
            })}
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

      {inst.reason && (
        <div className={styles.reason}>
          <ClampText lines={2} crit={inst.state === 'FAILED'}>
            {inst.reason}
          </ClampText>
        </div>
      )}
    </div>
  );
}
