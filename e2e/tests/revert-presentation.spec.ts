// Real SPA, transport, store and mocked providers. Hold the preparation RPC to
// prove the Stop transition is local, and delay provider startup to prove older
// message replacement stays loading until its atomic cut/send publication.
import { test, expect, type SeedResult, type HarnessMockEvent } from './fixtures.js';
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

async function seed(harness: HarnessApp, provider: 'claude' | 'codex', history: boolean) {
  const result = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{
    name: `revert-${provider}`, repo: {}, threads: [{
      title: `Revert ${provider}`, provider,
      turns: history ? [
        { userText: 'Old question', items: [{ kind: 'assistant_text', summary: 'Old answer' }] },
        { userText: 'Later question', items: [{ kind: 'assistant_text', summary: 'Later answer' }] },
      ] : [],
    }, { title: 'Other conversation', provider, turns: [{ userText: 'Other prompt', items: [{ kind: 'assistant_text', summary: 'Other answer' }] }] }],
  }] });
  const threadId = result.projects[0].threadIds[0];
  if (!history) await harness.rpc('SaveDraft', threadId, 'First line\nSecond line', [], [], null);
  return threadId;
}

for (const provider of ['claude', 'codex'] as const) {
  test(`${provider}: Stop restores composer and chrome before preflight, gates Send, preserves editing`, async ({ harness, page }) => {
    let release: (() => void) | undefined;
    let counts = 0;
    await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
      const server = socket.connectToServer();
      socket.onMessage(message => {
        const frame = JSON.parse(String(message));
        if (frame.type === 'rpc' && frame.methodId === 2617952423) {
          counts++;
          release = () => server.send(message);
        } else server.send(message);
      });
      server.onMessage(message => socket.send(message));
    });
    await harness.rpc('HarnessSetScenario', { scenario: waitingScenario(provider) });
    const threadId = await seed(harness, provider, false);
    await harness.open(page);
    await page.getByTestId('thread-row-title').filter({ hasText: `Revert ${provider}` }).click();
    const input = page.getByLabel('Message Input');
    const original = 'First line\nSecond line';
    await input.fill(original);
    await page.getByRole('button', { name: 'Send message', exact: true }).click();
    await harness.waitForEvent<HarnessMockEvent>('harness:mock', ev => ev.report.kind === 'waiting_signal' && ev.report.detail === 'answer');
    await page.getByRole('button', { name: 'Interrupt current turn', exact: true }).click();
    await expect.poll(() => release !== undefined).toBe(true);
    // The count RPC has not reached Go. None of these outcomes can depend on it.
    await expect(input).toHaveValue(original);
    await expect(input).toBeFocused();
    expect(await input.evaluate((element: HTMLTextAreaElement) => element.selectionStart)).toBe(original.length);
    await expect(page.getByTestId('activity-rail-working')).toHaveCount(0);
    await expect(page.getByText(original, { exact: true })).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Interrupt current turn', exact: true })).toHaveCount(0);
    const send = page.getByRole('button', { name: 'Send message', exact: true });
    await expect(send).toBeDisabled();
    await input.fill(`${original}\nEdited while cleaning up`);
    await input.press('Control+Enter');
    expect(counts).toBe(1);
    await page.getByTestId('thread-row-title').filter({ hasText: 'Other conversation' }).click();
    await expect(page.getByText('Other answer', { exact: true })).toBeVisible();
    await expect(page.getByLabel('Message Input')).toHaveValue('');
    await page.getByTestId('thread-row-title').filter({ hasText: `Revert ${provider}` }).click();
    await expect(input).toHaveValue(`${original}\nEdited while cleaning up`);
    await expect(send).toBeDisabled();
    release!();
    await expect(send).toBeEnabled();
    await expect(input).toHaveValue(`${original}\nEdited while cleaning up`);
    await expect.poll(async () => (await harness.rpc<{ content: string }>('GetDraft', threadId)).content).toBe(`${original}\nEdited while cleaning up`);
    const items = await harness.rpc<Array<{ kind: string }>>('ListItems', threadId, true);
    expect(items.filter(item => item.kind === 'user_text')).toHaveLength(0);
    await page.reload();
    await page.getByTestId('thread-row-title').filter({ hasText: `Revert ${provider}` }).click();
    await expect(page.getByLabel('Message Input')).toHaveValue(`${original}\nEdited while cleaning up`);
  });

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
    const threadId = await seed(harness, provider, true);
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
}


