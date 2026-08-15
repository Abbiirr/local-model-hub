import { useCallback, useEffect, useMemo, useState } from 'react';
import type { InstanceState, InstanceView, ModelSummary } from '../api/types';
import { api, ApiError, isQueuedResponse } from '../api/client';
import { useCompatVerdicts, type CompatEntry } from '../api/useCompatVerdicts';
import { formatMib } from '../lib/format';
import {
  Panel,
  Badge,
  Reason,
  ClampText,
  EmptyState,
  ErrorState,
  Skeleton,
  type Tone,
} from './ui';
import {
  AdvancedStartForm,
  EMPTY_PROFILE,
  profileToRequest,
  summarize,
  type LaunchProfileDraft,
} from './AdvancedStartForm';
import styles from './InstancesCard.module.css';

// InstancesCard is the Phase-4 run/stop surface (P4.1/P4.2). It shows every spawned instance
// with its lifecycle state badge + mandatory reason + VRAM numbers + stop button, and offers a
// start button per PRESENT startable inventory model (GGUF or Ollama), each annotated with its
// cached compat status. A "won't fit" model is still submittable — the daemon is the authority
// and refuses with the numeric reason. ADMIN actions — starting/stopping spawns/kills GPU
// processes; the daemon binds loopback + token-gates.

// Map an instance state to a badge tone.
const STATE_TONE: Record<InstanceState, Tone> = {
  REQUESTED: 'neutral',
  STARTING: 'accent',
  HEALTHY: 'ok',
  DRAINING: 'warn',
  STOPPED: 'neutral',
  FAILED: 'crit',
};

function isActive(s: InstanceState): boolean {
  return s === 'REQUESTED' || s === 'STARTING' || s === 'HEALTHY' || s === 'DRAINING';
}

// Annotate a picker option with its compat status. This is advisory only — a "won't fit" option is
// still submittable; the daemon is the authority and refuses with the numeric reason.
function compatAnnotation(status: string | undefined): { label: string; tone: Tone } {
  switch (status) {
    case 'runs':
      return { label: 'fits', tone: 'ok' };
    case 'runs_degraded':
      return { label: 'fits (degraded)', tone: 'warn' };
    case 'wont_run':
      return { label: "won't fit", tone: 'crit' };
    default:
      return { label: 'unchecked', tone: 'neutral' };
  }
}

/**
 * Merge the LIVE verdict (fetched per row, §3.3) with the row's CACHED `compat_status`.
 *
 * Precedence is deliberate:
 *   • a settled live verdict wins — it was computed against the free VRAM there is right now;
 *   • while it is still in flight, the cached status is shown rather than a blank, so a row that
 *     already knows something never regresses to "unchecked" for a second;
 *   • a FAILED fetch falls back to the cached status too, and never invents `wont_run` — a check
 *     that could not be made is not evidence that a model will not run.
 * The reason string always rides along as the badge's tooltip (mandatory-reason rule).
 */
function verdictAnnotation(
  model: ModelSummary,
  entry: CompatEntry | undefined,
): { label: string; tone: Tone; title: string; estimate?: number } {
  if (entry?.state === 'ready') {
    const a = compatAnnotation(entry.status);
    return { ...a, title: entry.reason, estimate: entry.estimatedVramMb };
  }
  const cached = compatAnnotation(model.compat_status);
  if (entry?.state === 'loading' && !model.compat_status) {
    return { label: 'checking…', tone: 'neutral', title: 'Computing a fit verdict for this model.' };
  }
  return {
    ...cached,
    title:
      entry?.state === 'error'
        ? `Could not compute a fit verdict: ${entry.reason}. Showing the last cached status.`
        : model.compat_status
          ? 'Cached verdict from the daemon inventory.'
          : 'No verdict yet for this model.',
  };
}

/** What the collapsed "Launch options" row reveals. Anything non-default MUST appear here — a
 *  hidden setting that silently changes what gets launched is worse than no disclosure at all. */
function optionsSummary(
  allowCpuOffload: boolean,
  profile: LaunchProfileDraft,
): string {
  const parts: string[] = [];
  if (allowCpuOffload) parts.push('CPU offload allowed');
  const p = summarize(profile);
  if (p) parts.push(p);
  return parts.join(' · ');
}

