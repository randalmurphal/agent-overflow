// A turn the Claude CLI starts on its own, replayed against the real app.
//
// THE INCIDENT (thread 774f2b92, 2026-10-24). A background command finished
// after the turn that launched it had ended, and the CLI woke itself: a
// fresh system/init with no message from the app, then streamed thinking.
// The app filed that work under the finished turn and read its turn from the
// newest stored row. A message sent while the thinking streamed was written
// to the next turn, the next thinking delta followed it into a second row,
// and the first row never ended. The pane withholds everything after a row
// that is still streaming, so the timeline froze, the sent message stayed
// above the composer, and Stop recorded nothing.
//
// A CLI turn is now a turn of its own (docs/architecture/turn-lifecycle.md
// §Turns the provider starts). The wake opens one, the message sent during
// it is queued, and the CLI drains it mid-loop into the wake's turn, as it
// would into a turn the app started. Verdicts observable only here: the pane
// keeps revealing text written after the send, the message leaves the queue
// preview for the timeline, and no row is left streaming.
//
// With a local_agent still running when the model's message ends, the CLI
// withholds `result` until the agent finishes, and writes system/init for
// each new input it acts on meanwhile: a message the user sends, a wake.
// The ended message settled its turn, so each is a turn of its own after
// it. When the model instead continues its message's turn with no init (a
// ToolSearch or ExitPlanMode continuation), that turn reopens: a message
// sent during the continuation queues behind it like any running turn.
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  claudeTurnsScenario,
  emit,
  listItems,
  seedAgentThread,
  startMock,
  textLines,
  thinkingLines,
  waitForGate,
} from './agent-visibility-helpers.js';

const INIT_LINE = JSON.stringify({ type: 'system', subtype: 'init', model: 'claude-mock-1', tools: [] });
const STARTED = 'Started the batch in the background.';
const WAKE_THINKING_OPEN = 'Batch 5 is fully green; ';
const WAKE_THINKING_FULL = 'Batch 5 is fully green; check the xdist worker count before batch 6.';
const SENT = 'Oracle lane: reviewer B traced the baseline to the bridge.';
const REPLY = 'Took the oracle note: batch 6 waits for the bridge fix.';
const WAITING = 'The reviewer agent is still running.';
const WAIT_SENT = 'While it runs: also check the xdist worker count.';
const WAIT_REPLY = 'Checked: the xdist worker count is 8.';
const WAKE_REPLY = 'The reviewer agent finished: no findings.';
const PLANNED = 'Here is the plan for batch 6.';
const CONT_THINKING_OPEN = 'Loading the deferred tool; ';
const CONT_THINKING_FULL = 'Loading the deferred tool; then exit plan mode.';
const CONT_SENT = 'Also run the flaky suite twice.';
const CONT_REPLY = 'Plan approved: running batch 6 and the flaky suite twice.';

// Streamed text whose message ends with end_turn: the soft round close.
function endedTextLines(messageId: string, text: string): string[] {
  const lines = textLines(messageId, text);
  const end = JSON.stringify({
    type: 'stream_event',
    event: { type: 'message_delta', delta: { stop_reason: 'end_turn', stop_sequence: null }, usage: { output_tokens: 20 } },
    parent_tool_use_id: null,
  });
  return [...lines.slice(0, 4), end, ...lines.slice(4)];
}

