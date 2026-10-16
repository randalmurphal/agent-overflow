// Text Claude discards after a stalled response stays, marked, and the
// reply continues below it.
//
// Claude Code 2.1.300 closes a response it stopped reading with its own
// content_block_stop and a message_stop carrying `abandoned_blocks`
// (docs/references/claude-wire.md §abandoned_blocks): the blocks from that
// index on were streamed but are not in the transcript. The rows those
// blocks streamed into must stay exactly as the user saw them, muted and
// tagged, with the record row after them, and what Claude wrote next must
// render below. Nothing disappears and nothing re-streams in place
// (docs/decisions.md §Streaming and reveal). The marking is derived from
// the record row, so it must hold across a reload.
//
// Three shapes from the captures behind the fixtures, each in the light
// and the dark theme: block 1 cut after block 0 finished (continuation
// streams under a new id), the stall inside block 0 (the whole response
// discarded, the retry a bare assistant snapshot under a new id), and a
// thinking block cut the same way.
import type { Locator, Page, TestInfo } from '@playwright/test';
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  claudeScenario,
  emit,
  itemMeta,
  listItems,
  seedAgentThread,
  startMock,
} from './agent-visibility-helpers.js';

const j = (value: unknown): string => JSON.stringify(value);

/** A stream_event envelope the way 2.1.300 writes it: the inner event under
 * `event`, the response id beside it. */
function stream(apiMessageId: string, event: Record<string, unknown>, extra: Record<string, unknown> = {}): string {
  return j({ type: 'stream_event', event, session_id: 's', parent_tool_use_id: null, api_message_id: apiMessageId, ...extra });
}

function textDeltas(apiMessageId: string, index: number, chunks: string[]): string[] {
  return chunks.map((text) => stream(apiMessageId, { type: 'content_block_delta', index, delta: { type: 'text_delta', text } }));
}

function thinkingDeltas(apiMessageId: string, index: number, chunks: string[]): string[] {
  return chunks.map((thinking) => stream(apiMessageId, { type: 'content_block_delta', index, delta: { type: 'thinking_delta', thinking } }));
}

/** The CLI's own close of a response it stopped reading. */
function abandonLines(apiMessageId: string, openBlockIndex: number | null, fromBlockIndex: number): string[] {
  return [
    ...(openBlockIndex === null ? [] : [stream(apiMessageId, { type: 'content_block_stop', index: openBlockIndex })]),
    stream(apiMessageId, { type: 'message_stop' }, { abandoned_blocks: { api_message_id: apiMessageId, from_block_index: fromBlockIndex } }),
    // The synthetic nudge the CLI sends itself; AO drops it.
    j({ type: 'user', message: { role: 'user', content: [{ type: 'text', text: 'Your response above was cut off mid-stream. Resume directly from where it stops.' }] }, parent_tool_use_id: null, isSynthetic: true }),
  ];
}

const PARTIAL_HEAD = '## What the parser does\n\nThe envelope carries the response id beside the inner event, so a consumer that missed the `message_start` can still tell responses apart. Each block start names its response and index.\n\n';
const PARTIAL_BODY = 'Triage binds the row the first delta creates to that block. When the stall comes, the CLI closes the open block itself and the row settles as any other block would.\n\n- the row keeps its text\n- the record lands after it\n- the reply ';
const PARTIAL_FENCE = 'continues below it:\n\n```go\nfunc (r *Router) handleBlocksAbandoned(evt provider.ProviderEvent) error {\n\tvar meta provider.BlocksAbandonedMeta\n\tif len(evt.Meta) == 0 {';
const PARTIAL = PARTIAL_HEAD + PARTIAL_BODY + PARTIAL_FENCE;

const FIRST_BLOCK = 'The first block finished before the stall, so it stays part of the answer.';
const CONTINUATION = 'here, after the stall. The second half arrived from the continuation request, and the reply ends normally.';
const RETRY = 'Retried from scratch: the parser reads the envelope, triage binds the rows, and the record names what the stall discarded. This reply replaces the draft above.';
const THINKING = 'Let me work through the stall handling. The envelope names the response, the block start names the index, and the first delta binds the row. If the stream stalls here the CLI closes the block and ';

type Case = 'continuation' | 'retry' | 'thinking';

