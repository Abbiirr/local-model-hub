import { useState } from 'react';
import { api, ApiError } from '../api/client';
import type { FreeGPUResult } from '../api/types';
import { formatMib } from '../lib/format';
import { Reason } from './ui';
import styles from './FreeGpuButton.module.css';

// "Give me my GPU back" in one action (PLAN-ui-gpu-rework.md §3.2).
//
// This is an ADMIN action: it kills engine processes. Three things it must never do, and the
// shape of this component follows from them:
//   1. Fire on a single click. A mis-click would stop every running model, so the button arms a
//      confirm step that spells out what is about to happen.
//   2. Claim VRAM came back. The daemon MEASURES an NVML delta before/after; we render THAT
//      number (`reclaimed_mib`), never a sum of estimates, and never "success" from a 200.
//   3. Silently override a pin. Pinned instances are skipped by default; when any were, we say
//      so and offer the explicit include_pinned retry rather than doing it automatically.

type Phase = 'idle' | 'confirm' | 'working';

export function FreeGpuButton(props: {
  /** Fired after a sweep that actually stopped something — free VRAM moved, so cached compat
   *  verdicts and the instances list are stale. */
  onFreed?: () => void;
}) {
  const [phase, setPhase] = useState<Phase>('idle');
  const [result, setResult] = useState<FreeGPUResult | null>(null);
  const [error, setError] = useState<string | null>(null);

  async function run(includePinned: boolean) {
    setPhase('working');
    setError(null);
    try {
      const res = await api.freeGPU(includePinned);
      setResult(res);
      if (res.stopped > 0) props.onFreed?.();
    } catch (e) {
      setError(e instanceof ApiError ? e.reason : String(e));
    } finally {
      setPhase('idle');
    }
  }

  return (
    <div className={styles.wrap}>
      <div className={styles.row}>
        <div className={styles.copy}>
          <div className={styles.title}>Free the GPU</div>
          <div className={styles.hint}>
            Stops every running model. Pinned instances are kept.
          </div>
        </div>
        {phase === 'confirm' ? (
          <div className={styles.confirmRow}>
            <button
              type="button"
              className={styles.dangerBtn}
              onClick={() => void run(false)}
            >
              Stop all
            </button>
            <button
              type="button"
              className={styles.quietBtn}
              onClick={() => setPhase('idle')}
            >
              Cancel
            </button>
          </div>
        ) : (
          <button
            type="button"
            className={styles.freeBtn}
            disabled={phase === 'working'}
            onClick={() => setPhase('confirm')}
          >
            {phase === 'working' ? 'Stopping…' : 'Free GPU'}
          </button>
        )}
      </div>

      {phase === 'confirm' && (
        <Reason>
          This drains and stops every instance LocalHub is running, then re-reads free VRAM to
          report what actually came back. In-flight requests are drained first, and pinned
          instances are left alone.
        </Reason>
      )}

      {error && <div className={styles.error}>Free GPU failed: {error}</div>}

      {result && <FreeGpuOutcome result={result} phase={phase} onIncludePinned={() => void run(true)} />}
    </div>
  );
}

/** The outcome of a sweep: the MEASURED delta, the per-instance list, and the pinned retry. */
function FreeGpuOutcome({
  result,
  phase,
  onIncludePinned,
}: {
  result: FreeGPUResult;
  phase: Phase;
  onIncludePinned: () => void;
}) {
  // reclaimed_mib is a before/after NVML delta and CAN be negative (another process grabbed VRAM
  // mid-sweep). Report that honestly instead of clamping it to a comfortable 0.
  const reclaimed = result.reclaimed_mib;
  const tone =
    reclaimed > 0 ? styles.ok : reclaimed < 0 ? styles.warnText : styles.neutralText;

  return (
    <div className={styles.outcome}>
      <div className={styles.outcomeHead}>
        <span className={styles.counts}>
          stopped <b>{result.stopped}</b>
          {result.skipped > 0 && (
            <>
              {' · '}pinned kept <b>{result.skipped}</b>
            </>
          )}
          {result.failed > 0 && (
            <>
              {' · '}failed <b>{result.failed}</b>
            </>
          )}
        </span>
        <span className={`${styles.reclaimed} ${tone}`}>
          {reclaimed >= 0 ? '+' : '−'}
          {formatMib(Math.abs(reclaimed))} measured
        </span>
      </div>

      <div className={styles.deltaLine}>
        free VRAM {formatMib(result.vram_free_before_mib)} →{' '}
        {formatMib(result.vram_free_after_mib)} (NVML re-read, not an API claim)
      </div>

      <Reason>{result.reason}</Reason>

      {result.results && result.results.length > 0 && (
        <ul className={styles.list}>
          {result.results.map((r) => (
            <li key={r.id} className={styles.item}>
              <span className={r.stopped ? styles.stopped : styles.kept}>
                {r.stopped ? 'stopped' : 'kept'}
              </span>
              <span className={styles.model}>{r.model_id}</span>
              <span className={styles.itemReason} title={r.reason}>
                {r.reason}
              </span>
            </li>
          ))}
        </ul>
      )}

      {/* A pin is an explicit "keep this resident" instruction, so overriding it is a SECOND
          deliberate action, never a fallback the first click quietly performed. */}
      {result.skipped > 0 && (
        <div className={styles.pinnedRetry}>
          <span className={styles.hint}>
            {result.skipped} pinned instance{result.skipped === 1 ? ' was' : 's were'} kept.
          </span>
          <button
            type="button"
            className={styles.dangerBtn}
            disabled={phase === 'working'}
            onClick={onIncludePinned}
          >
            Stop pinned too
          </button>
        </div>
      )}
    </div>
  );
}
