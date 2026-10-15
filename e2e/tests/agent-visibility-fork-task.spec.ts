// Forked skills as a task (claude-wire.md §E9, Claude 2.1.284+): the fork
// is announced by a `system/task_started` bound to its Skill call and ends
// with a `task_notification` alone, then the forked result.
//
//   1. The fork is an awaited agent: its Skill row settles in place on the
//      forked result, never joins the background set, and leaves no
//      background tray entry or completion sibling.
//   2. The fork's pane offers Stop while it runs, as an awaited Agent's
//      does. Stop sends the fork's task id; the CLI kills the fork, the
//      Skill call answers with an error, and the main turn goes on.
//   3. The turn's Stop ends a running fork with the turn.
//   4. A typed forked command (`/code-review high`) is the same agent: its
//      command row becomes the fork's card once the session mirror proves
//      the fork, settles when the fork ends, and its pane Stop stops it by
//      the task id the mirror gave it.
//
// The mock writes the CLI's stop frames for an awaited agent
// (cmd/ao-mockprovider/claude_tasks.go; forked_skill_stop_task,
// forked_skill_interrupt and forked_skill_direct_command_stop_task,
// all _20261014).
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  claudeScenario,
  emit,
  listItems,
  seedAgentThread,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  textLines,
  toolResultLine,
  toolUseLine,
  transcriptMirrorLine,
  waitForGate,
} from './agent-visibility-helpers.js';
import type { HarnessApp } from '../src/harness.js';

const FORK_COMMAND = 'git diff HEAD~1 --stat';
const FORK_REPORT = 'No blocking findings.';

function forkLaunchLines(): string[] {
  return [
    ...textLines('msg-lead', 'Running the code-review skill.'),
    toolUseLine('msg-skill', 'tu-skill', 'Skill', { skill: 'code-review', args: 'high' }),
    taskStartedLine('task-fork', 'tu-skill', '/code-review'),
    toolUseLine('msg-fork-diff', 'tu-fork-diff', 'Bash', { command: FORK_COMMAND }, 'tu-skill'),
  ];
}

test('a forked skill on a task settles in place and leaves no background row', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('fork-task', [
      emit([
        ...forkLaunchLines(),
        toolResultLine('tu-fork-diff', ' 3 files changed, 40 insertions(+)', { parentToolUseId: 'tu-skill' }),
        ...textLines('msg-fork-final', FORK_REPORT, 'tu-skill'),
        taskNotificationLine('task-fork', 'tu-skill', '/code-review'),
        toolResultLine('tu-skill', `Skill "code-review" completed (forked execution).\n\nResult:\n${FORK_REPORT}`, {
          toolUseResult: {
            success: true,
            commandName: 'code-review',
            status: 'forked',
            agentId: 'task-fork',
            result: FORK_REPORT,
          },
        }),
        ...textLines('msg-final', 'The review found nothing blocking.'),
        RESULT_LINE,
      ]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'fork-task-app', 'Fork task');
  await harness.open(page);
  await page.getByText('Fork task').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'review the change', null);
  await harness.waitForEvent('provider:turn_completed');

  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      const skill = items.find((i) => i.id === 'tu-skill');
      return {
        status: skill?.status,
        background: skill?.isBackground ?? false,
        siblings: items.filter((i) => i.completionOf === 'tu-skill').length,
        underFork: items.filter((i) => i.kind === 'tool_call' && i.parentId === 'tu-skill').map((i) => i.toolName),
      };
    })
    .toEqual({ status: 'completed', background: false, siblings: 0, underFork: ['Bash'] });

  const timeline = page.getByTestId('message-timeline-scroll');
  const card = timeline.getByTestId('subagent-group');
  await expect(card).toHaveCount(1);
  await expect(card).toHaveAttribute('data-status', 'completed');
  await expect(card).not.toHaveAttribute('data-background', 'true');
  await expect(card.getByTestId('subagent-group-kind')).toHaveText('skill');
  await expect(card.getByTestId('subagent-group-label')).toContainText('code-review');
  await expect(page.getByTestId('background-task-tray-row')).toHaveCount(0);
  await expect(page.getByTestId('composer-send')).toBeVisible();
});

test("Stop in a running fork's pane stops the fork and the main turn goes on", async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('fork-task-stop', [
      emit(forkLaunchLines()),
      // The fork runs its Bash call; the mock answers the pane's
      // stop_task with the CLI's kill frames and the Skill's error result.
      { waitSignal: { name: 'stopped' } },
      emit([...textLines('msg-after', 'The review was stopped before it finished.'), RESULT_LINE]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'fork-stop-app', 'Fork stop');
  await harness.open(page);
  await page.getByText('Fork stop').click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'review the change', null);
  await waitForGate(harness, 'stopped');

  const timeline = page.getByTestId('message-timeline-scroll');
  const card = timeline.getByTestId('subagent-group').first();
  await expect(card).toHaveAttribute('data-status', 'running');
  await expect(card.getByTestId('subagent-group-kind')).toHaveText('skill');
  // An awaited agent: no background button on the card.
  await card.hover();
  await expect(card.getByTestId('subagent-group-background-button')).toHaveCount(0);

  await card.getByTestId('subagent-group-open-pane').first().click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByTestId('agent-pane-stop')).toBeVisible();
  await pane.getByTestId('agent-pane-stop').getByTestId('composer-interrupt').click();
  await expect(pane.getByTestId('agent-pane-stop-error')).toHaveCount(0);

  // The CLI's answer lands: the fork is killed and the Skill call errors.
  await expect
    .poll(async () => (await listItems(harness, threadId)).find((i) => i.id === 'tu-skill')?.status)
    .toBe('errored');
  await expect(pane.getByTestId('agent-pane-stop')).toHaveCount(0);

  // The main turn was never interrupted: it goes on and completes.
  await advance(harness, mockId, 'stopped');
  await harness.waitForEvent('provider:turn_completed');
  await expect(timeline.getByText('The review was stopped before it finished.')).toBeVisible();
  await expect(page.getByTestId('composer-send')).toBeVisible();
  await expect(page.getByTestId('background-task-tray-row')).toHaveCount(0);
  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      return items.filter((i) => i.status === 'running').map((i) => i.id);
    })
    .toEqual([]);
});

