// Shared presentational primitives: Panel, Badge, Chip, Bar, states.
import type { ReactNode } from 'react';
import styles from './ui.module.css';

export type Tone = 'ok' | 'warn' | 'crit' | 'neutral' | 'accent';

export function Panel(props: {
  title: ReactNode;
  actions?: ReactNode;
  flush?: boolean;
  children: ReactNode;
}) {
  return (
    <section className={styles.panel}>
      <header className={styles.panelHeader}>
        <div className={styles.panelTitle}>{props.title}</div>
        {props.actions}
      </header>
      <div className={props.flush ? styles.panelBodyFlush : styles.panelBody}>
        {props.children}
      </div>
    </section>
  );
}

export function Badge(props: { tone: Tone; dot?: boolean; children: ReactNode }) {
  return (
    <span className={`${styles.badge} ${styles[props.tone]}`}>
      {props.dot && <span className={styles.dot} />}
      {props.children}
    </span>
  );
}

export function Chip(props: { children: ReactNode; title?: string }) {
  return (
    <span className={styles.chip} title={props.title}>
      {props.children}
    </span>
  );
}

export function ChipRow(props: { children: ReactNode }) {
  return <div className={styles.chipRow}>{props.children}</div>;
}

export function Bar(props: {
  fraction: number; // 0..1
  level: 'ok' | 'warn' | 'crit';
  label?: string;
}) {
  const pct = Math.round(Math.min(1, Math.max(0, props.fraction)) * 100);
  return (
    <div
      className={styles.bar}
      role="progressbar"
      aria-valuenow={pct}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-label={props.label}
    >
      <div
        className={`${styles.barFill} ${styles[props.level]}`}
        style={{ width: `${pct}%` }}
      />
    </div>
  );
}

/** Reason text — always rendered when present (mandatory-reason rule). */
export function Reason(props: { children: ReactNode; crit?: boolean }) {
  return (
    <div className={`${styles.reason} ${props.crit ? styles.reasonCrit : ''}`}>
      {props.children}
    </div>
  );
}

export function EmptyState(props: { children: ReactNode }) {
  return <div className={styles.emptyState}>{props.children}</div>;
}

export function ErrorState(props: { children: ReactNode }) {
  return <div className={styles.errorState}>{props.children}</div>;
}

export function Skeleton(props: { children?: ReactNode }) {
  return <div className={styles.skeleton}>{props.children ?? 'Loading…'}</div>;
}

export function IconButton(props: {
  onClick: () => void;
  disabled?: boolean;
  children: ReactNode;
  title?: string;
}) {
  return (
    <button
      className={styles.iconBtn}
      onClick={props.onClick}
      disabled={props.disabled}
      title={props.title}
      type="button"
    >
      {props.children}
    </button>
  );
}
