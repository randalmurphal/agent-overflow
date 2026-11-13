// Where a find match is in the content shown now, and where that sits in
// the review diff's row model: the list row that shows it and the pixels
// from that row's top to the match's line, and the reading position a new
// query starts from. The diff body scrolls.
//
// A published match is a logical identity (reviewFindScan.ts). Whether it
// is present now and where it is (file index, row, `data-find-key`, fold,
// the painter's index among the element's matches) are derived here
// against the content at the moment of use, by the painter, the shift and
// the reveal (`resolveReviewFindHit`): a match resolves when, and only
// when, a scan of its element's text as shown now would yield it, by the
// scan's own matching rule. The reading position, asked over a scan's own
// fresh matches, needs only where a match sits (`placeReviewFindHit`).
//
// The per-file cache holds where each code line tag is, which depends on
// the file alone: a file is immutable, so an entry never outlives what it
// was read from, and the cache dies with the file. Comments (drafts, PR
// threads) are not part of the file and are never cached here.

import type { DiffReviewComment, ReviewThread } from '../types/models';
import type { FindRun } from './findHighlights';
import { RowWalker, splitRowCount } from './patchRows';
import type { ReviewFile } from './patchStore';
import { findableBody } from './reviewComments';
import {
  countFindMatches,
  findDraftKey,
  findHeaderKey,
  findLineKey,
  findMarkerLines,
  findRowTag,
  findThreadCommentKey,
  reviewCommentKey,
  type ReviewFindFileSpot,
  type ReviewFindHit,
  type ReviewFindPlace,
} from './reviewFindScan';
import { prThreadAnchor, REVIEW_LINE_HEIGHT_PX, reviewInsertsByFile, type InsertRow, type LineBlockRow, type ReviewRowsResult } from './reviewRows';

/** The content a match is placed in. */
export interface ReviewFindLocateContent {
  readonly files: readonly ReviewFile[];
  readonly drafts: readonly DiffReviewComment[];
  readonly prThreads: readonly ReviewThread[];
}

/** The content as shown now: what the scan reads (reviewFindScan.ts
 * `ReviewFindContent`), less the work of reading it. */
export interface ReviewFindShown extends ReviewFindLocateContent {
  /** Every run of a PR comment's shown body, shown now or not. */
  commentRuns(body: string, rowKey: string): readonly FindRun[];
  /** The `data-find-key`s of the drafts open in their editor. */
  readonly editing: ReadonlySet<string>;
  /** The hidden lines of each collapsed fold of a conflict file. */
  foldLines?(path: string): ReadonlyMap<number, readonly string[]> | null;
}

/** Where a present match is now. */
export interface ReviewFindResolved {
  readonly fileIndex: number;
  /** The `data-find-key` of the element that shows it. */
  readonly rowKey: string;
  /** Its index among that element's matches, as the painter counts them. */
  readonly ordinal: number;
  readonly place: ReviewFindPlace;
}

/** Whether a match is present now: where; not; or a line whose text the
 * patch budget evicted, which cannot be judged until it is read back. */
export type ReviewFindResolution =
  | { readonly kind: 'present'; readonly at: ReviewFindResolved }
  | { readonly kind: 'absent' }
  | { readonly kind: 'evicted'; readonly file: ReviewFile; readonly lineIndex: number };

const ABSENT: ReviewFindResolution = { kind: 'absent' };

/** Line spots per file, by tag; null for a tag the file does not have.
 * Bounded: a long session of queries over one large file starts its map
 * over. */
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

function remember(file: ReviewFile, tag: string, spot: ReviewFindFileSpot | null): void {
  const map = spotsOf(file);
  if (map.size >= SPOTS_PER_FILE && !map.has(tag)) map.clear();
  map.set(tag, spot);
}

/** Records where a scan of `file` found `hit`, when that is the file's
 * alone to say (a code line). */
