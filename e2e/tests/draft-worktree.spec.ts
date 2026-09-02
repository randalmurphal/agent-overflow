// A committed workspace choice is durable draft state. Selecting New
// Worktree materializes the placeholder before any git mutation, and the
// thread-scoped worktree operation creates and binds the checkout atomically.
// Empty-draft cleanup ignores staged and attached worktree drafts, so it
// cannot delete the owner while the operation is in flight or after it lands.
// The final test keeps the inverse transition pinned: an ordinary text-only
// draft still dematerializes when its text is erased.
import type { Page } from '@playwright/test';
import { realpath } from 'node:fs/promises';
import { test, expect, type HarnessMockEvent, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';

const PROJECT = 'draft-worktree-app';
/** Committed by the fixture and left unchecked-out, so a worktree can attach it. */
const EXISTING_BRANCH = 'existing-work';

/** The subset of a raw thread row these tests read. */
interface ThreadRow {
  id: string;
  projectId: string;
  workspacePath: string;
  worktreePath?: string;
  branch?: string;
}

interface WorktreeListItem {
  path: string;
  branch: string;
}

async function seedProject(harness: HarnessApp): Promise<string> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: PROJECT,
        repo: {
          commits: [{ message: 'init', files: { 'README.md': '# seed\n' } }],
          branches: [EXISTING_BRANCH],
        },
      },
    ],
  });
  return seed.projects[0].projectId;
}

const threadRows = (harness: HarnessApp) => harness.rpc<ThreadRow[]>('HarnessListThreadRows');

/** The worktree git itself registered for `branch`, or undefined. */
async function registeredWorktree(
  harness: HarnessApp,
  projectId: string,
  branch: string,
): Promise<WorktreeListItem | undefined> {
  const list = await harness.rpc<WorktreeListItem[] | null>(
    'GitListWorktrees',
    { projectId, workspacePath: '' },
  );
  return (list ?? []).find((wt) => wt.branch === branch);
}

const basename = (p: string) => p.split('/').pop() ?? p;

async function expectSameFilesystemPath(actual: string | undefined, expected: string): Promise<void> {
  expect(actual).toBeDefined();
  expect(await realpath(actual!)).toBe(await realpath(expected));
}

/**
 * The composer's workspace strip labels. `env` is where the next turn will
 * run, `branch` is what it will run on. After an apply they describe the
 * worktree bound to the row.
 */
function stripLabels(page: Page) {
  return {
    env: page.getByTestId('env-picker-trigger'),
    branch: page.getByTestId('branch-picker-trigger'),
  };
}

/** Error toasts only — the class is the toast's one type discriminator. */
const errorToasts = (page: Page) => page.locator('[role="alert"].text-error');

/** Open a project's brand-new draft placeholder: a pane, no thread row. */
async function openFreshDraft(harness: HarnessApp, page: Page): Promise<void> {
  await page.goto(harness.url);
  await page.getByTestId('project-item-new-thread').first().click();
  await expect(page.getByTestId('composer-workspace-strip')).toBeVisible();
  // The precondition the whole file rests on: a placeholder is not a row.
  expect(await threadRows(harness)).toHaveLength(0);
}

/** Stage "next turn runs in a new worktree" from the workspace picker. */
async function stageNewWorktree(page: Page): Promise<void> {
  await page.getByTestId('env-picker-trigger').click();
  await page.getByRole('menuitem', { name: 'New Worktree' }).click();
  await expect(stripLabels(page).env).toHaveText('New Worktree');
}

test('worktree selection materializes a draft and create binds it to an existing branch', async ({
  harness,
  page,
}) => {
  const projectId = await seedProject(harness);
  await openFreshDraft(harness, page);

  await stageNewWorktree(page);
  await expect.poll(async () => (await threadRows(harness)).length).toBe(1);
  await expect(page.getByTestId('thread-row')).toHaveCount(1);
  await page.getByTestId('branch-picker-trigger').click();
  await page.getByRole('menuitem', { name: EXISTING_BRANCH }).click();
  await expect(stripLabels(page).branch).toHaveText(EXISTING_BRANCH);

  await page.getByTestId('apply-worktree-intent-button').click();

  // Await the success toast first: it lands when the RPC resolves, so the
  // no-error assertion below is checked after the window an error could
  // have appeared in, not before it.
  await expect(
    page.getByRole('alert').filter({ hasText: `Created worktree on ${EXISTING_BRANCH}` }),
  ).toBeVisible();
  await expect(errorToasts(page)).toHaveCount(0);

  // The strip names the worktree git actually registered — not just some
  // label the frontend invented for itself.
  const worktree = await registeredWorktree(harness, projectId, EXISTING_BRANCH);
  expect(worktree, 'git registered no worktree for the attached branch').toBeDefined();
  await expect(stripLabels(page).env).toHaveText(basename(worktree!.path));
  await expect(stripLabels(page).branch).toHaveText(EXISTING_BRANCH);
  await expect(page.getByTestId('apply-worktree-intent-button')).toHaveCount(0);

  const rows = await threadRows(harness);
  expect(rows).toHaveLength(1);
  await expectSameFilesystemPath(rows[0].workspacePath, worktree!.path);
  await expectSameFilesystemPath(rows[0].worktreePath, worktree!.path);
  expect(rows[0].branch).toBe(EXISTING_BRANCH);
});

