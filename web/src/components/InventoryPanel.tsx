import { useState } from 'react';
import { isQueuedResponse } from '../api/client';
import type { ModelSummary, Verdict } from '../api/types';
import { api } from '../api/client';
import { ApiError } from '../api/client';
import { formatBytes } from '../lib/format';
import {
  EmptyState,
  ErrorState,
  IconButton,
  Panel,
  Reason,
  Skeleton,
} from './ui';
import { PresenceBadge, SourceBadge } from './SourceBadge';
import { CompatBadge, CompatVerdictView } from './CompatVerdict';
import styles from './InventoryPanel.module.css';

// A model is "present" unless the daemon's dedicated `presence` field marks it missing
// (a row whose file vanished from disk is kept, not deleted; core.ModelSummary.Presence).
function isPresent(m: ModelSummary): boolean {
  return m.presence !== 'missing';
}

// A model is runnable from Inventory when it's present and its format has a spawnable engine
// (GGUF via llama.cpp, or an Ollama model via ollama). Safetensors/pytorch have no Phase-4
// spawn path, so we don't offer Run for them. The daemon is the source of truth: it will still
// refuse (with a numeric reason) or queue anything the UI lets through.
function isRunnable(m: ModelSummary): boolean {
  return isPresent(m) && m.format === 'gguf';
}

// Per-model preview state: the fetched verdict, an in-flight flag, and an error reason.
interface PreviewState {
  verdict?: Verdict;
  loading: boolean;
  error?: string;
}

// Per-model run state: an in-flight flag, a success/queued note, and a refusal reason. The
// three outcomes mirror POST /api/instances (200 started · 202 queued · 409 refused).
type RunTone = 'ok' | 'warn' | 'crit';
interface RunState {
  loading: boolean;
  note?: string; // human-readable outcome (started / queued / refused)
  tone?: RunTone;
}

