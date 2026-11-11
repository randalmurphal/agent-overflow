// Where a find match is in the content shown now, and where that sits in
// the review diff's row model: the list row that shows it and the pixels
// from that row's top to the match's line, and the reading position a new
// query starts from. The diff body scrolls.
//
// A published match is a logical identity (reviewFindScan.ts). Its file
// index, row, `data-find-key` and fold are derived here against the
// content at the moment of use, by the painter, the shift, the reveal and
// the reading position alike; a match that does not resolve is not
// present now. What the scan read seeds the per-file cache; a file is
// immutable, so its entries never go stale, and the cache dies with it.

import type { DiffReviewComment, ReviewThread } from '../types/models';
import { RowWalker, splitRowCount } from './patchRows';
import type { ReviewFile } from './patchStore';
import { findElementKey, findLineKey, findHeaderKey, findMarkerLines, findRowTag, type ReviewFindFileSpot, type ReviewFindHit } from './reviewFindScan';
import { REVIEW_LINE_HEIGHT_PX, reviewInsertsByFile, type InsertRow, type LineBlockRow, type ReviewRowsResult } from './reviewRows';

/** The content a match resolves against: what the view shows now. */
export interface ReviewFindLocateContent {
  readonly files: readonly ReviewFile[];
  readonly drafts: readonly DiffReviewComment[];
  readonly prThreads: readonly ReviewThread[];
}

/** Where a match is now. */
export interface ReviewFindLocation extends ReviewFindFileSpot {
  readonly fileIndex: number;
}

/** Element spots per file, by `findElementKey`; null for an element the
 * file does not have. Bounded: a long session of queries over one large
 * file starts its map over. */
const spots = new WeakMap<ReviewFile, Map<string, ReviewFindFileSpot | null>>();
const SPOTS_PER_FILE = 4096;
const pathIndexes = new WeakMap<readonly ReviewFile[], Map<string, number>>();

function spotsOf(file: ReviewFile): Map<string, ReviewFindFileSpot | null> {
  let map = spots.get(file);
  if (!map) {
    map = new Map();
    spots.set(file, map);
  }
  return map;
}

function remember(file: ReviewFile, key: string, spot: ReviewFindFileSpot | null): void {
  const map = spotsOf(file);
  if (map.size >= SPOTS_PER_FILE && !map.has(key)) map.clear();
  map.set(key, spot);
}

/** Records where a scan of `file` found `hit`. */
export function seedReviewFindSpot(file: ReviewFile, hit: ReviewFindHit, spot: ReviewFindFileSpot): void {
  remember(file, findElementKey(hit.at), spot);
}

/** For tests: forgets what scans recorded about `file`. */
export function forgetReviewFindSpotsForTest(file: ReviewFile): void {
  spots.delete(file);
}

function fileIndexOf(files: readonly ReviewFile[], path: string): number {
  let map = pathIndexes.get(files);
  if (!map) {
    map = new Map(files.map((file, index) => [file.path, index]));
    pathIndexes.set(files, map);
  }
  return map.get(path) ?? -1;
}

// The row a line tag names in `file`, walked as the scan walks it.
function lineSpot(file: ReviewFile, tag: string): ReviewFindFileSpot | null {
  const body = file.body;
  const walker = new RowWalker(file);
  const fold = /^c(\d+):(\d+)$/.exec(tag);
  let markerRun = 0;
  while (walker.next()) {
    if (walker.type === 'gap') continue;
    const lineIndex = walker.lineIndex;
    if (walker.type !== 'marker') {
      markerRun = 0;
      if (findRowTag(walker) === tag) return { rowKey: findLineKey(file.path, lineIndex), line: lineIndex, place: { kind: 'line', lineIndex } };
      continue;
    }
    const folded = body.fold(lineIndex);
    const { oldAt, newAt } = findMarkerLines(walker);
    if (folded) {
      markerRun = 0;
      const h = fold ? Number(fold[1]) - oldAt : -1;
      if (fold && h >= 0 && h < folded.lines && Number(fold[2]) - newAt === h) {
        return { rowKey: findLineKey(file.path, lineIndex), line: lineIndex, place: { kind: 'fold', foldId: folded.id } };
      }
      continue;
    }
    if (`m${oldAt}:${newAt}:${markerRun}` === tag) return { rowKey: findLineKey(file.path, lineIndex), line: lineIndex, place: { kind: 'line', lineIndex } };
    markerRun += 1;
  }
  return null;
}

