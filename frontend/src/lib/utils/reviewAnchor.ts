import type { PatchDisplayRow, PatchFile } from './patchFiles';
import { comparePathsTreeOrder } from './reviewTree';
import { REVIEW_LINE_HEIGHT_PX, type ReviewRow, type ReviewRowsResult } from './reviewRows';

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

/** The slice of the virtualizer handle the anchor math needs. */
export interface RowGeometry {
  findItemIndex(offset: number): number;
  getItemOffset(index: number): number;
}

type LineBlockRow = Extract<ReviewRow, { kind: 'line-block' }>;

function visualDisplayRow(row: LineBlockRow, index: number): PatchDisplayRow | null {
  if (row.splitRows) {
    const pair = row.splitRows[index];
    return pair?.right ?? pair?.left ?? null;
  }
  return row.rows[index] ?? null;
}

function lineAnchorOf(display: PatchDisplayRow | null): { line: number; side: 'new' | 'old' } | null {
  if (!display || display.gap) return null;
  if (display.newLine > 0) return { line: display.newLine, side: 'new' };
  if (display.oldLine > 0) return { line: display.oldLine, side: 'old' };
  return null;
}

/**
 * The anchor under the viewport top, or null at offset 0 — the top is
 * deliberately unanchored so a reload at the top stays at the top (a
 * new first file becomes visible instead of pushing the view down).
 * With word wrap on, the inner-line math degrades to block-level
 * anchoring (blocks are measured, lines aren't fixed-height).
 */
export function captureReadingAnchor(
  built: ReviewRowsResult,
  files: readonly PatchFile[],
  geometry: RowGeometry,
  offset: number,
  wordWrap: boolean,
): ReadingAnchor | null {
  if (built.rows.length === 0 || offset <= 0) return null;
  const rowIndex = geometry.findItemIndex(offset);
  const row = built.rows[rowIndex];
  const file = row ? files[row.fileIndex] : undefined;
  if (!row || !file) return null;
  const rowTop = geometry.getItemOffset(rowIndex);
  if (row.kind === 'line-block') {
    const visualCount = (row.splitRows ?? row.rows).length;
    const inner = wordWrap
      ? 0
      : Math.max(0, Math.min(visualCount - 1, Math.floor((offset - rowTop) / REVIEW_LINE_HEIGHT_PX)));
    for (let index = inner; index < visualCount; index += 1) {
      const lineAnchor = lineAnchorOf(visualDisplayRow(row, index));
      if (lineAnchor) {
        const lineTop = rowTop + (wordWrap ? 0 : index * REVIEW_LINE_HEIGHT_PX);
        return { path: file.path, ...lineAnchor, delta: offset - lineTop };
      }
    }
  }
  // Header / comment / gap-only block: anchor the file itself.
  return { path: file.path, line: 0, side: 'new', delta: offset - rowTop };
}

function findNearestLine(
  built: ReviewRowsResult,
  fileIndex: number,
  headerRow: number,
  target: ReadingAnchor,
): { rowIndex: number; inner: number } | null {
  let best: { rowIndex: number; inner: number; dist: number } | null = null;
  for (let rowIndex = headerRow + 1; rowIndex < built.rows.length; rowIndex += 1) {
    const row = built.rows[rowIndex];
    if (!row || row.fileIndex !== fileIndex) break;
    if (row.kind !== 'line-block') continue;
    const visualCount = (row.splitRows ?? row.rows).length;
    for (let inner = 0; inner < visualCount; inner += 1) {
      const info = lineAnchorOf(visualDisplayRow(row, inner));
      if (!info || info.side !== target.side) continue;
      const dist = Math.abs(info.line - target.line);
      if (!best || dist < best.dist) best = { rowIndex, inner, dist };
      if (dist === 0) return best;
      // Line numbers are monotonic per side within a file: once past
      // the target and no longer improving, the best cannot change.
      if (info.line > target.line && best.dist < dist) return best;
    }
  }
  return best;
}

/**
 * The scrollTop that puts `target` back under the viewport top in a
 * rebuilt row model, or null when nothing usable survived (keep the
 * current position). A vanished file falls back to the next surviving
 * file in tree order; a collapsed or side-less file falls back to its
 * header.
 */
export function resolveReadingAnchor(
  built: ReviewRowsResult,
  files: readonly PatchFile[],
  geometry: RowGeometry,
  target: ReadingAnchor,
  wordWrap: boolean,
): number | null {
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
    const best = findNearestLine(built, fileIndex, headerRow, target);
    if (best) {
      const lineTop = geometry.getItemOffset(best.rowIndex)
        + (wordWrap ? 0 : best.inner * REVIEW_LINE_HEIGHT_PX);
      return Math.max(0, lineTop + target.delta);
    }
    // Collapsed / side vanished: fall through to the header.
  }
  const delta = fileSurvived && target.line === 0 ? target.delta : 0;
  return Math.max(0, geometry.getItemOffset(headerRow) + delta);
}