function scenario(kind: Case): unknown {
  const chunk = (text: string): string[] => text.match(/[\s\S]{1,24}/g) ?? [];
  const msg1 = 'msg_abandoned_01';
  const msg2 = 'msg_abandoned_02';
  const start = (id: string) => stream(id, { type: 'message_start', message: { id, role: 'assistant', model: 'claude-mock-1', content: [], stop_reason: null, usage: { input_tokens: 10, output_tokens: 1 } } });
  const blockStart = (id: string, index: number, type: 'text' | 'thinking') =>
    stream(id, { type: 'content_block_start', index, content_block: type === 'text' ? { type, text: '' } : { type, thinking: '' } });
  const blockStop = (id: string, index: number) => stream(id, { type: 'content_block_stop', index });
  const snapshot = (id: string, content: unknown[], stopReason: string | null = null) =>
    j({ type: 'assistant', message: { id, role: 'assistant', model: 'claude-mock-1', content, stop_reason: stopReason }, parent_tool_use_id: null });
  const end = (id: string) => [
    stream(id, { type: 'message_delta', delta: { stop_reason: 'end_turn', stop_sequence: null }, usage: { output_tokens: 20 } }),
    stream(id, { type: 'message_stop' }),
  ];

  if (kind === 'continuation') {
    return claudeScenario('claude-abandoned-continuation', [
      emit([
        start(msg1),
        blockStart(msg1, 0, 'text'),
        ...textDeltas(msg1, 0, chunk(FIRST_BLOCK)),
        // The finished block's snapshot arrives before its stop, as captured.
        snapshot(msg1, [{ type: 'text', text: FIRST_BLOCK }]),
        blockStop(msg1, 0),
        blockStart(msg1, 1, 'text'),
        ...textDeltas(msg1, 1, chunk(PARTIAL)),
        ...abandonLines(msg1, 1, 1),
        start(msg2),
        blockStart(msg2, 0, 'text'),
        ...textDeltas(msg2, 0, chunk(CONTINUATION)),
        snapshot(msg2, [{ type: 'text', text: CONTINUATION }]),
        blockStop(msg2, 0),
        ...end(msg2),
        RESULT_LINE,
      ]),
    ]);
  }
  if (kind === 'retry') {
    return claudeScenario('claude-abandoned-retry', [
      emit([
        start(msg1),
        blockStart(msg1, 0, 'text'),
        ...textDeltas(msg1, 0, chunk(PARTIAL)),
        ...abandonLines(msg1, 0, 0),
        // The retry arrives whole, under a new id, with no message_start.
        snapshot(msg2, [{ type: 'text', text: RETRY }], 'end_turn'),
        RESULT_LINE,
      ]),
    ]);
  }
  return claudeScenario('claude-abandoned-thinking', [
    emit([
      start(msg1),
      blockStart(msg1, 0, 'thinking'),
      ...thinkingDeltas(msg1, 0, chunk(THINKING)),
      ...abandonLines(msg1, 0, 0),
      snapshot(msg2, [{ type: 'text', text: RETRY }], 'end_turn'),
      RESULT_LINE,
    ]),
  ]);
}

interface Expected {
  /** The row the user saw, which the discard must leave in place. */
  discardedText: string;
  /** A row that must stay unmarked, if the case has one. */
  keptText?: string;
  /** What Claude wrote after the discard. */
  nextText: string;
  wholeResponse: boolean;
  tail: string;
}

const EXPECTED: Record<Case, Expected> = {
  continuation: {
    discardedText: 'the row keeps its text',
    keptText: FIRST_BLOCK,
    nextText: CONTINUATION,
    wholeResponse: false,
    tail: 'Claude picked the reply up below.',
  },
  retry: {
    discardedText: 'the row keeps its text',
    nextText: RETRY,
    wholeResponse: true,
    tail: 'Claude started the reply over below.',
  },
  thinking: {
    discardedText: 'the first delta binds the row',
    nextText: RETRY,
    wholeResponse: true,
    tail: 'Claude started the reply over below.',
  },
};

