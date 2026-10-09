import { describe, expect, it } from 'vitest';
import type { ReviewThread } from '../types/models';
import { parseReviewFiles, type ReviewFile } from './patchStore';
import { THREAD_CONTEXT_ROWS, threadAnchorRow, threadContextRows } from './reviewThreadContext';

// Display rows (old, new):
//   0 one (1,1)  1 two (2,2)  2 -three (3,0)  3 +THREE (0,3)
//   4 +three.5 (0,4)  5 four (4,5)  6 five (5,6)  7 six (6,7)
function mixedFile(): ReviewFile {
  return parseReviewFiles([
    'diff --git a/src/a.ts b/src/a.ts',
    'index 1111111..2222222 100644',
    '--- a/src/a.ts',
    '+++ b/src/a.ts',
    '@@ -1,6 +1,7 @@',
    ' one',
    ' two',
    '-three',
    '+THREE',
    '+three.5',
    ' four',
    ' five',
    ' six',
  ].join('\n'))[0];
}

function addedFile(): ReviewFile {
  return parseReviewFiles([
    'diff --git a/src/new.ts b/src/new.ts',
    'new file mode 100644',
    '--- /dev/null',
    '+++ b/src/new.ts',
    '@@ -0,0 +1,3 @@',
    '+a',
    '+b',
    '+c',
  ].join('\n'))[0];
}

function thread(overrides: Partial<ReviewThread> = {}): ReviewThread {
  return {
    id: 't1',
    path: 'src/a.ts',
    line: 5,
    side: 'right',
    isResolvable: true,
    isResolved: false,
    isOutdated: false,
    comments: [],
    ...overrides,
  };
}

function lines(rows: { oldLine: number; newLine: number }[]): [number, number][] {
  return rows.map((row) => [row.oldLine, row.newLine]);
}

describe('threadAnchorRow', () => {
  it('finds the display row of a right-side line', () => {
    const file = mixedFile();
    expect(threadAnchorRow(file, thread({ line: 5, side: 'right' }))).toBe(5);
    expect(threadAnchorRow(file, thread({ line: 3, side: 'right' }))).toBe(3);
  });

  it('finds a left-side line on the old side', () => {
    // Old line 3 is the deleted row, not the added row at new line 3. The
    // forge lowercases sides (Go); GitLab reports 'old'.
    expect(threadAnchorRow(mixedFile(), thread({ line: 3, side: 'left' }))).toBe(2);
    expect(threadAnchorRow(mixedFile(), thread({ line: 3, side: 'old' }))).toBe(2);
  });

  it('falls back to startLine when the thread has no line', () => {
    expect(threadAnchorRow(mixedFile(), thread({ line: null, startLine: 4 }))).toBe(4);
  });

  it('is -1 for a thread with no line', () => {
    expect(threadAnchorRow(mixedFile(), thread({ line: null }))).toBe(-1);
    expect(threadAnchorRow(mixedFile(), thread({ line: 0 }))).toBe(-1);
  });

  it('is -1 for a line the diff does not show, never a neighbour', () => {
    // The diff row treats this thread as orphaned; borrowing the nearest
    // line would show code the comment was not written against.
    expect(threadAnchorRow(addedFile(), thread({ path: 'src/new.ts', line: 99, side: 'right' }))).toBe(-1);
    expect(threadContextRows(addedFile(), thread({ path: 'src/new.ts', line: 99, side: 'right' }))).toBeNull();
  });

  it('is -1 for a line on a side the file has no rows on', () => {
    // A new file has no old side: a left-side thread anchors nothing.
    expect(threadAnchorRow(addedFile(), thread({ path: 'src/new.ts', line: 2, side: 'left' }))).toBe(-1);
  });
});

describe('threadContextRows', () => {
  it('returns THREAD_CONTEXT_ROWS rows ending at the anchored row', () => {
    const context = threadContextRows(mixedFile(), thread({ line: 6 }))!;
    expect(context.rows).toHaveLength(THREAD_CONTEXT_ROWS);
    expect(context.complete).toBe(true);
    expect(lines(context.rows)).toEqual([[0, 3], [0, 4], [4, 5], [5, 6]]);
    expect(context.rows.at(-1)?.line.content).toBe(' five');
  });

  it('returns fewer rows near the top of the file', () => {
    const context = threadContextRows(mixedFile(), thread({ line: 2 }))!;
    expect(lines(context.rows)).toEqual([[1, 1], [2, 2]]);
  });

  it('ends at the deleted row for a left-side thread', () => {
    const context = threadContextRows(mixedFile(), thread({ line: 3, side: 'left' }))!;
    expect(lines(context.rows)).toEqual([[1, 1], [2, 2], [3, 0]]);
  });

  it('is null when the thread anchors nothing in the file', () => {
    expect(threadContextRows(mixedFile(), thread({ line: null }))).toBeNull();
    expect(threadContextRows(addedFile(), thread({ path: 'src/new.ts', line: 2, side: 'left' }))).toBeNull();
  });
});
