import { describe, expect, it } from 'vitest';
import {
  captureReadingAnchor,
  resolveReadingAnchor,
  type ReadingAnchor,
  type RowGeometry,
} from './reviewAnchor';
import { parsePatchFiles, type PatchFile } from './patchFiles';
import {
  buildReviewRows,
  reviewRowEstimate,
  REVIEW_FILE_HEADER_PX,
  REVIEW_LINE_HEIGHT_PX,
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
  };
}

function fileFor(path: string, lines: number, startLine = 1): PatchFile {
  return parsePatchFiles([
    `diff --git a/${path} b/${path}`,
    'new file mode 100644',
    '--- /dev/null',
    `+++ b/${path}`,
    `@@ -0,0 +${startLine},${lines} @@`,
    ...Array.from({ length: lines }, (_, index) => `+line ${index + 1}`),
  ].join('\n'))[0];
}

function buildFor(files: PatchFile[]): ReviewRowsResult {
  return buildReviewRows({
    files,
    viewMode: 'stacked',
    collapsedPaths: new Set(),
    drafts: [],
    openEditors: [],
    prThreads: [],
    expandedPRThreadIds: new Set(),
  });
}

describe('captureReadingAnchor', () => {
  it('is null at the top — the top stays the top', () => {
    const files = [fileFor('a.ts', 10)];
    const built = buildFor(files);
    expect(captureReadingAnchor(built, files, geometryOf(built), 0, false)).toBeNull();
  });

  it('anchors the line under the viewport top with its pixel delta', () => {
    const files = [fileFor('a.ts', 10)];
    const built = buildFor(files);
    const geometry = geometryOf(built);
    // Header (60px) + 3 lines + 7px into line 4.
    const offset = REVIEW_FILE_HEADER_PX + 3 * REVIEW_LINE_HEIGHT_PX + 7;
    const anchor = captureReadingAnchor(built, files, geometry, offset, false);
    expect(anchor).toEqual({ path: 'a.ts', line: 4, side: 'new', delta: 7 });
  });

  it('anchors the file header when the top row is the header', () => {
    const files = [fileFor('a.ts', 10)];
    const built = buildFor(files);
    const anchor = captureReadingAnchor(built, files, geometryOf(built), 10, false);
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
    const anchor = captureReadingAnchor(built, files, geometry, offset, false);
    expect(anchor?.path).toBe('b.ts');
    expect(resolveReadingAnchor(built, files, geometry, anchor!, false)).toBe(offset);
  });

  it('keeps the anchored line stable when content above it grows', () => {
    const before = [fileFor('a.ts', 10), fileFor('b.ts', 30)];
    const builtBefore = buildFor(before);
    const geomBefore = geometryOf(builtBefore);
    const anchor = anchorAt(12);
    const offsetBefore = resolveReadingAnchor(builtBefore, before, geomBefore, anchor, false)!;

    // a.ts triples in size; b.ts line 12 must stay under the viewport top.
    const after = [fileFor('a.ts', 30), fileFor('b.ts', 30)];
    const builtAfter = buildFor(after);
    const offsetAfter = resolveReadingAnchor(builtAfter, after, geometryOf(builtAfter), anchor, false)!;
    expect(offsetAfter - offsetBefore).toBe(20 * REVIEW_LINE_HEIGHT_PX);
  });

  it('snaps to the nearest surviving line when the exact line left', () => {
    const files = [fileFor('b.ts', 8)];
    const built = buildFor(files);
    const geometry = geometryOf(built);
    // Line 12 no longer exists (file shrank to 8 lines) → nearest is 8.
    const top = resolveReadingAnchor(built, files, geometry, anchorAt(12), false)!;
    expect(top).toBe(REVIEW_FILE_HEADER_PX + 7 * REVIEW_LINE_HEIGHT_PX + 5);
  });

  it('falls back to the next file in tree order when the file vanished', () => {
    const files = [fileFor('a.ts', 5), fileFor('c.ts', 5)];
    const built = buildFor(files);
    const geometry = geometryOf(built);
    const top = resolveReadingAnchor(built, files, geometry, anchorAt(3, 'b.ts'), false);
    // c.ts header (a.ts header + 5 lines), delta NOT carried over.
    expect(top).toBe(REVIEW_FILE_HEADER_PX + 5 * REVIEW_LINE_HEIGHT_PX);
  });

  it('returns null when nothing after the anchor survives', () => {
    const files = [fileFor('a.ts', 5)];
    const built = buildFor(files);
    expect(resolveReadingAnchor(built, files, geometryOf(built), anchorAt(3, 'z.ts'), false)).toBeNull();
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
    const top = resolveReadingAnchor(built, files, geometryOf(built), anchorAt(12), false);
    expect(top).toBe(0); // header row offset, no line delta
  });
});