test('a message sent while Claude works after waking itself lands and the pane keeps revealing', async ({ harness, page }) => {
  const wakeThinking = thinkingLines('msg-wake', WAKE_THINKING_FULL);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario(
      'claude-self-wake',
      [
        [
          emit([...textLines('msg-start', STARTED), RESULT_LINE]),
          { waitSignal: { name: 'wake' } },
          // The CLI wakes itself: init with no message behind it, then
          // thinking that is still streaming.
          emit([
            INIT_LINE,
            wakeThinking[0],
            wakeThinking[1],
            JSON.stringify({
              type: 'stream_event',
              event: 'content_block_delta',
              data: { type: 'content_block_delta', delta: { type: 'thinking_delta', thinking: WAKE_THINKING_OPEN } },
            }),
          ]),
          { waitSignal: { name: 'drain' } },
          { drainQueuedInput: true },
          emit([
            JSON.stringify({
              type: 'stream_event',
              event: 'content_block_delta',
              data: {
                type: 'content_block_delta',
                delta: { type: 'thinking_delta', thinking: WAKE_THINKING_FULL.slice(WAKE_THINKING_OPEN.length) },
              },
            }),
            ...wakeThinking.slice(3),
            ...textLines('msg-reply', REPLY),
            RESULT_LINE,
          ]),
        ],
      ],
      { holdQueuedInput: true },
    ),
  });

  const threadId = await seedAgentThread(harness, 'claude-self-wake-app', 'Self wake');
  await harness.open(page);
  await page.getByText('Self wake').click();
  const mockId = await startMock(harness, threadId);
  const firstStarted = harness.waitForEvent('provider:turn_started', (ev: any) => ev.threadId === threadId);
  await harness.rpc('SendMessage', threadId, 'start batch 6 in the background', null);
  await firstStarted;

  const timeline = page.getByTestId('message-timeline-scroll');
  await waitForGate(harness, 'wake');
  await expect(timeline.getByText(STARTED)).toBeVisible({ timeout: 20_000 });

  const wakeStarted = harness.waitForEvent('provider:turn_started', (ev: any) => ev.threadId === threadId);
  await advance(harness, mockId, 'wake');
  const wakeTurnStart = (await wakeStarted) as any;
  await waitForGate(harness, 'drain');

  // The wake is a turn of its own and still running: the message queues.
  await page.getByLabel('Message Input').fill(SENT);
  await page.getByTestId('composer-send').click();
  const preview = page.getByTestId('send-queue-preview');
  await expect(preview.getByText(SENT)).toBeVisible({ timeout: 20_000 });

  const done = harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await advance(harness, mockId, 'drain');
  await done;

  // Text written after the send reveals: nothing froze the pane.
  await expect(timeline.getByText(REPLY)).toBeVisible({ timeout: 30_000 });
  await expect(timeline.getByText(SENT)).toBeVisible();
  await expect(preview).toHaveCount(0);

  const items = await listItems(harness, threadId);
  const stuck = items.filter((item) => item.status === 'streaming' || item.status === 'running');
  expect(stuck, 'rows left streaming or running').toEqual([]);
  const thinking = items.filter((item) => item.kind === 'thinking');
  expect(thinking.map((item) => item.status)).toEqual(['completed']);

  const turnOf = (id: string) => (items.find((item) => item.id === id) as any)?.turnIndex as number | undefined;
  const userRow = items.find((item) => item.kind === 'user_text' && item.summary === SENT);
  expect(userRow, 'sent message row').toBeTruthy();
  const startedRow = items.find((item) => item.kind === 'assistant_text' && item.summary === STARTED);
  const wakeTurn = turnOf(thinking[0].id);
  const replyRow = items.find((item) => item.kind === 'assistant_text' && item.summary === REPLY);
  // The wake is not filed under the turn that had already finished.
  expect(wakeTurn).toBe(turnOf(startedRow!.id)! + 1);
  expect(wakeTurnStart.turnIndex).toBe(wakeTurn);
  expect(turnOf(userRow!.id)).toBe(wakeTurn);
  expect(turnOf(replyRow!.id)).toBe(wakeTurn);
});

