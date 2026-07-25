import { describe, expect, it } from 'vitest';
import {
  computeVramBar,
  formatBytes,
  formatMetric,
  formatMib,
  mibToGib,
  relativeTime,
} from './format';

describe('mibToGib', () => {
  it('converts MiB to GiB', () => {
    expect(mibToGib(1024)).toBe(1);
    expect(mibToGib(16380)).toBe(16); // 15.99 → 16.0 at 1 digit
    expect(mibToGib(13287, 2)).toBeCloseTo(12.98, 2);
  });
  it('handles non-finite', () => {
    expect(mibToGib(NaN)).toBe(0);
    expect(mibToGib(Infinity)).toBe(0);
  });
});

describe('formatMib', () => {
  it('renders GiB with unit', () => {
    expect(formatMib(2823)).toBe('2.8 GiB');
    expect(formatMib(16380)).toBe('16.0 GiB');
  });
  it('renders N/A for missing/negative', () => {
    expect(formatMib(null)).toBe('N/A');
    expect(formatMib(undefined)).toBe('N/A');
    expect(formatMib(-1)).toBe('N/A');
  });
});

describe('formatBytes', () => {
  it('picks binary units', () => {
    expect(formatBytes(0)).toBe('0 B');
    expect(formatBytes(1024)).toBe('1.00 KiB');
    expect(formatBytes(4700000000)).toBe('4.38 GiB');
    // 14979000000 / 1024^3 = 13.95 → rounds to 14.0 at 1 digit
    expect(formatBytes(14979000000)).toBe('14.0 GiB');
    expect(formatBytes(13000000000)).toBe('12.1 GiB');
  });
  it('handles null/negative', () => {
    expect(formatBytes(null)).toBe('N/A');
    expect(formatBytes(-5)).toBe('N/A');
  });
});

describe('computeVramBar', () => {
  it('prefers explicit used and computes fraction/level', () => {
    const bar = computeVramBar(16380, 13287, 2823);
    expect(bar.usedMib).toBe(2823);
    expect(bar.freeMib).toBe(13287);
    expect(bar.usedFraction).toBeCloseTo(2823 / 16380, 5);
    expect(bar.usedPct).toBe(17);
    expect(bar.level).toBe('ok');
  });

  it('derives used from total-free when used absent', () => {
    const bar = computeVramBar(16380, 4000);
    expect(bar.usedMib).toBe(12380);
    expect(bar.usedPct).toBe(Math.round((12380 / 16380) * 100));
  });

  it('flags warn at >=70% and crit at >=90%', () => {
    expect(computeVramBar(100, 25).level).toBe('warn'); // 75% used
    expect(computeVramBar(100, 5).level).toBe('crit'); // 95% used
    expect(computeVramBar(100, 50).level).toBe('ok'); // 50%
  });

  it('clamps over-report to 100% (never overflows the bar)', () => {
    const bar = computeVramBar(100, 0, 250); // used > total
    expect(bar.usedFraction).toBe(1);
    expect(bar.usedPct).toBe(100);
  });

  it('handles zero/invalid total safely', () => {
    const bar = computeVramBar(0, 0);
    expect(bar.usedFraction).toBe(0);
    expect(bar.usedPct).toBe(0);
  });
});

describe('formatMetric', () => {
  it('formats with unit', () => {
    expect(formatMetric(55, '°C')).toBe('55°C');
    expect(formatMetric(14.76, ' W', 1)).toBe('14.8 W');
    expect(formatMetric(0, '%')).toBe('0%');
  });
  it('returns N/A for the -1 unavailable sentinel', () => {
    expect(formatMetric(-1, '%')).toBe('N/A');
    expect(formatMetric(null, '%')).toBe('N/A');
  });
});

describe('relativeTime', () => {
  it('formats seconds/minutes ago', () => {
    const now = Date.parse('2026-07-06T07:05:40Z');
    expect(relativeTime('2026-07-06T07:05:33Z', now)).toBe('7s ago');
    expect(relativeTime('2026-07-06T07:00:40Z', now)).toBe('5m ago');
    expect(relativeTime('2026-07-06T07:05:39Z', now)).toBe('just now');
  });
  it('returns empty for unparseable', () => {
    expect(relativeTime(null)).toBe('');
    expect(relativeTime('not-a-date')).toBe('');
  });
});
