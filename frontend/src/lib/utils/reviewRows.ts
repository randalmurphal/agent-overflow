import type { DiffReviewComment, DiffReviewLineSide, ReviewThread } from '../types/models';
import {
  buildSplitDisplayRows,
  type PatchDisplayRow,
  type SplitDisplayRow,
} from './patchFiles';
import {
  anchorLines,
  anchorLinesMatch,
  InsertSlots,
  materializeRows,
  RowWalker,
  type AnchorLines,
  type RowStart,
} from './patchRows';
import { reportFrontendDiagnostic } from './frontendErrorCapture';
import type { LineRange } from './reviewRange';
import { patchMemory, patchTextVersion } from './patchMemory.svelte';
import { PatchTextLost, type ReviewFile } from './patchStore';
import type { RowEstimate } from './virtual/types';
import { imageRowHeight, imageSides, isImagePath, REVIEW_BINARY_ROW_PX } from './reviewImageRows';

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
// The lead row before the first file: the PR overview (Description,
// Conversation) or, with one commit picked, that commit's card. Priors
// remember each one's measured height by key across reopens; these are
// the first-render guesses.
export const REVIEW_OVERVIEW_ESTIMATE_PX = 480;
export const REVIEW_OVERVIEW_ROW_KEY = 'overview';
export const REVIEW_COMMIT_CARD_ESTIMATE_PX = 96;
export const REVIEW_COMMIT_CARD_ROW_KEY = 'commit';
/** What leads the list: the PR overview or the picked commit's card. */
export type ReviewLeadRow = 'overview' | 'commit';

export interface CommentAnchor {
  filePath: string;
  oldLine?: number;
  newLine?: number;
  side: DiffReviewLineSide;
  selectedText?: string;
}

/**
 * Where an open new-comment editor is (stores/reviewPane.svelte.ts):
 * `placed` on its proven line; `detached` in its file after a suggested
 * row, until the reader picks a line; `gone` when its file is not in the
 * loaded diff, in the pane-level group.
 */
export type EditorState = 'placed' | 'detached' | 'gone';

/** An open new-comment editor as the rows place it. */
export interface EditorRow {
  /** Minted when the editor opens: keys its text, focus and row. */
  readonly id: string;
  readonly filePath: string;
  readonly side: CommentAnchor['side'];
  readonly state: EditorState;
  /** Placed: the line it saves on. */
  readonly anchor: CommentAnchor | null;
  /** Detached: the row it renders after and "Use this line" takes. It
   * proves nothing; null when the file has no row on its side. */
  readonly suggested: CommentAnchor | null;
  /** Armed: the next line picked takes it ("Move"). */
  readonly armed: boolean;
  /** Placed: which of the rows its coordinate names it is on, from 0. */
  readonly occurrence: number;
  /** Placed on a coordinate several rows of its file have: it cannot be
   * saved there (`namesOneRow`). */
  readonly repeated: boolean;
  /** The diff on screen it was placed in (`ShownDiff.generation`): an
   * action the reader took on it is valid only there. */
  readonly generation: number;
  /** Placed on a range (shift+click): the lines it spans, its `anchor`
   * the last of them. Absent for one line. */
  readonly range?: LineRange;
  /** Why the last shift+click did not make a range. */
  readonly rangeRefusal?: string;
}

/** The row key of the group of editors whose file is not in the diff. */
export const REVIEW_GONE_GROUP_ROW_KEY = 'gone-editors';

export type ReviewRow =
  /** The lead row before the first file (fileIndex -1): it scrolls with
   * the diff and is measured like any other row. */
  | { kind: 'overview'; fileIndex: -1; lead: ReviewLeadRow }
  | { kind: 'file-header'; fileIndex: number; path: string }
  | LineBlockRow
  /** A new-comment editor: in its file (fileIndex), or in the group of
   * editors whose file is not in the diff (fileIndex -1). */
  | { kind: 'draft-editor'; fileIndex: number; editor: EditorRow }
  /** The head of that group (fileIndex -1). */
  | { kind: 'gone-group'; fileIndex: -1; count: number }
  | { kind: 'comment-thread'; fileIndex: number; threadKey: string; anchor: CommentAnchor }
  | { kind: 'pr-thread'; fileIndex: number; thread: ReviewThread; anchor: CommentAnchor; collapsed: boolean; orphaned: boolean }
  | { kind: 'surface-end'; fileIndex: number }
  /** A binary file's images or sizes, or a text SVG shown rendered, in
   * place of its lines; `heightPx` is reserved (utils/reviewImageRows.ts). */
  | { kind: 'media'; fileIndex: number; image: boolean; rendered: boolean; heightPx: number };

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
  openEditors: readonly EditorRow[];
  prThreads?: readonly ReviewThread[];
  expandedPRThreadIds?: ReadonlySet<string>;
  /** Lead the list with the PR overview or a commit card. */
  overview?: ReviewLeadRow;
  /** Text SVGs shown rendered instead of their lines. */
  renderedPaths?: ReadonlySet<string>;
  /** The diff records no old side (edits): images show their new side. */
  newSideOnly?: boolean;
  /** Binary files get a media row (images, sizes) instead of git's line. */
  media?: boolean;
}

