import { type FormEvent, useEffect, useState } from 'react';
import { api, dashboardToken } from './api/client';
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
import { StartupPanel } from './components/StartupPanel';
import styles from './App.module.css';

export default function App() {
  const [state, setState] = useState<'checking' | 'login' | 'ready'>('checking');
  const [reason, setReason] = useState<string | null>(null);

  useEffect(() => {
    void (async () => {
      try {
        const status = await api.dashboardAuthStatus();
        if (!status.auth_required) return setState('ready');
        const token = dashboardToken();
        if (!token) return setState('login');
        await api.dashboardLogin(token);
        setState('ready');
      } catch (e) {
        setReason(e instanceof Error ? e.message : String(e));
        setState('login');
      }
    })();
  }, []);

  if (state !== 'ready') {
    return <DashboardLogin loading={state === 'checking'} reason={reason} onReady={() => setState('ready')} />;
  }
  return <Dashboard />;
}

function DashboardLogin({ loading, reason, onReady }: { loading: boolean; reason: string | null; onReady: () => void }) {
  const [token, setToken] = useState('');
  const [error, setError] = useState(reason);
  const [submitting, setSubmitting] = useState(false);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    try {
      await api.dashboardLogin(token);
      onReady();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  };
  return <main className={styles.authShell}><form className={styles.authCard} onSubmit={submit}>
    <h1>LocalHub</h1><p>{loading ? 'Checking dashboard access…' : 'Enter the server admin token to open this dashboard.'}</p>
    {!loading && <><label htmlFor="admin-token">Admin token</label><input id="admin-token" type="password" autoComplete="current-password" value={token} onChange={(e) => setToken(e.target.value)} required autoFocus />
    {error && <div className={styles.connBannerCrit} role="alert">{error}</div>}<button type="submit" disabled={submitting}>{submitting ? 'Connecting…' : 'Open dashboard'}</button></>}
  </form></main>;
}

function Dashboard() {
  // Live hardware over SSE (WS-capable, see live/connection.ts). Phase 1 = SSE.
  const { hardware, status, invalidations } = useLive();

  // Engines + inventory are fetched (no live push in Phase 1). They refetch when
  // the live channel signals engines.updated / models.updated (future), and the
  // inventory refetches after a manual rescan.
  const engines = useResource(() => api.engines(), [invalidations.engines]);
  const models = useResource(() => api.models(), [invalidations.models]);

  // Shared GPU/CPU placement toggle for starts made from either the Instances picker or an
  // Inventory Run. Lifted here (matching how models/invalidations flow down as props) so both
  // sibling panels share one source of truth. Default OFF = full GPU, never offload to CPU.
  const [allowCpuOffload, setAllowCpuOffload] = useState(false);

  // Cached fit verdicts are only true for the free VRAM they were computed against. A Free-GPU
  // sweep changes that materially, so bumping this epoch is what invalidates them — and it is the
  // ONLY invalidation lever, so an ordinary re-render can never turn into a refetch storm.
  const [compatEpoch, setCompatEpoch] = useState(0);

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

        {/* Desktop: two balanced columns (left = tall GPU + instances; right =
            the shorter engines/compat/downloads/queue stack), inventory full
            width below. Mobile: the column wrappers collapse (display:contents)
            and each panel is CSS-ordered to the spec §mobile sequence
            (GPU → instances → queue → the rest). */}
        <div className={styles.left}>
          <div className={styles.pGpu}>
            <GpuPanel
              hardware={hardware}
              live={status.state === 'connected'}
              onGpuFreed={() => {
                setCompatEpoch((e) => e + 1);
                models.refresh();
              }}
            />
          </div>
          <div className={styles.pInstances}>
            <InstancesCard
              models={models.data}
              instancesInvalidation={invalidations.instances}
              allowCpuOffload={allowCpuOffload}
              onAllowCpuOffloadChange={setAllowCpuOffload}
              compatEpoch={compatEpoch}
            />
          </div>
        </div>

        <div className={styles.right}>
          <div className={styles.pQueue}>
            <QueueCard queueInvalidation={invalidations.queue} />
          </div>
          <div className={styles.pEngines}>
            <EnginesPanel
              engines={engines.data}
              loading={engines.loading}
              error={engines.error}
            />
          </div>
          <div className={styles.pCompat}>
            <CompatCard />
          </div>
          <div className={styles.pDownloads}>
            <DownloadsCard />
          </div>
        </div>

        <div className={styles.wide}>
          <StartupPanel models={models.data} />
        </div>

        <div className={styles.wide}>
          <InventoryPanel
            models={models.data}
            loading={models.loading}
            error={models.error}
            onRescanned={() => models.refresh()}
            allowCpuOffload={allowCpuOffload}
            onAllowCpuOffloadChange={setAllowCpuOffload}
            onLaunchStateChanged={() => models.refresh()}
            compatEpoch={compatEpoch}
          />
        </div>
      </main>

      <footer className={styles.footer}>
        LocalHub Compass · one-box model control plane · binds 127.0.0.1
      </footer>
    </div>
  );
}
