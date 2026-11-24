// A committed workspace choice is durable draft state. Selecting New
// Worktree materializes the placeholder before any git mutation, and the
// thread-scoped worktree operation creates and binds the checkout atomically.
// Empty-draft cleanup ignores staged and attached worktree drafts, so it
// cannot delete the owner while the operation is in flight or after it lands.
// Every screen lists such a draft once it is real (a bound worktree or draft
// text), whether it opened before or after, and erasing its text unlists it
// everywhere unless a worktree is bound.
// The final tests keep the inverse transition pinned: an ordinary text-only
// draft still dematerializes when its text is erased, and text typed while
// that delete's reply is held (a slow connection) or on another screen is
// never saved to the deleted row and ends up in a thread of its own.
import type { Browser, Page } from '@playwright/test';
import { realpath } from 'node:fs/promises';
import { test, expect, type HarnessMockEvent, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { DRAFT_SAVE_DEBOUNCE_MS } from '../../frontend/src/lib/pageTiming.ts';
import { methodIdByName, trackPageCalls } from './offhost-helpers.js';

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
  await harness.open(page);
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

test('worktree selection materializes a draft, create binds it to an existing branch, and a send from it reuses that thread', async ({
  harness,
  page,
}) => {
  const projectId = await seedProject(harness);
  await openFreshDraft(harness, page);

  await stageNewWorktree(page);
  await expect.poll(async () => (await threadRows(harness)).length).toBe(1);
  // A staged choice lives on this screen only; the sidebar lists the
  // thread once a worktree is bound to it.
  await expect(page.getByTestId('thread-row')).toHaveCount(0);
  await page.getByTestId('branch-picker-trigger').click();
  await page.getByRole('menuitem', { name: EXISTING_BRANCH }).click();
  await expect(stripLabels(page).branch).toHaveText(EXISTING_BRANCH);

  await page.getByTestId('apply-worktree-intent-button').click();
  await expect(page.getByTestId('thread-row')).toHaveCount(1);

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

  const rowsBeforeSend = await threadRows(harness);
  expect(rowsBeforeSend).toHaveLength(1);
  await expectSameFilesystemPath(rowsBeforeSend[0].workspacePath, worktree!.path);
  await expectSameFilesystemPath(rowsBeforeSend[0].worktreePath, worktree!.path);
  expect(rowsBeforeSend[0].branch).toBe(EXISTING_BRANCH);
  const rowBeforeSend = rowsBeforeSend[0]!;

  // Sending from that draft reuses the thread already bound to the worktree.
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

  // And the send did not move the user's strip back to Base.
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

// A second frontend never receives the deleting RPC's reply. It must converge
// from the broadcast, while the initiating composer keeps its placeholder.
test('erasing a text draft removes it live from another client and leaves the composer usable', async ({
  harness, page, browser,
}) => {
  await seedProject(harness);
  await openFreshDraft(harness, page);
  const observer = await browser.newContext({ viewport: { width: 412, height: 915 }, isMobile: true, hasTouch: true });
  try {
    const phone = await observer.newPage();
    await harness.open(phone);
    await page.getByLabel('Message Input').fill('shared unsent thought');
    await expect(page.getByTestId('thread-row')).toHaveCount(1);
    await expect(phone.getByTestId('thread-row')).toHaveCount(1);
    const [original] = await threadRows(harness);

    await page.getByLabel('Message Input').fill('');
    await expect(page.getByTestId('thread-row')).toHaveCount(0);
    await expect(phone.getByTestId('thread-row')).toHaveCount(0);
    await expect.poll(async () => (await threadRows(harness)).length).toBe(0);
    await expect(page.getByLabel('Message Input')).toBeVisible();

    await page.getByLabel('Message Input').fill('a new thought');
    await expect(page.getByTestId('thread-row')).toHaveCount(1);
    await expect(phone.getByTestId('thread-row')).toHaveCount(1);
    const [replacement] = await threadRows(harness);
    expect(replacement.id).not.toBe(original.id);
    await phone.getByTestId('thread-row').click();
    await expect(phone.getByLabel('Message Input')).toHaveValue('a new thought');
    await expect(errorToasts(page)).toHaveCount(0);
    await expect(errorToasts(phone)).toHaveCount(0);
  } finally {
    await observer.close();
  }
});

/** Bind the open draft to a worktree on EXISTING_BRANCH and wait for the bind. */
async function applyExistingBranchWorktree(page: Page): Promise<void> {
  await page.getByTestId('branch-picker-trigger').click();
  await page.getByRole('menuitem', { name: EXISTING_BRANCH }).click();
  await page.getByTestId('apply-worktree-intent-button').click();
  await expect(
    page.getByRole('alert').filter({ hasText: `Created worktree on ${EXISTING_BRANCH}` }),
  ).toBeVisible();
}

/** A screen of its own (a separate browser context), opened now. */
async function openScreen(harness: HarnessApp, browser: Browser): Promise<{ page: Page; close: () => Promise<void> }> {
  const context = await browser.newContext();
  const screen = await context.newPage();
  await harness.open(screen);
  await expect(screen.getByTestId('project-item-new-thread').first()).toBeVisible();
  return { page: screen, close: () => context.close() };
}

// The sidebar lists a never-sent thread once it is real: draft content, a
// bound worktree, or an item. A screen that opens later and a screen already
// open list the same never-sent threads.
test('a screen opened after an empty draft was bound to a worktree lists it, as the open screens do', async ({
  harness, page, browser,
}) => {
  await seedProject(harness);
  await openFreshDraft(harness, page);
  await stageNewWorktree(page);
  await expect.poll(async () => (await threadRows(harness)).length).toBe(1);

  const early = await openScreen(harness, browser);
  try {
    // Staged but not bound: no screen lists it.
    await expect(early.page.getByTestId('thread-row')).toHaveCount(0);
    await expect(page.getByTestId('thread-row')).toHaveCount(0);

    await applyExistingBranchWorktree(page);
    await expect(page.getByTestId('thread-row')).toHaveCount(1);
    await expect(early.page.getByTestId('thread-row')).toHaveCount(1);

    const late = await openScreen(harness, browser);
    try {
      await expect(late.page.getByTestId('thread-row')).toHaveCount(1);
      const [row] = await threadRows(harness);
      await expect(late.page.locator(`[data-sidebar-thread-id="${row.id}"]`)).toHaveCount(1);
    } finally {
      await late.close();
    }
    await expect(errorToasts(page)).toHaveCount(0);
  } finally {
    await early.close();
  }
});

test('erasing the text of a worktree-bound draft keeps it listed on every screen', async ({
  harness, page, browser,
}) => {
  await seedProject(harness);
  await openFreshDraft(harness, page);
  await stageNewWorktree(page);
  await applyExistingBranchWorktree(page);
  const input = page.getByLabel('Message Input');
  await input.fill('half a thought');
  const [row] = await threadRows(harness);
  await expect.poll(async () => (await harness.rpc<{ content: string }>('GetDraft', row.id)).content).toBe('half a thought');

  const other = await openScreen(harness, browser);
  try {
    await expect(other.page.getByTestId('thread-row')).toHaveCount(1);
    await input.fill('');
    await expect.poll(async () => (await harness.rpc<{ content: string }>('GetDraft', row.id)).content).toBe('');
    await expect(page.getByTestId('thread-row')).toHaveCount(1);
    await expect(other.page.getByTestId('thread-row')).toHaveCount(1);
    const late = await openScreen(harness, browser);
    try {
      await expect(late.page.getByTestId('thread-row')).toHaveCount(1);
    } finally {
      await late.close();
    }
    expect((await threadRows(harness)).map((r) => r.id)).toEqual([row.id]);
  } finally {
    await other.close();
  }
});

// A staged worktree choice keeps the erased draft's row (empty-draft cleanup
// skips it), but nothing makes the thread real: it leaves every sidebar and
// the pane typing into it stays on it.
test('erasing the text of a draft with no worktree takes it off every screen and keeps the pane', async ({
  harness, page, browser,
}) => {
  await seedProject(harness);
  await openFreshDraft(harness, page);
  await stageNewWorktree(page);
  const input = page.getByLabel('Message Input');
  await input.fill('half a thought');
  await expect(page.getByTestId('thread-row')).toHaveCount(1);
  await expect.poll(async () => (await threadRows(harness)).length).toBe(1);
  const [row] = await threadRows(harness);
  await expect.poll(async () => (await harness.rpc<{ content: string }>('GetDraft', row.id)).content).toBe('half a thought');

  const other = await openScreen(harness, browser);
  try {
    await expect(other.page.getByTestId('thread-row')).toHaveCount(1);
    await input.fill('');
    await expect(page.getByTestId('thread-row')).toHaveCount(0);
    await expect(other.page.getByTestId('thread-row')).toHaveCount(0);
    const late = await openScreen(harness, browser);
    try {
      await expect(late.page.getByTestId('thread-row')).toHaveCount(0);
    } finally {
      await late.close();
    }
    // The pane typing into it keeps its thread and its staged choice.
    expect((await threadRows(harness)).map((r) => r.id)).toEqual([row.id]);
    await expect(stripLabels(page).env).toHaveText('New Worktree');
    await input.fill('typed again');
    await expect(page.getByTestId('thread-row')).toHaveCount(1);
    await expect(other.page.getByTestId('thread-row')).toHaveCount(1);
    expect((await threadRows(harness)).map((r) => r.id)).toEqual([row.id]);
    await expect(errorToasts(page)).toHaveCount(0);
  } finally {
    await other.close();
  }
});

/**
 * Records the page's draft saves and holds the first empty-draft delete's
 * reply. The delete reaches the backend at once; its reply waits, as a slow
 * round trip holds it on a connected browser or phone.
 */
async function holdDeleteReply(page: Page) {
  const deleteMethod = methodIdByName('DeleteEmptyDraftThread');
  const saveMethod = methodIdByName('SaveDraft');
  const saves: Array<[string, string]> = [];
  let deleteCallId = '';
  let release: (() => void) | undefined;
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message)) as { type?: string; id?: string; methodId?: number; params?: unknown[] };
      if (frame.type === 'rpc' && frame.methodId === saveMethod) saves.push([String(frame.params?.[0]), String(frame.params?.[1])]);
      if (frame.type === 'rpc' && frame.methodId === deleteMethod && !deleteCallId) deleteCallId = String(frame.id);
      server.send(message);
    });
    server.onMessage((message) => {
      const frame = JSON.parse(String(message)) as { type?: string; id?: string };
      if (frame.type === 'rpc' && deleteCallId && String(frame.id) === deleteCallId && !release) {
        release = () => socket.send(message);
        return;
      }
      socket.send(message);
    });
  });
  return {
    held: () => Boolean(release),
    release: () => release!(),
    savesAfterErase: (threadId: string) =>
      saves.filter(([id, content]) => id === threadId && content !== '' && content !== 'half a thought'),
  };
}

