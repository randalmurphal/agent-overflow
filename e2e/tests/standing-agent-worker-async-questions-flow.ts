// Shared body of the async question tests in
// standing-agent-worker-prompts.spec.ts and its compact twin
// (docs/specs/standing-agents.md, "Owned workers"). An owned Codex worker
// ends its turn asking an async question, which goes to the thread that
// spawned it as its blocking prompts do: the spawner's agent is woken with
// it and answers it with thread_reply, and the answer reaches the worker as
// its next message. When the spawner's session dies before taking the
// question in, it goes to the person: it notifies, the spawner's row counts
// a worker waiting on them, and their answer withdraws both.
import type { Locator, Page } from '@playwright/test';

import { expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  advanceGate,
  awaitGate,
  awaitToolAnswer,
  awaitTurnCompleted,
  setScenario,
  threadToolsScenario,
  type ProviderName,
} from './thread-tools-helpers.js';
import { wakeInput, type NotificationSend } from './standing-agent-wake-helpers.js';
import { capture } from './wake-rows-helpers.js';
import { openAgentRow, openAgents } from './agents-tray-helpers.js';

const PROMPT_TOKEN = 'thread_reply, token (\\S+), once';

/** The Codex frame of an agent message that asks async questions. */
function asyncQuestionLine(id: string, title: string, options: string[]): string {
  return JSON.stringify({
    jsonrpc: '2.0',
    method: 'item/completed',
    params: {
      threadId: '${THREAD_ID}',
      turnId: '${TURN_ID}',
      item: { type: 'agentMessage', id, delivery: 'async', text: title, questions: [{ title, options }] },
    },
  });
}

interface AsyncQuestion {
  itemId: string;
  index: number;
  state: string;
}

// A retraction names only the notification's id.
const questionNotice = (worker: string, itemId: string) => (data: NotificationSend) =>
  data.kind === 'approval-needed' && data.id === `question:${worker}:${itemId}`;

/**
 * A Claude lead's owned Codex worker asks; the question wakes the lead, its
 * Agents row reads blocked on the question and waiting on the lead, nobody
 * is notified, and the lead's thread_reply answers it in the worker.
 */
