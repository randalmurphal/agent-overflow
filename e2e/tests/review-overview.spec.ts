// The pull request overview in the review pane, through the shipped UI,
// the real backend and the fake forge CLI. The PR carries a description,
// a PR-level comment, review threads in every state (a bot finding with a
// folded prompt, a resolved exchange, an outdated note, an open nit on a
// second file) and two verdicts. Covered: the overview is the diff list's
// first row with both sections folded; every thread is a card that names
// its author and state and jumps to its diff row;
// scrolling into the diff replaces the overview with peek buttons in the
// title bar that bring it back; the title bar's unresolved stepper lands
// on the thread's diff row, expanded; the row on the diff reads the same
// state as its card.

import { randomBytes } from 'node:crypto';
import type { Locator } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  expectEveryForgeCallHandled,
  openPullRequestReview,
  publishPullRequest,
  type ForgeRepo,
} from './forge-helpers.js';

const TITLE = 'Overview thread';
// Enough rows that the overview row is unmounted, not just off screen,
// once the reader is at the end of the file, and under the 400-row
// default-collapse rule (stores/reviewPaneLoad.ts collapsedByDefault).
const PARSER_LINES = 380;

const BOT = 'coderabbitai[bot]';

function parserSource(): string {
  const lines: string[] = [];
  for (let index = 1; index <= PARSER_LINES; index += 1) {
    lines.push(`export const token${index} = parse(input, ${index});`);
  }
  return `${lines.join('\n')}\n`;
}

const DESCRIPTION = [
  '## Summary',
  '',
  'Guards the parser against a null token and renames the lookahead.',
  '',
  '- `parse` returns early on an empty input',
  '- `peek` is now `lookahead`',
  '',
  '## Test plan',
  '',
  '```sh',
  'pnpm test parser',
  '```',
  '',
].join('\n');

const BOT_FINDING = [
  '_⚠️ Potential issue_ | _🔴 Critical_',
  '',
  '**Guard against a null token before dereferencing.**',
  '',
  '`parse` reads `token.kind` on line 12 while `token` can be `null` for an empty input.',
  '',
  '```suggestion',
  'if (token === null) return EMPTY;',
  '```',
  '',
  '<details>',
  '<summary>🤖 Prompt for AI Agents</summary>',
  '',
  '```',
  'In src/parser.ts around line 12, return EMPTY when token is null.',
  '```',
  '',
  '</details>',
  '',
].join('\n');

function repo(): ForgeRepo {
  return {
    forge: 'github',
    project: `ao-e2e/overview-${randomBytes(4).toString('hex')}`,
    pulls: [
      {
        number: 12,
        title: 'Guard the parser',
        body: DESCRIPTION,
        author: 'octocat',
        authorName: 'Octo Cat',
        comments: [
          { author: 'bob', authorName: 'Bob Stone', body: 'Looks good overall, one nit inline.', createdAt: '2026-03-01T10:00:00Z' },
        ],
        threads: [
          {
            id: 'T_bot',
            path: 'src/parser.ts',
            line: 12,
            comments: [{ author: BOT, body: BOT_FINDING, createdAt: '2026-03-02T09:00:00Z' }],
          },
          {
            id: 'T_resolved',
            path: 'src/parser.ts',
            line: 30,
            resolved: true,
            comments: [
              { author: 'alice', authorName: 'Alice Doe', body: 'Can we name this `lookahead`?', createdAt: '2026-03-01T11:00:00Z' },
              { author: 'octocat', authorName: 'Octo Cat', body: 'Sure, renamed in the next push.', createdAt: '2026-03-01T11:30:00Z' },
            ],
          },
          {
            id: 'T_outdated',
            path: 'src/parser.ts',
            line: 5,
            outdated: true,
            comments: [{ author: 'bob', authorName: 'Bob Stone', body: 'This loop ran twice before.', createdAt: '2026-02-28T08:00:00Z' }],
          },
          {
            id: 'T_nit',
            path: 'docs/notes.md',
            line: 2,
            comments: [{ author: 'alice', authorName: 'Alice Doe', body: 'Typo: recieve → receive.', createdAt: '2026-03-01T12:00:00Z' }],
          },
        ],
        reviews: [
          { author: 'alice', state: 'CHANGES_REQUESTED', body: 'Two threads to settle first.', submittedAt: '2026-03-01T12:30:00Z' },
          { author: 'bob', state: 'APPROVED', submittedAt: '2026-03-01T13:00:00Z' },
        ],
      },
    ],
  };
}

