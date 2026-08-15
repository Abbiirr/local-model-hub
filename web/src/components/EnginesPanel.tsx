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
  const engines = props.engines ?? [];
  // Which llama.cpp build a start ACTUALLY uses (§3.5). Three are detected on the reference box
  // and, before this, nothing on screen said which one would run. Selection is capability-first
  // in CONFIGURED order (compat/input.go bestLlamaBinary): the first DETECTED binary that
  // satisfies the model's requirements wins, so the first detected entry is the default choice
  // and the others only take over when a model demands a capability it lacks.
  const defaultLlamaIndex = engines.findIndex(
    (e) => e.name === 'llamacpp' && e.installed,
  );

  return (
    <Panel title="Engines">
      {props.loading && !props.engines ? (
        <Skeleton>Detecting engines…</Skeleton>
      ) : engines.length > 0 ? (
        <div className={styles.list}>
          {engines.map((e, i) => (
            <EngineCard
              key={`${e.name}-${cardKey(e)}-${i}`}
              e={e}
              role={llamaRole(e, i, defaultLlamaIndex)}
            />
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

/** A short, honest label for WHEN this llama.cpp build gets used, or null when the question does
 *  not apply (a non-llamacpp engine, or one that was not detected). */
function llamaRole(
  e: Engine,
  index: number,
  defaultIndex: number,
): { label: string; title: string } | null {
  if (e.name !== 'llamacpp' || !e.installed || defaultIndex < 0) return null;
  if (index === defaultIndex) {
    return {
      label: 'used by default',
      title:
        'Binary selection walks the configured order and takes the first detected build that ' +
        'satisfies the model. This one is first, so it runs every start unless a model needs a ' +
        'capability it does not have.',
    };
  }
  // A later build is only reachable when it holds a capability the default lacks — say WHICH.
  const caps = e.details?.capabilities ?? {};
  const specials: string[] = [];
  if (caps.has_tq3 || e.details?.has_tq3) specials.push('TQ3_* quants');
  if (caps.has_spec_mtp) specials.push('draft-mtp profiles');
  if (specials.length === 0) return null;
  return {
    label: `fallback: ${specials.join(', ')}`,
    title:
      'Not used for an ordinary start — the build above is chosen first. This one is selected ' +
      `only when the model or profile requires ${specials.join(' or ')}.`,
  };
}

function EngineCard({
  e,
  role,
}: {
  e: Engine;
  role: { label: string; title: string } | null;
}) {
  const reachable = e.running;
  const detected = e.installed;
  const backends = e.details?.backends ?? [];
  const caps = engineCaps(e);
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
          {/* WHICH of several detected llama.cpp builds a start actually uses (§3.5). */}
          {role && (
            <span title={role.title}>
              <Badge tone={role.label === 'used by default' ? 'ok' : 'neutral'}>
                {role.label}
              </Badge>
            </span>
          )}
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
        <div className={styles.reasonWrap}>
          <Reason crit={detected && !reachable}>{e.reason}</Reason>
        </div>
      )}
    </div>
  );
}

/** Human capability chips for an engine.
 *
 *  The llama.cpp adapter reports a `capabilities` BOOLEAN MAP, not the `caps` string list the
 *  original projection assumed — so on a real box the chip row rendered backends only and every
 *  capability was buried in the prose reason. Prefer the list when present, otherwise derive it
 *  from the map; unknown keys pass through with their `has_` prefix stripped rather than being
 *  dropped, so a newly added capability shows up instead of silently disappearing. */
function engineCaps(e: Engine): string[] {
  const listed = e.details?.caps;
  if (listed && listed.length > 0) return listed;
  const map = e.details?.capabilities;
  if (!map) return [];
  const names: Record<string, string> = {
    has_fit: 'fit',
    has_flash_attn: 'flash-attn',
    has_sleep_idle: 'sleep-idle',
    has_tq3: 'tq3',
    has_kv_cache_types: 'kv-types',
    has_parallel_slots: 'slots',
    has_spec_mtp: 'spec-mtp',
    has_spec_draft_max: 'spec-draft-max',
  };
  return Object.entries(map)
    .filter(([, on]) => on === true)
    .map(([k]) => names[k] ?? k.replace(/^has_/, ''));
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
