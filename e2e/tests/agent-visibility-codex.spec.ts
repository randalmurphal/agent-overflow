// Agent visibility — the CODEX criterion
// (docs/specs/agent-visibility.md § "Success criteria", item 6):
//
//   Codex `spawn_agent` children render with the same card (at the
//   completion point) and pane, with token counts from the child thread.
//
// "The same card and pane" is the load-bearing half: the assertions below
// are the same testids the Claude specs use, on a thread whose provider is
// Codex and whose launch row is a `collab_agent` tool call.
//
// The token count comes from the child THREAD's own
// `thread/tokenUsage/updated` — a notification addressed to a provider
// thread the parent never shows. It reaches the card only because the
// session intercepts it for a mapped child and re-emits it as a scoped
// `EventSubagentProgress` naming the spawn tool_use
// (internal/provider/codex/session_notifications.go). The number is the
// child's TRUE CUMULATIVE spend — fresh input + cache writes + all output,
// assembled in `childAgentTokenSpend` off the provider's own cumulative
// counters, so it never goes backwards when the child compacts. It is NOT
// `total.totalTokens`, which re-counts the cached prompt every round.
import { test, expect } from './fixtures.js';
import {
  advance,
  seedAgentThread,
  startMock,
  waitForGate,
} from './agent-visibility-helpers.js';

const CHILD_THREAD = 'child-reviewer';
const SPAWN_CALL = 'call_spawn_reviewer';
const FINAL_ANSWER = 'Reviewer verdict: the parser drift is cosmetic.';

function rpc(method: string, params: unknown): string {
  return JSON.stringify({ jsonrpc: '2.0', method, params });
}

const SPAWN_LINES = [
  rpc('turn/started', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}' } }),
  rpc('rawResponseItem/completed', {
    threadId: '${THREAD_ID}',
    turnId: '${TURN_ID}',
    item: {
      type: 'function_call',
      name: 'spawn_agent',
      namespace: 'collaboration',
      call_id: SPAWN_CALL,
      // The real V2 argument set (codex 0.149.0): a model-chosen
      // task_name, fork_turns, and a Fernet-encrypted message no client
      // can read. There is no nickname and no agent_type on this wire,
      // which is why the task name IS the card's label.
      arguments: JSON.stringify({
        task_name: 'reviewer',
        fork_turns: 'all',
        message: 'gAAAAABqi1w-encrypted-spawn-payload',
      }),
    },
  }),
  rpc('rawResponseItem/completed', {
    threadId: '${THREAD_ID}',
    turnId: '${TURN_ID}',
    item: {
      type: 'function_call_output',
      call_id: SPAWN_CALL,
      output: JSON.stringify({ task_name: '/root/reviewer' }),
    },
  }),
  // The ownership statement. Until this lands the child thread is
  // "unmapped foreign", and its notifications are quarantined rather
  // than routed — which is exactly why the token tick is gated behind a
  // signal instead of raced against this line.
  rpc('item/completed', {
    threadId: '${THREAD_ID}',
    turnId: '${TURN_ID}',
    item: {
      type: 'subAgentActivity',
      id: SPAWN_CALL,
      kind: 'started',
      agentThreadId: CHILD_THREAD,
      agentPath: '/root/reviewer',
    },
  }),
];

// Cumulative spend = fresh input (91000 - 88000) + cache writes (100) +
// all output (300) = 3400. Two wrong answers are seeded alongside it:
// `total.totalTokens` (91300) re-counts the cached prompt every round,
// and the latest-input composition (4000 + 100 + 300 = 4400) dips
// whenever the child compacts.
const CHILD_TOKENS_LINE = rpc('thread/tokenUsage/updated', {
  threadId: CHILD_THREAD,
  tokenUsage: {
    last: {
      totalTokens: 4080,
      inputTokens: 4000,
      cachedInputTokens: 3900,
      cacheWriteInputTokens: 100,
      outputTokens: 80,
    },
    total: {
      totalTokens: 91300,
      inputTokens: 91000,
      cachedInputTokens: 88000,
      cacheWriteInputTokens: 100,
      outputTokens: 300,
    },
    modelContextWindow: 272000,
  },
});

const CHILD_TOOL_LINES = [
  rpc('item/started', {
    threadId: CHILD_THREAD,
    turnId: 'child-turn',
    item: { id: 'child-tool-read', type: 'commandExecution', status: 'inProgress', command: 'rg TODO' },
  }),
  rpc('item/completed', {
    threadId: CHILD_THREAD,
    turnId: 'child-turn',
    item: { id: 'child-tool-read', type: 'commandExecution', status: 'completed', command: 'rg TODO', exitCode: 0 },
  }),
  rpc('item/started', {
    threadId: CHILD_THREAD,
    turnId: 'child-turn',
    item: { id: 'child-tool-test', type: 'commandExecution', status: 'inProgress', command: 'pnpm test' },
  }),
  rpc('item/completed', {
    threadId: CHILD_THREAD,
    turnId: 'child-turn',
    item: { id: 'child-tool-test', type: 'commandExecution', status: 'completed', command: 'pnpm test', exitCode: 0 },
  }),
];

