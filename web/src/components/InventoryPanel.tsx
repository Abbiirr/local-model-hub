import { useMemo, useState } from 'react';
import { isQueuedResponse } from '../api/client';
import type { LaunchState, ModelSummary, Verdict } from '../api/types';
import { api } from '../api/client';
import { ApiError } from '../api/client';
import { useCompatVerdicts, type CompatEntry } from '../api/useCompatVerdicts';
import { formatBytes, formatMib } from '../lib/format';
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

// ── Launch state (residency) ────────────────────────────────────────────────────
// The four-value vocabulary the daemon accepts on PUT /api/models/{id}/launch-state.
// These describe RESIDENCY (whether the model stays loaded in VRAM) — NOT visibility in
// any list. Order matches the "coldest to warmest" reading the operator expects in a
// dropdown, with the daemon's own default (available_on_request) third.
const LAUNCH_STATES: { value: LaunchState; label: string; hint: string }[] = [
  {
    value: 'always_available',
    label: 'Always keep available',
    hint: 'Kept loaded even when idle; never evicted, reloaded if it dies.',
  },
  {
    value: 'available',
    label: 'Available',
    hint: 'Loaded now; may still be evicted if VRAM runs short.',
  },
  {
    value: 'available_on_request',
    label: 'Available on request',
    hint: 'Cold until a request arrives, then unloads again when idle. (default)',
  },
  {
    value: 'unavailable',
    label: 'Unavailable',
    hint: 'Never loads automatically — start it manually.',
  },
];

const KNOWN_LAUNCH_STATES = new Set(LAUNCH_STATES.map((s) => s.value));

// A missing/unrecognized value (older daemon, or a value this build doesn't know yet) MUST
// degrade to the daemon's own default rather than being read as `unavailable` — silently
// treating an unknown state as "never auto-load" would be a behavior claim the UI can't back up.
function launchStateOf(m: ModelSummary): LaunchState {
  return m.launch_state && KNOWN_LAUNCH_STATES.has(m.launch_state)
    ? m.launch_state
    : 'available_on_request';
}

