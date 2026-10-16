// The reasoning-effort vocabulary as the effort picker shows it. Every
// surface that names an effort (the picker, its trigger, a subagent's
// model affix) reads its label here so one tier never renders two ways.

import type { ReasoningEffortOption } from '../types/settings';

/** Effort tiers a model with no catalog entry still offers. */
export const FALLBACK_EFFORTS: readonly ReasoningEffortOption[] = [
  { slug: 'low', label: 'Low' },
  { slug: 'medium', label: 'Medium' },
  { slug: 'high', label: 'High' },
  { slug: 'xhigh', label: 'xHigh' },
];

/**
 * Display label for an effort slug. `label` is the catalog option's own
 * label when one is known; xhigh is always "xHigh" whatever the catalog
 * spells it.
 */
export function effortLabel(slug: string, label = ''): string {
  if (slug === 'xhigh') return 'xHigh';
  if (label.trim() !== '') return label;
  if (slug === 'none') return 'None';
  if (slug === 'minimal') return 'Minimal';
  if (slug === '') return '';
  return slug[0].toUpperCase() + slug.slice(1);
}
