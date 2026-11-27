// A pull request of a ticket's Work shown in the app (docs/specs/issues.md,
// "Tickets, pull requests and branches"), shared by the desktop and compact
// projects, through the shipped UI, the real backend, the fake Jira and the
// fake forge. Covered: a merged pull request whose repository a project
// here has as its origin opens its review in the ticket's place, with its
// diff, its state and its title, its number in the bar over it, the ticket
// hidden and inert under it; Open on GitHub opens its page; on desktop
// mod+f searches the review
// and Escape closes that find first; Back (Escape on desktop, the browser's
// Back on the phone, the same ladder as Android's) closes a CI log open in
// the review, then the review, and the ticket shows as it was with its
// Work not read again; the bar's Back on desktop and the header's on the
// phone do the same; a pull request of a fork's parent, which the fork's
// checkout cannot show, opens on the forge; a review whose code fails to
// load keeps its bar, Back and Open on GitHub; the review takes the
// keyboard when it opens, but not from where the person moved it while
// its code loaded, by a focus or by a press on something that takes none;
// on desktop the review
// beside the Backlog at 380 px keeps its bar and title bar inside the
// ticket's column; and on desktop, in the ticket's own pane, the review's
// bar is the pane's header, painted over nothing of the ticket's, and the
// pane-navigation chord puts the keyboard in the review.

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

/** What holds the keyboard: the review's body, the bar's forge button,
 * or what else. */
