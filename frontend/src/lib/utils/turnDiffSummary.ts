// Pure helper for the per-turn "+N −M · K files" badge rendered at the end of
// each turn in the message timeline. Takes the items for a turn + a parser
// that returns DiffMeta for an item's payload id, and produces the aggregate
// line counts and file count.

import type { DiffMeta, Item, ToolResultMeta } from '../types/models';

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
 * Aggregate the insertions/deletions/file-count for every diff-bearing item in
 * a turn. File-change tool results contribute through `payloadKind=tool_result`
 * and their `inlineDiff.files` metadata; raw diff payloads contribute through
 * `payloadKind=diff`.
 */
export function summarizeTurnDiffs(
  items: readonly Item[],
  turnIndex: number,
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
    if (!item.payloadMeta) continue;
    try {
      if (item.payloadKind === 'diff') {
        const meta = JSON.parse(item.payloadMeta) as DiffMeta;
        insertions += meta.insertions;
        deletions += meta.deletions;
        if (!seenPaths.has(meta.filePath)) {
          seenPaths.add(meta.filePath);
          fileCount += 1;
        }
        continue;
      }
      if (item.payloadKind !== 'tool_result') continue;
      const meta = JSON.parse(item.payloadMeta) as ToolResultMeta;
      const files = meta.inlineDiff?.files ?? [];
      for (const file of files) {
        insertions += file.insertions ?? 0;
        deletions += file.deletions ?? 0;
        if (!seenPaths.has(file.path)) {
          seenPaths.add(file.path);
          fileCount += 1;
        }
      }
    } catch {
      // Ignore malformed payload metadata.
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
