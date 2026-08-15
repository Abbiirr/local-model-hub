import { useCallback, useState } from 'react';
import { api, ApiError } from '../api/client';
import type { ModelLaunchDefault, StartInstanceRequest } from '../api/types';
import styles from './AdvancedStartForm.module.css';

/**
 * LaunchProfileDraft is the form's editable state — the UI mirror of compat.LaunchProfile. Every field
 * here changes the emitted llama-server flags and/or the VRAM estimate, which is exactly the
 * membership rule the Go side uses.
 *
 * Strings (not numbers) for the numeric inputs: an in-progress edit like "" or "1" must not be
 * coerced to 0 mid-typing, and `profileToRequest` is the single place that parses them.
 */
export interface LaunchProfileDraft {
  ctx: string;
  kvTypeK: string;
  kvTypeV: string;
  slots: string;
  specMode: string;
  specDraftMax: string;
  allowCpuOffload: boolean;
}

export const EMPTY_PROFILE: LaunchProfileDraft = {
  ctx: '',
  kvTypeK: '',
  kvTypeV: '',
  slots: '',
  specMode: '',
  specDraftMax: '',
  allowCpuOffload: false,
};

/** KV cache dtypes the ESTIMATOR can price, as OPTION VALUES.
 *
 *  '' means "send no -ctk/-ctv flag", which llama.cpp defaults to f16 — so it renders as
 *  "f16 (default)" and there is deliberately NO separate explicit 'f16' entry. Offering both put two
 *  options in the list that produce an identical cache dtype, differing only in whether a redundant
 *  flag is emitted. A browser check caught it ("f16 (default)" sitting directly above "f16"); the
 *  unit test could not, because it only asserted the value list was correct, never that the list made
 *  sense to read.
 *
 *  tq3_0 is absent even though the CUDA fork advertises it: an unpriced footprint must never be
 *  admitted, so the API rejects it too. */
const KV_TYPES = ['', 'q8_0', 'q4_0'];

/** Speculative modes VALIDATED on this box. b10106 also advertises draft-simple/draft-eagle3/
 *  draft-dflash and several ngram variants, but an unvalidated mode is not offered — the value set is
 *  pinned to what has been tested (PLAN §7). */
const SPEC_MODES = [
  { value: '', label: 'off' },
  { value: 'ngram-cache', label: 'ngram-cache (draft-free)' },
  { value: 'draft-mtp', label: 'draft-mtp (embedded MTP head)' },
];

function numOrUndef(s: string): number | undefined {
  const t = s.trim();
  if (t === '') return undefined;
  const n = Number(t);
  return Number.isFinite(n) ? n : undefined;
}

/**
 * profileToRequest projects the draft onto the fields of a start request, OMITTING everything the
 * operator left blank. Omission matters: a blank field must mean "the daemon default", not "0" — the
 * server treats 0/"" as unset, and sending an explicit 0 for slots would be indistinguishable from
 * asking for it.
 *
 * `allow_cpu_offload` is intentionally NOT included: it stays owned by the existing shared toggle in
 * InstancesCard so there is exactly one control for it, not two that can disagree.
 */
export function profileToRequest(
  p: LaunchProfileDraft,
): Partial<StartInstanceRequest> {
  const req: Partial<StartInstanceRequest> = {};
  const ctx = numOrUndef(p.ctx);
  if (ctx !== undefined) req.ctx = ctx;
  if (p.kvTypeK) req.kv_type_k = p.kvTypeK;
  if (p.kvTypeV) req.kv_type_v = p.kvTypeV;
  const slots = numOrUndef(p.slots);
  if (slots !== undefined) req.slots = slots;
  if (p.specMode) req.speculative = p.specMode;
  const draftMax = numOrUndef(p.specDraftMax);
  if (draftMax !== undefined) req.spec_draft_max = draftMax;
  return req;
}

/** profileFromDefault maps a SAVED default back into form state (the pre-fill direction). */
export function profileFromDefault(d: ModelLaunchDefault): LaunchProfileDraft {
  return {
    ctx: d.ctx ? String(d.ctx) : '',
    kvTypeK: d.kv_type_k ?? '',
    kvTypeV: d.kv_type_v ?? '',
    slots: d.slots ? String(d.slots) : '',
    specMode: d.spec_mode ?? '',
    specDraftMax: d.spec_draft_max ? String(d.spec_draft_max) : '',
    allowCpuOffload: d.allow_cpu_offload ?? false,
  };
}

