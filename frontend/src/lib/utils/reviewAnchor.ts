import type { PatchDisplayRow } from './patchFiles';
import { nearestLineRow } from './patchRows';
import type { ReviewFile } from './patchStore';
import { comparePathsTreeOrder } from './reviewTree';
import {
  REVIEW_LINE_HEIGHT_PX,
  type BlockRowsCache,
  type LineBlockRow,
  type ReviewRowsResult,
} from './reviewRows';

// Reading-anchor math for the review diff body: capture the content
// position under the viewport top as (file, line, pixel delta) and
// re-locate it in a rebuilt row model, so a reload / gap expansion /
// PR-thread refresh never moves the line being read. Pure functions
// over the row model + a row-geometry view (the virtualizer handle in
// production, prefix-summed estimates in tests).

export interface ReadingAnchor {
  path: string;
  /** 0 anchors the file header itself. */
  line: number;
  side: 'new' | 'old';
  /** Pixels from the anchored line's top to the viewport top. */
  delta: number;
}

/** The slice of the virtualizer handle the anchor math needs. Offsets
 * exist only for held rows (LongListVirtualizer). */
export interface RowGeometry {
  findItemIndex(offset: number): number;
  getItemOffset(index: number): number;
  holds(index: number): boolean;
}

/** A row and the pixels from its top to the viewport top. */
export interface ReadingPosition {
  index: number;
  offset: number;
}

function visualDisplayRow(blocks: BlockRowsCache, file: ReviewFile, row: LineBlockRow, index: number): PatchDisplayRow | null {
  const { rows, splitRows } = blocks.get(file, row);
  if (splitRows) {
    const pair = splitRows[index];
    return pair?.right ?? pair?.left ?? null;
  }
  return rows[index] ?? null;
}

function lineAnchorOf(display: PatchDisplayRow | null): { line: number; side: 'new' | 'old' } | null {
  if (!display || display.gap) return null;
  if (display.newLine > 0) return { line: display.newLine, side: 'new' };
  if (display.oldLine > 0) return { line: display.oldLine, side: 'old' };
  return null;
}

/**
 * The first line of a block at or after visual row `from` (`step` 1), or
 * at or before it (`step` -1), with its top relative to the block's.
 * Tops count REVIEW_LINE_HEIGHT_PX per visual row. With word wrap on that
 * is where the row would be unwrapped, and capture and resolve use the
 * same measure, so a position still round-trips exactly.
 */
function blockLineAnchor(
  blocks: BlockRowsCache,
  file: ReviewFile,
  row: LineBlockRow,
  from: number,
  step: 1 | -1,
): { line: number; side: 'new' | 'old'; top: number } | null {
  const visualCount = row.splitCount ?? row.count;
  for (let index = from; index >= 0 && index < visualCount; index += step) {
    const lineAnchor = lineAnchorOf(visualDisplayRow(blocks, file, row, index));
    if (lineAnchor) return { ...lineAnchor, top: index * REVIEW_LINE_HEIGHT_PX };
  }
  return null;
}

/**
 * The anchor under the viewport top, or null at offset 0 — the top is
 * deliberately unanchored so a reload at the top stays at the top (a
 * new first file becomes visible instead of pushing the view down). A row
 * with no line under the viewport top (a comment, a gap) anchors the
 * nearest line above it, so it stays in place however the rows around
 * it change.
 */
