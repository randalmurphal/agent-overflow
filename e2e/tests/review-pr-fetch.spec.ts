// How a pull request review pane gets the PR's commits, through the shipped
// UI, the real backend and the fake forge, with the workspace's origin made
// unreachable. Covered: a PR whose commits the checkout already has loads
// with no origin at all; a PR head the checkout lacks fails with git's own
// diagnosis and a Retry, not "No changed files", and loads on its own once
// origin answers and the PR poll runs again; a refresh that fails while a
// diff is on screen keeps that diff under the error, and Retry replaces it.
// A GitLab merge request whose diff GitLab is still preparing (diff_refs
// null) waits without an error and loads once GitLab reports it, on the PR
// poll's retry schedule rather than its interval. The PR poll runs every 2s
// in the outage tests so the recovery frame comes quickly.

import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdtemp, writeFile } from 'node:fs/promises';
import path from 'node:path';

import { HARNESS_TIMING, launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  expectEveryForgeCallHandled,
  openPullRequestReview,
  publishPullRequest,
  seedForge,
  type ForgeRepo,
} from './forge-helpers.js';

const TITLE = 'Fetch thread';

function gitIn(harness: HarnessApp) {
  const env = { ...process.env, HOME: path.join(harness.bootstrap.dataRoot, 'home'), GIT_CONFIG_NOSYSTEM: '1' };
  return (cwd: string, ...args: string[]) => execFileSync('git', args, { cwd, env, encoding: 'utf8' }).trim();
}

