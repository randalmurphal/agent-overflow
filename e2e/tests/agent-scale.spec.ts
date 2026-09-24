// One turn launches 100 async agents and every one of them streams and
// settles (docs/specs/agent-visibility.md, the scale bar: 1 to 100
// subagents with the same per-event cost). What must hold at 100:
//
//   launched  - the tray counts 100 and lists a row per agent; the main
//               timeline holds only the immutable spawn rows.
//   streaming - every child row lands under its own launch: none of the
//               300 sidechain rows reaches the main timeline, and a pane
//               opened on demand shows exactly its agent's rows.
//   settled   - 100 completion siblings, the tray closes, and every
//               launch is closed in the store.
//
// The mock emits each phase as one burst (5 ms between lines), so the
// spec is also the load check: the streaming burst is 100 agents' rows
// arriving at the rate the CLI writes them.
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeScenario,
  emit,
  listItems,
  seedAgentThread,
  startMock,
  taskNotificationLine,
  taskProgressLine,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';

const AGENTS = 100;
const SAMPLE = 42;
const indexes = Array.from({ length: AGENTS }, (_, i) => i);
const tu = (i: number) => `tu-${i}`;
const readId = (i: number) => `tu-${i}-read`;
const taskId = (i: number) => `task-${i}`;
const description = (i: number) => `shard ${i} sweep`;
const scanning = (i: number) => `Shard ${i}: scanning.`;
const report = (i: number) => `Shard ${i} is clean.`;
const tasks = indexes.map((i) => ({ task_id: taskId(i), task_type: 'local_agent', description: description(i) }));