for (const provider of ['claude', 'codex'] as const) {
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
    const threadId = await seed(harness, provider, true);
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

  test(`${provider}: a lost Stop reply preserves edits and keeps Send gated until recovery`, async ({ harness, page }) => {
    let requestId = '';
    let dropped = false;
    let revertCalls = 0;
    let releaseRead: (() => void) | undefined;
    await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
      const server = socket.connectToServer();
      socket.onMessage(message => {
        const frame = JSON.parse(String(message));
        if (frame.type === 'rpc' && frame.methodId === 753394581) {
          revertCalls++;
          requestId = frame.id;
        }
        if (frame.type === 'rpc' && frame.methodId === 3307838728) releaseRead = () => server.send(message);
        else server.send(message);
      });
      server.onMessage(message => {
        const frame = JSON.parse(String(message));
        if (!dropped && frame.type === 'rpc' && frame.id === requestId) {
          dropped = true;
          void socket.close({ code: 1012, reason: 'lost Stop reply' });
          void server.close();
        } else socket.send(message);
      });
    });
    await harness.rpc('HarnessSetScenario', { scenario: waitingScenario(provider) });
    const threadId = await seed(harness, provider, false);
    await harness.open(page);
    await page.getByTestId('thread-row-title').filter({ hasText: `Revert ${provider}` }).click();
    const composer = page.getByLabel('Message Input');
    await page.getByRole('button', { name: 'Send message', exact: true }).click();
    await harness.waitForEvent<HarnessMockEvent>('harness:mock', ev => ev.report.kind === 'waiting_signal' && ev.report.detail === 'answer');
    await page.getByRole('button', { name: 'Interrupt current turn', exact: true }).click();
    await expect.poll(() => releaseRead !== undefined).toBe(true);
    await expect(composer).toHaveValue('First line\nSecond line');
    await composer.fill('Edited while reconnecting');
    const send = page.getByRole('button', { name: 'Send message', exact: true });
    await expect(send).toBeDisabled();
    releaseRead!();
    await expect(send).toBeEnabled();
    await expect(composer).toHaveValue('Edited while reconnecting');
    await expect(page.getByTestId('activity-rail-working')).toHaveCount(0);
    await expect.poll(async () => (await harness.rpc<{ content: string }>('GetDraft', threadId)).content).toBe('Edited while reconnecting');
    const rows = await harness.rpc<Array<{ kind: string }>>('ListItems', threadId, true);
    expect(rows.filter(row => row.kind === 'user_text')).toHaveLength(0);
    expect(revertCalls).toBe(1);
  });

  test(`${provider}: Stop preserves the sent message when background tasks block revert`, async ({ harness, page }) => {
    let countId = '';
    let revertCalls = 0;
    await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
      const server = socket.connectToServer();
      socket.onMessage(message => {
        const frame = JSON.parse(String(message));
        if (frame.type === 'rpc' && frame.methodId === 2617952423) countId = frame.id;
        if (frame.type === 'rpc' && frame.methodId === 753394581) revertCalls++;
        server.send(message);
      });
      server.onMessage(message => {
        const frame = JSON.parse(String(message));
        if (frame.type === 'rpc' && frame.id === countId) socket.send(JSON.stringify({ ...frame, result: 1 }));
        else socket.send(message);
      });
    });
    await harness.rpc('HarnessSetScenario', { scenario: waitingScenario(provider) });
    const threadId = await seed(harness, provider, false);
    await harness.open(page);
    await page.getByTestId('thread-row-title').filter({ hasText: `Revert ${provider}` }).click();
    await page.getByRole('button', { name: 'Send message', exact: true }).click();
    await harness.waitForEvent<HarnessMockEvent>('harness:mock', ev => ev.report.kind === 'waiting_signal' && ev.report.detail === 'answer');
    await page.getByRole('button', { name: 'Interrupt current turn', exact: true }).click();
    await expect(page.getByRole('button', { name: 'Interrupt current turn', exact: true })).toHaveCount(0);
    await expect(page.getByTestId('user-message-bubble')).toContainText('First line');
    await expect(page.getByLabel('Message Input')).toHaveValue('');
    expect(revertCalls).toBe(0);
    const rows = await harness.rpc<Array<{ kind: string }>>('ListItems', threadId, true);
    expect(rows.filter(row => row.kind === 'user_text')).toHaveLength(1);
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
    const threadId = await seed(harness, provider, true);
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
  const scenario = { ...waitingScenario('codex'), onStart: [{ exit: { code: 1 } }] };
  await harness.rpc('HarnessSetScenario', { scenario });
  const threadId = await seed(harness, 'codex', true);
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
  await expect(page.getByRole('button', { name: 'Send message', exact: true })).toBeEnabled();
});
