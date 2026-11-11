// The browser's Back inside a review on the compact layout (the ladder
// utils/stepBack.ts runs, as Android's Back does): a surface open inside
// the review closes before the review. In a thread's companion review an
// open menu, the full-screen diagram viewer, the open file-tree rail and
// a CI job's log each close on one Back with the review still on screen,
// the log returning to the diff line the reader left; the next Back closes
// the review. In the review pinned on the Pull requests page the open rail
// and the log each close on one Back before the review returns to the list.
// Find before the log: compact-review-find.spec.ts, pulls-page-find-flow.ts.
import { randomBytes } from 'node:crypto';
import type { Locator, Page } from '@playwright/test';

import { expect, test, type SeedResult } from './fixtures.js';
import { expandReviewSection, expectEveryForgeCallHandled, publishPullRequest } from './forge-helpers.js';
import { launchPullsHarness, openPage, pickFilter, row } from './pulls-page-flow.js';

const TITLE = 'Phone review back';
const BODY = 'The flow:\n\n```mermaid\ngraph TD\n  A[Queue] --> B[Send]\n```\n';
const LINE = 'export const value300 = 300;';

function source(): string {
  return Array.from({ length: 600 }, (_, i) => `export const value${i} = ${i};`).join('\n') + '\n';
}

function pull(id: number) {
  return {
    number: 7,
    title: 'Values',
    body: BODY,
    ci: { id, jobs: [{ id: id + 1, name: 'unit', stage: 'test', status: 'success' as const, log: 'unit line 1\nunit line 2\n' }] },
  };
}

async function openLog(page: Page, review: Locator): Promise<Locator> {
  await review.getByTestId('review-ci-chip').filter({ hasText: 'test' }).tap();
  await page.getByTestId('review-ci-job').filter({ hasText: 'unit' }).tap();
  const log = review.getByTestId('review-ci-log-scroll');
  await expect(log).toBeVisible({ timeout: 30_000 });
  return log;
}

async function openRail(review: Locator): Promise<Locator> {
  const rail = review.getByTestId('review-rail');
  if ((await rail.getAttribute('data-open')) !== 'true') await rail.getByTestId('review-tree-toggle').tap();
  await expect(rail).toHaveAttribute('data-open', 'true');
  return rail;
}

test('Back closes what is open inside a companion review before the review', async ({ harness, page }) => {
  test.setTimeout(120_000);
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: `compact-review-back-${randomBytes(4).toString('hex')}`,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
      threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  await publishPullRequest(harness, seeded.projects[0].path, {
    forge: 'gitlab',
    project: `ao-e2e/compact-review-back-${randomBytes(4).toString('hex')}`,
    pulls: [pull(9_800)],
  }, { 'src/values.ts': source() });
  await harness.open(page);

  await page.getByTestId('thread-row').filter({ hasText: TITLE }).tap();
  const more = page.getByTestId('chat-header-more');
  const menu = page.locator('[data-popover]');
  const prRow = menu.getByRole('menuitem', { name: 'MR !7' });
  await expect(async () => {
    if ((await menu.count()) > 0) await page.keyboard.press('Escape');
    await expect(menu).toHaveCount(0);
    await more.tap();
    await expect(prRow).toBeVisible({ timeout: 2_000 });
  }).toPass({ timeout: 30_000 });
  await prRow.tap();
  const review = page.locator('section[data-pane-kind="review"]');
  const header = review.getByTestId('review-pr-header');
  await expect(header).toBeVisible();
  await expect(review).toBeInViewport({ ratio: 1 });
  await expect(review.getByTestId('review-diff-stats')).toContainText('1 file', { timeout: 30_000 });

  // An open menu closes first.
  await review.getByTestId('review-view-options').tap();
  await expect(menu).toHaveCount(1);
  await page.goBack();
  await expect(menu).toHaveCount(0);
  await expect(review).toBeInViewport({ ratio: 1 });

  // The full-screen diagram from the description closes first.
  const description = await expandReviewSection(page, 'review-pr-description');
  const diagram = description.locator('.streamdown-mermaid-host');
  await expect(diagram.locator('svg').first()).toBeVisible({ timeout: 30_000 });
  await diagram.getByLabel('Toggle expand').tap();
  const viewer = page.getByRole('dialog', { name: 'Diagram Viewer' });
  await expect(viewer).toBeVisible();
  await page.goBack();
  await expect(viewer).toHaveCount(0);
  await expect(review).toBeInViewport({ ratio: 1 });

  // The open rail covers the diff and closes first.
  const rail = await openRail(review);
  await page.goBack();
  await expect(rail).toHaveAttribute('data-open', 'false');
  await expect(review).toBeInViewport({ ratio: 1 });

  // The log closes first and the diff comes back where the reader was.
  const scroller = review.getByTestId('review-scroll');
  const line = review.locator('[data-find-key]').filter({ hasText: LINE });
  await expect(async () => {
    if ((await line.count()) === 0) await scroller.evaluate((el) => { el.scrollTop += el.clientHeight; });
    await expect(line).toHaveCount(1, { timeout: 500 });
  }).toPass({ timeout: 20_000 });
  await line.evaluate((el) => el.scrollIntoView({ block: 'center' }));
  await expect(line).toBeInViewport();
  const before = (await line.boundingBox())!.y;
  const log = await openLog(page, review);
  await page.goBack();
  await expect(log).toHaveCount(0);
  await expect(header).toBeVisible();
  await expect(review).toBeInViewport({ ratio: 1 });
  await expect(line).toBeInViewport();
  expect(Math.abs((await line.boundingBox())!.y - before)).toBeLessThanOrEqual(2);

  // The next Back leaves the review.
  await page.goBack();
  await expect(header).toHaveCount(0);
  await expectEveryForgeCallHandled(harness);
});

test('Back closes the rail and the log inside the pinned review before the list', async ({ page }) => {
  test.setTimeout(120_000);
  const harness = await launchPullsHarness();
  try {
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: 'pinned-back', repo: { commits: [{ message: 'init', files: { 'README.md': '# Pinned\n' } }] }, threads: [] }],
    });
    await publishPullRequest(harness, seed.projects[0].path, {
      forge: 'gitlab',
      project: `ao-e2e/pinned-back-${randomBytes(4).toString('hex')}`,
      pulls: [pull(9_900)],
    }, { 'src/values.ts': source() });
    await harness.open(page);
    const pane = await openPage(page);
    await pickFilter(page, pane, 'all');
    const column = pane.getByTestId('pulls-list-column');
    await row(page, 7).getByRole('button').first().tap();
    const review = page.getByTestId('review-pane');
    await expect(review.getByTestId('review-diff-stats')).toContainText('1 file', { timeout: 60_000 });
    await expect(column).toBeHidden();

    const rail = await openRail(review);
    await page.goBack();
    await expect(rail).toHaveAttribute('data-open', 'false');
    await expect(review).toBeVisible();
    await expect(column).toBeHidden();

    const log = await openLog(page, review);
    await page.goBack();
    await expect(log).toHaveCount(0);
    await expect(review.getByTestId('review-diff-stats')).toBeVisible();

    await page.goBack();
    await expect(review).toHaveCount(0);
    await expect(column).toBeVisible();
    await expectEveryForgeCallHandled(harness);
  } finally {
    await page.close();
    await harness.close();
  }
});