async function seedOverviewPullRequest(harness: HarnessApp): Promise<void> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `overview-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  await publishPullRequest(harness, seed.projects[0].path, repo(), {
    'src/parser.ts': parserSource(),
    'docs/notes.md': '# Notes\n\nWe recieve tokens one at a time.\n',
  });
}

function card(review: Locator, threadId: string): Locator {
  return review.locator(`[data-testid="review-conversation-thread"][data-thread-id="${threadId}"]`);
}

/** Scroll the diff list until the overview row is above the viewport. */
async function scrollPastOverview(review: Locator): Promise<void> {
  const scroller = review.getByTestId('review-scroll');
  await expect
    .poll(async () => {
      await scroller.evaluate((el) => { el.scrollTop += 600; });
      return review.getByTestId('review-overview-peek').isVisible();
    })
    .toBe(true);
}

test('the overview heads the diff, its cards read every thread state, and the title bar brings it back', async ({
  harness,
  page,
}, testInfo) => {
  await seedOverviewPullRequest(harness);
  // The appearance mode follows the system by default; the dark theme is
  // where the state edges and tints have to read.
  await page.emulateMedia({ colorScheme: 'dark' });
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);
  await expect(review.getByTestId('review-file-header-path').first()).toBeVisible();

  // The overview is the first row, both sections folded; the diff shows
  // under it, and under the conversation once that is open.
  const overview = review.getByTestId('review-overview');
  await expect(overview).toBeVisible();
  await expect(review.getByTestId('review-pr-description')).toHaveAttribute('data-open', 'false');
  await expect(review.getByTestId('review-pr-conversation')).toHaveAttribute('data-open', 'false');
  await expect(review.getByTestId('review-line-block').first()).toBeInViewport();
  await review.getByTestId('review-pr-conversation').locator('button[aria-expanded]').first().click();
  await expect(review.getByTestId('review-pr-conversation')).toHaveAttribute('data-open', 'true');
  await expect(review.getByTestId('review-file-header-path').first()).toBeInViewport();
  await expect(review.getByTestId('review-pr-author')).toContainText('Octo Cat');

  // Every thread is a card in its state; the verdicts sit among them.
  await expect(review.getByTestId('review-conversation-thread')).toHaveCount(5);
  await expect(review.getByTestId('review-conversation-verdict')).toHaveCount(2);
  await expect(card(review, 'T_bot')).toHaveAttribute('data-state', 'unresolved');
  await expect(card(review, 'T_bot')).toContainText('bot');
  await expect(card(review, 'T_bot')).toContainText('Guard against a null token');
  await expect(card(review, 'T_resolved')).toHaveAttribute('data-state', 'resolved');
  await expect(card(review, 'T_resolved')).toContainText('Alice Doe');
  await expect(card(review, 'T_outdated')).toHaveAttribute('data-state', 'outdated');
  await expect(card(review, 'T_nit')).toHaveAttribute('data-state', 'unresolved');
  await expect(review.getByTestId('review-conversation-open-count')).toContainText('2 unresolved');
  await page.screenshot({ path: testInfo.outputPath('overview-open.png') });
  // Newest first: the verdicts and the oldest (outdated) thread sit
  // further down the section's own scroller.
  await review.getByTestId('review-conversation').evaluate((el) => { el.parentElement!.scrollTop = el.parentElement!.scrollHeight; });
  await expect(card(review, 'T_outdated')).toBeInViewport();
  await page.screenshot({ path: testInfo.outputPath('overview-end.png') });
  await review.getByTestId('review-conversation').evaluate((el) => { el.parentElement!.scrollTop = 0; });

  // The code a thread was written against folds under its header.
  await card(review, 'T_bot').getByTestId('review-thread-context').locator('button[aria-expanded]').click();
  await expect(card(review, 'T_bot').getByTestId('review-thread-context')).toContainText('token12 = parse(input, 12)');
  await page.screenshot({ path: testInfo.outputPath('overview-context.png') });

  // The description opens as its own section above the conversation.
  await review.getByTestId('review-pr-description').locator('button[aria-expanded]').first().click();
  await expect(review.getByTestId('review-pr-description')).toHaveAttribute('data-open', 'true');
  await expect(review.getByTestId('review-pr-description')).toContainText('Guards the parser');
  await page.screenshot({ path: testInfo.outputPath('overview-description.png') });

  // A section's own scroll position is part of the reading state.
  await review.getByTestId('review-conversation').evaluate((el) => { el.parentElement!.scrollTop = 140; });

  // Scrolling into the diff takes the overview with it; the title bar
  // offers the way back.
  await expect(review.getByTestId('review-overview-peek')).toHaveCount(0);
  // The bar does not breathe when its peek controls appear.
  const barHeightBefore = (await review.getByTestId('review-pr-header').boundingBox())!.height;
  await scrollPastOverview(review);
  expect((await review.getByTestId('review-pr-header').boundingBox())!.height).toBe(barHeightBefore);
  await expect(review.getByTestId('review-peek-conversation')).toContainText('2 unresolved');
  await page.screenshot({ path: testInfo.outputPath('diff-scrolled.png') });

  // The stepper lands on the newest open thread's diff row, expanded.
  await review.getByTestId('review-unresolved-stepper-bar').getByRole('button', { name: 'Next unresolved thread' }).click();
  const botRow = review.locator('[data-testid="review-pr-thread"][data-state="unresolved"]').filter({ hasText: BOT });
  await expect(botRow).toBeVisible();
  await expect(botRow).toBeInViewport();
  await expect(botRow.locator('button[aria-expanded]').first()).toHaveAttribute('aria-expanded', 'true');
  await expect(botRow).toContainText('Guard against a null token');
  await page.screenshot({ path: testInfo.outputPath('diff-thread-row.png') });

  // The actions sit at the foot of the card as on the forge: the reply
  // field opens the composer in place, with Copy and Resolve beside it.
  await botRow.getByTestId('review-thread-reply').click();
  await expect(botRow.getByTestId('review-thread-composer')).toBeVisible();
  await expect(botRow.getByTestId('review-thread-resolve')).toHaveText('Resolve');
  await page.screenshot({ path: testInfo.outputPath('diff-thread-composer.png') });
  await page.keyboard.press('Escape');
  await expect(botRow.getByTestId('review-thread-composer')).toHaveCount(0);
  await expect(botRow.getByTestId('review-thread-reply')).toBeVisible();

  // The diff row's "Open in conversation" brings the overview back at
  // the thread's card.
  await botRow.getByTestId('review-pr-thread-jump-conversation').click();
  await expect(overview).toBeInViewport();
  await expect(card(review, 'T_bot')).toBeInViewport();
  await expect(review.getByTestId('review-overview-peek')).toHaveCount(0);

  // Deep in the file the overview row is unmounted; its sections' open
  // state and scroll offsets come back with it. The offset is wherever
  // the card scroll above left the section.
  const remembered = await review.getByTestId('review-conversation').evaluate((el) => el.parentElement!.scrollTop);
  expect(remembered).toBeGreaterThan(0);
  await review.getByTestId('review-scroll').evaluate((el) => { el.scrollTop = el.scrollHeight; });
  await expect(review.getByTestId('review-overview')).toHaveCount(0);
  await expect(review.getByTestId('review-overview-peek')).toBeVisible();
  await review.getByTestId('review-peek-conversation').click();
  await expect(overview).toBeInViewport();
  await expect(review.getByTestId('review-pr-description')).toHaveAttribute('data-open', 'true');
  await expect(review.getByTestId('review-pr-conversation')).toHaveAttribute('data-open', 'true');
  await expect.poll(() => review.getByTestId('review-conversation').evaluate((el) => el.parentElement!.scrollTop)).toBe(remembered);
  await review.getByTestId('review-conversation').evaluate((el) => { el.parentElement!.scrollTop = 0; });

  // A card's location jumps to the row on the diff.
  await card(review, 'T_nit').getByTestId('review-conversation-jump-diff').click();
  const nitRow = review.locator('[data-testid="review-pr-thread"]').filter({ hasText: 'recieve' });
  await expect(nitRow).toBeInViewport();
  await expect(nitRow).toHaveAttribute('data-state', 'unresolved');

  await expectEveryForgeCallHandled(harness);
});