export function InventoryPanel(props: {
  models: ModelSummary[] | null;
  loading: boolean;
  error: string | null;
  onRescanned: () => void;
  // Shared GPU/CPU placement toggle (lifted to App; same setting the Instances picker drives).
  // false = full GPU (never split to CPU). Run reads it; the ACTIONS-header annotation flips it.
  allowCpuOffload: boolean;
  onAllowCpuOffloadChange: (v: boolean) => void;
}) {
  const [rescanning, setRescanning] = useState(false);
  const [rescanError, setRescanError] = useState<string | null>(null);
  // Keyed by model id — the verdict shown inline after "Preview fit".
  const [previews, setPreviews] = useState<Record<string, PreviewState>>({});
  // Keyed by model id — the Run outcome shown inline beside the row's actions.
  const [runs, setRuns] = useState<Record<string, RunState>>({});

  async function rescan() {
    setRescanning(true);
    setRescanError(null);
    try {
      await api.rescanModels();
      props.onRescanned();
    } catch (e) {
      setRescanError(e instanceof ApiError ? e.reason : String(e));
    } finally {
      setRescanning(false);
    }
  }

  async function preview(id: string) {
    setPreviews((p) => ({ ...p, [id]: { ...p[id], loading: true, error: undefined } }));
    try {
      const verdict = await api.modelCompat(id);
      setPreviews((p) => ({ ...p, [id]: { verdict, loading: false } }));
    } catch (e) {
      setPreviews((p) => ({
        ...p,
        [id]: { loading: false, error: e instanceof ApiError ? e.reason : String(e) },
      }));
    }
  }

  // Run — the PRIMARY product action. POST /api/instances has three outcomes:
  //   200 → started (or already warm); surface the new state.
  //   202 → residency QUEUED the start on VRAM; surface position + blocked_reason (nothing spawned).
  //   409 → the daemon REFUSED (verdict not runs/degraded, or over budget); surface the numeric
  //         reason (source of truth is the core, not a UI filter).
  // The Instances panel picks up any spawned/queued instance over the live channel — we don't
  // reach across components here; we only report the outcome inline.
  async function run(id: string) {
    setRuns((r) => ({ ...r, [id]: { loading: true } }));
    try {
      const res = await api.startInstance(
        { model_id: id },
        { allowCpuOffload: props.allowCpuOffload },
      );
      if (isQueuedResponse(res)) {
        setRuns((r) => ({
          ...r,
          [id]: {
            loading: false,
            tone: 'warn',
            note: `Queued (position ${res.position}): ${res.blocked_reason}`,
          },
        }));
      } else {
        setRuns((r) => ({
          ...r,
          [id]: {
            loading: false,
            tone: 'ok',
            note: `Starting — ${res.state}. ${res.reason ?? ''}`.trim(),
          },
        }));
      }
    } catch (e) {
      setRuns((r) => ({
        ...r,
        [id]: {
          loading: false,
          tone: 'crit',
          note: e instanceof ApiError ? e.reason : String(e),
        },
      }));
    }
  }

  const models = props.models ?? [];

  return (
    <Panel
      title={
        <>
          Inventory
          {props.models && <span className={styles.count}>· {models.length}</span>}
        </>
      }
      flush
      actions={
        <div className={styles.headerActions}>
          {/* Shared GPU/CPU placement mode — reflects the Instances picker toggle; click to flip.
              Run uses this setting. Off = full GPU, never split to CPU. */}
          <button
            type="button"
            className={styles.offloadTag}
            aria-pressed={props.allowCpuOffload}
            onClick={() => props.onAllowCpuOffloadChange(!props.allowCpuOffload)}
            title="GPU/CPU placement for Run. Off = full GPU (never split to CPU). Click to toggle."
          >
            {props.allowCpuOffload ? 'CPU offload allowed' : 'full GPU'}
          </button>
          <IconButton
            onClick={rescan}
            disabled={rescanning}
            title="Rescan model directories (control-plane; walks disk)"
          >
            {rescanning ? 'Rescanning…' : 'Rescan'}
          </IconButton>
        </div>
      }
    >
      {props.loading && !props.models ? (
        <Skeleton>Loading inventory…</Skeleton>
      ) : models.length === 0 && !props.error ? (
        <EmptyState>
          No models found.
          <br />
          <Reason>
            The scanner found no GGUF/safetensors/Ollama models in the configured
            model directories. Try Rescan after adding models.
          </Reason>
        </EmptyState>
      ) : (
        <>
          <Table
            models={models}
            previews={previews}
            runs={runs}
            onPreview={preview}
            onRun={run}
          />
          <Cards
            models={models}
            previews={previews}
            runs={runs}
            onPreview={preview}
            onRun={run}
          />
        </>
      )}

      {props.error && (
        <ErrorState>Could not load inventory: {props.error}</ErrorState>
      )}
      {rescanError && <ErrorState>Rescan failed: {rescanError}</ErrorState>}
    </Panel>
  );
}

// PreviewCell shows compatibility for a row: the full verdict view once "Preview fit" has been
// run, otherwise the cached compat_status badge (or a muted "unchecked" hint). The button that
// triggers a preview lives in RunCell (Actions column) so the primary Run action sits with it.
function PreviewCell({
  model,
  state,
}: {
  model: ModelSummary;
  state: PreviewState | undefined;
}) {
  return (
    <div className={styles.compatCell}>
      {state?.verdict ? (
        <CompatVerdictView verdict={state.verdict} />
      ) : model.compat_status && model.compat_status !== 'unknown' ? (
        <CompatBadge status={model.compat_status as Verdict['status']} />
      ) : (
        <span className={styles.unchecked}>unchecked</span>
      )}
      {state?.error && <ErrorState>Preview failed: {state.error}</ErrorState>}
    </div>
  );
}

