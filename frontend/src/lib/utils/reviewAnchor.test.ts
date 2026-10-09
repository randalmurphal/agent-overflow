import { describe, expect, it } from 'vitest';
import type { DiffReviewComment } from '../types/models';
import {
  captureReadingAnchor,
  resolveReadingAnchor,
  type ReadingAnchor,
  type ReadingPosition,
  type RowGeometry,
} from './reviewAnchor';
import { parseReviewFiles, type ReviewFile } from './patchStore';
import {
  BlockRowsCache,
  buildReviewRows,
  reviewRowEstimate,
  REVIEW_FILE_HEADER_PX,
  REVIEW_LINE_HEIGHT_PX,
  REVIEW_OVERVIEW_ESTIMATE_PX,
  type ReviewRowsResult,
} from './reviewRows';

// Geometry from the estimate table via prefix sums — the same numbers
// the engine derives for exact rows, without mounting a virtualizer.
function geometryOf(built: ReviewRowsResult, wordWrap = false): RowGeometry {
  const estimate = reviewRowEstimate(built, wordWrap);
  const offsets: number[] = [0];
  for (let index = 0; index < built.rows.length; index += 1) {
    offsets.push(offsets[index] + estimate.at(index));
  }
  return {
    getItemOffset: (index) => offsets[index] ?? 0,
    findItemIndex: (offset) => {
      for (let index = 0; index < built.rows.length; index += 1) {
        if (offset < offsets[index + 1]) return index;
      }
      return Math.max(0, built.rows.length - 1);
    },
    holds: (index) => index >= 0 && index < built.rows.length,
  };
}

function topOf(position: ReadingPosition | null, geometry: RowGeometry): number | null {
  return position === null ? null : geometry.getItemOffset(position.index) + position.offset;
}

function fileFor(path: string, lines: number, startLine = 1): ReviewFile {
  return parseReviewFiles([
    `diff --git a/${path} b/${path}`,
    'new file mode 100644',
    '--- /dev/null',
    `+++ b/${path}`,
    `@@ -0,0 +${startLine},${lines} @@`,
    ...Array.from({ length: lines }, (_, index) => `+line ${index + 1}`),
  ].join('\n'))[0];
}

function draftAt(path: string, newLine: number): DiffReviewComment {
  return {
    id: `draft-${path}-${newLine}`,
    threadId: 'thread-1',
    scope: 'workspace',
    sourceKey: 'source',
    filePath: path,
    status: 'draft',
    newLine,
    side: 'new',
    selectedText: '',
    body: 'draft',
    createdAt: 1,
    updatedAt: 1,
  };
}

function buildFor(files: ReviewFile[], drafts: DiffReviewComment[] = [], overview = false): ReviewRowsResult {
  return buildReviewRows({
    files,
    viewMode: 'stacked',
    collapsedPaths: new Set(),
    drafts,
    openEditors: [],
    prThreads: [],
    expandedPRThreadIds: new Set(),
    overview,
  });
}

