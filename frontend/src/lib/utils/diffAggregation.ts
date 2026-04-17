// Pure helpers for computing the diff panel's "agent cumulative" source.
//
// Each Item in a thread may carry a payloadId pointing at a unified diff that
// the agent generated during the turn. The panel aggregates them by:
//   1) selecting items whose kind === 'diff' and whose payloadId is set,
//   2) walking them in itemIndex order (turnIndex-then-itemIndex, as the
//      persist layer already emits them in that order),
//   3) fetching each payload via a loader callback and concatenating.
//
// The fetch step lives outside this module so tests can exercise selection
// independently of the Wails binding. Callers pass a `load(payloadId)` hook.
// `aggregateAgentDiffs` memoizes by payloadId via the provided `cache` map so
// switching source tabs doesn't re-fetch payloads we already have.

import type { Item } from '../types/models';

export interface AgentDiffEntry {
  /** Stable identifier for the item that produced the diff. */
  itemId: string;
  /** Payload id to fetch through the loader. */
  payloadId: string;
  /** Conversation turn this diff belongs to (for grouping). */
  turnIndex: number;
  /** Per-turn ordering (0-based). */
  itemIndex: number;
}

/**
 * Pick diff-carrying items out of a mixed item list in their natural order.
 * Items without a payloadId are skipped — they cannot contribute to the
 * cumulative patch. Non-diff items are skipped.
 */
export function selectAgentDiffEntries(items: readonly Item[]): AgentDiffEntry[] {
  const out: AgentDiffEntry[] = [];
  for (const item of items) {
    if (item.kind !== 'diff') continue;
    if (!item.payloadId) continue;
    out.push({
      itemId: item.id,
      payloadId: item.payloadId,
      turnIndex: item.turnIndex,
      itemIndex: item.itemIndex,
    });
  }
  return out;
}

/**
 * Resolve every entry's payload text through `load` and concatenate the
 * results with `\n\n` separators. Uses `cache` to memoize individual
 * payloadId fetches; cache hits short-circuit the loader.
 *
 * Throws the first error the loader returns — the caller surfaces it as a
 * dismissible banner rather than silently dropping the whole view.
 */
export async function aggregateAgentDiffs(
  entries: readonly AgentDiffEntry[],
  load: (payloadId: string) => Promise<string>,
  cache: Map<string, string>,
): Promise<string> {
  if (entries.length === 0) return '';

  const texts: string[] = [];
  for (const entry of entries) {
    const cached = cache.get(entry.payloadId);
    if (cached !== undefined) {
      texts.push(cached);
      continue;
    }
    const text = await load(entry.payloadId);
    cache.set(entry.payloadId, text);
    texts.push(text);
  }
  return texts.join('\n\n');
}

/**
 * Total insertions/deletions across the selected entries, sourced from the
 * PayloadMeta for each payloadId if available. Returns zeros when metas are
 * missing — we don't want to parse diff text just to show a tab badge.
 */
export interface DiffStats {
  insertions: number;
  deletions: number;
  fileCount: number;
}

export function summarizeEntries(
  entries: readonly AgentDiffEntry[],
  metas: ReadonlyMap<string, { meta: string } | undefined>,
): DiffStats {
  let insertions = 0;
  let deletions = 0;
  let fileCount = 0;
  for (const entry of entries) {
    const meta = metas.get(entry.payloadId);
    if (!meta) continue;
    try {
      const parsed = JSON.parse(meta.meta) as {
        insertions?: number;
        deletions?: number;
      };
      insertions += parsed.insertions ?? 0;
      deletions += parsed.deletions ?? 0;
      fileCount += 1;
    } catch {
      // A broken meta shouldn't poison the entire summary — just skip it.
    }
  }
  return { insertions, deletions, fileCount };
}
