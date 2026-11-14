// Git actions menu on a thread's checkout, against real git in the seeded
// repository and a bare remote under the harness data root (no socket):
// - With a merge in progress whose conflict is resolved and staged, the
//   Commit dialog says "A merge is in progress. Committing completes the
//   merge.", committing records the merge commit (two parents, no
//   MERGE_HEAD), and a later commit's dialog no longer says so.
//   While the merge is pending every file row is selected and locked.
// - The Commit dialog lists the changed files with none selected, previews
//   the active file's diff, commits only the selected files (leaving
//   another file's staged change in the index), stays open on the rest,
//   and closes after the commit that leaves nothing.
// - Push is enabled for a branch with no upstream, pushes it to origin,
//   sets the upstream, and is disabled once nothing is ahead.
import { mkdir, rename, writeFile } from 'node:fs/promises';
import path from 'node:path';
import type { Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { errorToasts, harnessGit, openThread } from './worktree-removal-helpers.js';

const MERGE_NOTICE = 'A merge is in progress. Committing completes the merge.';

async function seedThread(harness: HarnessApp, name: string): Promise<{ root: string; title: string }> {
  const title = `${name} thread`;
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name,
      repo: { commits: [{ message: 'init', files: { 'notes.md': 'base\n' } }] },
      threads: [{ title, provider: 'claude', turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }] }],
    }],
  });
  return { root: seed.projects[0].path, title };
}

async function commitFile(harness: HarnessApp, root: string, file: string, content: string, message: string): Promise<void> {
  await writeFile(path.join(root, file), content);
  harnessGit(harness, root, 'add', file);
  harnessGit(harness, root, 'commit', '--quiet', '-m', message);
}

function gitMenuItem(page: Page, name: string) {
  return page.getByRole('menu', { name: 'Git actions' }).getByRole('menuitem', { name, exact: true });
}

async function openGitMenu(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'More git actions' }).click();
  await expect(page.getByRole('menu', { name: 'Git actions' })).toBeVisible();
}

/** Opens Commit from the menu once git status reports changes. */
async function openCommitDialog(page: Page) {
  await expect(async () => {
    await openGitMenu(page);
    const commit = gitMenuItem(page, 'Commit');
    try {
      await expect(commit).toBeEnabled({ timeout: 1_000 });
    } catch (err) {
      await page.keyboard.press('Escape');
      throw err;
    }
    await commit.click();
  }).toPass({ timeout: 30_000 });
  const dialog = page.getByRole('dialog', { name: 'Commit Changes' });
  await expect(dialog).toBeVisible();
  return dialog;
}

test('committing a resolved merge says it completes the merge and records the merge commit', async ({ harness, page }) => {
  const { root, title } = await seedThread(harness, 'git-merge-commit');
  harnessGit(harness, root, 'checkout', '--quiet', '-b', 'side');
  await commitFile(harness, root, 'notes.md', 'side\n', 'side change');
  harnessGit(harness, root, 'checkout', '--quiet', 'main');
  await commitFile(harness, root, 'notes.md', 'main\n', 'main change');
  const mainHead = harnessGit(harness, root, 'rev-parse', 'HEAD');
  const sideHead = harnessGit(harness, root, 'rev-parse', 'side');

  await harness.open(page);
  await openThread(page, title);

  // The merge stops on the conflict; resolve it and stage the result.
  expect(() => harnessGit(harness, root, 'merge', '--no-edit', 'side')).toThrow();
  await writeFile(path.join(root, 'notes.md'), 'main and side\n');
  harnessGit(harness, root, 'add', 'notes.md');
  expect(harnessGit(harness, root, 'rev-parse', 'MERGE_HEAD')).toBe(sideHead);

  const dialog = await openCommitDialog(page);
  await expect(dialog.getByTestId('commit-dialog-pending-operation')).toHaveText(MERGE_NOTICE);
  // Git records a merge whole, so the one row is selected and stays so.
  const mergedRow = dialog.getByRole('option', { name: 'notes.md', exact: true });
  await expect(mergedRow).toHaveAttribute('aria-selected', 'true');
  await mergedRow.getByTestId('commit-file-check').click();
  await expect(mergedRow).toHaveAttribute('aria-selected', 'true');
  await expect(dialog.getByTestId('commit-dialog-toggle-all')).toHaveCount(0);
  await dialog.getByLabel('Subject').fill('Merge side into main');
  await dialog.getByRole('button', { name: 'Commit', exact: true }).click();

  // The dialog closes once the commit returns; HEAD is read after that.
  await expect(dialog).toHaveCount(0);
  const head = harnessGit(harness, root, 'rev-parse', 'HEAD');
  await expect(page.getByRole('alert').filter({ hasText: `Committed ${head.slice(0, 7)}` })).toBeVisible();
  expect(head).not.toBe(mainHead);
  expect(harnessGit(harness, root, 'rev-list', '--parents', '-n', '1', 'HEAD').split(' ')).toEqual([head, mainHead, sideHead]);
  expect(harnessGit(harness, root, 'log', '-1', '--format=%s')).toBe('Merge side into main');
  expect(() => harnessGit(harness, root, 'rev-parse', '--verify', '--quiet', 'MERGE_HEAD')).toThrow();

  // With the merge finished, an ordinary commit carries no merge notice.
  await writeFile(path.join(root, 'after.md'), 'after the merge\n');
  harnessGit(harness, root, 'add', 'after.md');
  const next = await openCommitDialog(page);
  await expect(next.getByLabel('Subject')).toBeVisible();
  await expect(next.getByTestId('commit-dialog-pending-operation')).toHaveCount(0);
  await next.getByRole('button', { name: 'Cancel' }).click();
  await expect(errorToasts(page)).toHaveCount(0);
});

