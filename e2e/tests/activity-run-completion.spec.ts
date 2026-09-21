// Offscreen agent completion in a partially shipped run: launch context
// travels with the completion, and opening the run preserves its ownership.
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE, asyncAgentAckLine, claudeScenario, emit, seedAgentThread,
  sidechainTranscript, startMock, taskNotificationLine, taskStartedLine,
  taskUpdatedLine, textLines, toolResultLine, toolUseLine, waitForGate,
} from './agent-visibility-helpers.js';

test('a completion forms its card when its launch is outside the shipped activity window', async ({ harness, page }) => {
  const report = 'The offscreen agent finished its complete investigation.';
  const work = Array.from({ length: 60 }, (_, index) => [
    toolUseLine(`msg-work-${index}`, `work-${index}`, 'Bash', { command: `echo ${index}` }),
    toolResultLine(`work-${index}`, `${index}`),
  ]).flat();
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('offscreen-completion', [
      emit([
        toolUseLine('msg-agent', 'agent-launch', 'Agent', {
          description: 'investigate offscreen', subagent_type: 'sweeper', run_in_background: true,
        }),
        taskStartedLine('task-offscreen', 'agent-launch', 'investigate offscreen'),
        taskUpdatedLine('task-offscreen', { is_backgrounded: true }),
        asyncAgentAckLine('agent-launch', 'task-offscreen', 'investigate offscreen'),
        ...work,
      ]),
      { writeFile: { path: 'offscreen.jsonl', content: sidechainTranscript([{ text: report }]) } },
      emit([
        taskUpdatedLine('task-offscreen', { status: 'completed', end_time: 1787419835322 }),
        taskNotificationLine('task-offscreen', 'agent-launch', report, { outputFile: '${CWD}/offscreen.jsonl' }),
        ...textLines('msg-finished', 'Main agent acknowledged the report.'),
        RESULT_LINE,
      ]),
      { waitSignal: { name: 'finished' } },
    ]),
  });
  const threadId = await seedAgentThread(harness, 'offscreen-agents', 'Offscreen agent report');
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'investigate', null);
  await waitForGate(harness, 'finished');
  type Row = { id: string; kind: string; completionOf?: string; completionLaunch?: { id: string } };
  const tail = await harness.rpc<{ items: Row[] }>('ListThreadSliceAround', threadId, '', 200,
    { inlinePreviews: false, runWindowRows: 30, maxBytes: 1000000 });
  expect(tail.items.some(item => item.id === 'agent-launch')).toBe(false);
  expect(tail.items.find(item => item.completionOf === 'agent-launch')?.completionLaunch?.id).toBe('agent-launch');

  await harness.open(page);
  await page.getByText('Offscreen agent report', { exact: true }).click();
  const run = page.getByTestId('activity-run');
  await expect(run).toHaveCount(1);
  await expect(run).toHaveAttribute('data-collapsed', 'true');
  await expect(page.getByText(report, { exact: true })).toHaveCount(0);
  await run.getByTestId('activity-run-header').click();
  const card = run.getByTestId('subagent-group');
  await expect(card).toHaveCount(1);
  await expect(card.getByTestId('subagent-group-preview')).toContainText(report);
  await expect(page.getByTestId('subagent-group')).toHaveCount(1);
  await expect(run.getByTestId('activity-run-later')).toHaveCount(0);
  await expect(page.getByText('Activity moved while it was loading', { exact: true })).toHaveCount(0);
});
