import { useState } from 'react';
import type { CompatPreviewRequest, Verdict } from '../api/types';
import { api, ApiError } from '../api/client';
import { Panel, IconButton, ErrorState, Reason } from './ui';
import { CompatVerdictView } from './CompatVerdict';
import styles from './CompatCard.module.css';

// "Check remote model" card: preview a compatibility verdict for a Hugging Face repo WITHOUT
// downloading weights (the money endpoint, spec §3). A GGUF file field routes the remote-GGUF
// path (gguf-parser range reads); leaving it blank routes the remote-safetensors path
// (config.json + index.json fetched, metadata only).
export function CompatCard() {
  const [repo, setRepo] = useState('');
  const [file, setFile] = useState('');
  const [loading, setLoading] = useState(false);
  const [verdict, setVerdict] = useState<Verdict | null>(null);
  const [error, setError] = useState<string | null>(null);

  async function check() {
    const trimmedRepo = repo.trim();
    if (!trimmedRepo) {
      setError('Enter a Hugging Face repo (owner/name).');
      return;
    }
    setLoading(true);
    setError(null);
    setVerdict(null);
    const req: CompatPreviewRequest = { hf_repo: trimmedRepo };
    if (file.trim()) req.file = file.trim();
    try {
      const v = await api.compatPreview(req);
      setVerdict(v);
    } catch (e) {
      setError(e instanceof ApiError ? e.reason : String(e));
    } finally {
      setLoading(false);
    }
  }

  return (
    <Panel title="Check remote model">
      <form
        className={styles.form}
        onSubmit={(e) => {
          e.preventDefault();
          check();
        }}
      >
        <label className={styles.field}>
          <span className={styles.label}>HF repo</span>
          <input
            className={styles.input}
            name="compat-hf-repo"
            placeholder="e.g. bartowski/Llama-3.2-1B-Instruct-GGUF"
            value={repo}
            onChange={(e) => setRepo(e.target.value)}
            spellCheck={false}
            autoCapitalize="off"
          />
        </label>
        <label className={styles.field}>
          <span className={styles.label}>
            GGUF file <span className={styles.optional}>(blank = safetensors)</span>
          </span>
          <input
            className={styles.input}
            name="compat-gguf-file"
            placeholder="e.g. Llama-3.2-1B-Instruct-Q4_K_M.gguf"
            value={file}
            onChange={(e) => setFile(e.target.value)}
            spellCheck={false}
            autoCapitalize="off"
          />
        </label>
        <IconButton onClick={check} disabled={loading} title="Estimate fit without downloading">
          {loading ? 'Checking…' : 'Check fit'}
        </IconButton>
      </form>

      <Reason>
        Reads model metadata over the network (no weights) and estimates whether it fits on
        this box. GGUF files are range-read; safetensors config.json is fetched.
      </Reason>

      {error && <ErrorState>{error}</ErrorState>}
      {verdict && (
        <div className={styles.result}>
          <CompatVerdictView verdict={verdict} defaultOpen />
        </div>
      )}
    </Panel>
  );
}
