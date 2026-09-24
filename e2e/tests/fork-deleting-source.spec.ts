// A fork asked for from a thread whose delete has begun is refused with a
// sentence the user can act on (app_thread_fork.go forkSource/forkRefusal,
// store.ErrForkSourceDeleted).
//
// The page keeps a deleted thread's sidebar row and pane until its
// DeleteThread call returns (threadRowActions.ts deleteThreadAction) or
// the thread's `deleted` broadcast arrives, and the delete of a large
// thread drains its items for seconds. Both fork entry points stay
// reachable in that window: the per-message fork in the open pane and the
// row's Fork Thread. The spec holds the page's DeleteThread reply and every
// event after it, so the window stays open after the backend's delete has
// finished, and forks the thread through each entry point.
//
// A fork that reaches the backend while the delete drains waits for the
// delete to release the thread's action lock and then finds the source
// gone; during a store delete that holds no action lock it reads the
// source's deleting mark.
// Both are unit tested (app_fork_test.go TestForkDuringASourceDeleteIsRefused,
// fork_lineage_test.go). This level shows the refusal crossing the wire and
// reaching the user from each entry point, with no fork created.
import { expect, test, type SeedResult } from './fixtures.js';
import { threadRows } from './thread-tools-helpers.js';

const SOURCE_TITLE = 'Deleting source';
const REFUSAL = 'This thread was deleted, so it cannot be forked.';
// methodRoutes.ts: DeleteThread.
const DELETE_THREAD = 1186337974;

interface ThreadEvent {
  action: string;
  id?: string;
}

const exactly = (title: string) => new RegExp(`^${title.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`);

test('a fork from a thread whose delete has begun is refused and creates no thread', async ({ harness, page }) => {
  await harness.rpc('UpdateSettings', { confirmDelete: true });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'fork-deleting-source',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# fixture\n' } }] },
        threads: [
          {
            title: SOURCE_TITLE,
            provider: 'claude',
            // The row offers Fork Thread only for a thread with a session.
            sessionRef: 'deleting-source-session',
            turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }],
          },
        ],
      },
    ],
  });
  const project = seed.projects[0];
  const sourceId = project.threadIds[0];

  // From the page's DeleteThread call on, the server's frames to the page
  // wait here, except the replies to other calls.
  let deleteCallId: string | undefined;
  const held: Array<string | Buffer> = [];
  let release: (() => void) | undefined;
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.methodId === DELETE_THREAD && frame.params?.[0] === sourceId) {
        deleteCallId = frame.id;
        release = () => {
          release = undefined;
          for (const waiting of held.splice(0)) socket.send(waiting);
        };
      }
      server.send(message);
    });
    server.onMessage((message) => {
      const frame = JSON.parse(String(message));
      const holding = release !== undefined
        && (frame.type === 'event' || frame.type === 'batch' || (frame.type === 'rpc' && frame.id === deleteCallId));
      if (holding) held.push(message);
      else socket.send(message);
    });
  });

  await harness.open(page);
  const sidebarRow = page.getByTestId('thread-row-title').filter({ hasText: exactly(SOURCE_TITLE) });
  await sidebarRow.click();
  await expect(page.getByTestId('chat-header-title')).toHaveText(SOURCE_TITLE);
  const userRow = page.getByTestId('user-message-bubble').filter({ hasText: 'set the stage' }).locator('xpath=..');
  await expect(userRow).toBeVisible();

  const sourceGone = harness.waitForEvent<ThreadEvent>(
    'thread:updated',
    (event) => event.action === 'deleted' && event.id === sourceId,
  );
  await sidebarRow.click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Delete', exact: true }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete' }).click();
  await expect.poll(() => deleteCallId !== undefined).toBe(true);
  await sourceGone;

  // The backend has deleted the thread; the page has not been told.
  await expect(sidebarRow).toBeVisible();
  await expect(page.getByTestId('chat-header-title')).toHaveText(SOURCE_TITLE);

  await userRow.hover();
  const forkButton = userRow.getByLabel('Fork from this message');
  await expect(forkButton).toBeEnabled();
  await forkButton.click();
  await expect(page.getByText(`Fork failed: ${REFUSAL}`, { exact: true })).toBeVisible();

  await sidebarRow.click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Fork Thread', exact: true }).click();
  await expect(page.getByText(REFUSAL, { exact: true })).toBeVisible();

  // Neither call created a thread, and the source is gone.
  expect((await threadRows(harness)).filter((row) => row.projectId === project.projectId)).toEqual([]);
  await expect(page.getByTestId('thread-row-title').filter({ hasText: `${SOURCE_TITLE} (fork)` })).toHaveCount(0);

  // The delete's reply and broadcast arrive: the row and the pane go.
  expect(held.length).toBeGreaterThan(0);
  release!();
  await expect(sidebarRow).toHaveCount(0);
  await expect(page.getByTestId('user-message-bubble').filter({ hasText: 'set the stage' })).toHaveCount(0);
  await expect(page.getByTestId('chat-header-title').filter({ hasText: exactly(SOURCE_TITLE) })).toHaveCount(0);
});
