import { describe, expect, it } from 'vitest';
import { parseReviewFiles } from './patchStore';
import { draftRange, rangeLabel, rowInRange, sharedHalf, spanRange, threadRange } from './reviewRange';
import type { CommentAnchor } from './reviewRows';

// Two hunks: lines 10-14 (a context line, a deletion, two additions, a
// context line) and lines 40-41.
const PATCH = [
  'diff --git a/src/a.ts b/src/a.ts',
  'index 1111111..2222222 100644',
  '--- a/src/a.ts',
  '+++ b/src/a.ts',
  '@@ -10,3 +10,4 @@',
  ' keep ten',
  '-gone eleven',
  '+new eleven',
  '+new twelve',
  ' keep twelve',
  '@@ -40,2 +41,2 @@',
  ' far forty',
  ' far forty-one',
].join('\n');

const file = parseReviewFiles(PATCH)[0];
const at = (anchor: Omit<CommentAnchor, 'filePath'>): CommentAnchor => ({ filePath: 'src/a.ts', ...anchor });
const context10 = at({ side: 'context', oldLine: 10, newLine: 10 });
const deleted11 = at({ side: 'old', oldLine: 11 });
const added11 = at({ side: 'new', newLine: 11 });
const added12 = at({ side: 'new', newLine: 12 });
const context12 = at({ side: 'context', oldLine: 12, newLine: 13 });
const far = at({ side: 'context', oldLine: 40, newLine: 41 });

describe('spanRange', () => {
  it('spans unchanged and added lines on the new half, in either direction', () => {
    const down = spanRange(file, context10, added12);
    expect(down).toEqual({ kind: 'range', start: context10, end: added12, range: { filePath: 'src/a.ts', half: 'new', from: 10, to: 12 } });
    const up = spanRange(file, context12, context10);
    expect(up).toMatchObject({ kind: 'range', start: context10, end: context12, range: { half: 'new', from: 10, to: 13 } });
  });

  it('spans a deleted line on the old half', () => {
    expect(spanRange(file, context10, deleted11)).toMatchObject({ kind: 'range', range: { half: 'old', from: 10, to: 11 } });
  });

  it('is one line when both ends are the same row', () => {
    expect(spanRange(file, added11, added11)).toEqual({ kind: 'single', anchor: added11 });
  });

  it('refuses a deleted line to an added one: they share no half', () => {
    expect(spanRange(file, deleted11, added12)).toEqual({ kind: 'refused', reason: 'other-side' });
  });

  it('refuses another file, a file comment, and a line not in the diff', () => {
    expect(spanRange(file, context10, { ...added12, filePath: 'src/b.ts' })).toEqual({ kind: 'refused', reason: 'other-file' });
    expect(spanRange(file, { filePath: 'src/a.ts', side: 'file' }, added12)).toEqual({ kind: 'refused', reason: 'file' });
    expect(spanRange(file, context10, at({ side: 'new', newLine: 30 }))).toEqual({ kind: 'refused', reason: 'not-in-diff' });
  });

  it('refuses a range from one hunk into the next', () => {
    expect(spanRange(file, context12, far)).toEqual({ kind: 'refused', reason: 'across-hunks' });
    // With no gap row between them (gaps off), by the jump in numbers.
    expect(spanRange({ ...file, suppressGaps: true }, context12, far)).toEqual({ kind: 'refused', reason: 'across-hunks' });
  });
});

describe('ranges of drafts and threads', () => {
  it('reads a draft range on its half, and none for one line', () => {
    expect(draftRange({ filePath: 'a', newLine: 15, startNewLine: 12, startOldLine: 10 })).toEqual({ filePath: 'a', half: 'new', from: 12, to: 15 });
    expect(draftRange({ filePath: 'a', oldLine: 8, startOldLine: 5 })).toEqual({ filePath: 'a', half: 'old', from: 5, to: 8 });
    expect(draftRange({ filePath: 'a', newLine: 15 })).toBeNull();
  });

  it('reads a forge thread that carries startLine', () => {
    expect(threadRange({ path: 'a', line: 18, startLine: 12, side: 'right' })).toEqual({ filePath: 'a', half: 'new', from: 12, to: 18 });
    expect(threadRange({ path: 'a', line: 18, startLine: 12, side: 'left' })?.half).toBe('old');
    expect(threadRange({ path: 'a', line: 18, side: 'right' })).toBeNull();
    expect(rangeLabel({ filePath: 'a', half: 'new', from: 12, to: 18 })).toBe('Lines 12–18');
  });

  it('tints a row by its number on the range half only', () => {
    const range = { filePath: 'a', half: 'new' as const, from: 10, to: 12 };
    expect(rowInRange(0, 11, null, range)).toBe(true);
    // A deleted row inside a new-half range is not part of it.
    expect(rowInRange(11, 0, null, range)).toBe(false);
    // Split view: only the range's half.
    expect(rowInRange(10, 10, 'old', range)).toBe(false);
    expect(rowInRange(10, 10, 'new', range)).toBe(true);
    expect(rowInRange(0, 13, null, range)).toBe(false);
  });

  it('names the half two lines share, new first', () => {
    expect(sharedHalf(context10, added12)).toBe('new');
    expect(sharedHalf(context10, deleted11)).toBe('old');
    expect(sharedHalf(deleted11, added12)).toBeNull();
  });
});
