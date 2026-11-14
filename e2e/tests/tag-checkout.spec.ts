// Tags in the branch picker against real git, end to end: the picker lists
// tags after every branch and mixes them in while searching; picking a tag
// detaches the workspace at the tag's commit and the strip names the tag; a
// commit made there reads as "detached at <sha>"; "New branch…" from that
// detached HEAD keeps the commit; a tag staged for a new worktree lands the
// thread in a worktree detached at the tag; and the git menu's "Create
// branch from here" keeps a commit made there. The unit levels meet only at
// the `refs/tags/<name>` wire string; this proves both sides agree on it.
import type { Page } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { realpath } from 'node:fs/promises';
import { test, expect, type HarnessMockEvent, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';

const PROJECT = 'tag-checkout-app';
const BRANCH = 'v1-maintenance';
const OLD_TAG = 'v1.0';
const NEW_TAG = 'v1.1';

interface ThreadRow {
  id: string;
  workspacePath: string;
  worktreePath?: string;
  branch?: string;
}

interface WorktreeListItem {
  path: string;
  branch: string;
  tag?: string;
  head: string;
}

/** git in a fixture checkout, kept off the developer's global and system config. */
function gitIn(harness: HarnessApp) {
  const env = {
    ...process.env,
    HOME: path.join(harness.bootstrap.dataRoot, 'home'),
    GIT_CONFIG_NOSYSTEM: '1',
    GIT_CONFIG_GLOBAL: '/dev/null',
  };
  return (cwd: string, args: string[], extraEnv: Record<string, string> = {}) =>
    execFileSync('git', args, { cwd, env: { ...env, ...extraEnv }, encoding: 'utf8' }).trim();
}

const IDENTITY = ['-c', 'user.email=tags@example.com', '-c', 'user.name=Tags'];

/**
 * A repository with three commits on main, a branch at the tip, and two
 * annotated tags on the older commits, so the tip is a commit after the
 * newest tag. Tagger dates are pinned so the tag order is fixed.
 */
async function seedProject(harness: HarnessApp): Promise<{ projectId: string; root: string }> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: PROJECT,
        repo: {
          commits: [
            { message: 'first', files: { 'README.md': '# one\n' } },
            { message: 'second', files: { 'README.md': '# two\n' } },
            { message: 'third', files: { 'README.md': '# three\n' } },
          ],
          branches: [BRANCH],
        },
      },
    ],
  });
  const { projectId, path: root } = seed.projects[0];
  const git = gitIn(harness);
  git(root, [...IDENTITY, 'tag', '-a', OLD_TAG, '-m', OLD_TAG, 'HEAD~2'], {
    GIT_COMMITTER_DATE: '2026-01-01T00:00:00Z',
  });
  git(root, [...IDENTITY, 'tag', '-a', NEW_TAG, '-m', NEW_TAG, 'HEAD~1'], {
    GIT_COMMITTER_DATE: '2026-02-01T00:00:00Z',
  });
  return { projectId, root };
}

const threadRows = (harness: HarnessApp) => harness.rpc<ThreadRow[]>('HarnessListThreadRows');
const errorToasts = (page: Page) => page.locator('[role="alert"].text-error');
const branchTrigger = (page: Page) => page.getByTestId('branch-picker-trigger');
const branchMenu = (page: Page) => page.getByRole('menu', { name: 'Branches' });
const pickerRow = (page: Page, name: string) =>
  branchMenu(page).getByRole('menuitem', { name: new RegExp(`^${name.replace(/\./g, '\\.')}\\b`) });

async function openFreshDraft(harness: HarnessApp, page: Page): Promise<void> {
  await harness.open(page);
  await page.getByTestId('project-item-new-thread').first().click();
  await expect(page.getByTestId('composer-workspace-strip')).toBeVisible();
}

/** The branch and tag rows of the open picker, in display order. */
async function refRows(page: Page): Promise<string[]> {
  const texts = await branchMenu(page).getByRole('menuitem').allInnerTexts();
  return texts
    .map((text) => text.replace(/\s+/g, ' ').trim())
    .filter((text) => /^(main|v1)/.test(text));
}

async function expectSameFilesystemPath(actual: string | undefined, expected: string): Promise<void> {
  expect(actual).toBeDefined();
  expect(await realpath(actual!)).toBe(await realpath(expected));
}

