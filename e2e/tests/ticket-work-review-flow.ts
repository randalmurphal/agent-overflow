// A pull request of a ticket's Work shown in the app (docs/specs/issues.md,
// "Tickets, pull requests and branches"), shared by the desktop and compact
// projects, through the shipped UI, the real backend, the fake Jira and the
// fake forge. Covered: a merged pull request whose repository a project
// here has as its origin opens its review in the ticket's place, with its
// diff, its state and its number and title over it, the ticket inert under
// it; Open on GitHub opens its page; on desktop mod+f searches the review
// and Escape closes that find first; Back (Escape on desktop, the browser's
// Back on the phone, the same ladder as Android's) closes a CI log open in
// the review, then the review, and the ticket shows as it was with its
// Work not read again; the bar's Back on desktop and the header's on the
// phone do the same; a pull request of a fork's parent, which the fork's
// checkout cannot show, opens on the forge; and on desktop the review
// beside the Backlog at 380 px keeps its bar and title bar inside the
// ticket's column.

import type { Locator, Page } from '@playwright/test';

import { screenshot } from '../src/screenshot.js';
import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { publishPullRequest, seedForge, type ForgeRepo } from './forge-helpers.js';
import { openBacklog } from './issues-backlog-flow.js';
import { WEB1, forgeOps, hostOpens, openWork, workOf, type WorkSurface } from './ticket-work-flow.js';
import { connectFakeJira, openIssuesPane, press, seedFakeJira } from './tracker-helpers.js';
import { harnessGit } from './worktree-removal-helpers.js';

const TITLE = 'WEB-1 Retry the cursor read';
const WEB3 = '10103';

/** A project that lists the WEB board, whose origin is a fake GitHub
 * repository where merged pull request 801 names `key` (WEB-1), with a CI
 * job. */
async function reviewProject(harness: HarnessApp, name: string, key = 'WEB-1'): Promise<{ repo: string }> {
  await seedFakeJira(harness);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name, repo: { commits: [{ message: 'init', files: { 'README.md': '# Web\n' } }] } }] });
  const { projectId, path } = seed.projects[0];
  await connectFakeJira(harness, projectId);
  const repo: ForgeRepo = {
    forge: 'github',
    project: `ao-e2e/${name}-${Date.now().toString(36)}`,
    pulls: [{
      number: 801, title: TITLE.replace('WEB-1', key), author: 'alice', authorName: 'Alice Ng', state: 'merged',
      ci: { id: 9_900, jobs: [{ id: 9_901, name: 'unit', stage: 'test', status: 'success', log: 'unit line 1\nunit line 2\n' }] },
    }],
  };
  await publishPullRequest(harness, path, repo, { 'src/cursor.ts': 'export const cursor = 1;\n' });
  return { repo: repo.project };
}

/** Opens Work's row of 801 once the forge search has answered. */
async function openRow(work: Locator, compact: boolean): Promise<void> {
  const row = work.getByTestId('ticket-work-pull').filter({ hasText: '#801' });
  await expect(row).toHaveAttribute('data-opens', 'review', { timeout: 30_000 });
  await press(row, compact);
}

/** The review shown in the ticket's place, once its diff has loaded. */
async function shownReview(pane: Locator): Promise<{ shown: Locator; review: Locator }> {
  const shown = pane.getByTestId('ticket-review');
  await expect(shown).toBeVisible();
  const review = shown.getByTestId('review-pane');
  await expect(review.getByTestId('review-diff-stats')).toBeVisible({ timeout: 60_000 });
  return { shown, review };
}

async function openLog(page: Page, review: Locator, compact: boolean): Promise<Locator> {
  // GitHub's checks are one chip.
  await press(review.getByTestId('review-ci-chip').first(), compact);
  await press(page.getByTestId('review-ci-job').filter({ hasText: 'unit' }), compact);
  const log = review.getByTestId('review-ci-log');
  await expect(log).toBeVisible({ timeout: 30_000 });
  return log;
}

