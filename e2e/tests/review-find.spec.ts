// Find inside the review pane (mod+f), through the shipped UI, the real
// backend and the fake forge. Covered: mod+f with the review focused opens
// the bar under the review's toolbar, in the flow; the count covers every
// match of the diff, rows the virtualizer has not mounted and a file too
// large to open by default included; the new query scrolls its off-screen
// match in and paints it current; Enter steps to the match in the
// collapsed file, which opens, and wraps; Hide tests narrows the count and
// keeps the current match; Escape closes the bar, clears the paint and
// returns focus to the diff. With the conflict view open, find searches
// the conflicted file and not the diff. With a CI job log open, find
// counts the log on the backend, opens the collapsed step holding the
// match and scrolls its line in, painted current. Compact:
// compact-review-find.spec.ts.
// Unit and component coverage: reviewFindScan.test.ts, reviewFind.svelte.test.ts,
// ciLogFind.test.ts, findMatch.test.ts, PaneFindBar.test.ts,
// findHighlights.browser.test.ts, internal/cilog/search_test.go.
import { randomBytes } from 'node:crypto';
import type { Locator, Page } from '@playwright/test';

import { expect, test, type SeedResult } from './fixtures.js';
import { expectEveryForgeCallHandled, openPullRequestReview, publishPullRequest, type ForgeStep } from './forge-helpers.js';
import { pushToOriginMain } from './review-conflicts-flow.js';

const TITLE = 'Find in review';
const NEEDLE = 'needle-deep';

function lines(count: number, needleAt: number, needle: string): string {
  return Array.from({ length: count }, (_, i) => (i === needleAt ? `export const found = '${needle}';` : `export const value${i} = ${i};`)).join('\n') + '\n';
}

/** The ranges painted under a highlight name, as text. */
function painted(page: Page, name: string): Promise<string[]> {
  return page.evaluate((n) => {
    const highlight = CSS.highlights.get(n);
    return highlight ? [...highlight].map((range) => range.toString()) : [];
  }, name);
}

/** Whether the current match's range lies inside the review's scroller. */
function currentInView(page: Page, name: string): Promise<boolean> {
  return page.evaluate((n) => {
    const highlight = CSS.highlights.get(`${n}-current`);
    const range = highlight ? [...highlight][0] : undefined;
    const scroller = document.querySelector('section[data-pane-kind="review"] [data-testid="review-scroll"]');
    if (!(range instanceof Range) || !scroller) return false;
    const rect = range.getBoundingClientRect();
    const view = scroller.getBoundingClientRect();
    return rect.height > 0 && rect.top >= view.top && rect.bottom <= view.bottom;
  }, name);
}

async function highlightName(review: Locator): Promise<string> {
  const id = await review.getAttribute('data-pane-id');
  return `ao-find-${id!.replace(/[^A-Za-z0-9_-]/g, '_')}`;
}