describe('captureReadingAnchor', () => {
  it('is null at the top — the top stays the top', () => {
    const files = [fileFor('a.ts', 10)];
    const built = buildFor(files);
    expect(captureReadingAnchor(built, files, new BlockRowsCache(), geometryOf(built), 0)).toBeNull();
  });

  it('anchors the line under the viewport top with its pixel delta', () => {
    const files = [fileFor('a.ts', 10)];
    const built = buildFor(files);
    const geometry = geometryOf(built);
    // Header (60px) + 3 lines + 7px into line 4.
    const offset = REVIEW_FILE_HEADER_PX + 3 * REVIEW_LINE_HEIGHT_PX + 7;
    const anchor = captureReadingAnchor(built, files, new BlockRowsCache(), geometry, offset);
    expect(anchor).toEqual({ path: 'a.ts', line: 4, side: 'new', delta: 7 });
  });

  it('anchors a comment row to the line above it, so the comment stays put when rows above change', () => {
    const files = [fileFor('a.ts', 40)];
    const draft = draftAt('a.ts', 10);
    const built = buildFor(files, [draft]);
    const geometry = geometryOf(built);
    const commentRow = built.rows.findIndex((row) => row.kind === 'comment-thread');
    const offset = geometry.getItemOffset(commentRow) + 30;

    const anchor = captureReadingAnchor(built, files, new BlockRowsCache(), geometry, offset);
    expect(anchor).toEqual({ path: 'a.ts', line: 10, side: 'new', delta: REVIEW_LINE_HEIGHT_PX + 30 });

    // Another draft above splits the block and adds a comment row.
    const rebuilt = buildFor(files, [draftAt('a.ts', 4), draft]);
    const regeometry = geometryOf(rebuilt);
    const movedRow = rebuilt.rowKeys.indexOf(built.rowKeys[commentRow]);
    const top = topOf(resolveReadingAnchor(rebuilt, files, new BlockRowsCache(), anchor!), regeometry);
    expect(top).toBe(regeometry.getItemOffset(movedRow) + 30);
  });

  it('anchors a comment row below the header to the file', () => {
    const files = [fileFor('a.ts', 10)];
    const built = buildFor(files, [{ ...draftAt('a.ts', 0), side: 'file', newLine: undefined }]);
    const geometry = geometryOf(built);
    const offset = REVIEW_FILE_HEADER_PX + 12;
    const anchor = captureReadingAnchor(built, files, new BlockRowsCache(), geometry, offset);
    expect(anchor).toEqual({ path: 'a.ts', line: 0, side: 'new', delta: REVIEW_FILE_HEADER_PX + 12 });
    expect(topOf(resolveReadingAnchor(built, files, new BlockRowsCache(), anchor!), geometry)).toBe(offset);
  });

  it('anchors nothing it cannot measure: a row outside the held rows', () => {
    const files = [fileFor('a.ts', 40)];
    const built = buildFor(files, [draftAt('a.ts', 10)]);
    const full = geometryOf(built);
    const commentRow = built.rows.findIndex((row) => row.kind === 'comment-thread');
    // Held from the comment row down: the line above it has no offset.
    const held: RowGeometry = { ...full, holds: (index) => index >= commentRow };
    const offset = full.getItemOffset(commentRow) + 30;
    expect(captureReadingAnchor(built, files, new BlockRowsCache(), held, offset)).toBeNull();
  });

  it('anchors the file header when the top row is the header', () => {
    const files = [fileFor('a.ts', 10)];
    const built = buildFor(files);
    const anchor = captureReadingAnchor(built, files, new BlockRowsCache(), geometryOf(built), 10);
    expect(anchor).toEqual({ path: 'a.ts', line: 0, side: 'new', delta: 10 });
  });
});

