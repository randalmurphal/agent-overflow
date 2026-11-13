// The review pane's conflict view on a GitLab merge request whose target
// branch moved after the head was pushed, shared by the desktop and compact
// projects, through the shipped UI, the real backend and the fake forge. The fake records the base as GitLab does
// (start_sha is the merge base), so the merge is computed against the
// target branch's tip on origin, which the checkout has never fetched.
// Covered: View conflicts shows the conflicted file with its markers and
// both sides; with the view open, a target branch that moves again to a
// clean merge and a forge that reports it clean re-read the view, which
// then says there are no conflicts. The PR poll runs every 2s.

import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdtemp, writeFile } from 'node:fs/promises';
import path from 'node:path';

import { HARNESS_TIMING, launchHarness, type HarnessApp } from '../src/harness.js';
import type { Locator, Page } from '@playwright/test';

import { expect, test, type SeedResult } from './fixtures.js';
import {
  expectEveryForgeCallHandled,
  forgeOrigin,
  openPullRequestReview,
  publishPullRequest,
  seedForge,
  type ForgeRepo,
} from './forge-helpers.js';

const TITLE = 'Conflicts thread';

function gitIn(harness: HarnessApp) {
  const env = { ...process.env, HOME: path.join(harness.bootstrap.dataRoot, 'home'), GIT_CONFIG_NOSYSTEM: '1' };
  return (cwd: string, ...args: string[]) => execFileSync('git', args, { cwd, env, encoding: 'utf8' }).trim();
}

/** Commits `content` as shared.txt on origin's main from a scratch clone the
 * workspace never sees. */
export async function pushToOriginMain(harness: HarnessApp, repo: ForgeRepo, content: string): Promise<string> {
  const git = gitIn(harness);
  const bare = (await forgeOrigin(harness, repo.project)).path;
  const scratch = await mkdtemp(path.join(harness.bootstrap.dataRoot, 'pusher-'));
  git(scratch, 'clone', '--quiet', '--branch', 'main', bare, '.');
  await writeFile(path.join(scratch, 'shared.txt'), content);
  git(scratch, 'add', 'shared.txt');
  git(scratch, '-c', 'user.email=outside@example.com', '-c', 'user.name=Outside', 'commit', '--quiet', '-m', 'edit shared.txt on main');
  git(scratch, 'push', '--quiet', 'origin', 'main');
  return git(scratch, 'rev-parse', 'HEAD');
}

/** Opens the review companion on the thread's MR. Compact reaches the MR
 * through the header's menu, and the companion is the pane on screen. */
async function openReview(page: Page, compact: boolean): Promise<Locator> {
  if (!compact) return openPullRequestReview(page, TITLE);
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
  await expect(review.getByTestId('review-pr-header')).toBeVisible();
  return review;
}

export function reviewConflictsFlow({ compact }: { compact: boolean }): void {
  test('View conflicts merges with the target branch as it is now', async ({ page }) => {
    test.setTimeout(90_000);
    const harness = await launchHarness({ env: { AO_HARNESS_TIMING: [...HARNESS_TIMING.split(','), 'pr-update=2s'].join(',') } });
    try {
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [
          {
            name: `conflicts-${randomBytes(4).toString('hex')}`,
            repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n', 'shared.txt': 'original\n' } }] },
            threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
          },
        ],
      });
      const workspace = seed.projects[0].path;
      const repo = await publishPullRequest(harness, workspace, {
        forge: 'gitlab',
        project: `ao-e2e/conflicts-${randomBytes(4).toString('hex')}`,
        pulls: [{ number: 7, title: 'Edit the shared file', body: '', mergeable: 'conflicts' }],
      }, { 'shared.txt': 'from the feature\n' });
      const [pull] = repo.pulls ?? [];
      // The recorded base stays the merge base, as GitLab reports it.
      expect(pull.startSha).toBe(pull.baseSha);
      await pushToOriginMain(harness, repo, 'from main\n');

      await harness.open(page);
      const review = await openReview(page, compact);
      const pill = review.getByTestId('review-pr-readiness');
      await expect(pill).toContainText('Blocked', { timeout: 30_000 });
      const section = review.getByTestId('review-pr-merge');
      if ((await section.getAttribute('data-open')) !== 'true') {
        if (compact) await pill.tap();
        else await pill.click();
      }
      await expect(section).toHaveAttribute('data-open', 'true');
      const conflictsRow = section.getByTestId('review-merge-row-conflicts');
      if (compact) await conflictsRow.tap();
      else await conflictsRow.click();

      const paths = review.getByTestId('review-file-header-path');
      await expect(paths).toContainText(['shared.txt']);
      await expect(paths).toHaveCount(1);
      const body = review.getByTestId('review-line-block');
      await expect(body.first()).toContainText('<<<<<<<');
      await expect(body.first()).toContainText('from main');
      await expect(body.first()).toContainText('from the feature');
      await expect(body.first()).toContainText('>>>>>>>');
      await expect(review.getByTestId('review-conflicts-empty')).toHaveCount(0);
      await expect(review.getByTestId('review-conflicts-error')).toHaveCount(0);

      // Main takes the feature's line, so the merge is clean; the forge's
      // report of it re-reads the open view.
      await pushToOriginMain(harness, repo, 'from the feature\n');
      await seedForge(harness, [{ ...repo, pulls: [{ ...pull, mergeable: 'clean' }] }]);
      await expect(review.getByTestId('review-conflicts-empty')).toHaveText('No conflicts against origin/main right now', { timeout: 20_000 });
      await expect(paths).toHaveCount(0);
      await expect(review.getByTestId('review-conflicts-error')).toHaveCount(0);
      await expectEveryForgeCallHandled(harness);
    } finally {
      await page.close();
      await harness.close();
    }
  });
}
