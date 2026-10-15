// A thread_send request the target's CLI was handed and had not taken in (no
// `started`, no echo) is not lost when the target's session ends: the request
// goes back on the target's queue, owed again, and the next session start
// delivers it. Its sender hears nothing until the answer, which arrives once
// (docs/specs/agent-thread-tools.md, settlement). Until then the stopped
// thread reads as idle: a queued message counts as pending only while a
// session is live or starting. Two ends: the person's Stop followed by a
// start, and a death the app reconnects. Claude only: its CLI holds a
// message queued into a running turn (claude.holdQueuedInput), which is what
// leaves the request handed over and not taken in; a Codex steer and a
// Codex turn's own message are taken in at once. The same idle reading on
// both providers is covered by a held wake whose start keeps failing
// (standing-agent-wakes.spec.ts, "a later fire retries a start that failed").
import { test, expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { advanceGate, awaitGate, awaitToolAnswer, plainScenario, setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import type { FlushedEvent } from './agent-message-rows-helpers.js';
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
async function expectAnsweredOnce(harness: HarnessApp, page: import('@playwright/test').Page, sender: string, token: string) {
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