describe('resolveReadingAnchor', () => {
  const anchorAt = (line: number, path = 'b.ts', delta = 5): ReadingAnchor =>
    ({ path, line, side: 'new', delta });

  it('round-trips: capture then resolve reproduces the offset', () => {
    const files = [fileFor('a.ts', 40), fileFor('b.ts', 40)];
    const built = buildFor(files);
    const geometry = geometryOf(built);
    const offset = 2 * REVIEW_FILE_HEADER_PX + 55 * REVIEW_LINE_HEIGHT_PX + 3;
    const anchor = captureReadingAnchor(built, files, new BlockRowsCache(), geometry, offset);
    expect(anchor?.path).toBe('b.ts');
    expect(topOf(resolveReadingAnchor(built, files, new BlockRowsCache(), anchor!), geometry)).toBe(offset);
  });

  it('round-trips under word wrap, where blocks measure taller than their lines', () => {
    const files = [fileFor('a.ts', 20), fileFor('b.ts', 80)];
    const built = buildFor(files);
    const flat = geometryOf(built);
    // Every block renders at 3x its unwrapped height.
    const offsets: number[] = [0];
    for (let index = 0; index < built.rows.length; index += 1) {
      const size = flat.getItemOffset(index + 1) - flat.getItemOffset(index) || REVIEW_FILE_HEADER_PX;
      offsets.push(offsets[index] + (built.rows[index].kind === 'line-block' ? 3 * size : size));
    }
    const wrapped: RowGeometry = {
      getItemOffset: (index) => offsets[index] ?? 0,
      findItemIndex: (offset) => Math.max(0, offsets.findIndex((top) => top > offset) - 1),
      holds: () => true,
    };
    for (const offset of [offsets[3] + 5, offsets[3] + 1500, offsets[4] - 1, offsets[5] + 77]) {
      const anchor = captureReadingAnchor(built, files, new BlockRowsCache(), wrapped, offset);
      expect(topOf(resolveReadingAnchor(built, files, new BlockRowsCache(), anchor!), wrapped)).toBe(offset);
    }
  });

  it('keeps the anchored line stable when content above it grows', () => {
    const before = [fileFor('a.ts', 10), fileFor('b.ts', 30)];
    const builtBefore = buildFor(before);
    const geomBefore = geometryOf(builtBefore);
    const anchor = anchorAt(12);
    const offsetBefore = topOf(resolveReadingAnchor(builtBefore, before, new BlockRowsCache(), anchor), geomBefore)!;

    // a.ts triples in size; b.ts line 12 must stay under the viewport top.
    const after = [fileFor('a.ts', 30), fileFor('b.ts', 30)];
    const builtAfter = buildFor(after);
    const offsetAfter = topOf(resolveReadingAnchor(builtAfter, after, new BlockRowsCache(), anchor), geometryOf(builtAfter))!;
    expect(offsetAfter - offsetBefore).toBe(20 * REVIEW_LINE_HEIGHT_PX);
  });

  it('snaps to the nearest surviving line when the exact line left', () => {
    const files = [fileFor('b.ts', 8)];
    const built = buildFor(files);
    const geometry = geometryOf(built);
    // Line 12 no longer exists (file shrank to 8 lines) → nearest is 8.
    const top = topOf(resolveReadingAnchor(built, files, new BlockRowsCache(), anchorAt(12)), geometry)!;
    expect(top).toBe(REVIEW_FILE_HEADER_PX + 7 * REVIEW_LINE_HEIGHT_PX + 5);
  });

  it('falls back to the next file in tree order when the file vanished', () => {
    const files = [fileFor('a.ts', 5), fileFor('c.ts', 5)];
    const built = buildFor(files);
    const geometry = geometryOf(built);
    const top = topOf(resolveReadingAnchor(built, files, new BlockRowsCache(), anchorAt(3, 'b.ts')), geometry);
    // c.ts header (a.ts header + 5 lines), delta NOT carried over.
    expect(top).toBe(REVIEW_FILE_HEADER_PX + 5 * REVIEW_LINE_HEIGHT_PX);
  });

  it('returns null when nothing after the anchor survives', () => {
    const files = [fileFor('a.ts', 5)];
    const built = buildFor(files);
    expect(topOf(resolveReadingAnchor(built, files, new BlockRowsCache(), anchorAt(3, 'z.ts')), geometryOf(built))).toBeNull();
  });

  it('falls back to the header when the file collapsed', () => {
    const files = [fileFor('b.ts', 20)];
    const built = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(['b.ts']),
      drafts: [],
      openEditors: [],
      prThreads: [],
      expandedPRThreadIds: new Set(),
    });
    const top = topOf(resolveReadingAnchor(built, files, new BlockRowsCache(), anchorAt(12)), geometryOf(built));
    expect(top).toBe(0); // header row offset, no line delta
  });
});

describe('reading anchor over the PR overview row', () => {
  it('anchors a position inside the overview to the empty path with its pixel delta', () => {
    const files = [fileFor('a.ts', 10)];
    const built = buildFor(files, [], true);
    const geometry = geometryOf(built);
    const offset = 123;
    const anchor = captureReadingAnchor(built, files, new BlockRowsCache(), geometry, offset);
    expect(anchor).toEqual({ path: '', line: 0, side: 'new', delta: offset - geometry.getItemOffset(0) });
    expect(topOf(resolveReadingAnchor(built, files, new BlockRowsCache(), anchor!), geometry)).toBe(offset);
  });

  it('resolves the empty path into the overview, or to the top when there is no overview', () => {
    const files = [fileFor('a.ts', 10)];
    const anchor: ReadingAnchor = { path: '', line: 0, side: 'new', delta: 42 };
    expect(resolveReadingAnchor(buildFor(files, [], true), files, new BlockRowsCache(), anchor))
      .toEqual({ index: 0, offset: 42 });
    // Row 0 is a file header now: the delta measured inside the overview
    // means nothing there, so the view goes to the top.
    expect(resolveReadingAnchor(buildFor(files), files, new BlockRowsCache(), anchor))
      .toEqual({ index: 0, offset: 0 });
  });

  it('still anchors file lines below the overview', () => {
    const files = [fileFor('a.ts', 10)];
    const built = buildFor(files, [], true);
    const geometry = geometryOf(built);
    const offset = REVIEW_OVERVIEW_ESTIMATE_PX + REVIEW_FILE_HEADER_PX + 3 * REVIEW_LINE_HEIGHT_PX + 7;
    const anchor = captureReadingAnchor(built, files, new BlockRowsCache(), geometry, offset);
    expect(anchor).toEqual({ path: 'a.ts', line: 4, side: 'new', delta: 7 });
    // Overview, header, block: the line sits in row 2.
    expect(resolveReadingAnchor(built, files, new BlockRowsCache(), anchor!))
      .toEqual({ index: 2, offset: 3 * REVIEW_LINE_HEIGHT_PX + 7 });
  });
});
