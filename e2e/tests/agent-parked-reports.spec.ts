// Every report a background agent makes is its own card at its own
// timeline position (docs/specs/agent-visibility.md, §Agent runs and stops).
//
// A PARKED agent (claude-wire.md §E6b) reports and stops while a
// background command it started still runs, and wakes when the command
// finishes. Each stop of each run writes a completion-shaped sibling at
// the write head: a `parked` one for a pause, the ending one for the
// final stop. A §E6 resume after completion is a run of its own, with
// its own stops. What must hold:
//
//   cards    - after the final stop the timeline shows a card at every
//              stop, in order: two parked cards with their report heads,
//              then the final card; the resumed run adds its own parked
//              card and final card after the resume. Each card's digest
//              holds every row of its round up to its stop. No row is
//              hidden and no agent bell is written.
//   launch   - a launch row's indicator shows until its launch settles
//              at the ending stop, then stays off; the resume's row has
//              its own.
//   parked   - a parked card says so: the parked indicator and "Reported,
//              waiting on 1 background command" ("Reported again" for a
//              woken run), the report head collapsed, and the full
//              report, loaded by id, when expanded.
//   tray     - between stops the tray shows the agent parked, then
//              running again after the wake.
//   chip     - the activity run holding the launch names the agent as
//              its running member while it is parked and after its
//              wake, and no longer once the ending stop lands.
//   restart  - a restart keeps every row.
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import type { Locator, Page } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  agentResumeLines,
  agentWakeLine,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeTurnsScenario,
  emit,
  listItems,
  seedAgentThread,
  shellBackgroundAckLine,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolUseLine,
  waitForGate,
  type Item,
  type ScenarioStep,
} from './agent-visibility-helpers.js';

const AGENT = { task_id: 'task-bg', task_type: 'local_agent', description: 'gate watcher' };
const OUTPUT_FILE = '${CWD}/gate-output.jsonl';

const REPORT_1_HEAD = 'First gate run started: the race is in fork_moves.go.';
const REPORT_1 = `${REPORT_1_HEAD}\n\nThe log is keyed by transaction; waiting for the run to confirm it.`;
const REPORT_2_HEAD = 'Second gate run started after the first passed.';
const REPORT_2 = `${REPORT_2_HEAD}\n\nThe retry path is covered too; waiting for the run.`;
const FINAL_1 = 'Both gate runs passed; the fix holds.';
const REPORT_3_HEAD = 'Third gate run started on the follow-up.';
const REPORT_3 = `${REPORT_3_HEAD}\n\nChecking the flaky shard once more; waiting for the run.`;
const FINAL_2 = 'The follow-up run passed as well.';

function shellTask(n: number) {
  return { task_id: `task-shell-${n}`, task_type: 'local_bash', description: `gate run ${n}` };
}

function shellDone(n: number): string {
  return `Background command "gate run ${n}" completed (exit code 0)`;
}

// One run of the agent: it starts a gate run in the background, reports,
// and stops while the run still goes. `boundToolUse` is what the stop's
// notification names: the launch or carrier on a run the CLI started
// with a tool_use, nothing on a woken run.
function parkedRun(n: number, report: string, boundToolUse: string): ScenarioStep[] {
  const shell = shellTask(n);
  return [
    { waitSignal: { name: `park-${n}` } },
    emit([
      ...textLines(`msg-run-${n}-start`, `Starting gate run ${n}.`, 'tu-bg'),
      toolUseLine(`msg-run-${n}-shell`, `tu-shell-${n}`, 'Bash', { command: `gate run ${n}`, run_in_background: true }, 'tu-bg'),
      taskStartedLine(shell.task_id, `tu-shell-${n}`, shell.description, { taskType: 'local_bash', ownedBySubagent: true }),
      shellBackgroundAckLine(`tu-shell-${n}`, shell.task_id, 'tu-bg'),
      backgroundTasksChangedLine([AGENT, shell]),
      ...textLines(`msg-run-${n}-report`, report, 'tu-bg'),
      taskUpdatedLine('task-bg', { status: 'completed', end_time: 1787419835000 + n }),
      taskNotificationLine('task-bg', boundToolUse, report, {
        outputFile: OUTPUT_FILE,
        usage: { total_tokens: 1000 * n, tool_uses: 1, duration_ms: 100 * n },
        uuid: `park-${n}`,
      }),
      backgroundTasksChangedLine([shell]),
    ]),
    // The gate run finishes and the CLI wakes the agent.
    { waitSignal: { name: `wake-${n}` } },
    emit([
      taskUpdatedLine(shell.task_id, { status: 'completed', end_time: 1787419845000 + n }),
      taskNotificationLine(shell.task_id, `tu-shell-${n}`, shellDone(n), { uuid: `shell-${n}` }),
      backgroundTasksChangedLine([]),
      agentWakeLine('task-bg', 'gate watcher', {
        taskId: shell.task_id, toolUseId: `tu-shell-${n}`, status: 'completed', summary: shellDone(n),
      }),
      backgroundTasksChangedLine([AGENT]),
    ]),
  ];
}

