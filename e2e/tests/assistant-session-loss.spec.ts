// An assistant whose provider session the provider cleaned up
// (docs/specs/standing-agents.md, "Next thread"), end to end on both mock
// providers.
//
// Coverage: a resume of an assistant's current thread finds the session
// gone (Claude: the transcript file is missing; Codex: thread/resume finds
// no rollout); the app moves the assistant to a new thread on its own; the
// successor's opening block says the provider cleaned up the previous
// thread's session and that the last message may not have arrived, and
// reaches the provider; the pane follows to the successor and the previous
// thread sits in the done footer. A plain thread whose session is gone is
// not moved anywhere.
import { existsSync, rmSync } from 'node:fs';
import { join } from 'node:path';

import { test, expect, type SeedResult } from './fixtures.js';
import { captureAgentMessage } from './wake-rows-helpers.js';
import { awaitTurnCompleted, plainScenario, setScenario, threadRows, type ProviderName } from './thread-tools-helpers.js';

const NO_ROLLOUT = '{"jsonrpc":"2.0","id":${REQUEST_ID},"error":{"code":-32600,"message":"no rollout found for thread id ${THREAD_ID}"}}';

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a session the provider cleaned up moves the assistant to a new thread that says so`, async ({ harness, page }) => {
    test.setTimeout(180_000);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `assistant-session-loss-${provider}`, repo: {}, threads: [{ title: 'Ops', provider, assistant: true }] }],
    });
    const { path: cwd, threadIds } = seed.projects[0];
    const [first] = threadIds;
    const scenario = plainScenario({ name: `assistant-session-loss-${provider}`, provider, texts: ['On it.'], afterTurns: 'repeatLast' });
    await setScenario(harness, cwd, scenario);

    // A first turn gives the thread a provider session; the session ends.
    const firstTurn = awaitTurnCompleted(harness, first);
    await harness.rpc('SendMessage', first, 'Check the board.', null);
    await firstTurn;
    await harness.rpc('StopSession', first);
    const { sessionRef } = await harness.rpc<{ sessionRef: string }>('GetThread', first);
    expect(sessionRef).not.toBe('');

    // The provider cleans the session up.
    if (provider === 'claude') {
      const transcript = join(harness.bootstrap.homeDir!, '.claude', 'projects', 'mock', `${sessionRef}.jsonl`);
      expect(existsSync(transcript), transcript).toBe(true);
      rmSync(transcript);
    } else {
      // The successor starts a new Codex thread; only the resume is refused.
      await setScenario(harness, cwd, { ...scenario, codex: { responses: { 'thread/resume': NO_ROLLOUT } } });
    }

    await harness.open(page);
    await page.locator(`[data-sidebar-thread-id="${first}"]`).click();
    await expect(page.getByTestId('chat-header-title')).toHaveText('Ops');
    const moved = harness.waitForEvent<{ cwd: string; report: { kind: string; input?: string } }>(
      'harness:mock',
      (ev) => ev.report.kind === 'user_input' && ev.cwd === cwd && (ev.report.input ?? '').includes(`The provider cleaned up the session of your previous thread ${first}`),
      60_000,
    );
    await page.getByLabel('Message Input').filter({ visible: true }).fill('Anything new?');
    await page.getByTestId('composer-send').filter({ visible: true }).click();

    const input = (await moved).report.input ?? '';
    expect(input).toContain('You are "Ops", a standing agent.');
    expect(input).toContain("Agent Overflow's history of the previous thread is intact.");
    expect(input).toContain(`The last message to your previous thread may not have reached you. Read it with thread_show (thread_id ${first})`);

    let second = '';
    await expect
      .poll(async () => {
        second = (await threadRows(harness)).find((row) => row.title === 'Ops' && row.id !== first)?.id ?? '';
        return second;
      })
      .not.toBe('');
    expect((await harness.rpc<{ threadId: string }>('GetStandingAgent', second)).threadId).toBe(second);
    expect(await harness.rpc('GetStandingAgent', first)).toBeNull();

    // The pane follows; the opening row says why; the previous thread is done.
    const opening = page.locator('[data-testid="agent-message-row"][data-agent-message-kind="handoff"]');
    await expect(opening).toHaveCount(1);
    await expect(opening.getByTestId('agent-message-sender')).toHaveText('Agent Overflow');
    await expect(page.locator(`[data-sidebar-agent][data-sidebar-thread-id="${second}"]`)).toBeVisible();
    await expect(page.getByTestId('thread-row-agent-done').getByTestId('thread-row-agent-done-label')).toHaveText('1 done');
    await captureAgentMessage(page, `${provider}-assistant-session-lost.png`, opening);
  });
}

test('a plain thread whose session is gone stays where it is', async ({ harness }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'plain-session-loss', repo: {}, threads: [{ title: 'Plain', provider: 'claude' }] }],
  });
  const { path: cwd, threadIds } = seed.projects[0];
  const [thread] = threadIds;
  await setScenario(harness, cwd, plainScenario({ name: 'plain-session-loss', provider: 'claude', texts: ['On it.'], afterTurns: 'repeatLast' }));
  const firstTurn = awaitTurnCompleted(harness, thread);
  await harness.rpc('SendMessage', thread, 'Check the board.', null);
  await firstTurn;
  await harness.rpc('StopSession', thread);
  const { sessionRef } = await harness.rpc<{ sessionRef: string }>('GetThread', thread);
  rmSync(join(harness.bootstrap.homeDir!, '.claude', 'projects', 'mock', `${sessionRef}.jsonl`));

  const next = awaitTurnCompleted(harness, thread);
  await harness.rpc('SendMessage', thread, 'Anything new?', null);
  await next;
  expect((await threadRows(harness)).filter((row) => row.title === 'Plain').map((row) => row.id)).toEqual([thread]);
});
