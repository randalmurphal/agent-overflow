// Subagent reasoning effort beside the model, for both providers.
//
// Claude: the Agent tool's `effort` input (Claude Code 2.1.292+) names the
// effort; without one the launch runs at the effort the parser stamped on
// it at launch (`inherited_effort`, the session's `--effort`: the seeded
// Opus 4.7 thread's default xhigh). A subagent on a model with no effort
// tiers (Haiku) runs without one, so no effort shows. Until the subagent
// reports its model, the launch's requested `model` is shown, and an alias
// the app cannot resolve (`haiku`: Haiku 4.5 drops efforts, Haiku 5.5 runs
// them) shows no effort. A resumed agent that named no effort runs at the
// session's effort when it resumes, which its resume row shows; after a
// restart, agents nested in a resumed run inherit the effort its launch
// named, from the thread's history. claude-tui reads the session's effort
// off main-loop requests the CLI clamped to the main model: a high on a
// model without xhigh may be xhigh, so an Opus child shows no effort and a
// Sonnet child shows High.
//
// Codex: a 0.162+ `subAgentActivity` started item carries the child's
// resolved `model` and `reasoningEffort`; the spawn row shows them without
// the child `thread/resume` read (the scenario's resume answers another
// profile, which a read would put on the row).
//
// Labels are the effort picker's ("xHigh", "Medium"), joined to the model
// with the composer's separator.
import { test, expect } from './fixtures.js';
import type { Page, TestInfo } from '@playwright/test';
import {
  RESULT_LINE,
  advance,
  agentResumeLines,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
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
  waitForGate,
} from './agent-visibility-helpers.js';

/** Screenshots a region in light and dark appearance for the PR. */
async function shoot(page: Page, testInfo: TestInfo, region: import('@playwright/test').Locator, name: string): Promise<void> {
  for (const theme of ['light', 'dark'] as const) {
    await page.emulateMedia({ colorScheme: theme });
    await expect(page.locator('html')).toHaveClass(theme === 'dark' ? /dark/ : /^(?!.*\bdark\b).*$/);
    await region.screenshot({ path: testInfo.outputPath(`effort-${name}-v4-${theme}.png`) });
  }
  await page.emulateMedia({ colorScheme: 'light' });
}

/** A subagent's own assistant text, reporting the model it runs on. */
function childTextLine(messageId: string, text: string, model: string, parentToolUseId: string): string {
  return JSON.stringify({
    type: 'assistant',
    parent_tool_use_id: parentToolUseId,
    message: { id: messageId, role: 'assistant', model, content: [{ type: 'text', text }] },
  });
}

/** The CLI's `/effort` success reply: CLI-generated output on its `<synthetic>` model. */
function effortReplyLine(messageId: string, text: string): string {
  return JSON.stringify({
    type: 'assistant',
    message: { id: messageId, role: 'assistant', model: '<synthetic>', content: [{ type: 'text', text }] },
  });
}

const SWEEP_TASK = { task_id: 'task-bg', task_type: 'local_agent', description: 'background sweep' };