// What a Codex child actually does: its transcript streams to the parent
// thread, parented to the spawn (`isUnsafeChildProjectionEvent` lets
// assistant text through). Its final message IS the answer — the
// FINAL_ANSWER envelope below repeats the same text into the parent's
// model context, and the completion sibling's `preview` is a 240-char
// truncation of it.
const CHILD_TRANSCRIPT_LINES = [
  rpc('item/agentMessage/delta', {
    threadId: CHILD_THREAD,
    turnId: 'child-turn',
    itemId: 'child-answer',
    delta: FINAL_ANSWER,
  }),
  rpc('item/completed', {
    threadId: CHILD_THREAD,
    turnId: 'child-turn',
    item: { id: 'child-answer', type: 'agentMessage', text: FINAL_ANSWER },
  }),
];

const FINAL_ANSWER_LINES = [
  rpc('rawResponseItem/completed', {
    threadId: '${THREAD_ID}',
    turnId: '${TURN_ID}',
    item: {
      type: 'agent_message',
      id: 'amsg-reviewer',
      author: '/root/reviewer',
      recipient: '/root',
      content: [
        {
          type: 'input_text',
          text:
            `Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/reviewer\nPayload:\n${FINAL_ANSWER}`,
        },
      ],
      internal_chat_message_metadata_passthrough: { turn_id: '${TURN_ID}' },
    },
  }),
  rpc('turn/completed', {
    threadId: '${THREAD_ID}',
    turn: { id: '${TURN_ID}', status: 'completed' },
  }),
];

/** Sets the scenario, seeds a Codex thread, opens it, and sends. */
async function startSpawnTurn(
  harness: Parameters<typeof seedAgentThread>[0],
  page: import('@playwright/test').Page,
  projectName: string,
  title: string,
): Promise<string> {
  await harness.rpc('HarnessSetScenario', {
    scenario: {
      version: 1,
      name: 'codex-spawn-visibility',
      provider: 'codex',
      turns: [
        {
          label: 'spawn-one-child',
          steps: [
            { emit: { lines: SPAWN_LINES, delayBetweenMs: 5 } },
            // Held so the page is subscribed before the live tick: a
            // Codex child's progress is in-memory UI state, exactly like
            // Claude's task_progress.
            { waitSignal: { name: 'tokens' } },
            { emit: { lines: [CHILD_TOKENS_LINE], delayBetweenMs: 5 } },
            { waitSignal: { name: 'tools' } },
            { emit: { lines: CHILD_TOOL_LINES, delayBetweenMs: 5 } },
            { waitSignal: { name: 'answer' } },
            { emit: { lines: CHILD_TRANSCRIPT_LINES, delayBetweenMs: 5 } },
            { emit: { lines: FINAL_ANSWER_LINES, delayBetweenMs: 5 } },
          ],
        },
      ],
      afterTurns: 'silent',
    },
  });

  const threadId = await seedAgentThread(harness, projectName, title, 'codex');
  await page.goto(harness.url);
  await page.getByText(title).click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'review this', null);
  return mockId;
}

