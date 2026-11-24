import { describe, expect, it } from 'vitest';
import { parsePatchFiles } from './patchFiles';
import { parseReviewFiles } from './patchParser';
import {
  anchorLines,
  anchorLinesMatch,
  anchorRow,
  anchorRows,
  completeAnchor,
  nearestLineRow,
  RowWalker,
} from './patchRows';
import { MIXED_PATCH } from '../../test/helpers/patchFixtures';
import { buildPatchDisplayRows } from '../../test/helpers/patchDisplayRows';

describe('split-view halves of a context row', () => {
  // Old 1..3, new 1..3: 1 and 3 unchanged, 2 replaced.
  const file = parseReviewFiles([
    'diff --git a/src/app.ts b/src/app.ts',
    'index 1111111..2222222 100644',
    '--- a/src/app.ts',
    '+++ b/src/app.ts',
    '@@ -1,3 +1,4 @@',
    ' alpha',
    '-beta',
    '+beta two',
    '+inserted',
    ' gamma',
  ].join('\n'))[0];

  it('names the context row from either half with both numbers; one number names it only for a forge thread', () => {
    const gamma = anchorRow(file, { side: 'context', oldLine: 3, newLine: 4 });
    expect(gamma).toBeGreaterThan(0);
    expect(anchorRow(file, { side: 'old', oldLine: 3, newLine: 4 })).toBe(gamma);
    expect(anchorRow(file, { side: 'new', oldLine: 3, newLine: 4 })).toBe(gamma);
    // A saved draft with one number on an unchanged line names no row.
    expect(anchorRow(file, { side: 'old', oldLine: 3 })).toBe(-1);
    expect(anchorRow(file, { side: 'new', newLine: 4 })).toBe(-1);
    expect(anchorRow(file, { side: 'old', oldLine: 3 }, true)).toBe(gamma);
    expect(anchorRow(file, { side: 'new', newLine: 4 }, true)).toBe(gamma);
    expect(anchorRow(file, { side: 'old', oldLine: 2 })).toBeGreaterThan(0);
    expect(anchorRow(file, { side: 'old', oldLine: 4 }, true)).toBe(-1);
  });

  it('completes a half to both numbers, and leaves a changed line as it is', () => {
    expect(completeAnchor(file, { side: 'old' as const, oldLine: 3 })).toEqual({ side: 'old', oldLine: 3, newLine: 4 });
    expect(completeAnchor(file, { side: 'new' as const, newLine: 4 })).toEqual({ side: 'new', oldLine: 3, newLine: 4 });
    expect(completeAnchor(file, { side: 'old' as const, oldLine: 2 })).toEqual({ side: 'old', oldLine: 2 });
    expect(completeAnchor(file, { side: 'new' as const, newLine: 9 })).toEqual({ side: 'new', newLine: 9 });
  });

  it('agrees with the membership check, strict and loose', () => {
    const lines = anchorLines(file);
    for (const anchor of [
      { side: 'old' as const, oldLine: 3 },
      { side: 'new' as const, newLine: 4 },
      { side: 'old' as const, oldLine: 3, newLine: 4 },
      { side: 'new' as const, oldLine: 3, newLine: 4 },
      { side: 'old' as const, oldLine: 2 },
      { side: 'new' as const, newLine: 3 },
      { side: 'context' as const, oldLine: 1, newLine: 1 },
      { side: 'old' as const, oldLine: 4 },
      { side: 'new' as const, newLine: 5 },
      { side: 'context' as const, oldLine: 3, newLine: 3 },
    ]) {
      for (const loose of [false, true]) {
        expect(anchorLinesMatch(lines, anchor, loose), `${JSON.stringify(anchor)} ${loose}`).toBe(anchorRow(file, anchor, loose) >= 0);
      }
    }
  });

  it('counts the rows an anchor names and the place of the one at a line', () => {
    expect(anchorRows(file, { side: 'old', oldLine: 3, newLine: 4 })).toMatchObject({ count: 1, occurrence: 0, context: true });
    expect(anchorRows(file, { side: 'new', newLine: 9 })).toMatchObject({ count: 0, row: -1 });
  });
});

describe('row lookups', () => {
  const file = parseReviewFiles(MIXED_PATCH)[0];
  const rows = buildPatchDisplayRows(parsePatchFiles(MIXED_PATCH)[0].lines);

  it('finds the row a comment anchor names', () => {
    rows.forEach((row, index) => {
      if (row.gap) return;
      const side = row.side;
      const expected = rows.findIndex((candidate) => !candidate.gap && candidate.side === side
        && (side === 'old' ? candidate.oldLine === row.oldLine
          : side === 'new' ? candidate.newLine === row.newLine
            : candidate.oldLine === row.oldLine && candidate.newLine === row.newLine));
      expect(anchorRow(file, { side, oldLine: row.oldLine, newLine: row.newLine })).toBe(expected);
      expect(expected).toBeLessThanOrEqual(index);
    });
    expect(anchorRow(file, { side: 'new', newLine: 9999 })).toBe(-1);
  });

  it('finds the nearest row on a side', () => {
    expect(nearestLineRow(file, 'new', 12)).toBe(rows.findIndex((row) => row.newLine === 12));
    expect(nearestLineRow(file, 'old', 11)).toBe(rows.findIndex((row) => row.oldLine === 11 && row.newLine === 0));
    expect(nearestLineRow(file, 'new', 1)).toBe(rows.findIndex((row) => row.newLine > 0));
  });

  it('walks rows with the ids buildPatchDisplayRows assigns', () => {
    const walker = new RowWalker(file);
    const ids: string[] = [];
    while (walker.next()) ids.push(walker.rowId());
    expect(ids).toEqual(rows.map((row) => row.id));
  });
});