async function expectMarked(timeline: Locator, kind: Case): Promise<void> {
  const want = EXPECTED[kind];
  if (kind === 'thinking') {
    // A thinking row is an activity run of its own, collapsed by default
    // after a reload; the record must stand outside it, so the run holds
    // the one row and expanding it shows the marked reasoning.
    const runs = timeline.getByTestId('activity-run');
    await expect(runs).toHaveCount(1);
    await expect(runs.getByTestId('activity-run-header-counts')).toHaveText(/^1 thinking$/);
    if ((await runs.getAttribute('data-collapsed')) === 'true') {
      await runs.getByTestId('activity-run-header').click();
      await expect(runs).toHaveAttribute('data-collapsed', 'false');
    }
  }
  const discarded = timeline.locator('[data-item-id][data-discarded="true"]');
  await expect(discarded).toHaveCount(1);
  await expect(discarded).toHaveAttribute('data-item-kind', kind === 'thinking' ? 'thinking' : 'assistant_text');
  await expect(discarded).toHaveAttribute('data-item-status', 'completed');
  await expect(discarded).toContainText(want.discardedText);
  await expect(discarded.getByTestId(kind === 'thinking' ? 'thinking-discarded' : 'assistant-message-discarded')).toHaveText('Discarded');

  const record = timeline.getByTestId('abandoned-blocks-notification');
  await expect(record).toHaveCount(1);
  await expect(record).toContainText('Discarded by Claude after a stalled response');
  await expect(record).toContainText(want.tail);
  await expect(record).toHaveAttribute('data-whole-response', String(want.wholeResponse));
  await expect(record).toHaveAttribute('data-followed', 'true');

  const next = timeline.locator('[data-item-id][data-item-kind="assistant_text"]', { hasText: want.nextText.slice(0, 40) });
  await expect(next).toHaveCount(1);
  await expect(next).not.toHaveAttribute('data-discarded', 'true');
  // The reply reveals at reading pace; wait for all of it.
  await expect(next).toContainText(want.nextText, { timeout: 20_000 });
  if (want.keptText) {
    const kept = timeline.locator('[data-item-id][data-item-kind="assistant_text"]', { hasText: want.keptText.slice(0, 40) });
    await expect(kept).toHaveCount(1);
    await expect(kept).not.toHaveAttribute('data-discarded', 'true');
  }

  // Order on screen: the discarded row, then the record, then the reply.
  const order = await timeline.locator('[data-item-id]').evaluateAll((rows) =>
    rows.map((row) => ({
      id: row.getAttribute('data-item-id') ?? '',
      discarded: row.getAttribute('data-discarded') === 'true',
      record: row.querySelector('[data-testid="abandoned-blocks-notification"]') !== null,
      text: row.textContent ?? '',
    })),
  );
  const discardedAt = order.findIndex((row) => row.discarded);
  const recordAt = order.findIndex((row) => row.record);
  const nextAt = order.findIndex((row) => row.text.includes(want.nextText.slice(0, 40)));
  expect(discardedAt, 'discarded row rendered').toBeGreaterThanOrEqual(0);
  expect(recordAt, 'record rendered after the discarded row').toBeGreaterThan(discardedAt);
  expect(nextAt, 'reply rendered after the record').toBeGreaterThan(recordAt);
}

async function openThread(page: Page, title: string): Promise<Locator> {
  const timeline = page.getByTestId('message-timeline-scroll');
  await page.getByTestId('thread-row').getByText(title, { exact: true }).click();
  await expect(timeline.getByText('Ready.')).toBeVisible();
  return timeline;
}

async function screenshot(timeline: Locator, testInfo: TestInfo, name: string): Promise<void> {
  const body = await timeline.screenshot();
  await testInfo.attach(name, { body, contentType: 'image/png' });
  await timeline.screenshot({ path: testInfo.outputPath(`${name}.png`) });
}

for (const theme of ['light', 'dark'] as const) {
  for (const kind of ['continuation', 'retry', 'thinking'] as const) {
    test(`${kind}: the discarded ${kind === 'thinking' ? 'reasoning' : 'text'} stays marked and the reply follows (${theme})`, async ({ harness, page }, testInfo) => {
      const title = `Abandoned ${kind} ${theme}`;
      await harness.rpc('HarnessSetScenario', { scenario: scenario(kind) });
      const threadId = await seedAgentThread(harness, `claude-abandoned-${kind}-${theme}`, title);
      // The appearance mode follows the system by default.
      await page.emulateMedia({ colorScheme: theme });
      await page.setViewportSize({ width: 1280, height: 1000 });
      await harness.open(page);
      const timeline = await openThread(page, title);
      await startMock(harness, threadId);

      const completed = harness.waitForEvent('provider:turn_completed');
      await harness.rpc('SendMessage', threadId, 'say something long', null);
      await completed;

      await expectMarked(timeline, kind);
      await screenshot(timeline, testInfo, `abandoned-${kind}-${theme}`);

      // The store: the discarded row is completed history with the text it
      // streamed, and the record names it; nothing was rewritten.
      const items = await listItems(harness, threadId);
      const record = items.find((item) => item.kind === 'notification' && item.toolName === 'abandoned_blocks');
      expect(record, 'abandoned_blocks notification row').toBeTruthy();
      const meta = itemMeta(record);
      const ids = meta.item_ids as string[];
      expect(ids).toHaveLength(1);
      const discardedRow = items.find((item) => item.id === ids[0]);
      expect(discardedRow?.status).toBe('completed');
      expect(discardedRow?.kind).toBe(kind === 'thinking' ? 'thinking' : 'assistant_text');
      if (kind !== 'thinking') {
        expect(discardedRow?.summary).toBe(PARTIAL);
      }
      expect(meta.from_block_index).toBe(kind === 'continuation' ? 1 : 0);
      expect(meta.api_message_id).toBe('msg_abandoned_01');
      expect(items.filter((item) => item.kind === 'assistant_text' || item.kind === 'thinking').map((item) => item.summary))
        .toContain(EXPECTED[kind].nextText);

      // The marking is derived from the record row, so a cold load shows it.
      await page.reload();
      const reloaded = await openThread(page, title);
      await expectMarked(reloaded, kind);
    });
  }
}
