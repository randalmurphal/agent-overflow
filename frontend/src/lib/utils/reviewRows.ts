import type { DiffReviewComment, ReviewThread } from '../types/models';
import {
  buildSplitDisplayRows,
  type PatchDisplayRow,
  type SplitDisplayRow,
} from './patchFiles';
import {
  anchorLines,
  anchorLinesMatch,
  materializeRows,
  RowWalker,
  type AnchorLines,
  type RowStart,
} from './patchRows';
import type { ReviewFile } from './patchStore';
import type { RowEstimate } from './virtual/types';

export const REVIEW_LINE_HEIGHT_PX = 20;
// The file-header row paints the between-files separation gap INSIDE its
// exact height (gap band + header bar), so the estimate table stays
// truthful: header row = GAP + BAR. The gap band also paints the
// PREVIOUS file's closing cap (rounded bottom border) at its top edge
// (files render as inset card slabs on the darker page background). The
// sticky overlay renders the bar alone.
export const REVIEW_FILE_GAP_PX = 24;
export const REVIEW_FILE_HEADER_BAR_PX = 36;
export const REVIEW_FILE_HEADER_PX = REVIEW_FILE_GAP_PX + REVIEW_FILE_HEADER_BAR_PX;
// Trailing row closing the LAST file's slab (every other file is closed
// by the next header's gap band).
export const REVIEW_SURFACE_END_PX = 8;
export const REVIEW_LINE_BLOCK_MAX_LINES = 32;
const REVIEW_COMMENT_ESTIMATE_PX = 120;

export interface CommentAnchor {
  filePath: string;
  oldLine?: number;
  newLine?: number;
  side: DiffReviewComment['side'];
  selectedText?: string;
}

export type ReviewRow =
  | { kind: 'file-header'; fileIndex: number; path: string }
  | LineBlockRow
  | { kind: 'draft-editor'; fileIndex: number; anchor: CommentAnchor }
  | { kind: 'comment-thread'; fileIndex: number; threadKey: string; anchor: CommentAnchor }
  | { kind: 'pr-thread'; fileIndex: number; thread: ReviewThread; anchor: CommentAnchor; collapsed: boolean; orphaned: boolean }
  | { kind: 'surface-end'; fileIndex: number };

/**
 * Up to REVIEW_LINE_BLOCK_MAX_LINES display rows of one file, as the walk
 * state before its first row and a row count. Rows are materialized from
 * the file's body when the block renders (BlockRowsCache).
 */
export interface LineBlockRow {
  kind: 'line-block';
  fileIndex: number;
  start: RowStart;
  count: number;
  /** Split-view rows the block renders as; present in split mode. */
  splitCount?: number;
  startLine: number;
}

export interface ReviewRowsInput {
  files: readonly ReviewFile[];
  viewMode: 'stacked' | 'split';
  collapsedPaths: ReadonlySet<string>;
  drafts: readonly DiffReviewComment[];
  openEditors: readonly CommentAnchor[];
  prThreads?: readonly ReviewThread[];
  expandedPRThreadIds?: ReadonlySet<string>;
}

export interface ReviewRowsResult {
  rows: ReviewRow[];
  rowKeys: string[];
  fileOfRow: number[];
  firstRowOfFile: number[];
}

type InsertRow =
  | { kind: 'comment-thread'; threadKey: string; anchor: CommentAnchor }
  | { kind: 'draft-editor'; anchor: CommentAnchor }
  | { kind: 'pr-thread'; thread: ReviewThread; anchor: CommentAnchor; collapsed: boolean; orphaned: boolean };

export function buildReviewRows(input: ReviewRowsInput): ReviewRowsResult {
  const rows: ReviewRow[] = [];
  const rowKeys: string[] = [];
  const fileOfRow: number[] = [];
  const firstRowOfFile: number[] = new Array(input.files.length).fill(-1);
  const insertsByFile = buildInsertsByFile(
    input.files,
    input.drafts,
    input.openEditors,
    input.prThreads ?? [],
    input.expandedPRThreadIds ?? new Set(),
  );

  function push(row: ReviewRow, key: string, fileIndex: number): void {
    rows.push(row);
    rowKeys.push(key);
    fileOfRow.push(fileIndex);
  }

  for (let fileIndex = 0; fileIndex < input.files.length; fileIndex += 1) {
    const file = input.files[fileIndex];
    firstRowOfFile[fileIndex] = rows.length;
    push({ kind: 'file-header', fileIndex, path: file.path }, `h:${file.path}`, fileIndex);

    // A collapsed file is just its header row — the header carries the
    // chevron, +/- counts, and kind badge, so no body row is needed.
    if (input.collapsedPaths.has(file.path)) continue;

    const inserts = insertsByFile.get(file.path);
    pushFileLevelInserts(push, fileIndex, inserts);
    pushLineBlocks(push, fileIndex, file, input.viewMode, inserts);
    // Anchors whose line no longer exists in the diff (the source moved
    // under a draft) still render — flushed after the file's blocks, never
    // silently dropped.
    for (const bucket of inserts?.values() ?? []) {
      for (const insert of bucket) pushInsert(push, fileIndex, insert);
    }
    inserts?.clear();
  }

  if (input.files.length > 0) {
    const lastFile = input.files.length - 1;
    push({ kind: 'surface-end', fileIndex: lastFile }, 'end', lastFile);
  }

  return { rows, rowKeys, fileOfRow, firstRowOfFile };
}