/** Materialize a draft with text, erase it, and wait until its delete has landed with the reply held. */
async function eraseWithDeleteHeld(harness: HarnessApp, page: Page, hold: Awaited<ReturnType<typeof holdDeleteReply>>) {
  await openFreshDraft(harness, page);
  const input = page.getByLabel('Message Input');
  await input.fill('half a thought');
  await expect.poll(async () => (await threadRows(harness)).length).toBe(1);
  const [original] = await threadRows(harness);
  await input.fill('');
  await expect.poll(hold.held, { message: 'the delete never replied' }).toBe(true);
  expect((await threadRows(harness)).map((row) => row.id)).not.toContain(original.id);
  return { input, original };
}

/**
 * The composer's save debounce: every save a page still had due falls due
 * within it, so moving a page clock this far runs them, and it is far short
 * of anything else of the connection.
 */
const PAST_DRAFT_DEBOUNCE_MS = DRAFT_SAVE_DEBOUNCE_MS;

test('typing again while the erased draft is being deleted keeps every character in the replacement thread', async ({
  harness,
  page,
}) => {
  await seedProject(harness);
  const hold = await holdDeleteReply(page);
  // The page clock runs as the real one does; the save debounce is crossed
  // below by moving it on.
  await page.clock.install();
  const { input, original } = await eraseWithDeleteHeld(harness, page, hold);

  await input.pressSequentially('typed again');
  // The hold outlasts the composer's save debounce, so the new text's save
  // falls due while the reply is outstanding.
  await page.clock.fastForward(PAST_DRAFT_DEBOUNCE_MS);
  hold.release();

  await expect.poll(async () => (await threadRows(harness)).length).toBe(1);
  const [replacement] = await threadRows(harness);
  expect(replacement.id).not.toBe(original.id);
  await expect(input).toHaveValue('typed again');
  await expect.poll(async () => (await harness.rpc<{ content: string }>('GetDraft', replacement.id)).content).toBe('typed again');
  expect(hold.savesAfterErase(original.id)).toEqual([]);
  await expect(errorToasts(page)).toHaveCount(0);
});

