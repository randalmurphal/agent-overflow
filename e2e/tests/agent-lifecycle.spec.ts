// An agent's rows belong to the agent, not to the turn that launched it
// (docs/architecture/turn-lifecycle.md, §Agent-owned rows). What must hold:
//
//   turn end - the parent's `result` settles the main thread's rows only:
//              an async agent's tool call in flight keeps running and its
//              later result lands, and its open text keeps streaming into
//              the same row.
//   Stop     - a Stop in a later turn kills agents launched in an earlier
//              one (claude-wire.md §Background task ownership). The mock
//              writes the CLI's kill frames: each agent's
//              `task_updated{killed}` and `task_notification{stopped}`
//              before the kill frames of the shells it owns. Every row
//              under each agent settles as stopped, the agent ends killed
//              and never parked, its card reads "Agent stopped", and the
//              tray empties.
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeTurnsScenario,
  emit,
  finishTextLines,
  itemMeta,
  listItems,
  openTextLines,
  seedAgentThread,
  shellBackgroundAckLine,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
  type Item,
} from './agent-visibility-helpers.js';

const timelineSelector =
  '[data-testid="message-timeline-scroll"]:not([data-testid="agent-pane-timeline"] [data-testid="message-timeline-scroll"])';

function agentLaunchLines(tu: string, taskId: string, description: string): string[] {
  return [
    toolUseLine(`msg-launch-${tu}`, tu, 'Agent', { description, subagent_type: 'worker', prompt: `Do ${description}.` }),
    taskStartedLine(taskId, tu, description),
    asyncAgentAckLine(tu, taskId, description),
  ];
}

/** Rows still open. A background launch row is the spawn record and stays
 * `running` for good; its completion sibling carries how it ended. */
function unsettled(items: Item[]): string[] {
  return items
    .filter((i) => (i.status === 'running' || i.status === 'streaming') && !(i.kind === 'tool_call' && i.isBackground))
    .map((i) => `${i.id}:${i.status}`)
    .sort();
}

test("the parent turn ending leaves an agent's tool call and text open, and both land", async ({ harness }) => {
  const agent = { task_id: 'task-a', task_type: 'local_agent', description: 'reader' };
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('agent-outlives-turn', [
      [
        emit([
          ...textLines('msg-lead', 'Starting the reader.'),
          ...agentLaunchLines('tu-a', 'task-a', 'reader'),
          backgroundTasksChangedLine([agent]),
          toolUseLine('msg-a-read', 'tu-a-read', 'Read', { file_path: 'README.md' }, 'tu-a'),
          ...openTextLines('msg-a-text', 'Half', 'tu-a'),
          RESULT_LINE,
        ]),
        { waitSignal: { name: 'finish' } },
        emit([
          ...finishTextLines('msg-a-text', ' done', 'Half done', 'tu-a'),
          toolResultLine('tu-a-read', '# fixture', { parentToolUseId: 'tu-a' }),
          ...textLines('msg-a-report', 'Read the README.', 'tu-a'),
          taskUpdatedLine('task-a', { status: 'completed', end_time: 1787419835322 }),
          taskNotificationLine('task-a', 'tu-a', 'Read the README.', { outputFile: '${CWD}/a.jsonl', uuid: 'done-a' }),
          backgroundTasksChangedLine([]),
        ]),
      ],
    ]),
  });

  const threadId = await seedAgentThread(harness, 'outlive-app', 'Agent outlives turn');
  const mockId = await startMock(harness, threadId);
  const completed = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'read the readme', null);
  await completed;

  const agentRows = async () => {
    const items = await listItems(harness, threadId);
    const read = items.find((i) => i.id === 'tu-a-read');
    return {
      read: read?.status ?? 'none',
      readUnresolved: read?.summary.includes('unresolved') ?? false,
      texts: items.filter((i) => i.parentId === 'tu-a' && i.kind === 'assistant_text').map((t) => `${t.status}:${t.summary}`),
      sibling: items.find((i) => i.completionOf === 'tu-a')?.status ?? 'none',
      unsettled: unsettled(items),
    };
  };
  // The parent's result settled the main thread's rows only.
  await expect.poll(agentRows).toEqual({
    read: 'running',
    readUnresolved: false,
    texts: ['streaming:Half'],
    sibling: 'none',
    unsettled: ['text:1:tu-a:1:streaming', 'tu-a-read:running'],
  });

  await waitForGate(harness, 'finish');
  await advance(harness, mockId, 'finish');
  await expect.poll(agentRows).toEqual({
    read: 'completed',
    readUnresolved: false,
    texts: ['completed:Half done', 'completed:Read the README.'],
    sibling: 'completed',
    unsettled: [],
  });
});

