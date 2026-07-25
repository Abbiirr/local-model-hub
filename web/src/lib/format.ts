// Pure formatting/math helpers. Unit-tested (format.test.ts) — the VRAM bar math
// and MiB→GiB display are correctness-relevant (a wrong "free" reading is what the
// whole product exists to get right).

/** MiB → GiB, rounded to `digits` decimals (default 1). */
export function mibToGib(mib: number, digits = 1): number {
  if (!Number.isFinite(mib)) return 0;
  return round(mib / 1024, digits);
}

/** "12.3 GiB" from a MiB value; "N/A" for non-finite/negative. */
export function formatMib(mib: number | null | undefined, digits = 1): string {
  if (mib == null || !Number.isFinite(mib) || mib < 0) return 'N/A';
  return `${mibToGib(mib, digits).toFixed(digits)} GiB`;
}

/** Human size from raw bytes (models.size_bytes). Binary units. */
export function formatBytes(bytes: number | null | undefined): string {
  if (bytes == null || !Number.isFinite(bytes) || bytes < 0) return 'N/A';
  if (bytes === 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  const i = Math.min(
    units.length - 1,
    Math.floor(Math.log(bytes) / Math.log(1024)),
  );
  const val = bytes / Math.pow(1024, i);
  const digits = i === 0 ? 0 : val >= 100 ? 0 : val >= 10 ? 1 : 2;
  return `${val.toFixed(digits)} ${units[i]}`;
}

/**
 * VRAM bar model. Given total/free/used MiB, returns the used fraction (0..1) and
 * a display-ready percentage. `used` is preferred when present; otherwise derived
 * from total-free. Clamped to [0,1] so a stale/over-report never overflows the bar.
 */
export interface VramBar {
  usedMib: number;
  freeMib: number;
  totalMib: number;
  /** used / total, clamped to [0,1]. */
  usedFraction: number;
  /** integer 0..100 for display + aria. */
  usedPct: number;
  /** severity tier drives the bar color. */
  level: 'ok' | 'warn' | 'crit';
}

export function computeVramBar(
  totalMib: number,
  freeMib: number,
  usedMib?: number | null,
): VramBar {
  const total = Number.isFinite(totalMib) && totalMib > 0 ? totalMib : 0;
  const free = Number.isFinite(freeMib) && freeMib >= 0 ? freeMib : 0;
  let used: number;
  if (usedMib != null && Number.isFinite(usedMib) && usedMib >= 0) {
    used = usedMib;
  } else {
    used = Math.max(0, total - free);
  }
  const fraction = total > 0 ? clamp(used / total, 0, 1) : 0;
  const pct = Math.round(fraction * 100);
  const level: VramBar['level'] =
    fraction >= 0.9 ? 'crit' : fraction >= 0.7 ? 'warn' : 'ok';
  return {
    usedMib: used,
    freeMib: free,
    totalMib: total,
    usedFraction: fraction,
    usedPct: pct,
    level,
  };
}

/** Percentage for util/temp/power gauges; "N/A" when the probe reports -1. */
export function formatMetric(
  value: number | null | undefined,
  unit: string,
  digits = 0,
): string {
  if (value == null || !Number.isFinite(value) || value < 0) return 'N/A';
  return `${value.toFixed(digits)}${unit}`;
}

/** "3s ago" / "just now" from an RFC3339 timestamp; empty string if unparseable. */
export function relativeTime(iso: string | null | undefined, now = Date.now()): string {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const secs = Math.max(0, Math.round((now - t) / 1000));
  if (secs < 2) return 'just now';
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.floor(mins / 60);
  return `${hrs}h ago`;
}

function round(n: number, digits: number): number {
  const f = Math.pow(10, digits);
  return Math.round(n * f) / f;
}

function clamp(n: number, lo: number, hi: number): number {
  return Math.min(hi, Math.max(lo, n));
}
