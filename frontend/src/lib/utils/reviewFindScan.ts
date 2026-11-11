// The review pane's find over the diff or conflict files it shows: every
// match of a query in document order, read from the files' compact
// storage, never the DOM, so rows the virtualizer has not mounted count.
//
// Searched: each file's path (its header), the code text of added,
// removed and context rows and of conflict marker rows (never the +/-
// prefix, the line numbers or a hunk gap's hidden lines, which are not
// in memory), the hidden lines of a collapsed conflict fold, and the
// bodies of the draft comments the diff inserts after their lines, and of
// its PR thread comments as their markdown renders (`commentText`). A match names the element
// that shows it (`data-find-key`, the keys below) and its index among
// that element's matches, which the highlighter paints as current.
//
// The scan yields to the event loop between slices of work and stops
// when its owner says it is no longer live. Every unit of work counts
// against the slice: a row, a file, a hidden fold line, a comment's run of
// text, and each `LONG_TEXT` characters of one long line. Text the patch
// memory budget evicted is read back through `whenResident`.

import type { DiffReviewComment, ReviewThread } from '../types/models';
import { findNeedle, foldFindText } from './findMatch';
import { RowWalker } from './patchRows';
import type { ReviewFile } from './patchStore';
import { visibleBody } from './reviewComments';
import { reviewInsertsByFile, type InsertRow } from './reviewRows';

/** `data-find-key` of a file header's path. */
export function findHeaderKey(path: string): string {
  return `h:${path}`;
}

/** `data-find-key` of a row's code text: its line index in the file. */
export function findLineKey(path: string, lineIndex: number): string {
  return `l:${lineIndex}:${path}`;
}

/** `data-find-key` of a draft comment's body. */
export function findDraftKey(commentId: string): string {
  return `dc:${commentId}`;
}

/** `data-find-key` of a PR thread comment's body, by its index in the thread. */
export function findThreadCommentKey(threadId: string, index: number): string {
  return `pc:${index}:${threadId}`;
}

/** What a match's reveal must make visible before it scrolls. */
export type ReviewFindPlace =
  | { kind: 'header' }
  | { kind: 'line'; lineIndex: number }
  | { kind: 'fold'; foldId: number }
  | { kind: 'draft'; commentId: string }
  | { kind: 'thread'; threadId: string };

export interface ReviewFindHit {
  /** The same text at the same place across a rescan of changed content. */
  readonly identity: string;
  /** The `data-find-key` of the element that shows it. */
  readonly rowKey: string;
  /** Its index among that element's matches. */
  readonly occurrence: number;
  readonly fileIndex: number;
  readonly path: string;
  /** Order within the file: the line index of the row it is in or follows;
   * -2 for the header, -1 above the first row. */
  readonly line: number;
  readonly place: ReviewFindPlace;
}

export interface ReviewFindContent {
  readonly files: readonly ReviewFile[];
  readonly drafts: readonly DiffReviewComment[];
  readonly prThreads: readonly ReviewThread[];
  /** The runs of text a PR comment's shown body renders as in the element
   * `rowKey` names (chat/markdown/markdownFindText.ts). */
  commentText(body: string, rowKey: string): readonly string[];
  /** The hidden lines of each collapsed fold of a conflict file, by fold id. */
  foldLines?(path: string): ReadonlyMap<number, readonly string[]> | null;
}

export interface ReviewFindScanOptions {
  limit: number;
  /** False once a newer scan or a close replaced this one. Asked at
   * every pause, before and after it. */
  live(): boolean;
  /** Called with each match, in document order. */
  emit(hit: ReviewFindHit): void;
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
/** Characters of one text searched as one unit of work. */
export const LONG_TEXT = 16_384;
const DEFAULT_SLICE_MS = 8;

class StopScan extends Error {}

// `at`, moved past a low surrogate so a slice there keeps its pair.
function codePointBoundary(text: string, at: number): number {
  if (at <= 0 || at >= text.length) return at;
  const low = text.charCodeAt(at);
  const high = text.charCodeAt(at - 1);
  return low >= 0xdc00 && low <= 0xdfff && high >= 0xd800 && high <= 0xdbff ? at + 1 : at;
}

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

  // A run's matches, left to right without overlap, `LONG_TEXT`
  // characters of it at a time; a long run reads the clock after each.
  // `each` answers true to stop.
  async function eachMatch(run: string, each: () => boolean): Promise<boolean> {
    let from = 0;
    while (from + needle.length <= run.length) {
      const end = codePointBoundary(run, Math.min(run.length, from + LONG_TEXT));
      const folded = foldFindText(run.slice(from, codePointBoundary(run, Math.min(run.length, end + needle.length - 1))));
      let next = end;
      for (let at = folded.indexOf(needle); at >= 0 && from + at < end; at = folded.indexOf(needle, at + needle.length)) {
        if (each()) return true;
        next = Math.max(next, from + at + needle.length);
      }
      from = next;
      if (end < run.length) await tick(CLOCK_ROWS);
    }
    return false;
  }

