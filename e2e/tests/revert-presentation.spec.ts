// Edit-and-resend presentation against the real SPA, transport, store and
// mocked providers. Hold the preparation RPC and delay provider startup to
// prove older-message replacement stays loading until its atomic cut/send
// publication; drop the replacement reply to prove Send stays gated until a
// read reconciles it; and cover the background-task confirmation and a
// startup failure after the cut.
import { test, expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';

function waitingScenario(provider: 'claude' | 'codex', startupDelayMs = 0) {
  return {
    version: 1, name: `revert-presentation-${provider}`, provider, startupDelayMs,
    turns: [{ steps: [
      ...(provider === 'codex' ? [{ emit: { lines: [JSON.stringify({
        jsonrpc: '2.0', method: 'turn/started',
        params: { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}' } },
      })] } }] : []),
      { waitSignal: { name: 'answer' } },
    ] }],
    afterTurns: 'repeatLast',
  };
}

async function seed(harness: HarnessApp, provider: 'claude' | 'codex') {
  const result = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{
    name: `revert-${provider}`, repo: {}, threads: [{
      title: `Revert ${provider}`, provider,
      turns: [
        { userText: 'Old question', items: [{ kind: 'assistant_text', summary: 'Old answer' }] },
        { userText: 'Later question', items: [{ kind: 'assistant_text', summary: 'Later answer' }] },
      ],
    }],
  }] });
  return result.projects[0].threadIds[0];
}

for (const provider of ['claude', 'codex'] as const) {
  test(`${provider}: older-message preparation retains history and WIP until the replacement is ready`, async ({ harness, page }) => {
    let release: (() => void) | undefined;
    await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
      const server = socket.connectToServer();
      socket.onMessage(message => {
        const frame = JSON.parse(String(message));
        if (frame.type === 'rpc' && frame.methodId === 2617952423) release = () => server.send(message);
        else server.send(message);
      });
      server.onMessage(message => socket.send(message));
    });
    await harness.rpc('HarnessSetScenario' , { scenario: waitingScenario(provider, 2000) });
    const threadId = await seed(harness, provider);
    await harness.open(page);
    await page.getByTestId('thread-row-title').filter({ hasText: `Revert ${provider}` }).click();
    const composer = page.getByLabel('Message Input');
    await composer.fill('Unsent work stays here');
    await expect.poll(async () => (await harness.rpc<{ content: string }>('GetDraft', threadId)).content).toBe('Unsent work stays here');
    await page.getByText('Old question', { exact: true }).hover();
    await page.getByLabel('Edit message and resend from here').first().click();
    const editor = page.getByTestId('user-message-editor');
    await editor.getByLabel('Message Input').fill('Replacement question');
    await page.getByTestId('user-message-edit-send').click();
    await expect.poll(() => release !== undefined).toBe(true);
    // Preparation is held before Go. The old history and editor stay visible.
    await expect(editor).toBeVisible();
    await expect(page.getByText('Later answer', { exact: true })).toBeVisible();
    await expect(page.getByTestId('user-message-edit-send')).toBeDisabled();
    expect((await harness.rpc<{ content: string }>('GetDraft', threadId)).content).toBe('Unsent work stays here');
    await page.evaluate(() => {
      const state = { frames: [] as Array<{ editor: boolean; replacement: number; later: boolean }>, running: true };
      (window as any).__revertFrames = state;
      const sample = () => {
        const bubbles = [...document.querySelectorAll('[data-testid="user-message-bubble"]')];
        state.frames.push({ editor: !!document.querySelector('[data-testid="user-message-editor"]'),
          replacement: bubbles.filter(node => !node.querySelector('textarea') && node.textContent?.includes('Replacement question')).length,
          later: document.querySelector('[data-testid="message-timeline-scroll"]')?.textContent?.includes('Later answer') ?? false });
        if (state.running) requestAnimationFrame(sample);
      };
      requestAnimationFrame(sample);
    });
    release!();
    await expect(page.getByText('Replacement question', { exact: true })).toBeVisible();
    await expect(editor).toHaveCount(0);
    await expect(page.getByText('Later answer', { exact: true })).toHaveCount(0);
    await expect(page.getByLabel('Message Input')).toHaveValue('Unsent work stays here');
    const items = await harness.rpc<Array<{ kind: string; summary: string }>>('ListItems', threadId, true);
    expect(items.filter(item => item.kind === 'user_text').map(item => item.summary)).toEqual(['Replacement question']);
    const frames = await page.evaluate(async () => {
      await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
      const state = (window as any).__revertFrames;
      state.running = false;
      return state.frames as Array<{ editor: boolean; replacement: number; later: boolean }>;
    });
    expect(frames.some(frame => frame.replacement === 1)).toBe(true);
    expect(frames.every(frame => frame.editor || frame.replacement === 1)).toBe(true);
    expect(frames.every(frame => frame.replacement <= 1)).toBe(true);
    expect(frames.filter(frame => frame.replacement === 1).every(frame => !frame.later)).toBe(true);
  });

  test(`${provider}: a lost replacement reply waits for reconciliation without resending`, async ({ harness, page }) => {
    let requestId = '';
    let dropped = false;
    let replacementCalls = 0;
    let releaseRead: (() => void) | undefined;
    await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
      const server = socket.connectToServer();
      socket.onMessage(message => {
        const frame = JSON.parse(String(message));
        if (frame.type === 'rpc' && frame.methodId === 2059566413) {
          replacementCalls++;
          requestId = frame.id;
        }
        if (frame.type === 'rpc' && frame.methodId === 3307838728) {
          releaseRead = () => server.send(message);
        } else server.send(message);
      });
      server.onMessage(message => {
        const frame = JSON.parse(String(message));
        if (!dropped && frame.type === 'rpc' && frame.id === requestId) {
          dropped = true;
          void socket.close({ code: 1012, reason: 'lost replacement reply' });
          void server.close();
          return;
        }
        socket.send(message);
      });
    });
    await harness.rpc('HarnessSetScenario', { scenario: waitingScenario(provider) });
    const threadId = await seed(harness, provider);
    await harness.open(page);
    await page.getByTestId('thread-row-title').filter({ hasText: `Revert ${provider}` }).click();
    await page.getByLabel('Message Input').fill('Unsent follow-up');
    await page.getByText('Old question', { exact: true }).hover();
    await page.getByLabel('Edit message and resend from here').first().click();
    await page.getByTestId('user-message-editor').getByLabel('Message Input').fill('Replacement after reconnect');
    await page.getByTestId('user-message-edit-send').click();
    await expect.poll(() => releaseRead !== undefined).toBe(true);
    await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeDisabled();
    releaseRead!();
    await expect(page.getByText('Replacement after reconnect', { exact: true })).toBeVisible();
    await expect(page.getByTestId('user-message-editor')).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeEnabled();
    await expect(page.getByLabel('Message Input')).toHaveValue('Unsent follow-up');
    expect(replacementCalls).toBe(1);
    const rows = await harness.rpc<Array<{ kind: string; summary: string }>>('ListItems', threadId, true);
    expect(rows.filter(row => row.kind === 'user_text').map(row => row.summary)).toEqual(['Replacement after reconnect']);
  });

  test(`${provider}: background-task confirmation keeps the editor and cancellation preserves history`, async ({ harness, page }) => {
    let countId = '';
    let resendCalls = 0;
    await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
      const server = socket.connectToServer();
      socket.onMessage(message => {
        const frame = JSON.parse(String(message));
        if (frame.type === 'rpc' && frame.methodId === 2617952423) countId = frame.id;
        if (frame.type === 'rpc' && frame.methodId === 2059566413) resendCalls++;
        server.send(message);
      });
      server.onMessage(message => {
        const frame = JSON.parse(String(message));
        // Exercise the UI's positive preflight result; the backend's independent
        // running-task and consent guards have store/provider integration tests.
        if (frame.type === 'rpc' && frame.id === countId) socket.send(JSON.stringify({ ...frame, result: 1 }));
        else socket.send(message);
      });
    });
    const threadId = await seed(harness, provider);
    await harness.open(page);
    await page.getByTestId('thread-row-title').filter({ hasText: `Revert ${provider}` }).click();
    await page.getByText('Old question', { exact: true }).hover();
    await page.getByLabel('Edit message and resend from here').first().click();
    await page.getByTestId('user-message-editor').getByLabel('Message Input').fill('Keep this edit');
    await page.getByTestId('user-message-edit-send').click();
    const dialog = page.getByRole('dialog');
    await expect(dialog).toContainText('1 running background task');
    await dialog.getByRole('button', { name: 'Cancel', exact: true }).click();
    await expect(page.getByTestId('user-message-editor').getByLabel('Message Input')).toHaveValue('Keep this edit');
    await expect(page.getByText('Later answer', { exact: true })).toBeVisible();
    expect(resendCalls).toBe(0);
    const rows = await harness.rpc<Array<{ kind: string; summary: string }>>('ListItems', threadId, true);
    expect(rows.filter(row => row.kind === 'user_text').map(row => row.summary)).toEqual(['Old question', 'Later question']);
  });
}

