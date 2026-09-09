import type { ModelInfo } from '../types/settings';

interface Selection {
  provider?: string;
  model: string;
  reasoningEffort?: string;
  fastMode?: boolean;
}

// Catalogs describe current advertised choices. They cannot revoke a stored
// selection or prove whether an execution request will succeed.
export function catalogContradiction(selection: Selection, models: ModelInfo[]): string | null {
  const model = models.find((candidate) => candidate.slug === (selection.provider === 'claude' ? selection.model.replace(/\[1m\]$/, '') : selection.model));
  if (!model) return `The refreshed catalog no longer lists ${selection.model}.`;
  const unsupported: string[] = [];
  if (selection.reasoningEffort && (selection.provider === 'codex' || model.reasoningEfforts?.length) && !model.reasoningEfforts?.some((effort) => effort.slug === selection.reasoningEffort)) {
    unsupported.push(`${selection.reasoningEffort} effort`);
  }
  if (selection.fastMode && !model.capabilities?.includes('fast_mode')) unsupported.push('fast mode');
  return unsupported.length ? `The refreshed catalog no longer lists ${unsupported.join(' and ')} for ${selection.model}.` : null;
}