export function seedReviewFindSpot(file: ReviewFile, hit: ReviewFindHit, spot: ReviewFindFileSpot): void {
  if (hit.at.kind === 'line') remember(file, hit.at.tag, spot);
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
function walkLineSpot(file: ReviewFile, tag: string): ReviewFindFileSpot | null {
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
        return { rowKey: findLineKey(file.path, lineIndex), line: lineIndex, place: { kind: 'fold', foldId: folded.id }, foldLine: h };
      }
      continue;
    }
    if (`m${oldAt}:${newAt}:${markerRun}` === tag) return { rowKey: findLineKey(file.path, lineIndex), line: lineIndex, place: { kind: 'line', lineIndex } };
    markerRun += 1;
  }
  return null;
}

function lineSpot(file: ReviewFile, tag: string): ReviewFindFileSpot | null {
  const map = spotsOf(file);
  let spot = map.get(tag);
  if (spot === undefined) {
    spot = walkLineSpot(file, tag);
    remember(file, tag, spot);
  }
  return spot;
}

// The line each comment row of `file` follows, by row key, placed as the
// scan and the row model place them: -1 above the first row, else the row
// whose anchor its bucket is, else after the last row.
function commentLines(file: ReviewFile, content: ReviewFindLocateContent): Map<string, number> {
  const out = new Map<string, number>();
  const inserts = reviewInsertsByFile([file], content.drafts, [], content.prThreads, new Set()).get(file.path);
  if (!inserts) return out;
  const keysOf = (bucket: readonly InsertRow[]): string[] => bucket.flatMap((row) => {
    if (row.kind === 'comment-thread') return [findDraftKey(row.threadKey)];
    if (row.kind === 'pr-thread') {
      // The thread's comments, then its held replies at its foot.
      const replies = content.drafts.filter((draft) => draft.status === 'draft' && draft.replyThreadId === row.thread.id).map((draft) => findDraftKey(draft.id));
      return [...row.thread.comments.map((comment) => findThreadCommentKey(row.thread.id, reviewCommentKey(comment))), ...replies];
    }
    return [];
  });
  for (const key of keysOf(inserts.takeHead())) out.set(key, -1);
  const walker = new RowWalker(file);
  let last = -1;
  while (inserts.size > 0 && walker.next()) {
    if (walker.type === 'gap') continue;
    last = walker.lineIndex;
    const bucket = inserts.take(walker.type, walker.oldLine, walker.newLine);
    if (bucket) for (const key of keysOf(bucket)) out.set(key, walker.lineIndex);
  }
  // Left over only when the walk reached the last row.
  for (const key of keysOf(inserts.takeRest())) out.set(key, last);
  return out;
}

/**
 * Where `hit` sits in `content` for document order: its file's index and
 * the line it is in or follows (-2 the header, -1 above the first row), or
 * null when it has no place now. Not whether it is present: that is
 * `resolveReviewFindHit`'s. `memo` holds the comment rows of each file
 * between calls over one content.
 */
export function placeReviewFindHit(content: ReviewFindLocateContent, hit: ReviewFindHit, memo?: Map<string, Map<string, number>>): { fileIndex: number; line: number } | null {
  const fileIndex = fileIndexOf(content.files, hit.path);
  if (fileIndex < 0) return null;
  const file = content.files[fileIndex];
  const at = hit.at;
  if (at.kind === 'header') return { fileIndex, line: -2 };
  if (at.kind === 'line') {
    const spot = lineSpot(file, at.tag);
    return spot ? { fileIndex, line: spot.line } : null;
  }
  let lines = memo?.get(file.path);
  if (!lines) {
    lines = commentLines(file, content);
    memo?.set(file.path, lines);
  }
  const line = lines.get(at.kind === 'draft' ? findDraftKey(at.commentId) : findThreadCommentKey(at.threadId, at.comment));
  return line === undefined ? null : { fileIndex, line };
}

/**
 * Whether `hit` is present in `shown` for `needle` (folded, `findNeedle`),
 * and where: present when a scan of its element's text as shown now (the
 * editing set, the <details> state, the fold state) would yield its
 * identity, with the element and painter's index that scan would give.
 */
