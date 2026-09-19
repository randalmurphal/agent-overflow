import type { ModelInfo } from '../types/settings';

interface Selection {
  provider?: string;
  model: string;
  reasoningEffort?: string;
  fastMode?: boolean;
}

// Catalogs describe current advertised choices. They cannot revoke a stored
// selection or prove whether an execution request will succeed.

/**
 * The parts of a selection the catalog does not list: the model slug itself,
 * or the effort and fast-mode choices the listed model lacks. Empty when the
 * catalog supports every part.
 */
export function catalogWithdrawals(selection: Selection, models: ModelInfo[]): string[] {
  const model = models.find((candidate) => candidate.slug === (selection.provider === 'claude' ? selection.model.replace(/\[1m\]$/, '') : selection.model));
  if (!model) return [selection.model];
  const withdrawn: string[] = [];
  if (selection.reasoningEffort && (selection.provider === 'codex' || model.reasoningEfforts?.length) && !model.reasoningEfforts?.some((effort) => effort.slug === selection.reasoningEffort)) {
    withdrawn.push(`${selection.reasoningEffort} effort`);
  }
  if (selection.fastMode && !model.capabilities?.includes('fast_mode')) withdrawn.push('fast mode');
  return withdrawn;
}

/** Describes withdrawals reported by catalogWithdrawals for the same selection. */
export function describeWithdrawals(selection: Selection, withdrawn: string[]): string {
  if (withdrawn.includes(selection.model)) return `The refreshed catalog no longer lists ${selection.model}.`;
  return `The refreshed catalog no longer lists ${withdrawn.join(' and ')} for ${selection.model}.`;
}
