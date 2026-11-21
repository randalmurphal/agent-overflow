// Keyboard pane navigation carries the caret into the pane it reaches. A
// side chat still preparing has no composer, so its section takes the caret,
// and keys typed after the move never reach the conversation focus left.
import { test, expect, type SeedResult } from './fixtures.js';
import { plainScenario, setScenario } from './thread-tools-helpers.js';

test('typing after a move onto a preparing side chat sends nothing to the thread left', async ({ harness, page }) => {
  let release: (() => void) | undefined;
  // Holds the side chat's fork, so its pane stays preparing.
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const server = socket.connectToServer();
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.methodId === 2246569884) release = () => server.send(message);
      else server.send(message);
    });
    server.onMessage(message => socket.send(message));
  });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: 'pane-nav-caret', repo: {}, threads: [{
    title: 'Nav source', provider: 'claude', turns: [{ userText: 'Earlier prompt', items: [{ kind: 'assistant_text', summary: 'Earlier answer' }] }],
  }] }] });
  await setScenario(harness, seed.projects[0].path, plainScenario({ name: 'pane-nav-caret', provider: 'claude', texts: ['Ready.'] }));
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Nav source' }).click();
  const source = page.locator('section[data-pane-kind="thread"]');
  const composer = source.getByLabel('Message Input');
  await composer.fill('Prepare');
  await source.getByTestId('composer-send').click();
  await harness.waitForEvent('provider:turn_completed');
  await composer.fill('/side-chat');
  await source.getByTestId('composer-send').click();
  await expect.poll(() => !!release).toBe(true);
  const sideChat = page.locator('section[data-pane-kind="side-chat"]');
  await expect(sideChat.getByTestId('side-chat-preparing')).toBeVisible();

  await composer.click();
  await composer.fill('kept draft');
  await page.keyboard.press('Alt+l');
  await expect(sideChat).toHaveAttribute('data-pane-focused', 'true');
  await page.keyboard.type('stray');
  await page.keyboard.press('Enter');
  await expect(composer).toHaveValue('kept draft');
  await expect(source.getByText('stray')).toHaveCount(0);
  await expect(sideChat).toBeFocused();
  release!();
});
