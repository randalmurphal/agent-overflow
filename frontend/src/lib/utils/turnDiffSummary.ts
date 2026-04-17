// Pure helper for the per-turn "+N −M · K files" badge rendered at the end of
// each turn in the message timeline. Takes the items for a turn + a parser
// that returns DiffMeta for an item's payload id, and produces the aggregate
// line counts and file count.

import type { DiffMeta, Item } from '../types/models';

export interface TurnDiffSummary {
  insertions: number;
  deletions: number;
  fileCount: number;
}

export const EMPTY_TURN_DIFF_SUMMARY: TurnDiffSummary = {
  insertions: 0,
  deletions: 0,
  fileCount: 0,
};

/**
 * Aggregate the insertions/deletions/file-count for every diff item in a turn.
 *
 * `items` may include non-diff items (we filter by `kind === 'diff'` and a
 * non-empty `payloadId`). The metaFor function must return the parsed DiffMeta
 * for a payloadId or `null` if the meta hasn't loaded yet — items with a
 * missing meta are skipped so a partially-hydrated thread still renders
 * something useful.
 */
export function summarizeTurnDiffs(
  items: readonly Item[],
  turnIndex: number,
  metaFor: (payloadId: string) => DiffMeta | null,
): TurnDiffSummary {
  let insertions = 0;
  let deletions = 0;
  let fileCount = 0;
  // Dedupe by file path so a turn that rewrites the same file multiple times
  // doesn't inflate the file count. The insertion/deletion totals still sum
  // every diff entry — they reflect per-operation line counts, not net change.
  const seenPaths = new Set<string>();
  for (const item of items) {
    if (item.turnIndex !== turnIndex) continue;
    if (item.kind !== 'diff') continue;
    if (!item.payloadId) continue;
    const meta = metaFor(item.payloadId);
    if (!meta) continue;
    insertions += meta.insertions;
    deletions += meta.deletions;
    if (!seenPaths.has(meta.filePath)) {
      seenPaths.add(meta.filePath);
      fileCount += 1;
    }
  }
  return { insertions, deletions, fileCount };
}

/**
 * True when the badge should render. A turn with no diff items, or one whose
 * diffs have all-zero inserts/deletes, produces nothing.
 */
export function turnSummaryIsMeaningful(summary: TurnDiffSummary): boolean {
  if (summary.fileCount === 0) return false;
  return summary.insertions > 0 || summary.deletions > 0;
}