/** summarize renders the armed profile compactly for the collapsed disclosure, so a non-default
 *  profile is never silently in effect. Returns '' when nothing is set. */
export function summarize(p: LaunchProfileDraft): string {
  const parts: string[] = [];
  if (p.ctx) parts.push(`ctx ${p.ctx}`);
  if (p.kvTypeK || p.kvTypeV) {
    parts.push(`kv ${p.kvTypeK || 'f16'}/${p.kvTypeV || 'f16'}`);
  }
  if (p.slots) parts.push(`-np ${p.slots}`);
  if (p.specMode) {
    parts.push(p.specDraftMax ? `${p.specMode} n=${p.specDraftMax}` : p.specMode);
  }
  return parts.join(' · ');
}

/**
 * AdvancedStartForm is the per-start launch-profile editor (M2.3) plus the saved-default controls.
 *
 * It exists because a full MTP profile was previously unreachable from any UI or API caller: `kv_quant`
 * is a single bool meaning q4_0 on BOTH sides, and there was no way to express slots, the speculative
 * mode, or the draft maximum at all (KNOWN-LIMITATIONS #21).
 *
 * The Qwen3.6-27B profile PROVEN at 64K on this box is `-ctk q4_0 -ctv q4_0 --spec-type draft-mtp
 * --spec-draft-n-max 2 -np 1 -ngl 99` (scripts/run-qwen-64k-q4q4.ps1). Its KV pair is SYMMETRIC, so
 * `kv_quant` alone could have expressed that much of it — but not the speculative mode or slot count.
 * An asymmetric pair such as q8_0/q4_0 is a valid alternative this form also makes reachable.
 *
 * The saved default is ADVISORY: "Load saved" pre-fills the form and the operator sees exactly what
 * will be sent. Nothing here changes a start the operator did not confirm (M6.1).
 */