export function InstancesCard(props: {
  models: ModelSummary[] | null;
  instancesInvalidation: number;
  // Shared GPU/CPU placement toggle (lifted to App). false = full GPU (never split to CPU).
  allowCpuOffload: boolean;
  onAllowCpuOffloadChange: (v: boolean) => void;
  /** Bumped by App when free VRAM changed materially (a Free-GPU sweep). Cached fit verdicts are
   *  only true for the VRAM they were computed against, so this is their invalidation signal. */
  compatEpoch?: number;
}) {
  const [instances, setInstances] = useState<InstanceView[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [startingModel, setStartingModel] = useState<string | null>(null);
  const [actionReason, setActionReason] = useState<string | null>(null);
  const [showHistory, setShowHistory] = useState(false);
  // Advanced launch profile (M2.3): independent K/V cache types, slots, and speculative mode. Held
  // here so ONE armed profile applies to whichever model is started from this picker — the same scope
  // as the existing CPU-offload toggle. Empty by default, so the plain "Start" path sends exactly
  // what it sent before this feature existed.
  const [profile, setProfile] = useState<LaunchProfileDraft>(EMPTY_PROFILE);
  // The model the saved-default actions target. A default is PER-MODEL, so "Load saved" / "Save as
  // default" need a specific one; this tracks the most recent model the operator started.
  const [profileModelId, setProfileModelId] = useState<string | null>(null);
  // Two-step confirm before killing an ADOPTED process (see onStop).
  const [confirmKillId, setConfirmKillId] = useState<string | null>(null);
  // Attach-a-running-server form. Host matters: a server bound to a LAN address is unreachable on
  // loopback, which is the usual cause of a 502 from the attach endpoint.
  const [attachPort, setAttachPort] = useState('');
  const [attachHost, setAttachHost] = useState('');
  const [attachVram, setAttachVram] = useState('');
  const [attaching, setAttaching] = useState(false);
  // §3.4: the model list is the primary action, so the CPU-offload toggle, the advanced profile
  // and the attach form collapse behind one disclosure. Collapsed by default — on a GPU-only box
  // leading with "Allow CPU offload" is leading with the wrong thing.
  const [showOptions, setShowOptions] = useState(false);

  const refresh = useCallback(async () => {
    try {
      const list = await api.instances();
      setInstances(Array.isArray(list) ? list : []);
      setError(null);
    } catch (e) {
      setError(e instanceof ApiError ? e.reason : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  // Initial load + refetch when the live channel signals instances.updated. Also a slow poll
  // fallback so STARTING→HEALTHY transitions surface even without a live push.
  useEffect(() => {
    void refresh();
  }, [refresh, props.instancesInvalidation]);

  useEffect(() => {
    const id = setInterval(() => void refresh(), 4000);
    return () => clearInterval(id);
  }, [refresh]);

  const onStart = useCallback(
    async (modelId: string) => {
      setStartingModel(modelId);
      setProfileModelId(modelId); // the saved-default actions target the model just started
      setActionReason(null);
      try {
        // The armed advanced profile (independent -ctk/-ctv, -np, speculative mode + draft max) is
        // merged into the request. profileToRequest OMITS every blank field, so an untouched form
        // produces the byte-identical body this call sent before M2.3.
        const res = await api.startInstance(
          { model_id: modelId, ...profileToRequest(profile) },
          { allowCpuOffload: props.allowCpuOffload },
        );
        if (isQueuedResponse(res)) {
          // 202: residency queued the start on VRAM — nothing spawned yet. Surface the position +
          // blocked_reason so the operator knows why it's waiting (mandatory-reason rule).
          setActionReason(
            `Queued ${modelId} (position ${res.position}): ${res.blocked_reason}`,
          );
        } else {
          setActionReason(`Started ${modelId}: ${res.state}. ${res.reason ?? ''}`);
        }
        void refresh();
      } catch (e) {
        // A 409 REFUSED carries the numeric why in `reason` (gate).
        setActionReason(e instanceof ApiError ? `Refused: ${e.reason}` : String(e));
      } finally {
        setStartingModel(null);
      }
    },
    [refresh, props.allowCpuOffload, profile],
  );

  // Stopping an ADOPTED instance kills a process LocalHub never started (typically a launcher
  // script's server). The API refuses it without ?force=true; the UI additionally requires a SECOND
  // click, so a single mis-click can never kill someone else's server.
  const onStop = useCallback(
    async (id: string, adopted: boolean) => {
      if (adopted && confirmKillId !== id) {
        setConfirmKillId(id);
        setActionReason(
          'This instance was ADOPTED — LocalHub did not start it. Stopping it will KILL that process. ' +
            'Click again to confirm, or leave it alone and stop it where it was started.',
        );
        return;
      }
      setBusyId(id);
      setActionReason(null);
      try {
        const inst = await api.stopInstance(id, { force: adopted });
        setActionReason(inst.reason ?? `Stopped ${id}.`);
        void refresh();
      } catch (e) {
        setActionReason(e instanceof ApiError ? e.reason : String(e));
      } finally {
        setBusyId(null);
        setConfirmKillId(null);
      }
    },
    [refresh, confirmKillId],
  );

  // Adopt an already-running llama-server. No admission runs — the process already holds its VRAM —
  // and the profile is read from the live process, so this is the path for a model started by
  // scripts/run-qwen-64k-q4q4.ps1 or any other launcher.
  const onAttach = useCallback(async () => {
    const port = Number(attachPort);
    if (!Number.isFinite(port) || port <= 0) {
      setActionReason('Enter the port the running server listens on.');
      return;
    }
    setAttaching(true);
    setActionReason(null);
    try {
      const inst = await api.attachInstance({
        port,
        host: attachHost.trim() || undefined,
        vram_mib: Number(attachVram) > 0 ? Number(attachVram) : undefined,
      });
      setActionReason(`Attached ${inst.model_id} (pid ${inst.pid}): ${inst.reason ?? ''}`);
      void refresh();
    } catch (e) {
      // A 502 here almost always means the server binds a LAN address and `host` was left blank.
      setActionReason(e instanceof ApiError ? `Attach failed: ${e.reason}` : String(e));
    } finally {
      setAttaching(false);
    }
  }, [attachPort, attachHost, attachVram, refresh]);

  // Startable inventory models: every PRESENT model with a spawnable engine (GGUF via llama.cpp,
  // or an Ollama model), minus any already running. We deliberately do NOT filter by cached
  // compat_status — a model may be unchecked, or the operator may want to try a wont_run one and
  // let the daemon refuse with the authoritative numeric reason (source of truth is the core, not
  // a UI filter). Each option is annotated with its cached compat status.
  const activeModelIds = new Set(
    (instances ?? []).filter((i) => isActive(i.state)).map((i) => i.model_id),
  );
  const startable = (props.models ?? []).filter(
    (m) =>
      m.format === 'gguf' &&
      m.presence !== 'missing' &&
      !activeModelIds.has(m.id),
  );
  const inventoryEmpty = (props.models ?? []).length === 0;

  // §3.3 — real verdicts for every startable row, fetched once and cached (see useCompatVerdicts).
  // Memoised so a poll tick that returns an identical instance list does not look like a new set
  // of ids and retrigger the fetch.
  const startableIds = useMemo(
    () => startable.map((m) => m.id),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [startable.map((m) => m.id).join('\n')],
  );
  const verdicts = useCompatVerdicts(startableIds, props.compatEpoch ?? 0);

  const active = (instances ?? []).filter((i) => isActive(i.state));
  const recent = (instances ?? []).filter((i) => !isActive(i.state)).slice(0, 5);

  return (
    <Panel title="Instances">
      {actionReason && <Reason>{actionReason}</Reason>}
      {error && <ErrorState>{error}</ErrorState>}

      {loading && !instances ? (
        <Skeleton>Loading instances…</Skeleton>
      ) : (
        <>
          {active.length === 0 && recent.length === 0 && (
            <EmptyState>No instances yet. Start a runnable model below.</EmptyState>
          )}

          {active.map((inst) => (
            <InstanceRow
              key={inst.id}
              inst={inst}
              busy={busyId === inst.id}
              confirmKill={confirmKillId === inst.id}
              onStop={() => onStop(inst.id, inst.adopted === true)}
            />
          ))}

          {/* Terminal/stopped instances are kept out of the default view (the
              homepage shows only what's live). They stay reachable behind a
              quiet, collapsed-by-default History toggle. */}
          {recent.length > 0 && (
            <div className={styles.history}>
              <button
                type="button"
                className={styles.historyToggle}
                onClick={() => setShowHistory((v) => !v)}
                aria-expanded={showHistory}
              >
                {showHistory ? '▾' : '▸'} History ({recent.length})
              </button>
              {showHistory &&
                recent.map((inst) => (
                  <InstanceRow key={inst.id} inst={inst} recent />
                ))}
            </div>
          )}
        </>
      )}

      {/* §3.4 — the model list comes FIRST. Starting a model is the primary action on this panel;
          the CPU-offload toggle, the advanced profile and the attach form are occasional operator
          settings and now sit behind one disclosure below it.

          Every present GGUF model is listed (not only the ones that fit) — the daemon refuses an
          over-budget start with the authoritative numeric reason, so a UI filter would only hide
          the explanation. Each row carries a REAL verdict (§3.3) rather than "unchecked". */}
      <div className={styles.startSection}>
        <div className={styles.startHeading}>Start a model</div>
        {startable.length === 0 ? (
          <EmptyState>
            {inventoryEmpty ? (
              <>
                No models in inventory yet.
                <br />
                Add one from the Downloads panel (Hugging Face, a GGUF URL, or a local
                import), then start it here.
              </>
            ) : (
              <>
                Every present model is already running.
                <br />
                Stop one above, or add more from the Downloads panel.
              </>
            )}
          </EmptyState>
        ) : (
          <ul className={styles.startList}>
            {startable.map((m) => {
              const annot = verdictAnnotation(m, verdicts[m.id]);
              return (
                <li key={m.id} className={styles.startItem}>
                  <div className={styles.startModel}>
                    <span className={styles.startName} title={m.display_name}>
                      {m.display_name}
                    </span>
                    {/* The verdict's mandatory reason rides along as the tooltip — the badge is a
                        summary, never the whole answer. */}
                    <span title={annot.title} className={styles.startVerdict}>
                      <Badge tone={annot.tone}>{annot.label}</Badge>
                      {annot.estimate != null && (
                        <span className={styles.startEstimate}>
                          ~{formatMib(annot.estimate)}
                        </span>
                      )}
                    </span>
                  </div>
                  <button
                    type="button"
                    className={styles.startBtn}
                    disabled={startingModel !== null}
                    onClick={() => onStart(m.id)}
                  >
                    {startingModel === m.id ? 'Starting…' : 'Start'}
                  </button>
                </li>
              );
            })}
          </ul>
        )}

        {/* Launch options — collapsed by default. The collapsed summary names anything non-default
            that is armed, so a changed setting can never be silently in effect. */}
        <div className={styles.options}>
          <button
            type="button"
            className={styles.optionsToggle}
            onClick={() => setShowOptions((v) => !v)}
            aria-expanded={showOptions}
          >
            {showOptions ? '▾' : '▸'} Launch options
            {!showOptions && optionsSummary(props.allowCpuOffload, profile) && (
              <span className={styles.optionsSummary}>
                {' · '}
                {optionsSummary(props.allowCpuOffload, profile)}
              </span>
            )}
          </button>

          {showOptions && (
            <div className={styles.optionsBody}>
              {/* GPU/CPU placement toggle — applies to every start from this picker AND to Inventory
                  Run (shared, lifted to App). Off by default: full GPU, never split to CPU. */}
              <label className={styles.offloadRow}>
                <input
                  type="checkbox"
                  className={styles.offloadInput}
                  checked={props.allowCpuOffload}
                  onChange={(e) => props.onAllowCpuOffloadChange(e.target.checked)}
                />
                <span className={styles.offloadLabel}>Allow CPU offload</span>
                <span className={styles.offloadHint}>
                  default: full GPU — never split to CPU
                </span>
              </label>

              {/* Advanced launch profile (M2.3): the independent -ctk/-ctv pair, slot count, and
                  speculative mode. This is the only way to express the validated Qwen3.6-27B MTP
                  profile (-ctk q8_0 -ctv q4_0 --spec-type draft-mtp --spec-draft-n-max 2 -np 1). */}
              <AdvancedStartForm
                profile={profile}
                onChange={setProfile}
                modelId={profileModelId}
              />

              {/* ATTACH an already-running server (migration 0012). This is how a model started
                  outside LocalHub — e.g. scripts/run-qwen-64k-q4q4.ps1 — becomes manageable here. No
                  admission runs and nothing is spawned: the profile, context and PID are READ from
                  the live process, so attaching cannot disturb it. Host matters — a server bound to
                  a LAN address is unreachable on loopback, the usual cause of an attach failure. */}
              <div className={styles.attachRow}>
                <span className={styles.attachLabel}>Attach a running server</span>
                <input
                  className={styles.attachInput}
                  type="number"
                  min={1}
                  max={65535}
                  inputMode="numeric"
                  placeholder="port"
                  aria-label="Attach port"
                  value={attachPort}
                  onChange={(e) => setAttachPort(e.target.value)}
                />
                <input
                  className={styles.attachInput}
                  type="text"
                  placeholder="host (default 127.0.0.1)"
                  aria-label="Attach host"
                  value={attachHost}
                  onChange={(e) => setAttachHost(e.target.value)}
                />
                <input
                  className={styles.attachInput}
                  type="number"
                  min={0}
                  inputMode="numeric"
                  placeholder="VRAM MiB (optional)"
                  aria-label="Attach measured VRAM MiB"
                  value={attachVram}
                  onChange={(e) => setAttachVram(e.target.value)}
                />
                <button
                  type="button"
                  className={styles.attachBtn}
                  disabled={attaching}
                  onClick={() => void onAttach()}
                >
                  {attaching ? 'Attaching…' : 'Attach'}
                </button>
                <span className={styles.attachHint}>
                  reads the live process; never starts or stops anything
                </span>
              </div>
            </div>
          )}
        </div>
      </div>
    </Panel>
  );
}

function InstanceRow(props: {
  inst: InstanceView;
  busy?: boolean;
  recent?: boolean;
  confirmKill?: boolean;
  onStop?: () => void;
}) {
  const { inst } = props;
  return (
    <div className={`${styles.row} ${props.recent ? styles.rowRecent : ''}`}>
      <div className={styles.rowHead}>
        <Badge tone={STATE_TONE[inst.state]} dot>
          {inst.state}
        </Badge>
        <span className={styles.model}>{inst.model_id}</span>
        {inst.port ? <span className={styles.port}>:{inst.port}</span> : null}
        {/* An ADOPTED instance was started outside LocalHub. Surfacing it is a safety affordance:
            Stop here kills someone else's process, and residency will never auto-evict it. */}
        {inst.adopted ? <Badge tone="warn">adopted</Badge> : null}
        {!props.recent && props.onStop && (
          <button
            type="button"
            className={styles.stopBtn}
            disabled={props.busy}
            onClick={props.onStop}
          >
            {props.busy
              ? 'Stopping…'
              : props.confirmKill
                ? 'Confirm kill'
                : inst.adopted
                  ? 'Stop (external)'
                  : 'Stop'}
          </button>
        )}
      </div>

      <div className={styles.meta}>
        {inst.vram_est_mib ? <span>est {inst.vram_est_mib} MiB</span> : null}
        {inst.vram_obs_mib ? <span>peak {inst.vram_obs_mib} MiB</span> : null}
        {inst.vram_freed_mib ? <span>freed {inst.vram_freed_mib} MiB</span> : null}
        {inst.ctx ? <span>ctx {inst.ctx}</span> : null}
        {inst.adopted_source ? <span>{inst.adopted_source}</span> : null}
        {inst.pid ? <span>pid {inst.pid}</span> : null}
        {/* M5: a HEALTHY instance with consecutive failed probes is DEGRADED, not healthy — the
            monitor is seeing failures but has not reached the reap threshold. Showing the count is
            what stops the badge from over-claiming. */}
        {inst.health_failures ? (
          <span>health failing ×{inst.health_failures}</span>
        ) : null}
        {/* M5.5: on a terminal instance, an UNVERIFIED reclaim must be visually distinct from a
            verified one. state=STOPPED alone never means "VRAM is back". */}
        {inst.reclaim_verified === false ? (
          <span>reclaim UNVERIFIED</span>
        ) : null}
      </div>

      {inst.reason && (
        <div className={styles.reason}>
          <ClampText lines={2} crit={inst.state === 'FAILED'}>
            {inst.reason}
          </ClampText>
        </div>
      )}
    </div>
  );
}
