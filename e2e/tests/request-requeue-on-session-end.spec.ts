// A thread_send request the target's CLI was handed and had not taken in (no
// `started`, no echo) is not lost when the target's session ends: the request
// goes back on the target's queue, owed again, and the next session start
// delivers it. Its sender hears nothing until the answer, which arrives once
// (docs/specs/agent-thread-tools.md, settlement). Until then the stopped
// thread reads as idle: a queued message counts as pending only while a
// session is live or starting. Two ends: the person's Stop followed by a
// start, and a death the app reconnects. Claude only: its CLI holds a
// message queued into a running turn (claude.holdQueuedInput), which is what
// leaves the request handed over and not taken in; the Codex mock has no
// such hold. The same idle reading on
// both providers is covered by a held wake whose start keeps failing
// (standing-agent-wakes.spec.ts, "a later fire retries a start that failed").
// A thread that cannot take the request back, an archived one, settles it
// instead, and the sender's row says the session ended before the thread
// read it. Every sentence a request ends on when it was not answered reads
// as an interrupted row naming what stopped it, on both providers.
import type { Locator, Page } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { advanceGate, awaitGate, awaitToolAnswer, plainScenario, setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import { goTemplate, type FlushedEvent } from './agent-message-rows-helpers.js';
import { captureAgentMessage, captureAgentMessageInView } from './wake-rows-helpers.js';
import { agentsBody, openAgents } from './agents-tray-helpers.js';

const REQUEST = 'Look at the flaky test as well.';
const ANSWER = 'The flaky test waits on a timer.';

/**
 * A sender's thread_send to a target whose turn is held, the request flushed
 * to the target's CLI and held there. The target's turn ends with `end`
 * when released. Returns the threads, the target's workspace, the request's
 * token and the held mock.
 */
async function holdRequest(harness: HarnessApp, tag: string, end: 'answer' | 'exit') {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: `${tag}-sender`, repo: {}, threads: [{ title: 'Sender', provider: 'claude' }] },
      { name: `${tag}-target`, repo: {}, threads: [{ title: 'Held worker', provider: 'claude' }] },
    ],
  });
  const [senderProject, targetProject] = seed.projects;
  const sender = senderProject.threadIds[0];
  const target = targetProject.threadIds[0];
  const targetScript = threadToolsScenario({
    name: `${tag}-target-script`,
    provider: 'claude',
    turns: [{ steps: [{ gate: 'hold-target' }], text: 'Still on it.' }],
    afterTurns: 'silent',
  }) as { turns: { steps: unknown[] }[] };
  if (end === 'exit') {
    // The process dies when released, before the turn ends and before the
    // CLI took the held request in.
    targetScript.turns[0].steps = [targetScript.turns[0].steps[0], { exit: { code: 1, flushDelayMs: 50 } }];
  }
  await setScenario(harness, targetProject.path, { ...targetScript, claude: { holdQueuedInput: true } });
  await setScenario(
    harness,
    senderProject.path,
    threadToolsScenario({
      name: `${tag}-sender-script`,
      provider: 'claude',
      turns: [
        {
          steps: [{ call: { tool: 'thread_send', args: { thread_id: target, message: REQUEST, wait_seconds: 0, notify: true } } }],
          text: 'Sent it.',
        },
        { text: 'Noted.' },
      ],
      afterTurns: 'silent',
    }),
  );
  await harness.rpc('StartSession', target);
  await harness.rpc('SendMessage', target, 'start on the first task', null);
  const held = await awaitGate(harness, 'hold-target', targetProject.path);
  await harness.rpc('StartSession', sender);
  await harness.rpc('SendMessage', sender, 'pass that on', null);
  const send = await awaitToolAnswer<{ token: string }>(harness, { tool: 'thread_send', timeoutMs: 60_000 });
  expect(send.isError, send.text).toBe(false);
  const token = send.value?.token ?? '';
  expect(token).not.toBe('');
  await harness.waitForEvent<FlushedEvent>(
    'provider:queue_flushed',
    (data) => data.threadId === target && data.items.some((item) => item.message.includes(REQUEST)),
    60_000,
  );
  return { sender, target, targetPath: targetProject.path, token, held };
}