// Per-model launch-state row state: an in-flight flag and a failure reason (the select reverts
// to the model's actual state on error — nothing is assumed to have taken effect).
interface LaunchStateRowState {
  loading: boolean;
  error?: string;
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
  // Fires after a launch-state change is accepted by the daemon so the caller can refetch the
  // models list (mirrors onRescanned — same "refresh the models query" trigger, App.tsx wires
  // both to models.refresh()).
  onLaunchStateChanged: () => void;
  /** Bumped by App when free VRAM changed materially (a Free-GPU sweep) — the invalidation signal
   *  for cached fit verdicts, which are only true for the VRAM they were computed against. */
  compatEpoch?: number;
}) {
  const [rescanning, setRescanning] = useState(false);
  const [rescanError, setRescanError] = useState<string | null>(null);
  // Vanished models remain in the catalog for manifests and instance history, but the normal
  // inventory is the set that exists now. Keep historical rows behind an explicit toggle so a
  // rescan cannot leave the registry looking as though deleted model files are still available.
  const [showMissing, setShowMissing] = useState(false);
  // Keyed by model id — the verdict shown inline after "Preview fit".
  const [previews, setPreviews] = useState<Record<string, PreviewState>>({});
  // Keyed by model id — the Run outcome shown inline beside the row's actions.
  const [runs, setRuns] = useState<Record<string, RunState>>({});
  // Keyed by model id — in-flight/error state for the launch-state select.
  const [launchStates, setLaunchStates] = useState<Record<string, LaunchStateRowState>>({});

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

  // Change a model's residency launch state. On success, refetch the models list (the daemon's
  // response also carries the updated summary, but the list is the single source of truth other
  // panels read from — refetching keeps them all consistent rather than patching one row locally).
  async function changeLaunchState(id: string, state: LaunchState) {
    setLaunchStates((s) => ({ ...s, [id]: { loading: true } }));
    try {
      await api.setModelLaunchState(id, state);
      setLaunchStates((s) => ({ ...s, [id]: { loading: false } }));
      props.onLaunchStateChanged();
    } catch (e) {
      setLaunchStates((s) => ({
        ...s,
        [id]: { loading: false, error: e instanceof ApiError ? e.reason : String(e) },
      }));
    }
  }

  const allModels = props.models ?? [];
  const missingCount = allModels.filter((m) => !isPresent(m)).length;
  const models = showMissing ? allModels : allModels.filter(isPresent);

  // §3.3 — real verdicts inline, so the Compatibility column stops reading "unchecked" for every
  // row. Only PRESENT models are fetched: a model whose file vanished cannot be measured, and
  // asking would spend a request to learn nothing. Memoised on the id list so a parent re-render
  // (or a models refetch returning identical rows) does not look like a new set of ids.
  const presentIds = useMemo(
    () => models.filter(isPresent).map((m) => m.id),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [models.filter(isPresent).map((m) => m.id).join('\n')],
  );
  const verdicts = useCompatVerdicts(presentIds, props.compatEpoch ?? 0);

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
          {missingCount > 0 && (
            <IconButton
              onClick={() => setShowMissing((visible) => !visible)}
              aria-pressed={showMissing}
              title="Show or hide catalog history for model files that are no longer present"
            >
              {showMissing ? 'Hide missing' : `Show missing (${missingCount})`}
            </IconButton>
          )}
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
          No present models found.
          <br />
          <Reason>
            {missingCount > 0
              ? `${missingCount} missing catalog ${missingCount === 1 ? 'record is' : 'records are'} hidden. Use Show missing to inspect history.`
              : 'The scanner found no GGUF/safetensors/Ollama models in the configured model directories. Try Rescan after adding models.'}
          </Reason>
        </EmptyState>
      ) : (
        <>
          <Table
            models={models}
            previews={previews}
            verdicts={verdicts}
            runs={runs}
            launchStates={launchStates}
            onPreview={preview}
            onRun={run}
            onLaunchStateChange={changeLaunchState}
          />
          <Cards
            models={models}
            previews={previews}
            verdicts={verdicts}
            runs={runs}
            launchStates={launchStates}
            onPreview={preview}
            onRun={run}
            onLaunchStateChange={changeLaunchState}
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

// PreviewCell shows compatibility for a row, in decreasing order of authority:
//   1. the full verdict view, once the operator explicitly ran "Preview fit" on this row;
//   2. the AUTO-fetched verdict (§3.3) — a badge, its estimated VRAM, and the mandatory reason as
//      a tooltip. This is what replaced a column of "unchecked" hints;
//   3. the cached compat_status from the inventory row, while (2) is still in flight;
//   4. "unchecked" — now only reachable for a MISSING model (never fetched) or a failed fetch.
// A failed fetch must land on (3)/(4), never on a manufactured `wont_run`: not knowing whether a
// model fits is not the same as knowing it does not.
function PreviewCell({
  model,
  state,
  entry,
}: {
  model: ModelSummary;
  state: PreviewState | undefined;
  entry: CompatEntry | undefined;
}) {
  const cached =
    model.compat_status && model.compat_status !== 'unknown'
      ? (model.compat_status as Verdict['status'])
      : undefined;

  let body;
  if (state?.verdict) {
    body = <CompatVerdictView verdict={state.verdict} />;
  } else if (entry?.state === 'ready') {
    body = (
      <span className={styles.autoVerdict} title={entry.reason}>
        <CompatBadge status={entry.status} />
        {entry.estimatedVramMb != null && (
          <span className={styles.autoEstimate}>~{formatMib(entry.estimatedVramMb)}</span>
        )}
      </span>
    );
  } else if (cached) {
    body = <CompatBadge status={cached} />;
  } else if (entry?.state === 'loading') {
    body = <span className={styles.unchecked}>checking…</span>;
  } else {
    body = (
      <span
        className={styles.unchecked}
        title={entry?.state === 'error' ? `Fit check failed: ${entry.reason}` : undefined}
      >
        unchecked
      </span>
    );
  }

  return (
    <div className={styles.compatCell}>
      {body}
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

// LaunchStateCell — the residency control: a compact select showing the model's current
// launch state (defaulting an absent/unknown value to available_on_request) with a terse
// consequence line underneath. Disabled while a change is in flight; reverts visually the
// moment the parent's models list refetches (or shows the failure reason on error).
function LaunchStateCell({
  model,
  state,
  onChange,
}: {
  model: ModelSummary;
  state: LaunchStateRowState | undefined;
  onChange: (id: string, state: LaunchState) => void;
}) {
  const current = launchStateOf(model);
  const active = LAUNCH_STATES.find((s) => s.value === current) ?? LAUNCH_STATES[2];
  return (
    <div className={styles.launchCell}>
      <select
        className={styles.launchSelect}
        aria-label={`Launch state for ${model.display_name}`}
        value={current}
        disabled={state?.loading}
        title={active.hint}
        onChange={(e) => onChange(model.id, e.target.value as LaunchState)}
      >
        {LAUNCH_STATES.map((s) => (
          <option key={s.value} value={s.value}>
            {s.label}
          </option>
        ))}
      </select>
      <span className={styles.launchHint} title={active.hint}>
        {state?.loading ? 'Saving…' : active.hint}
      </span>
      {state?.error && <ErrorState>Launch state change failed: {state.error}</ErrorState>}
    </div>
  );
}

function Table({
  models,
  previews,
  verdicts,
  runs,
  launchStates,
  onPreview,
  onRun,
  onLaunchStateChange,
}: {
  models: ModelSummary[];
  previews: Record<string, PreviewState>;
  verdicts: Record<string, CompatEntry>;
  runs: Record<string, RunState>;
  launchStates: Record<string, LaunchStateRowState>;
  onPreview: (id: string) => void;
  onRun: (id: string) => void;
  onLaunchStateChange: (id: string, state: LaunchState) => void;
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
            <th>Launch state</th>
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
              <td className={styles.launchTd}>
                <LaunchStateCell
                  model={m}
                  state={launchStates[m.id]}
                  onChange={onLaunchStateChange}
                />
              </td>
              <td className={styles.compatTd}>
                <PreviewCell
                  model={m}
                  state={previews[m.id]}
                  entry={verdicts[m.id]}
                />
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
  verdicts,
  runs,
  launchStates,
  onPreview,
  onRun,
  onLaunchStateChange,
}: {
  models: ModelSummary[];
  previews: Record<string, PreviewState>;
  verdicts: Record<string, CompatEntry>;
  runs: Record<string, RunState>;
  launchStates: Record<string, LaunchStateRowState>;
  onPreview: (id: string) => void;
  onRun: (id: string) => void;
  onLaunchStateChange: (id: string, state: LaunchState) => void;
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
          <div className={styles.cardLaunch}>
            <LaunchStateCell
              model={m}
              state={launchStates[m.id]}
              onChange={onLaunchStateChange}
            />
          </div>
          <div className={styles.cardCompat}>
            <PreviewCell
              model={m}
              state={previews[m.id]}
              entry={verdicts[m.id]}
            />
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
