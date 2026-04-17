// Pure helper for the active-tools work-group chip. Given the live set of
// running tools, produce the label text and the ordered, de-duplicated list of
// tool names the chip should expose. The component decides whether to render a
// chip (count >= 2) or a single WorkEntry (count === 1).

import type { WorkEntryData } from '../types/models';

export interface ActiveToolsSummary {
  count: number;
  names: string[];
  label: string;
}

/**
 * Build the chip summary from a list of live tool entries. The label is
 * "Running N tools — A, B, C" where N is the total in-flight count and the
 * trailing list is the first three unique tool names in appearance order. If
 * there are more distinct names than the cap, an ellipsis is appended.
 */
export function summarizeActiveTools(
  entries: readonly WorkEntryData[],
  maxNames = 3,
): ActiveToolsSummary {
  const count = entries.length;
  const names: string[] = [];
  const seen = new Set<string>();
  for (const entry of entries) {
    const raw = entry.name ?? entry.type;
    const display = formatToolName(raw);
    if (display.length === 0) continue;
    if (seen.has(display)) continue;
    seen.add(display);
    names.push(display);
  }

  if (count === 0) {
    return { count: 0, names, label: '' };
  }

  const displayedNames = names.slice(0, maxNames);
  const truncated = names.length > maxNames;
  const noun = count === 1 ? 'tool' : 'tools';
  const nameSuffix = displayedNames.length > 0
    ? ` — ${displayedNames.join(', ')}${truncated ? ', ...' : ''}`
    : '';
  return {
    count,
    names,
    label: `Running ${count} ${noun}${nameSuffix}`,
  };
}

/**
 * Normalize a provider tool name for display. Preserves the original casing
 * for recognisable names like "Read" or "Bash", lowercases all-uppercase
 * tokens (which would otherwise look shouty in the chip), and trims
 * surrounding whitespace.
 */
function formatToolName(raw: string): string {
  const trimmed = raw.trim();
  if (trimmed.length === 0) return '';
  return trimmed;
}
