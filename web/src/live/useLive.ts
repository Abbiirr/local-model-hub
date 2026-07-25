// React binding for LiveConnection. Owns one connection for the app lifetime,
// exposes the latest hardware snapshot + connection status, and re-fetches
// engines/models when the server pushes models.updated / engines.updated (or, in
// Phase 1 where those pushes don't exist yet, callers just poll separately).

import { useEffect, useRef, useState } from 'react';
import type { HardwareSnapshot } from '../api/types';
import {
  LiveConnection,
  type ConnectionStatus,
  type LiveConfig,
  type LiveEvent,
} from './connection';

export interface UseLiveResult {
  hardware: HardwareSnapshot | null;
  status: ConnectionStatus;
  /** increments whenever the server signals models/engines/instances changed (for refetch). */
  invalidations: { models: number; engines: number; instances: number };
}

export function useLive(cfg?: Partial<LiveConfig>): UseLiveResult {
  const [hardware, setHardware] = useState<HardwareSnapshot | null>(null);
  const [status, setStatus] = useState<ConnectionStatus>(() => ({
    state: 'idle',
    transport: 'sse',
    lastEventAt: null,
    attempts: 0,
    reason: 'not started',
  }));
  const [invalidations, setInvalidations] = useState({
    models: 0,
    engines: 0,
    instances: 0,
  });

  // Stable config across renders; only the first value is used (connection is
  // created once). Callers pass a literal or memoized object.
  const cfgRef = useRef(cfg);

  useEffect(() => {
    const conn = new LiveConnection(cfgRef.current);
    const offStatus = conn.onStatus(setStatus);
    const offEvent = conn.onEvent((e: LiveEvent) => {
      switch (e.type) {
        case 'hardware.snapshot':
          setHardware(e.data);
          break;
        case 'models.updated':
          setInvalidations((v) => ({ ...v, models: v.models + 1 }));
          break;
        case 'engines.updated':
          setInvalidations((v) => ({ ...v, engines: v.engines + 1 }));
          break;
        case 'instances.updated':
          setInvalidations((v) => ({ ...v, instances: v.instances + 1 }));
          break;
        case 'heartbeat':
          break;
      }
    });
    conn.start();
    return () => {
      offStatus();
      offEvent();
      conn.stop();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return { hardware, status, invalidations };
}
