// The reasoning effort a subagent runs at, as its row, card and pane show
// it next to the model ("Opus 4.7 xHigh").
//
// Codex reports the child's resolved effort with its model, so its value
// is shown as is. Claude reports none: the launch records what it asked
// for (claudeSubagentLabel.ts#deriveClaudeSubagentEffort), and the
// subagent's model decides what that becomes. Claude Code drops the
// effort for a model with no effort tiers and steps an xhigh or max the
// model does not offer down to high (2.1.300 effort resolution), so the
// model's catalog entry is applied here. A model the catalog does not
// list keeps the requested effort: silence is not a denial.

import { getProviderModels } from '../stores/providerModels.svelte';
import { threadMachine } from '../stores/attachedBackends.svelte';
import type { ModelInfo } from '../types/settings';
import { effortLabel } from './effortLabels';

/**
 * The catalog entry for a Claude model id as the CLI reports it: the bare
 * slug, with a context-tier marker (`[1m]`) or a release datestamp
 * (`-20251001`) the catalog does not carry.
 */
export function findClaudeCatalogModel(models: readonly ModelInfo[], model: string): ModelInfo | undefined {
  const id = model.trim();
  if (!id) return undefined;
  const untiered = id.replace(/\[[^\]]+\]$/, '');
  const candidates = [id, untiered, untiered.replace(/-\d{8}$/, '')];
  return models.find((candidate) => candidates.includes(candidate.slug));
}

/** The catalog entry for a Claude subagent's model on its thread's computer. */
export function claudeSubagentCatalogModel(
  threadId: string,
  projectId: string | null | undefined,
  model: string,
): ModelInfo | undefined {
  if (!model.trim()) return undefined;
  return findClaudeCatalogModel(getProviderModels('claude', threadMachine(threadId, projectId)), model);
}

/**
 * Display label for the effort a subagent runs at; '' when there is none
 * to show. `model` is the subagent model's Claude catalog entry, when known.
 */
export function subagentEffortLabel(
  provider: string,
  effort: string,
  model: ModelInfo | undefined,
): string {
  const slug = effort.trim();
  if (!slug) return '';
  if (provider !== 'claude' || !model) return effortLabel(slug);
  const tiers = model.reasoningEfforts ?? [];
  const runs = tiers.find((option) => option.slug === slug)
    ?? (slug === 'xhigh' || slug === 'max' ? tiers.find((option) => option.slug === 'high') : undefined);
  return runs ? effortLabel(runs.slug, runs.label) : '';
}

/** "Opus 4.7 xHigh": the model label and the effort label, either alone. */
export function modelEffortAffix(modelLabel: string, effort: string): string {
  return [modelLabel, effort].filter(Boolean).join(' ');
}
