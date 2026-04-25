// Pure helpers for the per-turn changed-files tree and "+N −M · K files" badge
// rendered at the end of each turn in the message timeline. The helpers parse
// item.payloadMeta defensively and drop malformed diff entries.

import type { ChangedFile, DiffMeta, Item, ToolResultMeta } from '../types/models';

export interface TurnDiffSummary {
  insertions: number;
  deletions: number;
  fileCount: number;
}

/**
 * Combined per-turn diff view. Bundles the ChangedFile list (for
 * ChangedFilesTree) with the aggregate summary (for TurnDiffBadge) so
 * callers compute both in a single pass over the turn's items. Null when
 * the turn has no diff activity — map presence is the gate for rendering.
 */
export interface TurnDiffView {
  files: ChangedFile[];
  summary: TurnDiffSummary;
}

export const EMPTY_TURN_DIFF_SUMMARY: TurnDiffSummary = {
  insertions: 0,
  deletions: 0,
  fileCount: 0,
};

function isFiniteNonNegativeNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0;
}

function isNonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.trim().length > 0;
}

function isChangeKind(value: unknown): value is DiffMeta['changeKind'] {
  return value === 'added'
    || value === 'modified'
    || value === 'deleted'
    || value === 'renamed';
}

function normalizeChangedFile(file: {
  path: unknown;
  insertions: unknown;
  deletions: unknown;
  kind: unknown;
  payloadId: string;
}): ChangedFile | null {
  if (!isNonEmptyString(file.path)) return null;
  if (!isFiniteNonNegativeNumber(file.insertions)) return null;
  if (!isFiniteNonNegativeNumber(file.deletions)) return null;
  return {
    path: file.path,
    insertions: file.insertions,
    deletions: file.deletions,
    kind: isChangeKind(file.kind) ? file.kind : 'modified',
    payloadId: file.payloadId,
  };
}

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
    for (const file of changedFilesForItem(item)) {
      insertions += file.insertions;
      deletions += file.deletions;
      if (!seenPaths.has(file.path)) {
        seenPaths.add(file.path);
        fileCount += 1;
      }
    }
  }
  return { insertions, deletions, fileCount };
}

/**
 * Extract the ChangedFile list for a single item. A diff payload contributes
 * one entry; a tool_result payload with inlineDiff contributes one entry per
 * file in inlineDiff.files. Items with no payload/meta or malformed meta
 * produce no entries (failed parse silently drops — parity with the prior
 * inline helper in MessageTimeline).
 */
export function changedFilesForItem(item: Item): ChangedFile[] {
  if (!item.payloadMeta) return [];
  try {
    if (item.payloadKind === 'diff' && item.payloadId) {
      const meta = JSON.parse(item.payloadMeta) as DiffMeta;
      const file = normalizeChangedFile({
        path: meta.filePath,
        insertions: meta.insertions,
        deletions: meta.deletions,
        kind: meta.changeKind,
        payloadId: item.payloadId,
      });
      return file ? [file] : [];
    }
    if (item.payloadKind !== 'tool_result' || !item.payloadId) return [];
    const meta = JSON.parse(item.payloadMeta) as ToolResultMeta;
    return (meta.inlineDiff?.files ?? []).flatMap((file) => {
      const changedFile = normalizeChangedFile({
        path: file.path,
        insertions: file.insertions ?? 0,
        deletions: file.deletions ?? 0,
        kind: file.kind,
        payloadId: item.payloadId!,
      });
      return changedFile ? [changedFile] : [];
    });
  } catch {
    return [];
  }
}

export function itemMayAffectDiffView(item: Item): boolean {
  if (!item.payloadMeta) return false;
  return item.payloadKind === 'diff' || item.payloadKind === 'tool_result';
}

/**
 * Build a combined per-turn diff view in one pass over the turn's items.
 * Returns null when the turn has no diff-bearing payloads at all — callers
 * use null as "skip this turn entirely". A turn whose files list is
 * non-empty but whose summary has no line changes still returns a view:
 * the tree renders, the badge checks turnSummaryIsMeaningful.
 */
export function buildTurnDiffView(
  items: readonly Item[],
  turnIndex: number,
): TurnDiffView | null {
  return buildDiffViewForItems(items.filter((item) => item.turnIndex === turnIndex));
}

/**
 * Build a combined diff view from items that already belong to the same turn.
 * Pane state keeps an item-id index per visible turn, so hot-path upserts can
 * call this without scanning unrelated visible rows.
 */
export function buildDiffViewForItems(
  items: readonly Item[],
): TurnDiffView | null {
  const files: ChangedFile[] = [];
  let insertions = 0;
  let deletions = 0;
  let fileCount = 0;
  const seenPaths = new Set<string>();

  for (const item of items) {
    const itemFiles = changedFilesForItem(item);
    if (itemFiles.length === 0) continue;
    for (const file of itemFiles) {
      files.push(file);
      insertions += file.insertions;
      deletions += file.deletions;
      if (!seenPaths.has(file.path)) {
        seenPaths.add(file.path);
        fileCount += 1;
      }
    }
  }

  if (files.length === 0) return null;
  return { files, summary: { insertions, deletions, fileCount } };
}

/**
 * True when the badge should render. A turn with no diff items, or one whose
 * diffs have all-zero inserts/deletes, produces nothing.
 */
export function turnSummaryIsMeaningful(summary: TurnDiffSummary): boolean {
  if (summary.fileCount === 0) return false;
  return summary.insertions > 0 || summary.deletions > 0;
}
