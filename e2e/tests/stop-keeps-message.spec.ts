// A person's Stop is a plain interrupt (docs/architecture/revert-modes.md):
// the sent message stays in the transcript, the turn ends interrupted, and
// the composer is left as it was. Real SPA, transport and backend with the
// claude and codex mocks parked before any agent output.
//
//   right after Send   - the message row stays, the composer stays empty,
//                        the thread reads interrupted, and the next send
//                        is a normal turn the agent answers.
//   send unanswered    - Esc while this client's send RPC is held does not
//                        interrupt yet: the composer keeps showing the send
//                        as pending, Send stays gated, and no interrupt
//                        reaches the backend until the send answers. Then
//                        the turn the send started is interrupted, and the
//                        message row stays.
//   lost Stop reply    - a connection lost before the interrupt answers
//                        releases the Send gate; the turn is still stopped.
//
// A sampler records the composer's value on every animation frame, so a
// one-frame restore of the sent text fails rather than being polled past.
//
// claude-tui's notice for a Stop before any reply is not covered here: no
// mock speaks the terminal UI. internal/app/app_interrupt_turn_test.go
// covers it.
import type { Page } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { RESULT_LINE, seedAgentThread, startMock, textLines, waitForGate } from './agent-visibility-helpers.js';

const SEND_WITH_OPTIONS = 3632185196;
const INTERRUPT_TURN = 850013031;
const SAVE_DRAFT = 3025273299;

const ANSWER = 'Answered after the stop.';
// The row the backend writes into a turn the person stopped.
const STOPPED = 'Stopped by user';

type Provider = 'claude' | 'codex';

interface TurnCompleted {
  threadId: string;
  stopReason: string;
}

const codexFrame = (method: string, params: object) => JSON.stringify({ jsonrpc: '2.0', method, params });
const codexStarted = () => codexFrame('turn/started', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}' } });

// Turn 1 parks before any output, so only a Stop ends it. Turn 2 answers.
function stopScenario(provider: Provider) {
  const answer = provider === 'claude'
    ? [...textLines('msg-after-stop', ANSWER), RESULT_LINE]
    : [
      codexStarted(),
      codexFrame('item/started', { threadId: '${THREAD_ID}', turnId: '${TURN_ID}', item: { type: 'agentMessage', id: 'msg-after-stop', status: 'inProgress', text: '' } }),
      codexFrame('item/agentMessage/delta', { threadId: '${THREAD_ID}', turnId: '${TURN_ID}', itemId: 'msg-after-stop', delta: ANSWER }),
      codexFrame('item/completed', { threadId: '${THREAD_ID}', turnId: '${TURN_ID}', item: { type: 'agentMessage', id: 'msg-after-stop', status: 'completed', text: ANSWER } }),
      codexFrame('turn/completed', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}', status: 'completed' } }),
    ];
  return {
    version: 1,
    name: `stop-keeps-message-${provider}`,
    provider,
    turns: [
      { steps: [...(provider === 'codex' ? [{ emit: { lines: [codexStarted()] } }] : []), { waitSignal: { name: 'answer' } }] },
      { steps: [{ emit: { lines: answer } }] },
    ],
    afterTurns: 'silent',
  };
}

async function openThread(harness: Parameters<typeof seedAgentThread>[0], page: Page, provider: Provider, title: string) {
  await harness.rpc('HarnessSetScenario', { scenario: stopScenario(provider) });
  const threadId = await seedAgentThread(harness, `stop-${provider}`, title, provider);
  await harness.open(page);
  await page.getByText(title, { exact: true }).click();
  await startMock(harness, threadId);
  return threadId;
}

async function startComposerSampler(page: Page): Promise<void> {
  await page.evaluate(() => {
    const state = { values: [] as string[], running: true };
    (window as any).__aoComposerValues = state;
    const sample = () => {
      const input = document.querySelector<HTMLTextAreaElement>('textarea[aria-label="Message Input"]');
      state.values.push(input?.value ?? '<none>');
      if (state.running) requestAnimationFrame(sample);
    };
    requestAnimationFrame(sample);
  });
}