test('the Commit dialog commits the selected files and stays open until nothing is left', async ({ harness, page }) => {
  const { root, title } = await seedThread(harness, 'git-commit-selection');
  await commitFile(harness, root, 'staged.md', 'staged base\n', 'add staged');
  await commitFile(harness, root, 'old-name.md', 'one\ntwo\nthree\nfour\n', 'add old-name');

  await harness.open(page);
  await openThread(page, title);

  await writeFile(path.join(root, 'notes.md'), 'base\nedited in notes\n');
  await writeFile(path.join(root, 'fresh [1].md'), 'a brand new file\n');
  await rename(path.join(root, 'old-name.md'), path.join(root, 'new-name.md'));
  // Staged by hand and left out of the first commit.
  await writeFile(path.join(root, 'staged.md'), 'staged base\nstaged by hand\n');
  harnessGit(harness, root, 'add', 'staged.md');
  const stagedEntry = harnessGit(harness, root, 'ls-files', '--stage', '--', 'staged.md');
  const startHead = harnessGit(harness, root, 'rev-parse', 'HEAD');

  const dialog = await openCommitDialog(page);
  const rows = dialog.getByRole('option');
  const row = (name: string) => dialog.getByRole('option', { name, exact: true });
  await expect(rows).toHaveCount(4);
  await expect(dialog.getByTestId('commit-dialog-selection')).toHaveText('0 of 4 files selected');
  await expect(dialog.getByRole('button', { name: 'Commit', exact: true })).toBeDisabled();
  // The list has focus, so its keys work without a click.
  await expect(dialog.getByRole('listbox', { name: 'Files to commit' })).toBeFocused();

  // The first row is active and previewed; the arrows move the preview.
  const preview = dialog.getByTestId('commit-file-preview');
  await expect(row('fresh [1].md')).toHaveAttribute('data-active', '');
  await expect(preview).toContainText('+a brand new file');
  await page.keyboard.press('ArrowDown');
  await expect(row('new-name.md')).toHaveAttribute('data-active', '');
  await expect(preview).toContainText('rename from old-name.md');
  await page.keyboard.press('ArrowDown');
  await expect(preview).toContainText('+edited in notes');
  await expect(preview).not.toContainText('rename from');

  // Select with space, the checkbox, and a rename row.
  await page.keyboard.press('Space');
  await expect(row('notes.md')).toHaveAttribute('aria-selected', 'true');
  await row('fresh [1].md').getByTestId('commit-file-check').click();
  await row('new-name.md').getByTestId('commit-file-check').click();
  await expect(dialog.getByTestId('commit-dialog-selection')).toHaveText('3 of 4 files selected');

  await dialog.getByLabel('Subject').fill('Commit three of four');
  await dialog.getByLabel('Subject').press('Control+Enter');

  // The dialog stays open on the file that was left out.
  await expect(rows).toHaveCount(1);
  await expect(row('staged.md')).toHaveAttribute('aria-selected', 'false');
  await expect(dialog.getByLabel('Subject')).toHaveValue('');
  const firstHead = harnessGit(harness, root, 'rev-parse', 'HEAD');
  expect(harnessGit(harness, root, 'rev-parse', 'HEAD~1')).toBe(startHead);
  expect(harnessGit(harness, root, 'log', '-1', '--format=%s')).toBe('Commit three of four');
  expect(harnessGit(harness, root, 'show', '--format=', '--name-status', '-M', 'HEAD').split('\n')).toEqual([
    'A\tfresh [1].md',
    'R100\told-name.md\tnew-name.md',
    'M\tnotes.md',
  ]);
  expect(harnessGit(harness, root, 'ls-files', '--stage', '--', 'staged.md')).toBe(stagedEntry);
  expect(harnessGit(harness, root, 'status', '--porcelain')).toBe('M  staged.md');

  // A click on a row leaves the keys with the list. `a` selects what is
  // left; committing it closes the dialog.
  await row('staged.md').click();
  await expect(dialog.getByRole('listbox', { name: 'Files to commit' })).toBeFocused();
  await page.keyboard.press('a');
  await expect(dialog.getByTestId('commit-dialog-selection')).toHaveText('1 of 1 file selected');
  await dialog.getByLabel('Subject').fill('Commit the rest');
  await dialog.getByRole('button', { name: 'Commit', exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(harnessGit(harness, root, 'rev-parse', 'HEAD~1')).toBe(firstHead);
  expect(harnessGit(harness, root, 'status', '--porcelain')).toBe('');
  await expect(errorToasts(page)).toHaveCount(0);
});

test('Push on a branch with no upstream publishes it to origin and sets the upstream', async ({ harness, page }) => {
  const { root, title } = await seedThread(harness, 'git-push-upstream');
  const bare = path.join(harness.bootstrap.dataRoot, 'push-remote', 'git-push-upstream.git');
  await mkdir(bare, { recursive: true });
  harnessGit(harness, bare, 'init', '--bare', '--quiet');
  harnessGit(harness, root, 'remote', 'add', 'origin', bare);
  harnessGit(harness, root, 'push', '--quiet', '--set-upstream', 'origin', 'main');
  harnessGit(harness, root, 'checkout', '--quiet', '-b', 'topic');
  await commitFile(harness, root, 'topic.md', 'topic\n', 'topic work');
  const topicHead = harnessGit(harness, root, 'rev-parse', 'HEAD');
  expect(() => harnessGit(harness, root, 'rev-parse', '--abbrev-ref', '@{u}')).toThrow();

  await harness.open(page);
  await openThread(page, title);

  // Nothing is ahead of a missing upstream, so Push is enabled only
  // because the branch has none.
  await expect(async () => {
    await openGitMenu(page);
    try {
      await expect(gitMenuItem(page, 'Push')).toBeEnabled({ timeout: 1_000 });
    } catch (err) {
      await page.keyboard.press('Escape');
      throw err;
    }
  }).toPass({ timeout: 30_000 });
  await gitMenuItem(page, 'Push').click();

  await expect(page.getByRole('alert').filter({ hasText: 'Pushed successfully' })).toBeVisible();
  expect(harnessGit(harness, root, 'rev-parse', '--abbrev-ref', '@{u}')).toBe('origin/topic');
  expect(harnessGit(harness, bare, 'rev-parse', 'refs/heads/topic')).toBe(topicHead);

  // Tracking and even with origin, there is nothing left to push.
  await expect(async () => {
    await openGitMenu(page);
    try {
      await expect(gitMenuItem(page, 'Push')).toBeDisabled({ timeout: 1_000 });
    } finally {
      await page.keyboard.press('Escape');
    }
  }).toPass({ timeout: 30_000 });
  await expect(errorToasts(page)).toHaveCount(0);
});
