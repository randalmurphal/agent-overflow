// A person's Stop on a turn an agent's message started
// (docs/specs/agent-thread-tools.md, "How agent messages appear" and
// "Wakes").
//
// Coverage, on both mock providers: Stop on a turn a request started keeps
// the request (its row stays and nothing goes to the person's composer),
// records the user's Stop as the request's cause on both of its rows,
// and the sender's wake says the person stopped the turn, with
// what it said by then, never a restart.
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { Locator, Page } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { captureAgentMessageInView } from './wake-rows-helpers.js';
import { awaitGate, awaitToolAnswer, setScenario, threadToolsScenario, type TurnStep } from './thread-tools-helpers.js';

test.beforeEach(async ({ harness }) => {
  // These assertions read agent message rows, which sit inside activity runs.
  await harness.rpc('UpdateSettings', { activityRunDefault: 'expanded' });
});

const openThread = async (page: Page, title: string) => {
  await page.getByTestId('thread-row-title').filter({ hasText: title }).first().click();
  await expect(page.getByTestId('chat-header-title').filter({ hasText: title })).toBeVisible();
};

const agentRow = (page: Page, kind: string) => page.locator(`[data-testid="agent-message-row"][data-agent-message-kind="${kind}"]`);
const stopButton = (page: Page) => page.getByRole('button', { name: 'Interrupt current turn', exact: true });
const fact = (body: Locator, label: string) => body.locator(`[data-fact="${label}"]`);

async function expand(row: Locator): Promise<Locator> {
  await row.getByTestId('agent-message-toggle').click();
  const body = row.getByTestId('agent-message-body');
  await expect(body).toBeVisible();
  return body;
}

function db<T>(harness: HarnessApp, query: string, ...args: string[]): T[] {
  const conn = new DatabaseSync(join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    return conn.prepare(query).all(...args) as T[];
  } finally {
    conn.close();
  }
}

/** The receipt's and the request's state and cause, as `state/cause` each. */
const requestState = (harness: HarnessApp, token: string) =>
  db<{ settled: string }>(harness, `SELECT r.state || '/' || r.answer_cause || ' ' || q.state || '/' || q.answer_cause AS settled
    FROM thread_request_receipts r JOIN thread_requests q ON q.token = r.token WHERE r.token = ?`, token)[0]?.settled ?? '';

const PARTIAL = 'Looked at the first half.';
const ASK = 'Please stop-test this.';

/** A Claude turn that says PARTIAL and then waits on the gate. */
function claudePartialThenHold(gate: string): TurnStep[] {
  return [
    { emitLines: [`{"type":"assistant","message":{"id":"msg-partial","role":"assistant","content":[{"type":"text","text":${JSON.stringify(PARTIAL)}}]}}`] },
    { gate },
  ];
}

for (const provider of ['claude', 'codex'] as const) {
  test(`${provider}: Stop on a turn a request started keeps the request and tells the sender the person stopped it`, async ({
    harness,
    page,
  }) => {
    test.setTimeout(180_000);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        { name: `stop-sender-${provider}`, repo: {}, threads: [{ title: 'Stop sender', provider }] },
        { name: `stop-target-${provider}`, repo: {}, threads: [{ title: 'Stop target', provider }] },
      ],
    });
    const [senderProject, targetProject] = seed.projects;
    const sender = senderProject.threadIds[0];
    const target = targetProject.threadIds[0];
    await setScenario(harness, targetProject.path, {
      ...threadToolsScenario({
        name: `stop-target-${provider}`,
        provider,
        echoUserMessage: true,
        turns: [{ steps: provider === 'claude' ? claudePartialThenHold('hold-stop') : [{ gate: 'hold-stop' }], text: 'Never gets here.' }],
        afterTurns: 'silent',
      }),
    });
    await setScenario(harness, senderProject.path, threadToolsScenario({
      name: `stop-sender-${provider}`,
      provider,
      echoUserMessage: true,
      turns: [
        { steps: [{ call: { tool: 'thread_send', args: { thread_id: target, message: ASK, wait_seconds: 0, notify: true } } }], text: 'Sent.' },
        { text: 'Read the wake.' },
      ],
      afterTurns: 'silent',
    }));
    await harness.rpc('StartSession', sender);
    await harness.rpc('SendMessage', sender, 'send it over', null);
    const send = await awaitToolAnswer<{ token: string }>(harness, { tool: 'thread_send', timeoutMs: 60_000 });
    expect(send.isError, send.text).toBe(false);
    const token = send.value!.token;
    await awaitGate(harness, 'hold-stop', targetProject.path);

    await harness.open(page);
    await openThread(page, 'Stop target');
    const request = agentRow(page, 'request');
    await expect(request).toHaveCount(1);
    await stopButton(page).click();
    await expect(stopButton(page)).toHaveCount(0, { timeout: 30_000 });
    // The request's row stays and nothing goes to the person's composer.
    await expect(request).toHaveCount(1);
    await expect(page.getByLabel('Message Input')).toHaveValue('');
    // Both rows record the user's Stop as their cause.
    await expect.poll(() => requestState(harness, token), { timeout: 30_000 }).toBe('interrupted/user_stop interrupted/user_stop');

    await openThread(page, 'Stop sender');
    const wake = agentRow(page, 'interrupted');
    await expect(wake).toHaveCount(1, { timeout: 60_000 });
    await expect(wake.getByTestId('agent-message-sender')).toHaveText('Stop target');
    const body = await expand(wake);
    await expect(fact(body, 'Stopped by')).toHaveText('the user');
    await expect(body).not.toContainText('restart');
    if (provider === 'claude') await expect(body.getByTestId('agent-message-text')).toContainText(PARTIAL);
    await captureAgentMessageInView(page, `${provider}-person-stop-expanded.png`, wake);
    const raw = db<{ summary: string }>(harness,
      `SELECT summary FROM items WHERE thread_id = ? AND kind = 'user_text' AND summary LIKE '%[token ' || ? || ']%'`, sender, token);
    expect(raw.map((row) => row.summary).join('\n')).toContain('The user stopped this turn before it answered.');
  });
}