function finalStop(name: string, text: string): ScenarioStep[] {
  return [
    { waitSignal: { name } },
    emit([
      ...textLines(`msg-${name}`, text, 'tu-bg'),
      taskUpdatedLine('task-bg', { status: 'completed', end_time: 1787419855000 }),
      taskNotificationLine('task-bg', '', text, {
        outputFile: OUTPUT_FILE,
        usage: { total_tokens: 9000, tool_uses: 3, duration_ms: 900 },
        uuid: name,
      }),
      backgroundTasksChangedLine([]),
    ]),
  ];
}

const scenario = claudeTurnsScenario('agent-parked-reports', [
  [
    emit([
      ...textLines('msg-lead', 'Launching the gate watcher.'),
      toolUseLine('msg-launch', 'tu-bg', 'Agent', {
        description: 'gate watcher',
        subagent_type: 'gate-watcher',
        prompt: 'Run the gate in the background and report each time.',
      }),
      taskStartedLine('task-bg', 'tu-bg', 'gate watcher'),
      asyncAgentAckLine('tu-bg', 'task-bg', 'gate watcher'),
      backgroundTasksChangedLine([AGENT]),
      RESULT_LINE,
    ]),
    ...parkedRun(1, REPORT_1, 'tu-bg'),
    // A woken run's stop names no tool_use.
    ...parkedRun(2, REPORT_2, ''),
    ...finalStop('final-1', FINAL_1),
  ],
  [
    emit([
      ...textLines('msg-lead-2', 'Asking the watcher to check once more.'),
      ...agentResumeLines('msg-resume', 'tu-resume', 'task-bg', 'gate watcher', 'Check the gate once more.'),
      backgroundTasksChangedLine([AGENT]),
      RESULT_LINE,
    ]),
    ...parkedRun(3, REPORT_3, 'tu-resume'),
    ...finalStop('final-2', FINAL_2),
  ],
]);

function mainTimeline(page: Page): Locator {
  return page.locator(
    '[data-testid="message-timeline-scroll"]:not([data-testid="agent-pane-timeline"] [data-testid="message-timeline-scroll"])',
  );
}

function agentStops(items: Item[]): Item[] {
  return items.filter((i) => i.completionOf === 'tu-bg' || i.completionOf === 'tu-resume');
}

async function cardAnchors(timeline: Locator): Promise<Array<{ anchor: string | null; status: string | null }>> {
  return timeline.getByTestId('subagent-group').evaluateAll((cards) =>
    cards.map((card) => ({ anchor: card.getAttribute('data-anchor-id'), status: card.getAttribute('data-status') })),
  );
}

// The gate-run shells a card's expanded digest holds, in order. Leaves the
// card collapsed.
async function digestShells(card: Locator): Promise<string[]> {
  await card.getByTestId('subagent-group-toggle').click();
  const body = card.getByTestId('subagent-group-body');
  await expect(body.locator('[data-item-id^="tu-shell-"]').first()).toBeVisible();
  const ids = await body.locator('[data-item-id^="tu-shell-"]').evaluateAll((rows) =>
    rows.map((row) => row.getAttribute('data-item-id') ?? ''),
  );
  await card.getByTestId('subagent-group-toggle').click();
  await expect(body).toHaveCount(0);
  return ids;
}

async function gate(harness: HarnessApp, mockId: string, name: string): Promise<void> {
  await waitForGate(harness, name);
  await advance(harness, mockId, name);
}

// The tray keeps its open state across the thread's quiet stretch, so it
// is opened only when it is closed.
async function openTray(page: Page): Promise<void> {
  const toggle = page.getByTestId('activity-rail-background-toggle');
  await expect(toggle).toBeVisible();
  if ((await toggle.getAttribute('aria-expanded')) !== 'true') await toggle.click();
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
}

async function expectTrayParked(page: Page, rowId: string, reportHead: string): Promise<void> {
  const row = page.locator(`[data-testid="background-task-tray-row"][data-row-id="${rowId}"]`);
  await expect(row.getByTestId('background-task-tray-row-status')).toHaveAttribute('data-run-state', 'parked');
  await expect(row.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'parked');
  await expect(row.getByTestId('background-task-tray-row-activity')).toHaveText('Waiting on 1 background command');
  await expect(row.getByTestId('background-task-tray-row-report')).toContainText(reportHead);
}

