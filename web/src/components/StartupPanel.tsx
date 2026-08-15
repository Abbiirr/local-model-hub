import { useEffect, useMemo, useState } from 'react';
import { api, ApiError } from '../api/client';
import type { ModelSummary, StartupConfig, StartupModelConfig } from '../api/types';
import { ErrorState, Panel, Skeleton } from './ui';
import styles from './StartupPanel.module.css';

// Mirrors startupconfig.DefaultStartupConfig: start the control plane, load nothing, and never
// assume the operator wants last session's models back. Kept in sync with StartupConfigVersion.
const safeDefault: StartupConfig = {
  version: 2,
  start_localhub: true,
  open_web_ui: false,
  restore_last_session: false,
  startup_delay_seconds: 0,
  health_check_seconds: 60,
  models: [],
};

export function StartupPanel({ models }: { models: ModelSummary[] | null }) {
  const [config, setConfig] = useState<StartupConfig | null>(null);
  const [selected, setSelected] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    void api.startupConfig().then((loaded) => setConfig({ ...safeDefault, ...(loaded ?? {}), models: loaded?.models ?? [] })).catch((e) => {
      setError(e instanceof ApiError ? e.reason : String(e));
    });
  }, []);

  const available = useMemo(() => {
    const configured = new Set((config?.models ?? []).map((model) => model.model_id));
    return (models ?? []).filter((model) => !configured.has(model.id) && model.presence !== 'missing');
  }, [config, models]);

  const patchConfig = (patch: Partial<StartupConfig>) => {
    setConfig((current) => current ? { ...current, ...patch } : current);
    setNotice(null);
  };
  const patchModel = (index: number, patch: Partial<StartupModelConfig>) => {
    setConfig((current) => {
      if (!current) return current;
      const next = current.models.slice();
      next[index] = { ...next[index], ...patch };
      return { ...current, models: next };
    });
    setNotice(null);
  };
  const addModel = () => {
    if (!config || !selected) return;
    const model: StartupModelConfig = {
      model_id: selected,
      enabled: true,
      order: config.models.length,
      start_delay_seconds: 0,
      retry_policy: 'never',
      max_attempts: 0,
      use_saved_profile: false,
      mode: 'localhub_api',
    };
    patchConfig({ models: [...config.models, model] });
    setSelected('');
  };
  const move = (index: number, delta: number) => {
    if (!config) return;
    const target = index + delta;
    if (target < 0 || target >= config.models.length) return;
    const next = config.models.slice();
    [next[index], next[target]] = [next[target], next[index]];
    patchConfig({ models: next.map((model, order) => ({ ...model, order })) });
  };
  const remove = (index: number) => {
    if (!config) return;
    patchConfig({ models: config.models.filter((_, i) => i !== index).map((model, order) => ({ ...model, order })) });
  };
  const save = async () => {
    if (!config) return;
    setSaving(true);
    setError(null);
    setNotice(null);
    try {
      const saved = await api.saveStartupConfig(config);
      setConfig(saved);
      setNotice('Startup behavior saved. The tray app will apply it on its next health cycle and at login.');
    } catch (e) {
      setError(e instanceof ApiError ? e.reason : String(e));
    } finally {
      setSaving(false);
    }
  };

  return <Panel title="Startup behavior" actions={config && <button type="button" className={styles.save} disabled={saving} onClick={() => void save()}>{saving ? 'Saving…' : 'Save startup settings'}</button>}>
    {!config && !error && <Skeleton>Loading startup behavior…</Skeleton>}
    {error && <ErrorState>{error}</ErrorState>}
    {config && <div className={styles.body}>
      <p className={styles.intro}>Configure what the native tray app does at login. Windows launch itself is controlled from the tray’s <strong>Start with Windows</strong> item or Task Manager → Startup apps.</p>
      <div className={styles.globalGrid}>
        <label className={styles.check}><input type="checkbox" checked={config.start_localhub} onChange={(e) => patchConfig({ start_localhub: e.target.checked })} /> Start LocalHub</label>
        <label className={styles.check}><input type="checkbox" checked={config.open_web_ui} onChange={(e) => patchConfig({ open_web_ui: e.target.checked })} /> Open Web UI</label>
        <label className={styles.check}><input type="checkbox" checked={config.restore_last_session} onChange={(e) => patchConfig({ restore_last_session: e.target.checked })} /> Restore last session</label>
        <label>Login delay (seconds)<input type="number" min={0} max={3600} value={config.startup_delay_seconds} onChange={(e) => patchConfig({ startup_delay_seconds: Number(e.target.value) })} /></label>
        <label>Recovery check (seconds)<input type="number" min={15} max={3600} value={config.health_check_seconds} onChange={(e) => patchConfig({ health_check_seconds: Number(e.target.value) })} /></label>
      </div>

      <div className={styles.addRow}>
        <select aria-label="Model to add" value={selected} onChange={(e) => setSelected(e.target.value)}>
          <option value="">Select a model…</option>
          {available.map((model) => <option key={model.id} value={model.id}>{model.display_name || model.id}</option>)}
        </select>
        <button type="button" disabled={!selected} onClick={addModel}>Add to startup</button>
      </div>

      <div className={styles.models}>
        {config.models.length === 0 && <p className={styles.empty}>No models will be loaded automatically.</p>}
        {config.models.map((model, index) => <article className={styles.model} key={model.model_id}>
          <div className={styles.modelHead}>
            <label className={styles.check}><input type="checkbox" checked={model.enabled} onChange={(e) => patchModel(index, { enabled: e.target.checked })} /> <code>{model.model_id}</code></label>
            <div className={styles.order}>
              <button type="button" aria-label={`Move ${model.model_id} earlier`} disabled={index === 0} onClick={() => move(index, -1)}>↑</button>
              <button type="button" aria-label={`Move ${model.model_id} later`} disabled={index === config.models.length - 1} onClick={() => move(index, 1)}>↓</button>
              <button type="button" className={styles.remove} onClick={() => remove(index)}>Remove</button>
            </div>
          </div>
          <div className={styles.modelGrid}>
            <label>Mode<select value={model.mode} onChange={(e) => patchModel(index, { mode: e.target.value as StartupModelConfig['mode'] })}><option value="localhub_api">LocalHub API</option></select></label>
            <label>Retry<select value={model.retry_policy} onChange={(e) => { const policy = e.target.value as StartupModelConfig['retry_policy']; patchModel(index, { retry_policy: policy, max_attempts: policy === 'limited' ? Math.max(1, model.max_attempts || 3) : 0 }); }}><option value="never">Never</option><option value="limited">Limited</option><option value="keep_available">Keep available</option></select></label>
            {model.retry_policy === 'limited' && <label>Attempts<input type="number" min={1} max={100} value={model.max_attempts} onChange={(e) => patchModel(index, { max_attempts: Number(e.target.value) })} /></label>}
            <label>Load delay<input type="number" min={0} max={3600} value={model.start_delay_seconds} onChange={(e) => patchModel(index, { start_delay_seconds: Number(e.target.value) })} /></label>
            <label className={styles.check}><input type="checkbox" checked={model.use_saved_profile} onChange={(e) => patchModel(index, { use_saved_profile: e.target.checked })} /> Use saved profile</label>
          </div>
          <details className={styles.advanced}>
            <summary>Advanced launch overrides</summary>
            <div className={styles.advancedGrid}>
              <label>Engine<select value={model.engine ?? ''} onChange={(e) => patchModel(index, { engine: e.target.value as StartupModelConfig['engine'] })}><option value="">Automatic</option><option value="llamacpp">llama.cpp</option><option value="vllm">vLLM</option></select></label>
              <NumberField label="Context" value={model.ctx} onChange={(value) => patchModel(index, { ctx: value })} />
              <NumberField label="Batch" value={model.batch} onChange={(value) => patchModel(index, { batch: value })} />
              <NumberField label="Priority" value={model.priority} onChange={(value) => patchModel(index, { priority: value })} />
              <NumberField label="Slots" value={model.slots} onChange={(value) => patchModel(index, { slots: value })} />
              <label>K cache<select value={model.kv_type_k ?? ''} onChange={(e) => patchModel(index, { kv_type_k: e.target.value as StartupModelConfig['kv_type_k'] })}><option value="">Default</option><option value="f16">f16</option><option value="q8_0">q8_0</option><option value="q4_0">q4_0</option></select></label>
              <label>V cache<select value={model.kv_type_v ?? ''} onChange={(e) => patchModel(index, { kv_type_v: e.target.value as StartupModelConfig['kv_type_v'] })}><option value="">Default</option><option value="f16">f16</option><option value="q8_0">q8_0</option><option value="q4_0">q4_0</option></select></label>
              <label>Speculative<select value={model.speculative ?? ''} onChange={(e) => patchModel(index, { speculative: e.target.value as StartupModelConfig['speculative'] })}><option value="">Off</option><option value="ngram-cache">ngram cache</option><option value="draft-mtp">draft MTP</option></select></label>
              <NumberField label="Draft max" value={model.spec_draft_max} onChange={(value) => patchModel(index, { spec_draft_max: value })} />
              <label className={styles.check}><input type="checkbox" checked={model.pin ?? false} onChange={(e) => patchModel(index, { pin: e.target.checked })} /> Pin</label>
              <label className={styles.check}><input type="checkbox" checked={model.allow_cpu_offload ?? false} onChange={(e) => patchModel(index, { allow_cpu_offload: e.target.checked })} /> Allow CPU offload</label>
            </div>
          </details>
        </article>)}
      </div>
      {notice && <p className={styles.notice} role="status">{notice}</p>}
    </div>}
  </Panel>;
}

function NumberField({ label, value, onChange }: { label: string; value?: number; onChange: (value: number | undefined) => void }) {
  return <label>{label}<input type="number" value={value || ''} onChange={(e) => onChange(e.target.value === '' ? undefined : Number(e.target.value))} /></label>;
}