test('a message sent while the result is withheld opens its own turn', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario(
      'claude-withheld-result-send',
      [
        [
          emit(endedTextLines('msg-waiting', WAITING)),
          { waitSignal: { name: 'send' } },
          { drainQueuedInput: true },
          emit([INIT_LINE, ...textLines('msg-wait-reply', WAIT_REPLY), RESULT_LINE]),
        ],
      ],
      { holdQueuedInput: true },
    ),
  });

  const threadId = await seedAgentThread(harness, 'claude-withheld-send-app', 'Withheld send');
  await harness.open(page);
  await page.getByText('Withheld send').click();
  const mockId = await startMock(harness, threadId);
  const firstStarted = harness.waitForEvent('provider:turn_started', (ev: any) => ev.threadId === threadId);
  const roundEnded = harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await harness.rpc('SendMessage', threadId, 'review batch 6 with an agent', null);
  await firstStarted;

  const timeline = page.getByTestId('message-timeline-scroll');
  await waitForGate(harness, 'send');
  await roundEnded;
  await expect(timeline.getByText(WAITING)).toBeVisible({ timeout: 20_000 });

  const messageTurn = harness.waitForEvent('provider:turn_started', (ev: any) => ev.threadId === threadId);
  await page.getByLabel('Message Input').fill(WAIT_SENT);
  await page.getByTestId('composer-send').click();
  const done = harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await advance(harness, mockId, 'send');
  const started = (await messageTurn) as any;
  await done;

  await expect(timeline.getByText(WAIT_REPLY)).toBeVisible({ timeout: 30_000 });
  await expect(timeline.getByText(WAIT_SENT)).toBeVisible();
  await expect(page.getByTestId('send-queue-preview')).toHaveCount(0);

  const items = await listItems(harness, threadId);
  expect(items.filter((item) => item.status === 'streaming' || item.status === 'running'), 'rows left streaming or running').toEqual([]);
  const turnOf = (kind: string, summary: string) =>
    (items.find((item) => item.kind === kind && item.summary === summary) as any)?.turnIndex as number | undefined;
  const waitingTurn = turnOf('assistant_text', WAITING)!;
  expect(waitingTurn).toBeDefined();
  // The thread was idle: the message and its response are the next turn.
  expect(turnOf('user_text', WAIT_SENT)).toBe(waitingTurn + 1);
  expect(turnOf('assistant_text', WAIT_REPLY)).toBe(waitingTurn + 1);
  expect(started.turnIndex).toBe(waitingTurn + 1);
});

test('a wake while the result is withheld is a turn of its own', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('claude-withheld-result-wake', [
      [
        emit(endedTextLines('msg-waiting', WAITING)),
        { waitSignal: { name: 'wake' } },
        emit([INIT_LINE, ...textLines('msg-wake-reply', WAKE_REPLY), RESULT_LINE]),
      ],
    ]),
  });

  const threadId = await seedAgentThread(harness, 'claude-withheld-wake-app', 'Withheld wake');
  await harness.open(page);
  await page.getByText('Withheld wake').click();
  const mockId = await startMock(harness, threadId);
  const firstStarted = harness.waitForEvent('provider:turn_started', (ev: any) => ev.threadId === threadId);
  const roundEnded = harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await harness.rpc('SendMessage', threadId, 'review batch 6 with an agent', null);
  await firstStarted;

  const timeline = page.getByTestId('message-timeline-scroll');
  await waitForGate(harness, 'wake');
  await roundEnded;
  await expect(timeline.getByText(WAITING)).toBeVisible({ timeout: 20_000 });

  const wakeStarted = harness.waitForEvent('provider:turn_started', (ev: any) => ev.threadId === threadId);
  const done = harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await advance(harness, mockId, 'wake');
  const started = (await wakeStarted) as any;
  await done;

  await expect(timeline.getByText(WAKE_REPLY)).toBeVisible({ timeout: 30_000 });
  const items = await listItems(harness, threadId);
  expect(items.filter((item) => item.status === 'streaming' || item.status === 'running'), 'rows left streaming or running').toEqual([]);
  const turnOf = (summary: string) =>
    (items.find((item) => item.kind === 'assistant_text' && item.summary === summary) as any)?.turnIndex as number | undefined;
  const waitingTurn = turnOf(WAITING)!;
  expect(waitingTurn).toBeDefined();
  expect(turnOf(WAKE_REPLY)).toBe(waitingTurn + 1);
  expect(started.turnIndex).toBe(waitingTurn + 1);
});