export function AdvancedStartForm(props: {
  profile: LaunchProfileDraft;
  onChange: (p: LaunchProfileDraft) => void;
  /** The model the saved-default actions apply to. When null (no model selected) those actions are
   *  disabled — a default is per-model, so there is nothing to load or save against. */
  modelId: string | null;
}) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<string | null>(null);
  const [stale, setStale] = useState<string | null>(null);

  const { profile, onChange, modelId } = props;
  const set = useCallback(
    <K extends keyof LaunchProfileDraft>(k: K, v: LaunchProfileDraft[K]) => {
      onChange({ ...profile, [k]: v });
    },
    [profile, onChange],
  );

  const onLoadSaved = useCallback(async () => {
    if (!modelId) return;
    setBusy(true);
    setNote(null);
    setStale(null);
    try {
      const d = await api.modelLaunchDefault(modelId);
      onChange(profileFromDefault(d));
      setNote(
        d.validated_note
          ? `Loaded saved profile: ${d.validated_note}`
          : 'Loaded saved profile.',
      );
      // A profile validated on a DIFFERENT engine build is a stale claim, not a recommendation.
      if (d.stale_reason) setStale(d.stale_reason);
    } catch (e) {
      // 404 is the normal "nothing saved yet" case, not a failure worth alarming about.
      setNote(
        e instanceof ApiError && e.status === 404
          ? 'No saved profile for this model yet.'
          : `Could not load the saved profile: ${e instanceof ApiError ? e.reason : String(e)}`,
      );
    } finally {
      setBusy(false);
    }
  }, [modelId, onChange]);

  const onSaveDefault = useCallback(async () => {
    if (!modelId) return;
    setBusy(true);
    setNote(null);
    setStale(null);
    try {
      const req = profileToRequest(profile);
      const saved = await api.saveModelLaunchDefault(modelId, {
        ctx: req.ctx,
        kv_type_k: req.kv_type_k,
        kv_type_v: req.kv_type_v,
        slots: req.slots,
        spec_mode: req.speculative,
        spec_draft_max: req.spec_draft_max,
        allow_cpu_offload: profile.allowCpuOffload,
      });
      setNote(`Saved as the default profile for ${saved.model_id}.`);
      if (saved.stale_reason) setStale(saved.stale_reason);
    } catch (e) {
      // The server validates through the same path a start uses, so the reason names the valid set.
      setNote(`Could not save: ${e instanceof ApiError ? e.reason : String(e)}`);
    } finally {
      setBusy(false);
    }
  }, [modelId, profile]);

  const onClear = useCallback(() => {
    onChange(EMPTY_PROFILE);
    setNote(null);
    setStale(null);
  }, [onChange]);

  const summary = summarize(profile);

  return (
    <div className={styles.wrap}>
      <button
        type="button"
        className={styles.toggle}
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
      >
        {open ? '▾' : '▸'} Advanced launch profile
        {!open && summary ? (
          <span className={styles.summary}>{summary}</span>
        ) : null}
      </button>

      {open && (
        <div className={styles.body}>
          <div className={styles.grid}>
            <label className={styles.field}>
              <span className={styles.label}>Context (-c)</span>
              <input
                className={styles.input}
                type="number"
                min={0}
                inputMode="numeric"
                placeholder="daemon default"
                value={profile.ctx}
                onChange={(e) => set('ctx', e.target.value)}
              />
            </label>

            <label className={styles.field}>
              <span className={styles.label}>K cache (-ctk)</span>
              <select
                className={styles.select}
                value={profile.kvTypeK}
                onChange={(e) => set('kvTypeK', e.target.value)}
              >
                {KV_TYPES.map((t) => (
                  <option key={t || 'default'} value={t}>
                    {t || 'f16 (default)'}
                  </option>
                ))}
              </select>
            </label>

            <label className={styles.field}>
              <span className={styles.label}>V cache (-ctv)</span>
              <select
                className={styles.select}
                value={profile.kvTypeV}
                onChange={(e) => set('kvTypeV', e.target.value)}
              >
                {KV_TYPES.map((t) => (
                  <option key={t || 'default'} value={t}>
                    {t || 'f16 (default)'}
                  </option>
                ))}
              </select>
            </label>

            <label className={styles.field}>
              <span className={styles.label}>Slots (-np)</span>
              {/* aria-label is explicit because this field carries a hint span: the wrapping
                  label's accessible name would otherwise include the hint text. */}
              <input
                className={styles.input}
                type="number"
                min={1}
                inputMode="numeric"
                placeholder="1"
                aria-label="Slots (-np)"
                value={profile.slots}
                onChange={(e) => set('slots', e.target.value)}
              />
              <span className={styles.hint}>each slot multiplies the KV cache</span>
            </label>

            <label className={styles.field}>
              <span className={styles.label}>Speculative</span>
              <select
                className={styles.select}
                value={profile.specMode}
                onChange={(e) => set('specMode', e.target.value)}
              >
                {SPEC_MODES.map((m) => (
                  <option key={m.value || 'off'} value={m.value}>
                    {m.label}
                  </option>
                ))}
              </select>
            </label>

            <label className={styles.field}>
              <span className={styles.label}>Draft max</span>
              <input
                className={styles.input}
                type="number"
                min={0}
                inputMode="numeric"
                placeholder="engine default"
                aria-label="Draft max"
                value={profile.specDraftMax}
                disabled={!profile.specMode}
                onChange={(e) => set('specDraftMax', e.target.value)}
              />
              <span className={styles.hint}>
                --spec-draft-n-max; needs a speculative mode
              </span>
            </label>
          </div>

          <div className={styles.actions}>
            <button
              type="button"
              className={styles.actionBtn}
              disabled={busy || !modelId}
              onClick={() => void onLoadSaved()}
            >
              Load saved
            </button>
            <button
              type="button"
              className={styles.actionBtn}
              disabled={busy || !modelId}
              onClick={() => void onSaveDefault()}
            >
              Save as default
            </button>
            <button
              type="button"
              className={styles.actionBtn}
              disabled={busy}
              onClick={onClear}
            >
              Clear
            </button>
          </div>

          {note && <div className={styles.note}>{note}</div>}
          {stale && <div className={styles.stale}>⚠ {stale}</div>}
        </div>
      )}
    </div>
  );
}
