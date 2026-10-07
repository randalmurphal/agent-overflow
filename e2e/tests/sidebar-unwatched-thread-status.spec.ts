// The sidebar's status for a thread on this computer that the page has no
// pane on, when another thread messages it with thread_send. A message
// queued behind the thread's running turn reaches the page as wildcard
// queue frames, but the echo that consumes it reaches watching clients
// only, so the row reads the running state between the message and its
// echo from provider:sends_pending. Once the turns end the row reads
// idle, in both providers, whether the message was queued behind a busy
// turn or started a turn on an idle thread.
import { expect, test, type HarnessMockEvent, type SeedResult } from './fixtures.js';
import {
  RESULT_THREAD_PATTERN, advanceGate, awaitToolAnswer, setScenario, threadToolsScenario,
} from './thread-tools-helpers.js';

for (const provider of ['codex', 'claude'] as const) {
  test(`a ${provider} thread with no pane here settles after thread_send messages`, async ({ harness, page }) => {
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        { name: `unwatched-caller-${provider}`, repo: {}, threads: [{ title: 'Watched caller', provider: 'claude',
          turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }] },
        { name: `unwatched-target-${provider}`, repo: {}, threads: [] },
      ],
    });
    const caller = seed.projects[0].threadIds[0];
    const targetPath = seed.projects[1].path;
    await setScenario(harness, seed.projects[0].path, threadToolsScenario({
      name: 'unwatched-caller', provider: 'claude', turns: [
        { steps: [
          { call: { tool: 'thread_spawn', args: {
            prompt: 'Review the change.', title: 'Unwatched review', project_id: seed.projects[1].projectId, provider,
          } } },
          { capture: { var: 'TARGET', from: '${MCP_RESULT}', pattern: RESULT_THREAD_PATTERN } },
        ], text: 'Spawned.' },
        { steps: [{ call: { tool: 'thread_send', args: { thread_id: '${TARGET}', message: 'One more change.' } } }], text: 'Sent.' },
        { steps: [{ call: { tool: 'thread_send', args: { thread_id: '${TARGET}', message: 'Recheck please.' } } }], text: 'Sent again.' },
      ],
    }));
    await setScenario(harness, targetPath, threadToolsScenario({
      name: 'unwatched-target', provider, turns: [
        { steps: [{ gate: 'review' }], text: 'Reviewed.' },
        { text: 'Read the change.' },
        { text: 'Rechecked.' },
      ],
    }));
    const targetInput = (text: string) => harness.waitForEvent<HarnessMockEvent>('harness:mock',
      (ev) => ev.report.kind === 'user_input' && ev.cwd === targetPath && (ev.report.input ?? '').includes(text));
    const callerIdle = () => expect.poll(async () =>
      (await harness.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', caller)).activeTurn).toBeFalsy();

    // The page watches the caller; the target is a sidebar row.
    await harness.open(page);
    await page.getByTestId('thread-row').filter({ hasText: 'Watched caller' }).first().click();
    await expect(page.getByTestId('message-timeline-scroll')).toBeVisible();

    await harness.rpc('StartSession', caller);
    const reviewing = harness.waitForEvent<HarnessMockEvent>('harness:mock',
      (ev) => ev.report.kind === 'waiting_signal' && ev.report.detail === 'review' && ev.cwd === targetPath);
    await harness.rpc('SendMessage', caller, 'start a review', null);
    const spawn = await awaitToolAnswer<{ thread_id: string }>(harness, { tool: 'thread_spawn' });
    expect(spawn.isError, spawn.text).toBe(false);
    const target = spawn.value!.thread_id;
    const { mockId } = await reviewing;
    const row = page.getByTestId('thread-row').filter({ hasText: 'Unwatched review' }).first();
    const dot = row.getByTestId('thread-row-status-dot');
    // A settled row shows no working dot (an idle row may show none at all).
    const running = row.locator('[data-testid="thread-row-status-dot"][data-status="running"]');
    const settled = async () => {
      await expect.poll(async () => await harness.rpc<{ queueItems?: unknown[]; flushedItems?: unknown[] }>(
        'GetThreadLiveState', target,
      )).toMatchObject({ queueItems: [], flushedItems: [] });
      await expect.poll(async () => (await harness.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', target)).activeTurn).toBeFalsy();
      await expect(running).toHaveCount(0);
    };
    await expect(dot).toHaveAttribute('data-status', 'running');

    // A message queued behind the running turn, consumed at its boundary.
    await callerIdle();
    await harness.rpc('SendMessage', caller, 'tell it about one more change', null);
    const queued = await awaitToolAnswer(harness, { tool: 'thread_send' });
    expect(queued.isError, queued.text).toBe(false);
    const consumed = targetInput('One more change.');
    await advanceGate(harness, mockId, 'review');
    await consumed;
    await settled();

    // A message to the idle thread starts a turn of its own.
    await callerIdle();
    const rechecked = targetInput('Recheck please.');
    await harness.rpc('SendMessage', caller, 'ask for a recheck', null);
    await rechecked;
    await settled();
  });
}
