// A message flushed into a Codex thread the page has no pane on, whose
// turn then runs past the pane's opening window. The page holds the
// flushed entry from the wildcard queue frames but never sees the echo, so
// opening the pane finds the message consumed with its row in the history
// before the window. The pane hands it to that history: no pending preview
// and no working indicator once the turn has ended.
import { expect, test, type HarnessMockEvent, type SeedResult } from './fixtures.js';
import { setScenario, threadToolsScenario } from './thread-tools-helpers.js';

// More prose rows than the pane's opening slice (SLICE_AROUND_ITEM_BUDGET).
// Activity runs fold into a few rows, so the filler is assistant messages.
const TRAILING_MESSAGES = 230;

function commandLines(index: number): string[] {
  const item = `{"id":"cmd-${index}","type":"commandExecution","command":"echo ${index}"`;
  return [
    `{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"\${THREAD_ID}","turnId":"\${TURN_ID}","item":${item},"status":"inProgress"}}}`,
    `{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"\${THREAD_ID}","turnId":"\${TURN_ID}","item":${item},"status":"completed","exitCode":0}}}`,
  ];
}

function messageLines(index: number): string[] {
  const item = (text: string, status: string) =>
    `{"type":"agentMessage","id":"note-${index}","status":"${status}","text":"${text}"}`;
  return [
    `{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"\${THREAD_ID}","turnId":"\${TURN_ID}","item":${item('', 'inProgress')}}}`,
    `{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"\${THREAD_ID}","turnId":"\${TURN_ID}","item":${item(`Note ${index}.`, 'completed')}}}`,
  ];
}

test('a consumed message older than the opening window leaves no pending preview', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'history-before-window-other', repo: {}, threads: [{ title: 'Elsewhere', provider: 'claude',
        turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }] },
      { name: 'history-before-window', repo: {}, threads: [{ title: 'Long review', provider: 'codex' }] },
    ],
  });
  const target = seed.projects[1].threadIds[0];
  const targetPath = seed.projects[1].path;
  const trailing = Array.from({ length: TRAILING_MESSAGES }, (_, i) => messageLines(i)).flat();
  await setScenario(harness, targetPath, threadToolsScenario({
    name: 'history-before-window', provider: 'codex', turns: [
      { steps: [{ gate: 'review' }, { emitLines: commandLines(-1) }, { gate: 'consumed' }, { emitLines: trailing }],
        text: 'Reviewed.' },
    ],
  }));
  const gate = (name: string) => harness.waitForEvent<HarnessMockEvent>('harness:mock',
    (ev) => ev.report.kind === 'waiting_signal' && ev.report.detail === name && ev.cwd === targetPath);

  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Elsewhere' }).first().click();
  await expect(page.getByTestId('message-timeline-scroll')).toBeVisible();

  await harness.rpc('StartSession', target);
  const reviewing = gate('review');
  await harness.rpc('SendMessage', target, 'Review the change.', null);
  const { mockId } = await reviewing;
  const advance = (name: string) => harness.rpc('HarnessMockCommand', mockId, { type: 'advance', name });

  const steered = harness.waitForEvent<HarnessMockEvent>('harness:mock', (ev) =>
    ev.mockId === mockId && ev.report.kind === 'user_input' && (ev.report.input ?? '').includes('One more change.'));
  const consumed = gate('consumed');
  await harness.rpc('RegisterQueueItem', target, 'One more change.', {});
  await advance('review');
  await steered;
  await consumed;
  const finished = harness.waitForEvent('provider:turn_completed', (ev: { threadId?: string }) => ev.threadId === target);
  await advance('consumed');
  await finished;
  await expect.poll(async () => await harness.rpc<{ queueItems?: unknown[]; flushedItems?: unknown[]; activeTurn?: unknown }>(
    'GetThreadLiveState', target,
  )).toMatchObject({ queueItems: [], flushedItems: [] });

  await page.getByTestId('thread-row').filter({ hasText: 'Long review' }).first().click();
  await expect(page.getByText('Reviewed.', { exact: true })).toBeVisible();
  // The opening window starts after the steered message.
  await expect(page.getByTestId('user-message-bubble').filter({ hasText: 'One more change.' })).toHaveCount(0);
  await expect(page.getByTestId('send-queue-preview-row')).toHaveCount(0);
  await expect(page.getByTestId('activity-rail-working')).toHaveCount(0);
});
