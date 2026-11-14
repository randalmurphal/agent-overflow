// A new comment's editor whose file changes on disk under it, through the
// shipped UI and the real backend, shared by the desktop and compact
// projects. The comment is typed on an added line of the workspace diff,
// the file is rewritten so that line moves, and the diff reloads:
// - the box stays on screen and focused, says "The file changed under this
//   comment.", and "Use this line" then Add comment saves on the row shown;
// - "Move" then a line picked elsewhere saves on that line (compact: Back
//   cancels the armed move first);
// - with the change reverted the box waits under "Comments on files not in
//   this diff", and Discard removes it.
// With AO_REVIEW_SHOTS set, dark screenshots of the three states land there.
import { randomBytes } from 'node:crypto';
import { writeFile } from 'node:fs/promises';
import path from 'node:path';
import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { openThread } from './worktree-removal-helpers.js';

export interface RelocationSurface {
  compact: boolean;
}

const TITLE = 'Relocation thread';
const FILE = 'src/app.ts';

function source(change: 'none' | 'edit' | 'edit-and-header'): string {
  const lines = Array.from({ length: 30 }, (_, i) => `export const v${i + 1} = ${i + 1};`);
  if (change !== 'none') lines.splice(9, 1, 'export const v10 = 100;', 'export const extra = 1;');
  if (change === 'edit-and-header') lines.unshift('// header');
  return `${lines.join('\n')}\n`;
}

async function seed(harness: HarnessApp): Promise<string> {
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: `relocation-${randomBytes(4).toString('hex')}`,
      repo: { commits: [{ message: 'init', files: { [FILE]: source('none') } }] },
      threads: [{ title: TITLE, turns: [{ userText: 'Change the values.', items: [{ kind: 'assistant_text', summary: 'Done.' }] }] }],
    }],
  });
  const root = seeded.projects[0].path;
  await writeFile(path.join(root, FILE), source('edit'));
  return root;
}

function press(compact: boolean, target: Locator): Promise<void> {
  return compact ? target.tap() : target.click();
}

async function openReview(page: Page, compact: boolean): Promise<Locator> {
  if (compact) await page.getByTestId('thread-row').filter({ hasText: TITLE }).tap();
  else await openThread(page, TITLE);
  const toggle = page.getByTestId('review-toggle');
  await expect(toggle).toContainText('+2', { timeout: 30_000 });
  await press(compact, toggle);
  const review = page.locator('section[data-pane-kind="review"]');
  await expect(review.getByTestId('review-line-block').first()).toBeVisible({ timeout: 30_000 });
  await expect(review).toBeInViewport({ ratio: 1 });
  return review;
}

function row(review: Locator, text: string): Locator {
  return review.getByTestId('review-line-block').locator(':scope > div').filter({ hasText: text });
}

// Opens a comment on the added line `export const v10 = 100;` and types.
async function typeComment(review: Locator, compact: boolean, body: string): Promise<Locator> {
  const line = row(review, 'export const v10 = 100;');
  if (!compact) await line.hover();
  await press(compact, line.getByTestId('review-add-comment'));
  const textarea = review.getByTestId('review-draft-editor').locator('textarea');
  await expect(textarea).toBeFocused();
  await textarea.fill(body);
  return textarea;
}

// Reloads the way a refresh the reader did not make does: no focus moves.
async function reloadUnfocused(review: Locator): Promise<void> {
  await review.getByTestId('review-reload').evaluate((button: HTMLButtonElement) => button.click());
}

async function shot(page: Page, name: string, compact: boolean): Promise<void> {
  const dir = process.env.AO_REVIEW_SHOTS;
  if (!dir) return;
  await page.screenshot({ path: path.join(dir, `${compact ? 'compact' : 'desktop'}-${name}.png`) });
}