test('mod+f finds across the whole diff, opens what hides a match, and follows the filter', async ({ harness, page }) => {
  test.setTimeout(120_000);
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: `review-find-${randomBytes(4).toString('hex')}`,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
      threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  await publishPullRequest(harness, seeded.projects[0].path, {
    forge: 'github',
    project: `ao-e2e/review-find-${randomBytes(4).toString('hex')}`,
    pulls: [{ number: 41, title: 'Values', body: '' }],
  }, {
    // Tall enough that the virtualizer leaves its far rows unmounted.
    'src/big.ts': lines(3000, 2600, NEEDLE),
    // Over the row count that starts a file collapsed.
    'src/huge.ts': lines(5200, 4000, NEEDLE),
    'src/values.test.ts': lines(5, 2, NEEDLE),
  });
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);
  await expect(review.getByTestId('review-diff-stats')).toContainText('3 files', { timeout: 60_000 });
  const scroller = review.getByTestId('review-scroll');
  const hugeToggle = review.getByRole('button', { name: /^(Expand|Collapse) src\/huge\.ts$/ }).first();
  // At the list's end the large file sits collapsed and only the test
  // file's match is mounted; at the top none is.
  await scroller.evaluate((el) => { el.scrollTop = el.scrollHeight; });
  await expect(hugeToggle).toHaveAttribute('aria-expanded', 'false');
  await expect(review.getByText(NEEDLE)).toHaveCount(1);
  await scroller.evaluate((el) => { el.scrollTop = 0; });
  await expect(review.getByTestId('review-file-header-path').first()).toContainText('src/big.ts');
  await expect(review.getByText(NEEDLE)).toHaveCount(0);

  await scroller.focus();
  await page.keyboard.press('ControlOrMeta+f');
  const bar = review.getByTestId('pane-find-bar');
  await expect(bar).toBeVisible();
  await expect(review.getByTestId('pane-find-input')).toBeFocused();
  await expect(review.getByTestId('pane-find-input')).toHaveAttribute('placeholder', 'Find in diff');
  const toolbarBox = (await review.getByTestId('review-toolbar').boundingBox())!;
  const barBox = (await bar.boundingBox())!;
  const scrollerBox = (await scroller.boundingBox())!;
  expect(barBox.y).toBeGreaterThanOrEqual(toolbarBox.y + toolbarBox.height - 1);
  expect(scrollerBox.y).toBeGreaterThanOrEqual(barBox.y + barBox.height - 1);

  const name = await highlightName(review);
  const count = bar.getByTestId('pane-find-count');
  await page.keyboard.type(NEEDLE);
  await expect(count).toHaveText('1/3');
  // The query lands on its off-screen match and paints it current.
  await expect.poll(() => painted(page, `${name}-current`)).toEqual([NEEDLE]);
  await expect.poll(() => currentInView(page, name)).toBe(true);

  // The next match is inside the collapsed file: it opens.
  await page.keyboard.press('Enter');
  await expect(count).toHaveText('2/3');
  await expect(hugeToggle).toHaveAttribute('aria-expanded', 'true');
  await expect.poll(() => currentInView(page, name)).toBe(true);
  await page.keyboard.press('Enter');
  await expect(count).toHaveText('3/3');
  await expect.poll(() => currentInView(page, name)).toBe(true);
  await page.keyboard.press('Enter');
  await expect(count).toHaveText('1/3');
  await expect.poll(() => currentInView(page, name)).toBe(true);

  // Hide tests narrows the diff, and the count with it; the current match stays.
  const treeToggle = review.getByTestId('review-tree-toggle');
  if ((await treeToggle.getAttribute('aria-pressed')) === 'false') await treeToggle.click();
  await review.getByTestId('review-tree-ext-trigger').click();
  await page.locator('[data-popover]').last().getByTestId('review-tree-hide-tests').click();
  await page.keyboard.press('Escape');
  await expect(count).toHaveText('1/2');

  // Escape from the bar closes it, clears the paint and returns focus.
  await review.getByTestId('pane-find-input').focus();
  await page.keyboard.press('Escape');
  await expect(bar).toHaveCount(0);
  await expect(scroller).toBeFocused();
  await expect.poll(() => painted(page, name)).toEqual([]);
  await expect.poll(() => painted(page, `${name}-current`)).toEqual([]);
  await expectEveryForgeCallHandled(harness);
});

test('with the conflict view open, find searches the conflicted file', async ({ harness, page }) => {
  test.setTimeout(90_000);
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: `review-find-conflicts-${randomBytes(4).toString('hex')}`,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n', 'shared.txt': 'original\n' } }] },
      threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  const repo = await publishPullRequest(harness, seeded.projects[0].path, {
    forge: 'gitlab',
    project: `ao-e2e/review-find-conflicts-${randomBytes(4).toString('hex')}`,
    pulls: [{ number: 7, title: 'Edit the shared file', body: '', mergeable: 'conflicts' }],
  }, { 'shared.txt': 'from the feature\n', 'notes.md': 'only in the diff: from the feature\n' });
  await pushToOriginMain(harness, repo, 'from main\n');
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);

  const scroller = review.getByTestId('review-scroll');
  await expect(review.getByTestId('review-diff-stats')).toContainText('2 files', { timeout: 30_000 });
  await scroller.focus();
  await page.keyboard.press('ControlOrMeta+f');
  const bar = review.getByTestId('pane-find-bar');
  const count = bar.getByTestId('pane-find-count');
  await page.keyboard.type('from the feature');
  await expect(count).toHaveText('1/2');

  const pill = review.getByTestId('review-pr-readiness');
  await expect(pill).toContainText('Blocked', { timeout: 30_000 });
  const section = review.getByTestId('review-pr-merge');
  if ((await section.getAttribute('data-open')) !== 'true') await pill.click();
  await section.getByTestId('review-merge-row-conflicts').click();
  await expect(review.getByTestId('review-line-block').first()).toContainText('<<<<<<<');
  // The conflicted file holds the feature's line once; notes.md is not on screen.
  await expect(count).toHaveText('1/1');
  await expect(review.getByTestId('pane-find-input')).toHaveAttribute('placeholder', 'Find in conflicts');
  const name = await highlightName(review);
  await expect.poll(() => painted(page, `${name}-current`)).toEqual(['from the feature']);
  await review.getByTestId('pane-find-input').fill('from main');
  await expect(count).toHaveText('1/1');
  await review.getByTestId('pane-find-input').fill('>>>>>>>');
  await expect(count).toHaveText('1/1');
  await expectEveryForgeCallHandled(harness);
});