test('a Claude subagent shows the effort it runs at next to its model', async ({ harness, page }, testInfo) => {
  await harness.rpc('UpdateSettings', { activityRunDefault: 'expanded' });
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('claude-effort', [
      [
        emit([
          ...textLines('msg-lead', 'Delegating three ways.'),
          // Named model and effort, awaited.
          toolUseLine('msg-deep', 'tu-deep', 'Agent', {
            description: 'deep review',
            subagent_type: 'reviewer',
            prompt: 'review the parser',
            model: 'sonnet',
            effort: 'medium',
            run_in_background: false,
          }),
          taskStartedLine('task-deep', 'tu-deep', 'deep review', { subagentType: 'reviewer' }),
          // An alias whose family disagrees on effort; the subagent turns
          // out to run on Haiku 4.5, which drops the requested max.
          toolUseLine('msg-scout', 'tu-scout', 'Agent', {
            description: 'quick scout',
            subagent_type: 'scout',
            prompt: 'list the files',
            model: 'haiku',
            effort: 'max',
          }),
          taskStartedLine('task-scout', 'tu-scout', 'quick scout', { subagentType: 'scout' }),
        ]),
        { waitSignal: { name: 'agents-run' } },
        emit([
          // The subagents' own envelopes report the model each runs on.
          ...textLines('msg-deep-text', 'Reviewing closely.', 'tu-deep'),
          taskUpdatedLine('task-deep', { status: 'completed', end_time: 1787415964724 }),
          toolResultLine('tu-deep', 'Review done.'),
          childTextLine('msg-scout-text', 'Listing.', 'claude-haiku-4-5-20251001', 'tu-scout'),
          taskUpdatedLine('task-scout', { status: 'completed', end_time: 1787415964726 }),
          toolResultLine('tu-scout', 'Scout done.'),

          // No effort named, in the background: the session's xhigh.
          toolUseLine('msg-bg', 'tu-bg', 'Agent', {
            description: 'background sweep',
            subagent_type: 'sweeper',
            prompt: 'sweep the repo',
            run_in_background: true,
          }),
          taskStartedLine('task-bg', 'tu-bg', 'background sweep', { subagentType: 'sweeper' }),
          asyncAgentAckLine('tu-bg', 'task-bg', 'background sweep'),
          backgroundTasksChangedLine([SWEEP_TASK]),
          ...textLines('msg-bg-text', 'Sweeping.', 'tu-bg'),
          RESULT_LINE,

          taskUpdatedLine('task-bg', { status: 'completed', end_time: 1787415964999 }),
          taskNotificationLine('task-bg', 'tu-bg', 'Sweep finished.', {
            usage: { total_tokens: 1200, tool_uses: 1, duration_ms: 900 },
          }),
          backgroundTasksChangedLine([]),
        ]),
      ],
      [
        emit([
          // The session's effort moves before the sweeper is resumed.
          effortReplyLine('msg-effort', 'Set effort level to low (this session only): Quick'),
          ...textLines('msg-lead-2', 'Asking the sweeper once more.'),
          ...agentResumeLines('msg-resume', 'tu-resume', 'task-bg', 'background sweep', 'Sweep the docs too.', 'sweeper'),
          backgroundTasksChangedLine([SWEEP_TASK]),
          RESULT_LINE,
          taskUpdatedLine('task-bg', { status: 'completed', end_time: 1787415969999 }),
          taskNotificationLine('task-bg', 'tu-resume', 'Second sweep finished.', {
            usage: { total_tokens: 800, tool_uses: 1, duration_ms: 700 },
          }),
          backgroundTasksChangedLine([]),
        ]),
      ],
    ]),
  });
  const threadId = await seedAgentThread(harness, 'effort-app', 'Claude effort');
  await harness.open(page);
  await page.getByText('Claude effort').click();
  const mockId = await startMock(harness, threadId);
  const gate = waitForGate(harness, 'agents-run');
  await harness.rpc('SendMessage', threadId, 'delegate', null);
  await gate;

  const timeline = page.getByTestId('chat-timeline-surface').getByTestId('message-timeline-scroll');
  const cards = timeline.getByTestId('subagent-group');
  const deepCard = cards.filter({ hasText: 'deep review' });
  const scoutCard = cards.filter({ hasText: 'quick scout' });
  // Before the subagents report their models, each card shows the requested
  // one; an awaited launch reads as starting.
  await expect(deepCard.getByTestId('subagent-group-label')).toHaveText('Reviewer(Sonnet · Medium)');
  await expect(deepCard.getByTestId('subagent-group-preview')).toHaveText('Initializing...');
  await expect(scoutCard.getByTestId('subagent-group-label')).toHaveText('Scout(Haiku)');
  const done = harness.waitForEvent('provider:turn_completed');
  await advance(harness, mockId, 'agents-run');
  await done;

  const sweepCard = cards.filter({ hasText: 'Sweep finished', hasNotText: 'Second sweep' });
  await expect(deepCard.getByTestId('subagent-group-label')).toHaveText('Reviewer(Mock 1 · Medium)');
  await expect(scoutCard.getByTestId('subagent-group-label')).toHaveText('Scout(Haiku 4.5)');
  await expect(sweepCard.getByTestId('subagent-group-label')).toHaveText('Sweeper(Mock 1 · xHigh)');
  // The background launch row is the same agent, read off its own launch.
  const bgRow = timeline.locator('[data-item-id="tu-bg"]');
  await expect(bgRow.getByTestId('agent-row-preview')).toContainText('Sweeper(Mock 1 · xHigh)');

  await shoot(page, testInfo, timeline, 'claude-rows');

  // --- The pane shows it with the model -----------------------------
  await deepCard.getByTestId('subagent-group-open-pane').first().click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByTestId('agent-pane-model')).toContainText('Mock 1');
  await expect(pane.getByTestId('agent-pane-effort')).toHaveText('Medium');
  await shoot(page, testInfo, pane, 'claude-pane');

  await pane.getByTestId('agent-pane-close').click();
  await scoutCard.getByTestId('subagent-group-open-pane').first().click();
  await expect(pane.getByTestId('agent-pane-model')).toContainText('Haiku 4.5');
  await expect(pane.getByTestId('agent-pane-effort')).toHaveCount(0);
  await pane.getByTestId('agent-pane-close').click();

  // --- A resumed run shows the effort it resumed at -----------------
  const resumed = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'sweep again', null);
  await resumed;
  const resumeRow = timeline.locator('[data-item-id="tu-resume"]');
  await expect(resumeRow.getByTestId('agent-row-preview')).toContainText('Sweeper(Mock 1 · Low)');
  const resumedCard = cards.filter({ hasText: 'Second sweep finished' });
  await expect(resumedCard.getByTestId('subagent-group-label')).toHaveText('Sweeper(Mock 1 · Low)');
  // The first run keeps the effort it ran at.
  await expect(sweepCard.getByTestId('subagent-group-label')).toHaveText('Sweeper(Mock 1 · xHigh)');
  await shoot(page, testInfo, timeline, 'claude-resume');
});