async function seedPullRequest(harness: HarnessApp): Promise<{ workspace: string; repo: ForgeRepo }> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `fetch-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  const workspace = seed.projects[0].path;
  const repo = await publishPullRequest(harness, workspace, {
    forge: 'github',
    project: `ao-e2e/fetch-${randomBytes(4).toString('hex')}`,
    pulls: [{ number: 21, title: 'Fetch what is missing', body: '' }],
  }, { 'src/first.ts': 'export const first = 1;\n' });
  return { workspace, repo };
}

/** Points the workspace's origin connection at a proxy that refuses every
 * connection, and returns the restore. git exits 128 the way it does when
 * the network or the remote is down. */
async function breakOrigin(harness: HarnessApp, workspace: string): Promise<() => void> {
  const git = gitIn(harness);
  const working = git(workspace, 'config', 'core.gitProxy');
  const dir = await mkdtemp(path.join(harness.bootstrap.dataRoot, 'broken-proxy-'));
  const broken = path.join(dir, 'refuse.sh');
  await writeFile(broken, '#!/bin/sh\necho "proxy: connection refused" >&2\nexit 1\n', { mode: 0o755 });
  git(workspace, 'config', 'core.gitProxy', broken);
  return () => git(workspace, 'config', 'core.gitProxy', working);
}

/** Pushes a commit the workspace has never seen to the PR's head on origin
 * and re-seeds the forge so the PR reports it. */
async function movePullHead(harness: HarnessApp, repo: ForgeRepo, file: string): Promise<void> {
  const git = gitIn(harness);
  const bare = path.join(harness.bootstrap.dataRoot, 'forge-origin', `${repo.project}.git`);
  const scratch = await mkdtemp(path.join(harness.bootstrap.dataRoot, 'pusher-'));
  git(scratch, 'clone', '--quiet', '--branch', 'feature', bare, '.');
  await writeFile(path.join(scratch, file), `export const moved = '${file}';\n`);
  git(scratch, 'add', file);
  git(scratch, '-c', 'user.email=outside@example.com', '-c', 'user.name=Outside', 'commit', '--quiet', '-m', `add ${file}`);
  git(scratch, 'push', '--quiet', 'origin', 'feature');
  const head = git(scratch, 'rev-parse', 'HEAD');
  git(bare, 'update-ref', 'refs/pull/21/head', head);
  const [pull, ...rest] = repo.pulls ?? [];
  await seedForge(harness, [{ ...repo, pulls: [{ ...pull, headSha: head }, ...rest] }]);
}

async function launch(): Promise<HarnessApp> {
  return launchHarness({ env: { AO_HARNESS_TIMING: [...HARNESS_TIMING.split(','), 'pr-update=2s'].join(',') } });
}

test('a PR whose commits the checkout has loads with origin unreachable', async ({ page }) => {
  const harness = await launch();
  try {
    const { workspace } = await seedPullRequest(harness);
    await breakOrigin(harness, workspace);
    await harness.open(page);
    const review = await openPullRequestReview(page, TITLE);
    await expect(review.getByTestId('review-file-header-path')).toContainText(['src/first.ts']);
    await expect(review.getByTestId('review-file-header-path')).toHaveCount(1);
    await expect(review.getByTestId('review-error')).toHaveCount(0);
    await expectEveryForgeCallHandled(harness);
  } finally {
    await page.close();
    await harness.close();
  }
});

test('a PR head the checkout lacks fails with git\'s reason and loads once origin answers', async ({ page }) => {
  test.setTimeout(90_000);
  const harness = await launch();
  try {
    const { workspace, repo } = await seedPullRequest(harness);
    await movePullHead(harness, repo, 'src/second.ts');
    const restore = await breakOrigin(harness, workspace);
    await harness.open(page);
    const review = await openPullRequestReview(page, TITLE);

    const message = review.getByTestId('review-error-message');
    await expect(message).toContainText('fetch pull request commits');
    await expect(message).toContainText('exited with code 128');
    await expect(message).toContainText('fatal:');
    await expect(review.getByTestId('review-error-retry')).toBeVisible();
    await expect(review.getByTestId('review-empty')).toHaveCount(0);

    // Nothing to do by hand: the next PR poll after origin is back retries.
    restore();
    await expect(review.getByTestId('review-file-header-path')).toContainText(['src/first.ts', 'src/second.ts'], { timeout: 20_000 });
    await expect(review.getByTestId('review-error')).toHaveCount(0);
    await expectEveryForgeCallHandled(harness);
  } finally {
    await page.close();
    await harness.close();
  }
});

test('a failed refresh keeps the diff on screen under the error, and Retry replaces it', async ({ page }) => {
  test.setTimeout(90_000);
  const harness = await launch();
  try {
    const { workspace, repo } = await seedPullRequest(harness);
    await harness.open(page);
    const review = await openPullRequestReview(page, TITLE);
    const paths = review.getByTestId('review-file-header-path');
    await expect(paths).toContainText(['src/first.ts']);
    await expect(paths).toHaveCount(1);

    const restore = await breakOrigin(harness, workspace);
    await movePullHead(harness, repo, 'src/second.ts');
    const stale = review.getByTestId('review-pr-stale');
    await expect(stale).toBeVisible({ timeout: 20_000 });
    await stale.getByRole('button', { name: 'Reload' }).click();

    await expect(review.getByTestId('review-error-message')).toContainText('exited with code 128');
    await expect(paths).toContainText(['src/first.ts']);
    await expect(paths).toHaveCount(1);
    await expect(review.getByTestId('review-empty')).toHaveCount(0);

    restore();
    await review.getByTestId('review-error-retry').click();
    await expect(paths).toContainText(['src/first.ts', 'src/second.ts']);
    await expect(review.getByTestId('review-error')).toHaveCount(0);
    await expect(stale).toHaveCount(0);
    await expectEveryForgeCallHandled(harness);
  } finally {
    await page.close();
    await harness.close();
  }
});

test('a GitLab MR whose diff is still being prepared loads once GitLab reports it', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `prepare-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  const repo = await publishPullRequest(harness, seed.projects[0].path, {
    forge: 'gitlab',
    project: `ao-e2e/prepare-${randomBytes(4).toString('hex')}`,
    pulls: [{ number: 4, title: 'Just opened', body: '', diffPending: true }],
  }, { 'src/first.ts': 'export const first = 1;\n' });
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);
  const paths = review.getByTestId('review-file-header-path');

  // Pending, and nothing wrong.
  await expect(review.getByTestId('review-pr-header')).toContainText('Just opened');
  await expect(review.getByTestId('review-awaiting-pr')).toHaveText('Waiting for the merge request…');
  await expect(paths).toHaveCount(0);
  await expect(review.getByTestId('review-error')).toHaveCount(0);
  await expect(review.getByTestId('review-empty')).toHaveCount(0);

  // The PR poll interval is 45s here; the retry schedule brings it sooner.
  const [pull] = repo.pulls ?? [];
  await seedForge(harness, [{ ...repo, pulls: [{ ...pull, diffPending: false }] }]);
  await expect(paths).toContainText(['src/first.ts'], { timeout: 15_000 });
  await expect(review.getByTestId('review-awaiting-pr')).toHaveCount(0);
  await expect(review.getByTestId('review-error')).toHaveCount(0);
  await expectEveryForgeCallHandled(harness);
});
