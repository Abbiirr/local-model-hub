// Small fetch hook for read-only resources (engines, models). Handles loading,
// error (with .reason surfaced), manual refresh, and refetch-on-signal.

import { useCallback, useEffect, useRef, useState } from 'react';
import { ApiError } from './client';

export interface ResourceState<T> {
  data: T | null;
  loading: boolean;
  /** null when no error; otherwise a human-readable reason. */
  error: string | null;
  /** stale-while-refresh flag: data present but a background refetch is running. */
  refreshing: boolean;
  /** last successful fetch time (ms epoch), for staleness display. */
  updatedAt: number | null;
  refresh: () => void;
}

export function useResource<T>(
  fetcher: () => Promise<T>,
  deps: readonly unknown[] = [],
): ResourceState<T> {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [updatedAt, setUpdatedAt] = useState<number | null>(null);
  const mounted = useRef(true);
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const run = useCallback(async () => {
    const hadData = data != null;
    if (hadData) setRefreshing(true);
    else setLoading(true);
    try {
      const result = await fetcherRef.current();
      if (!mounted.current) return;
      setData(result);
      setError(null);
      setUpdatedAt(Date.now());
    } catch (e) {
      if (!mounted.current) return;
      const reason =
        e instanceof ApiError ? e.reason : e instanceof Error ? e.message : String(e);
      setError(reason);
    } finally {
      if (mounted.current) {
        setLoading(false);
        setRefreshing(false);
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [data == null]);

  useEffect(() => {
    void run();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  return { data, loading, error, refreshing, updatedAt, refresh: () => void run() };
}
