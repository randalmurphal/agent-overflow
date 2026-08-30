// Agent visibility — the TREE half of the spec's success criteria
// (docs/specs/agent-visibility.md § "Success criteria", items 1 and 2),
// driven through the real backend and the real SPA.
//
//   1. A forked `code-review` renders as ONE `skill` card and none of its
//      tool calls — nor any of its fan-out children's — appear as the main
//      agent's.
//   2. A depth-2 background agent renders as a running card under its
//      parent's card, with the live tool count and activity line
//      `system/task_progress` supplies, and appears in the background tray
//      indented under its parent.
//
// Both open the UI BEFORE the live turn starts: `task_progress` ticks are
// in-memory frontend state (nothing persists until the terminal), so a tick
// emitted before the page subscribed is gone for good.
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

const FORK_DIFF_COMMAND = 'git diff HEAD~2 --stat';

test.beforeEach(async ({ harness }) => {
  // Attribution assertions need the cards mounted; collapse behavior has its
  // own coverage and the product default is intentionally collapsed now.
  await harness.rpc('UpdateSettings', { activityRunDefault: 'expanded' });
});

test('a forked code-review skill is one skill card and nothing it does is the main agent’s', async ({
  harness,
  page,
}) => {
  // §E9: a forked Skill gets no task_started and no task id at all. Its
  // rows are attributed to the Skill tool_use and its completion's
  // `tool_use_result {status:"forked", agentId, commandName}` is the only
  // identity statement — which is also what binds the fan-out beneath it.
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('fork-code-review', [
      emit([
        ...textLines('msg-lead', 'Running the code-review skill.'),
        toolUseLine('msg-skill', 'tu-skill', 'Skill', {
          skill: 'code-review',
          args: 'medium --base HEAD~2',
        }),

        // The fork's own tool call.
        toolUseLine('msg-fork-diff', 'tu-fork-diff', 'Bash', { command: FORK_DIFF_COMMAND }, 'tu-skill'),
        toolResultLine('tu-fork-diff', ' 5 files changed, 210 insertions(+)', {
          parentToolUseId: 'tu-skill',
        }),

        // Fan-out: two agents the FORK launches, each with its own tool call.
        toolUseLine('msg-angle-a', 'tu-angle-a', 'Agent', {
          description: 'Angle A: correctness',
          subagent_type: 'angle-a',
        }, 'tu-skill'),
        taskStartedLine('task-angle-a', 'tu-angle-a', 'Angle A: correctness'),
        asyncAgentAckLine('tu-angle-a', 'task-angle-a', 'Angle A: correctness', 'tu-skill'),
        toolUseLine('msg-angle-a-read', 'tu-angle-a-read', 'Read', {
          file_path: 'internal/provider/claude/parser.go',
        }, 'tu-angle-a'),
        toolResultLine('tu-angle-a-read', 'package claude', { parentToolUseId: 'tu-angle-a' }),

        toolUseLine('msg-angle-b', 'tu-angle-b', 'Agent', {
          description: 'Angle B: perf',
          subagent_type: 'angle-b',
        }, 'tu-skill'),
        taskStartedLine('task-angle-b', 'tu-angle-b', 'Angle B: perf'),
        asyncAgentAckLine('tu-angle-b', 'task-angle-b', 'Angle B: perf', 'tu-skill'),
        toolUseLine('msg-angle-b-grep', 'tu-angle-b-grep', 'Grep', { pattern: 'allocate' }, 'tu-angle-b'),
        toolResultLine('tu-angle-b-grep', 'no matches', { parentToolUseId: 'tu-angle-b' }),

        // Both fan-out children settle.
        taskUpdatedLine('task-angle-a', { status: 'completed', end_time: 1787415964724 }),
        taskNotificationLine('task-angle-a', 'tu-angle-a', 'Angle A verdict: correct.'),
        taskUpdatedLine('task-angle-b', { status: 'completed', end_time: 1787415964725 }),
        taskNotificationLine('task-angle-b', 'tu-angle-b', 'Angle B verdict: no regressions.'),

        // The fork closes. `status:"forked"` is the whole signal.
        toolResultLine('tu-skill', 'Skill "code-review" completed (forked execution).\n\nResult:\nNo blocking findings.', {
          toolUseResult: {
            success: true,
            commandName: 'code-review',
            status: 'forked',
            agentId: 'fork-agent-1',
            result: 'No blocking findings.',
          },
        }),
        ...textLines('msg-final', 'The review found nothing blocking.'),
        RESULT_LINE,
      ]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'fork-app', 'Forked skill');
  await page.goto(harness.url);
  await page.getByText('Forked skill').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'review the change', null);
  await harness.waitForEvent('provider:turn_completed');

  // --- Attribution, at the row level -------------------------------
  // Nothing the fork or its fan-out produced may sit at the top level.
  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      const skill = items.filter((i) => i.kind === 'tool_call' && i.toolName === 'Skill');
      if (skill.length !== 1) return null;
      return {
        topLevelToolCalls: items
          .filter((i) => i.kind === 'tool_call' && !i.parentId)
          .map((i) => i.toolName)
          .sort(),
        underFork: items
          .filter((i) => i.kind === 'tool_call' && i.parentId === skill[0].id)
          .map((i) => i.toolName)
          .sort(),
        underAngles: items
          .filter(
            (i) =>
              i.kind === 'tool_call' &&
              (i.parentId === 'tu-angle-a' || i.parentId === 'tu-angle-b'),
          )
          .map((i) => i.toolName)
          .sort(),
      };
    })
    .toEqual({
      topLevelToolCalls: ['Skill'],
      underFork: ['Agent', 'Agent', 'Bash'],
      underAngles: ['Grep', 'Read'],
    });

  // --- One card, in the timeline -----------------------------------
  const timeline = page.getByTestId('message-timeline-scroll');
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(1);
  await expect(timeline.getByTestId('subagent-group-kind')).toHaveText('skill');
  await expect(timeline.getByTestId('subagent-group-label')).toContainText('code-review');

  // Collapsed, the fork's work is nowhere on the main timeline — and the
  // old expandable transcript is gone entirely (component deleted).
  await expect(timeline.getByText(FORK_DIFF_COMMAND)).toHaveCount(0);
  await expect(page.getByTestId('claude-subagent-transcript')).toHaveCount(0);

  // Expanded, the fork's OWN work lives inside the card (its Bash row) —
  // but its fan-out children do not render as nested cards: the digest
  // never recursively embeds child agents (ed6d2b40; spec "It never
  // recursively embeds child agents in the main thread"). The children
  // live in the agent pane as direct child rows.
  await timeline.getByTestId('subagent-group-toggle').click();
  const body = timeline.getByTestId('subagent-group-body').first();
  await expect(body.getByText(FORK_DIFF_COMMAND)).toBeVisible();
  await expect(body.getByTestId('subagent-group')).toHaveCount(0);
  // The fan-out children render as their spawn ROWS (the same agent row
  // the main timeline gets), never as cards.
  await expect(body.locator('[data-item-id="tu-angle-a"]').getByTestId('agent-row-preview')).toContainText('Angle A');
  await expect(body.locator('[data-item-id="tu-angle-b"]').getByTestId('agent-row-preview')).toContainText('Angle B');
  // One kind chip on the page: the fork's own `skill`.
  await expect(timeline.getByTestId('subagent-group-kind')).toHaveText(['skill']);
});

