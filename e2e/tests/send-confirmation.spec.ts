// A sent message is drawn only from the backend's own evidence of it. This
// holds the send RPC before it reaches the host: the composer is empty, the
// Send button reads as pending, and the timeline has no row for the message.
// Once the host has the send, its row arrives and the button leaves pending.
//
// Only a browser can show this: the pending state lives between a click and
// a wire event, and what replaces it is a mounted timeline row.
import { expect, test } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  claudeScenario,
  emit,
  seedAgentThread,
  startMock,
  textLines,
  waitForGate,
} from './agent-visibility-helpers.js';

const SEND_WITH_OPTIONS = 3632185196;

interface Item {
  kind: string;
  summary: string;
}

test('a sent message shows on the Send button until the backend row arrives', async ({ harness, page }) => {
  let release: (() => void) | undefined;
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.methodId === SEND_WITH_OPTIONS) release = () => server.send(message);
      else server.send(message);
    });
    server.onMessage((message) => socket.send(message));
  });
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('send-confirmation', [
      emit(textLines('start', 'Working.')),
      { waitSignal: { name: 'finish' } },
      emit([...textLines('finish', 'Completed.'), RESULT_LINE]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'send-confirmation', 'Send confirmation');
  await harness.open(page);
  await page.getByText('Send confirmation', { exact: true }).click();
  const mockId = await startMock(harness, threadId);

  const message = 'Confirm this message from the backend.';
  const input = page.getByLabel('Message Input');
  const bubble = page.getByTestId('user-message-bubble').filter({ hasText: message });
  const sent = async () =>
    (await harness.rpc<Item[]>('ListItems', threadId, true)).filter(
      (item) => item.kind === 'user_text' && item.summary === message,
    ).length;
  await input.fill(message);
  await page.getByRole('button', { name: 'Send message', exact: true }).click();
  await expect.poll(() => release !== undefined).toBe(true);

  // The host has not seen the send.
  const button = page.getByTestId('composer-send');
  await expect(button).toHaveAttribute('data-pending', 'true');
  await expect(button).toHaveAttribute('aria-busy', 'true');
  await expect(button).toHaveAccessibleName('Sending message');
  await expect(button).toBeDisabled();
  await expect(page.getByTestId('composer-interrupt')).toHaveCount(0);
  await expect(input).toHaveValue('');
  await expect(bubble).toHaveCount(0);
  await expect(page.getByTestId('send-queue-preview-row')).toHaveCount(0);
  expect(await sent()).toBe(0);

  release!();
  await expect(bubble).toHaveCount(1);
  await expect(page.locator('[data-testid="composer-send"][data-pending]')).toHaveCount(0);
  await waitForGate(harness, 'finish');
  await expect(page.getByRole('button', { name: 'Interrupt current turn', exact: true })).toBeVisible();
  expect(await sent()).toBe(1);

  await advance(harness, mockId, 'finish');
  await harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await expect(page.getByText('Completed.', { exact: true })).toBeVisible();
  await expect(bubble).toHaveCount(1);
  await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeDisabled();
});
