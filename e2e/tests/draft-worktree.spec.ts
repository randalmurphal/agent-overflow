// Naming a workspace for a DRAFT is a project-scoped operation: the
// composer's worktree/branch flow must not touch thread rows at all until
// the user actually sends something.
//
// The regression this pins had a narrow, ugly shape. Cutting a worktree
// used to run through the THREAD-scoped RPCs (PrepareThreadWorktree /
// AttachThreadWorktree), every one of which begins by reading a thread row
// — so the composer materialized an empty one just to have something to
// name. That row is item-less and content-less, which is exactly what the
// composer's own empty-draft cleanup deletes, and the two raced: the
// cleanup's DeleteEmptyDraftThread landed first and the worktree call came
// back "sql: no rows in result set" in a toast, with a worktree on disk and
// nothing pointing at it. The project-scoped path (PrepareProjectWorktree /
// AttachProjectWorktree / CreateProjectBranch) removes the row from the
// story entirely; the thread adopts the result at CreateThread time.
//
// So the load-bearing assertion in three of these four tests is a NEGATIVE
// one — that no thread row exists — and it cannot be made through any
// production read: `App.ListThreads` hides a row until it has an item or a
// content-carrying draft, which is precisely the row this bug created.
// `HarnessListThreadRows` is the raw row read that makes "nothing was
// created" observable. The fourth test is the other half of the same
// contract: the empty-draft cleanup that raced is still expected to work,
// so a row that materializes for typed text still dematerializes when the
// text is erased.
import type { Page } from '@playwright/test';
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
    'GitListWorktreesForProject',
    projectId,
  );
  return (list ?? []).find((wt) => wt.branch === branch);
}

const basename = (p: string) => p.split('/').pop() ?? p;

/**
 * The composer's workspace strip labels. `env` is where the next turn will
 * run, `branch` is what it will run on — both read the EFFECTIVE workspace,
 * so after an apply they describe the worktree even though no thread row
 * has moved (or, here, exists).
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

test('a fresh draft cuts a worktree on an existing branch and creates no thread row', async ({
  harness,
  page,
}) => {
  const projectId = await seedProject(harness);
  await openFreshDraft(harness, page);

  await stageNewWorktree(page);
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

  // The regression, stated directly: a worktree exists, and no thread does.
  expect(await threadRows(harness)).toHaveLength(0);
  await expect(page.getByTestId('thread-row')).toHaveCount(0);
  await expect(page.getByTestId('project-thread-list-empty')).toBeVisible();
});

test('sending from that draft materializes one thread bound to the worktree', async ({
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

  await page.getByLabel('Message Input').fill('Where am I running?');
  await page.getByTestId('composer-send').click();

  // The provider session is the end of the binding chain: the mock reports
  // the cwd it was spawned in, and that is the worktree or the flow bound
  // the wrong thing somewhere between the strip and the session.
  const mock = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'registered',
  );
  expect(mock.cwd).toBe(worktree!.path);

  await expect(page.getByText('Where am I running?')).toBeVisible();
  await expect(errorToasts(page)).toHaveCount(0);

  // Exactly one row, and it inherited the applied workspace rather than
  // sitting at the project root.
  const rows = await threadRows(harness);
  expect(rows).toHaveLength(1);
  expect(rows[0].projectId).toBe(projectId);
  expect(rows[0].worktreePath).toBe(worktree!.path);
  expect(rows[0].workspacePath).toBe(worktree!.path);
  expect(rows[0].branch).toBe(EXISTING_BRANCH);

  // And the send did not move the user's strip back to Local.
  await expect(stripLabels(page).env).toHaveText(basename(worktree!.path));
  await expect(stripLabels(page).branch).toHaveText(EXISTING_BRANCH);
  await expect(page.getByTestId('thread-row')).toHaveCount(1);
});

test('a fresh draft cuts a worktree on a NEW branch and creates no thread row', async ({
  harness,
  page,
}) => {
  const NEW_BRANCH = 'draft-cut';
  const projectId = await seedProject(harness);
  await openFreshDraft(harness, page);

  await stageNewWorktree(page);
  // The other half of the picker: the inline name input, which routes to
  // PrepareProjectWorktree instead of AttachProjectWorktree.
  await page.getByTestId('new-branch-toggle').click();
  await page.getByTestId('worktree-branch-name-input').fill(NEW_BRANCH);
  await page.getByTestId('apply-worktree-intent-button').click();

  await expect(
    page.getByRole('alert').filter({ hasText: `Created worktree on ${NEW_BRANCH}` }),
  ).toBeVisible();
  await expect(errorToasts(page)).toHaveCount(0);

  // Looking the worktree up BY BRANCH is the branch assertion here: git
  // reports the new branch checked out at the path the strip now names.
  // The branch TRIGGER deliberately keeps reading "From <base>" after an
  // apply — `markWorktreeIntentApplied` leaves the staged quadrant alone,
  // so the create-branch flow stays on screen with the name the user typed
  // — which is why this quadrant asserts the label on the env trigger only.
  const worktree = await registeredWorktree(harness, projectId, NEW_BRANCH);
  expect(worktree, 'git registered no worktree for the new branch').toBeDefined();
  await expect(stripLabels(page).env).toHaveText(basename(worktree!.path));
  await expect(page.getByTestId('worktree-branch-name-input')).toHaveValue(NEW_BRANCH);

  expect(await threadRows(harness)).toHaveLength(0);
  await expect(page.getByTestId('thread-row')).toHaveCount(0);
  await expect(page.getByTestId('project-thread-list-empty')).toBeVisible();
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