export function reviewRowEstimate(result: ReviewRowsResult, wordWrap: boolean): RowEstimate {
  return {
    at(index: number): number {
      const row = result.rows[index];
      if (!row) return REVIEW_LINE_HEIGHT_PX;
      if (row.kind === 'file-header') return REVIEW_FILE_HEADER_PX;
      if (row.kind === 'surface-end') return REVIEW_SURFACE_END_PX;
      // Split view renders side pairs, so the visual row count is
      // splitRows.length, not the stacked display-row count.
      if (row.kind === 'line-block') return (row.splitCount ?? row.count) * REVIEW_LINE_HEIGHT_PX;
      return REVIEW_COMMENT_ESTIMATE_PX;
    },
    isExact(index: number): boolean {
      const row = result.rows[index];
      if (row?.kind === 'surface-end') return true;
      if (wordWrap) return false;
      return row?.kind === 'file-header' || row?.kind === 'line-block';
    },
  };
}

function buildInsertsByFile(
  files: readonly ReviewFile[],
  drafts: readonly DiffReviewComment[],
  openEditors: readonly CommentAnchor[],
  prThreads: readonly ReviewThread[],
  expandedPRThreadIds: ReadonlySet<string>,
): Map<string, Map<number, InsertRow[]>> {
  const byFile = new Map<string, Map<number, InsertRow[]>>();
  const filesByPath = new Map<string, ReviewFile>();
  for (const file of files) filesByPath.set(file.path, file);
  // One walk per file that has threads, not one per thread.
  const linesByPath = new Map<string, AnchorLines | null>();
  function linesOf(path: string): AnchorLines | null {
    let lines = linesByPath.get(path);
    if (lines === undefined) {
      const file = filesByPath.get(path);
      lines = file ? anchorLines(file) : null;
      linesByPath.set(path, lines);
    }
    return lines;
  }

  function add(filePath: string, line: number, row: InsertRow): void {
    const byLine = byFile.get(filePath) ?? new Map<number, InsertRow[]>();
    const bucket = byLine.get(line) ?? [];
    bucket.push(row);
    byLine.set(line, bucket);
    byFile.set(filePath, byLine);
  }

  for (const comment of drafts) {
    if (comment.status !== 'draft') continue;
    const anchor = commentAnchor(comment);
    add(comment.filePath, anchorLine(anchor), {
      kind: 'comment-thread',
      threadKey: comment.id,
      anchor,
    });
  }

  for (const anchor of openEditors) {
    add(anchor.filePath, anchorLine(anchor), {
      kind: 'draft-editor',
      anchor,
    });
  }

  for (const thread of prThreads) {
    const anchor = prThreadAnchor(thread);
    const lines = anchor.side === 'file' ? null : linesOf(anchor.filePath);
    const anchored = lines !== null && anchorLinesMatch(lines, anchor);
    const orphaned = thread.isOutdated || !anchored;
    add(anchor.filePath, orphaned ? 0 : anchorLine(anchor), {
      kind: 'pr-thread',
      thread,
      anchor,
      collapsed: (thread.isResolved || thread.isOutdated) && !expandedPRThreadIds.has(thread.id),
      orphaned,
    });
  }

  for (const byLine of byFile.values()) {
    for (const bucket of byLine.values()) {
      bucket.sort(compareInsertRows);
    }
  }

  return byFile;
}

function pushFileLevelInserts(
  push: (row: ReviewRow, key: string, fileIndex: number) => void,
  fileIndex: number,
  inserts: Map<number, InsertRow[]> | undefined,
): void {
  for (const insert of inserts?.get(0) ?? []) {
    pushInsert(push, fileIndex, insert);
  }
  inserts?.delete(0);
}