// RunCell — the PRIMARY action per inventory row: a "Run" button that starts the model. Only
// rendered for runnable models (present GGUF / Ollama). "Preview fit" sits beside it as the
// SECONDARY action. The outcome (started / queued / refused) prints inline with a mandatory
// reason, colored by tone; a critical (refusal) note reads as an ErrorState.
function RunCell({
  model,
  preview,
  run,
  onPreview,
  onRun,
}: {
  model: ModelSummary;
  preview: PreviewState | undefined;
  run: RunState | undefined;
  onPreview: (id: string) => void;
  onRun: (id: string) => void;
}) {
  const runnable = isRunnable(model);
  return (
    <div className={styles.actionCell}>
      <div className={styles.actionRow}>
        {runnable && (
          <button
            type="button"
            className={styles.runBtn}
            disabled={run?.loading}
            onClick={() => onRun(model.id)}
            title="Start this model (POST /api/instances — spawns the engine process)"
          >
            {run?.loading ? 'Starting…' : 'Run'}
          </button>
        )}
        <IconButton
          onClick={() => onPreview(model.id)}
          disabled={preview?.loading}
          title="Compute a compatibility verdict for this model (no download, no spawn)"
        >
          {preview?.loading ? 'Checking…' : 'Preview fit'}
        </IconButton>
      </div>
      {run?.note &&
        (run.tone === 'crit' ? (
          <ErrorState>{run.note}</ErrorState>
        ) : (
          <Reason>{run.note}</Reason>
        ))}
    </div>
  );
}

function Table({
  models,
  previews,
  runs,
  onPreview,
  onRun,
}: {
  models: ModelSummary[];
  previews: Record<string, PreviewState>;
  runs: Record<string, RunState>;
  onPreview: (id: string) => void;
  onRun: (id: string) => void;
}) {
  return (
    <div className={styles.tableWrap}>
      <table className={styles.table}>
        <thead>
          <tr>
            <th>Name</th>
            <th>Source</th>
            <th>Format</th>
            <th>Quant</th>
            <th>Size</th>
            <th>Presence</th>
            <th>Compatibility</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          {models.map((m) => (
            <tr key={m.id}>
              <td className={styles.modelName}>{m.display_name}</td>
              <td>
                <SourceBadge source={m.source} format={m.format} />
              </td>
              <td>{m.format}</td>
              <td className={styles.quant}>{m.quantization ?? '—'}</td>
              <td className={styles.size}>{formatBytes(m.size_bytes)}</td>
              <td>
                <PresenceBadge present={isPresent(m)} />
              </td>
              <td className={styles.compatTd}>
                <PreviewCell model={m} state={previews[m.id]} />
              </td>
              <td className={styles.actionTd}>
                <RunCell
                  model={m}
                  preview={previews[m.id]}
                  run={runs[m.id]}
                  onPreview={onPreview}
                  onRun={onRun}
                />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function Cards({
  models,
  previews,
  runs,
  onPreview,
  onRun,
}: {
  models: ModelSummary[];
  previews: Record<string, PreviewState>;
  runs: Record<string, RunState>;
  onPreview: (id: string) => void;
  onRun: (id: string) => void;
}) {
  return (
    <div className={styles.cards}>
      {models.map((m) => (
        <div key={m.id} className={styles.card}>
          <div className={styles.cardHead}>
            <span className={styles.modelName}>{m.display_name}</span>
            <PresenceBadge present={isPresent(m)} />
          </div>
          <div className={styles.cardMeta}>
            <span>
              <SourceBadge source={m.source} format={m.format} />
            </span>
            <span>
              fmt <b>{m.format}</b>
            </span>
            {m.quantization && (
              <span>
                quant <b>{m.quantization}</b>
              </span>
            )}
            <span>
              size <b>{formatBytes(m.size_bytes)}</b>
            </span>
          </div>
          {(m.local_path || m.repo) && (
            <div className={styles.cardPath} title={m.local_path ?? m.repo ?? ''}>
              {m.local_path ?? m.repo}
            </div>
          )}
          <div className={styles.cardCompat}>
            <PreviewCell model={m} state={previews[m.id]} />
            <RunCell
              model={m}
              preview={previews[m.id]}
              run={runs[m.id]}
              onPreview={onPreview}
              onRun={onRun}
            />
          </div>
        </div>
      ))}
    </div>
  );
}
