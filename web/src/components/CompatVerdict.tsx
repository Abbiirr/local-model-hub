import { useState } from 'react';
import type { CompatStatus, Verdict } from '../api/types';
import { Badge, Reason } from './ui';
import type { Tone } from './ui';
import styles from './CompatVerdict.module.css';

// Maps the four-value verdict status to a badge tone + label. runs=ok, runs_degraded=warn,
// wont_run=crit, unknown=neutral. The label matches the spec vocabulary the user reads.
export function statusTone(status: CompatStatus): { tone: Tone; label: string } {
  switch (status) {
    case 'runs':
      return { tone: 'ok', label: 'runs' };
    case 'runs_degraded':
      return { tone: 'warn', label: 'degraded' };
    case 'wont_run':
      return { tone: 'crit', label: "won't run" };
    default:
      return { tone: 'neutral', label: 'unknown' };
  }
}

/** Compact badge for the inventory table (status only). */
export function CompatBadge({ status }: { status: CompatStatus }) {
  const { tone, label } = statusTone(status);
  return (
    <Badge tone={tone} dot>
      {label}
    </Badge>
  );
}

// Full verdict view: badge + expandable reason + estimated VRAM, max ctx, run flags, warnings.
// The reason is ALWAYS shown (mandatory-reason rule) — expansion only reveals the extra
// numeric detail + flags, never hides the reason.
export function CompatVerdictView({
  verdict,
  defaultOpen = false,
}: {
  verdict: Verdict;
  defaultOpen?: boolean;
}) {
  const [open, setOpen] = useState(defaultOpen);
  const { tone, label } = statusTone(verdict.status);

  return (
    <div className={styles.verdict}>
      <button
        type="button"
        className={styles.summary}
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
      >
        <Badge tone={tone} dot>
          {label}
        </Badge>
        {verdict.recommended_engine && (
          <span className={styles.engine}>
            via <b>{verdict.recommended_engine}</b>
          </span>
        )}
        {verdict.estimated_vram_mb > 0 && (
          <span className={styles.vram}>
            ~{formatMiB(verdict.estimated_vram_mb)}
          </span>
        )}
        <span className={styles.chevron}>{open ? '▾' : '▸'}</span>
      </button>

      {/* Reason is always visible. */}
      <Reason crit={verdict.status === 'wont_run'}>{verdict.reason}</Reason>

      {open && (
        <div className={styles.detail}>
          <dl className={styles.grid}>
            {verdict.estimated_vram_mb > 0 && (
              <>
                <dt>Estimated VRAM</dt>
                <dd>{formatMiB(verdict.estimated_vram_mb)}</dd>
              </>
            )}
            {verdict.vram_free_mb != null && (
              <>
                <dt>Free / total VRAM</dt>
                <dd>
                  {formatMiB(verdict.vram_free_mb)} /{' '}
                  {formatMiB(verdict.vram_total_mb ?? 0)}
                </dd>
              </>
            )}
            {verdict.requested_context != null && (
              <>
                <dt>Requested ctx</dt>
                <dd>{verdict.requested_context.toLocaleString()}</dd>
              </>
            )}
            {verdict.max_context_that_fits != null &&
              verdict.max_context_that_fits > 0 && (
                <>
                  <dt>Max ctx that fits</dt>
                  <dd>{verdict.max_context_that_fits.toLocaleString()}</dd>
                </>
              )}
            {verdict.confidence && (
              <>
                <dt>Confidence</dt>
                <dd>{verdict.confidence}</dd>
              </>
            )}
            {verdict.estimator && (
              <>
                <dt>Estimator</dt>
                <dd className={styles.mono}>{verdict.estimator}</dd>
              </>
            )}
          </dl>

          {verdict.run_flags && verdict.run_flags.length > 0 && (
            <div className={styles.flags}>
              <div className={styles.flagsLabel}>Run flags</div>
              <code className={styles.flagsCode}>
                {verdict.run_flags.join(' ')}
              </code>
            </div>
          )}

          {verdict.warnings && verdict.warnings.length > 0 && (
            <ul className={styles.warnings}>
              {verdict.warnings.map((w, i) => (
                <li key={i}>{w}</li>
              ))}
            </ul>
          )}

          {verdict.engine_choices && verdict.engine_choices.length > 1 && (
            <div className={styles.alts}>
              <div className={styles.flagsLabel}>Per-engine</div>
              {verdict.engine_choices.map((c, i) => {
                const t = statusTone(c.status);
                return (
                  <div key={i} className={styles.alt}>
                    <Badge tone={t.tone}>{c.engine}</Badge>
                    {c.detail && <span className={styles.altDetail}>{c.detail}</span>}
                    <span className={styles.altStatus}>{t.label}</span>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function formatMiB(mib: number): string {
  if (mib >= 1024) return `${(mib / 1024).toFixed(1)} GiB`;
  return `${mib} MiB`;
}