export function resolveReviewFindHit(shown: ReviewFindShown, hit: ReviewFindHit, needle: string): ReviewFindResolution {
  const fileIndex = fileIndexOf(shown.files, hit.path);
  if (fileIndex < 0 || needle === '') return ABSENT;
  const file = shown.files[fileIndex];
  const at = hit.at;
  const present = (rowKey: string, place: ReviewFindPlace, ordinal: number): ReviewFindResolution => ({ kind: 'present', at: { fileIndex, rowKey, ordinal, place } });
  // The `occurrence`-th match of one run, or not.
  const has = (text: string) => hit.occurrence < countFindMatches(text, needle);
  if (at.kind === 'header') {
    return hit.run === 0 && has(file.path) ? present(findHeaderKey(file.path), { kind: 'header' }, hit.occurrence) : ABSENT;
  }
  if (at.kind === 'draft') {
    const rowKey = findDraftKey(at.commentId);
    const draft = shown.drafts.find((each) => each.id === at.commentId && each.status === 'draft' && each.filePath === file.path);
    if (!draft || shown.editing.has(rowKey) || hit.run !== 0 || !has(draft.body)) return ABSENT;
    if (!draft.replyThreadId) return present(rowKey, { kind: 'draft', commentId: at.commentId }, hit.occurrence);
    // A held reply shows at its thread's foot, in this file's diff.
    const thread = shown.prThreads.find((each) => each.id === draft.replyThreadId);
    if (!thread || prThreadAnchor(thread).filePath !== file.path) return ABSENT;
    return present(rowKey, { kind: 'thread', threadId: thread.id }, hit.occurrence);
  }
  if (at.kind === 'thread') {
    const thread = shown.prThreads.find((each) => each.id === at.threadId);
    const comment = thread && prThreadAnchor(thread).filePath === file.path
      ? thread.comments.find((each) => reviewCommentKey(each) === at.comment)
      : undefined;
    const body = comment ? findableBody(comment.body) : '';
    const rowKey = findThreadCommentKey(at.threadId, at.comment);
    if (body === '' || shown.editing.has(rowKey)) return ABSENT;
    const runs = shown.commentRuns(body, rowKey);
    const run = runs[hit.run];
    if (!run?.shown || !has(run.text)) return ABSENT;
    let before = 0;
    for (let i = 0; i < hit.run; i += 1) if (runs[i].shown) before += countFindMatches(runs[i].text, needle);
    return present(rowKey, { kind: 'thread', threadId: at.threadId }, before + hit.occurrence);
  }
  const spot = lineSpot(file, at.tag);
  if (!spot || hit.run !== 0) return ABSENT;
  if (spot.place.kind === 'fold') {
    const hidden = shown.foldLines?.(file.path)?.get(spot.place.foldId);
    const text = spot.foldLine === undefined ? undefined : hidden?.[spot.foldLine];
    if (text === undefined || !has(text)) return ABSENT;
    // The fold row's matches are its hidden lines', in order.
    let before = 0;
    for (let h = 0; h < spot.foldLine!; h += 1) before += countFindMatches(hidden![h], needle);
    return present(spot.rowKey, spot.place, before + hit.occurrence);
  }
  if (spot.place.kind !== 'line') return ABSENT;
  const lineIndex = spot.place.lineIndex;
  if (!file.body.residentLine(lineIndex)) return { kind: 'evicted', file, lineIndex };
  const raw = file.body.text(lineIndex);
  // Code text: the +/- or context prefix is the gutter's; a marker is all text.
  const text = at.tag.startsWith('m') ? raw : raw.slice(1);
  return has(text) ? present(spot.rowKey, spot.place, hit.occurrence) : ABSENT;
}

export interface FindRowTarget {
  index: number;
  /** Pixels from the row's top to the match's line (exact with wrap off). */
  within: number;
}

/** The row showing a match at `at` in `files`, or null when the row model
 * does not show it (a collapsed file, a fold not yet opened). */
export function findHitRow(built: ReviewRowsResult, files: readonly ReviewFile[], at: { readonly fileIndex: number; readonly place: ReviewFindPlace }): FindRowTarget | null {
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
export function findHitAtOrBelow(at: { fileIndex: number; line: number } | null, position: { fileIndex: number; line: number } | null): boolean {
  if (!at) return false;
  if (!position) return true;
  return at.fileIndex > position.fileIndex || (at.fileIndex === position.fileIndex && at.line >= position.line);
}
