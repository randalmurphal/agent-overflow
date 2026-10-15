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
//   5. A typed fork's Agent and every agent's last message, which Claude
//      writes after the agent's terminal, reach the fork's card.
//   6. A resume a typed fork sends is the resumed round's background
//      carrier until the agent ends, in the same process and after a
//      restart, where the thread's history names the agent's launch.
//   7. After a restart, a resumed typed fork's rows return to its command
//      card, the root the thread's history names.
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
  claudeTurnsScenario,
  emit,
  itemMeta,
  listItems,
  seedAgentThread,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  taskUpdatedLine,
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

const CHILD_TASK = 'task-typed-child';

test("a typed fork's child agent and every last message reach the fork's card", async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('typed-fork-child', [
      emit([
        taskStartedLine(TYPED_TASK, '', '/code-review'),
        transcriptMirrorLine(TYPED_TASK, [{ text: 'Reviewing the change.' }], {
          metadata: true,
          attributionSkill: 'code-review',
          uuidPrefix: 'fork-1',
        }),
        // The CLI announces the child before the mirror delivers the fork's
        // Agent call, and the child's first rows can come first too
        // (forked_skill_direct_command_20261014).
        taskStartedLine(CHILD_TASK, 'tu-typed-child', 'check the diff'),
        transcriptMirrorLine(CHILD_TASK, [{ user: 'Check the diff.' }], {
          metadataToolUseId: 'tu-typed-child',
          uuidPrefix: 'child-1',
        }),
        transcriptMirrorLine(
          TYPED_TASK,
          [{ toolUse: { id: 'tu-typed-child', name: 'Agent', input: { description: 'check the diff', prompt: 'Check the diff.' } } }],
          { uuidPrefix: 'fork-2' },
        ),
        transcriptMirrorLine(CHILD_TASK, [{ tool: { id: 'tu-child-read', name: 'Read', result: '# fixture' } }], {
          uuidPrefix: 'child-2',
        }),
        taskUpdatedLine(CHILD_TASK, { status: 'completed', end_time: 1792029207656 }),
        taskNotificationLine(CHILD_TASK, 'tu-typed-child', 'check the diff'),
        // Claude writes each agent's last message after its terminal.
        transcriptMirrorLine(CHILD_TASK, [{ text: 'CHILD-LAST' }], { uuidPrefix: 'child-3' }),
        transcriptMirrorLine(TYPED_TASK, [{ toolResult: { id: 'tu-typed-child', result: 'CHILD-LAST' } }], {
          uuidPrefix: 'fork-3',
        }),
        taskNotificationLine(TYPED_TASK, '', '/code-review'),
        transcriptMirrorLine(TYPED_TASK, [{ text: 'FORK-LAST' }], { uuidPrefix: 'fork-4' }),
        RESULT_LINE,
      ]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'typed-fork-child-app', 'Typed fork child');
  await harness.open(page);
  await page.getByText('Typed fork child').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, '/code-review high', null);
  await harness.waitForEvent('provider:turn_completed');

  await expect
    .poll(async () => {
      const row = await typedForkRow(harness, threadId);
      const items = await listItems(harness, threadId);
      const under = (parent: string | undefined) =>
        items.filter((i) => parent && i.parentId === parent).map((i) => i.toolName || i.summary);
      return { status: row?.status, fork: under(row?.id), child: under('tu-typed-child') };
    })
    .toEqual({
      status: 'completed',
      fork: ['Reviewing the change.', 'Agent', 'FORK-LAST'],
      child: ['Check the diff.', 'Read', 'CHILD-LAST'],
    });

  const card = page.getByTestId('message-timeline-scroll').getByTestId('subagent-group');
  await expect(card).toHaveCount(1);
  await card.getByTestId('subagent-group-open-pane').first().click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByText('FORK-LAST')).toBeVisible();
  await pane.getByRole('button', { name: 'Expand 1 activity row' }).click();
  await expect(pane.getByTestId('subagent-group')).toHaveAttribute('data-status', 'completed');
});

const AGENT_TASK = 'task-resumed-agent';
const RESUME_PROMPT = `<agent-message from="${TYPED_TASK}">\nReply with the word beta.\n</agent-message>`;

/** Turn 1 of local_agent_resumed_by_typed_fork_20261014: an awaited agent
 * runs and finishes. */