// Leaving a new thread with text in it keeps that text as a draft thread in
// the sidebar; leaving during the erased draft's delete keeps it the same way.
test('text typed during the erased draft’s delete and left by opening another thread stays as a draft thread', async ({
  harness,
  page,
}) => {
  await seedProject(harness);
  const hold = await holdDeleteReply(page);
  const { input, original } = await eraseWithDeleteHeld(harness, page, hold);

  await input.pressSequentially('typed then left');
  await page.getByTestId('project-item-new-thread').first().click();
  await expect(input).toHaveValue('');
  hold.release();

  await expect(page.getByTestId('thread-row')).toHaveCount(1);
  await expect.poll(async () => (await threadRows(harness)).length).toBe(1);
  const [kept] = await threadRows(harness);
  expect(kept.id).not.toBe(original.id);
  await expect.poll(async () => (await harness.rpc<{ content: string }>('GetDraft', kept.id)).content).toBe('typed then left');
  expect(hold.savesAfterErase(original.id)).toEqual([]);
  await expect(errorToasts(page)).toHaveCount(0);
  await page.getByTestId('thread-row').click();
  await expect(input).toHaveValue('typed then left');
});

/**
 * A page whose draft saves for one thread reach the server only once
 * released; after that every frame passes. `calls` is the page's own record
 * of its calls, installed after the route so it sees the routed socket; a
 * save the route holds stays open in it until released and answered.
 */
