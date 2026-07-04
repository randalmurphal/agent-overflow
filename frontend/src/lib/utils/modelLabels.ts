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

/**
 * Display label for a model slug coming out of the usage ledger, where
 * the provider isn't carried alongside the slug. Claude slugs get the
 * same friendly form the model picker shows ("claude-fable-5" →
 * "Fable 5", release datestamps stripped); GPT slugs are title-cased to
 * match the Codex catalog names ("gpt-5.3-codex" → "GPT-5.3 Codex").
 * Anything else renders as-is — an unrecognized raw slug is better
 * than a wrong guess.
 *
 * A trailing context-tier marker survives as a suffix ("Sonnet 5 [1m]"):
 * Claude bills the extended-context tier as its own wire model
 * (claude-sonnet-5[1m]) and the ledger keeps them separate, so
 * stripping the marker would make the two rows read as duplicates.
 */
export function displayUsageModelLabel(slug: string): string {
  const trimmed = slug.trim();
  if (trimmed === '') return slug;
  const tierMatch = /\[([^\]]+)\]$/.exec(trimmed);
  const base = tierMatch ? trimmed.slice(0, tierMatch.index) : trimmed;
  const tierSuffix = tierMatch ? ` [${tierMatch[1]}]` : '';
  if (/^claude/i.test(base)) {
    return displayModelLabel(PROVIDER_DEFINITIONS.claude.id, base) + tierSuffix;
  }
  const gptMatch = /^gpt-([^-]+)(.*)$/i.exec(base);
  if (gptMatch) {
    const version = gptMatch[1];
    const rest = gptMatch[2].split('-').filter(Boolean).map(capitalizeWord);
    return [`GPT-${version}`, ...rest].join(' ') + tierSuffix;
  }
  return trimmed;
}

function capitalizeWord(word: string): string {
  if (word === '') return word;
  return word[0].toUpperCase() + word.slice(1);
}