export function reviewEditorRelocationFlow({ compact }: RelocationSurface): void {
  test('a comment whose line moved stays on screen and saves on the row the reader takes', async ({ harness, page }) => {
    const root = await seed(harness);
    await harness.open(page);
    const review = await openReview(page, compact);
    const textarea = await typeComment(review, compact, 'Check this value.');

    await writeFile(path.join(root, FILE), source('edit-and-header'));
    await reloadUnfocused(review);
    const note = review.getByTestId('review-draft-editor-note');
    await expect(note).toContainText('The file changed under this comment.');
    await expect(textarea).toBeFocused();
    await expect(textarea).toHaveValue('Check this value.');
    await expect(textarea).toBeInViewport();
    const target = review.locator('[data-review-target]');
    await expect(target).toHaveCount(1);
    await expect(target).toContainText('export const v10 = 100;');
    await expect(review.getByRole('button', { name: 'Add comment' })).toBeDisabled();
    await expect(review).not.toContainText(/unplaced|detached/i);
    await shot(page, 'detached', compact);

    await press(compact, review.getByRole('button', { name: 'Use this line' }));
    await expect(note).toHaveCount(0);
    await press(compact, review.getByRole('button', { name: 'Add comment' }));
    const saved = review.getByTestId('review-comment-thread');
    await expect(saved).toContainText('Check this value.');
    await expect(saved.locator(`[title="${FILE}:11"]`)).toHaveCount(1);
    await expect(review.getByTestId('review-draft-editor')).toHaveCount(0);
  });

  test('Move puts the comment on the line the reader picks', async ({ harness, page }) => {
    const root = await seed(harness);
    await harness.open(page);
    const review = await openReview(page, compact);
    await typeComment(review, compact, 'Belongs higher up.');

    await writeFile(path.join(root, FILE), source('edit-and-header'));
    await reloadUnfocused(review);
    const note = review.getByTestId('review-draft-editor-note');
    await expect(note).toContainText('The file changed under this comment.');
    await press(compact, review.getByRole('button', { name: 'Move' }));
    await expect(note).toContainText('Choose a line for this comment.');
    await shot(page, 'armed', compact);
    if (compact) {
      // Back cancels the armed move before anything else.
      await page.goBack();
      await expect(note).toContainText('The file changed under this comment.');
      await expect(review).toBeInViewport();
      await press(compact, review.getByRole('button', { name: 'Move' }));
      await expect(note).toContainText('Choose a line for this comment.');
    }

    const pick = row(review, 'export const v2 = 2;');
    if (!compact) await pick.hover();
    await press(compact, pick.getByTestId('review-add-comment'));
    await expect(note).toHaveCount(0);
    await expect(review.getByTestId('review-draft-editor')).toHaveCount(1);
    await expect(review.getByTestId('review-draft-editor').locator('textarea')).toHaveValue('Belongs higher up.');
    await press(compact, review.getByRole('button', { name: 'Add comment' }));
    const saved = review.getByTestId('review-comment-thread');
    await expect(saved).toContainText('Belongs higher up.');
    await expect(saved.locator(`[title="${FILE}:3"]`)).toHaveCount(1);
  });

  test('a comment whose file was reverted waits in the group until discarded', async ({ harness, page }) => {
    const root = await seed(harness);
    await harness.open(page);
    const review = await openReview(page, compact);
    await typeComment(review, compact, 'Was on a reverted file.');

    await writeFile(path.join(root, FILE), source('none'));
    await reloadUnfocused(review);
    const group = review.getByTestId('review-gone-editors');
    await expect(group).toHaveText('Comments on files not in this diff');
    await expect(review.getByTestId('review-draft-editor-path')).toHaveText(FILE);
    await expect(review.getByTestId('review-draft-editor-note')).toContainText('This file is no longer in the diff.');
    await expect(review.getByTestId('review-draft-editor').locator('textarea')).toHaveValue('Was on a reverted file.');
    await expect(review.getByRole('button', { name: 'Copy text' })).toBeVisible();
    await shot(page, 'group', compact);
    await press(compact, review.getByRole('button', { name: 'Discard' }));
    await expect(group).toHaveCount(0);
    await expect(review.getByTestId('review-draft-editor')).toHaveCount(0);
  });
}
