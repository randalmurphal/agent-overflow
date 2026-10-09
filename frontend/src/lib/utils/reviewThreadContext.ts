import type { ReviewThread } from '../types/models';
import type { ReviewFile } from './patchStore';
import { exactLineRow, materializeRows, rowStartAt, type MaterializedRows } from './patchRows';

// The lines a review thread was written against, read from the diff the
// pane already holds (no forge fetch): the anchored line and a few rows
// above it, the way a forge shows the hunk over a thread. The rows come
// from the file's compact body, so a thread on evicted text reads as
// pending until `whenResident` brings the lines back (`complete`).

export const THREAD_CONTEXT_ROWS = 4;

/** The thread's anchored display row in `file`, or -1 when the diff does
 * not show that exact line (the diff row treats it as orphaned too). */
export function threadAnchorRow(file: ReviewFile, thread: ReviewThread): number {
  const line = thread.line ?? thread.startLine ?? null;
  if (!line) return -1;
  const side = thread.side === 'left' || thread.side === 'old' ? 'old' : 'new';
  return exactLineRow(file, side, line);
}

/** Up to THREAD_CONTEXT_ROWS display rows ending at the thread's line,
 * or null when the thread anchors nothing in `file`. */
export function threadContextRows(file: ReviewFile, thread: ReviewThread): MaterializedRows | null {
  const row = threadAnchorRow(file, thread);
  if (row < 0) return null;
  const first = Math.max(0, row - (THREAD_CONTEXT_ROWS - 1));
  return materializeRows(file, rowStartAt(file, first), row - first + 1);
}
