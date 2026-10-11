// Advisor calls the API never runs (docs/references/claude-wire.md,
// orphaned server-side tool calls) render truthfully on the live wire.
//
// One turn replays the observed shapes: a message calls Bash and then the
// advisor (the API skips that advisor), the next message calls the advisor
// again and it runs, and a later message calls the advisor before a client
// tool. Every rendered frame is sampled: the skipped call never gets a row,
// the running call is the only advisor row while it runs, the
// advisor-before-tool call settles as "Not run", and no row ever reads
// "Advisor call failed". A reload shows the same rows.
import { test, expect } from './fixtures.js';
import type { Page } from '@playwright/test';
import {
  RESULT_LINE,
  advance,
  claudeScenario,
  emit,
  seedAgentThread,
  startMock,
  textLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';

const j = (value: unknown): string => JSON.stringify(value);

function messageStart(id: string): string {
  return j({ type: 'stream_event', event: 'message_start', data: { type: 'message_start', message: { id, role: 'assistant' } } });
}

function advisorCall(messageId: string, id: string): string {
  return j({
    type: 'assistant',
    message: { id: messageId, role: 'assistant', model: 'claude-mock-1', content: [{ type: 'server_tool_use', id, name: 'advisor', input: {} }] },
  });
}

function advisorResult(messageId: string, id: string, text: string): string {
  return j({
    type: 'assistant',
    message: {
      id: messageId,
      role: 'assistant',
      model: 'claude-mock-1',
      content: [{ type: 'advisor_tool_result', tool_use_id: id, content: { type: 'advisor_result', text } }],
    },
  });
}

function scenario(): unknown {
  return claudeScenario('advisor-not-run', [
    emit([
      messageStart('msg-1'),
      toolUseLine('msg-1', 'toolu_bash_1', 'Bash', { command: 'true' }),
      advisorCall('msg-1', 'srvtoolu_skipped'),
      toolResultLine('toolu_bash_1', 'ok'),
      messageStart('msg-2'),
      advisorCall('msg-2', 'srvtoolu_ran'),
    ]),
    { waitSignal: { name: 'advising' } },
    emit([
      advisorResult('msg-2', 'srvtoolu_ran', 'Looks right.'),
      messageStart('msg-3'),
      advisorCall('msg-3', 'srvtoolu_not_run'),
      toolUseLine('msg-3', 'toolu_bash_2', 'Bash', { command: 'true' }),
      toolResultLine('toolu_bash_2', 'ok'),
      ...textLines('msg-4', 'Done advising.'),
      RESULT_LINE,
    ]),
  ]);
}

interface AdvisorFrames {
  maxRows: number;
  maxRowsBeforeGate: number;
  gate: boolean;
  sawFailed: boolean;
}

async function installSampler(page: Page): Promise<void> {
  await page.evaluate(() => {
    const state: AdvisorFrames = { maxRows: 0, maxRowsBeforeGate: 0, gate: false, sawFailed: false };
    (window as unknown as { __aoAdvisor: AdvisorFrames }).__aoAdvisor = state;
    // An activity run mounts a window of its rows, so a run's advisor rows
    // are read from its header counts; rows outside a run are counted.
    const advisorRows = (): number => {
      let total = 0;
      for (const counts of document.querySelectorAll('[data-testid="activity-run-header-counts"]')) {
        total += Number(/(\d+) Advisor/.exec(counts.textContent ?? '')?.[1] ?? 0);
      }
      for (const row of document.querySelectorAll('[data-testid="advisor-row"]')) {
        if (!row.closest('[data-testid="activity-run"]')) total += 1;
      }
      return total;
    };
    const tick = () => {
      const rows = advisorRows();
      state.maxRows = Math.max(state.maxRows, rows);
      if (!state.gate) state.maxRowsBeforeGate = Math.max(state.maxRowsBeforeGate, rows);
      if (document.body.textContent?.includes('Advisor call failed')) state.sawFailed = true;
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  });
}

async function expectSettledRows(page: Page): Promise<void> {
  const timeline = page.getByTestId('message-timeline-scroll');
  // The settled turn folds into an activity run; its counts include only
  // the two advisor calls that have rows.
  const run = timeline.getByTestId('activity-run').last();
  await expect(run.getByTestId('activity-run-header-counts')).toContainText('2 Advisor');
  if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
  const rows = timeline.getByTestId('advisor-row');
  await expect(rows).toHaveCount(2);
  await expect(rows.nth(0)).not.toContainText('Not run');
  await expect(rows.nth(0).locator('[data-testid="indicator"][data-state="running"]')).toHaveCount(0);
  await expect(rows.nth(1)).toContainText('Not run');
  await expect(rows.nth(1).getByTestId('indicator')).toHaveAttribute('data-state', 'declined');
  await expect(timeline.getByText('Advisor call failed')).toHaveCount(0);
}

test('advisor calls the API never ran show no row or settle as Not run', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', { scenario: scenario() });
  const threadId = await seedAgentThread(harness, 'advisor-not-run', 'Advisor not run');
  await harness.open(page);
  const timeline = page.getByTestId('message-timeline-scroll');
  await page.getByTestId('thread-row').getByText('Advisor not run', { exact: true }).click();
  await expect(timeline.getByText('Ready.')).toBeVisible();
  const mockId = await startMock(harness, threadId);
  await installSampler(page);

  const advising = waitForGate(harness, 'advising');
  const completed = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'check with the advisor', null);
  await advising;

  // The call that runs is the only advisor row, and it is running.
  await expect(timeline.locator('[data-testid="advisor-row"] [data-testid="indicator"][data-state="running"]')).toHaveCount(1);
  await page.evaluate(() => {
    (window as unknown as { __aoAdvisor: AdvisorFrames }).__aoAdvisor.gate = true;
  });

  await advance(harness, mockId, 'advising');
  await completed;
  await expect(timeline.getByText('Done advising.')).toBeVisible();
  await expectSettledRows(page);

  const frames = await page.evaluate(() => (window as unknown as { __aoAdvisor: AdvisorFrames }).__aoAdvisor);
  expect(frames.maxRowsBeforeGate, 'advisor rows on any frame while only one call had run').toBe(1);
  expect(frames.maxRows, 'advisor rows on any frame').toBe(2);
  expect(frames.sawFailed, 'a frame showed "Advisor call failed"').toBe(false);

  await page.reload();
  await page.getByTestId('thread-row').getByText('Advisor not run', { exact: true }).click();
  await expect(timeline.getByText('Done advising.')).toBeVisible();
  await expectSettledRows(page);
});