export function registerTicketWorkReviewFlow({ compact }: WorkSurface): void {
  test('a pull request row opens its review in the ticket\'s place, and Back steps out of the review, then shows the ticket as it was', async ({ harness, page }) => {
    test.setTimeout(180_000);
    const { repo } = await reviewProject(harness, compact ? 'compact-work-review' : 'work-review');
    const opens = hostOpens(page);
    await harness.open(page);
    const { ticket, work } = await openWork(page, WEB1, compact);
    await expect(work.getByTestId('ticket-work-pull').filter({ hasText: '#801' })).toHaveAttribute('data-state', 'merged', { timeout: 30_000 });
    await expect(work.getByTestId('ticket-work-searching')).toHaveCount(0, { timeout: 30_000 });
    const searches = (await forgeOps(harness, 'PullsByKey')).length;
    const pane = page.getByTestId('issues-pane');
    const under = pane.getByTestId('ticket-under-review');

    await openRow(work, compact);
    const { shown, review } = await shownReview(pane);
    await expect(shown.getByTestId('ticket-review-number')).toHaveText('#801');
    await expect(shown.getByTestId('ticket-review-title')).toHaveText(TITLE);
    await expect(review.getByTestId('review-pr-state')).toHaveText(/merged/i);
    await expect(under).toHaveAttribute('inert', '');
    // The phone's header holds the one Back.
    await expect(shown.getByTestId('ticket-review-back')).toHaveCount(compact ? 0 : 1);
    if (compact) await expect(pane.getByTestId('issues-back')).toBeVisible();

    // Its page on the forge is one press away.
    await press(shown.getByTestId('ticket-review-forge'), compact);
    await expect.poll(opens).toEqual([`https://github.com/${repo}/pull/801`]);

    if (!compact) {
      // mod+f searches the review the person is in; Escape closes the
      // find, not the review.
      await page.keyboard.press('ControlOrMeta+f');
      const bar = shown.getByTestId('pane-find-bar');
      await expect(bar).toBeVisible();
      await expect(pane.getByTestId('pane-find-bar')).toHaveCount(1);
      await page.keyboard.type('cursor');
      await expect(bar.getByTestId('pane-find-count')).toHaveText(/\d+\/\d+/, { timeout: 15_000 });
      await page.keyboard.press('Escape');
      await expect(bar).toHaveCount(0);
      await expect(shown).toBeVisible();
    }

    // A CI log open in the review closes first, the review still shown.
    const log = await openLog(page, review, compact);
    if (compact) await page.goBack();
    else await page.keyboard.press('Escape');
    await expect(log).toHaveCount(0);
    await expect(review.getByTestId('review-diff-stats')).toBeVisible();

    // Then the review: the ticket shows as it was, with its Work open and
    // not read again.
    if (compact) await page.goBack();
    else await press(shown.getByTestId('ticket-review-back'), compact);
    await expect(shown).toHaveCount(0);
    await expect(under).not.toHaveAttribute('inert');
    await expect(ticket).toHaveAttribute('data-issue', WEB1);
    await expect(work).toBeVisible();
    expect(await forgeOps(harness, 'PullsByKey')).toHaveLength(searches);

    // Opened again, the header's Back on the phone and Escape on desktop
    // show the ticket too.
    await openRow(work, compact);
    await shownReview(pane);
    if (compact) await press(pane.getByTestId('issues-back'), true);
    else await page.keyboard.press('Escape');
    await expect(shown).toHaveCount(0);
    await expect(work).toBeVisible();
  });

  test('a pull request of a fork\'s parent, which the fork\'s checkout cannot show, opens on the forge', async ({ harness, page }) => {
    test.setTimeout(150_000);
    const name = compact ? 'compact-work-fork' : 'work-fork';
    await seedFakeJira(harness);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name, repo: { commits: [{ message: 'init', files: { 'README.md': '# Web\n' } }] } }] });
    const { projectId, path } = seed.projects[0];
    await connectFakeJira(harness, projectId);
    const stamp = Date.now().toString(36);
    const parent = `ao-e2e/${name}-up-${stamp}`;
    const fork = `ao-e2e/${name}-${stamp}`;
    await seedForge(harness, [
      { forge: 'github', project: parent, pulls: [{ number: 811, title: 'Fix the login', author: 'alice', headRef: 'WEB-1-login', headRepo: fork }] },
      { forge: 'github', project: fork, parent },
    ]);
    harnessGit(harness, path, 'branch', 'WEB-1-login');
    harnessGit(harness, path, 'remote', 'add', 'origin', `git://github.com/${fork}.git`);
    const opens = hostOpens(page);
    await harness.open(page);
    const { work } = await openWork(page, WEB1, compact);
    const row = work.getByTestId('ticket-work-pull').filter({ hasText: '#811' });
    await expect(row).toHaveAttribute('data-opens', 'forge', { timeout: 30_000 });
    await press(row, compact);
    await expect.poll(opens).toEqual([`https://github.com/${parent}/pull/811`]);
    await expect(page.getByTestId('ticket-review')).toHaveCount(0);
  });

  if (!compact) {
    test('the review beside the Backlog at 380 px keeps its bar and title bar inside the ticket\'s column', async ({ harness, page }, testInfo) => {
      test.setTimeout(180_000);
      await page.setViewportSize({ width: 1600, height: 900 });
      // WEB-3, the row the Backlog section shows unfolded.
      await reviewProject(harness, 'work-review-narrow', 'WEB-3');
      await harness.open(page);
      const pane = await openIssuesPane(page, false);
      await expect(pane.getByTestId('issues-card').first()).toBeVisible({ timeout: 30_000 });
      const backlog = await openBacklog(page, pane, false);
      await backlog.locator(`[data-testid="issues-backlog-row"][data-issue="${WEB3}"]`).getByTestId('issues-backlog-row-open').click();
      const ticket = pane.getByTestId('ticket-screen');
      await expect(ticket).toHaveAttribute('data-issue', WEB3);
      await ticket.getByRole('radio', { name: 'Beside' }).click();
      const placed = pane.getByTestId('issues-ticket');
      await expect(placed).toHaveAttribute('data-placement', 'beside');
      await pane.getByTestId('issues-ticket-resize').focus();
      await page.keyboard.press('End');
      await expect.poll(async () => Math.round((await placed.boundingBox())!.width)).toBe(380);

      await openRow(await workOf(ticket, false), false);
      const { shown, review } = await shownReview(pane);
      await expect(review.getByTestId('review-pr-header')).toBeVisible();
      const column = (await placed.boundingBox())!;
      for (const part of [shown.getByTestId('ticket-review-bar'), shown.getByTestId('ticket-review-forge'), shown.getByTestId('ticket-review-back'), review.getByTestId('review-pr-header')]) {
        const box = (await part.boundingBox())!;
        expect(box.x, `${await part.getAttribute('data-testid')} starts inside the column`).toBeGreaterThanOrEqual(column.x - 0.5);
        expect(box.x + box.width, `${await part.getAttribute('data-testid')} ends inside the column`).toBeLessThanOrEqual(column.x + column.width + 0.5);
      }
      // Nothing in the review scrolls the column sideways.
      expect(await shown.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(0);
      await screenshot(placed, { path: testInfo.outputPath('review-beside-380.png') });
    });
  }
}
