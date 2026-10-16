// Subagent reasoning effort beside the model, for both providers.
//
// Claude: the Agent tool's `effort` input (Claude Code 2.1.292+) names the
// effort; without one the launch runs at the effort the parser stamped on
// it at launch (`inherited_effort`, the session's `--effort`: the seeded
// Opus 4.7 thread's default xhigh). A subagent on a model with no effort
// tiers (Haiku) runs without one, so no effort shows.
//
// Codex: a 0.162+ `subAgentActivity` started item carries the child's
// resolved `model` and `reasoningEffort`; the spawn row shows them without
// the child `thread/resume` read (the scenario's resume answers another
// profile, which a read would put on the row).
//
// Labels are the effort picker's ("xHigh", "Medium").
import { test, expect } from './fixtures.js';
import type { Page, TestInfo } from '@playwright/test';
import {
  RESULT_LINE,
  advance,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeScenario,
  emit,
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
    await region.screenshot({ path: testInfo.outputPath(`effort-${name}-${theme}.png`) });
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

test('a Claude subagent shows the effort it runs at next to its model', async ({ harness, page }, testInfo) => {
  await harness.rpc('UpdateSettings', { activityRunDefault: 'expanded' });
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('claude-effort', [
      emit([
        ...textLines('msg-lead', 'Delegating three ways.'),
        // Named effort.
        toolUseLine('msg-deep', 'tu-deep', 'Agent', {
          description: 'deep review',
          subagent_type: 'reviewer',
          prompt: 'review the parser',
          effort: 'medium',
        }),
        taskStartedLine('task-deep', 'tu-deep', 'deep review'),
        ...textLines('msg-deep-text', 'Reviewing closely.', 'tu-deep'),
        taskUpdatedLine('task-deep', { status: 'completed', end_time: 1787415964724 }),
        toolResultLine('tu-deep', 'Review done.'),

        // No effort named, in the background: the session's xhigh.
        toolUseLine('msg-bg', 'tu-bg', 'Agent', {
          description: 'background sweep',
          subagent_type: 'sweeper',
          prompt: 'sweep the repo',
          run_in_background: true,
        }),
        taskStartedLine('task-bg', 'tu-bg', 'background sweep'),
        asyncAgentAckLine('tu-bg', 'task-bg', 'background sweep'),
        backgroundTasksChangedLine([{ task_id: 'task-bg', task_type: 'local_agent', description: 'background sweep' }]),
        ...textLines('msg-bg-text', 'Sweeping.', 'tu-bg'),

        // Haiku has no effort tiers: the CLI drops the requested max.
        toolUseLine('msg-scout', 'tu-scout', 'Agent', {
          description: 'quick scout',
          subagent_type: 'scout',
          prompt: 'list the files',
          model: 'haiku',
          effort: 'max',
        }),
        taskStartedLine('task-scout', 'tu-scout', 'quick scout'),
        childTextLine('msg-scout-text', 'Listing.', 'claude-haiku-4-5-20251001', 'tu-scout'),
        taskUpdatedLine('task-scout', { status: 'completed', end_time: 1787415964726 }),
        toolResultLine('tu-scout', 'Scout done.'),
        RESULT_LINE,

        taskUpdatedLine('task-bg', { status: 'completed', end_time: 1787415964999 }),
        taskNotificationLine('task-bg', 'tu-bg', 'Sweep finished.', {
          usage: { total_tokens: 1200, tool_uses: 1, duration_ms: 900 },
        }),
        backgroundTasksChangedLine([]),
      ]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'effort-app', 'Claude effort');
  await harness.open(page);
  await page.getByText('Claude effort').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'delegate', null);
  await harness.waitForEvent('provider:turn_completed');

  const timeline = page.getByTestId('chat-timeline-surface').getByTestId('message-timeline-scroll');
  const cards = timeline.getByTestId('subagent-group');
  const deepCard = cards.filter({ hasText: 'deep review' });
  const scoutCard = cards.filter({ hasText: 'quick scout' });
  const sweepCard = cards.filter({ hasText: 'Sweep' }).first();
  await expect(deepCard.getByTestId('subagent-group-label')).toHaveText('Reviewer(Mock 1 Medium)');
  await expect(scoutCard.getByTestId('subagent-group-label')).toHaveText('Scout(Haiku 4.5)');
  await expect(sweepCard.getByTestId('subagent-group-label')).toHaveText('Sweeper(Mock 1 xHigh)');
  // The background launch row is the same agent, read off its own launch.
  const bgRow = timeline.locator('[data-item-id="tu-bg"]');
  await expect(bgRow.getByTestId('agent-row-preview')).toContainText('Sweeper(Mock 1 xHigh)');

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
  await expect(spawnRow.getByTestId('collab-tool-row')).toContainText('(default - GPT 5.6 Sol - xHigh)');

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
  await expect(card.getByTestId('subagent-group-label')).toContainText('(GPT 5.6 Sol xHigh)');
  await expect(timeline).not.toContainText('GPT 5.2 Codex');
  await expect(spawnRow.getByTestId('collab-tool-row')).toContainText('(default - GPT 5.6 Sol - xHigh)');
  await pane.getByTestId('agent-pane-close').click();
  await expect(pane).toHaveCount(0);
  await shoot(page, testInfo, timeline, 'codex-rows');
});