test('Stop kills agents launched in an earlier turn: every row under each settles stopped, none parks', async ({
  harness,
  page,
}) => {
  const agents = ['a', 'b'].map((n) => ({
    n,
    tu: `tu-${n}`,
    taskId: `task-${n}`,
    description: `worker ${n}`,
    shellTu: `tu-${n}-shell`,
    shellTask: `task-${n}-shell`,
  }));
  const agentTasks = agents.map((a) => ({ task_id: a.taskId, task_type: 'local_agent', description: a.description }));
  const shellTasks = agents.map((a) => ({ task_id: a.shellTask, task_type: 'local_bash', description: `watch ${a.n}` }));
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('stop-earlier-agents', [
      [
        emit([
          ...textLines('msg-lead', 'Launching two workers.'),
          ...agents.flatMap((a) => agentLaunchLines(a.tu, a.taskId, a.description)),
          backgroundTasksChangedLine(agentTasks),
          RESULT_LINE,
        ]),
        // After the launching turn ended, each agent starts a shell it
        // owns, calls a tool and starts to answer; none of it finishes.
        { waitSignal: { name: 'work' } },
        emit([
          ...agents.flatMap((a) => [
            toolUseLine(`msg-${a.n}-shell`, a.shellTu, 'Bash', { command: `watch ${a.n}`, run_in_background: true }, a.tu),
            taskStartedLine(a.shellTask, a.shellTu, `watch ${a.n}`, { taskType: 'local_bash', ownedBySubagent: true }),
            shellBackgroundAckLine(a.shellTu, a.shellTask, a.tu),
            toolUseLine(`msg-${a.n}-read`, `${a.tu}-read`, 'Read', { file_path: 'README.md' }, a.tu),
            ...openTextLines(`msg-${a.n}-text`, `Worker ${a.n} is reading`, a.tu),
          ]),
          backgroundTasksChangedLine([...agentTasks, ...shellTasks]),
        ]),
      ],
      [
        // The second turn streams and stays open until the Stop.
        emit(openTextLines('msg-second', 'Thinking about the next step')),
        { waitSignal: { name: 'never' } },
      ],
    ]),
  });

  const threadId = await seedAgentThread(harness, 'stop-earlier-app', 'Stop earlier agents');
  await harness.open(page);
  await page.getByText('Stop earlier agents').click();
  const mockId = await startMock(harness, threadId);
  const firstCompleted = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'launch two workers', null);
  await firstCompleted;
  await waitForGate(harness, 'work');
  await advance(harness, mockId, 'work');
  // Per agent: its Read call and its open text.
  await expect.poll(async () => unsettled(await listItems(harness, threadId)).length).toBe(agents.length * 2);
  await expect(page.getByTestId('activity-rail-background-toggle')).toBeVisible();

  await harness.rpc('SendMessage', threadId, 'what next', null);
  await waitForGate(harness, 'never');
  const stop = page.getByRole('button', { name: 'Interrupt current turn', exact: true });
  await expect(stop).toBeVisible();
  const stopped = harness.waitForEvent('provider:turn_completed');
  await stop.click();
  await expect(page.getByTestId('background-kill-dialog')).toBeVisible();
  await expect(page.getByTestId('background-kill-agent')).toHaveCount(agents.length);
  await page.getByTestId('background-kill-confirm').click();
  await stopped;

  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      return {
        unsettled: unsettled(items),
        siblings: items.filter((i) => i.completionOf).map((i) => `${i.completionOf}:${i.status}`).sort(),
        parkedBells: items.filter((i) => itemMeta(i).kind === 'parked_agent').map((i) => i.id),
        agentRows: agents
          .flatMap((a) =>
            items
              .filter((i) => i.parentId === a.tu && (i.id === `${a.tu}-read` || i.kind === 'assistant_text'))
              .map((i) => `${a.tu}:${i.kind}:${i.status}:${i.summary.endsWith('stopped')}`),
          )
          .sort(),
      };
    })
    .toEqual({
      unsettled: [],
      siblings: agents.flatMap((a) => [`${a.tu}:killed`, `${a.shellTu}:killed`]).sort(),
      parkedBells: [],
      agentRows: agents
        .flatMap((a) => [`${a.tu}:assistant_text:errored:true`, `${a.tu}:tool_call:errored:true`])
        .sort(),
    });
  await expect(page.getByTestId('activity-rail-background-toggle')).toHaveCount(0);
  const cards = page.locator(timelineSelector).getByTestId('subagent-group');
  await expect(cards).toHaveCount(agents.length);
  for (let i = 0; i < agents.length; i++) {
    await expect(cards.nth(i).getByTestId('subagent-group-error')).toContainText('Agent stopped');
  }
});
