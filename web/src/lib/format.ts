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

// ── Extended GPU telemetry (-1 sentinel) ─────────────────────────────────────
// Every extended nvidia-smi field uses -1 for "the driver did not report this". Rendering that as
// 0 would be a lie the user cannot detect: an idle RTX 4060 Ti genuinely reports 0 % fan and can
// genuinely sit near 0 MHz, so "0" and "unknown" MUST look different. `formatNa` is the single
// place that decision is made, and it is why the -1 sentinel never reaches a React component.

/** The one string used for "the probe did not report this". Lowercase, so it reads as an absence
 *  rather than as a value. */
export const NA = 'n/a';

/**
 * Format a telemetry reading that uses the -1 sentinel. Returns {@link NA} for absent, non-finite
 * or NEGATIVE values, and the formatted number otherwise — so a legitimate 0 renders as "0", not
 * as "n/a".
 */
export function formatNa(
  value: number | null | undefined,
  unit = '',
  digits = 0,
): string {
  if (value == null || !Number.isFinite(value) || value < 0) return NA;
  return `${value.toFixed(digits)}${unit}`;
}

/**
 * Format a "current / max" pair (PCIe gen, PCIe width, SM clock). BOTH sides are always shown:
 * PCIe downtrains to gen 1 at idle, so a bare "gen 1" reads as broken hardware when it is normal
 * power management. An unknown side renders as {@link NA} independently of the other.
 */
export function formatCurMax(
  current: number | null | undefined,
  max: number | null | undefined,
  unit = '',
): string {
  const cur = formatNa(current, unit);
  const mx = formatNa(max, unit);
  if (cur === NA && mx === NA) return NA;
  return `${cur} / ${mx}`;
}

/**
 * RAM admission state. On a 16 GB box RAM binds BEFORE VRAM: residency refuses a start while free
 * RAM is under the configured floor, even with most of the card idle. Returning `belowFloor` as a
 * derived fact (rather than colouring inline in JSX) keeps the rule testable and identical to the
 * daemon's comparison — `free < floor`, strictly.
 *
 * `hasFloor` is false when the daemon omits/zeroes the floor; the caller must then say nothing
 * about admission rather than invent a default.
 */
export interface RamFloorState {
  hasFloor: boolean;
  belowFloor: boolean;
  /** MiB still to free before a start would be admitted; 0 when already above the floor. */
  shortMib: number;
}

export function ramFloorState(
  freeMib: number | null | undefined,
  floorMib: number | null | undefined,
): RamFloorState {
  const floor = floorMib != null && Number.isFinite(floorMib) && floorMib > 0 ? floorMib : 0;
  if (floor === 0 || freeMib == null || !Number.isFinite(freeMib)) {
    return { hasFloor: floor > 0, belowFloor: false, shortMib: 0 };
  }
  const below = freeMib < floor;
  return { hasFloor: true, belowFloor: below, shortMib: below ? floor - freeMib : 0 };
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
