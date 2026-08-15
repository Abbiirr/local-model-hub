import { useState } from 'react';
import type { GPU, GPUProcess } from '../api/types';
import { NA, formatCurMax, formatNa } from '../lib/format';
import { Badge } from './ui';
import styles from './GpuPanel.module.css';

// Extended GPU telemetry (PLAN-ui-gpu-rework.md §3.1), split out of GpuPanel so that file stays
// about the hero VRAM reading.
//
// The whole point of these rows is that VRAM numbers explain FULL but never explain SLOW. A card
// pinned at PCIe gen 1 ×8, or throttled by sw_power_cap, loads and runs models slowly while every
// VRAM figure on screen looks perfectly healthy.
//
// SENTINEL DISCIPLINE: every numeric field here uses -1 for "nvidia-smi did not report this", and
// `formatNa` is the only thing allowed to interpret it. Rendering -1 as 0 would be undetectably
// wrong — 0 % fan and ~0 MHz clocks are LEGITIMATE readings on an idle card.

/** A throttle string decoded for display. Empty means unthrottled; `idle` is benign clock-down,
 *  not a problem — anything else is actively limiting performance and reads as a warning. */
function throttleView(throttle: string | undefined): {
  label: string;
  tone: 'ok' | 'warn' | 'neutral';
  hint: string;
} {
  const t = (throttle ?? '').trim();
  if (t === '') {
    return { label: 'none', tone: 'ok', hint: 'No throttle reasons active — clocks are unrestricted.' };
  }
  const reasons = t.split(',').map((r) => r.trim()).filter(Boolean);
  const onlyIdle = reasons.length === 1 && reasons[0] === 'idle';
  if (onlyIdle) {
    return {
      label: 'idle',
      tone: 'neutral',
      hint: 'Clocks are down because the GPU is idle. This is normal power management, not a limit.',
    };
  }
  return {
    label: reasons.join(', '),
    tone: 'warn',
    hint: `nvidia-smi reports active throttle reasons: ${reasons.join(', ')}. These cap clocks and explain slowness that VRAM numbers do not.`,
  };
}

/** One label/value detail row. `hint` becomes the row title so the "why does this matter" text is
 *  available without spending vertical space on it. */
function Detail(props: { label: string; value: string; hint?: string; muted?: boolean }) {
  return (
    <div className={styles.detailRow} title={props.hint}>
      <span className={styles.detailLabel}>{props.label}</span>
      <span
        className={`${styles.detailValue} ${props.muted ? styles.detailMuted : ''}`}
      >
        {props.value}
      </span>
    </div>
  );
}

export function GpuDetails({ gpu }: { gpu: GPU }) {
  const throttle = throttleView(gpu.throttle);
  const pcie = `gen ${formatCurMax(gpu.pcie_gen, gpu.pcie_gen_max)} · ×${formatCurMax(
    gpu.pcie_width,
    gpu.pcie_width_max,
  )}`;

  return (
    <div className={styles.details}>
      <Detail
        label="SM clock"
        value={formatCurMax(gpu.clock_sm_mhz, gpu.clock_sm_max_mhz, ' MHz')}
        hint="Current / maximum graphics clock. A current clock far below max with no throttle reason usually just means idle."
        muted={formatNa(gpu.clock_sm_mhz) === NA}
      />
      <Detail
        label="Mem clock"
        value={formatNa(gpu.clock_mem_mhz, ' MHz')}
        hint="Current memory clock."
        muted={formatNa(gpu.clock_mem_mhz) === NA}
      />
      <Detail
        label="Fan"
        value={formatNa(gpu.fan_pct, '%')}
        hint="Fan duty cycle. 0% is a real reading on a cool card with zero-RPM fans; 'n/a' means nvidia-smi reported nothing."
        muted={formatNa(gpu.fan_pct) === NA}
      />
      <Detail
        label="PCIe link"
        value={pcie}
        hint="Negotiated link generation and width, with the maximum this board supports. The generation DOWNTRAINS at idle (gen 1 is normal when nothing is transferring), which is why both numbers are shown. A permanently reduced link halves host→VRAM bandwidth and explains slow model loads."
      />
      <Detail
        label="Power"
        value={`${formatNa(gpu.power, ' W')} / ${formatNa(gpu.power_limit_w, ' W')}`}
        hint={`Current draw against the enforced power limit. Board maximum: ${formatNa(
          gpu.power_max_limit_w,
          ' W',
        )}.`}
      />
      <div className={styles.detailRow} title={throttle.hint}>
        <span className={styles.detailLabel}>Throttle</span>
        <span className={styles.detailValue}>
          <Badge tone={throttle.tone}>{throttle.label}</Badge>
        </span>
      </div>
    </div>
  );
}

/**
 * The processes holding a CUDA context.
 *
 * `used_mib` is ALWAYS -1 on Windows: WDDM does not attribute VRAM per process, so nvidia-smi
 * returns [N/A] on every row. We render the LIST (knowing which processes hold the card is
 * actionable on its own) with usage as "n/a" — showing 0, or omitting the column and letting the
 * reader assume, would both be claims we cannot back up.
 *
 * Collapsed by default: on a desktop box this is a dozen-plus browser/shell processes, and burying
 * the VRAM hero reading under that list would trade one usability problem for another.
 */
export function GpuProcesses({ processes }: { processes: GPUProcess[] }) {
  const [open, setOpen] = useState(false);
  if (processes.length === 0) return null;

  const anyUsage = processes.some((p) => p.used_mib >= 0);

  return (
    <div className={styles.procs}>
      <button
        type="button"
        className={styles.procToggle}
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
      >
        {open ? '▾' : '▸'} GPU holders ({processes.length})
      </button>
      {open && (
        <>
          {!anyUsage && (
            <div className={styles.procNote}>
              Per-process VRAM is not available on Windows — the WDDM driver does not attribute
              memory per process, so nvidia-smi reports it as n/a for every process. The list is
              still who holds the card.
            </div>
          )}
          <ul className={styles.procList}>
            {processes.map((p, i) => (
              <li key={`${p.pid}-${i}`} className={styles.procItem}>
                <span className={styles.procPid}>{p.pid}</span>
                {/* An empty name means Windows denied access to the image path. Showing the pid
                    alone is honest; inventing a placeholder name would not be. */}
                <span className={styles.procName} title={p.name || undefined}>
                  {p.name ? basename(p.name) : <em className={styles.procUnknown}>name withheld by Windows</em>}
                </span>
                <span className={styles.procUsed}>{formatNa(p.used_mib, ' MiB')}</span>
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}

/** Last path segment of a Windows or POSIX path; the full path stays in the row title. */
function basename(p: string): string {
  const i = Math.max(p.lastIndexOf('\\'), p.lastIndexOf('/'));
  return i >= 0 ? p.slice(i + 1) : p;
}