type RequestState = { token: string; state: string; wakeItemId?: string };

async function requestState(harness: HarnessApp, sender: string, token: string): Promise<string> {
  const states = await harness.rpc<RequestState[]>('GetThreadRequestStates', sender, [token]);
  return states.find((state) => state.token === token)?.state ?? '';
}

/** The request's answer reached the sender once, and nothing said it was cut. */
async function expectAnsweredOnce(harness: HarnessApp, page: Page, sender: string, token: string) {
  await expect.poll(() => requestState(harness, sender, token), { timeout: 60_000 }).toBe('finished');
  await page.getByTestId('thread-row-title').filter({ hasText: 'Sender' }).first().click();
  const wakes = page.getByTestId('agent-message-row').filter({ hasText: ANSWER });
  await expect(wakes).toHaveCount(1);
  await expect(page.getByTestId('message-timeline-scroll').getByText(/session ended|Stopped by/)).toHaveCount(0);
}

test('claude: a Stop gives back a request the CLI had not taken in, and the next start delivers it', async ({ harness, page }) => {
  const { sender, target, targetPath, token } = await holdRequest(harness, 'requeue-stop', 'answer');
  await harness.open(page);

  await harness.rpc('StopSession', target);
  // Back on the target's queue, owed again: accepted, and the sender told
  // nothing.
  await expect.poll(() => requestState(harness, sender, token), { timeout: 15_000 }).toBe('accepted');
  // The target lists it as incoming, from its sender, and its row is gone
  // from the timeline until it is delivered again.
  await openAgents(page, 'Held worker');
  const incoming = agentsBody(page).getByTestId('agent-incoming-row');
  await expect(incoming).toHaveCount(1);
  await expect(incoming.getByTestId('agent-incoming-row-sender')).toHaveText('from Sender');
  await expect(page.getByTestId('agent-message-row').filter({ hasText: REQUEST })).toHaveCount(0);
  // It waits for the next start: nothing is working on it, so the thread
  // reads as idle in the sidebar and in its composer.
  await expect(
    page.getByTestId('thread-row').filter({ hasText: 'Held worker' }).locator('[data-testid="thread-row-status-dot"][aria-label="Working"]'),
  ).toHaveCount(0);
  await expect(page.getByTestId('activity-rail-hairline')).toHaveCount(0);
  await captureAgentMessage(page, 'claude-requeued-request-incoming.png', incoming);

  await setScenario(harness, targetPath, plainScenario({ name: 'requeue-stop-next', provider: 'claude', texts: [ANSWER], afterTurns: 'silent' }));
  await harness.rpc('StartSession', target);
  await expect(page.getByTestId('message-timeline-scroll').getByText(ANSWER)).toBeVisible({ timeout: 60_000 });
  await expect(page.getByTestId('agent-message-row').filter({ hasText: REQUEST })).toHaveCount(1);
  await expect(incoming).toHaveCount(0);
  await expectAnsweredOnce(harness, page, sender, token);
});

test('claude: a session death gives back a request the CLI had not taken in, and the reconnect delivers it', async ({ harness, page }) => {
  const { sender, target, targetPath, token, held } = await holdRequest(harness, 'requeue-death', 'exit');
  await harness.open(page);
  await page.getByTestId('thread-row-title').filter({ hasText: 'Held worker' }).first().click();

  // The reconnect's process answers; the one that dies does not.
  await setScenario(harness, targetPath, plainScenario({ name: 'requeue-death-next', provider: 'claude', texts: [ANSWER], afterTurns: 'silent' }));
  const died = harness.waitForEvent<{ threadId: string }>('provider:session_died', (data) => data.threadId === target, 30_000);
  await advanceGate(harness, held.mockId, 'hold-target');
  await died;

  await expect(page.getByTestId('message-timeline-scroll').getByText(ANSWER)).toBeVisible({ timeout: 60_000 });
  await expect(page.getByTestId('agent-message-row').filter({ hasText: REQUEST })).toHaveCount(1);
  await expectAnsweredOnce(harness, page, sender, token);
});