async function holdDraftSaves(page: Page, threadId: () => string) {
  const saveMethod = methodIdByName('SaveDraft');
  const held: Array<() => void> = [];
  let holding = true;
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message)) as { type?: string; id?: string; methodId?: number; params?: unknown[] };
      if (holding && frame.type === 'rpc' && frame.methodId === saveMethod && threadId() && frame.params?.[0] === threadId()) {
        held.push(() => server.send(message));
        return;
      }
      server.send(message);
    });
  });
  return {
    calls: await trackPageCalls(page),
    count: () => held.length,
    release: () => {
      holding = false;
      held.splice(0).forEach((send) => send());
    },
  };
}

// Other screens on the same new thread keep typing while this one erases it.
// Their saves reach the backend only after the delete, so each learns of the
// deletion holding text the thread never received.
test('text typed on other screens into a new thread this screen erases ends in a thread of its own on each', async ({
  harness, page, browser,
}) => {
  await seedProject(harness);
  let shared = '';
  const others = await Promise.all([browser.newContext(), browser.newContext()]);
  try {
    // Every page's clock runs as the real one does; the end moves them all
    // on past the save debounce.
    const screens = await Promise.all(others.map(async (context) => {
      const other = await context.newPage();
      await other.clock.install();
      return { page: other, saves: await holdDraftSaves(other, () => shared) };
    }));
    await page.clock.install();
    const calls = await trackPageCalls(page);
    await openFreshDraft(harness, page);
    await page.getByLabel('Message Input').fill('half a thought');
    await expect.poll(async () => (await threadRows(harness)).length).toBe(1);
    shared = (await threadRows(harness))[0].id;
    for (const screen of screens) await harness.open(screen.page);
    const texts = ['typed on the second screen', 'typed on the third screen'];
    for (const [i, screen] of screens.entries()) {
      await screen.page.getByTestId('thread-row').click();
      const input = screen.page.getByLabel('Message Input');
      await expect(input).toHaveValue('half a thought');
      await input.fill(texts[i]);
      await expect.poll(screen.saves.count, { message: 'the save never fell due' }).toBeGreaterThan(0);
    }

    await page.getByLabel('Message Input').fill('');
    await expect.poll(async () => (await threadRows(harness)).length).toBe(2);
    for (const screen of screens) screen.saves.release();

    const rows = await threadRows(harness);
    expect(rows.map((row) => row.id)).not.toContain(shared);
    await expect.poll(async () => (await Promise.all(rows.map(async (row) =>
      (await harness.rpc<{ content: string }>('GetDraft', row.id)).content))).sort()).toEqual([...texts].sort());
    for (const [i, screen] of screens.entries()) {
      await expect(screen.page.getByLabel('Message Input')).toHaveValue(texts[i]);
      await expect(errorToasts(screen.page)).toHaveCount(0);
    }
    await expect(page.getByLabel('Message Input')).toHaveValue('');
    await expect(errorToasts(page)).toHaveCount(0);
    // Nothing a page still had due makes another thread: every debounce has
    // run, and every call it sent, which the page recorded as it sent it
    // inside the clock move, is answered.
    for (const view of [page, ...screens.map((screen) => screen.page)]) await view.clock.fastForward(PAST_DRAFT_DEBOUNCE_MS);
    for (const pageCalls of [calls, ...screens.map((screen) => screen.saves.calls)]) {
      await pageCalls.drain('a call a page made after the last save debounce was not answered');
    }
    expect((await threadRows(harness)).length).toBe(2);
  } finally {
    await Promise.all(others.map((context) => context.close()));
  }
});
