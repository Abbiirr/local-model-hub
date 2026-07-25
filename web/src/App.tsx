import { useEffect } from 'react';
import { api } from './api/client';
import { useResource } from './api/useResource';
import { useLive } from './live/useLive';
import { Header } from './components/Header';
import { GpuPanel } from './components/GpuPanel';
import { EnginesPanel } from './components/EnginesPanel';
import { InventoryPanel } from './components/InventoryPanel';
import { CompatCard } from './components/CompatCard';
import { DownloadsCard } from './components/DownloadsCard';
import { InstancesCard } from './components/InstancesCard';
import { QueueCard } from './components/QueueCard';
import styles from './App.module.css';

export default function App() {
  // Live hardware over SSE (WS-capable, see live/connection.ts). Phase 1 = SSE.
  const { hardware, status, invalidations } = useLive();

  // Engines + inventory are fetched (no live push in Phase 1). They refetch when
  // the live channel signals engines.updated / models.updated (future), and the
  // inventory refetches after a manual rescan.
  const engines = useResource(() => api.engines(), [invalidations.engines]);
  const models = useResource(() => api.models(), [invalidations.models]);

  // A one-shot poll fallback for engines so a card appears even if the live
  // channel never signals (Phase 1: it won't). Cheap, 15s cadence.
  useEffect(() => {
    const id = setInterval(() => engines.refresh(), 15000);
    return () => clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const disconnected =
    status.state === 'reconnecting' || status.state === 'closed';

  return (
    <div className={styles.shell}>
      <Header status={status} lastUpdate={hardware?.captured_at ?? null} />

      <main className={styles.main}>
        {disconnected && (
          <div
            className={`${styles.connBanner} ${
              status.state === 'closed' ? styles.connBannerCrit : ''
            }`}
            role="status"
          >
            Live connection {status.state}: {status.reason}
          </div>
        )}

        {/* Mobile order (single column): GPU → Engines → Inventory.
            Desktop: GPU+Engines left column, Inventory right. */}
        <div className={styles.left}>
          <GpuPanel hardware={hardware} live={status.state === 'connected'} />
          <EnginesPanel
            engines={engines.data}
            loading={engines.loading}
            error={engines.error}
          />
          <CompatCard />
          <DownloadsCard />
        </div>

        <div className={styles.right}>
          <InstancesCard
            models={models.data}
            instancesInvalidation={invalidations.instances}
          />
          <QueueCard queueInvalidation={invalidations.instances} />
          <InventoryPanel
            models={models.data}
            loading={models.loading}
            error={models.error}
            onRescanned={() => models.refresh()}
          />
        </div>
      </main>

      <footer className={styles.footer}>
        LocalHub Compass · read-only observability (Phase 1) · binds 127.0.0.1
      </footer>
    </div>
  );
}
