import { useCallback, useEffect, useRef, useState } from 'react';
import type { DownloadJob, DownloadRequest } from '../api/types';
import { api, ApiError } from '../api/client';
import { Panel, IconButton, ErrorState, Reason, Bar, Badge } from './ui';
import styles from './DownloadsCard.module.css';

// The pickle / trust_remote_code warnings that get the LOUD critical styling (spec §7 UX:
// "dangerous actions requiring explicit confirmation" — a pickle/trust_remote_code model is a
// code-execution risk). Everything else (missing license, gated, local-import origin) is a
// plain warning.
function isCriticalWarning(w: string): boolean {
  const low = w.toLowerCase();
  return low.includes('pickle') || low.includes('trust_remote_code');
}

type Kind = 'hf' | 'gguf_url' | 'ollama' | 'local_import';

const KIND_LABELS: Record<Kind, string> = {
  hf: 'HF repo',
  gguf_url: 'GGUF URL',
  ollama: 'Ollama pull',
  local_import: 'Local import',
};

// DownloadsCard drives the Phase-3 supply chain from the dashboard: the two-step HF flow
// (resolve → show commit + warnings → confirm), single-GGUF URL, Ollama pull, and local
// import. It polls the active job's status for live progress and renders the linked manifest's
// warnings + a reproduce button.
export function DownloadsCard() {
  const [kind, setKind] = useState<Kind>('hf');
  const [repo, setRepo] = useState('');
  const [ref, setRef] = useState('');
  const [file, setFile] = useState('');
  const [url, setUrl] = useState('');
  const [name, setName] = useState('');
  const [path, setPath] = useState('');

  const [job, setJob] = useState<DownloadJob | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [reproduce, setReproduce] = useState<string | null>(null);

  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const stopPolling = useCallback(() => {
    if (pollRef.current) {
      clearInterval(pollRef.current);
      pollRef.current = null;
    }
  }, []);

  // Poll the active job for progress until it reaches a terminal state.
  useEffect(() => {
    if (!job) return;
    const terminal =
      job.state === 'succeeded' ||
      job.state === 'failed' ||
      job.state === 'awaiting_ok';
    if (terminal) {
      stopPolling();
      return;
    }
    if (!pollRef.current) {
      pollRef.current = setInterval(async () => {
        try {
          const j = await api.downloadStatus(job.id);
          setJob(j);
        } catch {
          /* transient; keep the last snapshot */
        }
      }, 700);
    }
    return stopPolling;
  }, [job, stopPolling]);

  async function submit(confirm: boolean) {
    setError(null);
    setReproduce(null);
    const req: DownloadRequest = { kind };
    if (kind === 'hf') {
      if (!repo.trim()) {
        setError('Enter a Hugging Face repo (owner/name).');
        return;
      }
      req.repo = repo.trim();
      if (ref.trim()) req.ref = ref.trim();
      if (file.trim()) req.files = [file.trim()];
      req.confirm = confirm;
    } else if (kind === 'gguf_url') {
      if (!url.trim()) {
        setError('Enter a direct .gguf URL.');
        return;
      }
      req.url = url.trim();
    } else if (kind === 'ollama') {
      if (!name.trim()) {
        setError('Enter an Ollama model name (e.g. qwen2.5:0.5b).');
        return;
      }
      req.name = name.trim();
    } else if (kind === 'local_import') {
      if (!path.trim()) {
        setError('Enter the absolute path of an existing model file.');
        return;
      }
      req.path = path.trim();
    }
    setBusy(true);
    try {
      const j = await api.createDownload(req);
      setJob(j);
    } catch (e) {
      setError(e instanceof ApiError ? e.reason : String(e));
    } finally {
      setBusy(false);
    }
  }

  async function showReproduce() {
    if (!job?.model_id) return;
    try {
      const b = await api.reproduce(job.model_id);
      setReproduce(JSON.stringify(b, null, 2));
    } catch (e) {
      setError(e instanceof ApiError ? e.reason : String(e));
    }
  }

  const resolved = job?.resolved;
  const progress = job?.progress;
  const isAwaiting = job?.state === 'awaiting_ok';
  const isRunning =
    job?.state === 'running' || job?.state === 'hashing';
  const isDone = job?.state === 'succeeded';
  const isFailed = job?.state === 'failed';

  return (
    <Panel title="Download a model">
      <div className={styles.kindTabs}>
        {(Object.keys(KIND_LABELS) as Kind[]).map((k) => (
          <button
            key={k}
            type="button"
            className={`${styles.kindTab} ${kind === k ? styles.kindTabActive : ''}`}
            onClick={() => {
              setKind(k);
              setJob(null);
              setError(null);
              setReproduce(null);
            }}
          >
            {KIND_LABELS[k]}
          </button>
        ))}
      </div>

      <form
        className={styles.form}
        onSubmit={(e) => {
          e.preventDefault();
          submit(false);
        }}
      >
        {kind === 'hf' && (
          <>
            <label className={styles.field}>
              <span className={styles.label}>HF repo</span>
              <input
                className={styles.input}
                placeholder="e.g. Qwen/Qwen2.5-0.5B-Instruct-GGUF"
                value={repo}
                onChange={(e) => setRepo(e.target.value)}
                spellCheck={false}
                autoCapitalize="off"
              />
            </label>
            <label className={styles.field}>
              <span className={styles.label}>
                Ref <span className={styles.optional}>(branch/tag/sha; blank = main)</span>
              </span>
              <input
                className={styles.input}
                placeholder="main"
                value={ref}
                onChange={(e) => setRef(e.target.value)}
                spellCheck={false}
              />
            </label>
            <label className={styles.field}>
              <span className={styles.label}>
                File <span className={styles.optional}>(one GGUF; blank = default set)</span>
              </span>
              <input
                className={styles.input}
                placeholder="e.g. qwen2.5-0.5b-instruct-q4_k_m.gguf"
                value={file}
                onChange={(e) => setFile(e.target.value)}
                spellCheck={false}
              />
            </label>
          </>
        )}
        {kind === 'gguf_url' && (
          <label className={styles.field}>
            <span className={styles.label}>Direct .gguf URL</span>
            <input
              className={styles.input}
              placeholder="https://…/model.gguf"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              spellCheck={false}
            />
          </label>
        )}
        {kind === 'ollama' && (
          <label className={styles.field}>
            <span className={styles.label}>Ollama model name</span>
            <input
              className={styles.input}
              placeholder="qwen2.5:0.5b"
              value={name}
              onChange={(e) => setName(e.target.value)}
              spellCheck={false}
            />
          </label>
        )}
        {kind === 'local_import' && (
          <label className={styles.field}>
            <span className={styles.label}>Absolute path (must be inside a model dir)</span>
            <input
              className={styles.input}
              placeholder="K:\\projects\\llm_models\\models\\model.gguf"
              value={path}
              onChange={(e) => setPath(e.target.value)}
              spellCheck={false}
            />
          </label>
        )}

        <IconButton
          onClick={() => submit(false)}
          disabled={busy}
          title={kind === 'hf' ? 'Resolve the commit and show what will be downloaded' : 'Start'}
        >
          {busy ? 'Working…' : kind === 'hf' ? 'Resolve commit' : 'Start'}
        </IconButton>
      </form>

      <Reason>
        {kind === 'hf'
          ? 'HF downloads pin the exact commit SHA before any bytes are fetched. You will see the commit, files, and warnings, then confirm.'
          : kind === 'local_import'
            ? 'Registers an existing file with a manifest. Origin is unverifiable; the sha256 is computed in the background.'
            : 'Downloads with an immutable manifest and per-file sha256.'}
      </Reason>

      {error && <ErrorState>{error}</ErrorState>}

      {/* Two-step gate: show the resolved commit + files + warnings, then confirm. */}
      {isAwaiting && resolved && (
        <div className={styles.result}>
          <div className={styles.commit}>
            commit {resolved.commit.slice(0, 12)}… @ {resolved.repo}
          </div>
          {resolved.license && (
            <div className={styles.label}>license: {resolved.license}</div>
          )}
          <div className={styles.fileList}>
            {resolved.files.map((f) => (
              <div
                key={f.path}
                className={`${styles.fileRow} ${f.selected ? styles.fileRowSelected : ''}`}
              >
                <span>{f.selected ? '☑ ' : '☐ '}{f.path}</span>
                <span>{f.size_bytes ? mib(f.size_bytes) : ''}</span>
              </div>
            ))}
          </div>
          <WarningList warnings={resolved.warnings} />
          <div className={styles.actions}>
            <IconButton
              onClick={() => submit(true)}
              disabled={busy}
              title="Download the selected files at this commit"
            >
              Confirm download ({mib(resolved.total_size_bytes)})
            </IconButton>
          </div>
        </div>
      )}

      {isRunning && (
        <div className={styles.progressWrap}>
          <Bar
            fraction={
              progress && progress.bytes_total > 0
                ? progress.bytes_done / progress.bytes_total
                : 0
            }
            level="ok"
            label="download progress"
          />
          <div className={styles.progressMeta}>
            <span>
              {job?.state === 'hashing' ? 'hashing' : progress?.phase ?? 'starting'}
              {progress?.resumed ? ' (resumed)' : ''}
              {progress && progress.file_count > 1
                ? ` · file ${progress.file_index}/${progress.file_count}`
                : ''}
            </span>
            <span>
              {progress
                ? `${mib(progress.bytes_done)}${
                    progress.bytes_total ? ` / ${mib(progress.bytes_total)}` : ''
                  }`
                : ''}
            </span>
          </div>
          <div className={styles.label}>{progress?.file}</div>
        </div>
      )}

      {isDone && (
        <div className={styles.result}>
          <Badge tone="ok" dot>
            done
          </Badge>{' '}
          <span className={styles.label}>{job?.reason}</span>
          <div className={styles.actions}>
            {job?.model_id && (
              <IconButton onClick={showReproduce} title="Show the reproduce export JSON">
                Reproduce
              </IconButton>
            )}
          </div>
          {reproduce && <div className={styles.reproduceBox}>{reproduce}</div>}
        </div>
      )}

      {isFailed && <ErrorState>{job?.reason}</ErrorState>}
    </Panel>
  );
}

function WarningList(props: { warnings: string[] }) {
  if (!props.warnings || props.warnings.length === 0) return null;
  return (
    <div className={styles.warnings}>
      {props.warnings.map((w, i) => (
        <div
          key={i}
          className={isCriticalWarning(w) ? styles.warnPickle : styles.warnPlain}
        >
          {isCriticalWarning(w) ? '⚠ ' : ''}
          {w}
        </div>
      ))}
    </div>
  );
}

function mib(bytes: number): string {
  if (!bytes) return '0 MiB';
  const m = bytes / (1024 * 1024);
  if (m >= 1024) return `${(m / 1024).toFixed(2)} GiB`;
  return `${m.toFixed(1)} MiB`;
}
