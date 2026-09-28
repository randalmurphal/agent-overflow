// Removing a worktree from inside the app, through the shipped UI.
//
// Own worktree from the composer's workspace picker: the thread moves to
// Base and the open picker keeps listing the project's OTHER worktrees
// (and not the removed one) at once. The removal's reply carries the moved
// rows (RemoveOtherWorktree's `reattached`), which the picker applies before
// it re-reads the list; the thread:updated broadcast of the same rows is an
// event and can arrive after the reply, which the spec forces by holding the
// page's event frames. A terminal opened in the worktree is named in the
// confirmation ("1 terminal will close.") and its tab is gone afterwards.
//
// Draft on another client: a "+ New" draft bound to a worktree in one
// browser, with a staged new-branch intent, is moved to Base and loses the
// staged intent when a second browser removes that worktree from its git
// actions menu. The removing page never sees the draft's pane, so only the
// thread:updated / worktree:removed events can move it.
import type { Page } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { methodNameById } from './offhost-helpers.js';
import {
  attachWorktree,
  basename,
  errorToasts,
  listWorktrees,
  openThread,
  samePath,
  seedWorktreeProject,
  threadRow,
  threadRows,
} from './worktree-removal-helpers.js';

/**
 * Holds every event frame to `page` from its RemoveOtherWorktree call until
 * the reply to its first GitListWorktrees call after the removal's reply.
 */
async function holdEventsPastRemovalReply(page: Page) {
  let phase: 'idle' | 'holding' | 'replied' | 'released' = 'idle';
  let removeCallId = '';
  let listCallId = '';
  let heldCount = 0;
  const held: Array<string | Buffer> = [];
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message)) as { type?: string; id?: string; methodId?: number };
      if (frame.type === 'rpc' && frame.methodId !== undefined) {
        const method = methodNameById(frame.methodId);
        if (phase === 'idle' && method === 'RemoveOtherWorktree') {
          phase = 'holding';
          removeCallId = String(frame.id);
        } else if (phase === 'replied' && !listCallId && method === 'GitListWorktrees') {
          listCallId = String(frame.id);
        }
      }
      server.send(message);
    });
    server.onMessage((message) => {
      const frame = JSON.parse(String(message)) as { type?: string; id?: string };
      if ((phase === 'holding' || phase === 'replied') && (frame.type === 'event' || frame.type === 'batch')) {
        held.push(message);
        heldCount++;
        return;
      }
      socket.send(message);
      if (frame.type !== 'rpc') return;
      if (phase === 'holding' && String(frame.id) === removeCallId) phase = 'replied';
      else if (phase === 'replied' && listCallId && String(frame.id) === listCallId) {
        phase = 'released';
        for (const waiting of held.splice(0)) socket.send(waiting);
      }
    });
  });
  return { state: () => phase, heldEvents: () => heldCount };
}

const envTrigger = (page: Page) => page.getByTestId('env-picker-trigger');
const branchTrigger = (page: Page) => page.getByTestId('branch-picker-trigger');

test('removing the thread\'s own worktree from the workspace picker keeps the other worktrees listed and closes its terminal', async ({
  harness,
  page,
}) => {
  const OWN = 'own-wt';
  const OTHER = 'other-wt';
  const project = await seedWorktreeProject(harness, 'worktree-remove-own', ['Own worktree thread', 'Other worktree thread'], [OWN, OTHER]);
  const [ownId, otherId] = project.threadIds;
  const own = await attachWorktree(harness, ownId, OWN);
  const other = await attachWorktree(harness, otherId, OTHER);
  const ownPath = own.worktreePath!;

  // Pin the ordering the picker has to survive: from the removal call on,
  // every event frame to the page (the thread:updated of the moved row
  // among them) waits until the page's worktree list read after the reply
  // has been answered. The reply overtaking the broadcast is a real
  // ordering; this makes it the only one the test sees.
  const ordering = await holdEventsPastRemovalReply(page);

  await harness.open(page);
  await openThread(page, 'Own worktree thread');
  await expect(envTrigger(page)).toHaveText(basename(ownPath));

  // A terminal in the thread's pane opens in its workspace: the worktree.
  await page.getByRole('button', { name: 'Toggle Terminal' }).click();
  await expect.poll(async () => {
    const terminals = await harness.rpc<Array<{ cwd: string }>>('ListTerminals', ownId);
    return terminals.length === 1 && (await samePath(terminals[0].cwd, ownPath));
  }, { message: 'the terminal did not open in the worktree' }).toBe(true);
  const terminalTabs = page.locator('[data-testid^="terminal-tab-"]:not([data-testid^="terminal-tab-close-"])');
  await expect(terminalTabs).toHaveCount(1);

  await envTrigger(page).click();
  const menu = page.getByRole('menu', { name: 'Workspace' });
  await expect(menu.getByRole('menuitem', { name: basename(other.worktreePath!) })).toBeVisible();
  await menu.getByRole('button', { name: `Remove worktree ${basename(ownPath)}` }).click();
  const confirm = page.getByTestId('env-picker-confirm-row');
  await expect(confirm.getByTestId('env-picker-confirm-terminals')).toHaveText('1 terminal will close.');

  await confirm.getByTestId('env-picker-confirm-remove').click();
  await expect(page.getByRole('alert').filter({ hasText: `Removed worktree ${basename(ownPath)}` })).toBeVisible();

  // The trigger follows the moved row to the project root.
  await expect(envTrigger(page)).toHaveText('Base');
  await expect.poll(() => ordering.state(), { message: 'the page never re-read the worktree list' }).toBe('released');
  expect(ordering.heldEvents(), 'no event was overtaken, so the ordering was not exercised').toBeGreaterThan(0);
  // The picker is still open on the list it re-read after the removal: the
  // other worktree is there and the removed one is not. No reopen, no
  // later refresh: a list read against the removed directory would show
  // nothing but Base and New Worktree here.
  await expect(menu).toBeVisible();
  await expect(menu.getByRole('menuitem', { name: basename(other.worktreePath!) })).toBeVisible();
  await expect(menu.getByRole('menuitem', { name: basename(ownPath) })).toHaveCount(0);
  await expect(page.getByTestId('env-picker-list-error')).toHaveCount(0);

  // The terminal opened in the worktree closed, on the backend and on screen.
  await expect.poll(async () => (await harness.rpc<unknown[]>('ListTerminals', ownId)).length).toBe(0);
  await expect(terminalTabs).toHaveCount(0);

  // The row and git agree with the screen.
  const moved = await threadRow(harness, ownId);
  expect(await samePath(moved.workspacePath, project.root)).toBe(true);
  expect(moved.worktreePath ?? '').toBe('');
  const remaining = await listWorktrees(harness, project.projectId, project.root);
  expect(await Promise.all(remaining.map((wt) => samePath(wt.path, ownPath)))).not.toContain(true);
  expect((await threadRow(harness, otherId)).worktreePath).toBe(other.worktreePath);
  await expect(errorToasts(page)).toHaveCount(0);
});

