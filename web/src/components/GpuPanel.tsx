import type { HardwareSnapshot } from '../api/types';
import { computeVramBar, formatMetric, formatMib } from '../lib/format';
import { Bar, EmptyState, Panel, Reason, Skeleton } from './ui';
import styles from './GpuPanel.module.css';

export function GpuPanel(props: {
  hardware: HardwareSnapshot | null;
  live: boolean;
}) {
  const hw = props.hardware;
  return (
    <Panel title="GPU">
      {!hw ? (
        <Skeleton>Waiting for first hardware frame…</Skeleton>
      ) : hw.gpus.length === 0 ? (
        <EmptyState>
          No NVIDIA GPU detected.
          <br />
          <Reason>
            The hardware probe found no GPU via nvidia-smi. CPU/RAM/disk below are
            still reported.
          </Reason>
        </EmptyState>
      ) : (
        <div className={styles.grid}>
          {hw.gpus.map((g) => {
            const bar = computeVramBar(g.vram_total, g.vram_free, g.vram_used);
            return (
              <div key={g.index} className={styles.gpuCard}>
                <div className={styles.gpuName}>
                  <h3>{g.name}</h3>
                  <span className={styles.gpuMeta}>
                    driver {g.driver || '—'} · CC {g.cc || '—'}
                    {g.cuda ? ` · CUDA ${g.cuda}` : ''}
                  </span>
                </div>

                <div className={styles.vramRow}>
                  <span className={styles.vramNums}>
                    <span className={styles.vramUsed}>
                      {formatMib(bar.usedMib)}
                    </span>{' '}
                    / {formatMib(bar.totalMib)} used
                  </span>
                  <span className={styles.vramNums}>
                    {formatMib(bar.freeMib)} free · {bar.usedPct}%
                  </span>
                </div>
                <Bar
                  fraction={bar.usedFraction}
                  level={bar.level}
                  label={`VRAM ${bar.usedPct}% used`}
                />

                <div className={styles.stats}>
                  <Stat label="Util" value={formatMetric(g.util, '%')} />
                  <Stat label="Temp" value={formatMetric(g.temp, '°C')} />
                  <Stat label="Power" value={formatMetric(g.power, ' W')} />
                </div>
              </div>
            );
          })}

          <SystemSummary hw={hw} />
        </div>
      )}
    </Panel>
  );
}

function Stat(props: { label: string; value: string }) {
  return (
    <div className={styles.stat}>
      <div className={styles.statVal}>{props.value}</div>
      <div className={styles.statLabel}>{props.label}</div>
    </div>
  );
}

function SystemSummary(props: { hw: HardwareSnapshot }) {
  const { cpu, ram, disk } = props.hw;
  return (
    <div>
      <div className={styles.sysRow}>
        <div className={styles.sysItem}>
          CPU{' '}
          <b>
            {cpu.cores}c/{cpu.threads}t
          </b>
          <br />
          <span style={{ color: 'var(--text-faint)' }}>{cpu.model}</span>
        </div>
        <div className={styles.sysItem}>
          RAM <b>{formatMib(ram.used)}</b> / {formatMib(ram.total)}
          <br />
          <span style={{ color: 'var(--text-faint)' }}>
            {formatMib(ram.free)} free
          </span>
        </div>
      </div>
      {disk.length > 0 && (
        <div className={styles.diskList}>
          {disk.map((d) => (
            <div key={d.path} className={styles.sysItem}>
              Disk <span className="mono">{d.path}</span>{' '}
              <b>{d.free_gib.toFixed(1)} GiB</b> free of{' '}
              {d.total_gib.toFixed(0)} GiB
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