test('a Codex spawn_agent child keeps its launched row, opens the same pane, and gets its card at the completion', async ({
  harness,
  page,
}) => {
  const mockId = await startSpawnTurn(harness, page, 'codex-agent-app', 'Codex children');

  // --- The launch row, unchanged --------------------------------------
  // The row is the collab `launched` leaf it was before the card existed,
  // plus the one approved addition, the open-in-pane door (user ruling
  // 2026-08-23). No card while the child runs.
  const timeline = page.getByTestId('message-timeline-scroll');
  const spawnRow = timeline.locator(`[data-item-id="${SPAWN_CALL}"]`);
  await expect(spawnRow.getByTestId('collab-tool-row')).toHaveCount(1);
  await expect(spawnRow).toContainText('reviewer');
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(0);
  await spawnRow.hover();
  await expect(spawnRow.getByTestId('collab-tool-row-open-pane')).toBeVisible();

  // --- Tokens, from the child thread, live on the tray row ------------
  await waitForGate(harness, 'tokens');
  await advance(harness, mockId, 'tokens');
  await page.getByTestId('activity-rail-background-toggle').click();
  const trayRow = page.getByTestId('background-task-tray-row').first();
  // The child's cumulative spend (3400), not the 91.3k that re-counts the
  // cached prompt each round and not the 4.4k latest-input figure.
  await expect(trayRow.getByTestId('background-task-tray-row-tokens')).toHaveText('3.4k tokens');

  // Child tool calls stay in the pane transcript. The live tray keeps only
  // the newest direct call, and the historical spawn row does not grow.
  await waitForGate(harness, 'tools');
  await advance(harness, mockId, 'tools');
  await expect(trayRow.getByTestId('background-task-tray-row-activity')).toContainText('pnpm test');
  await expect(trayRow.getByTestId('background-task-tray-row-activity')).not.toContainText('rg TODO');
  await expect(spawnRow).not.toContainText('pnpm test');

  // --- The same pane ------------------------------------------------
  await spawnRow.hover();
  await spawnRow.getByTestId('collab-tool-row-open-pane').click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane).toBeVisible();
  await expect(pane.getByTestId('agent-pane-model')).toBeVisible();
  await expect(pane.getByTestId('agent-pane-breadcrumb-entry')).toHaveCount(0);
  await expect(pane.getByTestId('agent-pane-breadcrumb-current')).toContainText('reviewer');
  // The model-chosen task name IS the crumb: a V2 spawn sends no
  // nickname, so the label falls back to the agent path's tail. There
  // is no second plaintext string, and repeating it would read
  // "reviewer - reviewer".
  await expect(pane.getByTestId('agent-pane-description')).toHaveCount(0);
  await expect(pane.getByTestId('workspace-strip-usage')).toHaveText('3.4k');
  // `close_agent` is a model tool, so the pane offers no Stop.
  await expect(pane.getByTestId('agent-pane-stop')).toHaveCount(0);

  // The full child transcript keeps both calls even though the tray shows
  // only the latest one.
  await expect(pane.getByTestId('agent-pane-timeline')).toContainText('rg TODO');
  await expect(pane.getByTestId('agent-pane-timeline')).toContainText('pnpm test');

  // --- The child's answer lands: the card, at the completion ---------
  await waitForGate(harness, 'answer');
  await advance(harness, mockId, 'answer');
  await harness.waitForEvent('provider:turn_completed');
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(1);
  const card = timeline.getByTestId('subagent-group').first();
  await expect(card.getByTestId('subagent-group-kind')).toHaveText('agent');
  await expect(card.getByTestId('subagent-group-label')).toContainText('reviewer');
  await expect(card.getByTestId('subagent-group-description')).toHaveCount(0);
  await expect(card).toHaveAttribute('data-background', 'true');
  await expect(card.getByTestId('subagent-group-background-button')).toHaveCount(0);
  await expect(card.getByTestId('subagent-group-tokens')).toHaveText('3.4k tokens');
  await expect(card.getByTestId('subagent-group-duration')).toBeVisible();
  await expect(card.getByTestId('subagent-group-open-pane')).toHaveCount(1);
  // The card sits below the launch row, at the completion point.
  const [spawnBox, cardBox] = await Promise.all([spawnRow.boundingBox(), card.boundingBox()]);
  expect(cardBox!.y).toBeGreaterThan(spawnBox!.y + spawnBox!.height - 1);
  // The launch row is still the launch row.
  await expect(spawnRow.getByTestId('collab-tool-row')).toHaveCount(1);
});

// Regression pin — a Codex child's answer renders ONCE, as its own
// message, formatted and whole.
//
// The answer exists in three places: the child's own assistant row
// (parented to the spawn), the FINAL_ANSWER envelope that puts it into
// the parent's model context, and the 240-char `payloadMeta.preview` on
// the completion sibling. Only the first is a message. The preview is
// the COLLAPSED one-liner and nothing else — rendering it in the body
// too showed the same text twice, unformatted and cut mid-word (user
// ruling 2026-08-23).
test('a Codex child\u2019s answer renders once, as a normal message', async ({
  harness,
  page,
}) => {
  const mockId = await startSpawnTurn(harness, page, 'codex-answer-app', 'Codex answer');
  await waitForGate(harness, 'tokens');
  await advance(harness, mockId, 'tokens');
  await waitForGate(harness, 'tools');
  await advance(harness, mockId, 'tools');
  await waitForGate(harness, 'answer');
  await advance(harness, mockId, 'answer');
  await harness.waitForEvent('provider:turn_completed');

  const timeline = page.getByTestId('message-timeline-scroll');
  const card = timeline.getByTestId('subagent-group').first();
  await expect(card.getByTestId('subagent-group-preview')).toContainText(FINAL_ANSWER);

  await card.getByTestId('subagent-group-toggle').first().click();
  const body = card.getByTestId('subagent-group-body');
  await expect(body).toContainText(FINAL_ANSWER);
  await expect(card.getByTestId('subagent-group-final-answer')).toHaveCount(0);
  await expect(body.getByText(FINAL_ANSWER, { exact: false })).toHaveCount(1);

  await card.getByTestId('subagent-group-open-pane').first().click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByTestId('agent-pane-timeline')).toContainText(FINAL_ANSWER);
  await expect(pane.getByTestId('agent-pane-final-answer')).toHaveCount(0);
  await expect(pane.getByTestId('agent-pane-empty')).toHaveCount(0);
});