function keyboardIn(page: Page): Promise<string> {
  return page.evaluate(() => {
    const active = document.activeElement;
    if (!active || active === document.body) return 'body';
    if (active.closest('[data-testid="ticket-review-body"]')) return 'review body';
    return active.closest('[data-testid]')?.getAttribute('data-testid') ?? active.tagName;
  });
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
    await expect(review.getByTestId('review-pr-title')).toHaveAttribute('title', TITLE);
    await expect(review.getByTestId('review-pr-state')).toHaveText(/merged/i);
    // Opened by a person, it takes the keyboard.
    await expect.poll(() => keyboardIn(page)).toBe('review body');
    await expect(under).toHaveAttribute('inert', '');
    await expect(under).toBeHidden();
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
    await expect(under).toBeVisible();
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

  test('a review whose code fails to load keeps its bar, Back and the forge link', async ({ harness, page }) => {
    test.setTimeout(150_000);
    const { repo } = await reviewProject(harness, compact ? 'compact-work-review-fail' : 'work-review-fail');
    const opens = hostOpens(page);
    await harness.open(page);
    const { work } = await openWork(page, WEB1, compact);
    await page.route(/\/TicketReview-[^/]*\.js(\?|$)/, (route) => route.abort());
    await openRow(work, compact);
    const pane = page.getByTestId('issues-pane');
    const shown = pane.getByTestId('ticket-review');
    await expect(shown.getByTestId('ticket-review-load-error')).toBeVisible({ timeout: 30_000 });
    await expect(shown.getByTestId('ticket-review-number')).toHaveText('#801');
    await press(shown.getByTestId('ticket-review-forge'), compact);
    await expect.poll(opens).toEqual([`https://github.com/${repo}/pull/801`]);
    if (compact) await press(pane.getByTestId('issues-back'), true);
    else await press(shown.getByTestId('ticket-review-back'), false);
    await expect(shown).toHaveCount(0);
    await expect(work).toBeVisible();
  });

  test('a review whose code loads after the person moved the keyboard leaves it where they put it', async ({ harness, page }) => {
    test.setTimeout(150_000);
    await reviewProject(harness, compact ? 'compact-work-review-held' : 'work-review-held');
    await harness.open(page);
    const { work } = await openWork(page, WEB1, compact);
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let asked!: () => void;
    const requested = new Promise<void>((resolve) => { asked = resolve; });
    await page.route(/\/TicketReview-[^/]*\.js(\?|$)/, async (route) => {
      asked();
      await held;
      await route.continue();
    });
    await openRow(work, compact);
    const pane = page.getByTestId('issues-pane');
    const shown = pane.getByTestId('ticket-review');
    await requested;
    await expect(shown.getByTestId('ticket-review-loading')).toBeVisible();
    // While the code loads the person moves to the bar's forge button.
    const forge = shown.getByTestId('ticket-review-forge');
    await forge.focus();
    await expect.poll(() => keyboardIn(page)).toBe('ticket-review-forge');
    release();
    await shownReview(pane);
    await expect.poll(() => keyboardIn(page)).toBe('ticket-review-forge');
    // A key pressed now reaches the button, not the review.
    await expect(forge).toBeFocused();
  });

  test('a review whose code loads after a press on the pane\'s header, which takes no focus, leaves the keyboard alone', async ({ harness, page }) => {
    test.setTimeout(150_000);
    await reviewProject(harness, compact ? 'compact-work-review-press' : 'work-review-press');
    await harness.open(page);
    const { work } = await openWork(page, WEB1, compact);
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let asked!: () => void;
    const requested = new Promise<void>((resolve) => { asked = resolve; });
    await page.route(/\/TicketReview-[^/]*\.js(\?|$)/, async (route) => {
      asked();
      await held;
      await route.continue();
    });
    await openRow(work, compact);
    const pane = page.getByTestId('issues-pane');
    await requested;
    await expect(pane.getByTestId('ticket-review-loading')).toBeVisible();
    // A point of the Issues pane's header that nothing focusable covers.
    const header = page.locator('section[data-pane-kind="issues"]').getByTestId('tool-pane-header');
    const box = (await header.boundingBox())!;
    const point = await page.evaluate(([left, top, width, height]) => {
      const y = top + height / 2;
      for (let x = left + width - 2; x > left; x -= 4) {
        const el = document.elementFromPoint(x, y);
        if (el && !el.closest('button, a, input, select, textarea, [tabindex], [role="tab"], [role="radio"], [contenteditable="true"]')) return { x, y };
      }
      return null;
    }, [box.x, box.y, box.width, box.height]);
    expect(point, 'a blank point of the header').not.toBeNull();
    await page.mouse.click(point!.x, point!.y);
    release();
    await shownReview(pane);
    await expect(pane.getByTestId('ticket-review-body')).toBeVisible();
    expect(await keyboardIn(page)).not.toBe('review body');
  });

  if (!compact) {
    test('in the ticket\'s own pane the review\'s bar is the pane\'s header over nothing of the ticket\'s, and a pane move puts the keyboard in the review', async ({ harness, page }) => {
      test.setTimeout(180_000);
      await page.setViewportSize({ width: 1800, height: 900 });
      await reviewProject(harness, 'work-review-own');
      await harness.open(page);
      const issues = await openIssuesPane(page, false);
      const card = issues.locator(`[data-testid="issues-card"][data-issue="${WEB1}"]`);
      await expect(card).toBeVisible({ timeout: 30_000 });
      await card.getByTestId('issues-card-open').click({ modifiers: ['ControlOrMeta'] });
      const own = page.locator('section[data-pane-kind="ticket"]');
      const ticket = own.getByTestId('ticket-screen');
      await expect(ticket).toHaveAttribute('data-issue', WEB1);
      await openRow(await workOf(ticket, false), false);
      const { shown } = await shownReview(own);

      // What paints at the bar's number and Back is the review's, not the
      // ticket's header under it.
      for (const part of [shown.getByTestId('ticket-review-number'), shown.getByTestId('ticket-review-back'), shown.getByTestId('ticket-review-forge')]) {
        await expect(part).toBeVisible();
        const box = (await part.boundingBox())!;
        const hit = await page.evaluate(([x, y]) => {
          const el = document.elementFromPoint(x, y);
          return el?.closest('[data-testid="ticket-review"]') ? 'review' : el?.closest('[data-testid="ticket-under-review"]') ? 'ticket' : String(el?.tagName);
        }, [box.x + box.width / 2, box.y + box.height / 2]);
        expect(hit, `${await part.getAttribute('data-testid')} is painted by`).toBe('review');
      }
      await expect(own.getByTestId('ticket-key')).toBeHidden();

      // Out to the Issues pane and back by the chord: the keyboard lands in
      // the review's diff, and what is typed after it reaches no other pane.
      await page.keyboard.press('Alt+h');
      await expect(page.locator('section[data-pane-kind="issues"]')).toHaveAttribute('data-pane-focused', 'true');
      await page.keyboard.press('Alt+l');
      await expect(own).toHaveAttribute('data-pane-focused', 'true');
      await expect.poll(() => page.evaluate(() => {
        const active = document.activeElement;
        if (!active || active === document.body) return 'body';
        if (active.closest('[data-testid="ticket-under-review"]')) return 'ticket';
        return active.closest('[data-testid="ticket-review"]') && active.hasAttribute('data-pane-scroller') ? 'review scroller' : active.getAttribute('data-testid') ?? active.tagName;
      })).toBe('review scroller');
    });

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