/**
 * A main-loop launch as claude-tui reconstructs it: stamped with its
 * request's `output_config.effort` (`ao_request_effort`), which the CLI
 * clamped to the request's model.
 */
function requestLaunchLine(messageId: string, toolUseId: string, input: Record<string, unknown>, effort: string, model: string): string {
  const envelope = JSON.parse(toolUseLine(messageId, toolUseId, 'Agent', input));
  envelope.ao_request_effort = effort;
  envelope.message.model = model;
  return JSON.stringify(envelope);
}

test('a child inherits an unknown effort when the main model clamped its tier away', async ({ harness, page }, testInfo) => {
  // An xHigh session on Sonnet 4.6, which lacks xhigh, sends high: an Opus
  // child may run at xHigh or High, a Sonnet child runs at High either way.
  const kid = (id: string, description: string): string =>
    requestLaunchLine(`msg-${id}`, `tu-${id}`, { description, subagent_type: 'checker', prompt: description }, 'high', 'claude-sonnet-4-6');
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('claude-effort-clamped', [
      emit([
        kid('opus-kid', 'opus check'),
        taskStartedLine('task-opus-kid', 'tu-opus-kid', 'opus check', { subagentType: 'checker' }),
        kid('sonnet-kid', 'sonnet check'),
        taskStartedLine('task-sonnet-kid', 'tu-sonnet-kid', 'sonnet check', { subagentType: 'checker' }),
        childTextLine('msg-opus-kid-text', 'Checking on Opus.', 'claude-opus-4-7', 'tu-opus-kid'),
        taskUpdatedLine('task-opus-kid', { status: 'completed', end_time: 1787415964724 }),
        toolResultLine('tu-opus-kid', 'Opus done.'),
        childTextLine('msg-sonnet-kid-text', 'Checking on Sonnet.', 'claude-sonnet-4-6', 'tu-sonnet-kid'),
        taskUpdatedLine('task-sonnet-kid', { status: 'completed', end_time: 1787415964726 }),
        toolResultLine('tu-sonnet-kid', 'Sonnet done.'),
        RESULT_LINE,
      ]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'effort-clamped-app', 'Clamped effort');
  await harness.open(page);
  await page.getByText('Clamped effort').click();
  await startMock(harness, threadId);
  const done = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'check', null);
  await done;

  const launch = (await listItems(harness, threadId)).find((item) => item.id === 'tu-opus-kid');
  expect(itemMeta(launch)).toMatchObject({ inherited_effort: 'high', inherited_effort_alternatives: ['xhigh'] });

  const timeline = page.getByTestId('chat-timeline-surface').getByTestId('message-timeline-scroll');
  const cards = timeline.getByTestId('subagent-group');
  const opusCard = cards.filter({ hasText: 'opus check' });
  await expect(opusCard.getByTestId('subagent-group-label')).toHaveText('Checker(Opus 4.7)');
  await expect(cards.filter({ hasText: 'sonnet check' }).getByTestId('subagent-group-label')).toHaveText(
    'Checker(Sonnet 4.6 · High)',
  );
  await shoot(page, testInfo, timeline, 'claude-clamped');

  await opusCard.getByTestId('subagent-group-open-pane').first().click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByTestId('agent-pane-model')).toContainText('Opus 4.7');
  await expect(pane.getByTestId('agent-pane-effort')).toHaveCount(0);
});