test('Codex startup failure after the cut recovers the edit with composer WIP', async ({ harness, page }) => {
  const scenario = {
    ...waitingScenario('codex'),
    codex: { responses: {
      initialize: '{"jsonrpc":"2.0","id":${REQUEST_ID},"error":{"code":-32000,"message":"Injected startup failure"}}',
    } },
  };
  await harness.rpc('HarnessSetScenario', { scenario });
  const threadId = await seed(harness, 'codex');
  await harness.open(page);
  await page.getByTestId('thread-row-title').filter({ hasText: 'Revert codex' }).click();
  await page.getByLabel('Message Input').fill('Existing draft');
  await page.getByText('Old question', { exact: true }).hover();
  await page.getByLabel('Edit message and resend from here').first().click();
  await page.getByTestId('user-message-editor').getByLabel('Message Input').fill('Recover this replacement');
  await page.getByTestId('user-message-edit-send').click();
  await expect(page.getByTestId('user-message-editor')).toHaveCount(0);
  await expect(page.getByLabel('Message Input')).toHaveValue('Recover this replacement\n\nExisting draft');
  await expect.poll(async () => (await harness.rpc<{ content: string }>('GetDraft', threadId)).content).toBe('Recover this replacement\n\nExisting draft');
  const rows = await harness.rpc<Array<{ kind: string }>>('ListItems', threadId, true);
  expect(rows.filter(row => row.kind === 'user_text')).toEqual([]);
  await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeEnabled();
});
