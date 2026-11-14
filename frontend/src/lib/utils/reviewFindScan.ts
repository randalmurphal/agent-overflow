// The review pane's find over the diff or conflict files it shows: every
// match of a query in document order, read from the files' compact
// storage, never the DOM, so rows the virtualizer has not mounted count.
//
// Searched: each file's path (its header), the code text of added,
// removed and context rows and of conflict marker rows (never the +/-
// prefix, the line numbers or a hunk gap's hidden lines, which are not
// in memory), the hidden lines of a collapsed conflict fold, and the
// bodies of the draft comments the diff inserts after their lines, and of
// its PR thread comments as their markdown renders (`commentText`); never a
// textarea, nor a draft open in its editor (`editing`).
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
// (`findMatchChunk`); the scan hands its own reading of where each line
// match is to `emit` so the owner can seed the resolver.
//
// The scan yields to the event loop between slices of work and stops
// when its owner says it is no longer live. Every unit of work counts
// against the slice: a row, a file, a hidden fold line, a comment's run of
// text, and each `LONG_TEXT` characters of one long line. Text the patch
// memory budget evicted is read back through `whenResident`.

import type { DiffReviewComment, ReviewComment, ReviewThread } from '../types/models';
import { findNeedle, foldFindText } from './findMatch';
import type { FindRun } from './findHighlights';
import { RowWalker } from './patchRows';
import type { ReviewFile } from './patchStore';
import { findableBody } from './suggestionMarks';
import { reviewInsertsByFile, type InsertRow } from './reviewRows';

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

/** The key of the element `at` names within its file. */
export function findElementKey(at: ReviewFindAt): string {
  if (at.kind === 'header') return 'h';
  if (at.kind === 'line') return at.tag;
  if (at.kind === 'draft') return findDraftKey(at.commentId);
  return findThreadCommentKey(at.threadId, at.comment);
}

export function makeReviewFindHit(path: string, at: ReviewFindAt, run: number, occurrence: number): ReviewFindHit {
  const index = at.kind === 'thread' ? `${run}:${occurrence}` : `${occurrence}`;
  return { identity: `${path}\u0000${findElementKey(at)}\u0000${index}`, path, at, run, occurrence };
}

/** Characters of one text searched as one unit of work. */
export const LONG_TEXT = 16_384;

// `at`, moved past a low surrogate so a slice there keeps its pair.
function codePointBoundary(text: string, at: number): number {
  if (at <= 0 || at >= text.length) return at;
  const low = text.charCodeAt(at);
  const high = text.charCodeAt(at - 1);
  return low >= 0xdc00 && low <= 0xdfff && high >= 0xd800 && high <= 0xdbff ? at + 1 : at;
}

/**
 * The matching rule review find counts by, the scan and the resolver
 * alike: the matches of `needle` (folded, `findNeedle`) in `run` from
 * `from`, left to right without overlap, `LONG_TEXT` characters at a time.
 * Answers how many start in this chunk, where the next chunk starts, and
 * whether this one ended the run.
 */
export function findMatchChunk(run: string, from: number, needle: string): { count: number; next: number; last: boolean } {
  const end = codePointBoundary(run, Math.min(run.length, from + LONG_TEXT));
  const folded = foldFindText(run.slice(from, codePointBoundary(run, Math.min(run.length, end + needle.length - 1))));
  let count = 0;
  let next = end;
  for (let at = folded.indexOf(needle); at >= 0 && from + at < end; at = folded.indexOf(needle, at + needle.length)) {
    count += 1;
    next = Math.max(next, from + at + needle.length);
  }
  return { count, next, last: end >= run.length };
}