test('after a restart, agents nested in a resumed run inherit the effort its launch named', async ({ harness, page }, testInfo) => {
  const LOW_TASK = { task_id: 'task-low', task_type: 'local_agent', description: 'low sweep' };
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('claude-effort-before-restart', [
      emit([
        toolUseLine('msg-low', 'tu-low', 'Agent', {
          description: 'low sweep',
          subagent_type: 'sweeper',
          prompt: 'sweep lightly',
          effort: 'low',
          run_in_background: true,
        }),
        taskStartedLine('task-low', 'tu-low', 'low sweep', { subagentType: 'sweeper' }),
        asyncAgentAckLine('tu-low', 'task-low', 'low sweep'),
        backgroundTasksChangedLine([LOW_TASK]),
        RESULT_LINE,
        taskUpdatedLine('task-low', { status: 'completed', end_time: 1787415964999 }),
        taskNotificationLine('task-low', 'tu-low', 'Low sweep finished.'),
        backgroundTasksChangedLine([]),
      ]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'effort-restart-app', 'Effort restart');
  await harness.open(page);
  await page.getByText('Effort restart').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'sweep', null);
  await harness.waitForEvent('provider:turn_completed');
  await harness.rpc('StopSession', threadId);

  // The new process knows the session's effort (max), not the agent's.
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('claude-effort-after-restart', [
      emit([
        effortReplyLine('msg-effort-max', 'Set effort level to max (this session only): Deepest'),
        ...agentResumeLines('msg-resume-low', 'tu-resume-low', 'task-low', 'low sweep', 'Check once more.', 'sweeper'),
        backgroundTasksChangedLine([LOW_TASK]),
        RESULT_LINE,
        // The resumed round launches an agent of its own, parented to the
        // original launch like every row of the agent.
        toolUseLine('msg-inner', 'tu-inner', 'Agent', { description: 'inner check', subagent_type: 'checker', prompt: 'look' }, 'tu-low'),
        ...textLines('msg-inner-text', 'Inner checking.', 'tu-inner'),
        toolResultLine('tu-inner', 'Inner done.', { parentToolUseId: 'tu-low' }),
        taskUpdatedLine('task-low', { status: 'completed', end_time: 1787415969999 }),
        taskNotificationLine('task-low', 'tu-resume-low', 'Second low sweep finished.'),
        backgroundTasksChangedLine([]),
      ]),
    ]),
  });
  await startMock(harness, threadId);
  const resumed = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'sweep again', null);
  await resumed;

  await expect
    .poll(async () => itemMeta((await listItems(harness, threadId)).find((item) => item.id === 'tu-inner')).inherited_effort)
    .toBe('low');
  const timeline = page.getByTestId('chat-timeline-surface').getByTestId('message-timeline-scroll');
  const resumedCard = timeline.getByTestId('subagent-group').filter({ hasText: 'Second low sweep finished' });
  await expect(resumedCard.getByTestId('subagent-group-label')).toHaveText('Sweeper(Opus 4.7 · Low)');
  await resumedCard.getByTestId('subagent-group-open-pane').first().click();
  const pane = page.getByTestId('companion-pane-agent-body');
  const innerCard = pane.getByTestId('agent-pane-timeline').getByTestId('subagent-group').filter({ hasText: 'inner check' });
  await expect(innerCard.getByTestId('subagent-group-label')).toHaveText('Checker(Mock 1 · Low)');
  await shoot(page, testInfo, pane, 'claude-restart-nested');
});

const CHILD_THREAD = 'child-effort';
const SPAWN_CALL = 'call_spawn_effort';
const ANSWER = 'Auditor verdict: clean.';

function rpc(method: string, params: unknown): string {
  return JSON.stringify({ jsonrpc: '2.0', method, params });
}

