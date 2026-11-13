// The Conversation feed's live arrivals, Hide resolved threads and the
// Pull requests list's time, shared by the desktop and compact projects,
// through the shipped UI, the real backend and the fake forge, on a
// GitHub pull request and a GitLab merge request opened pinned from the
// Pull requests page. Covered: the list's age is the created age under
// the Created sort and the updated age otherwise, and its hover text
// names both times; a comment the forge gains while the pane is open
// appears in the feed with no press, and with the reader scrolled into
// the feed it joins above without moving the card at the top of the
// section's viewport, behind an up-arrow "N new" chip that scrolls to it
// and goes; the Conversation header's refresh shows a comment the forge
// gained with no chip; Hide resolved threads in the view-options menu
// takes the resolved thread out of the feed and the diff, the feed says
// "1 resolved hidden", another PR opened next starts with its resolved
// thread hidden, and pressing the line there shows it again. Dark-theme
// screenshots of the view-options menu, the hidden note, the chip and the
// list sorted by Created are attached.

import { randomBytes } from 'node:crypto';
import type { Locator, Page, TestInfo } from '@playwright/test';

import { HARNESS_TIMING, launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  expectEveryForgeCallHandled,
  publishPullRequest,
  seedForge,
  type ForgeComment,
  type ForgeRepo,
} from './forge-helpers.js';
import { openPage, openPinnedReview, pickFilter, row } from './pulls-page-flow.js';

export interface ConversationSurface {
  /** The compact project: taps, the review full width. */
  compact: boolean;
}

type Forge = 'github' | 'gitlab';
const NUMBER: Record<Forge, number> = { github: 61, gitlab: 62 };
// A second PR the remembered Hide resolved threads applies to.
const OTHER: Record<Forge, number> = { github: 71, gitlab: 72 };
const HOUR = 3_600_000;
const DAY = 24 * HOUR;

// Enough PR comments that the feed overflows its section. Every comment
// has a fixed id, as a forge's does, so a re-seed keeps its identity.
function notes(base: number): ForgeComment[] {
  return Array.from({ length: 10 }, (_, index) => ({
    id: 5_100 + index,
    author: 'bob',
    authorName: 'Bob Stone',
    body: `Note ${index + 1} on the change.\n\nA second paragraph so each card has some height to read.`,
    createdAt: new Date(base + index * HOUR).toISOString(),
  }));
}

interface Seeded {
  repo: ForgeRepo;
  /** When the PR was opened and last updated. */
  createdAt: number;
  updatedAt: number;
}

async function seedPullRequest(harness: HarnessApp, forge: Forge, number = NUMBER[forge]): Promise<Seeded> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: `live-${forge}-${randomBytes(4).toString('hex')}`, repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] }, threads: [] }],
  });
  const now = Date.now();
  const createdAt = now - 3 * DAY - HOUR;
  const updatedAt = now - 2 * HOUR - 60_000;
  const repo = await publishPullRequest(harness, seed.projects[0].path, {
    forge,
    project: `ao-e2e/live-${randomBytes(4).toString('hex')}`,
    pulls: [{
      number,
      title: 'Retry a failed delivery',
      author: 'alice',
      authorName: 'Alice Ng',
      createdAt: new Date(createdAt).toISOString(),
      updatedAt: new Date(updatedAt).toISOString(),
      comments: notes(createdAt),
      threads: [
        { id: 'T_open', path: 'src/deliver.ts', line: 1, comments: [{ id: 5_201, author: 'bob', body: 'Should this retry twice?', createdAt: new Date(createdAt + 20 * HOUR).toISOString() }] },
        { id: 'T_done', path: 'src/deliver.ts', line: 2, resolved: true, comments: [{ id: 5_202, author: 'carol', body: 'Settled: the name reads well.', createdAt: new Date(createdAt + 21 * HOUR).toISOString() }] },
      ],
    }],
  }, { 'src/deliver.ts': 'export async function deliver(): Promise<void> {\n  await post();\n}\n' });
  return { repo, createdAt, updatedAt };
}

// The repository with one more PR comment, posted now.
let nextCommentId = 5_300;
function withComment(repo: ForgeRepo, body: string): ForgeRepo {
  const [pull, ...rest] = repo.pulls ?? [];
  return { ...repo, pulls: [{ ...pull, comments: [...(pull.comments ?? []), { id: nextCommentId++, author: 'dave', authorName: 'Dave Ko', body, createdAt: new Date().toISOString() }] }, ...rest] };
}