test("the turn's Stop ends a running fork with the turn", async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('fork-task-interrupt', [emit(forkLaunchLines()), { waitSignal: { name: 'never' } }]),
  });

  const threadId = await seedAgentThread(harness, 'fork-interrupt-app', 'Fork interrupt');
  await harness.open(page);
  await page.getByText('Fork interrupt').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'review the change', null);

  const timeline = page.getByTestId('message-timeline-scroll');
  const card = timeline.getByTestId('subagent-group').first();
  await expect(card).toHaveAttribute('data-status', 'running');
  const completed = harness.waitForEvent('provider:turn_completed');
  await page.getByTestId('composer-interrupt').click();
  await completed;

  // The CLI stops the fork and answers the Skill call as a rejected tool use.
  await expect(card).toHaveAttribute('data-status', 'errored');
  await expect(page.getByTestId('composer-send')).toBeVisible();
  await expect(page.getByTestId('background-task-tray-row')).toHaveCount(0);
  await expect
    .poll(async () => (await listItems(harness, threadId)).filter((i) => i.status === 'running').map((i) => i.id))
    .toEqual([]);
});

const TYPED_TASK = 'task-typed-fork';

/** A typed `/code-review high`: the fork's task names no tool_use, and its
 * rows arrive only through the session mirror, which proves the fork with
 * an agent_metadata row that names no tool_use and rows that name the
 * skill (forked_skill_direct_command_20261014). */
function typedForkLines(): string[] {
  return [
    taskStartedLine(TYPED_TASK, '', '/code-review'),
    transcriptMirrorLine(
      TYPED_TASK,
      [{ text: 'Reviewing the change.' }, { tool: { id: 'tu-typed-read', name: 'Read', result: '# fixture' } }],
      { metadata: true, attributionSkill: 'code-review' },
    ),
  ];
}

async function typedForkRow(harness: HarnessApp, threadId: string) {
  return (await listItems(harness, threadId)).find((i) => i.kind === 'tool_call' && i.toolName === 'Skill');
}

test('a typed forked command becomes the fork card and settles with the fork', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('typed-fork', [
      emit([...typedForkLines(), taskNotificationLine(TYPED_TASK, '', '/code-review'), RESULT_LINE]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'typed-fork-app', 'Typed fork');
  await harness.open(page);
  await page.getByText('Typed fork').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, '/code-review high', null);
  await harness.waitForEvent('provider:turn_completed');

  await expect
    .poll(async () => {
      const row = await typedForkRow(harness, threadId);
      const items = await listItems(harness, threadId);
      return {
        status: row?.status,
        background: row?.isBackground ?? false,
        underFork: items.filter((i) => i.kind === 'tool_call' && row && i.parentId === row.id).map((i) => i.toolName),
      };
    })
    .toEqual({ status: 'completed', background: false, underFork: ['Read'] });

  const card = page.getByTestId('message-timeline-scroll').getByTestId('subagent-group');
  await expect(card).toHaveCount(1);
  await expect(card).toHaveAttribute('data-status', 'completed');
  await expect(card.getByTestId('subagent-group-kind')).toHaveText('skill');
  await expect(card.getByTestId('subagent-group-label')).toContainText('code-review');
  await expect(page.getByTestId('background-task-tray-row')).toHaveCount(0);
});

test("Stop in a typed fork's pane stops the fork by its task id", async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('typed-fork-stop', [
      emit(typedForkLines()),
      { waitSignal: { name: 'stopped' } },
      // After the stop the CLI ends the command's turn: no launch exists
      // to answer (forked_skill_direct_command_stop_task_20261014).
      emit([RESULT_LINE]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'typed-fork-stop-app', 'Typed fork stop');
  await harness.open(page);
  await page.getByText('Typed fork stop').click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, '/code-review high', null);
  await waitForGate(harness, 'stopped');

  const card = page.getByTestId('message-timeline-scroll').getByTestId('subagent-group').first();
  await expect(card).toHaveAttribute('data-status', 'running');
  await expect(card.getByTestId('subagent-group-kind')).toHaveText('skill');
  await card.hover();
  await expect(card.getByTestId('subagent-group-background-button')).toHaveCount(0);

  await card.getByTestId('subagent-group-open-pane').first().click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await pane.getByTestId('agent-pane-stop').getByTestId('composer-interrupt').click();
  await expect(pane.getByTestId('agent-pane-stop-error')).toHaveCount(0);

  await advance(harness, mockId, 'stopped');
  await harness.waitForEvent('provider:turn_completed');
  await expect(card).toHaveAttribute('data-status', 'errored');
  await expect(pane.getByTestId('agent-pane-stop')).toHaveCount(0);
  await expect(page.getByTestId('composer-send')).toBeVisible();
  await expect
    .poll(async () => (await listItems(harness, threadId)).filter((i) => i.status === 'running').map((i) => i.id))
    .toEqual([]);
});
