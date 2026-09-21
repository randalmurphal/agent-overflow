// Compact layout: a pending side chat is visible, closable, and a late
// successful fork cannot recreate the dismissed pane or retain its scratch row.
import { test, expect, type SeedResult } from './fixtures.js';
import { plainScenario, setScenario, threadRows } from './thread-tools-helpers.js';

test('closing a preparing side chat returns to its source and disposes the late fork', async ({ harness, page }) => {
  let release: (() => void) | undefined;
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const server = socket.connectToServer();
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.methodId === 2246569884) release = () => server.send(message);
      else server.send(message);
    });
    server.onMessage(message => socket.send(message));
  });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: 'compact-fork', repo: {}, threads: [{
    title: 'Fork source', provider: 'claude', turns: [{ userText: 'Earlier prompt', items: [{ kind: 'assistant_text', summary: 'Earlier answer' }] }],
  }] }] });
  const source = seed.projects[0];
  await setScenario(harness, source.path, plainScenario({ name: 'compact-fork', provider: 'claude', texts: ['Ready.'] }));
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Fork source' }).click();
  await page.getByLabel('Message Input').fill('Prepare');
  await page.getByTestId('composer-send').click();
  await harness.waitForEvent('provider:turn_completed');
  await page.getByLabel('Message Input').fill('/side-chat');
  await page.getByTestId('composer-send').click();
  await expect.poll(() => !!release).toBe(true);
  const pending = page.getByTestId('side-chat-preparing');
  await expect(pending).toBeInViewport();
  const pane = page.locator('section[data-pane-kind="side-chat"]');
  await pane.getByTestId('pane-close').click();
  await expect(pane).toHaveCount(0);
  await expect(page.getByLabel('Message Input')).toBeInViewport();
  release!();
  // The composer waits for openSideChat to finish, including disposal of a
  // fork whose pane disappeared while the request was being prepared.
  await expect(page.getByText('The pane moved on before the side chat opened.', { exact: true })).toBeVisible();
  expect((await threadRows(harness)).filter(row => row.forkedFromThreadId === source.threadIds[0])).toHaveLength(0);
  await expect(pane).toHaveCount(0);
});