function awaitedAgentTurn(): string[] {
  return [
    toolUseLine('msg-agent', 'tu-agent', 'Agent', {
      description: 'Test agent call',
      subagent_type: 'general-purpose',
      prompt: 'Reply with the word alpha.',
    }),
    taskStartedLine(AGENT_TASK, 'tu-agent', 'Test agent call', { prompt: 'Reply with the word alpha.' }),
    ...textLines('msg-agent-alpha', 'alpha', 'tu-agent'),
    taskUpdatedLine(AGENT_TASK, { status: 'completed', end_time: 1792029207656 }),
    taskNotificationLine(AGENT_TASK, 'tu-agent', 'alpha', { uuid: 'notify-agent-1' }),
    toolResultLine('tu-agent', [{ type: 'text', text: 'alpha' }], {
      toolUseResult: { status: 'completed', agentId: AGENT_TASK, content: [{ type: 'text', text: 'alpha' }] },
    }),
    ...textLines('msg-main-alpha', 'The agent said alpha.'),
    RESULT_LINE,
  ];
}

/** Turn 2: the typed fork resumes the agent with a SendMessage the session
 * mirror delivers after the rebind task_started. */
function forkResumesAgentTurn(): string[] {
  return [
    taskStartedLine(TYPED_TASK, '', '/resume-fork'),
    transcriptMirrorLine(TYPED_TASK, [{ text: 'Messaging the agent.' }], {
      metadata: true,
      attributionSkill: 'resume-fork',
      uuidPrefix: 'fork-1',
    }),
    taskStartedLine(AGENT_TASK, 'tu-fork-send', 'Test agent call', { prompt: RESUME_PROMPT }),
    transcriptMirrorLine(
      TYPED_TASK,
      [
        {
          toolUse: { id: 'tu-fork-send', name: 'SendMessage', input: { to: AGENT_TASK, message: 'Reply with the word beta.' } },
          toolResult: { id: 'tu-fork-send', result: `{"success":true,"message":"Resuming agent","resumedAgentId":"${AGENT_TASK}"}` },
        },
      ],
      { uuidPrefix: 'fork-2' },
    ),
    taskNotificationLine(TYPED_TASK, '', '/resume-fork'),
    transcriptMirrorLine(TYPED_TASK, [{ text: 'Sent.' }], { uuidPrefix: 'fork-3' }),
    RESULT_LINE,
  ];
}

/** The resumed agent's round: rows on stdout under its launch, then its
 * terminal, which names the carrier. */
function resumedAgentRound(): string[] {
  return [
    ...textLines('msg-agent-beta', 'beta', 'tu-agent'),
    taskUpdatedLine(AGENT_TASK, { status: 'completed', end_time: 1792029221235 }),
    taskNotificationLine(AGENT_TASK, 'tu-fork-send', 'beta', { uuid: 'notify-agent-2' }),
  ];
}

/** The carrier is the round's background row: running, then ended by the
 * completion sibling the agent's terminal writes under it. */
async function expectResumedRound(harness: HarnessApp, threadId: string, running: boolean) {
  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      const carrier = items.find((i) => i.id === 'tu-fork-send');
      return {
        carrier: carrier?.status,
        background: carrier?.isBackground ?? false,
        root: itemMeta(carrier).transcript_root_id,
        ended: items.filter((i) => i.completionOf === 'tu-fork-send').map((i) => i.status),
        beta: items.some((i) => i.parentId === 'tu-agent' && i.summary === 'beta'),
        launches: items.filter((i) => i.toolName === 'Agent').map((i) => i.id),
      };
    })
    .toEqual({
      carrier: 'running',
      background: true,
      root: 'tu-agent',
      ended: running ? [] : ['completed'],
      beta: !running,
      launches: ['tu-agent'],
    });
}

test('a resume a typed fork sends keeps the resumed round running until the agent ends', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('typed-fork-resume', [
      [emit(awaitedAgentTurn())],
      [emit(forkResumesAgentTurn()), { waitSignal: { name: 'resumed' } }, emit(resumedAgentRound())],
    ]),
  });

  const threadId = await seedAgentThread(harness, 'typed-fork-resume-app', 'Typed fork resume');
  await harness.open(page);
  await page.getByText('Typed fork resume').click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'ask an agent for alpha', null);
  await harness.waitForEvent('provider:turn_completed');

  const turn2 = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, `/resume-fork ${AGENT_TASK}`, null);
  await turn2;
  await waitForGate(harness, 'resumed');
  await expectResumedRound(harness, threadId, true);

  await advance(harness, mockId, 'resumed');
  await expectResumedRound(harness, threadId, false);
});

