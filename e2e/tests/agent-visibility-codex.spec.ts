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
//
// Codex reports no tool count. Agent Overflow counts the execution's own
// tool_call rows under the spawn (internal/triage/codex_execution_tools.go):
// live on the tray while the child runs, and from the stored rows on the
// completion card.
//
// A child's background command (`unifiedExecStartup`) that finishes after
// the root's turn ended is saved on the child's turn, under the spawn: it
// shows in the agent pane whether the root is idle or running a later
// turn, and a cut of that later turn cannot take it.
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
  // The ownership statement. Until this lands the child thread is
  // "unmapped foreign", and its notifications are quarantined rather
  // than routed — which is exactly why the token tick is gated behind a
  // signal instead of raced against this line.
  //
  // Wire order: the spawn handler emits this activity BEFORE it returns,
  // so the started activity always precedes the function_call_output
  // (docs/references/codex-wire.md). The session relies on that: an
  // output with no typed activity behind it is a refusal and gets its own
  // result row, which on a reversed order would overwrite the spawn row.
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
  rpc('rawResponseItem/completed', {
    threadId: '${THREAD_ID}',
    turnId: '${TURN_ID}',
    item: {
      type: 'function_call_output',
      call_id: SPAWN_CALL,
      output: JSON.stringify({ task_name: '/root/reviewer' }),
    },
  }),
  // The child's own turn opens on ITS thread; the spawn output above
  // never says so. This is what drives the running state (and Stop).
  rpc('turn/started', { threadId: CHILD_THREAD, turn: { id: 'child-turn' } }),
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

// Codex core renders the FINAL_ANSWER envelope FROM the child's terminal
// status, so the child's turn/completed always precedes it; the completion
// record is written when the envelope lands and carries it as payload.
const FINAL_ANSWER_LINES = [
  rpc('turn/completed', { threadId: CHILD_THREAD, turn: { id: 'child-turn', status: 'completed' } }),
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
  await harness.open(page);
  await page.getByText(title).click();
  const mockId = await startMock(harness, threadId);
  await harness.send(threadId, 'review this');
  return mockId;
}

