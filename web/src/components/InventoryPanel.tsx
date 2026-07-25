import { useState } from 'react';
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

// Per-model preview state: the fetched verdict, an in-flight flag, and an error reason.
interface PreviewState {
  verdict?: Verdict;
  loading: boolean;
  error?: string;
}

export function InventoryPanel(props: {
  models: ModelSummary[] | null;
  loading: boolean;
  error: string | null;
  onRescanned: () => void;
}) {
  const [rescanning, setRescanning] = useState(false);
  const [rescanError, setRescanError] = useState<string | null>(null);
  // Keyed by model id — the verdict shown inline after "Preview fit".
  const [previews, setPreviews] = useState<Record<string, PreviewState>>({});

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
        <IconButton
          onClick={rescan}
          disabled={rescanning}
          title="Rescan model directories (control-plane; walks disk)"
        >
          {rescanning ? 'Rescanning…' : 'Rescan'}
        </IconButton>
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
          <Table models={models} previews={previews} onPreview={preview} />
          <Cards models={models} previews={previews} onPreview={preview} />
        </>
      )}

      {props.error && (
        <ErrorState>Could not load inventory: {props.error}</ErrorState>
      )}
      {rescanError && <ErrorState>Rescan failed: {rescanError}</ErrorState>}
    </Panel>
  );
}

// PreviewButton renders the "Preview fit" action (or a spinner) and, once a verdict arrives,
// the full verdict view. The cached compat_status (from the models row) shows as a badge until
// a fresh preview is run.
function PreviewCell({
  model,
  state,
  onPreview,
}: {
  model: ModelSummary;
  state: PreviewState | undefined;
  onPreview: (id: string) => void;
}) {
  return (
    <div className={styles.compatCell}>
      {state?.verdict ? (
        <CompatVerdictView verdict={state.verdict} />
      ) : (
        <div className={styles.compatRow}>
          {model.compat_status && (
            <CompatBadge status={model.compat_status as Verdict['status']} />
          )}
          <IconButton
            onClick={() => onPreview(model.id)}
            disabled={state?.loading}
            title="Compute a compatibility verdict for this model (no download)"
          >
            {state?.loading ? 'Checking…' : 'Preview fit'}
          </IconButton>
        </div>
      )}
      {state?.error && <ErrorState>Preview failed: {state.error}</ErrorState>}
    </div>
  );
}

function Table({
  models,
  previews,
  onPreview,
}: {
  models: ModelSummary[];
  previews: Record<string, PreviewState>;
  onPreview: (id: string) => void;
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
                <PreviewCell model={m} state={previews[m.id]} onPreview={onPreview} />
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
  onPreview,
}: {
  models: ModelSummary[];
  previews: Record<string, PreviewState>;
  onPreview: (id: string) => void;
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
            <div className={styles.cardPath}>{m.local_path ?? m.repo}</div>
          )}
          <div className={styles.cardCompat}>
            <PreviewCell model={m} state={previews[m.id]} onPreview={onPreview} />
          </div>
        </div>
      ))}
    </div>
  );
}