test('removing a worktree on one client moves a draft bound to it on another to Base and drops its staged branch', async ({
  harness,
  page,
  browser,
}) => {
  const SHARED = 'shared-wt';
  const NEW_BRANCH = 'staged-branch';
  const DRAFT_TEXT = 'half-written plan for the shared worktree';
  const project = await seedWorktreeProject(harness, 'worktree-remove-draft', ['Worktree owner'], [SHARED]);
  const [ownerId] = project.threadIds;
  const owner = await attachWorktree(harness, ownerId, SHARED);
  const sharedPath = owner.worktreePath!;

  // Client A: a "+ New" draft pointed at the existing worktree.
  await harness.open(page);
  await page.getByTestId('project-item-new-thread').first().click();
  await expect(page.getByTestId('composer-workspace-strip')).toBeVisible();
  await envTrigger(page).click();
  await page.getByRole('menu', { name: 'Workspace' }).getByRole('menuitem', { name: basename(sharedPath) }).click();
  await expect(envTrigger(page)).toHaveText(basename(sharedPath));
  await expect.poll(async () => (await threadRows(harness)).length).toBe(2);
  const draft = (await threadRows(harness)).find((row) => row.id !== ownerId)!;
  expect(await samePath(draft.worktreePath, sharedPath)).toBe(true);

  // Unsent text keeps the draft row through the move; an empty one would
  // fall back to a placeholder once nothing ties it to the worktree.
  await page.getByLabel('Message Input').fill(DRAFT_TEXT);

  // Stage "create a branch here" on the draft: the intent lives only in
  // this client until it is applied.
  await branchTrigger(page).click();
  await page.getByRole('menu', { name: 'Branches' }).getByRole('menuitem', { name: 'New branch…' }).click();
  await page.getByTestId('worktree-branch-name-input').fill(NEW_BRANCH);
  await expect(page.getByTestId('apply-worktree-intent-button')).toBeVisible();

  // Client B: the worktree's owner thread, removed from its git menu.
  const other = await browser.newContext();
  try {
    const remote = await other.newPage();
    await harness.open(remote);
    await openThread(remote, 'Worktree owner');
    await expect(envTrigger(remote)).toHaveText(basename(sharedPath));
    await remote.getByRole('button', { name: 'More git actions' }).click();
    await remote.getByRole('menuitem', { name: 'Remove Worktree' }).click();
    await remote.getByRole('button', { name: 'Remove', exact: true }).click();
    await expect(remote.getByRole('alert').filter({ hasText: 'Worktree removed' })).toBeVisible();
    await expect(envTrigger(remote)).toHaveText('Base');

    // Client A never saw the reply. Its draft follows the events: Base, the
    // root's branch, and no staged branch left to apply.
    await expect(envTrigger(page)).toHaveText('Base');
    await expect(page.getByTestId('apply-worktree-intent-button')).toHaveCount(0);
    await expect(page.getByTestId('worktree-branch-name-input')).toHaveCount(0);
    await expect(branchTrigger(page)).toHaveText('main');
    await expect(page.getByLabel('Message Input')).toHaveValue(DRAFT_TEXT);

    const movedDraft = await threadRow(harness, draft.id);
    expect(await samePath(movedDraft.workspacePath, project.root)).toBe(true);
    expect(movedDraft.worktreePath ?? '').toBe('');
    await expect(errorToasts(page)).toHaveCount(0);
    await expect(errorToasts(remote)).toHaveCount(0);
  } finally {
    await other.close();
  }
});
