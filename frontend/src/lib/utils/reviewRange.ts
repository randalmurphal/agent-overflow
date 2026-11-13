import type { DiffReviewComment, ReviewThread } from '../types/models';
import { anchorRows, RowWalker } from './patchRows';
import type { ReviewFile } from './patchStore';
import type { CommentAnchor } from './reviewRows';

// A comment on a range of lines: one file, one half of the diff, and lines
// of one hunk of the diff the forge has. The half is what the forge's
// position names (GitHub `start_side`/`side`, GitLab's line codes): an
// added line is on the new half, a deleted one on the old, an unchanged
// one on both, so a range from an unchanged line to an added one is a
// new-half range and one from a deleted line to an added one is refused.

/** The span of lines a range covers on its half. */
export interface LineRange {
  readonly filePath: string;
  readonly half: 'old' | 'new';
  readonly from: number;
  readonly to: number;
}

export type RangeRefusal = 'file' | 'other-file' | 'other-side' | 'not-in-diff' | 'across-hunks';

export const RANGE_REFUSAL_TEXT: Record<RangeRefusal, string> = {
  file: 'A comment on the whole file has no lines to span.',
  'other-file': 'A range stays in one file.',
  'other-side': 'A range stays on one side of the diff.',
  'not-in-diff': 'A range covers only lines the diff has.',
  'across-hunks': 'A range stays inside one hunk of the diff.',
};

export type RangeSpan =
  | { kind: 'range'; start: CommentAnchor; end: CommentAnchor; range: LineRange }
  /** Both ends are the same row: a single line. */
  | { kind: 'single'; anchor: CommentAnchor }
  | { kind: 'refused'; reason: RangeRefusal };

function halves(anchor: CommentAnchor): { old: boolean; neu: boolean } {
  return { old: (anchor.oldLine ?? 0) > 0, neu: (anchor.newLine ?? 0) > 0 };
}

/** The half two lines share, new first; null when they share none. */
export function sharedHalf(a: CommentAnchor, b: CommentAnchor): 'old' | 'new' | null {
  const x = halves(a);
  const y = halves(b);
  if (x.neu && y.neu) return 'new';
  if (x.old && y.old) return 'old';
  return null;
}

function lineOn(anchor: CommentAnchor, half: 'old' | 'new'): number {
  return (half === 'new' ? anchor.newLine : anchor.oldLine) ?? 0;
}

/**
 * The range from `origin` (the open editor's line) to `target` (the line
 * shift+clicked), in either direction, checked against `base`: the file as
 * the diff loaded it, before any expansion. A row an expansion supplied is
 * not in `base`, so a range touching one is refused as a line the diff
 * does not have; so is one whose rows cross from one hunk to the next.
 */
export function spanRange(base: ReviewFile, origin: CommentAnchor, target: CommentAnchor): RangeSpan {
  if (origin.side === 'file' || target.side === 'file') return { kind: 'refused', reason: 'file' };
  if (origin.filePath !== target.filePath || base.path !== origin.filePath) return { kind: 'refused', reason: 'other-file' };
  const half = sharedHalf(origin, target);
  if (half === null) return { kind: 'refused', reason: 'other-side' };
  const originRow = anchorRows(base, origin).row;
  const targetRow = anchorRows(base, target).row;
  if (originRow < 0 || targetRow < 0) return { kind: 'refused', reason: 'not-in-diff' };
  if (originRow === targetRow) return { kind: 'single', anchor: origin };
  const [start, end, first, last] = originRow < targetRow
    ? [origin, target, originRow, targetRow]
    : [target, origin, targetRow, originRow];
  const walker = new RowWalker(base);
  let previous = 0;
  while (walker.next()) {
    if (walker.row < first) continue;
    if (walker.row > last) break;
    if (walker.type === 'gap' || walker.type === 'marker') return { kind: 'refused', reason: 'across-hunks' };
    const line = half === 'new' ? walker.newLine : walker.oldLine;
    if (line === 0) continue;
    if (previous !== 0 && line !== previous + 1) return { kind: 'refused', reason: 'across-hunks' };
    previous = line;
  }
  return {
    kind: 'range',
    start,
    end,
    range: { filePath: base.path, half, from: lineOn(start, half), to: lineOn(end, half) },
  };
}

/** A saved draft's range, or null for one line, a file or a reply. */
export function draftRange(comment: Pick<DiffReviewComment, 'filePath' | 'oldLine' | 'newLine' | 'startOldLine' | 'startNewLine'>): LineRange | null {
  const startNew = comment.startNewLine ?? 0;
  const startOld = comment.startOldLine ?? 0;
  if (startNew > 0 && (comment.newLine ?? 0) > 0) return { filePath: comment.filePath, half: 'new', from: startNew, to: comment.newLine! };
  if (startOld > 0 && (comment.oldLine ?? 0) > 0) return { filePath: comment.filePath, half: 'old', from: startOld, to: comment.oldLine! };
  return null;
}

/** A forge thread's range (it carries `startLine`), or null. */
export function threadRange(thread: Pick<ReviewThread, 'path' | 'line' | 'startLine' | 'side'>): LineRange | null {
  const line = thread.line ?? 0;
  const start = thread.startLine ?? 0;
  if (line <= 0 || start <= 0 || start >= line) return null;
  return { filePath: thread.path, half: thread.side === 'left' || thread.side === 'old' ? 'old' : 'new', from: start, to: line };
}

/**
 * The ranges a diff tints, by file: placed range editors, range drafts,
 * and forge threads that span lines while unresolved or opened.
 */
export function rangesByFile(
  editors: readonly { readonly state: string; readonly range?: LineRange }[],
  drafts: readonly DiffReviewComment[],
  threads: readonly ReviewThread[],
  threadOpen: (threadId: string) => boolean,
): Map<string, LineRange[]> {
  const out = new Map<string, LineRange[]>();
  const add = (range: LineRange | null | undefined) => {
    if (range) out.set(range.filePath, [...(out.get(range.filePath) ?? []), range]);
  };
  for (const editor of editors) if (editor.state === 'placed') add(editor.range);
  for (const draft of drafts) add(draftRange(draft));
  for (const thread of threads) if (!thread.isResolved || threadOpen(thread.id)) add(threadRange(thread));
  return out;
}

/** `Lines 12–18`. */
export function rangeLabel(range: LineRange): string {
  return `Lines ${range.from}–${range.to}`;
}

/** Whether a display row (or its `half` in split view) is inside `range`. */
export function rowInRange(oldLine: number, newLine: number, half: 'old' | 'new' | null, range: LineRange): boolean {
  if (half !== null && half !== range.half) return false;
  const line = range.half === 'new' ? newLine : oldLine;
  return line > 0 && line >= range.from && line <= range.to;
}
