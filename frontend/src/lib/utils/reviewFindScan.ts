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
// Each match is emitted as its identity (reviewFindIdentity.ts), with the
// scan's own reading of where it is so the owner can seed the resolver.
//
// The scan yields to the event loop between slices of work and stops
// when its owner says it is no longer live. Work is counted in units: a
// file, a row, a hidden fold line and a comment's run of text count one
// each, and every character the scan reads or searches counts as well, one
// unit per `UNIT_CHARS` (`LONG_TEXT` characters are `CLOCK_ROWS` units).
// A text the rule searches counts the characters each chunk folds
// (`findMatchChunk`). A filter block, at most `LONG_TEXT` bytes of text,
// counts its length once searched. The clock is read once a count crosses
// a multiple of `CLOCK_ROWS` units, and the scan pauses there once the
// slice is spent, so between two clock reads it reads or searches at most
// 2 x `LONG_TEXT` + the needle's length characters, besides one line it
// reads whole.
//
// The rule reads a row's text only when its line can match: the scan runs
// a filter (findMatch.ts#findFilterPattern) over the text of a block of
// lines at a time, decoded at once and never folded, and a row whose line
// it does not flag costs its walk, its count toward the slice and its
// inserts, nothing else.
//
// Text the patch memory budget evicted is pinned, read back
// (`PatchBody.load`) and held until the walk leaves it, so a budget smaller
// than what one read brings back cannot evict it before its lines are
// read and make each of them another read. What is held is bounded by
// size: the lines from the evicted one through `patchTextReadAheadBytes`
// bytes, at least that one line, and the rest of the chunks
// they lie in (`PatchBody.pinnedEnd`), which the pin holds anyway. The
// next range is pinned before the last is released, so a chunk the two
// share is read once.

import type { DiffReviewComment, ReviewThread } from '../types/models';
import { patchTextReadAheadBytes } from './patchMemory.svelte';
import { findFilterPattern, findMatchChunk, findNeedle, flagFilterLines, LONG_TEXT } from './findMatch';
import type { FindRun } from './findHighlights';
import { RowWalker } from './patchRows';
import type { PatchBody } from './patchBody';
import type { ReviewFile } from './patchParser';
import {
  findDraftKey, findHeaderKey, findLineKey, findMarkerLines, findRowTag, findThreadCommentKey, makeReviewFindHit, reviewCommentKey,
  type ReviewFindAt, type ReviewFindFileSpot, type ReviewFindHit, type ReviewFindPlace,
} from './reviewFindIdentity';
import { findableBody } from './suggestionMarks';
import { reviewInsertsByFile, type InsertRow } from './reviewRows';

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

/** Most lines one filter block reads. */
export const BLOCK_LINES = 512;
/** Units of work between clock reads. */
export const CLOCK_ROWS = 256;
/** Characters read or searched that count one unit of work. */
const UNIT_CHARS = LONG_TEXT / CLOCK_ROWS;
/** Most bytes of text one filter block reads, which decode to no more
 * characters than the rule searches at a time. A longer line is never
 * read by the filter: the rule searches it `LONG_TEXT` at a time. */
const BLOCK_SIZE = LONG_TEXT;
const DEFAULT_SLICE_MS = 8;

class StopScan extends Error {}

// The end of the lines of one segment from line `start`, at most `lines`
// of them and `size` bytes, and at least one.
function linesEndOf(body: PatchBody, start: number, lines: number, size: number): number {
  const base = body.storedLineStart(start);
  let low = start + 1;
  let high = Math.min(body.segmentEnd(start), start + lines);
  while (low < high) {
    const mid = (low + high + 1) >> 1;
    if (body.storedLineEnd(mid - 1) - base <= size) low = mid;
    else high = mid - 1;
  }
  return low;
}

// The end of the filter block from line `start`.
function blockEndOf(body: PatchBody, start: number): number {
  return linesEndOf(body, start, BLOCK_LINES, BLOCK_SIZE);
}