export async function runAsyncQuestionAnsweredBySpawner(
  harness: HarnessApp,
  page: Page,
  layout: 'desktop' | 'compact',
  lead: ProviderName,
): Promise<void> {
  const tag = `async-answered-${lead}-${layout}`;
  const LEAD = 'Release lead';
  const QUESTION = 'Which environment should release 4.2 go to?';
  // A notification is sent whatever the window's focus, so the spec reads it.
  await harness.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: `${tag}-lead`, repo: {}, threads: [{ title: LEAD, provider: lead }] },
      { name: `${tag}-worker`, repo: {}, threads: [] },
    ],
  });
  const [leadP, workerP] = seed.projects;
  const leadID = leadP.threadIds[0];
  await setScenario(harness, leadP.path, threadToolsScenario({
    name: `${tag}-lead`, provider: lead, echoUserMessage: true,
    turns: [
      { steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Ship release 4.2.', title: 'Release worker', project_id: workerP.projectId, provider: 'codex' } } }], text: 'Started the release worker.' },
      {
        steps: [
          { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: PROMPT_TOKEN } },
          { gate: 'answer-question' },
          { call: { tool: 'thread_reply', args: { token: '${TOKEN}', text: 'staging' } } },
        ],
        text: 'Told it staging.',
      },
    ],
    afterTurns: 'silent',
  }));
  await setScenario(harness, workerP.path, threadToolsScenario({
    name: `${tag}-worker`, provider: 'codex', echoUserMessage: true,
    turns: [
      { steps: [{ gate: 'then-ask' }, { emitLines: [asyncQuestionLine('ask-env', QUESTION, ['staging', 'production'])] }], text: '' },
      { text: 'Deploying to staging.' },
    ],
    afterTurns: 'silent',
  }));

  await harness.open(page);
  await harness.rpc('StartSession', leadID);
  await harness.rpc('SendMessage', leadID, 'ship release 4.2', null);
  const spawn = await awaitToolAnswer<{ token: string; thread_id: string }>(harness, { tool: 'thread_spawn', cwd: leadP.path, timeoutMs: 60_000 });
  expect(spawn.isError, spawn.text).toBe(false);
  const worker = spawn.value!.thread_id;
  await awaitTurnCompleted(harness, leadID);
  const ask = await awaitGate(harness, 'then-ask', workerP.path);
  await advanceGate(harness, ask.mockId, 'then-ask');

  // ---- The question wakes the lead, as its other prompts would.
  const asked = (await wakeInput(harness, leadP.path, QUESTION)).report.input ?? '';
  expect(asked).toContain('"Release worker"');
  expect(asked).toContain(worker);
  expect(asked).toContain('option: staging');
  expect(asked).toMatch(/thread_reply, token \S+, once: your answer in text\./);
  const answerGate = await awaitGate(harness, 'answer-question', leadP.path);

  // ---- The worker's turn ended, yet its row is open: blocked on the
  // question, which waits on the lead and not on the person.
  await openAgents(page, LEAD);
  const row = openAgentRow(page, spawn.value!.token);
  await expect(row.getByTestId('agent-tray-row-state')).toHaveText('blocked', { timeout: 30_000 });
  await expect(row.getByTestId('agent-tray-row-blocked')).toHaveText(`Asks: ${QUESTION}, waiting on ${LEAD}`);
  expect(await harness.rpc<{ open: number; needYou: number }>('GetThreadWorkerSummary', leadID)).toMatchObject({ open: 1, needYou: 0 });
  expect(harness.countEvents<NotificationSend>('notification:send', (data) => !data.retract && data.target.threadId === worker)).toBe(0);
  if (layout === 'desktop') await page.mouse.move(800, 400);
  await capture(page, `async-question-${lead}-${layout}-routed.png`, row);

  // ---- The lead answers; the answer is the worker's next message.
  const reply = awaitToolAnswer<{ accepted: boolean; prompt: boolean; source_thread_id: string }>(harness, { tool: 'thread_reply', cwd: leadP.path });
  await advanceGate(harness, answerGate.mockId, 'answer-question');
  const replied = await reply;
  expect(replied.isError, replied.text).toBe(false);
  expect(replied.value).toMatchObject({ accepted: true, prompt: true, source_thread_id: worker });
  const answered = (await wakeInput(harness, workerP.path, 'Answer: staging')).report.input ?? '';
  expect(answered).toContain(`Question: ${QUESTION}`);
  await expect.poll(async () => (await harness.rpc<AsyncQuestion[]>('ListAsyncQuestions', worker, 'question:ask-env')).map((q) => q.state), { timeout: 30_000 }).toEqual(['delivered']);
  await awaitTurnCompleted(harness, worker);
  await expect(row.getByTestId('agent-tray-row-blocked')).toHaveCount(0);
  expect(harness.countEvents<NotificationSend>('notification:send', (data) => !data.retract && data.target.threadId === worker)).toBe(0);

  // ---- The worker's row shows the answer as the lead's message, all of
  // it, with no request facts about it.
  const press = (target: Locator) => (layout === 'compact' ? target.tap() : target.click());
  await press(row.getByTestId('agent-tray-row-open'));
  await expect(page.getByTestId('chat-header-title').filter({ hasText: 'Release worker' })).toBeVisible();
  const answer = page.locator('[data-testid="agent-message-row"][data-agent-message-kind="message"]');
  await expect(answer.getByTestId('agent-message-sender')).toHaveText(LEAD);
  await expect(answer.getByTestId('agent-message-first-line')).toHaveText(`Question: ${QUESTION}`);
  await press(answer.getByTestId('agent-message-toggle'));
  const body = answer.getByTestId('agent-message-body');
  await expect(body.getByTestId('agent-message-text')).toHaveText(`Question: ${QUESTION}\nAnswer: staging`);
  await expect(body.getByTestId('agent-message-facts')).toHaveCount(0);
  await answer.scrollIntoViewIfNeeded();
  await capture(page, `async-question-${lead}-${layout}-answer-row.png`, answer);
}

/**
 * A Codex lead's session dies before taking its worker's question in: the
 * question goes to the person, notifies for the owned worker, and badges
 * the lead's sidebar row and Agents row as waiting on the person, until the
 * person answers it in the worker.
 */
