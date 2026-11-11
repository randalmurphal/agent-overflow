// Where a find match sits in the review diff's row model: the list row
// that shows it and the pixels from that row's top to the match's line,
// and the reading position a new query starts from. Pure functions over
// the row model; the diff body scrolls.

import { RowWalker, splitRowCount } from './patchRows';
import type { ReviewFile } from './patchStore';
import type { ReviewFindHit } from './reviewFindScan';
import { REVIEW_LINE_HEIGHT_PX, type LineBlockRow, type ReviewRowsResult } from './reviewRows';

export interface FindRowTarget {
  index: number;
  /** Pixels from the row's top to the match's line (exact with wrap off). */
  within: number;
}

/** The row showing `hit`, or null when the row model does not show it
 * (a collapsed file, a fold not yet opened). */
export function findHitRow(built: ReviewRowsResult, files: readonly ReviewFile[], hit: ReviewFindHit): FindRowTarget | null {
  const first = built.firstRowOfFile[hit.fileIndex] ?? -1;
  if (first < 0 || files[hit.fileIndex]?.path !== hit.path) return null;
  const place = hit.place;
  if (place.kind === 'header') return { index: first, within: 0 };
  if (place.kind === 'draft' || place.kind === 'thread') {
    const key = place.kind === 'draft' ? `t:${place.commentId}` : `pt:${place.threadId}`;
    const index = built.rowKeys.indexOf(key, first);
    return index >= 0 ? { index, within: 0 } : null;
  }
  if (place.kind !== 'line') return null;
  const file = files[hit.fileIndex];
  let block: { index: number; row: LineBlockRow } | null = null;
  for (let index = first + 1; index < built.rows.length && built.fileOfRow[index] === hit.fileIndex; index += 1) {
    const row = built.rows[index];
    if (row.kind !== 'line-block') continue;
    if (row.start.line > place.lineIndex) break;
    block = { index, row };
  }
  if (!block) return null;
  const walker = new RowWalker(file, block.row.start);
  let before = 0;
  while (before < block.row.count && walker.next()) {
    if (walker.lineIndex === place.lineIndex) break;
    before += 1;
  }
  const rows = block.row.splitCount === undefined ? before : splitRowCount(file, block.row.start, before);
  return { index: block.index, within: rows * REVIEW_LINE_HEIGHT_PX };
}

/** The (file, line) a row of the list starts at, in find-hit order. */
export function findRowPosition(built: ReviewRowsResult, index: number): { fileIndex: number; line: number } | null {
  const row = built.rows[index];
  if (!row) return null;
  if (row.kind === 'overview') return { fileIndex: -1, line: -2 };
  if (row.kind === 'file-header') return { fileIndex: row.fileIndex, line: -2 };
  if (row.kind === 'line-block') return { fileIndex: row.fileIndex, line: row.start.line };
  // An inserted row follows a line: the block above it holds that line.
  for (let at = index - 1; at >= 0 && built.fileOfRow[at] === row.fileIndex; at -= 1) {
    const above = built.rows[at];
    if (above.kind === 'line-block') return { fileIndex: row.fileIndex, line: above.start.line };
    if (above.kind === 'file-header') break;
  }
  return { fileIndex: row.fileIndex, line: -1 };
}

/** Whether `hit` is at or below a position. */
export function findHitAtOrBelow(hit: ReviewFindHit, position: { fileIndex: number; line: number } | null): boolean {
  if (!position) return true;
  return hit.fileIndex > position.fileIndex || (hit.fileIndex === position.fileIndex && hit.line >= position.line);
}
