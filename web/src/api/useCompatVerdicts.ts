// Inline compatibility verdicts (PLAN-ui-gpu-rework.md §3.3).
//
// WHY THIS EXISTS: before it, every model row rendered an `unchecked` badge, because a verdict was
// only ever computed when the operator clicked "Preview fit" on one row at a time. That turned the
// list into a filename inventory instead of an answer to "what can I start right now?" — the single
// question the dashboard exists to answer.
//
// The endpoint (GET /api/models/{id}/compat) does GGUF header reads, and for a REMOTE model those
// are HTTP range-reads. Local models are fast, but "fast" is not "free", so this module:
//   • caches per model id at MODULE scope, so the Instances picker and the Inventory table share
//     one fetch instead of racing each other for the same verdict;
//   • de-duplicates concurrent requests for the same id via an in-flight map;
//   • bounds concurrency (FETCH_CONCURRENCY) so mounting a 20-model list does not open 20 sockets;
//   • never refetches on re-render — only when a NEW id appears, or when the caller bumps `epoch`
//     because free VRAM changed materially (a Free-GPU sweep), which is exactly when a cached
//     verdict stops being true.

import { useEffect, useMemo, useState } from 'react';
import { api, ApiError } from './client';
import type { CompatStatus, Verdict } from './types';

/** A cached verdict, reduced to what a row actually renders. */
export interface CompatEntry {
  /** `loading` while in flight, `error` when the fetch itself failed (status stays `unknown` —
   *  an unreachable endpoint is NOT evidence that a model won't run). */
  state: 'loading' | 'ready' | 'error';
  status: CompatStatus;
  /** From the verdict; absent when the estimator could not price the model. */
  estimatedVramMb?: number;
  /** The daemon's mandatory human-readable why. Shown as a tooltip on the badge. */
  reason: string;
}

const LOADING: CompatEntry = { state: 'loading', status: 'unknown', reason: 'checking fit…' };

/** How many verdicts to fetch at once. Small on purpose: this is a courtesy prefetch behind the
 *  operator's real work, not the operator's real work. */
const FETCH_CONCURRENCY = 3;

/** Separator for the effect's dependency key. A newline cannot occur in a model id (ids are
 *  slugified), so joining on it is lossless. */
const KEY_SEP = '\n';

const cache = new Map<string, CompatEntry>();
const inflight = new Map<string, Promise<CompatEntry>>();
let cacheEpoch = 0;

/**
 * Every mounted consumer of the cache.
 *
 * This exists because the cache is SHARED: when the Instances picker and the Inventory table both
 * want the same model, only one of them issues the fetch — the other sees the id already present
 * (as `loading`) and starts no work. Without a broadcast, that second component would never learn
 * the result and would render "checking…" forever. Notifying every subscriber on each settled
 * fetch is what makes one fetch serve both panels instead of stranding one of them.
 */
const listeners = new Set<() => void>();

function notify(): void {
  for (const l of listeners) l();
}

/** Drop every cached verdict. Exported for tests — production invalidation goes through `epoch`. */
export function resetCompatCache(): void {
  cache.clear();
  inflight.clear();
  cacheEpoch = 0;
}

const VALID_STATUS: ReadonlySet<string> = new Set([
  'runs',
  'runs_degraded',
  'wont_run',
  'unknown',
]);

/** Reduce a raw verdict to a row entry. Defensive: an older/odd payload degrades to `unknown`
 *  with a reason rather than throwing or, worse, claiming a status it did not send. */
function toEntry(v: Verdict | undefined): CompatEntry {
  if (!v || typeof v !== 'object') {
    return {
      state: 'error',
      status: 'unknown',
      reason: 'the daemon returned no verdict body for this model',
    };
  }
  const status = VALID_STATUS.has(v.status) ? v.status : 'unknown';
  return {
    state: 'ready',
    status,
    estimatedVramMb:
      typeof v.estimated_vram_mb === 'number' && v.estimated_vram_mb > 0
        ? v.estimated_vram_mb
        : undefined,
    reason: v.reason || 'the daemon returned a verdict with no reason',
  };
}

async function fetchOne(id: string): Promise<CompatEntry> {
  const existing = inflight.get(id);
  if (existing) return existing;
  const p = (async (): Promise<CompatEntry> => {
    try {
      return toEntry(await api.modelCompat(id));
    } catch (e) {
      // A transport/HTTP failure is NOT a verdict. Keep the status `unknown` so the row never
      // implies a model won't run just because the check could not be made.
      return {
        state: 'error',
        status: 'unknown',
        reason: e instanceof ApiError ? e.reason : String(e),
      };
    } finally {
      inflight.delete(id);
    }
  })();
  inflight.set(id, p);
  return p;
}

/** Run `ids` through a bounded worker pool, writing each result into the cache and broadcasting. */
async function fetchPool(ids: string[]): Promise<void> {
  let next = 0;
  const worker = async (): Promise<void> => {
    for (;;) {
      const i = next++;
      if (i >= ids.length) return;
      const id = ids[i];
      const entry = await fetchOne(id);
      cache.set(id, entry);
      notify();
    }
  };
  await Promise.all(
    Array.from({ length: Math.min(FETCH_CONCURRENCY, ids.length) }, worker),
  );
}

/**
 * Verdicts for `modelIds`, fetched once and cached.
 *
 * `epoch` is the ONLY invalidation lever: bump it when free VRAM changed materially (a Free-GPU
 * sweep, a stop) so stale "fits" verdicts are recomputed. Re-renders alone never refetch.
 */
export function useCompatVerdicts(
  modelIds: string[],
  epoch = 0,
): Record<string, CompatEntry> {
  const [version, setVersion] = useState(0);
  // Identity-stable key: a NEW array holding the SAME ids (which is what every parent re-render
  // produces) must not retrigger the effect. Depending on the array itself would refetch on every
  // render — exactly the hammering this module exists to prevent.
  const key = modelIds.join(KEY_SEP);

  // Subscribe for the lifetime of the component: a verdict this instance did not request may
  // still be one it renders (see `listeners`).
  useEffect(() => {
    const onChange = () => setVersion((v) => v + 1);
    listeners.add(onChange);
    return () => {
      listeners.delete(onChange);
    };
  }, []);

  useEffect(() => {
    if (epoch !== cacheEpoch) {
      cache.clear();
      cacheEpoch = epoch;
    }
    const wanted = key
      ? key.split(KEY_SEP).filter((id) => id && !cache.has(id))
      : [];
    if (wanted.length === 0) return;
    for (const id of wanted) cache.set(id, LOADING);
    // Results land in the shared cache and are broadcast from there, so an unmount mid-flight
    // wastes nothing and strands nobody.
    notify();
    void fetchPool(wanted);
  }, [key, epoch]);

  return useMemo(() => {
    const out: Record<string, CompatEntry> = {};
    for (const id of modelIds) {
      const hit = cache.get(id);
      if (hit) out[id] = hit;
    }
    return out;
    // `version` is the cache-mutation signal: the cache is a module-level Map, so React cannot
    // see writes to it. Bumping `version` when a fetch settles is what recomputes this map.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, version, epoch]);
}