test('with a CI job log open, find counts the log and opens the step holding the match', async ({ harness, page }) => {
  test.setTimeout(90_000);
  const steps: ForgeStep[] = [
    { number: 1, name: 'Set up job', status: 'success', startedAt: '2026-01-01T00:00:00Z', completedAt: '2026-01-01T00:00:05Z' },
    { number: 2, name: 'Run make test', status: 'success', startedAt: '2026-01-01T00:00:05Z', completedAt: '2026-01-01T00:10:00Z' },
  ];
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: `review-find-ci-${randomBytes(4).toString('hex')}`,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
      threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  await publishPullRequest(harness, seeded.projects[0].path, {
    forge: 'github',
    project: `ao-e2e/review-find-ci-${randomBytes(4).toString('hex')}`,
    pulls: [{
      number: 24,
      title: 'Log',
      ci: {
        id: 9_400,
        name: 'CI',
        jobs: [{
          id: 7_400,
          name: 'build',
          status: 'success',
          steps,
          synthetic: { lineBytes: 80, sections: [{ name: 'Set up job', lines: 50 }, { name: 'Run make test', lines: 3000 }] },
        }],
      },
    }],
  });
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);
  await review.getByTestId('review-ci-chip').filter({ hasText: 'CI' }).click();
  await page.getByTestId('review-ci-job').filter({ hasText: 'build' }).click();
  const log = review.getByTestId('review-ci-log');
  const step2 = log.locator('[data-testid="review-ci-section"][data-key="step:2"]');
  await expect(step2.getByTestId('review-ci-section-toggle')).toBeEnabled({ timeout: 60_000 });
  await expect(step2).toHaveAttribute('data-open', 'false');

  const scroll = log.getByTestId('review-ci-log-scroll');
  await scroll.focus();
  await page.keyboard.press('ControlOrMeta+f');
  const bar = review.getByTestId('pane-find-bar');
  await expect(review.getByTestId('pane-find-input')).toHaveAttribute('placeholder', 'Find in log');
  await page.keyboard.type('run_make_test_1 line 2500');
  await expect(bar.getByTestId('pane-find-count')).toHaveText('1/1');
  // The step opened; its header has scrolled away above the line.
  await expect(scroll).toContainText('run_make_test_1 line 2500 ');
  const name = await highlightName(review);
  await expect.poll(() => painted(page, `${name}-current`)).toEqual(['run_make_test_1 line 2500']);
  await expect.poll(() => page.evaluate((n) => {
    const range = [...(CSS.highlights.get(`${n}-current`) ?? [])][0];
    const view = document.querySelector('section[data-pane-kind="review"] [data-testid="review-ci-log-scroll"]');
    if (!(range instanceof Range) || !view) return false;
    const rect = range.getBoundingClientRect();
    const box = view.getBoundingClientRect();
    return rect.height > 0 && rect.top >= box.top && rect.bottom <= box.bottom;
  }, name)).toBe(true);
  await review.getByTestId('pane-find-input').fill('no such text anywhere');
  await expect(bar.getByTestId('pane-find-count')).toHaveText('No results');
  await expectEveryForgeCallHandled(harness);
});