test('a Codex child shows the effort its spawn activity reports, without a profile read', async ({ harness, page }, testInfo) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: {
      version: 1,
      name: 'codex-effort',
      provider: 'codex',
      turns: [
        {
          label: 'spawn',
          steps: [
            {
              emit: {
                delayBetweenMs: 5,
                lines: [
                  rpc('turn/started', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}' } }),
                  rpc('rawResponseItem/completed', {
                    threadId: '${THREAD_ID}',
                    turnId: '${TURN_ID}',
                    item: {
                      type: 'function_call',
                      name: 'spawn_agent',
                      namespace: 'collaboration',
                      call_id: SPAWN_CALL,
                      arguments: JSON.stringify({ task_name: 'auditor', fork_turns: 'all', message: 'gAAAAAB-encrypted' }),
                    },
                  }),
                  // codex 0.162: the started activity reports the child's
                  // resolved profile.
                  rpc('item/completed', {
                    threadId: '${THREAD_ID}',
                    turnId: '${TURN_ID}',
                    item: {
                      type: 'subAgentActivity',
                      id: SPAWN_CALL,
                      kind: 'started',
                      agentThreadId: CHILD_THREAD,
                      agentPath: '/root/auditor',
                      model: 'gpt-5.6-sol',
                      reasoningEffort: 'xhigh',
                    },
                  }),
                  rpc('rawResponseItem/completed', {
                    threadId: '${THREAD_ID}',
                    turnId: '${TURN_ID}',
                    item: { type: 'function_call_output', call_id: SPAWN_CALL, output: JSON.stringify({ task_name: '/root/auditor' }) },
                  }),
                  rpc('turn/started', { threadId: CHILD_THREAD, turn: { id: 'child-turn' } }),
                ],
              },
            },
            { waitSignal: { name: 'answer' } },
            {
              emit: {
                delayBetweenMs: 5,
                lines: [
                  rpc('item/completed', {
                    threadId: CHILD_THREAD,
                    turnId: 'child-turn',
                    item: { id: 'child-answer', type: 'agentMessage', text: ANSWER },
                  }),
                  rpc('turn/completed', { threadId: CHILD_THREAD, turn: { id: 'child-turn', status: 'completed' } }),
                  rpc('rawResponseItem/completed', {
                    threadId: '${THREAD_ID}',
                    turnId: '${TURN_ID}',
                    item: {
                      type: 'agent_message',
                      id: 'amsg-auditor',
                      author: '/root/auditor',
                      recipient: '/root',
                      content: [{ type: 'input_text', text: `Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/auditor\nPayload:\n${ANSWER}` }],
                      internal_chat_message_metadata_passthrough: { turn_id: '${TURN_ID}' },
                    },
                  }),
                  rpc('turn/completed', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}', status: 'completed' } }),
                ],
              },
            },
          ],
        },
      ],
      afterTurns: 'silent',
      // The child's thread/resume answers a different profile, so a read
      // the activity made unnecessary would show on the row.
      codex: { responses: {
        'thread/resume': JSON.stringify({
          jsonrpc: '2.0', id: '${REQUEST_ID}',
          result: { thread: { id: CHILD_THREAD }, model: 'gpt-5.2-codex', reasoningEffort: 'low' },
        }).replace('"${REQUEST_ID}"', '${REQUEST_ID}'),
      } },
    },
  });
  const threadId = await seedAgentThread(harness, 'codex-effort-app', 'Codex effort', 'codex');
  await harness.open(page);
  await page.getByText('Codex effort').click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'audit', null);

  const timeline = page.getByTestId('chat-timeline-surface').getByTestId('message-timeline-scroll');
  const spawnRow = timeline.locator(`[data-item-id="${SPAWN_CALL}"]`);
  await expect(spawnRow.getByTestId('collab-tool-row')).toContainText('(default · GPT 5.6 Sol · xHigh)');

  await spawnRow.hover();
  await spawnRow.getByTestId('collab-tool-row-open-pane').click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByTestId('agent-pane-model')).toHaveText('GPT 5.6 Sol');
  await expect(pane.getByTestId('agent-pane-effort')).toHaveText('xHigh');
  await shoot(page, testInfo, pane, 'codex-pane');

  await waitForGate(harness, 'answer');
  await advance(harness, mockId, 'answer');
  await harness.waitForEvent('provider:turn_completed');
  const card = timeline.getByTestId('subagent-group').first();
  await expect(card.getByTestId('subagent-group-label')).toContainText('(GPT 5.6 Sol · xHigh)');
  await expect(timeline).not.toContainText('GPT 5.2 Codex');
  await expect(spawnRow.getByTestId('collab-tool-row')).toContainText('(default · GPT 5.6 Sol · xHigh)');
  await pane.getByTestId('agent-pane-close').click();
  await expect(pane).toHaveCount(0);
  await shoot(page, testInfo, timeline, 'codex-rows');
});
