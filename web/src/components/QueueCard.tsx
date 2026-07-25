import { useCallback, useEffect, useState } from 'react';
import type { QueueEntry } from '../api/types';
import { api, ApiError } from '../api/client';
import {
  Panel,
  Badge,
  ClampText,
  ErrorState,
  Skeleton,
  type Tone,
} from './ui';
import styles from './QueueCard.module.css';

// QueueCard is the Phase-5 residency queue surface (P5.1). It shows admission-queue entries with
// their priority class, position, footprint, age, and the mandatory numeric blocked_reason, plus a
// cancel button for WAITING entries. The queue only has entries when a start was blocked on VRAM
// (POST /api/instances → 202 queued); most of the time it is empty.

const CLASS_LABEL: Record<number, string> = {
  0: 'interactive',
  1: 'normal',
  2: 'background',
};

const STATUS_TONE: Record<string, Tone> = {
  WAITING: 'warn',
  ADMITTED: 'ok',
  BLOCKED: 'crit',
  DONE: 'neutral',
  REJECTED: 'crit',
};

export function QueueCard(props: { queueInvalidation?: number }) {
  const [entries, setEntries] = useState<QueueEntry[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [showHistory, setShowHistory] = useState(false);

  const refresh = useCallback(async () => {
    try {
      const list = await api.queue();
      setEntries(Array.isArray(list) ? list : []);
      setError(null);
    } catch (e) {
      setError(e instanceof ApiError ? e.reason : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh, props.queueInvalidation]);

  // Poll while there are entries (the queue drains as VRAM frees); slow idle poll otherwise.
  useEffect(() => {
    const hasEntries = (entries?.length ?? 0) > 0;
    const id = setInterval(() => void refresh(), hasEntries ? 3000 : 10000);
    return () => clearInterval(id);
  }, [refresh, entries]);

  const onCancel = useCallback(
    async (id: string) => {
      setBusyId(id);
      try {
        await api.cancelQueueEntry(id);
        void refresh();
      } catch (e) {
        setError(e instanceof ApiError ? e.reason : String(e));
      } finally {
        setBusyId(null);
      }
    },
    [refresh],
  );

  const waiting = (entries ?? []).filter((e) => e.status === 'WAITING');
  const recent = (entries ?? []).filter((e) => e.status !== 'WAITING').slice(0, 5);

  return (
    <Panel title="Queue">
      {error && <ErrorState>{error}</ErrorState>}
      {loading && !entries ? (
        <Skeleton>Loading queue…</Skeleton>
      ) : waiting.length === 0 ? (
        // No live waiting entries: collapse the panel to a single quiet line
        // rather than a tall empty box. Any terminal entries stay reachable
        // behind the History toggle below.
        <>
          <div className={styles.quiet}>Queue empty</div>
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
                recent.map((e) => (
                  <div key={e.id} className={styles.recentRow}>
                    <Badge tone={STATUS_TONE[e.status] ?? 'neutral'}>{e.status}</Badge>
                    <span className={styles.recentModel}>{e.model_id}</span>
                  </div>
                ))}
            </div>
          )}
        </>
      ) : (
        <>
          {waiting.map((e) => (
            <div key={e.id} className={styles.row}>
              <div className={styles.rowHead}>
                <Badge tone={STATUS_TONE[e.status] ?? 'neutral'} dot>
                  {e.status}
                </Badge>
                <span className={styles.model}>{e.model_id}</span>
                {typeof e.position === 'number' && e.position > 0 && (
                  <Badge tone="accent">#{e.position}</Badge>
                )}
                <Badge tone="neutral">
                  {CLASS_LABEL[e.priority_class] ?? `class ${e.priority_class}`}
                </Badge>
                <button
                  type="button"
                  className={styles.cancelBtn}
                  disabled={busyId === e.id}
                  onClick={() => void onCancel(e.id)}
                >
                  {busyId === e.id ? 'Cancelling…' : 'Cancel'}
                </button>
              </div>
              <div className={styles.meta}>
                {typeof e.footprint_mib === 'number' && e.footprint_mib > 0 && (
                  <span>needs {e.footprint_mib} MiB</span>
                )}
                {typeof e.age_seconds === 'number' && (
                  <span>waiting {e.age_seconds}s</span>
                )}
              </div>
              {e.blocked_reason && (
                <div className={styles.reason}>
                  <ClampText lines={2}>{e.blocked_reason}</ClampText>
                </div>
              )}
            </div>
          ))}

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
                recent.map((e) => (
                  <div key={e.id} className={styles.recentRow}>
                    <Badge tone={STATUS_TONE[e.status] ?? 'neutral'}>{e.status}</Badge>
                    <span className={styles.recentModel}>{e.model_id}</span>
                  </div>
                ))}
            </div>
          )}
        </>
      )}
    </Panel>
  );
}