/** A block of a file with no inserts, reusable at any file index. */
interface BlockLayout {
  start: RowStart;
  count: number;
  splitCount?: number;
  startLine: number;
  key: string;
}

// A file's blocks without inserts depend only on the file and the view
// mode, so a rebuild (a collapse toggle, a thread refresh, more files
// arriving) walks only the files whose blocks changed.
const plainLayouts = new WeakMap<ReviewFile, { split: boolean; blocks: BlockLayout[] }>();

// Blocks hold up to REVIEW_LINE_BLOCK_MAX_LINES rows, aligned to multiples
// of it within the file, and end early after a row that has inserts.
function pushLineBlocks(
  push: (row: ReviewRow, key: string, fileIndex: number) => void,
  fileIndex: number,
  file: ReviewFile,
  viewMode: 'stacked' | 'split',
  inserts: Map<number, InsertRow[]> | undefined,
): void {
  const split = viewMode === 'split';
  if (!inserts || inserts.size === 0) {
    let layout = plainLayouts.get(file);
    if (!layout || layout.split !== split) {
      const blocks: BlockLayout[] = [];
      walkLineBlocks(file, split, undefined, (block) => blocks.push(block), () => {});
      layout = { split, blocks };
      plainLayouts.set(file, layout);
    }
    for (const block of layout.blocks) {
      const row: LineBlockRow = { kind: 'line-block', fileIndex, start: block.start, count: block.count, startLine: block.startLine };
      if (block.splitCount !== undefined) row.splitCount = block.splitCount;
      push(row, block.key, fileIndex);
    }
    return;
  }
  walkLineBlocks(
    file,
    split,
    inserts,
    (block) => {
      const row: LineBlockRow = { kind: 'line-block', fileIndex, start: block.start, count: block.count, startLine: block.startLine };
      if (block.splitCount !== undefined) row.splitCount = block.splitCount;
      push(row, block.key, fileIndex);
    },
    (insert) => pushInsert(push, fileIndex, insert),
  );
}

function walkLineBlocks(
  file: ReviewFile,
  split: boolean,
  inserts: Map<number, InsertRow[]> | undefined,
  onBlock: (block: BlockLayout) => void,
  onInsert: (insert: InsertRow) => void,
): void {
  const walker = new RowWalker(file);
  let start = walker.snapshot();
  let count = 0;
  let firstId = '';
  let startLine = 0;
  let splitRows = 0;
  let dels = 0;
  let adds = 0;

  function close(): void {
    if (count === 0) return;
    const block: BlockLayout = { start, count, startLine, key: '' };
    if (split) {
      if (dels > 0) splitRows += Math.max(dels, adds);
      block.splitCount = splitRows;
    }
    // Keyed by the first display row's id, not its line number: a block
    // starting at a deleted row (oldLine N) and one starting at a new row
    // (newLine N) would collide on N, and duplicate keys crash the keyed
    // each. Row ids are fixed per file, so re-blocking (an earlier block
    // splitting at a new anchor) never changes a later block's key. The
    // row count rides along so a block whose CONTENT changed (the half
    // left behind by an anchor split) reads as a new row and remeasures,
    // instead of keeping a word-wrap measurement taken at its old length.
    block.key = `b:${file.path}:${firstId}:${count}`;
    onBlock(block);
    start = walker.snapshot();
    count = 0;
    splitRows = 0;
    dels = 0;
    adds = 0;
  }

  while (walker.next()) {
    if (count === 0) {
      firstId = walker.rowId();
      startLine = walker.anchorLine();
    }
    count += 1;
    if (split) {
      if (walker.type === 'del') {
        if (adds > 0) {
          splitRows += Math.max(dels, adds);
          dels = 0;
          adds = 0;
        }
        dels += 1;
      } else if (walker.type === 'add') {
        if (dels > 0) adds += 1;
        else splitRows += 1;
      } else {
        if (dels > 0) splitRows += Math.max(dels, adds);
        dels = 0;
        adds = 0;
        splitRows += 1;
      }
    }
    const line = walker.anchorLine();
    const lineInserts = inserts?.get(line);
    const chunkEnd = (walker.row + 1) % REVIEW_LINE_BLOCK_MAX_LINES === 0;
    if (!chunkEnd && !lineInserts?.length) continue;
    close();
    for (const insert of lineInserts ?? []) onInsert(insert);
    // Consume the bucket: a deleted row (oldLine N) and a later row
    // (newLine N) both report line N, and attaching the same insert
    // twice would duplicate its row key.
    inserts?.delete(line);
  }
  close();
}

export interface MaterializedBlock {
  file: ReviewFile;
  rows: PatchDisplayRow[];
  splitRows?: SplitDisplayRow[];
}

