import type { ConnectionStatus } from '../live/connection';
import { relativeTime } from '../lib/format';
import { Badge, type Tone } from './ui';
import styles from './Header.module.css';

export function Header(props: {
  status: ConnectionStatus;
  lastUpdate: string | null;
  daemonVersion?: string | null;
}) {
  const s = props.status;
  const conn = connLabel(s);
  return (
    <header className={styles.header}>
      <div className={styles.brand}>
        <span className={styles.logo}>
          LocalHub<span>Compass</span>
        </span>
        {props.daemonVersion && (
          <span className={styles.ver}>v{props.daemonVersion}</span>
        )}
      </div>
      <div className={styles.right}>
        <span className={styles.updated}>
          {props.lastUpdate ? `updated ${relativeTime(props.lastUpdate)}` : ''}
        </span>
        <Badge tone={conn.tone} dot>
          {conn.label}
        </Badge>
      </div>
    </header>
  );
}

function connLabel(s: ConnectionStatus): { label: string; tone: Tone } {
  const t = s.transport.toUpperCase();
  switch (s.state) {
    case 'connected':
      return { label: `${t} live`, tone: 'ok' };
    case 'connecting':
      return { label: `${t}…`, tone: 'warn' };
    case 'reconnecting':
      return { label: `reconnecting`, tone: 'warn' };
    case 'closed':
      return { label: 'offline', tone: 'crit' };
    case 'idle':
    default:
      return { label: 'idle', tone: 'neutral' };
  }
}