// The line a comment's row follows in `file`, placed as the scan and the
// row model place it: -1 above the first row, else the row whose anchor
// its bucket is, else after the last row.
function commentLine(file: ReviewFile, content: ReviewFindLocateContent, isIt: (bucket: readonly InsertRow[]) => boolean): number | null {
  const inserts = reviewInsertsByFile([file], content.drafts, [], content.prThreads, new Set()).get(file.path);
  if (!inserts) return null;
  let anchor: number | null = null;
  for (const [line, bucket] of inserts) {
    if (isIt(bucket)) {
      anchor = line;
      break;
    }
  }
  if (anchor === null) return null;
  if (anchor === 0) return -1;
  const walker = new RowWalker(file);
  let last = -1;
  while (walker.next()) {
    if (walker.type === 'gap') continue;
    last = walker.lineIndex;
    if (walker.anchorLine() === anchor) return walker.lineIndex;
  }
  return last;
}

/** Where `hit` is in `content`, or null when it is not present now: its
 * file is not listed, its line or comment is gone. */
export function locateReviewFindHit(content: ReviewFindLocateContent, hit: ReviewFindHit): ReviewFindLocation | null {
  const fileIndex = fileIndexOf(content.files, hit.path);
  if (fileIndex < 0) return null;
  const file = content.files[fileIndex];
  const at = hit.at;
  if (at.kind === 'header') return { fileIndex, rowKey: findHeaderKey(file.path), line: -2, place: { kind: 'header' } };
  // A comment is present while the content lists it; where it sits is
  // the file's.
  if (at.kind === 'draft' && !content.drafts.some((draft) => draft.id === at.commentId && draft.filePath === file.path)) return null;
  if (at.kind === 'thread' && !content.prThreads.some((thread) => thread.id === at.threadId && at.comment < thread.comments.length)) return null;
  const key = findElementKey(at);
  const map = spotsOf(file);
  let spot = map.get(key);
  if (spot === undefined) {
    if (at.kind === 'line') spot = lineSpot(file, at.tag);
    else {
      const line = at.kind === 'draft'
        ? commentLine(file, content, (bucket) => bucket.some((row) => row.kind === 'comment-thread' && row.threadKey === at.commentId))
        : commentLine(file, content, (bucket) => bucket.some((row) => row.kind === 'pr-thread' && row.thread.id === at.threadId));
      spot = line === null ? null : {
        rowKey: key, line, place: at.kind === 'draft' ? { kind: 'draft', commentId: at.commentId } : { kind: 'thread', threadId: at.threadId },
      };
    }
    remember(file, key, spot);
  }
  return spot ? { fileIndex, ...spot } : null;
}

export interface FindRowTarget {
  index: number;
  /** Pixels from the row's top to the match's line (exact with wrap off). */
  within: number;
}

/** The row showing a match at `at` in `files`, or null when the row model
 * does not show it (a collapsed file, a fold not yet opened). */
export function findHitRow(built: ReviewRowsResult, files: readonly ReviewFile[], at: ReviewFindLocation): FindRowTarget | null {
  const first = built.firstRowOfFile[at.fileIndex] ?? -1;
  if (first < 0) return null;
  const place = at.place;
  if (place.kind === 'header') return { index: first, within: 0 };
  if (place.kind === 'draft' || place.kind === 'thread') {
    const key = place.kind === 'draft' ? `t:${place.commentId}` : `pt:${place.threadId}`;
    const index = built.rowKeys.indexOf(key, first);
    return index >= 0 ? { index, within: 0 } : null;
  }
  if (place.kind !== 'line') return null;
  const file = files[at.fileIndex];
  let block: { index: number; row: LineBlockRow } | null = null;
  for (let index = first + 1; index < built.rows.length && built.fileOfRow[index] === at.fileIndex; index += 1) {
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

/** Whether a match at `at` is at or below a position; never for a match
 * not present now. */
export function findHitAtOrBelow(at: ReviewFindLocation | null, position: { fileIndex: number; line: number } | null): boolean {
  if (!at) return false;
  if (!position) return true;
  return at.fileIndex > position.fileIndex || (at.fileIndex === position.fileIndex && at.line >= position.line);
}
