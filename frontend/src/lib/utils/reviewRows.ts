import type { DiffReviewComment, ReviewThread } from '../types/models';
import {
  buildPatchDisplayRows,
  buildSplitDisplayRows,
  type PatchDisplayRow,
  type PatchFile,
  type SplitDisplayRow,
} from './patchFiles';
import type { RowEstimate } from './virtual/types';

export const REVIEW_LINE_HEIGHT_PX = 20;
// The file-header row paints the between-files separation gap INSIDE its
// exact height (gap band + header bar), so the estimate table stays
// truthful: header row = GAP + BAR. The sticky overlay renders the bar
// alone.
export const REVIEW_FILE_GAP_PX = 16;
export const REVIEW_FILE_HEADER_BAR_PX = 36;
export const REVIEW_FILE_HEADER_PX = REVIEW_FILE_GAP_PX + REVIEW_FILE_HEADER_BAR_PX;
export const REVIEW_COLLAPSED_ROW_PX = 36;
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
  | { kind: 'file-collapsed'; fileIndex: number; path: string }
  | { kind: 'line-block'; fileIndex: number; rows: PatchDisplayRow[]; splitRows?: SplitDisplayRow[]; startLine: number }
  | { kind: 'draft-editor'; fileIndex: number; anchor: CommentAnchor }
  | { kind: 'comment-thread'; fileIndex: number; threadKey: string; anchor: CommentAnchor }
  | { kind: 'pr-thread'; fileIndex: number; thread: ReviewThread; anchor: CommentAnchor; collapsed: boolean; orphaned: boolean };

export interface ReviewRowsInput {
  files: PatchFile[];
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

    if (input.collapsedPaths.has(file.path)) {
      push({ kind: 'file-collapsed', fileIndex, path: file.path }, `c:${file.path}`, fileIndex);
      continue;
    }

    const inserts = insertsByFile.get(file.path);
    pushFileLevelInserts(push, fileIndex, inserts);
    const displayRows = buildPatchDisplayRows(file.lines);
    pushLineBlocks(push, fileIndex, file.path, displayRows, input.viewMode, inserts);
    // Anchors whose line no longer exists in the diff (the source moved
    // under a draft) still render — flushed after the file's blocks, never
    // silently dropped.
    for (const bucket of inserts?.values() ?? []) {
      for (const insert of bucket) pushInsert(push, fileIndex, insert);
    }
    inserts?.clear();
  }

  return { rows, rowKeys, fileOfRow, firstRowOfFile };
}

export function reviewRowEstimate(result: ReviewRowsResult, wordWrap: boolean): RowEstimate {
  return {
    at(index: number): number {
      const row = result.rows[index];
      if (!row) return REVIEW_LINE_HEIGHT_PX;
      if (row.kind === 'file-header') return REVIEW_FILE_HEADER_PX;
      if (row.kind === 'file-collapsed') return REVIEW_COLLAPSED_ROW_PX;
      // Split view renders side pairs, so the visual row count is
      // splitRows.length, not the stacked display-row count.
      if (row.kind === 'line-block') return (row.splitRows ?? row.rows).length * REVIEW_LINE_HEIGHT_PX;
      return REVIEW_COMMENT_ESTIMATE_PX;
    },
    isExact(index: number): boolean {
      if (wordWrap) return false;
      const row = result.rows[index];
      return row?.kind === 'file-header' || row?.kind === 'file-collapsed' || row?.kind === 'line-block';
    },
  };
}

function buildInsertsByFile(
  files: readonly PatchFile[],
  drafts: readonly DiffReviewComment[],
  openEditors: readonly CommentAnchor[],
  prThreads: readonly ReviewThread[],
  expandedPRThreadIds: ReadonlySet<string>,
): Map<string, Map<number, InsertRow[]>> {
  const byFile = new Map<string, Map<number, InsertRow[]>>();
  const displayRowsByFile = new Map<string, PatchDisplayRow[]>();
  for (const file of files) {
    displayRowsByFile.set(file.path, buildPatchDisplayRows(file.lines));
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
    const rows = displayRowsByFile.get(anchor.filePath) ?? [];
    const anchored = anchor.side !== 'file' && rows.some((row) => displayRowMatchesAnchor(row, anchor));
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

function pushLineBlocks(
  push: (row: ReviewRow, key: string, fileIndex: number) => void,
  fileIndex: number,
  path: string,
  displayRows: PatchDisplayRow[],
  viewMode: 'stacked' | 'split',
  inserts: Map<number, InsertRow[]> | undefined,
): void {
  for (let chunkStart = 0; chunkStart < displayRows.length; chunkStart += REVIEW_LINE_BLOCK_MAX_LINES) {
    const chunkEnd = Math.min(chunkStart + REVIEW_LINE_BLOCK_MAX_LINES, displayRows.length);
    let blockStart = chunkStart;
    for (let index = chunkStart; index < chunkEnd; index += 1) {
      const row = displayRows[index];
      if (!row) continue;
      const isChunkEnd = index === chunkEnd - 1;
      const line = displayRowLine(row);
      const lineInserts = inserts?.get(line) ?? [];
      if (!isChunkEnd && lineInserts.length === 0) continue;
      pushLineBlock(push, fileIndex, path, displayRows.slice(blockStart, index + 1), viewMode);
      for (const insert of lineInserts) {
        pushInsert(push, fileIndex, insert);
      }
      // Consume the bucket: a deleted row (oldLine N) and a later row
      // (newLine N) both report line N, and attaching the same insert
      // twice would duplicate its row key.
      inserts?.delete(line);
      blockStart = index + 1;
    }
  }
}

function pushLineBlock(
  push: (row: ReviewRow, key: string, fileIndex: number) => void,
  fileIndex: number,
  path: string,
  blockRows: PatchDisplayRow[],
  viewMode: 'stacked' | 'split',
): void {
  if (blockRows.length === 0) return;
  const startLine = displayRowLine(blockRows[0]);
  const row: ReviewRow = {
    kind: 'line-block',
    fileIndex,
    rows: blockRows,
    startLine,
  };
  if (viewMode === 'split') row.splitRows = buildSplitDisplayRows(blockRows);
  // Keyed by the first display row's id, not its line number: a block
  // starting at a deleted row (oldLine N) and one starting at a new row
  // (newLine N) would collide on N, and duplicate keys crash the keyed
  // each. Row ids are built once per file, so re-blocking (an earlier
  // block splitting at a new anchor) never changes a later block's key.
  // The row count rides along so a block whose CONTENT changed (the half
  // left behind by an anchor split) reads as a new row and remeasures,
  // instead of keeping a word-wrap measurement taken at its old length.
  push(row, `b:${path}:${blockRows[0].id}:${blockRows.length}`, fileIndex);
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

function displayRowMatchesAnchor(row: PatchDisplayRow, anchor: CommentAnchor): boolean {
  if (anchor.side === 'old') return row.side === 'old' && row.oldLine === anchor.oldLine;
  if (anchor.side === 'new') return row.side === 'new' && row.newLine === anchor.newLine;
  if (anchor.side === 'context') return row.oldLine === anchor.oldLine && row.newLine === anchor.newLine;
  return false;
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

function displayRowLine(row: PatchDisplayRow): number {
  if (row.side === 'old') return row.oldLine;
  return row.newLine || row.oldLine;
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
