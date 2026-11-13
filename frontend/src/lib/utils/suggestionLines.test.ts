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