test('a tag checks out detached, a commit there reads as detached, and a new branch keeps it', async ({
  harness,
  page,
}) => {
  const { root } = await seedProject(harness);
  const git = gitIn(harness);
  await openFreshDraft(harness, page);
  await expect(branchTrigger(page)).toHaveText('main');

  // Unsearched: every branch, then the tags newest first, each marked.
  await branchTrigger(page).click();
  await expect(pickerRow(page, OLD_TAG)).toBeVisible();
  expect(await refRows(page)).toEqual([
    expect.stringMatching(/^main/),
    expect.stringMatching(new RegExp(`^${BRANCH}`)),
    `${NEW_TAG} tag`,
    `${OLD_TAG} tag`,
  ]);

  // Searching mixes branches and tags in one list.
  await branchMenu(page).getByPlaceholder('Search branches and tags').fill('v1');
  await expect.poll(() => refRows(page)).toEqual([
    expect.stringMatching(new RegExp(`^${BRANCH}`)),
    `${NEW_TAG} tag`,
    `${OLD_TAG} tag`,
  ]);

  // Picking a tag detaches the workspace at the tag's commit.
  await pickerRow(page, NEW_TAG).click();
  await expect(page.getByRole('alert').filter({ hasText: `Checked out ${NEW_TAG}` })).toBeVisible();
  await expect(branchTrigger(page)).toHaveText(NEW_TAG);
  await expect(branchTrigger(page)).toHaveAttribute('data-checkout-kind', 'tag');
  expect(git(root, ['rev-parse', '--abbrev-ref', 'HEAD'])).toBe('HEAD');
  expect(git(root, ['rev-parse', 'HEAD'])).toBe(git(root, ['rev-parse', `${NEW_TAG}^{commit}`]));

  // A commit on the detached HEAD moves it off the tag.
  git(root, [...IDENTITY, 'commit', '--allow-empty', '--quiet', '-m', 'work on the tag']);
  const detached = git(root, ['rev-parse', 'HEAD']);
  await expect(branchTrigger(page)).toHaveText(`detached at ${detached.slice(0, 7)}`);
  await expect(branchTrigger(page)).toHaveAttribute('data-checkout-kind', 'commit');

  // "New branch…" from there branches at that commit.
  await branchTrigger(page).click();
  await branchMenu(page).getByRole('menuitem', { name: /New branch/ }).click();
  await expect(branchTrigger(page)).toHaveText(`From ${detached.slice(0, 7)}`);
  await page.getByTestId('worktree-branch-name-input').fill('keep-detached-work');
  await page.getByTestId('apply-worktree-intent-button').click();
  await expect(page.getByRole('alert').filter({ hasText: 'Created branch keep-detached-work' })).toBeVisible();
  await expect(branchTrigger(page)).toHaveText('keep-detached-work');
  await expect(branchTrigger(page)).toHaveAttribute('data-checkout-kind', 'branch');
  expect(git(root, ['symbolic-ref', '--short', 'HEAD'])).toBe('keep-detached-work');
  expect(git(root, ['rev-parse', 'keep-detached-work'])).toBe(detached);
  await expect(errorToasts(page)).toHaveCount(0);
});

test('a tag staged for a new worktree lands the thread in a worktree detached at the tag', async ({
  harness,
  page,
}) => {
  const { projectId, root } = await seedProject(harness);
  const git = gitIn(harness);
  await openFreshDraft(harness, page);

  await page.getByTestId('env-picker-trigger').click();
  await page.getByRole('menuitem', { name: 'New Worktree' }).click();
  await expect(page.getByTestId('env-picker-trigger')).toHaveText('New Worktree');
  await branchTrigger(page).click();
  await pickerRow(page, OLD_TAG).click();
  await expect(branchTrigger(page)).toHaveText(OLD_TAG);
  await expect(branchTrigger(page)).toHaveAttribute('data-checkout-kind', 'tag');

  await page.getByLabel('Message Input').fill('Where am I on the tag?');
  await page.getByTestId('composer-send').click();

  const mock = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'registered',
  );
  const worktrees = await harness.rpc<WorktreeListItem[] | null>(
    'GitListWorktrees',
    { projectId, workspacePath: '' },
  );
  const worktree = (worktrees ?? []).find((wt) => wt.tag === OLD_TAG && !wt.branch);
  expect(worktree, 'git registered no worktree detached at the tag').toBeDefined();
  await expectSameFilesystemPath(mock.cwd, worktree!.path);
  expect(git(worktree!.path, ['rev-parse', '--abbrev-ref', 'HEAD'])).toBe('HEAD');
  expect(git(worktree!.path, ['rev-parse', 'HEAD'])).toBe(git(root, ['rev-parse', `${OLD_TAG}^{commit}`]));
  // The project root did not move.
  expect(git(root, ['symbolic-ref', '--short', 'HEAD'])).toBe('main');

  const rows = await threadRows(harness);
  expect(rows).toHaveLength(1);
  await expectSameFilesystemPath(rows[0].worktreePath, worktree!.path);
  expect(rows[0].branch ?? '').toBe('');

  await expect(page.getByTestId('env-picker-trigger')).toHaveText(path.basename(worktree!.path));
  await expect(branchTrigger(page)).toHaveText(OLD_TAG);
  await expect(branchTrigger(page)).toHaveAttribute('data-checkout-kind', 'tag');

  // Work on the tag, then keep it from the git menu, which offers a branch
  // in Push's place on a detached HEAD.
  git(worktree!.path, [...IDENTITY, 'commit', '--allow-empty', '--quiet', '-m', 'fix on the tag']);
  const fix = git(worktree!.path, ['rev-parse', 'HEAD']);
  await expect(branchTrigger(page)).toHaveText(`detached at ${fix.slice(0, 7)}`);
  await page.getByLabel('More git actions').click();
  const gitMenu = page.getByRole('menu', { name: 'Git actions' });
  await expect(gitMenu.getByRole('menuitem', { name: 'Push' })).toHaveCount(0);
  await gitMenu.getByRole('menuitem', { name: 'Create branch from here' }).click();
  await expect(branchTrigger(page)).toHaveText(`From ${fix.slice(0, 7)}`);
  await page.getByTestId('worktree-branch-name-input').fill('hotfix-from-tag');
  await page.getByTestId('apply-worktree-intent-button').click();
  await expect(page.getByRole('alert').filter({ hasText: 'Created branch hotfix-from-tag' })).toBeVisible();
  await expect(branchTrigger(page)).toHaveText('hotfix-from-tag');
  expect(git(worktree!.path, ['symbolic-ref', '--short', 'HEAD'])).toBe('hotfix-from-tag');
  expect(git(worktree!.path, ['rev-parse', 'hotfix-from-tag'])).toBe(fix);
  await expect(errorToasts(page)).toHaveCount(0);
});