test('sending from that draft reuses the thread already bound to the worktree', async ({
  harness,
  page,
}) => {
  const projectId = await seedProject(harness);
  await openFreshDraft(harness, page);

  await stageNewWorktree(page);
  await page.getByTestId('branch-picker-trigger').click();
  await page.getByRole('menuitem', { name: EXISTING_BRANCH }).click();
  await page.getByTestId('apply-worktree-intent-button').click();
  await expect(
    page.getByRole('alert').filter({ hasText: `Created worktree on ${EXISTING_BRANCH}` }),
  ).toBeVisible();
  const worktree = await registeredWorktree(harness, projectId, EXISTING_BRANCH);
  expect(worktree).toBeDefined();
  const rowsBeforeSend = await threadRows(harness);
  expect(rowsBeforeSend).toHaveLength(1);
  const rowBeforeSend = rowsBeforeSend[0]!;

  await page.getByLabel('Message Input').fill('Where am I running?');
  await page.getByTestId('composer-send').click();

  // The provider session is the end of the binding chain: the mock reports
  // the cwd it was spawned in, and that is the worktree or the flow bound
  // the wrong thing somewhere between the strip and the session.
  const mock = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'registered',
  );
  await expectSameFilesystemPath(mock.cwd, worktree!.path);

  await expect(page.getByText('Where am I running?')).toBeVisible();
  await expect(errorToasts(page)).toHaveCount(0);

  // Exactly one row, still bound to the worktree rather than the project root.
  const rows = await threadRows(harness);
  expect(rows).toHaveLength(1);
  expect(rows[0].id).toBe(rowBeforeSend.id);
  expect(rows[0].projectId).toBe(projectId);
  await expectSameFilesystemPath(rows[0].worktreePath, worktree!.path);
  await expectSameFilesystemPath(rows[0].workspacePath, worktree!.path);
  expect(rows[0].branch).toBe(EXISTING_BRANCH);

  // And the send did not move the user's strip back to Local.
  await expect(stripLabels(page).env).toHaveText(basename(worktree!.path));
  await expect(stripLabels(page).branch).toHaveText(EXISTING_BRANCH);
  await expect(page.getByTestId('thread-row')).toHaveCount(1);
});

test('a fresh draft binds its materialized row to a new worktree branch', async ({
  harness,
  page,
}) => {
  const NEW_BRANCH = 'draft-cut';
  const projectId = await seedProject(harness);
  await openFreshDraft(harness, page);

  await stageNewWorktree(page);
  await expect.poll(async () => (await threadRows(harness)).length).toBe(1);
  // The other half of the picker: the inline name input routes through
  // PrepareThreadWorktree on the row stageNewWorktree just created.
  await page.getByTestId('new-branch-toggle').click();
  await page.getByTestId('worktree-branch-name-input').fill(NEW_BRANCH);
  await page.getByTestId('apply-worktree-intent-button').click();

  await expect(
    page.getByRole('alert').filter({ hasText: `Created worktree on ${NEW_BRANCH}` }),
  ).toBeVisible();
  await expect(errorToasts(page)).toHaveCount(0);

  // Looking the worktree up BY BRANCH is the branch assertion here: git
  // reports the new branch checked out at the path the strip now names.
  const worktree = await registeredWorktree(harness, projectId, NEW_BRANCH);
  expect(worktree, 'git registered no worktree for the new branch').toBeDefined();
  await expect(stripLabels(page).env).toHaveText(basename(worktree!.path));
  await expect(stripLabels(page).branch).toHaveText(NEW_BRANCH);
  await expect(page.getByTestId('apply-worktree-intent-button')).toHaveCount(0);

  const rows = await threadRows(harness);
  expect(rows).toHaveLength(1);
  await expectSameFilesystemPath(rows[0].workspacePath, worktree!.path);
  await expectSameFilesystemPath(rows[0].worktreePath, worktree!.path);
  expect(rows[0].branch).toBe(NEW_BRANCH);
});

test('typing materializes a draft row and erasing it takes the row back out', async ({
  harness,
  page,
}) => {
  await seedProject(harness);
  await openFreshDraft(harness, page);

  // Typed content is the one thing that legitimately materializes a row
  // from a placeholder — the sidebar shows it so unsent work is not lost.
  await page.getByLabel('Message Input').fill('half a thought');
  await expect(page.getByTestId('thread-row')).toHaveCount(1);
  await expect.poll(async () => (await threadRows(harness)).length).toBe(1);

  // Erasing it must take the row away again: that cleanup is what the old
  // worktree path raced, and removing the race must not have removed it.
  await page.getByLabel('Message Input').fill('');
  await expect(page.getByTestId('thread-row')).toHaveCount(0);
  await expect(page.getByTestId('project-thread-list-empty')).toBeVisible();
  await expect.poll(async () => (await threadRows(harness)).length).toBe(0);
  await expect(errorToasts(page)).toHaveCount(0);
});
