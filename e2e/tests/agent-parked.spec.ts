// A PARKED background agent (claude-wire.md §E6b): the agent reports and
// stops while a background command it started still runs, then wakes
// when that command finishes and stops again for good.
//
// What the user sees at each step, and the store facts behind it:
//
//   parked  - the tray row stays (the CLI's level set drops the agent, the
//             store keeps its launch open) and says so: the parked
//             indicator, "Waiting on N background command(s)" in place of
//             the live activity line, the report's head beneath it. The
//             timeline gets the parked bell with the same report head and
//             the full report on demand; no card, no completion sibling.
//   woken   - the row is a running background agent again; the bell stays.
//   final   - the card lands at the completion sibling, the tray empties,
//             and every bell of the task (parked and final) is hidden.
//
// The served run state rides ListLiveBackgroundTasks; the bell's report
// link is stamped on the notification row at write time (triage
// parkedBellMeta) and the full row loads by id (GetThreadItem).
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  agentWakeLine,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeScenario,
  emit,
  itemMeta,
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
} from './agent-visibility-helpers.js';

const REPORT_HEAD = 'Found the race in fork_moves.go: the log is keyed by transaction.';
const REPORT = `${REPORT_HEAD}\n\nWaiting for the gate run to finish before I confirm the fix.`;
const SHELL_DONE = 'Background command "sleep 60; echo LONG" completed (exit code 0)';

