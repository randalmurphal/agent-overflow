// Git actions menu on a thread's checkout, against real git in the seeded
// repository and a bare remote under the harness data root (no socket):
// - With a merge in progress whose conflict is resolved and staged, the
//   Commit dialog says "A merge is in progress. Committing completes the
//   merge.", committing records the merge commit (two parents, no
//   MERGE_HEAD), and a later commit's dialog no longer says so.
// - Push is enabled for a branch with no upstream, pushes it to origin,
//   sets the upstream, and is disabled once nothing is ahead.
import { mkdir, writeFile } from 'node:fs/promises';
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
  await dialog.getByLabel('Subject').fill('Merge side into main');
  await dialog.getByRole('button', { name: 'Commit', exact: true }).click();

  const head = harnessGit(harness, root, 'rev-parse', 'HEAD');
  await expect(page.getByRole('alert').filter({ hasText: `Committed ${head.slice(0, 7)}` })).toBeVisible();
  await expect(dialog).toHaveCount(0);
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
