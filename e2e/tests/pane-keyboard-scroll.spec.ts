// Keyboard scrolling in a pane after the pointer or the keyboard puts focus
// there. The browser starts a keyboard scroll (PageDown, ArrowDown, Space,
// End) at the focused element and walks up, so wherever a click or a pane
// move leaves focus decides what those keys scroll. Covered: a click on a
// thread's message text, then on a review diff's text, scrolls the
// timeline or the diff with each key; a click on a pane's text outside its
// scroller focuses nothing; a keyboard pane move (Alt+L) from a composer
// onto the review pane, which has no composer, puts the caret in the diff,
// so those keys scroll it and typing never reaches the composer left; a
// click on the timeline then type-to-focus puts the caret in the composer,
// where Escape keeps it and PageDown is the text field's.

import { randomBytes } from 'node:crypto';
import { writeFile } from 'node:fs/promises';
import path from 'node:path';
import type { Locator, Page } from '@playwright/test';

import { expect, test, type SeedResult } from './fixtures.js';

const KEYS = ['PageDown', 'ArrowDown', 'Space', 'End'] as const;

const scrollTop = (scroller: Locator) => scroller.evaluate((el) => el.scrollTop);

/** Wheels `scroller` back to its top, as a reader would. */
async function wheelToTop(page: Page, scroller: Locator): Promise<void> {
  await scroller.hover();
  await expect.poll(async () => {
    await page.mouse.wheel(0, -5000);
    return scrollTop(scroller);
  }).toBe(0);
}

/** The scroll position once a smooth keyboard scroll has come to rest. */
async function settledScrollTop(scroller: Locator): Promise<number> {
  let last = -1;
  await expect.poll(async () => {
    const now = await scrollTop(scroller);
    const settled = now === last;
    last = now;
    return settled;
  }, { intervals: [150] }).toBe(true);
  return last;
}

/** Each key scrolls `scroller` further down. */
async function expectKeysScroll(page: Page, scroller: Locator): Promise<void> {
  for (const key of KEYS) {
    const before = await settledScrollTop(scroller);
    await page.keyboard.press(key);
    await expect.poll(() => scrollTop(scroller), { message: `${key} scrolls` }).toBeGreaterThan(before);
  }
}

async function seedThreadWithDiff(harness: { rpc<T>(method: string, ...args: unknown[]): Promise<T> }, title: string): Promise<void> {
  const turns = Array.from({ length: 40 }, (_, index) => ({
    userText: `Question ${index}`,
    items: [{ kind: 'assistant_text', summary: `Answer ${index}. ${'A long line of reply text that wraps across the pane. '.repeat(6)}` }],
  }));
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: `pane-keyboard-scroll-${randomBytes(4).toString('hex')}`,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
      threads: [{ title, provider: 'claude', turns }],
    }],
  });
  const lines = Array.from({ length: 400 }, (_, index) => `diff line ${String(index).padStart(4, '0')}`);
  await writeFile(path.join(seeded.projects[0].path, 'notes.txt'), `${lines.join('\n')}\n`);
}

test('keys scroll the timeline and the review diff after a click on their text, and the diff after a pane move onto it', async ({ harness, page }) => {
  const title = 'Keyboard scroll thread';
  await seedThreadWithDiff(harness, title);
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: title }).click();
  const thread = page.locator('section[data-pane-kind="thread"]');
  const timeline = thread.getByTestId('message-timeline-scroll');
  await expect(thread.getByText('Answer 39.')).toBeVisible();

  // A click on message text, then each key scrolls the timeline.
  await wheelToTop(page, timeline);
  await thread.getByText('Answer 1.').click();
  await expectKeysScroll(page, timeline);

  // The same on the review pane's diff.
  await page.getByTestId('review-toggle').click();
  const review = page.locator('section[data-pane-kind="review"]');
  const diff = review.getByTestId('review-scroll');
  await expect(review.getByText('diff line 0002')).toBeVisible({ timeout: 30_000 });
  await review.getByText('diff line 0002').click();
  await expectKeysScroll(page, diff);

  // A click on the pane's text outside its scroller focuses nothing, as
  // with no pane section in the way.
  await review.getByTestId('review-diff-stats').click();
  expect(await page.evaluate(() => document.activeElement === document.body)).toBe(true);

  // A keyboard move from the composer onto the review pane: the keys scroll
  // the diff, and typing reaches no composer.
  await wheelToTop(page, diff);
  const composer = thread.getByLabel('Message Input');
  await composer.click();
  await composer.fill('kept draft');
  await page.keyboard.press('Alt+l');
  await expect(review).toHaveAttribute('data-pane-focused', 'true');
  await expectKeysScroll(page, diff);
  await page.keyboard.type('x');
  await expect(composer).toHaveValue('kept draft');
});

test('type-to-focus after a click on the timeline puts the caret in the composer, and Escape keeps it there', async ({ harness, page }) => {
  const title = 'Type to focus thread';
  await seedThreadWithDiff(harness, title);
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: title }).click();
  const thread = page.locator('section[data-pane-kind="thread"]');
  const timeline = thread.getByTestId('message-timeline-scroll');
  await expect(thread.getByText('Answer 39.')).toBeVisible();
  await wheelToTop(page, timeline);
  await thread.getByText('Answer 1.').click();
  await expect(timeline).toBeFocused();

  const composer = thread.getByLabel('Message Input');
  await page.keyboard.type('h');
  await expect(composer).toBeFocused();
  await expect(composer).toHaveValue('h');
  await page.keyboard.press('Escape');
  await expect(composer).toBeFocused();
  // PageDown in the composer is the text field's, not the timeline's.
  const before = await scrollTop(timeline);
  await page.keyboard.press('PageDown');
  await expect(composer).toBeFocused();
  expect(await scrollTop(timeline)).toBe(before);
});