test('a parked background agent shows its state and report on the tray and the bell, then wakes and settles', async ({
  harness,
  page,
}) => {
  const agent = { task_id: 'task-bg', task_type: 'local_agent', description: 'gate watcher' };
  const shell = { task_id: 'task-shell', task_type: 'local_bash', description: 'sleep 60; echo LONG' };
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('parked-agent', [
      emit([
        ...textLines('msg-lead', 'Launching the gate watcher.'),
        toolUseLine('msg-launch', 'tu-bg', 'Agent', {
          description: 'gate watcher',
          subagent_type: 'gate-watcher',
          prompt: 'Run the gate in the background and report.',
        }),
        taskStartedLine('task-bg', 'tu-bg', 'gate watcher'),
        asyncAgentAckLine('tu-bg', 'task-bg', 'gate watcher'),
        backgroundTasksChangedLine([agent]),
        RESULT_LINE,
      ]),
      // Round 1: the agent starts its shell, reports, and stops parked. Every
      // agent notification names its output file, as the CLI's do; the
      // envelope's summary is the report and the file is never read.
      { waitSignal: { name: 'park' } },
      emit([
        ...textLines('msg-s1', 'Starting the gate run.', 'tu-bg'),
        toolUseLine('msg-s2', 'tu-shell', 'Bash', { command: 'sleep 60; echo LONG', run_in_background: true }, 'tu-bg'),
        taskStartedLine('task-shell', 'tu-shell', 'sleep 60; echo LONG', { taskType: 'local_bash', ownedBySubagent: true }),
        shellBackgroundAckLine('tu-shell', 'task-shell', 'tu-bg'),
        backgroundTasksChangedLine([agent, shell]),
        ...textLines('msg-s3', REPORT, 'tu-bg'),
        taskUpdatedLine('task-bg', { status: 'completed', end_time: 1787419835322 }),
        taskNotificationLine('task-bg', 'tu-bg', 'WAITING', {
          outputFile: '${CWD}/gate-output.jsonl',
          usage: { total_tokens: 12000, tool_uses: 1, duration_ms: 2100 },
          uuid: 'park-1',
        }),
        backgroundTasksChangedLine([shell]),
      ]),
      // The shell finishes and the CLI wakes the agent from its transcript.
      { waitSignal: { name: 'wake' } },
      emit([
        taskUpdatedLine('task-shell', { status: 'completed', end_time: 1787419845000 }),
        taskNotificationLine('task-shell', 'tu-shell', SHELL_DONE, { uuid: 'shell-1' }),
        backgroundTasksChangedLine([]),
        agentWakeLine('task-bg', 'gate watcher', {
          taskId: 'task-shell', toolUseId: 'tu-shell', status: 'completed', summary: SHELL_DONE,
        }),
        backgroundTasksChangedLine([agent]),
      ]),
      // Round 2: the final stop.
      { waitSignal: { name: 'final' } },
      emit([
        ...textLines('msg-s4', 'Gate passed; the fix holds.', 'tu-bg'),
        taskUpdatedLine('task-bg', { status: 'completed', end_time: 1787419855000 }),
        taskNotificationLine('task-bg', 'tu-bg', 'Gate passed; the fix holds.', {
          outputFile: '${CWD}/gate-output.jsonl',
          usage: { total_tokens: 15500, tool_uses: 1, duration_ms: 3400 },
          uuid: 'final-1',
        }),
        backgroundTasksChangedLine([]),
      ]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'parked-app', 'Parked agent');
  await harness.open(page);
  await page.getByText('Parked agent').click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'watch the gate', null);
  await harness.waitForEvent('provider:turn_completed');

  const timeline = page.locator(
    '[data-testid="message-timeline-scroll"]:not([data-testid="agent-pane-timeline"] [data-testid="message-timeline-scroll"])',
  );
  await expect(timeline.locator('[data-item-id="tu-bg"]').getByTestId('agent-row-status')).toHaveAttribute('data-state', 'backgrounded');
  await page.getByTestId('activity-rail-background-toggle').click();
  const trayRows = page.getByTestId('background-task-tray-row');
  await expect(trayRows).toHaveCount(1);
  const agentRow = page.locator('[data-testid="background-task-tray-row"][data-row-id="tu-bg"]');
  await expect(agentRow.getByTestId('background-task-tray-row-status')).toHaveAttribute('data-run-state', 'running');
  await expect(agentRow.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'backgrounded');
  await expect(page.getByTestId('activity-rail-background-running-label')).toHaveText('1 running');

  // --- Parked ---------------------------------------------------------
  await waitForGate(harness, 'park');
  await advance(harness, mockId, 'park');

  // The store: the stop is stashed, no sibling, and the bell names the
  // report row it links.
  const parkedBellId = 'task-notification:task-bg:park-1';
  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      const bell = items.find((i) => i.id === parkedBellId);
      if (!bell) return null;
      const meta = itemMeta(bell);
      const report = items.find((i) => i.id === meta.parked_report_item_id);
      return {
        summary: bell.summary,
        kind: meta.kind,
        commands: meta.parked_commands,
        preview: meta.parked_report_preview,
        reportParent: report?.parentId ?? null,
        reportText: report?.summary ?? null,
        siblings: items.filter((i) => i.completionOf === 'tu-bg').length,
      };
    })
    .toEqual({
      summary: 'Agent "gate watcher" reported and is waiting on 1 background command',
      kind: 'parked_agent',
      commands: 1,
      preview: REPORT,
      reportParent: 'tu-bg',
      reportText: REPORT,
      siblings: 0,
    });

  // The tray: the agent row is parked, the shell it waits on sits under it.
  await expect(trayRows).toHaveCount(2);
  await expect(agentRow.getByTestId('background-task-tray-row-status')).toHaveAttribute('data-run-state', 'parked');
  await expect(agentRow.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'parked');
  await expect(agentRow.getByTestId('agent-row-status').getByTestId('indicator')).toHaveAttribute('aria-label', 'Parked');
  await expect(agentRow.getByTestId('background-task-tray-row-activity')).toHaveText('Waiting on 1 background command');
  await expect(agentRow.getByTestId('background-task-tray-row-report')).toHaveText(REPORT);
  const shellRow = page.locator('[data-testid="background-task-tray-row"][data-row-id="tu-shell"]');
  await expect(shellRow).toHaveAttribute('data-depth', '1');
  await expect(shellRow.getByTestId('background-task-tray-row-status')).not.toHaveAttribute('data-run-state');
  // The parked agent is not running; its shell is.
  await expect(page.getByTestId('activity-rail-background-running-label')).toHaveText('1 running');

  // The timeline: no card, the parked bell with the report head, and the
  // full report on demand. The spawn row is untouched.
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(0);
  const bell = timeline.getByTestId('parked-agent-bell');
  await expect(bell).toHaveCount(1);
  await expect(bell).toContainText('Agent "gate watcher" reported and is waiting on 1 background command');
  await expect(bell.getByTestId('parked-agent-bell-preview')).toHaveText(REPORT);
  await expect(bell.getByTestId('parked-agent-bell-report')).toHaveCount(0);
  await bell.getByTestId('parked-agent-bell-report-toggle').click();
  await expect(bell).toHaveAttribute('data-expanded', 'true');
  await expect(bell.getByTestId('parked-agent-bell-report')).toContainText('Waiting for the gate run to finish before I confirm the fix.');
  await expect(bell.getByTestId('parked-agent-bell-error')).toHaveCount(0);
  await bell.getByTestId('parked-agent-bell-report-toggle').click();
  await expect(bell.getByTestId('parked-agent-bell-preview')).toHaveText(REPORT);
  await expect(timeline.locator('[data-item-id="tu-bg"]').getByTestId('agent-row-status')).toHaveAttribute('data-state', 'backgrounded');

  // --- Woken ----------------------------------------------------------
  await waitForGate(harness, 'wake');
  await advance(harness, mockId, 'wake');
  await expect(trayRows).toHaveCount(1);
  await expect(agentRow.getByTestId('background-task-tray-row-status')).toHaveAttribute('data-run-state', 'running');
  await expect(agentRow.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'backgrounded');
  await expect(agentRow.getByTestId('background-task-tray-row-report')).toHaveCount(0);
  await expect(page.getByTestId('activity-rail-background-running-label')).toHaveText('1 running');
  // The woken round opened under the launch: the wake row is its prompt.
  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      return items.filter((i) => i.parentId === 'tu-bg' && itemMeta(i).subagent_wake_prompt === true).length;
    })
    .toBe(1);
  await expect(bell).toHaveCount(1);
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(0);

  // --- Final ----------------------------------------------------------
  await waitForGate(harness, 'final');
  await advance(harness, mockId, 'final');
  const card = timeline.getByTestId('subagent-group');
  await expect(card).toHaveCount(1);
  await expect(card).toHaveAttribute('data-background', 'true');
  await expect(card.getByTestId('subagent-group-preview')).toContainText('Gate passed; the fix holds.');
  await expect(timeline.getByTestId('parked-agent-bell')).toHaveCount(0);
  await expect(timeline.getByTestId('notification-row')).toHaveCount(0);
  await expect(page.getByTestId('activity-rail-background-toggle')).toHaveCount(0);
  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      return {
        siblings: items.filter((i) => i.completionOf === 'tu-bg').map((i) => i.status),
        bells: items.filter((i) => i.kind === 'notification' && itemMeta(i).task_id === 'task-bg').map((i) => i.id).sort(),
      };
    })
    .toEqual({
      siblings: ['completed'],
      bells: ['task-notification:task-bg:final-1', parkedBellId],
    });
});
