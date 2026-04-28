export type ModelProvider = 'claude' | 'codex' | string;

export function displayModelLabel(provider: ModelProvider, slug: string, name?: string): string {
  if (provider === 'claude') {
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
