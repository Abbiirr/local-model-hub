import type { Engine } from '../api/types';
import {
  Badge,
  Chip,
  ChipRow,
  EmptyState,
  ErrorState,
  Panel,
  Reason,
  Skeleton,
} from './ui';
import styles from './EnginesPanel.module.css';

export function EnginesPanel(props: {
  engines: Engine[] | null;
  loading: boolean;
  error: string | null;
}) {
  return (
    <Panel title="Engines">
      {props.loading && !props.engines ? (
        <Skeleton>Detecting engines…</Skeleton>
      ) : props.engines && props.engines.length > 0 ? (
        <div className={styles.list}>
          {props.engines.map((e, i) => (
            <EngineCard key={`${e.name}-${cardKey(e)}-${i}`} e={e} />
          ))}
        </div>
      ) : props.error ? null : (
        <EmptyState>No engines configured.</EmptyState>
      )}
      {props.error && (
        <ErrorState>Could not load engines: {props.error}</ErrorState>
      )}
    </Panel>
  );
}

function cardKey(e: Engine): string {
  return e.details?.resolved_path || e.details?.binary_path || e.base_url || '';
}

function EngineCard({ e }: { e: Engine }) {
  const reachable = e.running;
  const detected = e.installed;
  const backends = e.details?.backends ?? [];
  const caps = e.details?.caps ?? [];
  const binaryLabel =
    e.details?.resolved_path || e.details?.binary_path || undefined;

  const cls = [
    styles.card,
    !detected ? styles.cardMissing : '',
    detected && !reachable ? styles.cardUnreachable : '',
  ]
    .filter(Boolean)
    .join(' ');

  return (
    <div className={cls}>
      <div className={styles.head}>
        <div className={styles.name}>
          <h3>{engineLabel(e.name)}</h3>
          {e.version && <span className={styles.version}>{e.version}</span>}
        </div>
        <div className={styles.badges}>
          {detected ? (
            <Badge tone="accent" dot>
              detected
            </Badge>
          ) : (
            <Badge tone="neutral" dot>
              not found
            </Badge>
          )}
          {detected &&
            (reachable ? (
              <Badge tone="ok" dot>
                reachable
              </Badge>
            ) : (
              <Badge tone="warn" dot>
                unreachable
              </Badge>
            ))}
        </div>
      </div>

      {binaryLabel && (
        <div className={styles.baseUrl} title={binaryLabel}>
          {binaryLabel}
        </div>
      )}
      {!binaryLabel && e.base_url && (
        <div className={styles.baseUrl} title={e.base_url}>
          {e.base_url}
        </div>
      )}

      {(backends.length > 0 || caps.length > 0) && (
        <ChipRow>
          {backends.map((b) => (
            <Chip key={`b-${b}`} title="backend">
              {b}
            </Chip>
          ))}
          {caps.map((c) => (
            <Chip key={`c-${c}`} title="capability">
              {c}
            </Chip>
          ))}
        </ChipRow>
      )}

      {/* Mandatory reason — ALWAYS shown when not reachable (and shown as info
          otherwise if present). */}
      {e.reason && (
        <div style={{ marginTop: 6 }}>
          <Reason crit={detected && !reachable}>{e.reason}</Reason>
        </div>
      )}
    </div>
  );
}

function engineLabel(name: string): string {
  switch (name) {
    case 'ollama':
      return 'Ollama';
    case 'llamacpp':
      return 'llama.cpp';
    case 'vllm':
      return 'vLLM';
    default:
      return name;
  }
}
