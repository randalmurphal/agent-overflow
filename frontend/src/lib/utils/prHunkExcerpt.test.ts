import { afterEach, describe, expect, it } from 'vitest';
import { setPatchTextBudgetForTest } from './patchMemory.svelte';
import { parseReviewFiles } from './patchStore';
import { hunkExcerptForComment } from './prHunkExcerpt';
import { streamPatch } from '../../test/helpers/streamedPatch';

const patch = [
  'diff --git a/src/pad.ts b/src/pad.ts',
  '--- a/src/pad.ts',
  '+++ b/src/pad.ts',
  '@@ -1,0 +1,1 @@',
  `+${'p'.repeat(300)}`,
  'diff --git a/src/app.ts b/src/app.ts',
  '--- a/src/app.ts',
  '+++ b/src/app.ts',
  '@@ -10,5 +10,5 @@',
  ' one',
  ' two',
  '-three',
  '+THREE',
  ' four',
  ' five',
].join('\n') + '\n';

afterEach(() => setPatchTextBudgetForTest(null));

describe('hunkExcerptForComment', () => {
  it('quotes the rows around a comment, reading evicted text again', async () => {
    setPatchTextBudgetForTest(320);
    const { parser, files } = streamPatch(patch, 25);
    try {
      expect(files[1].body.resident()).toBe(false);
      const comment = { filePath: 'src/app.ts', newLine: 12, side: 'new' as const };
      const excerpt = await hunkExcerptForComment(files, comment, 1);
      expect(excerpt).toBe(await hunkExcerptForComment(parseReviewFiles(patch), comment, 1));
      expect(excerpt.split('\n').map((line) => line.slice(10))).toEqual(['-three', '+THREE', ' four']);
    } finally {
      parser.store.dispose();
    }
  });
});
