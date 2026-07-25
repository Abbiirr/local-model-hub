import type { Tone } from './ui';
import { Badge } from './ui';

// Source badge (ollama / gguf / hf). Note: `source` in the models row is the
// provenance (ollama | huggingface | local_path), while `format` is gguf |
// safetensors | ... . The spec's "source badge (ollama/gguf/hf)" blends these:
// we show a provenance-derived label, mapping local_path GGUFs to a "gguf" badge
// and huggingface to "hf".
export function SourceBadge(props: { source: string; format: string }) {
  const { label, tone } = classify(props.source, props.format);
  return <Badge tone={tone}>{label}</Badge>;
}

function classify(source: string, format: string): { label: string; tone: Tone } {
  switch (source) {
    case 'ollama':
      return { label: 'ollama', tone: 'accent' };
    case 'huggingface':
      return { label: 'hf', tone: 'ok' };
    case 'local_path':
    case 'local_import':
      // local files are usually GGUFs on this box; label by format when useful.
      if (format === 'gguf') return { label: 'gguf', tone: 'neutral' };
      if (format === 'safetensors') return { label: 'local·st', tone: 'neutral' };
      return { label: 'local', tone: 'neutral' };
    case 'direct_url':
      return { label: 'url', tone: 'neutral' };
    default:
      return { label: source || format || 'unknown', tone: 'neutral' };
  }
}

// Presence badge: driven by the daemon's dedicated `presence` field
// (core.ModelSummary.Presence = present | missing). Default = present.
export function PresenceBadge(props: { present: boolean }) {
  return props.present ? (
    <Badge tone="ok" dot>
      present
    </Badge>
  ) : (
    <Badge tone="crit" dot>
      missing
    </Badge>
  );
}
