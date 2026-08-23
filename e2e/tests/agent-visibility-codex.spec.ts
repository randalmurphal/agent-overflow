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
// (internal/provider/codex/session_notifications.go); the number is the
// CUMULATIVE `tokenUsage.total.totalTokens`, not the per-round `last`.
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
      arguments: JSON.stringify({
        agent_type: 'reviewer',
        task_name: 'reviewer',
        message: '<encrypted>',
      }),
    },
  }),
  rpc('rawResponseItem/completed', {
    threadId: '${THREAD_ID}',
    turnId: '${TURN_ID}',
    item: {
      type: 'function_call_output',
      call_id: SPAWN_CALL,
      output: JSON.stringify({
        agent_id: CHILD_THREAD,
        task_name: '/root/reviewer',
        nickname: 'reviewer',
      }),
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

const CHILD_TOKENS_LINE = rpc('thread/tokenUsage/updated', {
  threadId: CHILD_THREAD,
  tokenUsage: {
    last: { totalTokens: 1200 },
    total: { totalTokens: 4321 },
    modelContextWindow: 272000,
  },
});

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
            { waitSignal: { name: 'answer' } },
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
  // The CUMULATIVE total (4321), not the round's 1200.
  await expect(trayRow.getByTestId('background-task-tray-row-tokens')).toHaveText('4.3k tokens');

  // --- The same pane ------------------------------------------------
  await spawnRow.hover();
  await spawnRow.getByTestId('collab-tool-row-open-pane').click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane).toBeVisible();
  await expect(pane.getByTestId('agent-pane-model')).toBeVisible();
  await expect(pane.getByTestId('agent-pane-breadcrumb-entry')).toHaveCount(0);
  await expect(pane.getByTestId('agent-pane-breadcrumb-current')).toContainText('reviewer');
  await expect(pane.getByTestId('workspace-strip-usage')).toHaveText('4.3k');
  // `close_agent` is a model tool, so the pane offers no Stop.
  await expect(pane.getByTestId('agent-pane-stop')).toHaveCount(0);

  // The pane body is EMPTY, and honestly so: Codex delivers none of a
  // child's transcript to the parent thread, so there is no row whose
  // parent chain reaches this scope. The counter, the status line and
  // the breadcrumb are the whole pane for a Codex child.
  await expect(pane.getByTestId('agent-pane-empty')).toBeVisible();

  // --- The child's answer lands: the card, at the completion ---------
  await waitForGate(harness, 'answer');
  await advance(harness, mockId, 'answer');
  await harness.waitForEvent('provider:turn_completed');
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(1);
  const card = timeline.getByTestId('subagent-group').first();
  await expect(card.getByTestId('subagent-group-kind')).toHaveText('agent');
  await expect(card.getByTestId('subagent-group-label')).toContainText('reviewer');
  await expect(card).toHaveAttribute('data-background', 'true');
  await expect(card.getByTestId('subagent-group-background-button')).toHaveCount(0);
  await expect(card.getByTestId('subagent-group-tokens')).toHaveText('4.3k tokens');
  await expect(card.getByTestId('subagent-group-duration')).toBeVisible();
  await expect(card.getByTestId('subagent-group-open-pane')).toHaveCount(1);
  // The card sits below the launch row, at the completion point.
  const [spawnBox, cardBox] = await Promise.all([spawnRow.boundingBox(), card.boundingBox()]);
  expect(cardBox!.y).toBeGreaterThan(spawnBox!.y + spawnBox!.height - 1);
  // The launch row is still the launch row.
  await expect(spawnRow.getByTestId('collab-tool-row')).toHaveCount(1);
});

// Regression pin — a Codex child's FINAL_ANSWER must render.
//
// The answer arrives as the spawn launch's completion sibling, with the
// text in `payloadMeta.preview` (kind `tool_completion`,
// `completionOf: call_spawn_reviewer`). Codex delivers none of the
// child's transcript to the parent, so that preview is the child's whole
// product. The card sits AT that sibling and shows the answer collapsed
// (its preview line) and expanded (`subagent-group-final-answer`); the
// pane shows it too (`agent-pane-final-answer`).
test('a Codex child\u2019s FINAL_ANSWER is readable somewhere in the UI', async ({
  harness,
  page,
}) => {
  const mockId = await startSpawnTurn(harness, page, 'codex-answer-app', 'Codex answer');
  await waitForGate(harness, 'tokens');
  await advance(harness, mockId, 'tokens');
  await waitForGate(harness, 'answer');
  await advance(harness, mockId, 'answer');
  await harness.waitForEvent('provider:turn_completed');

  const timeline = page.getByTestId('message-timeline-scroll');
  const card = timeline.getByTestId('subagent-group').first();
  await expect(card.getByTestId('subagent-group-preview')).toContainText(FINAL_ANSWER);
  await card.getByTestId('subagent-group-toggle').first().click();
  await expect(card.getByTestId('subagent-group-final-answer')).toContainText(FINAL_ANSWER);

  await card.getByTestId('subagent-group-open-pane').first().click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByTestId('agent-pane-final-answer')).toContainText(FINAL_ANSWER);
});
