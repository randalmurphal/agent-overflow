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
// A forge thread written on the forge's own site can still cross: from a
// removed line (its old number) through an unchanged or added one (its
// new number). The pane reads it but never drafts one.

/** The span of lines a range covers on its half. */
export interface LineRange {
  readonly filePath: string;
  /** The half of its last line, `to`. */
  readonly half: 'old' | 'new';
  readonly from: number;
  readonly to: number;
  /** Set when `from` is an old number and `to` a new one. */
  readonly crossesHalves?: true;
  /**
   * A crossing range's rows, read off its file by `rangesByFile`: old
   * lines `from`..`oldTo` and new lines `newFrom`..`to`. Without them it
   * tints nothing.
   */
  readonly rows?: { readonly oldTo: number; readonly newFrom: number };
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

function threadHalf(side: string): 'old' | 'new' {
  return side === 'left' || side === 'old' ? 'old' : 'new';
}

/** A forge thread's range (it carries `startLine`), or null. */
export function threadRange(thread: Pick<ReviewThread, 'path' | 'line' | 'startLine' | 'side' | 'startSide'>): LineRange | null {
  const line = thread.line ?? 0;
  const start = thread.startLine ?? 0;
  if (line <= 0 || start <= 0) return null;
  const half = threadHalf(thread.side);
  const startHalf = thread.startSide ? threadHalf(thread.startSide) : half;
  if (startHalf === 'old' && half === 'new') return { filePath: thread.path, half, from: start, to: line, crossesHalves: true };
  if (startHalf !== half || start >= line) return null;
  return { filePath: thread.path, half, from: start, to: line };
}

/**
 * The rows of a crossing range in `file`: from the row of old line `from`
 * through the row of new line `to`, inside one hunk; null when the file
 * has no such run.
 */
function crossingRows(file: ReviewFile, from: number, to: number): LineRange['rows'] | null {
  const walker = new RowWalker(file);
  let started = false;
  let oldTo = 0;
  let newFrom = 0;
  while (walker.next()) {
    if (walker.type === 'gap' || walker.type === 'marker') {
      if (started) return null;
      continue;
    }
    if (!started && walker.oldLine !== from) continue;
    started = true;
    if (walker.oldLine > 0) oldTo = walker.oldLine;
    if (walker.newLine > 0 && newFrom === 0) newFrom = walker.newLine;
    if (walker.newLine === to) return { oldTo, newFrom };
  }
  return null;
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
  files: readonly ReviewFile[],
): Map<string, LineRange[]> {
  const out = new Map<string, LineRange[]>();
  const add = (range: LineRange | null | undefined) => {
    if (range) out.set(range.filePath, [...(out.get(range.filePath) ?? []), range]);
  };
  for (const editor of editors) if (editor.state === 'placed') add(editor.range);
  for (const draft of drafts) add(draftRange(draft));
  for (const thread of threads) {
    if (thread.isResolved && !threadOpen(thread.id)) continue;
    const range = threadRange(thread);
    if (!range?.crossesHalves) {
      add(range);
      continue;
    }
    const file = files.find((each) => each.path === range.filePath);
    const rows = file ? crossingRows(file, range.from, range.to) : null;
    if (rows) add({ ...range, rows });
  }
  return out;
}

/** `12–18`, or `-4 to +6` for a range from an old line to a new one. */
export function rangeSpan(range: LineRange): string {
  return range.crossesHalves ? `-${range.from} to +${range.to}` : `${range.from}–${range.to}`;
}

/** `Lines 12–18`. */
export function rangeLabel(range: LineRange): string {
  return `Lines ${rangeSpan(range)}`;
}

/** Whether a display row (or its `half` in split view) is inside `range`. */
export function rowInRange(oldLine: number, newLine: number, half: 'old' | 'new' | null, range: LineRange): boolean {
  if (range.crossesHalves) {
    const rows = range.rows;
    if (!rows) return false;
    const onOld = half !== 'new' && oldLine > 0 && oldLine >= range.from && oldLine <= rows.oldTo;
    const onNew = half !== 'old' && newLine > 0 && newLine >= rows.newFrom && newLine <= range.to;
    return onOld || onNew;
  }
  if (half !== null && half !== range.half) return false;
  const line = range.half === 'new' ? newLine : oldLine;
  return line > 0 && line >= range.from && line <= range.to;
}