/** Blocks one review surface materialized, with room for several screens. */
const BLOCK_ROWS_CACHE_MAX = 512;

let nextFileId = 1;
const fileIds = new WeakMap<ReviewFile, number>();

/**
 * The display rows of the blocks a review surface renders, built from the
 * file's body on first use. Keyed by file and position rather than block
 * object, so a rebuilt row model keeps the row objects of every block it
 * shares with the previous one and keyed rows do not re-render. Holds the
 * most recently used blocks of the files the surface shows; an evicted
 * block is rebuilt when it renders again.
 */
export class BlockRowsCache {
  private readonly blocks = new Map<string, MaterializedBlock>();

  get(file: ReviewFile, block: LineBlockRow): MaterializedBlock {
    let fileId = fileIds.get(file);
    if (fileId === undefined) {
      fileId = nextFileId++;
      fileIds.set(file, fileId);
    }
    const key = `${fileId}:${block.start.row}:${block.count}:${block.splitCount === undefined ? 0 : 1}`;
    const cached = this.blocks.get(key);
    if (cached) {
      this.blocks.delete(key);
      this.blocks.set(key, cached);
      return cached;
    }
    const rows = materializeRows(file, block.start, block.count);
    const out: MaterializedBlock = { file, rows };
    if (block.splitCount !== undefined) out.splitRows = buildSplitDisplayRows(rows);
    if (this.blocks.size >= BLOCK_ROWS_CACHE_MAX) {
      const oldest = this.blocks.keys().next().value;
      if (oldest !== undefined) this.blocks.delete(oldest);
    }
    this.blocks.set(key, out);
    return out;
  }

  /** Drops the blocks of files the surface no longer shows. */
  retain(files: readonly ReviewFile[]): void {
    if (this.blocks.size === 0) return;
    const shown = new Set(files);
    for (const [key, block] of this.blocks) {
      if (!shown.has(block.file)) this.blocks.delete(key);
    }
  }

  get size(): number {
    return this.blocks.size;
  }
}

function pushInsert(
  push: (row: ReviewRow, key: string, fileIndex: number) => void,
  fileIndex: number,
  insert: InsertRow,
): void {
  if (insert.kind === 'comment-thread') {
    push({
      kind: 'comment-thread',
      fileIndex,
      threadKey: insert.threadKey,
      anchor: insert.anchor,
    }, `t:${insert.threadKey}`, fileIndex);
    return;
  }
  if (insert.kind === 'pr-thread') {
    push({
      kind: 'pr-thread',
      fileIndex,
      thread: insert.thread,
      anchor: insert.anchor,
      collapsed: insert.collapsed,
      orphaned: insert.orphaned,
    }, `pt:${insert.thread.id}`, fileIndex);
    return;
  }
  push({
    kind: 'draft-editor',
    fileIndex,
    anchor: insert.anchor,
  }, `d:${anchorKey(insert.anchor)}`, fileIndex);
}

function prThreadAnchor(thread: ReviewThread): CommentAnchor {
  const line = thread.line ?? undefined;
  const side = thread.side === 'left' || thread.side === 'old' ? 'old' : 'new';
  if (!line) return { filePath: thread.path, side: 'file' };
  return side === 'old'
    ? { filePath: thread.path, side: 'old', oldLine: line }
    : { filePath: thread.path, side: 'new', newLine: line };
}

function commentAnchor(comment: DiffReviewComment): CommentAnchor {
  return {
    filePath: comment.filePath,
    oldLine: comment.oldLine,
    newLine: comment.newLine,
    side: comment.side,
    selectedText: comment.selectedText,
  };
}

function anchorLine(anchor: Pick<CommentAnchor, 'oldLine' | 'newLine' | 'side'>): number {
  if (anchor.side === 'file') return 0;
  if (anchor.side === 'old') return anchor.oldLine || 0;
  return anchor.newLine || anchor.oldLine || 0;
}

export function anchorKey(anchor: CommentAnchor): string {
  return `${anchor.filePath}:${anchor.side}:${anchor.oldLine || 0}:${anchor.newLine || 0}`;
}

function compareInsertRows(a: InsertRow, b: InsertRow): number {
  const aKey = insertKey(a);
  const bKey = insertKey(b);
  return aKey.localeCompare(bKey);
}

function insertKey(row: InsertRow): string {
  if (row.kind === 'comment-thread') return `0:${row.threadKey}`;
  if (row.kind === 'pr-thread') return `1:${row.thread.id}`;
  return `2:${anchorKey(row.anchor)}`;
}