function feedScroller(review: Locator): Locator {
  return review.getByTestId('review-conversation').locator('xpath=..');
}

async function scrollFeedToEnd(review: Locator): Promise<void> {
  const scroller = feedScroller(review);
  await expect.poll(() => scroller.evaluate((el) => el.scrollHeight > el.clientHeight + 100)).toBe(true);
  await scroller.evaluate((el) => { el.scrollTop = el.scrollHeight; });
  await expect.poll(() => scroller.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);
}

// The card at the top of the section's viewport and where it sits.
async function topCard(review: Locator): Promise<{ id: string; offset: number }> {
  return feedScroller(review).evaluate((scroller) => {
    const top = scroller.getBoundingClientRect().top;
    const card = [...scroller.querySelectorAll<HTMLElement>('[data-feed-id]')].find((el) => el.getBoundingClientRect().bottom > top)!;
    return { id: card.dataset.feedId!, offset: card.getBoundingClientRect().top - top };
  });
}

async function offsetOf(review: Locator, id: string): Promise<number> {
  return feedScroller(review).evaluate((scroller, feedId) => {
    const card = scroller.querySelector<HTMLElement>(`[data-feed-id="${feedId}"]`)!;
    return card.getBoundingClientRect().top - scroller.getBoundingClientRect().top;
  }, id);
}

async function useDarkTheme(page: Page): Promise<void> {
  await page.emulateMedia({ colorScheme: 'dark' });
  await expect(page.locator('html')).toHaveClass(/\bdark\b/);
}

async function openConversation(review: Locator, press: (target: Locator) => Promise<void>): Promise<Locator> {
  const section = review.getByTestId('review-pr-conversation');
  const toggle = section.locator('button[aria-expanded]').first();
  if ((await toggle.getAttribute('aria-expanded')) !== 'true') await press(toggle);
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  await expect(section.getByTestId('review-conversation-thread').first()).toBeVisible();
  return section;
}

function card(review: Locator, text: string): Locator {
  return review.getByTestId('review-conversation').locator('[data-feed-id]').filter({ hasText: text });
}