export async function runAsyncQuestionHandedBack(harness: HarnessApp, page: Page, layout: 'desktop' | 'compact'): Promise<void> {
  const tag = `async-handback-${layout}`;
  const LEAD = 'Ops lead';
  const QUESTION = 'When should the logs rotate?';
  // A notification is sent whatever the window's focus, so the spec reads it.
  await harness.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: `${tag}-lead`, repo: {}, threads: [{ title: LEAD, provider: 'codex' }] },
      { name: `${tag}-worker`, repo: {}, threads: [] },
    ],
  });
  const [leadP, workerP] = seed.projects;
  const leadID = leadP.threadIds[0];
  // The wake starts turn 2, which parks without echoing it.
  await setScenario(harness, leadP.path, threadToolsScenario({
    name: `${tag}-lead`, provider: 'codex', echoUserMessage: true,
    turns: [
      { steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Rotate the logs.', title: 'Ops worker', project_id: workerP.projectId, provider: 'codex' } } }], text: 'Started the ops worker.' },
      { steps: [{ gate: 'hold' }], text: 'unreached', echo: false },
    ],
    afterTurns: 'silent',
  }));
  await setScenario(harness, workerP.path, threadToolsScenario({
    name: `${tag}-worker`, provider: 'codex', echoUserMessage: true,
    turns: [
      { steps: [{ gate: 'ask' }, { emitLines: [asyncQuestionLine('ask-rotate', QUESTION, ['now', 'tonight'])] }], text: '' },
      { text: 'Rotating tonight.' },
    ],
    afterTurns: 'silent',
  }));

  await harness.open(page);
  await harness.rpc('StartSession', leadID);
  await harness.rpc('SendMessage', leadID, 'rotate the logs', null);
  const spawn = await awaitToolAnswer<{ token: string; thread_id: string }>(harness, { tool: 'thread_spawn', cwd: leadP.path, timeoutMs: 60_000 });
  expect(spawn.isError, spawn.text).toBe(false);
  const worker = spawn.value!.thread_id;
  await awaitTurnCompleted(harness, leadID);
  const ask = await awaitGate(harness, 'ask', workerP.path);
  const notice = questionNotice(worker, 'question:ask-rotate');
  const handedBack = harness.waitForEvent<NotificationSend>('notification:send', (data) => notice(data) && !data.retract, 60_000);
  await advanceGate(harness, ask.mockId, 'ask');
  await wakeInput(harness, leadP.path, QUESTION);
  const hold = await awaitGate(harness, 'hold', leadP.path);
  expect(harness.countEvents<NotificationSend>('notification:send', (data) => notice(data) && !data.retract)).toBe(0);

  // ---- The lead's session dies with the wake unconfirmed: the question is
  // the person's now, and says so.
  await harness.rpc('HarnessMockCommand', hold.mockId, { type: 'exit', code: 1 });
  const raised = await handedBack;
  expect(raised.body).toBe('Waiting for your answer');
  expect(raised.title).toBe('Ops worker');
  await expect.poll(() => harness.rpc<{ needYou: number }>('GetThreadWorkerSummary', leadID).then((s) => s.needYou), { timeout: 30_000 }).toBe(1);
  const badge = page.getByTestId('thread-row').filter({ hasText: LEAD }).getByTestId('thread-row-workers-need-you');
  await expect(badge).toHaveText('1');
  await openAgents(page, LEAD);
  const row = openAgentRow(page, spawn.value!.token);
  await expect(row.getByTestId('agent-tray-row-state')).toHaveText('blocked', { timeout: 30_000 });
  await expect(row.getByTestId('agent-tray-row-blocked')).toHaveText(`Asks: ${QUESTION}`);
  if (layout === 'desktop') await page.mouse.move(800, 400);
  await capture(page, `async-question-${layout}-handed-back.png`, row);

  // ---- The person answers in the worker: the notification is withdrawn,
  // the worker takes the answer, and nothing waits on the person.
  // Its routing to the lead retracted the question's id once already.
  const retractions = () => harness.countEvents<NotificationSend>('notification:send', (data) => notice(data) && Boolean(data.retract));
  const before = retractions();
  await harness.rpc('SubmitAsyncAnswers', worker, 'async-answer:person-rotate', [{ itemId: 'question:ask-rotate', index: 0, answer: 'tonight' }]);
  await expect.poll(retractions, { timeout: 30_000 }).toBe(before + 1);
  await wakeInput(harness, workerP.path, 'Answer: tonight');
  await awaitTurnCompleted(harness, worker);
  await expect(row.getByTestId('agent-tray-row-blocked')).toHaveCount(0);
  expect((await harness.rpc<{ needYou: number }>('GetThreadWorkerSummary', leadID)).needYou).toBe(0);
}