test('a Codex spawn_agent child keeps its launched row, opens the same pane, and gets its card at the completion, whose answer renders once', async ({
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
  await expect(trayRow.getByTestId('background-task-tray-row-tools')).toHaveCount(0);

  // Child tool calls stay in the pane transcript. The live tray keeps only
  // the newest direct call, and the historical spawn row does not grow.
  await waitForGate(harness, 'tools');
  await advance(harness, mockId, 'tools');
  await expect(trayRow.getByTestId('background-task-tray-row-tools')).toHaveText('2 tools');
  await expect(trayRow.getByTestId('background-task-tray-row-activity')).toContainText('pnpm test');
  await expect(trayRow.getByTestId('background-task-tray-row-activity')).not.toContainText('rg TODO');
  await expect(spawnRow).not.toContainText('pnpm test');
  // The row's header expands the child's digest in place: both calls, as
  // normal command rows, off the same child rows the pane reads.
  await trayRow.getByTestId('collab-tool-row-toggle').click();
  const digest = trayRow.getByTestId('background-task-tray-row-digest');
  await expect(digest).toHaveAttribute('data-scope-id', SPAWN_CALL);
  await expect(digest.locator('[data-item-id="child-tool-read"]')).toBeVisible();
  await expect(digest.locator('[data-item-id="child-tool-test"]')).toBeVisible();
  await expect(digest).toContainText('rg TODO');
  await expect(digest).toContainText('pnpm test');
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
  // A running child turn is owned by this session, so the pane offers Stop
  // (turn/interrupt on the child thread) while it runs.
  await expect(pane.getByTestId('agent-pane-stop')).toHaveCount(1);

  // The full child transcript keeps both calls even though the tray shows
  // only the latest one.
  await expect(pane.getByTestId('agent-pane-timeline')).toContainText('rg TODO');
  await expect(pane.getByTestId('agent-pane-timeline')).toContainText('pnpm test');

  // --- The child's answer lands: the card, at the completion ---------
  await waitForGate(harness, 'answer');
  await advance(harness, mockId, 'answer');
  await harness.waitForEvent('provider:turn_completed');
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(1);
  await expect(pane.getByTestId('agent-pane-stop')).toHaveCount(0);
  const card = timeline.getByTestId('subagent-group').first();
  await expect(card.getByTestId('subagent-group-kind')).toHaveText('agent');
  await expect(card.getByTestId('subagent-group-label')).toContainText('reviewer');
  await expect(card.getByTestId('subagent-group-description')).toHaveCount(0);
  await expect(card).toHaveAttribute('data-background', 'true');
  await expect(card.getByTestId('subagent-group-background-button')).toHaveCount(0);
  await expect(card.getByTestId('subagent-group-tokens')).toHaveText('3.4k tokens');
  await expect(card.getByTestId('subagent-group-tools')).toHaveText('2 tools');
  await expect(card.getByTestId('subagent-group-duration')).toBeVisible();
  await expect(card.getByTestId('subagent-group-open-pane')).toHaveCount(1);
  // The card sits below the launch row, at the completion point.
  const [spawnBox, cardBox] = await Promise.all([spawnRow.boundingBox(), card.boundingBox()]);
  expect(cardBox!.y).toBeGreaterThan(spawnBox!.y + spawnBox!.height - 1);
  // The launch row is still the launch row.
  await expect(spawnRow.getByTestId('collab-tool-row')).toHaveCount(1);

  // --- The answer renders ONCE, as its own message, formatted and whole
  // The answer exists in three places: the child's own assistant row
  // (parented to the spawn), the FINAL_ANSWER envelope that puts it into
  // the parent's model context, and the 240-char `payloadMeta.preview` on
  // the completion sibling. Only the first is a message. The preview is
  // the COLLAPSED one-liner and nothing else — rendering it in the body
  // too showed the same text twice, unformatted and cut mid-word (user
  // ruling 2026-08-23).
  await expect(card.getByTestId('subagent-group-preview')).toContainText(FINAL_ANSWER);
  await card.getByTestId('subagent-group-toggle').first().click();
  const body = card.getByTestId('subagent-group-body');
  await expect(body).toContainText(FINAL_ANSWER);
  await expect(card.getByTestId('subagent-group-final-answer')).toHaveCount(0);
  await expect(body.getByText(FINAL_ANSWER, { exact: false })).toHaveCount(1);
  // The card's own door opens the child's pane with the answer in it.
  await pane.getByTestId('agent-pane-close').click();
  await expect(pane).toHaveCount(0);
  await card.getByTestId('subagent-group-open-pane').first().click();
  await expect(pane.getByTestId('agent-pane-timeline')).toContainText(FINAL_ANSWER);
  await expect(pane.getByTestId('agent-pane-final-answer')).toHaveCount(0);
  await expect(pane.getByTestId('agent-pane-empty')).toHaveCount(0);
});

const CHILD_BG_CMD = 'child-bg-cmd';
const childBackgroundCommand = (status: 'inProgress' | 'completed') => rpc(
  `item/${status === 'inProgress' ? 'started' : 'completed'}`,
  {
    threadId: CHILD_THREAD,
    turnId: 'child-turn',
    item: {
      id: CHILD_BG_CMD, type: 'commandExecution', source: 'unifiedExecStartup', processId: '77',
      status, command: 'sleep 30 && echo built',
      ...(status === 'completed' ? { exitCode: 0, aggregatedOutput: 'built\n' } : {}),
    },
  },
);
// codex 0.162's started activity reports the child's profile, so the app
// makes no child `thread/resume` read, which the mock answers by rebinding
// ${THREAD_ID} to the child for the root's later turn.
const PROFILED_SPAWN_LINES = SPAWN_LINES.map(line => line.includes('"subAgentActivity"')
  ? rpc('item/completed', {
    threadId: '${THREAD_ID}',
    turnId: '${TURN_ID}',
    item: {
      type: 'subAgentActivity', id: SPAWN_CALL, kind: 'started', agentThreadId: CHILD_THREAD,
      agentPath: '/root/reviewer', model: 'gpt-5.6-sol', reasoningEffort: 'high',
    },
  })
  : line);
// The echo that confirms the turn's send, so the later message can be cut.
const userEcho = rpc('item/completed', {
  threadId: '${THREAD_ID}',
  turnId: '${TURN_ID}',
  item: { type: 'userMessage', id: 'umsg-${TURN}', status: 'completed', clientId: '${CLIENT_ID}', content: [{ type: 'text', text: '${USER_INPUT}' }] },
});
const rootTurnCompleted = rpc('turn/completed', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}', status: 'completed' } });

// The root's first turn: it spawns the child, the child starts a background
// command, and the root's turn ends with the command still running.
const spawnTurnLines = [
  PROFILED_SPAWN_LINES[0], userEcho, ...PROFILED_SPAWN_LINES.slice(1),
  childBackgroundCommand('inProgress'), rootTurnCompleted,
];

for (const rootRunning of [false, true]) {
  test(`a Codex child\u2019s background command finishing while the root is ${rootRunning ? 'running a later turn' : 'idle'} stays in the agent\u2019s history`, async ({
    harness,
    page,
  }) => {
    const late = [
      { waitSignal: { name: 'late' } },
      { emit: { lines: [childBackgroundCommand('completed')], delayBetweenMs: 5 } },
    ];
    await harness.rpc('HarnessSetScenario', {
      scenario: {
        version: 1,
        name: `codex-child-bg-command-${rootRunning}`,
        provider: 'codex',
        turns: rootRunning ? [
          { steps: [{ emit: { lines: spawnTurnLines, delayBetweenMs: 5 } }] },
          { steps: [
            { emit: { lines: [rpc('turn/started', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}' } }), userEcho], delayBetweenMs: 5 } },
            ...late,
            { waitSignal: { name: 'finish' } },
            { emit: { lines: [
              rpc('turn/completed', { threadId: CHILD_THREAD, turn: { id: 'child-turn', status: 'completed' } }),
              rootTurnCompleted,
            ], delayBetweenMs: 5 } },
          ] },
          // The resend that replaces the later message after its turn is cut.
          { steps: [{ emit: { lines: [
            rpc('turn/started', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}' } }),
            userEcho,
            rootTurnCompleted,
          ], delayBetweenMs: 5 } }] },
        ] : [
          { steps: [{ emit: { lines: spawnTurnLines, delayBetweenMs: 5 } }, ...late] },
        ],
        afterTurns: 'silent',
      },
    });
    const title = `Codex child command ${rootRunning}`;
    const threadId = await seedAgentThread(harness, `codex-child-cmd-${rootRunning}`, title, 'codex');
    await harness.open(page);
    await page.getByText(title).click();
    const mockId = await startMock(harness, threadId);
    const firstTurnDone = harness.waitForEvent('provider:turn_completed');
    await harness.send(threadId, 'build it in the background');
    await firstTurnDone;
    if (rootRunning) await harness.send(threadId, 'keep going');

    await waitForGate(harness, 'late');
    await advance(harness, mockId, 'late');

    interface Row { id: string; parentId: string; turnIndex: number; status: string }
    const spawn = async () => (await harness.items<Row>(threadId, true)).find(row => row.id === SPAWN_CALL);
    await expect.poll(async () => (await harness.items<Row>(threadId, true))
      .filter(row => row.id === CHILD_BG_CMD)
      .map(row => [row.parentId, row.turnIndex, row.status]))
      .toEqual([[SPAWN_CALL, (await spawn())!.turnIndex, 'completed']]);

    const spawnRow = page.getByTestId('message-timeline-scroll').locator(`[data-item-id="${SPAWN_CALL}"]`);
    await spawnRow.hover();
    await spawnRow.getByTestId('collab-tool-row-open-pane').click();
    const paneTimeline = page.getByTestId('companion-pane-agent-body').getByTestId('agent-pane-timeline');
    await expect(paneTimeline.locator(`[data-item-id="${CHILD_BG_CMD}"]`)).toBeVisible();
    await expect(paneTimeline).toContainText('sleep 30');
    if (!rootRunning) return;
    const laterTurnDone = harness.waitForEvent('provider:turn_completed');
    await waitForGate(harness, 'finish');
    await advance(harness, mockId, 'finish');
    await laterTurnDone;

    // Cutting the later turn keeps the agent's command; the cut message is
    // replaced by the resend.
    const later = (await harness.items<Row & { kind: string; summary: string }>(threadId, true))
      .find(row => row.kind === 'user_text' && row.summary === 'keep going');
    expect(later).toBeTruthy();
    await harness.rpc('RevertConversationAndResendMessage', threadId, later!.id, { content: 'keep going, rephrased' });
    await expect.poll(async () => (await harness.items<Row & { summary: string }>(threadId, true))
      .filter(row => row.id === CHILD_BG_CMD || row.summary === 'keep going')
      .map(row => row.id))
      .toEqual([CHILD_BG_CMD]);
    await expect(paneTimeline.locator(`[data-item-id="${CHILD_BG_CMD}"]`)).toBeVisible();
  });
}

// Two executions of one child: the first leaves a background command
// running and answers; the root's follow-up turn wakes the child for a
// second execution, during which the first one's command exits. The command
// shows in the agent pane where it exited, but it belongs to the first
// execution, whose card was already saved: it is counted in neither card
// and is not in the second card's body.
test('a Codex child’s command from an earlier execution is in neither card when it exits during a later one', async ({
  harness,
  page,
}) => {
  const childTool = (id: string, turnId: string) => [
    rpc('item/started', { threadId: CHILD_THREAD, turnId, item: { id, type: 'commandExecution', status: 'inProgress', command: id } }),
    rpc('item/completed', { threadId: CHILD_THREAD, turnId, item: { id, type: 'commandExecution', status: 'completed', command: id, exitCode: 0 } }),
  ];
  const childAnswer = (turnId: string, id: string, text: string) => [
    rpc('item/completed', { threadId: CHILD_THREAD, turnId, item: { id, type: 'agentMessage', text } }),
    rpc('turn/completed', { threadId: CHILD_THREAD, turn: { id: turnId, status: 'completed' } }),
    rpc('rawResponseItem/completed', {
      threadId: '${THREAD_ID}',
      turnId: '${TURN_ID}',
      item: {
        type: 'agent_message', id: `amsg-${id}`, author: '/root/reviewer', recipient: '/root',
        content: [{ type: 'input_text', text: `Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/reviewer\nPayload:\n${text}` }],
        internal_chat_message_metadata_passthrough: { turn_id: '${TURN_ID}' },
      },
    }),
  ];
  const rootStart = rpc('turn/started', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}' } });
  await harness.rpc('HarnessSetScenario', {
    scenario: {
      version: 1,
      name: 'codex-child-command-two-executions',
      provider: 'codex',
      turns: [
        { steps: [{ emit: { lines: [
          PROFILED_SPAWN_LINES[0], userEcho, ...PROFILED_SPAWN_LINES.slice(1),
          childBackgroundCommand('inProgress'),
          ...childAnswer('child-turn', 'answer-a', 'First pass done; the build keeps running.'),
          rootTurnCompleted,
        ], delayBetweenMs: 5 } }] },
        { steps: [
          { emit: { lines: [
            rootStart, userEcho,
            // The root's follow-up wakes the child for its second execution.
            rpc('rawResponseItem/completed', {
              threadId: '${THREAD_ID}', turnId: '${TURN_ID}',
              item: {
                type: 'function_call', name: 'followup_task', namespace: 'collaboration', call_id: 'call_followup_1',
                arguments: JSON.stringify({ target: '/root/reviewer', message: '<encrypted>' }),
              },
            }),
            rpc('item/completed', {
              threadId: '${THREAD_ID}', turnId: '${TURN_ID}',
              item: { type: 'subAgentActivity', id: 'call_followup_1', kind: 'interacted', agentThreadId: CHILD_THREAD, agentPath: '/root/reviewer' },
            }),
            rpc('turn/started', { threadId: CHILD_THREAD, turn: { id: 'child-turn-b' } }),
            ...childTool('child-b1', 'child-turn-b'),
          ], delayBetweenMs: 5 } },
          { waitSignal: { name: 'late' } },
          { emit: { lines: [childBackgroundCommand('completed')], delayBetweenMs: 5 } },
          { waitSignal: { name: 'finish' } },
          { emit: { lines: [
            ...childTool('child-b2', 'child-turn-b'),
            ...childAnswer('child-turn-b', 'answer-b', 'Second pass done.'),
            rootTurnCompleted,
          ], delayBetweenMs: 5 } },
        ] },
      ],
      afterTurns: 'silent',
    },
  });
  const title = 'Codex child two executions';
  const threadId = await seedAgentThread(harness, 'codex-child-two-runs', title, 'codex');
  await harness.open(page);
  await page.getByText(title).click();
  const mockId = await startMock(harness, threadId);
  const firstTurnDone = harness.waitForEvent('provider:turn_completed');
  await harness.send(threadId, 'review, and keep the build running');
  await firstTurnDone;
  const timeline = page.getByTestId('message-timeline-scroll');
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(1);
  const cardA = timeline.getByTestId('subagent-group').first();
  await expect(cardA.getByTestId('subagent-group-tools')).toHaveCount(0);

  await harness.send(threadId, 'one more pass');
  // The running execution's live count, as the backend answers it.
  const liveTools = async () => ((await harness.rpc<{ subagentProgress: { itemId: string; progress: { toolUses?: number } }[] }>('GetThreadLiveState', threadId))
    .subagentProgress.find(entry => entry.itemId === SPAWN_CALL)?.progress.toolUses ?? 0);
  await waitForGate(harness, 'late');
  await expect.poll(liveTools).toBe(1);
  await advance(harness, mockId, 'late');
  interface Row { id: string; parentId: string; turnIndex: number; meta?: string }
  await expect.poll(async () => (await harness.items<Row>(threadId, true))
    .filter(row => row.id === CHILD_BG_CMD)
    .map(row => [row.parentId, JSON.parse(row.meta ?? '{}').codex_execution_turn_id]))
    .toEqual([[SPAWN_CALL, 'child-turn']]);
  // The first execution's command does not join the running one's count.
  expect(await liveTools()).toBe(1);

  const laterTurnDone = harness.waitForEvent('provider:turn_completed');
  await waitForGate(harness, 'finish');
  await advance(harness, mockId, 'finish');
  await laterTurnDone;
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(2);
  const cardB = timeline.getByTestId('subagent-group').nth(1);
  await expect(cardB.getByTestId('subagent-group-tools')).toHaveText('2 tools');
  await expect(cardA.getByTestId('subagent-group-tools')).toHaveCount(0);
  await cardB.getByTestId('subagent-group-toggle').first().click();
  const bodyB = cardB.getByTestId('subagent-group-body');
  await expect(bodyB.locator('[data-item-id="child-b2"]')).toBeVisible();
  await expect(bodyB.locator('[data-item-id="child-b1"]')).toBeVisible();
  await expect(bodyB.locator(`[data-item-id="${CHILD_BG_CMD}"]`)).toHaveCount(0);

  // The pane is the agent's continuous history: the command sits where it
  // exited, between the second execution's two calls.
  await cardB.getByTestId('subagent-group-open-pane').first().click();
  const paneTimeline = page.getByTestId('companion-pane-agent-body').getByTestId('agent-pane-timeline');
  await paneTimeline.getByRole('button', { name: /Expand 3 activity rows/ }).click();
  const calls = paneTimeline.locator('[data-item-id="child-b1"], [data-item-id="child-bg-cmd"], [data-item-id="child-b2"]');
  await expect(calls).toHaveCount(3);
  expect(await calls.evaluateAll(nodes => nodes.map(node => node.getAttribute('data-item-id'))))
    .toEqual(['child-b1', CHILD_BG_CMD, 'child-b2']);
});