export function captureReadingAnchor(
  built: ReviewRowsResult,
  files: readonly ReviewFile[],
  blocks: BlockRowsCache,
  geometry: RowGeometry,
  offset: number,
): ReadingAnchor | null {
  if (built.rows.length === 0 || offset <= 0) return null;
  const rowIndex = geometry.findItemIndex(offset);
  const row = built.rows[rowIndex];
  const file = row ? files[row.fileIndex] : undefined;
  if (!row || !file || !geometry.holds(rowIndex)) return null;
  const rowTop = geometry.getItemOffset(rowIndex);
  let inner = 0;
  if (row.kind === 'line-block') {
    inner = Math.max(0, Math.min((row.splitCount ?? row.count) - 1, Math.floor((offset - rowTop) / REVIEW_LINE_HEIGHT_PX)));
    const lineAnchor = blockLineAnchor(blocks, file, row, inner, 1);
    if (lineAnchor) return { path: file.path, line: lineAnchor.line, side: lineAnchor.side, delta: offset - rowTop - lineAnchor.top };
  }
  // No line at or below the viewport top in this row (a comment, a gap):
  // the nearest line above it in the file.
  for (let index = rowIndex; index >= 0 && geometry.holds(index); index -= 1) {
    const above = built.rows[index];
    if (!above || above.fileIndex !== row.fileIndex || above.kind === 'file-header') break;
    if (above.kind !== 'line-block') continue;
    const from = index === rowIndex ? inner - 1 : (above.splitCount ?? above.count) - 1;
    const lineAnchor = blockLineAnchor(blocks, file, above, from, -1);
    if (!lineAnchor) continue;
    const lineTop = geometry.getItemOffset(index) + lineAnchor.top;
    return { path: file.path, line: lineAnchor.line, side: lineAnchor.side, delta: offset - lineTop };
  }
  // The header, or rows above the file's first line: anchor the file.
  const headerRow = built.firstRowOfFile[row.fileIndex] ?? -1;
  if (headerRow < 0 || !geometry.holds(headerRow)) return null;
  return { path: file.path, line: 0, side: 'new', delta: offset - geometry.getItemOffset(headerRow) };
}

/**
 * The block and visual row that hold the row nearest `target` in a file:
 * the display row is found from line numbers alone, and only the block
 * that holds it is materialized (to find its split-view pair).
 */
function findNearestLine(
  built: ReviewRowsResult,
  blocks: BlockRowsCache,
  file: ReviewFile,
  fileIndex: number,
  headerRow: number,
  target: ReadingAnchor,
): { rowIndex: number; inner: number } | null {
  const displayRow = nearestLineRow(file, target.side, target.line);
  if (displayRow < 0) return null;
  for (let rowIndex = headerRow + 1; rowIndex < built.rows.length; rowIndex += 1) {
    const row = built.rows[rowIndex];
    if (!row || row.fileIndex !== fileIndex) break;
    if (row.kind !== 'line-block') continue;
    if (displayRow < row.start.row || displayRow >= row.start.row + row.count) continue;
    const offset = displayRow - row.start.row;
    if (row.splitCount === undefined) return { rowIndex, inner: offset };
    const { rows, splitRows } = blocks.get(file, row);
    const id = rows[offset]?.id;
    const inner = splitRows?.findIndex((pair) => pair.left?.id === id || pair.right?.id === id) ?? -1;
    return { rowIndex, inner: Math.max(0, inner) };
  }
  return null;
}

/**
 * Where `target` sits in a rebuilt row model: the row and the pixels from
 * its top to the viewport top, or null when nothing usable survived (keep
 * the current position). A vanished file falls back to the next surviving
 * file in tree order; a collapsed or side-less file falls back to its
 * header.
 */
export function resolveReadingAnchor(
  built: ReviewRowsResult,
  files: readonly ReviewFile[],
  blocks: BlockRowsCache,
  target: ReadingAnchor,
): ReadingPosition | null {
  if (built.rows.length === 0) return null;
  let fileIndex = files.findIndex((file) => file.path === target.path);
  const fileSurvived = fileIndex >= 0;
  if (!fileSurvived) {
    fileIndex = files.findIndex((file) => comparePathsTreeOrder(file.path, target.path) > 0);
    if (fileIndex < 0) return null;
  }
  const headerRow = built.firstRowOfFile[fileIndex] ?? -1;
  if (headerRow < 0) return null;
  if (fileSurvived && target.line > 0) {
    const best = findNearestLine(built, blocks, files[fileIndex], fileIndex, headerRow, target);
    if (best) {
      return { index: best.rowIndex, offset: best.inner * REVIEW_LINE_HEIGHT_PX + target.delta };
    }
    // Collapsed / side vanished: fall through to the header.
  }
  return { index: headerRow, offset: fileSurvived && target.line === 0 ? target.delta : 0 };
}