async function stopComposerSampler(page: Page): Promise<string[]> {
  return await page.evaluate(() => {
    const state = (window as any).__aoComposerValues;
    state.running = false;
    return state.values as string[];
  });
}

async function userRows(harness: Parameters<typeof seedAgentThread>[0], threadId: string, text: string): Promise<number> {
  const rows = await harness.rpc<Array<{ kind: string; summary: string }>>('ListItems', threadId, true);
  return rows.filter((row) => row.kind === 'user_text' && row.summary === text).length;
}

// The follow-up is a normal turn: sent, answered, not interrupted.
async function sendFollowUp(harness: Parameters<typeof seedAgentThread>[0], page: Page, threadId: string, text: string) {
  const answered = harness.waitForEvent<TurnCompleted>('provider:turn_completed', (ev) => ev.threadId === threadId);
  await page.getByRole('button', { name: 'Send message', exact: true }).click();
  expect((await answered).stopReason).not.toBe('interrupted');
  await expect(page.getByTestId('user-message-bubble').filter({ hasText: text })).toHaveCount(1);
  await expect(page.getByText(ANSWER, { exact: true })).toBeVisible();
  await expect(page.getByLabel('Message Input')).toHaveValue('');
}

for (const provider of ['claude', 'codex'] as const) {
  test(`${provider}: Stop right after Send keeps the message and leaves the composer empty`, async ({ harness, page }) => {
    const title = `Stop right after send ${provider}`;
    const threadId = await openThread(harness, page, provider, title);
    const input = page.getByLabel('Message Input');
    const sent = `Stop me before you answer (${provider}).`;
    const followUp = `Now answer (${provider}).`;
    const bubble = page.getByTestId('user-message-bubble').filter({ hasText: sent });

    await input.fill(sent);
    await page.getByRole('button', { name: 'Send message', exact: true }).click();
    await waitForGate(harness, 'answer');
    await expect(bubble).toHaveCount(1);
    await expect(input).toHaveValue('');

    await startComposerSampler(page);
    const stopped = harness.waitForEvent<TurnCompleted>('provider:turn_completed', (ev) => ev.threadId === threadId);
    await page.getByRole('button', { name: 'Interrupt current turn', exact: true }).click();
    expect((await stopped).stopReason).toBe('interrupted');
    await expect(page.getByRole('button', { name: 'Interrupt current turn', exact: true })).toHaveCount(0);
    await expect(page.getByTestId('activity-rail-working')).toHaveCount(0);
    await expect(page.getByText(STOPPED, { exact: true })).toHaveCount(1);
    await expect(bubble).toHaveCount(1);
    await expect(input).toHaveValue('');
    expect(await userRows(harness, threadId, sent)).toBe(1);
    const values = await stopComposerSampler(page);
    expect(values.length).toBeGreaterThan(0);
    expect(new Set(values)).toEqual(new Set(['']));

    // Send is usable again: no stop is left holding it.
    await input.fill(followUp);
    await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeEnabled();
    await sendFollowUp(harness, page, threadId, followUp);
    await expect(bubble).toHaveCount(1);
  });

  test(`${provider}: a Stop during an unanswered send interrupts the turn once the send answers`, async ({ harness, page }) => {
    const sent = `Held send (${provider}).`;
    const draft = `Typed while stopping (${provider}).`;
    let release: (() => void) | undefined;
    let sends = 0;
    let interrupts = 0;
    let draftSaved = false;
    await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
      const server = socket.connectToServer();
      socket.onMessage((message) => {
        const frame = JSON.parse(String(message));
        if (frame.type === 'rpc' && frame.methodId === INTERRUPT_TURN) interrupts++;
        if (frame.type === 'rpc' && frame.methodId === SAVE_DRAFT && String(message).includes(draft)) draftSaved = true;
        if (frame.type === 'rpc' && frame.methodId === SEND_WITH_OPTIONS && sends++ === 0) release = () => server.send(message);
        else server.send(message);
      });
      server.onMessage((message) => socket.send(message));
    });
    const threadId = await openThread(harness, page, provider, `Stop held send ${provider}`);
    const input = page.getByLabel('Message Input');
    const bubble = page.getByTestId('user-message-bubble').filter({ hasText: sent });

    await input.fill(sent);
    await page.getByRole('button', { name: 'Send message', exact: true }).click();
    await expect.poll(() => release !== undefined).toBe(true);
    const button = page.getByTestId('composer-send');
    await expect(button).toHaveAttribute('data-pending', 'true');
    await expect(input).toHaveValue('');

    await startComposerSampler(page);
    await input.press('Escape');
    // The composer keeps showing the send as pending, and a typed draft
    // cannot be sent past the stop.
    await input.fill(draft);
    await expect(button).toHaveAttribute('data-pending', 'true');
    await expect(button).toHaveAccessibleName('Sending message');
    await expect(button).toBeDisabled();
    await input.press('Enter');
    await expect(input).toHaveValue(draft);
    // The draft's save left the page after the Esc, on the same ordered
    // socket, so an interrupt sent at the Esc would have been seen first.
    await expect.poll(() => draftSaved).toBe(true);
    expect(interrupts).toBe(0);
    expect(sends).toBe(1);
    expect(await userRows(harness, threadId, sent)).toBe(0);

    const stopped = harness.waitForEvent<TurnCompleted>('provider:turn_completed', (ev) => ev.threadId === threadId);
    release!();
    expect((await stopped).stopReason).toBe('interrupted');
    expect(interrupts).toBe(1);
    await expect(bubble).toHaveCount(1);
    expect(await userRows(harness, threadId, sent)).toBe(1);
    await expect(page.getByText(STOPPED, { exact: true })).toHaveCount(1);
    await expect(page.getByRole('button', { name: 'Interrupt current turn', exact: true })).toHaveCount(0);
    await expect(button).toBeEnabled();
    await expect(input).toHaveValue(draft);
    const values = await stopComposerSampler(page);
    expect(values.length).toBeGreaterThan(0);
    expect(values.filter((value) => value.includes(sent))).toEqual([]);

    await sendFollowUp(harness, page, threadId, draft);
    expect(interrupts).toBe(1);
    expect(sends).toBe(2);
  });

  test(`${provider}: a lost Stop reply releases the Send gate and the turn stays stopped`, async ({ harness, page }) => {
    let requestId = '';
    let dropped = false;
    let interrupts = 0;
    await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
      const server = socket.connectToServer();
      socket.onMessage((message) => {
        const frame = JSON.parse(String(message));
        if (frame.type === 'rpc' && frame.methodId === INTERRUPT_TURN) {
          interrupts++;
          requestId = frame.id;
        }
        server.send(message);
      });
      server.onMessage((message) => {
        const frame = JSON.parse(String(message));
        if (!dropped && requestId !== '' && frame.type === 'rpc' && frame.id === requestId) {
          dropped = true;
          void socket.close({ code: 1012, reason: 'lost Stop reply' });
          void server.close();
        } else socket.send(message);
      });
    });
    const threadId = await openThread(harness, page, provider, `Stop lost reply ${provider}`);
    const input = page.getByLabel('Message Input');
    const sent = `Stop with a lost reply (${provider}).`;
    const followUp = `After the lost reply (${provider}).`;

    await input.fill(sent);
    await page.getByRole('button', { name: 'Send message', exact: true }).click();
    await waitForGate(harness, 'answer');
    const stopped = harness.waitForEvent<TurnCompleted>('provider:turn_completed', (ev) => ev.threadId === threadId);
    await page.getByRole('button', { name: 'Interrupt current turn', exact: true }).click();
    expect((await stopped).stopReason).toBe('interrupted');
    await expect.poll(() => dropped).toBe(true);

    await input.fill(followUp);
    await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeEnabled();
    await expect(page.getByTestId('user-message-bubble').filter({ hasText: sent })).toHaveCount(1);
    expect(await userRows(harness, threadId, sent)).toBe(1);
    await sendFollowUp(harness, page, threadId, followUp);
    expect(interrupts).toBe(1);
  });
}
