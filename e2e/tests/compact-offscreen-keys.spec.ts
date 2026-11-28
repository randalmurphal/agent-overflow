// A key pressed with no field focused, on a phone with a keyboard attached
// (frontend/AGENTS.md, "Compact layout"). It acts only on the pane the
// screen shows. On the list screen a digit does not answer a thread's
// pending question, and a letter neither opens the thread nor types into
// its composer. On the thread screen the digit answers the question and
// the letter lands in the composer.
import type { Page } from '@playwright/test';

import { test, expect } from './fixtures.js';
import { advance, emit, seedAgentThread, startMock, waitForGate } from './agent-visibility-helpers.js';

const frame = (method: string, params: Record<string, unknown>) => JSON.stringify({ jsonrpc: '2.0', method, params: { threadId: '${THREAD_ID}', turnId: '${TURN_ID}', ...params } });
const started = frame('turn/started', { turn: { id: '${TURN_ID}' } });
const echo = frame('item/completed', { item: { type: 'userMessage', id: 'user-${TURN}', clientId: '${CLIENT_ID}', content: [{ type: 'text', text: '${USER_INPUT}' }] } });
const completed = frame('turn/completed', { turn: { id: '${TURN_ID}', status: 'completed' } });
const REQUEST_ID = 9101;
const ask = JSON.stringify({ jsonrpc: '2.0', id: REQUEST_ID, method: 'item/tool/requestUserInput', params: {
  threadId: '${THREAD_ID}', turnId: '${TURN_ID}', itemId: `blocking-${REQUEST_ID}`, isBlocking: true, autoResolutionMs: 1,
  questions: [{ id: 'scope', header: 'Scope', question: 'Choose the scope', options: [{ label: 'Small', description: 'One folder' }, { label: 'All', description: 'Every folder' }] }],
} });

/** Nothing holds the focus, and what a key did has reached the page. */
async function pressUnfocused(page: Page, key: string): Promise<void> {
  await page.evaluate(() => { if (document.activeElement instanceof HTMLElement) document.activeElement.blur(); });
  await expect.poll(() => page.evaluate(() => document.activeElement === document.body)).toBe(true);
  await page.keyboard.press(key);
  await page.evaluate(() => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
}

test('compact: a key with nothing focused acts only on the pane on screen', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', { scenario: {
    version: 1, name: 'offscreen-keys', provider: 'codex', afterTurns: 'silent',
    turns: [{ steps: [emit([started, echo, ask]), { waitSignal: { name: 'asked' } }, emit([completed])] }],
  } });
  const thread = await seedAgentThread(harness, 'offscreen-keys', 'Offscreen keys', 'codex');
  await harness.open(page);
  const screen = page.locator('html');
  const row = page.getByTestId('thread-row');
  await row.tap();
  await expect(screen).toHaveAttribute('data-compact-screen', 'thread');
  const mock = await startMock(harness, thread);
  await harness.send(thread, 'Ask before continuing');
  await waitForGate(harness, 'asked');
  const panel = page.getByTestId('composer-pending-user-input');
  const composer = page.getByLabel('Message Input');
  await expect(panel).toContainText('Choose the scope');
  const pending = async () => (await harness.rpc<{ userInputs: unknown[] }>('ListPendingInteractiveRequests', thread)).userInputs.length;

  // The list screen: the question is not on screen, and a digit leaves it.
  await page.getByTestId('compact-back').tap();
  await expect(screen).toHaveAttribute('data-compact-screen', 'list');
  await pressUnfocused(page, '2');
  await expect(panel.getByTestId('user-input-option-2')).toHaveAttribute('aria-pressed', 'false');
  await expect(screen).toHaveAttribute('data-compact-screen', 'list');
  expect(await pending()).toBe(1);

  // The thread screen: the same digit answers it.
  await row.tap();
  await expect(panel).toBeInViewport();
  const resolved = harness.waitForEvent<{ action: string; requestId: string }>('provider:user_input', (event) => event.action === 'resolve' && event.requestId === String(REQUEST_ID));
  await pressUnfocused(page, '2');
  await resolved;
  await expect(panel).toHaveCount(0);
  await advance(harness, mock, 'asked');
  await expect.poll(async () => Boolean((await harness.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', thread)).activeTurn)).toBe(false);

  // The list screen again, no question pending: a letter stays on the list
  // and types nowhere.
  await page.getByTestId('compact-back').tap();
  await expect(screen).toHaveAttribute('data-compact-screen', 'list');
  await pressUnfocused(page, 'x');
  await expect(screen).toHaveAttribute('data-compact-screen', 'list');
  await expect(composer).toHaveValue('');

  // The thread screen: the letter lands in the composer.
  await row.tap();
  await expect(composer).toBeInViewport();
  await pressUnfocused(page, 'x');
  await expect(composer).toHaveValue('x');
  await expect(composer).toBeFocused();
});
