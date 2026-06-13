import { PROVIDER_DEFINITIONS } from '../providers/catalog';
import type { ProviderID } from '../types/providers';

export type ModelProvider = ProviderID | string;

export function displayModelLabel(provider: ModelProvider, slug: string, name?: string): string {
  // claude-tui drives the same Claude models as the headless claude provider,
  // so it shares the friendly-label formatting (slug "claude-opus-4-8" →
  // "Opus 4.8") rather than falling through to the raw slug.
  if (
    provider === PROVIDER_DEFINITIONS.claude.id ||
    provider === PROVIDER_DEFINITIONS['claude-tui'].id
  ) {
    return displayClaudeModelLabel(slug, name);
  }
  return name?.trim() || slug;
}

function displayClaudeModelLabel(slug: string, name?: string): string {
  const trimmedName = name?.trim() ?? '';
  if (trimmedName !== '') {
    return trimmedName.replace(/^Claude\s+/i, '');
  }

  // Strip the wire prefix and any trailing release-stamp suffixes —
  // both `[1m]` (long-context tier marker) and `-YYYYMMDD` (point
  // release datestamp). Datestamps appear on canonical Claude model
  // ids (e.g. `claude-haiku-4-5-20251001`) and we want the version
  // number to read "Haiku 4.5", not "Haiku 4.5 20251001".
  const cleanedSlug = slug
    .trim()
    .replace(/^claude-/i, '')
    .replace(/\[[^\]]+\]$/, '')
    .replace(/-\d{8}$/, '');
  if (cleanedSlug === '') return slug;

  const parts = cleanedSlug.split('-').filter(Boolean);
  const family = parts.shift();
  if (!family) return slug;

  const versionParts: string[] = [];
  while (parts.length > 0 && /^\d+$/.test(parts[0])) {
    versionParts.push(parts.shift()!);
  }

  const label = [capitalizeWord(family)];
  if (versionParts.length > 0) label.push(versionParts.join('.'));
  if (parts.length > 0) label.push(...parts.map(capitalizeWord));
  return label.join(' ');
}

function capitalizeWord(word: string): string {
  if (word === '') return word;
  return word[0].toUpperCase() + word.slice(1);
}