  // Emits the matches of one element's text, given whole or as the runs
  // a match cannot cross; true once the limit is hit.
  async function emitText(text: string | readonly string[], make: (occurrence: number) => ReviewFindHit): Promise<boolean> {
    let occurrence = 0;
    const runs = typeof text === 'string' ? [text] : text;
    for (let i = 0; i < runs.length; i += 1) {
      const run = runs[i];
      if (i > 0) await tick();
      if (await eachMatch(run, () => {
        options.emit(make(occurrence));
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
      const base = { fileIndex, path };
      if (await emitText(path, (occurrence) => ({
        ...base, identity: `${path}\u0000h\u0000${occurrence}`, rowKey: findHeaderKey(path), occurrence, line: -2, place: { kind: 'header' },
      }))) return true;

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
          return await emitText(draft.body, (occurrence) => ({
            ...base, identity: `${path}\u0000${rowKey}\u0000${occurrence}`, rowKey, occurrence, line, place: { kind: 'draft', commentId: draft.id },
          }));
        }
        if (insert.kind !== 'pr-thread') return false;
        const thread = insert.thread;
        for (let index = 0; index < thread.comments.length; index += 1) {
          const rowKey = findThreadCommentKey(thread.id, index);
          await tick();
          const shown = visibleBody(thread.comments[index].body);
          if (shown !== '' && await emitText(content.commentText(shown, rowKey), (occurrence) => ({
            ...base, identity: `${path}\u0000${rowKey}\u0000${occurrence}`, rowKey, occurrence, line, place: { kind: 'thread', threadId: thread.id },
          }))) return true;
        }
        return false;
      };

      if (await emitInserts(fileInserts?.get(0), -1)) return true;
      fileInserts?.delete(0);

      const body = file.body;
      const folds = body.hasMarkers ? (content.foldLines?.(path) ?? null) : null;
      const walker = new RowWalker(file);
      let markerRun = 0;
      let lastLine = -1;
      while (walker.next()) {
        await tick();
        if (walker.type === 'gap') continue;
        const lineIndex = walker.lineIndex;
        lastLine = lineIndex;
        if (walker.type === 'marker') {
          const fold = body.fold(lineIndex);
          // The line numbers in effect at the row; a conflict file numbers
          // from 1 before its first hunk header.
          const at = walker.snapshot();
          const oldAt = at.sawHunk ? at.oldLine : 1;
          const newAt = at.sawHunk ? at.newLine : 1;
          if (fold) {
            markerRun = 0;
            const hidden = folds?.get(fold.id) ?? [];
            for (let h = 0; h < hidden.length; h += 1) {
              await tick();
              if (await emitText(hidden[h], (occurrence) => ({
                ...base,
                identity: `${path}\u0000c${oldAt + h}:${newAt + h}\u0000${occurrence}`,
                rowKey: findLineKey(path, lineIndex),
                occurrence,
                line: lineIndex,
                place: { kind: 'fold', foldId: fold.id },
              }))) return true;
            }
          } else {
            const tag = `m${oldAt}:${newAt}:${markerRun}`;
            markerRun += 1;
            const text = await lineText(file, lineIndex);
            if (await emitText(text, (occurrence) => ({
              ...base, identity: `${path}\u0000${tag}\u0000${occurrence}`, rowKey: findLineKey(path, lineIndex), occurrence, line: lineIndex, place: { kind: 'line', lineIndex },
            }))) return true;
          }
        } else {
          markerRun = 0;
          const tag = walker.type === 'add' ? `a${walker.newLine}` : walker.type === 'del' ? `d${walker.oldLine}` : `c${walker.oldLine}:${walker.newLine}`;
          // Code text: the +/- or context prefix is the gutter's.
          const text = (await lineText(file, lineIndex)).slice(1);
          if (await emitText(text, (occurrence) => ({
            ...base, identity: `${path}\u0000${tag}\u0000${occurrence}`, rowKey: findLineKey(path, lineIndex), occurrence, line: lineIndex, place: { kind: 'line', lineIndex },
          }))) return true;
        }
        const anchor = walker.anchorLine();
        const bucket = fileInserts?.get(anchor);
        if (bucket?.length) {
          if (await emitInserts(bucket, lineIndex)) return true;
          fileInserts?.delete(anchor);
        }
      }
      for (const bucket of fileInserts?.values() ?? []) {
        if (await emitInserts(bucket, lastLine)) return true;
      }
    }
  } catch (err) {
    if (err instanceof StopScan) return false;
    throw err;
  }
  return false;
}
