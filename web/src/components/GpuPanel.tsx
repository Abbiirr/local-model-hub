import type { GPU, HardwareSnapshot } from '../api/types';
import { computeVramBar, formatMib, formatNa, ramFloorState } from '../lib/format';
import { Bar, EmptyState, Panel, Reason, Skeleton } from './ui';
import { GpuDetails, GpuProcesses } from './GpuDetails';
import { FreeGpuButton } from './FreeGpuButton';
import styles from './GpuPanel.module.css';

// The GPU panel answers two questions, in this order:
//   1. "How much of the card can a start actually have?" — the ADMISSION budget, not raw free
//      VRAM. They differ by the 0.92 safety factor, and quoting only the raw figure is what makes
//      a refusal ("needs 13290 MiB, you have 14784 free") look like a bug.
//   2. "Why is it slow?" — clocks, PCIe link, power limit and throttle state (GpuDetails).
// Everything below the hero VRAM reading is detail; the file stays small by delegating.

export function GpuPanel(props: {
  hardware: HardwareSnapshot | null;
  live: boolean;
  /** Fired after a Free-GPU sweep actually stopped something, so the caller can invalidate
   *  anything derived from free VRAM (cached compat verdicts, the instances list). */
  onGpuFreed?: () => void;
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
          {hw.gpus.map((g) => (
            <GpuCard key={g.index} gpu={g} />
          ))}

          <GpuProcesses processes={hw.gpu_processes ?? []} />

          <FreeGpuButton onFreed={props.onGpuFreed} />

          <SystemSummary hw={hw} />
        </div>
      )}
    </Panel>
  );
}

function GpuCard({ gpu }: { gpu: GPU }) {
  const bar = computeVramBar(gpu.vram_total, gpu.vram_free, gpu.vram_used);
  return (
    <div className={styles.gpuCard}>
      <div className={styles.gpuName}>
        <h3>{gpu.name}</h3>
        <span className={styles.gpuMeta}>
          driver {gpu.driver || '—'} · CC {gpu.cc || '—'}
          {gpu.cuda ? ` · CUDA ${gpu.cuda}` : ''}
        </span>
      </div>

      <div className={styles.vramRow}>
        <span className={styles.vramNums}>
          <span className={styles.vramUsed}>{formatMib(bar.usedMib)}</span> /{' '}
          {formatMib(bar.totalMib)} used
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

      <AdmissionBudget gpu={gpu} />

      <div className={styles.stats}>
        <Stat label="Util" value={formatNa(gpu.util, '%')} />
        <Stat label="Temp" value={formatNa(gpu.temp, '°C')} />
        <Stat label="Power" value={formatNa(gpu.power, ' W')} />
      </div>

      <GpuDetails gpu={gpu} />
    </div>
  );
}

/**
 * The number ADMISSION compares a model against, shown beside raw free VRAM.
 *
 * This is the single most load-bearing addition to the panel. Raw free VRAM makes models look
 * like they fit when admission disagrees; the daemon reserves a 0.92 safety factor, so the honest
 * budget is always smaller. Printing both, with the gap named, is what makes a refusal reason and
 * the dashboard agree.
 */
function AdmissionBudget({ gpu }: { gpu: GPU }) {
  const usable = gpu.usable_vram_mib;
  const known = usable != null && Number.isFinite(usable) && usable >= 0;
  const held = known ? gpu.vram_free - usable : 0;
  return (
    <div className={styles.budget}>
      <div className={styles.budgetMain}>
        <span className={styles.budgetLabel}>Usable for a start</span>
        <span className={styles.budgetValue}>{formatMib(usable)}</span>
      </div>
      <div className={styles.budgetNote}>
        {known ? (
          <>
            admission budget — {formatMib(gpu.vram_free)} free minus a{' '}
            {formatMib(held)} safety reserve (0.92). A start is checked against this
            number, not against free VRAM.
          </>
        ) : (
          <>
            the daemon did not report an admission budget for this GPU, so the figure a start
            is checked against is unknown here — treat free VRAM as an upper bound only.
          </>
        )}
      </div>
    </div>
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
  // On a 16 GB box RAM binds BEFORE VRAM: residency refuses a start while free RAM is under the
  // floor, even with most of the card idle. Showing the floor next to free RAM — and marking the
  // breach — is what stops that refusal from looking arbitrary.
  const floor = ramFloorState(ram.free, ram.floor_mib);
  return (
    <div>
      <div className={styles.sysRow}>
        <div className={styles.sysItem}>
          CPU{' '}
          <b>
            {cpu.cores}c/{cpu.threads}t
          </b>
          <br />
          <span className={styles.sysFaint}>{cpu.model}</span>
        </div>
        <div className={styles.sysItem}>
          RAM <b>{formatMib(ram.used)}</b> / {formatMib(ram.total)}
          <br />
          <span className={floor.belowFloor ? styles.sysCrit : styles.sysFaint}>
            {formatMib(ram.free)} free
            {floor.hasFloor && <> · floor {formatMib(ram.floor_mib)}</>}
          </span>
        </div>
      </div>
      {floor.belowFloor && (
        <div className={styles.ramWarn}>
          Free RAM is below the residency floor — starts are refused until{' '}
          {formatMib(floor.shortMib)} more is free, however much VRAM is idle.
        </div>
      )}
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
