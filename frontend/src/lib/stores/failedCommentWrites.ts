import { anchorKey, type CommentAnchor } from '../utils/reviewRows';

// A comment posted now (a line comment, a reply, a sent comment's edit, a
// comment on the PR) that the forge refused after its review state was
// gone, its pane closed while the write was in flight. Its text and the
// forge's sentence are kept here, in memory, by the PR on its computer
// (`computerPRKey`), so the next review state of that PR shows them in the
// editor when the same line, thread reply, comment edit or PR comment
// field opens. Only failed writes are kept; one is freed when its editor
// sends, cancels or empties its text. At most FAILED_WRITES_MAX are kept
// across PRs; the oldest goes first, with a console warning naming it.

export type FailedWriteTarget =
  /** A comment on a line, or a range ending on `anchor` from `start`. */
  | { readonly kind: 'line'; readonly anchor: CommentAnchor; readonly start: CommentAnchor | null }
  | { readonly kind: 'reply'; readonly threadId: string }
  | { readonly kind: 'edit'; readonly threadId: string; readonly commentId: string }
  | { readonly kind: 'pr-comment' };

export interface FailedWrite {
  readonly target: FailedWriteTarget;
  readonly body: string;
  /** The forge's sentence. */
  readonly error: string;
  /** Where the comment was, as a person reads it: `src/a.ts:12`, the
   * thread's place, or `the pull request`. */
  readonly where: string;
}

export const FAILED_WRITES_MAX = 10;

interface Kept extends FailedWrite {
  readonly prKey: string;
}

const kept = new Map<string, Kept>();

function targetKey(target: FailedWriteTarget): string {
  switch (target.kind) {
    case 'line': return `line:${anchorKey(target.anchor)}`;
    case 'reply': return `reply:${target.threadId}`;
    case 'edit': return `edit:${target.threadId}\u0000${target.commentId}`;
    case 'pr-comment': return 'pr-comment';
  }
}

function entryKey(prKey: string, target: FailedWriteTarget): string {
  return `${prKey}\u0001${targetKey(target)}`;
}

/** Keeps a failed write, replacing one for the same editor, newest last. */
export function keepFailedWrite(prKey: string, write: FailedWrite): void {
  const key = entryKey(prKey, write.target);
  kept.delete(key);
  kept.set(key, { ...write, prKey });
  for (const [oldest, dropped] of kept) {
    if (kept.size <= FAILED_WRITES_MAX) break;
    kept.delete(oldest);
    console.warn(`A failed comment on ${dropped.where} (${dropped.prKey}) is no longer kept for reopening: at most ${FAILED_WRITES_MAX} are kept.`);
  }
}

/** The failed write kept for this editor of the PR, or null. */
export function failedWriteFor(prKey: string | null, target: FailedWriteTarget): FailedWrite | null {
  return prKey === null ? null : kept.get(entryKey(prKey, target)) ?? null;
}

/** The failed line comment kept on `anchor` of the PR, the last line of
 * its range or its first, or null. */
export function failedLineWriteAt(prKey: string | null, anchor: CommentAnchor): FailedWrite | null {
  if (prKey === null) return null;
  const key = anchorKey(anchor);
  for (const write of kept.values()) {
    if (write.prKey !== prKey || write.target.kind !== 'line') continue;
    if (anchorKey(write.target.anchor) === key || (write.target.start !== null && anchorKey(write.target.start) === key)) return write;
  }
  return null;
}

/** Frees the failed write kept for this editor of the PR, if any. */
export function releaseFailedWrite(prKey: string | null, target: FailedWriteTarget): void {
  if (prKey !== null) kept.delete(entryKey(prKey, target));
}

/** Every kept write; for tests. */
export function keptFailedWrites(): readonly FailedWrite[] {
  return [...kept.values()];
}

/** Drops every kept write; for tests. */
export function resetFailedWrites(): void {
  kept.clear();
}