function lineSpot(path: string, lineIndex: number): ReviewFindFileSpot {
  return { rowKey: findLineKey(path, lineIndex), line: lineIndex, place: { kind: 'line', lineIndex } };
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

  // Counts `units` of work and reads the clock each CLOCK_ROWS units: true
  // once the slice is spent, when the caller must `pause` before more.
  function spent(units = 1): boolean {
    const was = rows;
    rows += units;
    return Math.floor(rows / CLOCK_ROWS) !== Math.floor(was / CLOCK_ROWS) && now() >= sliceEnd;
  }

  // Gives the event loop a turn and starts a new slice. Liveness is asked
  // before and after: the owner shows progress first.
  async function pause(): Promise<void> {
    if (!options.live()) throw new StopScan();
    await options.pause();
    if (!options.live()) throw new StopScan();
    sliceEnd = now() + sliceMs;
  }

  // A run's matches by `findMatchChunk`, counting what each chunk folded.
  // `each` answers true to stop.
  async function eachMatch(run: string, each: () => boolean): Promise<boolean> {
    for (let from = 0; from + needle.length <= run.length;) {
      const chunk = findMatchChunk(run, from, needle);
      for (let i = 0; i < chunk.count; i += 1) if (each()) return true;
      if (spent(Math.floor(chunk.folded / UNIT_CHARS))) await pause();
      if (chunk.last) break;
      from = chunk.next;
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
      if (i > 0 && spent()) await pause();
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

  // The lines [.., heldEnd) of the current file read back last, pinned
  // until the walk passes heldEnd or the file or scan ends.
  let releaseHeld: (() => void) | null = null;
  let heldEnd = 0;
  function dropHeld(): void {
    releaseHeld?.();
    releaseHeld = null;
    heldEnd = 0;
  }

  // `read` run over lines [start, end) of `body` read back, which stay
  // held with the rest of their chunks; a scan that stopped being live
  // while the read was out goes no further.
  async function readBack<T>(body: PatchBody, start: number, end: number, read: () => T): Promise<T> {
    const holdEnd = body.pinnedEnd(start, end);
    const release = body.pin(start, holdEnd);
    dropHeld();
    releaseHeld = release;
    heldEnd = holdEnd;
    const loading = body.load(start, holdEnd);
    if (loading !== null) await loading;
    if (!options.live()) throw new StopScan();
    return read();
  }

  // A line's text, read back with the lines after it when evicted.
  async function lineText(body: PatchBody, lineIndex: number): Promise<string> {
    if (body.resident(lineIndex, lineIndex + 1)) return body.text(lineIndex);
    const end = linesEndOf(body, lineIndex, Infinity, patchTextReadAheadBytes());
    return await readBack(body, lineIndex, end, () => body.text(lineIndex));
  }

  // One file's filter: whether the rule must read line `lineIndex`, asked
  // in ascending line order. It searches a block of lines when the walk
  // enters it, and answers synchronously unless that block was evicted or
  // its search ended the slice. `text` gives the line last flagged from
  // the block's decoded text, or null for a line the filter did not
  // read. `flags` and `starts` are the scan's arrays, which only the
  // filter of the file being walked uses.
  function lineFilter(body: PatchBody, pattern: RegExp, flags: Int32Array, starts: Int32Array): {
    flagged(lineIndex: number): boolean | Promise<boolean>;
    text(): string | null;
  } {
    // The flagged lines, ascending, of the block [.., end), the first
    // `count` of `flags`, and where each starts in `block`, the block's
    // text; `at` is the first not behind the walk.
    let count = 0;
    let at = 0;
    let end = 0;
    let block: string | null = null;
    const pick = (lineIndex: number): boolean => {
      while (at < count && flags[at] < lineIndex) at += 1;
      return at < count && flags[at] === lineIndex;
    };
    // Flags the block's lines from their text; true once the slice is
    // spent.
    const search = (start: number, text: string): boolean => {
      block = text;
      count = flagFilterLines(pattern, text, start, flags, starts);
      return spent(Math.ceil(text.length / UNIT_CHARS));
    };
    const searchEvicted = async (start: number): Promise<boolean> => {
      if (search(start, await readBack(body, start, end, () => body.segmentText(start, end)))) await pause();
      return pick(start);
    };
    return {
      flagged(lineIndex) {
        if (lineIndex < end) return pick(lineIndex);
        at = 0;
        if (body.lineSize(lineIndex) > BLOCK_SIZE) {
          // One line over the block size: the rule reads and searches it.
          end = lineIndex + 1;
          flags[0] = lineIndex;
          count = 1;
          block = null;
          return true;
        }
        end = blockEndOf(body, lineIndex);
        if (!body.resident(lineIndex, end)) return searchEvicted(lineIndex);
        if (search(lineIndex, body.segmentText(lineIndex, end))) return pause().then(() => pick(lineIndex));
        return pick(lineIndex);
      },
      text() {
        if (block === null) return null;
        const start = starts[at];
        const newline = block.indexOf('\n', start);
        return block.slice(start, newline < 0 ? block.length : newline);
      },
    };
  }

  // Which rows the rule reads: none for a needle holding a newline, which
  // no line holds; every row for a needle the filter cannot spell
  // (findFilterPattern); otherwise the rows whose lines the filter flags.
  // The rule decides what a flagged line matches.
  const rowRule = needle.includes('\n') ? 'none' : (findFilterPattern(needle) ?? 'every');
  const flags = typeof rowRule === 'string' ? null : new Int32Array(BLOCK_LINES);
  const starts = flags === null ? null : new Int32Array(BLOCK_LINES);

  const inserts = reviewInsertsByFile(content.files, content.drafts, [], content.prThreads, new Set());
  try {
    for (let fileIndex = 0; fileIndex < content.files.length; fileIndex += 1) {
      if (spent()) await pause();
      const file = content.files[fileIndex];
      const path = file.path;
      if (await emitText(path, file, { kind: 'header' }, { rowKey: findHeaderKey(path), line: -2, place: { kind: 'header' } })) return true;

      const fileInserts = inserts.get(path);
      const emitInserts = async (bucket: readonly InsertRow[] | undefined, line: number): Promise<boolean> => {
        for (const insert of bucket ?? []) {
          if (spent()) await pause();
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
          if (spent()) await pause();
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
          if (spent()) await pause();
          if (await emitText(draft.body, file, { kind: 'draft', commentId: draft.id }, { rowKey, line, place: { kind: 'thread', threadId: thread.id } })) return true;
        }
        return false;
      };

      if (await emitInserts(fileInserts?.takeHead(), -1)) return true;

      const body = file.body;
      const folds = body.hasMarkers ? (content.foldLines?.(path) ?? null) : null;
      const walker = new RowWalker(file);
      const filter = typeof rowRule === 'string' || flags === null || starts === null ? null : lineFilter(body, rowRule, flags, starts);
      // A flagged line's text: from the filter's block when it read one.
      const flaggedText = async (lineIndex: number): Promise<string> => filter?.text() ?? await lineText(body, lineIndex);
      let markerRun = 0;
      let lastLine = -1;
      const media = content.showsMedia?.(file) ?? false;
      while (!media && walker.next()) {
        if (spent()) await pause();
        if (walker.type === 'gap') continue;
        const lineIndex = walker.lineIndex;
        lastLine = lineIndex;
        if (lineIndex >= heldEnd && releaseHeld !== null) dropHeld();
        let flagged = filter === null ? rowRule === 'every' : filter.flagged(lineIndex);
        if (typeof flagged !== 'boolean') flagged = await flagged;
        if (walker.type === 'marker') {
          const fold = body.fold(lineIndex);
          if (fold) {
            markerRun = 0;
            const { oldAt, newAt } = findMarkerLines(walker);
            const hidden = folds?.get(fold.id) ?? [];
            const place: ReviewFindPlace = { kind: 'fold', foldId: fold.id };
            const rowKey = findLineKey(path, lineIndex);
            for (let h = 0; h < hidden.length; h += 1) {
              if (spent()) await pause();
              if (await emitText(hidden[h], file, { kind: 'line', tag: `c${oldAt + h}:${newAt + h}` }, { rowKey, line: lineIndex, place, foldLine: h })) return true;
            }
          } else {
            const run = markerRun;
            markerRun += 1;
            if (flagged) {
              const { oldAt, newAt } = findMarkerLines(walker);
              const text = await flaggedText(lineIndex);
              if (await emitText(text, file, { kind: 'line', tag: `m${oldAt}:${newAt}:${run}` }, lineSpot(path, lineIndex))) return true;
            }
          }
        } else {
          markerRun = 0;
          if (flagged) {
            // Code text: the +/- or context prefix is the gutter's.
            const text = (await flaggedText(lineIndex)).slice(1);
            if (await emitText(text, file, { kind: 'line', tag: findRowTag(walker) }, lineSpot(path, lineIndex))) return true;
          }
        }
        if (fileInserts === undefined) continue;
        const bucket = fileInserts.take(walker.type, walker.oldLine, walker.newLine);
        if (bucket && await emitInserts(bucket, lineIndex)) return true;
      }
      dropHeld();
      if (await emitInserts(fileInserts?.takeRest(), lastLine)) return true;
    }
  } catch (err) {
    if (err instanceof StopScan) return false;
    throw err;
  } finally {
    dropHeld();
  }
  return false;
}
