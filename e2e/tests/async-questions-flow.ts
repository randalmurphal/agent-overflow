// Structured Codex questions: immediate rendering, stable drafts, grouped calls,
// mid-turn answers, late answers, and frontend reconnect on desktop and compact.
import { test, expect } from './fixtures.js';
import { advance, emit, seedAgentThread, startMock, waitForGate } from './agent-visibility-helpers.js';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { mkdtemp, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';

const frame = (method: string, params: Record<string, unknown>) => JSON.stringify({ jsonrpc: '2.0', method, params: { threadId: '${THREAD_ID}', turnId: '${TURN_ID}', ...params } });
const question = (method: string, id: string, questions: unknown[]) => frame(method, { item: { type: 'agentMessage', id, delivery: 'async', text: 'This prose must never appear.', questions } });
const first = [{ title: 'Which scope?', options: ['One', 'Two'] }];
const second = [{ title: 'When should it run?' }, { title: 'Which format?', options: ['Text', 'JSON'] }];
const echo = frame('item/completed', { item: { type: 'userMessage', id: 'user-${TURN}', clientId: '${CLIENT_ID}', content: [{ type: 'text', text: '${USER_INPUT}' }] } });
const started = frame('turn/started', { turn: { id: '${TURN_ID}' } });
const completed = frame('turn/completed', { turn: { id: '${TURN_ID}', status: 'completed' } });

interface Question { itemId: string; index: number; state: string; answer: string; userItemId: string }

export function asyncQuestionsFlow(): void {
  test('abrupt backend loss preserves pending questions and restores unsent answers once', async ({ page }) => {
    const root = await mkdtemp(join(tmpdir(), 'ao-question-crash-'));
    let host: HarnessApp | undefined;
    try {
      host = await launchHarness({ dataDir: root });
      await host.rpc('HarnessSetScenario', { scenario: {
        version: 1, name: 'question-crash', provider: 'codex', afterTurns: 'silent',
        turns: [{ steps: [emit([started, echo, question('item/started', 'ask-one', first), question('item/started', 'ask-two', second)]), { waitSignal: { name: 'hold-crash' } }] }],
      } });
      const thread = await seedAgentThread(host, 'question-crash', 'Crash recovery questions', 'codex');
      await host.open(page);
      await page.getByTestId('thread-row').click();
      const mock = await startMock(host, thread);
      await host.rpc('SendMessage', thread, 'Ask while working', null);
      await waitForGate(host, 'hold-crash');
      await page.getByLabel('Message Input').fill('A separate draft');
      await expect.poll(async () => (await host!.rpc<{ content: string }>('GetDraft', thread)).content).toBe('A separate draft');
      const mocks = await host.rpc<Array<{ mockId: string; registration: { pid: number } }>>('HarnessListMocks');
      const pid = mocks.find(m => m.mockId === mock)?.registration.pid;
      expect(pid).toBeGreaterThan(0);
      process.kill(pid!, 'SIGSTOP');
      const picker = page.getByTestId('async-question-picker');
      await picker.getByLabel('Your answer').fill('Two');
      await picker.getByRole('button', { name: 'Send answered (1)' }).click();
      await expect.poll(async () => (await host!.rpc<Question[]>('ListAsyncQuestions', thread, 'question:ask-one'))[0]?.state).toBe('submitted');
      expect(await host.crash()).toBe(true);
      host = undefined;
      await page.goto('about:blank');
      host = await launchHarness({ dataDir: root });
      await host.open(page);
      await page.getByTestId('thread-row').click();
      const restored = 'Question: Which scope?\nAnswer: Two\n\nA separate draft';
      await expect(page.getByLabel('Message Input')).toHaveValue(restored);
      await expect(picker).toContainText('When should it run?');
      await expect(picker.getByTestId('async-question-position')).toHaveText('1 / 2');
      expect((await host.rpc<Question[]>('ListAsyncQuestions', thread, 'question:ask-one'))[0].state).toBe('restored');
      expect(await host.stop()).toBe(true);
      host = undefined;
      await page.goto('about:blank');
      host = await launchHarness({ dataDir: root });
      expect((await host.rpc<{ content: string }>('GetDraft', thread)).content).toBe(restored);
      expect((await host.rpc<Question[]>('ListAsyncQuestions', thread, '')).map(q => q.state)).toEqual(['unanswered', 'unanswered']);
    } finally {
      await page.goto('about:blank');
      await host?.close();
      await rm(root, { recursive: true, force: true });
    }
  });

  test('blocking questions survive reload, accept multiple answers, and cancel on interrupt', async ({ harness, page }) => {
    const request = (id: number, questions: unknown[]) => JSON.stringify({ jsonrpc: '2.0', id, method: 'item/tool/requestUserInput', params: {
      threadId: '${THREAD_ID}', turnId: '${TURN_ID}', itemId: `blocking-${id}`, isBlocking: true, autoResolutionMs: 1, questions,
    } });
    const questions = [
      { id: 'scope', header: 'Scope', question: 'Choose the blocking scope', options: [{ label: 'Small', description: 'One folder' }, { label: 'All', description: 'Every folder' }] },
      { id: 'timing', header: 'Timing', question: 'Choose the blocking timing', options: [{ label: 'Now', description: 'Immediately' }, { label: 'Later', description: 'After review' }] },
    ];
    await harness.rpc('HarnessSetScenario', { scenario: {
      version: 1, name: 'blocking-question-lifecycle', provider: 'codex', afterTurns: 'silent',
      turns: [{ steps: [emit([started, echo, request(9001, questions)]), { waitSignal: { name: 'next-blocking' } },
        emit([request(9002, questions.slice(0, 1)), question('item/started', 'async-survivor', first)]), { waitSignal: { name: 'blocked-again' } },
        emit([frame('serverRequest/resolved', { requestId: 9002 })]), { waitSignal: { name: 'provider-resolved' } },
        emit([request(9003, questions.slice(0, 1))]), { waitSignal: { name: 'interrupt-blocking' } }, emit([completed]) ] }],
    } });
    const thread = await seedAgentThread(harness, 'blocking-questions', 'Blocking questions', 'codex');
    await harness.open(page);
    await page.getByTestId('thread-row').click();
    const mock = await startMock(harness, thread);
    await harness.rpc('SendMessage', thread, 'Ask before continuing', null);
    await waitForGate(harness, 'next-blocking');
    const panel = page.getByTestId('composer-pending-user-input');
    await expect(panel).toContainText('Choose the blocking scope');
    await page.reload();
    if (test.info().project.name === 'compact') await page.getByTestId('thread-row').click();
    await expect(panel).toContainText('Choose the blocking scope');
    const resolved = harness.waitForEvent<{ action: string; requestId: string }>('provider:user_input', event => event.action === 'resolve' && event.requestId === '9001');
    await panel.getByTestId('user-input-option-2').click();
    await panel.getByTestId('user-input-header-next').click();
    await expect(panel).toContainText('Choose the blocking timing');
    await panel.getByTestId('user-input-option-1').click();
    await panel.getByTestId('user-input-submit').click();
    await resolved;
    await expect(panel).toHaveCount(0);
    await advance(harness, mock, 'next-blocking');
    await waitForGate(harness, 'blocked-again');
    await expect(panel).toBeVisible();
    await expect(page.getByTestId('async-question-picker')).toBeVisible();
    await advance(harness, mock, 'blocked-again');
    await waitForGate(harness, 'provider-resolved');
    await expect(panel).toHaveCount(0);
    await expect(page.getByTestId('async-question-picker')).toContainText('Which scope?');
    await advance(harness, mock, 'provider-resolved');
    await waitForGate(harness, 'interrupt-blocking');
    await expect(panel).toBeVisible();
    await page.getByLabel('Message Input').focus();
    await page.keyboard.press('Escape');
    await expect(panel).toHaveCount(0);
    await expect(page.getByTestId('async-question-picker')).toContainText('Which scope?');
    expect((await harness.rpc<Question[]>('ListAsyncQuestions', thread, '')).map(q => q.state)).toEqual(['unanswered']);
  });

  test('questions arrive immediately and retain independent answers through work and reload', async ({ harness, page }) => {
    await harness.rpc('HarnessSetScenario', { scenario: {
      version: 1, name: 'async-questions', provider: 'codex', afterTurns: 'repeatLast',
      turns: [
        { steps: [
          emit([started, echo, question('item/started', 'ask-one', first)]),
          { waitSignal: { name: 'append-questions' } },
          emit([question('item/completed', 'ask-one', first), question('item/started', 'ask-two', second), question('item/completed', 'ask-two', second)]),
          { waitSignal: { name: 'finish' } },
          emit([completed]),
        ] },
        { steps: [emit([started, frame('item/completed', { item: { type: 'userMessage', id: 'late-${TURN}', clientId: '${CLIENT_ID}', content: [{ type: 'text', text: 'Question: Which format?\nAnswer: JSON with comments' }] } }), completed])] },
      ],
    } });
    const thread = await seedAgentThread(harness, 'question-test', 'Agent questions', 'codex');
    await harness.open(page);
    await page.getByTestId('thread-row').click();
    const mock = await startMock(harness, thread);
    await harness.rpc('SendMessage', thread, 'Start working', null);
    await waitForGate(harness, 'append-questions');
    const picker = page.getByTestId('async-question-picker');
    await expect(picker).toContainText('Which scope?');
    await expect(page.getByTestId('async-question-card')).toHaveCount(1);
    await expect(page.getByText('This prose must never appear.', { exact: true })).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Interrupt current turn', exact: true })).toBeVisible();
    await page.getByLabel('Message Input').fill('An unrelated composer draft');
    await picker.getByLabel('Your answer').fill('Two, with extra detail');
    await advance(harness, mock, 'append-questions');
    await waitForGate(harness, 'finish');
    await expect(picker.getByTestId('async-question-position')).toHaveText('1 / 3');
    await expect(picker.getByLabel('Your answer')).toHaveValue('Two, with extra detail');
    await expect(picker.getByLabel('Your answer')).toBeFocused();
    await expect(page.getByTestId('async-question-card')).toHaveCount(2);
    await picker.getByRole('button', { name: 'Next', exact: true }).click();
    await picker.getByLabel('Your answer').fill('明日\nafter lunch');
    await picker.getByRole('button', { name: 'Send answered (2)' }).click();
    await expect.poll(async () => (await harness.rpc<Question[]>('ListAsyncQuestions', thread, 'question:ask-one'))[0]?.state).toBe('delivered');
    await expect(page.getByLabel('Message Input')).toHaveValue('An unrelated composer draft');
    await expect(picker).toContainText('Which format?');
    const items = await harness.rpc<Array<{ kind: string; summary: string }>>('ListItems', thread, true);
    expect(items.filter(i => i.kind === 'user_text').map(i => i.summary)).toEqual(['set the stage', 'Start working', 'Question: Which scope?\nAnswer: Two, with extra detail\n\nQuestion: When should it run?\nAnswer: 明日\nafter lunch']);
    await picker.getByLabel('Your answer').fill('JSON with comments');
    await advance(harness, mock, 'finish');
    await harness.waitForEvent('provider:turn_completed');
    await page.reload();
    if (test.info().project.name === 'compact') await page.getByTestId('thread-row').click();
    await expect(picker.getByLabel('Your answer')).toHaveValue('JSON with comments');
    await picker.getByRole('button', { name: 'Dismiss question' }).click();
    await expect(picker).toHaveCount(0);
    await page.getByRole('button', { name: 'Open question', exact: true }).click();
    await expect(picker.getByLabel('Your answer')).toHaveValue('JSON with comments');
    await picker.getByRole('button', { name: 'Send answered (1)' }).click();
    await expect.poll(() => harness.rpc<Question[]>('ListAsyncQuestions', thread, '')).toEqual([]);
    await expect(picker).toHaveCount(0);
    const group = await harness.rpc<Question[]>('ListAsyncQuestions', thread, 'question:ask-two');
    expect(group.map(q => q.state)).toEqual(['delivered', 'delivered']);
    expect(group.every(q => q.userItemId)).toBe(true);
  });

  test('retrying a lost answer acknowledgement delivers one user message', async ({ harness, page }) => {
    const sendIds: string[] = [];
    let dropped = false;
    await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
      const server = socket.connectToServer();
      let pending: string | undefined;
      socket.onMessage(message => {
        const value = JSON.parse(String(message));
        if (value.type === 'rpc' && value.methodId === 1219107798) {
          sendIds.push(value.params[1]);
          if (!dropped) pending = value.id;
        }
        server.send(message);
      });
      server.onMessage(message => {
        const value = JSON.parse(String(message));
        if (pending !== undefined) {
          if (value.type === 'rpc' && value.id === pending) {
            expect(value.error).toBeUndefined();
            dropped = true;
            void socket.close({ code: 1012, reason: 'lost question answer acknowledgement' });
            void server.close();
          }
          return;
        }
        socket.send(message);
      });
    });
    await harness.rpc('HarnessSetScenario', { scenario: {
      version: 1, name: 'question-answer-retry', provider: 'codex', afterTurns: 'silent',
      turns: [{ steps: [emit([started, echo, question('item/started', 'ask-one', first), question('item/completed', 'ask-one', first)]), { waitSignal: { name: 'finish-retry' } }, emit([completed])] }],
    } });
    const thread = await seedAgentThread(harness, 'retry-test', 'Retry question answer', 'codex');
    await harness.open(page);
    await page.getByTestId('thread-row').click();
    const mock = await startMock(harness, thread);
    await harness.rpc('SendMessage', thread, 'Start working', null);
    await waitForGate(harness, 'finish-retry');
    const picker = page.getByTestId('async-question-picker');
    await picker.getByLabel('Your answer').fill('Two');
    await picker.getByRole('button', { name: 'Send answered (1)' }).click();
    await expect.poll(() => dropped).toBe(true);
    await expect.poll(async () => (await harness.rpc<Question[]>('ListAsyncQuestions', thread, 'question:ask-one'))[0]?.state).toBe('delivered');
    await picker.getByRole('button', { name: 'Retry sending answers' }).click();
    await expect(picker).toHaveCount(0);
    expect(sendIds).toHaveLength(2);
    expect(new Set(sendIds).size).toBe(1);
    const history = await harness.rpc<Array<{ summary: string }>>('GetThreadUserMessageHistory', thread, 20);
    expect(history.filter(row => row.summary === 'Question: Which scope?\nAnswer: Two')).toHaveLength(1);
    await advance(harness, mock, 'finish-retry');
    await harness.waitForEvent('provider:turn_completed');
  });
}
