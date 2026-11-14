import { describe, expect, it } from 'vitest';
import { parseReviewFiles } from './patchStore';
import { newSideLineIndexes, readNewSideLines } from './suggestionLines';

const PATCH = [
  'diff --git a/src/a.ts b/src/a.ts',
  '--- a/src/a.ts',
  '+++ b/src/a.ts',
  '@@ -1,4 +1,5 @@',
  ' const a = 1;',
  '-const b = 2;',
  '+const b = 3;',
  '+  // ```fenced```',
  ' const c = 4;',
  ' const d = 5;',
  '@@ -20,2 +21,2 @@',
  ' far();',
  '-away();',
  '+there();',
  '',
].join('\n');

function file() {
  const [only] = parseReviewFiles(PATCH);
  return only;
}

describe('readNewSideLines', () => {
  it('reads one line exactly, without its diff column', async () => {
    await expect(readNewSideLines(file(), 2, 2)).resolves.toEqual(['const b = 3;']);
    await expect(readNewSideLines(file(), 1, 1)).resolves.toEqual(['const a = 1;']);
  });

  it('reads a range across added and unchanged lines, keeping indentation and backticks', async () => {
    await expect(readNewSideLines(file(), 2, 4)).resolves.toEqual(['const b = 3;', '  // ```fenced```', 'const c = 4;']);
  });

  it('answers null for lines the diff does not show or a run that crosses hunks', async () => {
    await expect(readNewSideLines(file(), 6, 6)).resolves.toBeNull();
    await expect(readNewSideLines(file(), 5, 21)).resolves.toBeNull();
    expect(newSideLineIndexes(file(), 0, 1)).toBeNull();
    expect(newSideLineIndexes(file(), 3, 2)).toBeNull();
  });

  it('reads the second hunk', async () => {
    await expect(readNewSideLines(file(), 21, 22)).resolves.toEqual(['far();', 'there();']);
  });
});

describe('readNewSideLines on a CRLF file', () => {
  // The line ending is not the line's text: a trailing CR goes; a lone CR
  // inside a line is content and stays.
  const CRLF = [
    'diff --git a/w.txt b/w.txt',
    '--- a/w.txt',
    '+++ b/w.txt',
    '@@ -1,3 +1,4 @@',
    ' one\r',
    '-two\r',
    '+two!\r',
    '+in\rside\r',
    '+bare',
    ' three\r\r',
    '',
  ].join('\n');

  it.each([
    [1, 1, ['one']],
    [2, 2, ['two!']],
    [1, 2, ['one', 'two!']],
    [3, 3, ['in\rside']],
    [4, 4, ['bare']],
    [5, 5, ['three\r']],
    [1, 5, ['one', 'two!', 'in\rside', 'bare', 'three\r']],
  ] as const)('reads lines %i..%i without their line ending', async (from, to, want) => {
    const [only] = parseReviewFiles(CRLF);
    await expect(readNewSideLines(only, from, to)).resolves.toEqual(want);
  });
});