export function conversationLiveFlow({ compact }: ConversationSurface): void {
  const press = (target: Locator) => (compact ? target.tap() : target.click());
  const layout = compact ? 'compact' : 'desktop';

  for (const forge of ['github', 'gitlab'] as const) {
    test(`${forge}: the list's age follows its sort, and the forge's new comment joins the feed with no press`, async ({ page }, testInfo: TestInfo) => {
      test.setTimeout(120_000);
      // The PR pump polls every second, so a comment the forge gains
      // arrives as a background update.
      const harness = await launchHarness({ env: { AO_HARNESS_TIMING: [...HARNESS_TIMING.split(','), 'pr-update=1s'].join(',') } });
      try {
        const seeded = await seedPullRequest(harness, forge);
        await harness.open(page);
        await useDarkTheme(page);

        // The list's age is the time it is sorted by; its hover names both.
        const pane = await openPage(page);
        await pickFilter(page, pane, 'all');
        const age = row(page, NUMBER[forge]).getByTestId('pull-age');
        await expect(age).toContainText('2h');
        await expect(age).toHaveAttribute('title', /^Opened .+ · Updated .+$/);
        await press(pane.getByTestId('pulls-sort'));
        await press(page.getByTestId('pulls-sort-created'));
        await expect(pane.getByTestId('pulls-sort')).toHaveAttribute('data-sort', 'created');
        await expect(age).toContainText('3d');
        await expect(age).toHaveAttribute('title', /^Opened .+ · Updated .+$/);
        await page.screenshot({ path: testInfo.outputPath(`list-sorted-created-${layout}.png`) });
        await press(pane.getByTestId('pulls-sort'));
        await press(page.getByTestId('pulls-sort-updated'));
        await expect(age).toContainText('2h');

        // The page is on screen: the row opens the review pinned to it.
        await press(row(page, NUMBER[forge]).getByRole('button').first());
        const review = page.getByTestId('review-pane');
        await expect(review.getByTestId('review-diff-stats')).toBeVisible({ timeout: 60_000 });
        await openConversation(review, press);

        // At the top of the feed: the comment simply appears, no chip.
        const first = withComment(seeded.repo, 'Arrived while you were reading.');
        await seedForge(harness, [first]);
        await expect(card(review, 'Arrived while you were reading.')).toHaveCount(1, { timeout: 20_000 });
        await expect(review.getByTestId('review-conversation-new')).toHaveCount(0);

        // Scrolled into the feed: the next one joins above, the card on
        // top holds still, and the chip points up at the arrival.
        await scrollFeedToEnd(review);
        const before = await topCard(review);

        await seedForge(harness, [withComment(first, 'Arrived above you.')]);
        const chip = review.getByTestId('review-conversation-new');
        await expect(chip).toHaveText(/1 new/, { timeout: 20_000 });
        await expect(chip.locator('.lucide-arrow-up')).toHaveCount(1);
        expect(Math.abs((await offsetOf(review, before.id)) - before.offset)).toBeLessThanOrEqual(1);
        await review.getByTestId('review-pr-conversation').screenshot({ path: testInfo.outputPath(`new-chip-${layout}.png`) });
        await press(chip);
        await expect(chip).toHaveCount(0);
        await expect(card(review, 'Arrived above you.')).toBeInViewport();

        await expectEveryForgeCallHandled(harness);
      } finally {
        await page.close();
        await harness.close();
      }
    });

    test(`${forge}: refresh shows the forge's new comment with no chip, and Hide resolved threads hides the resolved one`, async ({ harness, page }, testInfo: TestInfo) => {
      test.setTimeout(120_000);
      const seeded = await seedPullRequest(harness, forge);
      await harness.open(page);
      await useDarkTheme(page);
      const review = await openPinnedReview(page, NUMBER[forge], compact);
      const conversation = await openConversation(review, press);

      // Scrolled into the feed, so a poll's arrival would count as new;
      // the reader's refresh shows what it read and counts nothing.
      await scrollFeedToEnd(review);
      await seedForge(harness, [withComment(seeded.repo, 'Read by the refresh.')]);
      await press(conversation.getByTestId('review-refresh-comments'));
      await expect(card(review, 'Read by the refresh.')).toHaveCount(1, { timeout: 20_000 });
      await expect(review.getByTestId('review-conversation-new')).toHaveCount(0);

      // Hide resolved threads: out of the feed and the diff, and said so.
      const resolvedCard = review.locator('[data-testid="review-conversation-thread"]').filter({ hasText: 'Settled: the name reads well.' });
      const resolvedRow = review.locator('[data-testid="review-pr-thread"][data-state="resolved"]');
      await expect(resolvedCard).toHaveCount(1);
      await expect(resolvedRow).toHaveCount(1);
      await press(review.getByTestId('review-view-options'));
      const menu = page.locator('[data-popover]').last();
      const toggle = menu.getByTestId('review-hide-resolved-toggle');
      await expect(toggle).toHaveAttribute('aria-checked', 'false');
      await page.screenshot({ path: testInfo.outputPath(`view-options-${layout}.png`) });
      await press(toggle);
      await expect(resolvedCard).toHaveCount(0);
      await expect(resolvedRow).toHaveCount(0);
      await expect(review.locator('[data-testid="review-pr-thread"]')).toHaveCount(1);
      const hidden = review.getByTestId('review-conversation-hidden-resolved');
      await expect(hidden).toHaveText('1 resolved hidden');
      await hidden.scrollIntoViewIfNeeded();
      await conversation.screenshot({ path: testInfo.outputPath(`resolved-hidden-${layout}.png`) });
      await press(review.getByTestId('review-view-options'));
      await expect(page.locator('[data-popover]').last().getByTestId('review-hide-resolved-toggle')).toHaveAttribute('aria-checked', 'true');
      await page.keyboard.press('Escape');

      // The choice is remembered: another PR opened now starts with its
      // resolved thread hidden.
      await seedPullRequest(harness, forge, OTHER[forge]);
      if (compact) await press(page.getByTestId('pulls-back'));
      await press(row(page, OTHER[forge]).getByRole('button').first());
      await expect(review.getByTestId('review-pr-header')).toContainText(`${OTHER[forge]}`);
      await openConversation(review, press);
      await expect(review.getByTestId('review-conversation-thread').filter({ hasText: 'Should this retry twice?' })).toHaveCount(1);
      await expect(resolvedCard).toHaveCount(0);
      await expect(resolvedRow).toHaveCount(0);
      await expect(hidden).toHaveText('1 resolved hidden');

      // The line is the way out there too.
      await press(hidden);
      await expect(resolvedCard).toHaveCount(1);
      await expect(resolvedRow).toHaveCount(1);
      await expect(hidden).toHaveCount(0);

      await expectEveryForgeCallHandled(harness);
    });
  }
}