test('a message sent while the model continues its ended message lands in that turn', async ({ harness, page }) => {
  const contThinking = thinkingLines('msg-cont', CONT_THINKING_FULL);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario(
      'claude-soft-close-continuation',
      [
        [
          emit(endedTextLines('msg-planned', PLANNED)),
          { waitSignal: { name: 'resume' } },
          // The model continues the same CLI turn with no init.
          emit([
            contThinking[0],
            contThinking[1],
            JSON.stringify({
              type: 'stream_event',
              event: 'content_block_delta',
              data: { type: 'content_block_delta', delta: { type: 'thinking_delta', thinking: CONT_THINKING_OPEN } },
            }),
          ]),
          { waitSignal: { name: 'drain' } },
          { drainQueuedInput: true },
          emit([
            JSON.stringify({
              type: 'stream_event',
              event: 'content_block_delta',
              data: {
                type: 'content_block_delta',
                delta: { type: 'thinking_delta', thinking: CONT_THINKING_FULL.slice(CONT_THINKING_OPEN.length) },
              },
            }),
            ...contThinking.slice(3),
            ...textLines('msg-cont-reply', CONT_REPLY),
            RESULT_LINE,
          ]),
        ],
      ],
      { holdQueuedInput: true },
    ),
  });

  const threadId = await seedAgentThread(harness, 'claude-continuation-app', 'Plan continuation');
  await harness.open(page);
  await page.getByText('Plan continuation').click();
  const mockId = await startMock(harness, threadId);
  const firstStarted = harness.waitForEvent('provider:turn_started', (ev: any) => ev.threadId === threadId);
  const roundEnded = harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await harness.rpc('SendMessage', threadId, 'plan batch 6', null);
  await firstStarted;

  const timeline = page.getByTestId('message-timeline-scroll');
  await waitForGate(harness, 'resume');
  await roundEnded;
  await expect(timeline.getByText(PLANNED)).toBeVisible({ timeout: 20_000 });

  const continued = harness.waitForEvent('provider:turn_started', (ev: any) => ev.threadId === threadId);
  await advance(harness, mockId, 'resume');
  const contRound = (await continued) as any;
  await waitForGate(harness, 'drain');

  // The continuation is running: the message queues behind it.
  await page.getByLabel('Message Input').fill(CONT_SENT);
  await page.getByTestId('composer-send').click();
  const preview = page.getByTestId('send-queue-preview');
  await expect(preview.getByText(CONT_SENT)).toBeVisible({ timeout: 20_000 });

  const done = harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await advance(harness, mockId, 'drain');
  await done;

  await expect(timeline.getByText(CONT_REPLY)).toBeVisible({ timeout: 30_000 });
  await expect(timeline.getByText(CONT_SENT)).toBeVisible();
  await expect(preview).toHaveCount(0);

  const items = await listItems(harness, threadId);
  expect(items.filter((item) => item.status === 'streaming' || item.status === 'running'), 'rows left streaming or running').toEqual([]);
  const thinking = items.filter((item) => item.kind === 'thinking');
  expect(thinking.map((item) => item.status)).toEqual(['completed']);
  const turnOf = (kind: string, summary: string) =>
    (items.find((item) => item.kind === kind && item.summary === summary) as any)?.turnIndex as number | undefined;
  const plannedTurn = turnOf('assistant_text', PLANNED)!;
  expect(plannedTurn).toBeDefined();
  expect(contRound.turnIndex).toBe(plannedTurn);
  expect((thinking[0] as any).turnIndex).toBe(plannedTurn);
  expect(turnOf('user_text', CONT_SENT)).toBe(plannedTurn);
  expect(turnOf('assistant_text', CONT_REPLY)).toBe(plannedTurn);
});