test('after a restart, a resume a typed fork sends finds the agent the earlier process ran', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('typed-fork-resume-before-restart', [emit(awaitedAgentTurn())]),
  });
  const threadId = await seedAgentThread(harness, 'typed-fork-restart-app', 'Typed fork restart');
  await harness.open(page);
  await page.getByText('Typed fork restart').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'ask an agent for alpha', null);
  await harness.waitForEvent('provider:turn_completed');
  await harness.rpc('StopSession', threadId);

  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('typed-fork-resume-after-restart', [
      emit(forkResumesAgentTurn()),
      { waitSignal: { name: 'resumed' } },
      emit(resumedAgentRound()),
    ]),
  });
  const mockId = await startMock(harness, threadId);
  const turn = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, `/resume-fork ${AGENT_TASK}`, null);
  await turn;
  await waitForGate(harness, 'resumed');
  await expectResumedRound(harness, threadId, true);
  await advance(harness, mockId, 'resumed');
  await expectResumedRound(harness, threadId, false);
});

test("after a restart, a resumed typed fork's rows return to its command card", async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('typed-fork-before-restart', [
      emit([...typedForkLines(), taskNotificationLine(TYPED_TASK, '', '/code-review'), RESULT_LINE]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'typed-fork-resumed-app', 'Typed fork resumed');
  await harness.open(page);
  await page.getByText('Typed fork resumed').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, '/code-review high', null);
  await harness.waitForEvent('provider:turn_completed');
  const fork = await typedForkRow(harness, threadId);
  expect(fork?.id).toMatch(/^claude-command:/);
  await harness.rpc('StopSession', threadId);

  // The resumed fork has no launching tool_use: its rows reach only the
  // mirror (local_agent_resumed_by_typed_fork_20261014, turn 3).
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('typed-fork-resumed-after-restart', [
      emit([
        toolUseLine('msg-send-fork', 'tu-send-fork', 'SendMessage', { to: TYPED_TASK, message: 'Anything else?' }),
        taskStartedLine(TYPED_TASK, 'tu-send-fork', '(resumed)', { prompt: 'Anything else?' }),
        toolResultLine('tu-send-fork', [{ type: 'text', text: `{"success":true,"resumedAgentId":"${TYPED_TASK}"}` }], {
          toolUseResult: { success: true, message: 'Resuming agent', resumedAgentId: TYPED_TASK },
        }),
        RESULT_LINE,
      ]),
      { waitSignal: { name: 'fork-round' } },
      emit([
        transcriptMirrorLine(TYPED_TASK, [], { metadata: true }),
        transcriptMirrorLine(TYPED_TASK, [{ text: 'FORK-AGAIN' }], { uuidPrefix: 'again-1' }),
        taskUpdatedLine(TYPED_TASK, { status: 'completed', end_time: 1792029221726 }),
        taskNotificationLine(TYPED_TASK, 'tu-send-fork', 'done', { uuid: 'notify-fork-2' }),
        transcriptMirrorLine(TYPED_TASK, [{ text: 'FORK-AGAIN-LAST' }], { uuidPrefix: 'again-2' }),
      ]),
    ]),
  });
  const mockId = await startMock(harness, threadId);
  const turn = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'ask the reviewer for more', null);
  await turn;
  await waitForGate(harness, 'fork-round');
  await advance(harness, mockId, 'fork-round');

  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      const carrier = items.find((i) => i.id === 'tu-send-fork');
      return {
        background: carrier?.isBackground ?? false,
        root: itemMeta(carrier).transcript_root_id,
        ended: items.filter((i) => i.completionOf === 'tu-send-fork').map((i) => i.status),
        underFork: items.filter((i) => i.parentId === fork?.id).map((i) => i.toolName || i.summary),
      };
    })
    .toEqual({
      background: true,
      root: fork?.id,
      ended: ['completed'],
      // The resume's prompt opens the round under the root.
      underFork: ['Reviewing the change.', 'Read', 'Anything else?', 'FORK-AGAIN', 'FORK-AGAIN-LAST'],
    });
});
