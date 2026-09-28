// A pull request diff larger than any single read and taller than a
// browser can lay out, through the shipped UI, the real backend and the
// fake forge CLI. The PR adds 20 files of 40,000 lines each: about 13 MB of
// diff, past the 10 MiB a whole-output read once refused, and about 16M px
// of rows once expanded, past the review list's held-range limit
// (frontend/src/lib/utils/virtual/heldRows.ts). Covered: the pane opens the
// whole diff with every file counted, expanding every file holds a range
// of the rows rather than all of them, and the file tree reaches the last
// line of the last file and then the first line of the first file, both
// outside the range held before the jump.

import { randomBytes } from 'node:crypto';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { expectEveryForgeCallHandled, openPullRequestReview, publishPullRequest } from './forge-helpers.js';

const TITLE = 'Large diff thread';
const FILES = 20;
const LINES = 40_000;
const HELD_LIMIT_PX = 12_000_000;

const fileName = (file: number) => `f${String(file).padStart(2, '0')}`;
// Fixed-width numbers: no line's text contains another's, so a substring
// match finds exactly one line (a line renders after its +/- marker).
const lineText = (file: number, line: number) => `${fileName(file)} line ${String(line).padStart(6, '0')}`;

function largeFiles(): Record<string, string> {
  const files: Record<string, string> = {};
  for (let file = 0; file < FILES; file += 1) {
    const lines = Array.from({ length: LINES }, (_, index) => lineText(file, index + 1));
    files[`big/${fileName(file)}.txt`] = `${lines.join('\n')}\n`;
  }
  return files;
}

async function seedLargePullRequest(harness: HarnessApp): Promise<void> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `large-diff-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  const files = largeFiles();
  const bytes = Object.values(files).reduce((sum, content) => sum + content.length, 0);
  expect(bytes, 'the added lines alone pass 10 MiB').toBeGreaterThan(10 * 1024 * 1024);
  await publishPullRequest(
    harness,
    seed.projects[0].path,
    { forge: 'github', project: `ao-e2e/large-diff-${randomBytes(4).toString('hex')}`, pulls: [{ number: 7, title: 'Large diff' }] },
    files,
  );
}

test('a pull request diff past 10 MiB and past the browser height limit opens and reaches every file', async ({
  harness,
  page,
}) => {
  test.setTimeout(180_000);
  await seedLargePullRequest(harness);
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);

  await expect(review.getByTestId('review-diff-stats')).toHaveText(`${FILES} files +${FILES * LINES}`, { timeout: 60_000 });
  await expect(review.getByTestId('review-error')).toHaveCount(0);
  // Files over 400 rows start collapsed.
  const expandAll = review.getByTestId('review-collapse-all-toggle');
  await expect(expandAll).toHaveAccessibleName('Expand all files');
  await expandAll.click();
  const scroll = review.getByTestId('review-scroll');
  await expect(review.getByText(lineText(0, 1))).toBeInViewport();
  expect(await scroll.evaluate((el) => el.scrollHeight)).toBeLessThanOrEqual(HELD_LIMIT_PX);

  const tree = review.getByTestId('review-tree-toggle');
  if ((await tree.getAttribute('aria-pressed')) !== 'true') await tree.click();

  const last = FILES - 1;
  await review.locator(`[data-testid="review-tree-file"][data-file-path="big/${fileName(last)}.txt"]`).click();
  await expect(review.getByText(lineText(last, 1))).toBeInViewport();
  // Moves of the held range can follow the reader to the end; scroll
  // until the last line is on screen.
  const lastLine = review.getByText(lineText(last, LINES));
  await expect
    .poll(async () => {
      await scroll.evaluate((el) => {
        el.scrollTop = el.scrollHeight;
      });
      return (await lastLine.count()) > 0 && (await lastLine.isVisible());
    })
    .toBe(true);
  await expect(lastLine).toBeInViewport();
  expect(await scroll.evaluate((el) => el.scrollHeight)).toBeLessThanOrEqual(HELD_LIMIT_PX);

  await review.locator(`[data-testid="review-tree-file"][data-file-path="big/${fileName(0)}.txt"]`).click();
  await expect(review.getByText(lineText(0, 1))).toBeInViewport();
  await expect(review.getByTestId('review-error')).toHaveCount(0);
  await expectEveryForgeCallHandled(harness);
});