export interface ReviewRowsResult {
  rows: ReviewRow[];
  rowKeys: string[];
  fileOfRow: number[];
  firstRowOfFile: number[];
}

/** A row inserted after a line of a file: a comment, a PR thread or a
 * draft editor. */
export type InsertRow =
  | { kind: 'comment-thread'; threadKey: string; anchor: CommentAnchor }
  /** `order`: the editor's place among the open editors. */
  | { kind: 'draft-editor'; editor: EditorRow; order: number }
  | { kind: 'pr-thread'; thread: ReviewThread; anchor: CommentAnchor; collapsed: boolean; orphaned: boolean };

export function buildReviewRows(input: ReviewRowsInput): ReviewRowsResult {
  const rows: ReviewRow[] = [];
  const rowKeys: string[] = [];
  const fileOfRow: number[] = [];
  const firstRowOfFile: number[] = Array.from({ length: input.files.length }, () => -1);
  const insertsByFile = reviewInsertsByFile(
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

  if (input.overview) {
    push(
      { kind: 'overview', fileIndex: -1, lead: input.overview },
      input.overview === 'commit' ? REVIEW_COMMIT_CARD_ROW_KEY : REVIEW_OVERVIEW_ROW_KEY,
      -1,
    );
  }

  // Editors whose file is not in the diff lead the files, under the
  // group's head. An editor of a file the filter hides is hidden with it.
  const grouped = input.openEditors.filter((editor) => editor.state === 'gone');
  if (grouped.length > 0) {
    push({ kind: 'gone-group', fileIndex: -1, count: grouped.length }, REVIEW_GONE_GROUP_ROW_KEY, -1);
    for (const editor of grouped) push({ kind: 'draft-editor', fileIndex: -1, editor }, `d:${editor.id}`, -1);
  }

  for (let fileIndex = 0; fileIndex < input.files.length; fileIndex += 1) {
    const file = input.files[fileIndex];
    firstRowOfFile[fileIndex] = rows.length;
    push({ kind: 'file-header', fileIndex, path: file.path }, `h:${file.path}`, fileIndex);

    // A collapsed file is just its header row — the header carries the
    // chevron, +/- counts, and kind badge, so no body row is needed.
    if (input.collapsedPaths.has(file.path)) continue;

    const inserts = insertsByFile.get(file.path);
    for (const insert of inserts?.takeHead() ?? []) pushInsert(push, fileIndex, insert);
    const rendered = input.renderedPaths?.has(file.path) ?? false;
    if (input.media && (file.header?.binary || rendered)) {
      const image = rendered || isImagePath(file.path);
      const sides = imageSides(file, input.newSideOnly ?? false).length;
      push({
        kind: 'media', fileIndex, image, rendered,
        heightPx: image && sides > 0 ? imageRowHeight(sides, input.viewMode === 'split') : REVIEW_BINARY_ROW_PX,
      }, `m:${file.path}`, fileIndex);
    } else {
      pushLineBlocks(push, fileIndex, file, input.viewMode, inserts);
    }
    // Anchors whose line no longer exists in the diff (the source moved
    // under a draft) still render — flushed after the file's blocks, never
    // silently dropped.
    for (const insert of inserts?.takeRest() ?? []) pushInsert(push, fileIndex, insert);
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
      if (row.kind === 'overview') return row.lead === 'commit' ? REVIEW_COMMIT_CARD_ESTIMATE_PX : REVIEW_OVERVIEW_ESTIMATE_PX;
      if (row.kind === 'media') return row.heightPx;
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

/**
 * The inserted rows of each file, slotted after the rows they follow
 * (`InsertSlots`). The row model takes them as it walks a file's rows; so
 * does the find scan.
 */
export function reviewInsertsByFile(
  files: readonly ReviewFile[],
  drafts: readonly DiffReviewComment[],
  openEditors: readonly EditorRow[],
  prThreads: readonly ReviewThread[],
  expandedPRThreadIds: ReadonlySet<string>,
): Map<string, InsertSlots<InsertRow>> {
  const byFile = new Map<string, InsertSlots<InsertRow>>();
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
  function slotsOf(path: string): InsertSlots<InsertRow> {
    let slots = byFile.get(path);
    if (!slots) {
      slots = new InsertSlots(compareInsertRows);
      byFile.set(path, slots);
    }
    return slots;
  }

  // A draft follows the row its saved numbers name; one no row has is
  // flushed after the file's rows. A draft saved before unchanged lines
  // carried both numbers names its row by one, as a forge thread does; it
  // shows there, and the review leaves it out (draftAnchorExists).
  // A held reply renders at the foot of its thread, not on a line.
  for (const comment of drafts) {
    if (comment.status !== 'draft' || comment.side === 'reply') continue;
    const anchor = commentAnchor(comment);
    slotsOf(comment.filePath).add({ kind: 'comment-thread', threadKey: comment.id, anchor }, anchor, true);
  }

  // A detached editor follows its suggested row (under the header when it
  // has none); a gone one is the row model's group, not a file's insert.
  openEditors.forEach((editor, order) => {
    if (editor.state === 'gone' || !filesByPath.has(editor.filePath)) return;
    const row: InsertRow = { kind: 'draft-editor', editor, order };
    const at = editor.state === 'placed' ? editor.anchor : editor.suggested;
    if (at === null) slotsOf(editor.filePath).addHead(row);
    else slotsOf(editor.filePath).add(row, at, false, editor.state === 'placed' ? editor.occurrence : 0);
  });

  for (const thread of prThreads) {
    const anchor = prThreadAnchor(thread);
    const lines = anchor.side === 'file' ? null : linesOf(anchor.filePath);
    // A forge thread names one side's number: it may be an unchanged row.
    const anchored = lines !== null && anchorLinesMatch(lines, anchor, true);
    // Placement follows the anchor alone: a thread the forge calls outdated
    // whose line still matches the diff stays on that line (GitLab keeps
    // the position through pushes); one whose line is gone sits under the
    // file header. Only settled threads start folded.
    const orphaned = !anchored;
    const row: InsertRow = {
      kind: 'pr-thread',
      thread,
      anchor,
      collapsed: thread.isResolved && !expandedPRThreadIds.has(thread.id),
      orphaned,
    };
    if (orphaned) slotsOf(anchor.filePath).addHead(row);
    else slotsOf(anchor.filePath).add(row, anchor, true);
  }

  return byFile;
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
  inserts: InsertSlots<InsertRow> | undefined,
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
  inserts: InsertSlots<InsertRow> | undefined,
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
    const lineInserts = inserts?.take(walker.type, walker.oldLine, walker.newLine) ?? null;
    const chunkEnd = (walker.row + 1) % REVIEW_LINE_BLOCK_MAX_LINES === 0;
    if (!chunkEnd && !lineInserts) continue;
    close();
    for (const insert of lineInserts ?? []) onInsert(insert);
  }
  close();
}

export interface MaterializedBlock {
  file: ReviewFile;
  rows: PatchDisplayRow[];
  splitRows?: SplitDisplayRow[];
}

function materializedBlock(file: ReviewFile, block: LineBlockRow, rows: PatchDisplayRow[]): MaterializedBlock {
  const out: MaterializedBlock = { file, rows };
  if (block.splitCount !== undefined) out.splitRows = buildSplitDisplayRows(rows);
  return out;
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
 *
 * A block over evicted text renders placeholder rows until its text is
 * read again. It is then built with that text pinned, so it completes
 * even when the text on screen alone passes the memory budget. Rows own
 * their text (patchRows.ts), so a kept block holds no chunk.
 */
export class BlockRowsCache {
  private readonly blocks = new Map<string, MaterializedBlock>();
  private readonly filling = new Set<string>();
  private shown: Set<ReviewFile> | null = null;

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
    const built = materializeRows(file, block.start, block.count);
    const out = materializedBlock(file, block, built.rows);
    if (!built.complete) {
      // Rendered again when the filled block lands.
      patchTextVersion();
      this.fill(key, file, block, built.lines);
      return out;
    }
    this.keep(key, out);
    return out;
  }

  /** Drops the blocks of files the surface no longer shows. */
  retain(files: readonly ReviewFile[]): void {
    const shown = new Set(files);
    this.shown = shown;
    for (const [key, block] of this.blocks) {
      if (!shown.has(block.file)) this.blocks.delete(key);
    }
  }

  // A failed read marks its store lost, and the store's owner reads the
  // diff again; the placeholders stay until then. Anything else is a
  // defect, reported.
  private fill(key: string, file: ReviewFile, block: LineBlockRow, lines: { start: number; end: number }): void {
    if (this.filling.has(key)) return;
    this.filling.add(key);
    file.body.whenResident(() => materializeRows(file, block.start, block.count), lines.start, lines.end)
      .then((built) => {
        if (!built.complete || this.blocks.has(key) || (this.shown && !this.shown.has(file))) return;
        this.keep(key, materializedBlock(file, block, built.rows));
        patchMemory.restored();
      })
      .catch((error: unknown) => {
        if (!(error instanceof PatchTextLost)) {
          reportFrontendDiagnostic('review diff: block rows could not be built', String(error));
        }
      })
      .finally(() => this.filling.delete(key));
  }

  private keep(key: string, block: MaterializedBlock): void {
    if (this.blocks.size >= BLOCK_ROWS_CACHE_MAX) {
      const oldest = this.blocks.keys().next().value;
      if (oldest !== undefined) this.blocks.delete(oldest);
    }
    this.blocks.set(key, block);
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
    editor: insert.editor,
  }, `d:${insert.editor.id}`, fileIndex);
}

/** Where a PR thread sits in the diff: its file and line, or its file. */
export function prThreadAnchor(thread: ReviewThread): CommentAnchor {
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
    side: comment.side === 'reply' ? 'file' : comment.side,
    selectedText: comment.selectedText,
  };
}

/** The line an anchor names, for finding an editor already open on it. */
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
  return `2:${String(row.order).padStart(8, '0')}`;
}