async function expectTrayRunning(page: Page, rowId: string): Promise<void> {
  const row = page.locator(`[data-testid="background-task-tray-row"][data-row-id="${rowId}"]`);
  await expect(row.getByTestId('background-task-tray-row-status')).toHaveAttribute('data-run-state', 'running');
  await expect(row.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'backgrounded');
  await expect(row.getByTestId('background-task-tray-row-report')).toHaveCount(0);
}

test('every stop of a background agent is a card at its own position, and a resume adds its own', async ({ page }, testInfo) => {
  test.setTimeout(180_000);
  const dataDir = await mkdtemp(path.join(tmpdir(), 'ao-agent-parked-reports-'));
  let harness: HarnessApp | undefined;
  try {
    harness = await launchHarness({ dataDir });
    await harness.rpc('UpdateSettings', { activityRunDefault: 'expanded' });
    await harness.rpc('HarnessSetScenario', { scenario });
    const threadId = await seedAgentThread(harness, 'parked-reports-app', 'Parked reports');
    await harness.open(page);
    await page.getByText('Parked reports').click();
    const mockId = await startMock(harness, threadId);
    const timeline = mainTimeline(page);

    // --- Run 1 parks, wakes; run 2 parks, wakes; run 3 ends ------------
    const turn1 = harness.waitForEvent('provider:turn_completed');
    await harness.rpc('SendMessage', threadId, 'watch the gate', null);
    await turn1;
    await openTray(page);

    // The activity run holding the agent's launch names it as its running
    // member until the ending stop: a parked stop pairs with the launch
    // but does not settle it.
    const agentRunChip = timeline.getByTestId('activity-run')
      .filter({ has: page.getByTestId('subagent-group') })
      .getByTestId('activity-run-header-running');

    await gate(harness, mockId, 'park-1');
    await expectTrayParked(page, 'tu-bg', REPORT_1_HEAD);
    await expect(timeline.getByTestId('subagent-group')).toHaveCount(1);
    await expect(agentRunChip).toHaveText('Agent');
    await gate(harness, mockId, 'wake-1');
    await expectTrayRunning(page, 'tu-bg');
    await expect(agentRunChip).toHaveText('Agent');

    await gate(harness, mockId, 'park-2');
    await expectTrayParked(page, 'tu-bg', REPORT_2_HEAD);
    await expect(timeline.getByTestId('subagent-group')).toHaveCount(2);
    await gate(harness, mockId, 'wake-2');
    await expectTrayRunning(page, 'tu-bg');

    await gate(harness, mockId, 'final-1');
    const cards = timeline.getByTestId('subagent-group');
    await expect(cards).toHaveCount(3);
    await expect(page.getByTestId('activity-rail-background-toggle')).toHaveCount(0);
    await expect(agentRunChip).toHaveCount(0);

    // The store: one sibling per stop, at the write head, in order, and
    // no bell for the agent.
    let items = await listItems(harness, threadId);
    const turn1Stops = agentStops(items);
    expect(turn1Stops.map((i) => i.status)).toEqual(['parked', 'parked', 'completed']);
    expect(turn1Stops.every((i) => i.completionOf === 'tu-bg' && i.kind === 'tool_completion')).toBe(true);
    expect(items.filter((i) => i.kind === 'notification')).toEqual([]);
    expect(await cardAnchors(timeline)).toEqual(turn1Stops.map((i) => ({ anchor: i.id, status: i.status })));

    // The two parked cards: the parked indicator, what the stop was
    // waiting on, the report head. The final card is a completed card.
    const parked1 = cards.nth(0);
    const parked2 = cards.nth(1);
    const final1 = cards.nth(2);
    await expect(parked1.getByTestId('subagent-group-status')).toHaveAttribute('data-state', 'parked');
    await expect(parked1.getByTestId('subagent-group-parked-status')).toHaveText('Reported, waiting on 1 background command');
    await expect(parked1.getByTestId('subagent-group-preview')).toContainText(REPORT_1_HEAD);
    await expect(parked2.getByTestId('subagent-group-status')).toHaveAttribute('data-state', 'parked');
    await expect(parked2.getByTestId('subagent-group-parked-status')).toHaveText('Reported again, waiting on 1 background command');
    await expect(parked2.getByTestId('subagent-group-preview')).toContainText(REPORT_2_HEAD);
    await expect(final1.getByTestId('subagent-group-status')).toHaveCount(0);
    await expect(final1.getByTestId('subagent-group-parked-status')).toHaveCount(0);
    await expect(final1.getByTestId('subagent-group-preview')).toHaveText(FINAL_1);
    await expect(timeline.getByTestId('notification-row')).toHaveCount(0);

    await testInfo.attach('parked-cards', {
      body: await timeline.screenshot(),
      contentType: 'image/png',
    });
    await page.screenshot({ path: testInfo.outputPath('parked-cards.png') });

    // Expanding a parked card loads the full report above its run's digest.
    await expect(parked1.getByTestId('subagent-group-parked-report')).toHaveCount(0);
    await parked1.getByTestId('subagent-group-toggle').click();
    await expect(parked1.getByTestId('subagent-group-parked-report')).toContainText(
      'The log is keyed by transaction; waiting for the run to confirm it.',
    );
    await expect(parked1.getByTestId('subagent-group-parked-report-error')).toHaveCount(0);
    await parked1.getByTestId('subagent-group-toggle').click();
    await expect(parked1.getByTestId('subagent-group-parked-report')).toHaveCount(0);

    // Each card is the agent as of its stop: its digest holds every row of
    // the round up to that stop, the earlier runs' included.
    expect(await digestShells(parked1)).toEqual(['tu-shell-1']);
    expect(await digestShells(parked2)).toEqual(['tu-shell-1', 'tu-shell-2']);
    expect(await digestShells(final1)).toEqual(['tu-shell-1', 'tu-shell-2']);

    // The launch settled at the ending stop: its row's dots are hidden.
    const launchRow = timeline.locator('[data-item-id="tu-bg"]');
    await expect(launchRow.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'settled');
    await expect(launchRow.getByTestId('agent-row-preview')).toContainText('gate watcher');

    // --- The resume after completion is a run of its own ----------------
    const turn2 = harness.waitForEvent('provider:turn_completed');
    await harness.rpc('SendMessage', threadId, 'check it again', null);
    await turn2;
    await openTray(page);
    await gate(harness, mockId, 'park-3');
    await expectTrayParked(page, 'tu-resume', REPORT_3_HEAD);
    await expect(cards).toHaveCount(4);
    // The resume is a new row with its own indicator, on through its park;
    // the settled launch's stays off.
    const resumeRow = timeline.locator('[data-item-id="tu-resume"]');
    await expect(resumeRow.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'backgrounded');
    await expect(launchRow.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'settled');
    await gate(harness, mockId, 'wake-3');
    await expectTrayRunning(page, 'tu-resume');
    await gate(harness, mockId, 'final-2');
    await expect(cards).toHaveCount(5);
    await expect(resumeRow.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'settled');
    await expect(launchRow.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'settled');

    items = await listItems(harness, threadId);
    const stops = agentStops(items);
    expect(stops.map((i) => [i.completionOf, i.status])).toEqual([
      ['tu-bg', 'parked'],
      ['tu-bg', 'parked'],
      ['tu-bg', 'completed'],
      ['tu-resume', 'parked'],
      ['tu-resume', 'completed'],
    ]);
    expect(items.filter((i) => i.kind === 'notification')).toEqual([]);
    const anchors = stops.map((i) => ({ anchor: i.id, status: i.status }));
    await expect.poll(() => cardAnchors(timeline)).toEqual(anchors);
    const parked3 = cards.nth(3);
    await expect(parked3.getByTestId('subagent-group-parked-status')).toHaveText('Reported, waiting on 1 background command');
    await expect(parked3.getByTestId('subagent-group-preview')).toContainText(REPORT_3_HEAD);
    await expect(cards.nth(4).getByTestId('subagent-group-preview')).toHaveText(FINAL_2);
    // The resume's cards cover its own round, from the resume to the stop.
    expect(await digestShells(parked3)).toEqual(['tu-shell-3']);
    expect(await digestShells(cards.nth(4))).toEqual(['tu-shell-3']);
    // The first run's cards read the same after the resume.
    await expect(cards.nth(0).getByTestId('subagent-group-preview')).toContainText(REPORT_1_HEAD);
    await expect(cards.nth(2).getByTestId('subagent-group-preview')).toHaveText(FINAL_1);

    // --- Restart ----------------------------------------------------------
    const rows = (list: Item[]) => list.map((i) => `${i.id}|${i.kind}|${i.status}|${i.completionOf ?? ''}|${i.summary}`);
    const before = rows(items);
    expect(await harness.stop()).toBe(true);
    harness = await launchHarness({ dataDir });
    expect(rows(await listItems(harness, threadId))).toEqual(before);
    await harness.open(page);
    await page.getByTestId('thread-row').filter({ hasText: 'Parked reports' }).click();
    await expect.poll(() => cardAnchors(mainTimeline(page))).toEqual(anchors);
    await expect(mainTimeline(page).getByTestId('subagent-group').nth(1).getByTestId('subagent-group-parked-status'))
      .toHaveText('Reported again, waiting on 1 background command');
  } finally {
    try {
      await page.close();
      await harness?.close();
    } finally {
      await rm(dataDir, { recursive: true, force: true });
    }
  }
});