const agentRow = (page: Page, kind: string) => page.locator(`[data-testid="agent-message-row"][data-agent-message-kind="${kind}"]`);
const fact = (body: Locator, label: string) => body.locator(`[data-fact="${label}"]`);

async function expand(row: Locator): Promise<Locator> {
  await row.getByTestId('agent-message-toggle').click();
  const body = row.getByTestId('agent-message-body');
  await expect(body).toBeVisible();
  return body;
}

test('claude: archiving a thread holding a request it had not taken in tells the sender it was not read', async ({ harness, page }) => {
  const { sender, target, token } = await holdRequest(harness, 'requeue-archive', 'answer');
  await harness.rpc('ArchiveThread', target);
  await expect.poll(() => requestState(harness, sender, token), { timeout: 30_000 }).toBe('interrupted');

  await harness.open(page);
  await page.getByTestId('thread-row-title').filter({ hasText: 'Sender' }).first().click();
  const wake = agentRow(page, 'interrupted');
  await expect(wake).toHaveCount(1, { timeout: 60_000 });
  await expect(wake.getByTestId('agent-message-sender')).toHaveText('Held worker');
  const body = await expand(wake);
  await expect(fact(body, 'Stopped by')).toHaveText('the session ended before it read the request');
  await expect(page.getByTestId('message-timeline-scroll').getByText(/\[token /)).toHaveCount(0);
});

// What the sender's row says for each way a request ends unanswered, from
// the Go templates (internal/threadtools/testdata/agent_messages.json).
const UNANSWERED: { fixture: string; stoppedBy: string; text?: string; shot: string }[] = [
  { fixture: 'interrupted by its session ending before it read the request', stoppedBy: 'the session ended before it read the request', shot: 'session-ended-unread' },
  { fixture: 'interrupted by its session ending before it answered the request', stoppedBy: 'the session ended before it answered the request', shot: 'session-ended-unanswered' },
  {
    fixture: 'interrupted by its session ending during the turn, with what it wrote',
    stoppedBy: 'the session ended during the turn that read the request',
    text: 'Half done.\nSecond line.',
    shot: 'session-ended-mid-turn',
  },
  { fixture: 'interrupted by the thread not taking it in', stoppedBy: 'the thread did not take it in', shot: 'not-taken-in' },
  {
    fixture: "interrupted by the thread not taking it in, its message back in that thread's composer",
    stoppedBy: "the thread did not take it in, and its message went back to that thread's composer",
    shot: 'not-taken-in-composer',
  },
];

for (const provider of ['claude', 'codex'] as const) {
  test(`${provider}: each way a request ends unanswered reads as an interrupted row naming what stopped it`, async ({ harness, page }) => {
    const turns = [];
    for (const [i, row] of UNANSWERED.entries()) {
      turns.push({
        userText: await goTemplate(row.fixture),
        userMeta: { origin: 'agent-thread', originThread: { threadId: 't-work', title: 'Worker', kind: 'wake', token: `tok-${i}` }, sendId: `thread-wake:tok-${i}` },
        items: [{ kind: 'assistant_text', summary: `Noted ${i}.` }],
      });
    }
    await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `unanswered-${provider}`, repo: {}, threads: [{ title: 'Lead', provider, turns }] }],
    });
    await harness.open(page);
    await page.getByTestId('thread-row-title').filter({ hasText: 'Lead' }).first().click();
    const rows = agentRow(page, 'interrupted');
    await expect(rows).toHaveCount(UNANSWERED.length);
    for (const [i, want] of UNANSWERED.entries()) {
      const row = rows.nth(i);
      await expect(row.getByTestId('agent-message-sender')).toHaveText('Worker');
      const body = await expand(row);
      await expect(fact(body, 'Stopped by')).toHaveText(want.stoppedBy);
      if (want.text) await expect(body.getByTestId('agent-message-text')).toHaveText(want.text);
      else await expect(body.getByTestId('agent-message-text')).toHaveCount(0);
      await captureAgentMessageInView(page, `${provider}-${want.shot}.png`, row);
      await row.getByTestId('agent-message-toggle').click();
    }
  });
}