test('one turn launches 100 background agents; each streams to its own scope and settles', async ({
  harness,
  page,
}) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('agent-scale', [
      emit([
        ...textLines('msg-lead', 'Sweeping every shard in parallel.'),
        ...indexes.flatMap((i) => [
          toolUseLine(`msg-launch-${i}`, tu(i), 'Agent', {
            description: description(i),
            subagent_type: 'sweeper',
            prompt: `Sweep shard ${i} and report.`,
          }),
          taskStartedLine(taskId(i), tu(i), description(i)),
          asyncAgentAckLine(tu(i), taskId(i), description(i)),
        ]),
        backgroundTasksChangedLine(tasks),
        RESULT_LINE,
      ]),
      // Every agent works at once: a text row, a tool call and its
      // result, each on the parent stream with parent_tool_use_id, then
      // the CLI's progress tick for the agent.
      { waitSignal: { name: 'stream' } },
      emit(
        indexes.flatMap((i) => [
          ...textLines(`msg-scan-${i}`, scanning(i), tu(i)),
          toolUseLine(`msg-read-${i}`, readId(i), 'Read', { file_path: `shards/shard-${i}.txt` }, tu(i)),
          toolResultLine(readId(i), `shard ${i}: 0 drifts`, { parentToolUseId: tu(i) }),
          taskProgressLine(taskId(i), tu(i), scanning(i), { total_tokens: 1000 * (i + 1), tool_uses: 1, duration_ms: 500 + i }, 'Read'),
        ]),
      ),
      // Every agent reports and stops; the CLI's level set shrinks by one
      // per completion, so the tray is nudged 100 times.
      { waitSignal: { name: 'finish' } },
      emit(
        indexes.flatMap((i) => [
          ...textLines(`msg-report-${i}`, report(i), tu(i)),
          taskUpdatedLine(taskId(i), { status: 'completed', end_time: 1787419835322 + i }),
          taskNotificationLine(taskId(i), tu(i), report(i), {
            outputFile: `\${CWD}/shard-${i}.jsonl`,
            usage: { total_tokens: 4000 + i, tool_uses: 1, duration_ms: 900 + i },
            uuid: `done-${i}`,
          }),
          backgroundTasksChangedLine(tasks.slice(i + 1)),
        ]),
      ),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'scale-app', 'Agent scale');
  await harness.open(page);
  await page.getByText('Agent scale').click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'sweep every shard', null);
  await harness.waitForEvent('provider:turn_completed');

  // --- Launched -------------------------------------------------------
  const timeline = page.locator(
    '[data-testid="message-timeline-scroll"]:not([data-testid="agent-pane-timeline"] [data-testid="message-timeline-scroll"])',
  );
  await expect(page.getByTestId('activity-rail-background-count')).toHaveText(String(AGENTS));
  await page.getByTestId('activity-rail-background-toggle').click();
  const trayRows = page.getByTestId('background-task-tray-row');
  await expect(trayRows).toHaveCount(AGENTS);
  await expect(page.getByTestId('activity-rail-background-running-label')).toHaveText(`${AGENTS} running`);
  const sampleTray = page.locator(`[data-testid="background-task-tray-row"][data-row-id="${tu(SAMPLE)}"]`);
  await expect(sampleTray.getByTestId('background-task-tray-row-status')).toHaveAttribute('data-run-state', 'running');
  await expect(sampleTray.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'backgrounded');
  // The spawn rows are the main timeline's only new activity: no card.
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(0);
  await expect(timeline.getByTestId('agent-row').first()).toBeVisible();
  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      const launches = items.filter((i) => i.kind === 'tool_call' && i.toolName === 'Agent' && !i.parentId);
      const children = items.filter((i) => i.parentId);
      return {
        launches: launches.length,
        background: launches.filter((i) => i.isBackground).length,
        // Each scope opens with the launch prompt as its first row
        // (persistProvisionalSubagentPrompt) and nothing else yet.
        children: children.length,
        childKinds: [...new Set(children.map((i) => i.kind))],
      };
    })
    .toEqual({ launches: AGENTS, background: AGENTS, children: AGENTS, childKinds: ['user_text'] });

  // --- Streaming ------------------------------------------------------
  await waitForGate(harness, 'stream');
  await advance(harness, mockId, 'stream');
  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      const byParent = new Map<string, number>();
      for (const item of items) {
        if (!item.parentId) continue;
        byParent.set(item.parentId, (byParent.get(item.parentId) ?? 0) + 1);
      }
      return {
        scopes: byParent.size,
        rowsPerScope: [...new Set(byParent.values())],
        sampleRead: items.find((i) => i.id === readId(SAMPLE))?.parentId ?? '',
      };
    })
    .toEqual({ scopes: AGENTS, rowsPerScope: [3], sampleRead: tu(SAMPLE) });
  // Not one child row on the main timeline, and still no card.
  await expect(timeline.getByRole('link', { name: /^Open shard-\d+\.txt in editor$/ })).toHaveCount(0);
  await expect(timeline.getByText(scanning(SAMPLE))).toHaveCount(0);
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(0);
  await expect(trayRows).toHaveCount(AGENTS);
  // The tray row is the running agent's live surface: its tick's tool
  // count, tokens and activity line, read from no child row.
  await expect(sampleTray.getByTestId('background-task-tray-row-tools')).toHaveText('1 tool');
  await expect(sampleTray.getByTestId('background-task-tray-row-tokens')).toHaveText(`${SAMPLE + 1}.0k tokens`);
  await expect(sampleTray.getByTestId('background-task-tray-row-activity')).toHaveText(scanning(SAMPLE));
  // The pane loads one agent's rows on demand, and only that agent's.
  await sampleTray.getByTestId('background-task-tray-row-open').click();
  const pane = page.getByTestId('companion-pane-agent-body');
  const paneTimeline = pane.getByTestId('agent-pane-timeline');
  await expect(paneTimeline.getByText(scanning(SAMPLE))).toBeVisible();
  await expect(paneTimeline.getByRole('link', { name: `Open shard-${SAMPLE}.txt in editor` })).toBeVisible();
  await expect(paneTimeline.getByRole('link', { name: /^Open shard-\d+\.txt in editor$/ })).toHaveCount(1);
  await expect(paneTimeline.getByText(scanning(SAMPLE + 1))).toHaveCount(0);
  await expect(pane.getByTestId('agent-pane-working')).toBeVisible();

  // --- Settled --------------------------------------------------------
  await waitForGate(harness, 'finish');
  await advance(harness, mockId, 'finish');
  await expect(page.getByTestId('activity-rail-background-toggle')).toHaveCount(0);
  await expect(paneTimeline.getByText(report(SAMPLE))).toBeVisible();
  await expect(pane.getByTestId('agent-pane-working')).toHaveCount(0);
  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      const siblings = items.filter((i) => i.completionOf);
      return {
        siblings: siblings.length,
        completed: siblings.filter((i) => i.status === 'completed').length,
        launchesCompleted: new Set(siblings.map((i) => i.completionOf)).size,
        childrenPerScope: [
          ...new Set(
            indexes.map((i) => items.filter((item) => item.parentId === tu(i)).length),
          ),
        ],
      };
    })
    .toEqual({ siblings: AGENTS, completed: AGENTS, launchesCompleted: AGENTS, childrenPerScope: [4] });
  // The cards land at the completion siblings; the last one is at the
  // tail, where the timeline follows.
  const lastCard = timeline.getByTestId('subagent-group').last();
  await expect(lastCard).toHaveAttribute('data-background', 'true');
  await expect(lastCard.getByTestId('subagent-group-preview')).toContainText(report(AGENTS - 1));
});
