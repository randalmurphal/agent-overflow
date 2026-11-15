// What a review find match is, and the `data-find-key`s that place it on
// the rows that show it.
//
// A match is a logical identity, nothing that goes stale when the content
// changes: its file's path, what in that file shows it (`ReviewFindAt`),
// the run of that element's text it is in (a rendered comment's runs are
// numbered in walk order, a closed <details> body's included, so a toggle
// renumbers nothing) and its index among that run's matches. A change that
// leaves the element and its source text alone leaves the identity alone;
// a code line is its side and number, so an edit that renumbers lines
// renames the matches below it. Whether it is present now, and where (file
// index, row, `data-find-key`, the painter's index in the element), is
// derived against the content shown at the moment of use
// (reviewFindRows.ts#resolveReviewFindHit), by the same matching rule
// (findMatch.ts#findMatchChunk) the scan (reviewFindScan.ts) counts by.

import type { ReviewComment } from '../types/models';
import type { RowWalker } from './patchRows';

/** `data-find-key` of a file header's path. */
export function findHeaderKey(path: string): string {
  return `h:${path}`;
}

/** `data-find-key` of a row's code text: its line index in the file. */
export function findLineKey(path: string, lineIndex: number): string {
  return `l:${lineIndex}:${path}`;
}

const LINE_KEY = /^l:(\d+):(.*)$/s;

/** The line index a `findLineKey` names; null for another key. */
export function findLineKeyIndex(key: string): number | null {
  const match = LINE_KEY.exec(key);
  return match ? Number(match[1]) : null;
}

/** The path a `findLineKey` names; null for another key. */
export function findLineKeyPath(key: string): string | null {
  return LINE_KEY.exec(key)?.[2] ?? null;
}

/** `data-find-key` of a draft comment's body. */
export function findDraftKey(commentId: string): string {
  return `dc:${commentId}`;
}

/** A PR thread comment's forge id, which a refresh of the thread keeps.
 * GitHub's GraphQL `databaseId` is nullable (the wire carries 0); such a
 * comment is keyed by its creation time and author instead, which a
 * refresh keeps as well. */
export function reviewCommentKey(comment: ReviewComment): string {
  return comment.databaseID > 0 ? String(comment.databaseID) : `@${comment.createdAt}@${comment.authorLogin}`;
}

/** `data-find-key` of a PR thread comment's body, by its thread and its
 * `reviewCommentKey`. */
export function findThreadCommentKey(threadId: string, comment: string): string {
  return `pc:${encodeURIComponent(threadId)}:${encodeURIComponent(comment)}`;
}

/** The thread id and comment key a `findThreadCommentKey` names. */
export function findThreadCommentOfKey(key: string): { threadId: string; comment: string } | null {
  const match = /^pc:([^:]*):([^:]*)$/.exec(key);
  return match ? { threadId: decodeURIComponent(match[1]), comment: decodeURIComponent(match[2]) } : null;
}

/** What in a file shows a match, by fields that survive a content change:
 * the path (`header`); a code line by its side and line number, `a<new>`,
 * `d<old>` or `c<old>:<new>` (a conflict fold's hidden line is the context
 * line it becomes when the fold opens), or a conflict marker line,
 * `m<old>:<new>:<run>`, by the line numbers in effect and its place in a
 * run of markers (`line`); a draft by its id; a PR thread comment by its
 * thread and `reviewCommentKey`. */
export type ReviewFindAt =
  | { kind: 'header' }
  | { kind: 'line'; tag: string }
  | { kind: 'draft'; commentId: string }
  | { kind: 'thread'; threadId: string; comment: string };

export interface ReviewFindHit {
  /** `path`, the element `at` names, `run` (a comment's) and `occurrence`,
   * as one string. */
  readonly identity: string;
  readonly path: string;
  readonly at: ReviewFindAt;
  /** The run of the element's text it is in: a rendered comment's, in walk
   * order, shown or not; 0 for every other element, which is one run. */
  readonly run: number;
  /** Its index among that run's matches. */
  readonly occurrence: number;
}

/** What a match's reveal must make visible before it scrolls. */
export type ReviewFindPlace =
  | { kind: 'header' }
  | { kind: 'line'; lineIndex: number }
  | { kind: 'fold'; foldId: number }
  | { kind: 'draft'; commentId: string }
  | { kind: 'thread'; threadId: string };

/** Where a match is in one file as read now; physical, never published. */
export interface ReviewFindFileSpot {
  /** The `data-find-key` of the element that shows it. */
  readonly rowKey: string;
  /** Order within the file: the line index of the row it is in or follows;
   * -2 for the header, -1 above the first row. */
  readonly line: number;
  readonly place: ReviewFindPlace;
  /** A fold's hidden line: its index in the fold. */
  readonly foldLine?: number;
}

// The key of the element `at` names within its file.
function findElementKey(at: ReviewFindAt): string {
  if (at.kind === 'header') return 'h';
  if (at.kind === 'line') return at.tag;
  if (at.kind === 'draft') return findDraftKey(at.commentId);
  return findThreadCommentKey(at.threadId, at.comment);
}

export function makeReviewFindHit(path: string, at: ReviewFindAt, run: number, occurrence: number): ReviewFindHit {
  const index = at.kind === 'thread' ? `${run}:${occurrence}` : `${occurrence}`;
  return { identity: `${path}\u0000${findElementKey(at)}\u0000${index}`, path, at, run, occurrence };
}

/** The tag of the code row a `RowWalker` is at (not a marker or gap). */
export function findRowTag(walker: RowWalker): string {
  return walker.type === 'add' ? `a${walker.newLine}` : walker.type === 'del' ? `d${walker.oldLine}` : `c${walker.oldLine}:${walker.newLine}`;
}

/** The line numbers in effect at a marker row; a conflict file numbers
 * from 1 before its first hunk header. */
export function findMarkerLines(walker: RowWalker): { oldAt: number; newAt: number } {
  const at = walker.snapshot();
  return { oldAt: at.sawHunk ? at.oldLine : 1, newAt: at.sawHunk ? at.newLine : 1 };
}