/** The matches of `needle` in `run`, by `findMatchChunk`. */
export function countFindMatches(run: string, needle: string): number {
  let count = 0;
  for (let from = 0; needle !== '' && from + needle.length <= run.length;) {
    const chunk = findMatchChunk(run, from, needle);
    count += chunk.count;
    if (chunk.last) break;
    from = chunk.next;
  }
  return count;
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

export interface ReviewFindContent {
  readonly files: readonly ReviewFile[];
  readonly drafts: readonly DiffReviewComment[];
  readonly prThreads: readonly ReviewThread[];
  /** Every run of text a PR comment's shown body renders, in walk order,
   * with whether it shows now, in the element `rowKey` names
   * (chat/markdown/markdownFindText.ts). */
  commentText(body: string, rowKey: string): readonly FindRun[];
  /** The `data-find-key`s of the drafts and sent comments open in their
   * editor: not shown. */
  editing?: ReadonlySet<string>;
  /** The hidden lines of each collapsed fold of a conflict file, by fold id. */
  foldLines?(path: string): ReadonlyMap<number, readonly string[]> | null;
  /** A file whose body shows as an image or its sizes (a binary file, a
   * text SVG shown rendered): no line of it is shown. */
  showsMedia?(file: ReviewFile): boolean;
}

export interface ReviewFindScanOptions {
  limit: number;
  /** False once a newer scan or a close replaced this one. Asked at
   * every pause, before and after it. */
  live(): boolean;
  /** Called with each match, in document order, and where it is in the
   * file the scan read. */
  emit(hit: ReviewFindHit, file: ReviewFile, spot: ReviewFindFileSpot): void;
  /** Gives the event loop a turn; the scan calls it between slices. */
  pause(): Promise<void>;
  /** Milliseconds of work between pauses. */
  sliceMs?: number;
  now?(): number;
}

/** Lines read back together when the scan meets evicted text. */
const LOAD_LINES = 512;
/** Units of work between clock reads. */
const CLOCK_ROWS = 256;
const DEFAULT_SLICE_MS = 8;

class StopScan extends Error {}

/**
 * Scans `content` for `query`. Resolves true when the matches stopped at
 * `limit`, false otherwise; a scan that stops being live resolves false
 * at its next pause without emitting more. A failed read of evicted text
 * rejects.
 */
export async function scanReviewFind(content: ReviewFindContent, query: string, options: ReviewFindScanOptions): Promise<boolean> {
  const needle = findNeedle(query);
  if (!needle) return false;
  const now = options.now ?? (() => performance.now());
  const sliceMs = options.sliceMs ?? DEFAULT_SLICE_MS;
  let emitted = 0;
  let rows = 0;
  let sliceEnd = now() + sliceMs;

  // Counts `units` of work; once a slice is spent, gives the event loop
  // a turn. Liveness is asked before and after: the owner shows progress
  // first.
  async function tick(units = 1): Promise<void> {
    const was = rows;
    rows += units;
    if (Math.floor(rows / CLOCK_ROWS) === Math.floor(was / CLOCK_ROWS) || now() < sliceEnd) return;
    if (!options.live()) throw new StopScan();
    await options.pause();
    if (!options.live()) throw new StopScan();
    sliceEnd = now() + sliceMs;
  }

  // A run's matches by `findMatchChunk`; a long run reads the clock after
  // each chunk. `each` answers true to stop.
  async function eachMatch(run: string, each: () => boolean): Promise<boolean> {
    for (let from = 0; from + needle.length <= run.length;) {
      const chunk = findMatchChunk(run, from, needle);
      for (let i = 0; i < chunk.count; i += 1) if (each()) return true;
      if (chunk.last) break;
      from = chunk.next;
      await tick(CLOCK_ROWS);
    }
    return false;
  }

  // Emits the matches of one element's text, given whole or as its runs
  // (a match cannot cross one), numbered within each run; a run not shown
  // keeps its number and yields none. True once the limit is hit.
  async function emitText(text: string | readonly FindRun[], file: ReviewFile, at: ReviewFindAt, spot: ReviewFindFileSpot): Promise<boolean> {
    const runs = typeof text === 'string' ? [{ text, shown: true }] : text;
    for (let i = 0; i < runs.length; i += 1) {
      if (!runs[i].shown) continue;
      if (i > 0) await tick();
      let occurrence = 0;
      if (await eachMatch(runs[i].text, () => {
        options.emit(makeReviewFindHit(file.path, at, i, occurrence), file, spot);
        occurrence += 1;
        emitted += 1;
        return emitted >= options.limit;
      })) return true;
    }
    return false;
  }

  // A line's text, read back when evicted; a scan that stopped being live
  // while the read was out goes no further.
  async function lineText(file: ReviewFile, lineIndex: number): Promise<string> {
    const body = file.body;
    if (body.residentLine(lineIndex)) return body.text(lineIndex);
    const text = await body.whenResident(() => body.text(lineIndex), lineIndex, Math.min(body.lineCount, lineIndex + LOAD_LINES));
    if (!options.live()) throw new StopScan();
    return text;
  }

  const inserts = reviewInsertsByFile(content.files, content.drafts, [], content.prThreads, new Set());
  try {
    for (let fileIndex = 0; fileIndex < content.files.length; fileIndex += 1) {
      await tick();
      const file = content.files[fileIndex];
      const path = file.path;
      if (await emitText(path, file, { kind: 'header' }, { rowKey: findHeaderKey(path), line: -2, place: { kind: 'header' } })) return true;

      const fileInserts = inserts.get(path);
      const emitInserts = async (bucket: readonly InsertRow[] | undefined, line: number): Promise<boolean> => {
        for (const insert of bucket ?? []) {
          await tick();
          if (await emitInsert(insert, line)) return true;
        }
        return false;
      };
      const emitInsert = async (insert: InsertRow, line: number): Promise<boolean> => {
        if (insert.kind === 'comment-thread') {
          const draft = content.drafts.find((comment) => comment.id === insert.threadKey);
          if (!draft) return false;
          const rowKey = findDraftKey(draft.id);
          if (content.editing?.has(rowKey)) return false;
          return await emitText(draft.body, file, { kind: 'draft', commentId: draft.id }, { rowKey, line, place: { kind: 'draft', commentId: draft.id } });
        }
        if (insert.kind !== 'pr-thread') return false;
        const thread = insert.thread;
        for (const comment of thread.comments) {
          const key = reviewCommentKey(comment);
          const rowKey = findThreadCommentKey(thread.id, key);
          // A sent comment open in its editor shows the editor, not its body.
          if (content.editing?.has(rowKey)) continue;
          await tick();
          const shown = findableBody(comment);
          if (shown !== '' && await emitText(content.commentText(shown, rowKey), file, { kind: 'thread', threadId: thread.id, comment: key }, {
            rowKey, line, place: { kind: 'thread', threadId: thread.id },
          })) return true;
        }
        // The thread's held replies, at its foot: shown, so revealed, with
        // the thread.
        for (const draft of content.drafts) {
          if (draft.status !== 'draft' || draft.replyThreadId !== thread.id) continue;
          const rowKey = findDraftKey(draft.id);
          if (content.editing?.has(rowKey)) continue;
          await tick();
          if (await emitText(draft.body, file, { kind: 'draft', commentId: draft.id }, { rowKey, line, place: { kind: 'thread', threadId: thread.id } })) return true;
        }
        return false;
      };

      if (await emitInserts(fileInserts?.takeHead(), -1)) return true;

      const body = file.body;
      const folds = body.hasMarkers ? (content.foldLines?.(path) ?? null) : null;
      const walker = new RowWalker(file);
      let markerRun = 0;
      let lastLine = -1;
      const media = content.showsMedia?.(file) ?? false;
      while (!media && walker.next()) {
        await tick();
        if (walker.type === 'gap') continue;
        const lineIndex = walker.lineIndex;
        lastLine = lineIndex;
        const lineSpot: ReviewFindFileSpot = { rowKey: findLineKey(path, lineIndex), line: lineIndex, place: { kind: 'line', lineIndex } };
        if (walker.type === 'marker') {
          const fold = body.fold(lineIndex);
          const { oldAt, newAt } = findMarkerLines(walker);
          if (fold) {
            markerRun = 0;
            const hidden = folds?.get(fold.id) ?? [];
            const place: ReviewFindPlace = { kind: 'fold', foldId: fold.id };
            for (let h = 0; h < hidden.length; h += 1) {
              await tick();
              if (await emitText(hidden[h], file, { kind: 'line', tag: `c${oldAt + h}:${newAt + h}` }, { rowKey: lineSpot.rowKey, line: lineIndex, place, foldLine: h })) return true;
            }
          } else {
            const tag = `m${oldAt}:${newAt}:${markerRun}`;
            markerRun += 1;
            const text = await lineText(file, lineIndex);
            if (await emitText(text, file, { kind: 'line', tag }, lineSpot)) return true;
          }
        } else {
          markerRun = 0;
          // Code text: the +/- or context prefix is the gutter's.
          const text = (await lineText(file, lineIndex)).slice(1);
          if (await emitText(text, file, { kind: 'line', tag: findRowTag(walker) }, lineSpot)) return true;
        }
        const bucket = fileInserts?.take(walker.type, walker.oldLine, walker.newLine);
        if (bucket && await emitInserts(bucket, lineIndex)) return true;
      }
      if (await emitInserts(fileInserts?.takeRest(), lastLine)) return true;
    }
  } catch (err) {
    if (err instanceof StopScan) return false;
    throw err;
  }
  return false;
}