test('a depth-2 background agent nests under its parent card and indents in the tray', async ({
  harness,
  page,
}) => {
  // Parent and child are BOTH backgrounded, which is what puts the child
  // at tray depth 1: the tray lists by backgrounded ancestry, and a
  // background launch under a FOREGROUND agent is deliberately a tray root
  // (utils/backgroundTray.ts).
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('depth-2-background', [
      emit([
        ...textLines('msg-lead', 'Launching the outer reviewer.'),
        toolUseLine('msg-outer', 'tu-outer', 'Agent', {
          description: 'outer reviewer',
          subagent_type: 'outer-reviewer',
        }),
        taskStartedLine('task-outer', 'tu-outer', 'outer reviewer'),
        asyncAgentAckLine('tu-outer', 'task-outer', 'outer reviewer'),
        backgroundTasksChangedLine([
          { task_id: 'task-outer', task_type: 'local_agent', description: 'outer reviewer' },
        ]),

        // The outer agent launches its own agent: depth 2.
        toolUseLine('msg-inner', 'tu-inner', 'Agent', {
          description: 'inner scanner',
          subagent_type: 'inner-scanner',
        }, 'tu-outer'),
        taskStartedLine('task-inner', 'tu-inner', 'inner scanner', { ownedBySubagent: true }),
        asyncAgentAckLine('tu-inner', 'task-inner', 'inner scanner', 'tu-outer'),
        backgroundTasksChangedLine([
          { task_id: 'task-outer', task_type: 'local_agent', description: 'outer reviewer' },
          { task_id: 'task-inner', task_type: 'local_agent', description: 'inner scanner' },
        ]),
        RESULT_LINE,
      ]),
      // Held so the page is provably subscribed before the live tick.
      { waitSignal: { name: 'tick' } },
      emit([
        taskProgressLine(
          'task-inner',
          'tu-inner',
          'Scanning the parser for drift',
          { total_tokens: 18227, tool_uses: 3, duration_ms: 2368 },
          'Grep',
        ),
      ]),
      // Both agents settle only after the running-state assertions. A
      // background agent left running outlives the test: `HarnessReset`
      // stops sessions and settles in-flight TURNS, but a running
      // background tool_call survives both and then blocks the project
      // delete ("cannot delete project while thread ... is running"),
      // which fails the NEXT test in this worker rather than this one.
      { waitSignal: { name: 'settle' } },
      emit([
        taskUpdatedLine('task-inner', { status: 'completed', end_time: 1787415964724 }),
        taskNotificationLine('task-inner', 'tu-inner', 'Inner scan done.'),
        taskUpdatedLine('task-outer', { status: 'completed', end_time: 1787415964725 }),
        taskNotificationLine('task-outer', 'tu-outer', 'Outer review done.'),
        backgroundTasksChangedLine([]),
      ]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'depth2-app', 'Nested background');
  await page.goto(harness.url);
  await page.getByText('Nested background').click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'review deeply', null);

  // The main turn returns while both agents keep running in the background.
  await harness.waitForEvent('provider:turn_completed');

  // While both run, the main timeline shows only the OUTER spawn record:
  // an immutable compact row, no card (ruling 2026-08-23 — a background
  // launch's card renders at its completion point, and the inner agent's
  // rows are the outer agent's transcript, never the main thread's).
  const timeline = page.getByTestId('message-timeline-scroll');
  const outerSpawnRow = timeline.locator('[data-item-id="tu-outer"]');
  await expect(outerSpawnRow.getByTestId('agent-row-preview')).toContainText('Outer Reviewer');
  await expect(outerSpawnRow.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'backgrounded');
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(0);
  await expect(timeline.locator('[data-item-id="tu-inner"]')).toHaveCount(0);

  // The background tray lists both, the child indented under its parent.
  await page.getByTestId('activity-rail-background-toggle').click();
  const trayRows = page.getByTestId('background-task-tray-row');
  await expect(trayRows).toHaveCount(2);
  await expect(trayRows.nth(0)).toHaveAttribute('data-depth', '0');
  await expect(trayRows.nth(0)).toHaveAttribute('data-row-id', 'tu-outer');
  await expect(trayRows.nth(1)).toHaveAttribute('data-depth', '1');
  await expect(trayRows.nth(1)).toHaveAttribute('data-row-id', 'tu-inner');
  // Indentation is the visual half of the same claim.
  const [outerBox, innerBox] = await Promise.all([
    trayRows.nth(0).boundingBox(),
    trayRows.nth(1).boundingBox(),
  ]);
  expect(innerBox!.x).toBeGreaterThan(outerBox!.x);

  // The live surface for a running background agent is its PANE: the
  // tray's open button scopes the companion to the inner agent, whose
  // composer shell shows the working chip with its elapsed timer.
  await trayRows.nth(1).getByTestId('background-task-tray-row-open').click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByTestId('agent-pane-breadcrumb-current')).toContainText('Inner Scanner');
  const shell = pane.getByTestId('agent-pane-composer-shell');
  await expect(shell.getByTestId('agent-pane-working')).toBeVisible();
  await expect(shell.getByTestId('agent-pane-working-elapsed')).toHaveText(/^\d+s$/);
  await expect(shell.getByTestId('agent-pane-activity-reserve')).toHaveCount(0);

  // Now the live tick, with the page already watching: the inner agent's
  // own token spend reaches its pane's usage slot.
  await waitForGate(harness, 'tick');
  await advance(harness, mockId, 'tick');
  await expect(pane.getByTestId('workspace-strip-usage')).toHaveText('18.2k');
  // ...and the tray row, the agent's other live surface, shows the tick's
  // tool count, tokens and activity line (user ruling 2026-08-23).
  await expect(trayRows.nth(1).getByTestId('background-task-tray-row-tools')).toHaveText('3 tools');
  await expect(trayRows.nth(1).getByTestId('background-task-tray-row-tokens')).toHaveText('18.2k tokens');
  await expect(trayRows.nth(1).getByTestId('background-task-tray-row-activity')).toContainText(
    'Scanning the parser for drift',
  );

  // Both settle: the tray is driven by the level set, so an empty
  // `background_tasks_changed` empties it — and the OUTER card appears at
  // its completion point on the main timeline. The inner agent does NOT
  // become a nested card inside it: the digest never recursively embeds
  // child agents (ed6d2b40) — the inner agent's surface is the agent
  // pane, reached from the outer pane's child row.
  await waitForGate(harness, 'settle');
  await advance(harness, mockId, 'settle');
  await expect(trayRows).toHaveCount(0);
  await expect(shell.getByTestId('agent-pane-working')).toHaveCount(0);
  await expect(shell.getByTestId('agent-pane-activity-reserve')).toHaveCount(1);

  await expect(timeline.getByTestId('subagent-group')).toHaveCount(1);
  const outerCard = timeline.getByTestId('subagent-group').first();
  await expect(outerCard.getByTestId('subagent-group-label')).toContainText('Outer Reviewer');
  await expect(outerCard).toHaveAttribute('data-background', 'true');
  await outerCard.getByTestId('subagent-group-toggle').first().click();
  const outerBody = outerCard.getByTestId('subagent-group-body').first();
  await expect(outerBody).toBeVisible();
  await expect(outerBody.getByTestId('subagent-group')).toHaveCount(0);
  // The inner launch renders as its spawn ROW inside the body, door and all.
  const innerSpawnRow = outerBody.locator('[data-item-id="tu-inner"]');
  await expect(innerSpawnRow.getByTestId('agent-row-preview')).toContainText('Inner Scanner');
  await expect(innerSpawnRow.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'backgrounded');
});
