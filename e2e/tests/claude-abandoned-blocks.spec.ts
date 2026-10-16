// Text Claude discards after a stalled response stays, marked, and the
// reply continues below it.
//
// Claude Code 2.1.300 closes a response it stopped reading with its own
// content_block_stop and a message_stop carrying `abandoned_blocks`
// (docs/references/claude-wire.md §abandoned_blocks): the blocks from that
// index on were streamed but are not in the transcript. The rows those
// blocks streamed into must stay exactly as the user saw them, muted (a
// text row also tagged), with the record row after them, and what Claude
// wrote next must render below. Nothing disappears and nothing re-streams in place
// (docs/decisions.md §Streaming and reveal). The server serves a named row
// marked (`discardedBy`), so the marking holds across a reload; the edge
// where the record is outside the loaded window is
// claude-abandoned-blocks-paging.spec.ts.
//
// The mark lands on a row the user has already read, so it must not move
// anything: the row's box, its text box and the line after its text are
// measured settled before the record lands and again after it, and must
// match. The dimming must resolve to the theme's fade strength.
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
  advance,
  claudeScenario,
  emit,
  itemMeta,
  listItems,
  seedAgentThread,
  startMock,
  waitForGate,
  type ScenarioStep,
} from './agent-visibility-helpers.js';

const j = (value: unknown): string => JSON.stringify(value);

const GATE_RECORD = 'abandon-record';
const GATE_REPLY = 'abandon-reply';

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

/** The CLI's own close of the block it stopped reading. */
function closeOpenBlock(apiMessageId: string, openBlockIndex: number): string {
  return stream(apiMessageId, { type: 'content_block_stop', index: openBlockIndex });
}

/** The CLI's record of the discard, after the open block's close. */
function abandonLines(apiMessageId: string, fromBlockIndex: number): string[] {
  return [
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
  // The discarded row settles, then the record lands, then the reply
  // streams, each step held at a gate so the spec can measure the row
  // between them.
  const abandon = (id: string, fromBlockIndex: number): ScenarioStep[] => [
    { waitSignal: { name: GATE_RECORD } },
    emit(abandonLines(id, fromBlockIndex)),
    { waitSignal: { name: GATE_REPLY } },
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
        closeOpenBlock(msg1, 1),
      ]),
      ...abandon(msg1, 1),
      emit([
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
        closeOpenBlock(msg1, 0),
      ]),
      ...abandon(msg1, 0),
      emit([
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
      closeOpenBlock(msg1, 0),
    ]),
    ...abandon(msg1, 0),
    emit([
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

type Theme = 'light' | 'dark';

/** The computed dimming under the default theme: per mode and row kind,
 * the least that keeps the dimmed text at the 4.5:1 body floor
 * (theme/discardedDim.ts; its browser test measures every built-in). Light
 * reasoning text is at the floor undimmed, so it is not dimmed. */
const OPACITY: Record<Theme, { text: string; reasoning: string }> = {
  dark: { text: '0.61', reasoning: '0.87' },
  light: { text: '0.7', reasoning: '1' },
};

const DISCARDED_LABEL = 'Discarded by Claude after the response stalled';

/** A text row's tag sits beside its time; a reasoning row's kind icon
 * becomes the record's scissors in the same box, since a visible tag would
 * narrow the text beside it. */
async function expectDiscardedCue(row: Locator, kind: 'text' | 'reasoning'): Promise<void> {
  if (kind === 'reasoning') {
    const slot = row.getByTestId('thinking-toggle-icon-slot');
    const cue = slot.getByTestId('thinking-discarded');
    await expect(cue).toBeVisible();
    await expect(cue).toHaveAttribute('title', DISCARDED_LABEL);
    await expect(cue).toHaveAttribute('aria-label', DISCARDED_LABEL);
    await expect(slot.locator('[data-icon="brain"]')).toHaveCount(0);
    return;
  }
  await expect(row.getByTestId('assistant-message-discarded')).toHaveText('Discarded');
  await expect(row.getByTestId('assistant-message-discarded')).toBeVisible();
}

/** The thinking row lives in an activity run; open it so the row renders. */
async function expandThinkingRun(timeline: Locator): Promise<void> {
  const runs = timeline.getByTestId('activity-run');
  await expect(runs).toHaveCount(1);
  if ((await runs.getAttribute('data-collapsed')) === 'true') {
    await runs.getByTestId('activity-run-header').click();
    await expect(runs).toHaveAttribute('data-collapsed', 'false');
  }
}

interface Box { x: number; y: number; width: number; height: number }

/** The discarded row's box, its text's box and the box of the line after
 * its text (the time and tag footer of a text row, the time of a reasoning
 * header), in the scroller's content coordinates so a scroll of the
 * timeline does not read as a shift. */
interface RowGeometry { row: Box; text: Box; after: Box; lead: Box | null }

/**
 * Reads the row's geometry once it has held still for a run of frames, so
 * a reveal or a late paint still settling is not mistaken for the mark's
 * effect.
 */
async function settledGeometry(page: Page, itemId: string, kind: Case): Promise<RowGeometry> {
  const result = await page.evaluate(
    async ({ itemId, textSelector, afterSelector, leadSelector }) => {
      const scroller = document.querySelector<HTMLElement>('[data-testid="message-timeline-scroll"]');
      const row = document.querySelector<HTMLElement>(`[data-item-id="${CSS.escape(itemId)}"]`);
      if (!scroller || !row) return { error: `missing ${scroller ? 'row' : 'scroller'}` };
      const box = (el: Element | null): Box | null => {
        if (!el) return null;
        const r = el.getBoundingClientRect();
        const s = scroller.getBoundingClientRect();
        return { x: r.left - s.left, y: r.top - s.top + scroller.scrollTop, width: r.width, height: r.height };
      };
      const read = (): RowGeometry | null => {
        const text = box(row.querySelector(textSelector));
        const after = box(row.querySelector(afterSelector));
        const lead = leadSelector ? box(row.querySelector(leadSelector)) : null;
        if (leadSelector && !lead) return null;
        return text && after ? { row: box(row)!, text, after, lead } : null;
      };
      const frame = () => new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
      const stableFrames = 20;
      let last: RowGeometry | null = null;
      let same = 0;
      for (let i = 0; i < 600; i++) {
        await frame();
        const next = read();
        if (next && last && JSON.stringify(next) === JSON.stringify(last)) {
          if (++same >= stableFrames) return { geometry: next };
        } else {
          same = 0;
        }
        last = next;
      }
      return { error: `row geometry did not settle: ${JSON.stringify(last)}` };
    },
    kind === 'thinking'
      ? {
          itemId,
          textSelector: '[data-testid="thinking-body"]',
          afterSelector: '[data-testid="thinking-header"] time',
          // The kind icon's box, where the discard cue takes its place.
          leadSelector: '[data-testid="thinking-toggle-icon-slot"]',
        }
      : { itemId, textSelector: '[data-testid="assistant-message-body"]', afterSelector: '[data-testid="assistant-message-meta"]', leadSelector: '' },
  );
  if ('error' in result) throw new Error(result.error);
  return result.geometry;
}

async function expectMarked(timeline: Locator, kind: Case, theme: Theme): Promise<void> {
  const want = EXPECTED[kind];
  if (kind === 'thinking') {
    // A thinking row is an activity run of its own, collapsed by default
    // after a reload; the record must stand outside it, so the run holds
    // the one row and expanding it shows the marked reasoning.
    await expect(timeline.getByTestId('activity-run-header-counts')).toHaveText(/^1 thinking$/);
    await expandThinkingRun(timeline);
  }
  const discarded = timeline.locator('[data-item-id][data-discarded="true"]');
  await expect(discarded).toHaveCount(1);
  await expect(discarded).toHaveAttribute('data-item-kind', kind === 'thinking' ? 'thinking' : 'assistant_text');
  await expect(discarded).toHaveAttribute('data-item-status', 'completed');
  await expect(discarded).toContainText(want.discardedText);
  await expectDiscardedCue(discarded, kind === 'thinking' ? 'reasoning' : 'text');
  const dimmed = discarded.locator(kind === 'thinking' ? '.discarded-reasoning' : '.discarded-row');
  await expect(dimmed).toHaveCSS('opacity', OPACITY[theme][kind === 'thinking' ? 'reasoning' : 'text']);

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
      const mockId = await startMock(harness, threadId);

      const atRecord = waitForGate(harness, GATE_RECORD);
      const completed = harness.waitForEvent('provider:turn_completed');
      await harness.rpc('SendMessage', threadId, 'say something long', null);
      await atRecord;

      // The row as the user read it: settled, all of it revealed, unmarked.
      if (kind === 'thinking') await expandThinkingRun(timeline);
      const row = timeline.locator(`[data-item-id][data-item-kind="${kind === 'thinking' ? 'thinking' : 'assistant_text'}"]`, {
        hasText: EXPECTED[kind].discardedText,
      });
      await expect(row).toHaveCount(1);
      await expect(row).toHaveAttribute('data-item-status', 'completed');
      await expect(row).toContainText(kind === 'thinking' ? THINKING.trim().slice(-30) : 'if len(evt.Meta) == 0 {', { timeout: 20_000 });
      await expect(row).not.toHaveAttribute('data-discarded', 'true');
      if (kind === 'thinking') await expect(row.getByTestId('thinking-toggle-icon-slot').locator('[data-icon="brain"]')).toHaveCount(1);
      const itemId = (await row.getAttribute('data-item-id'))!;
      const before = await settledGeometry(page, itemId, kind);

      // The record lands; the reply is still held.
      const atReply = waitForGate(harness, GATE_REPLY);
      await advance(harness, mockId, GATE_RECORD);
      await atReply;
      const marked = timeline.locator(`[data-item-id="${itemId}"]`);
      await expect(marked).toHaveAttribute('data-discarded', 'true');
      await expect(timeline.getByTestId('abandoned-blocks-notification')).toBeVisible();
      await expectDiscardedCue(marked, kind === 'thinking' ? 'reasoning' : 'text');
      await expect(marked.locator(kind === 'thinking' ? '.discarded-reasoning' : '.discarded-row'))
        .toHaveCSS('opacity', OPACITY[theme][kind === 'thinking' ? 'reasoning' : 'text']);
      const after = await settledGeometry(page, itemId, kind);
      // Marking moves nothing: the row keeps its box, its text keeps its
      // width and wrap, the line after the text (where the tag lands beside
      // the time) keeps its place and height, and a reasoning row's cue
      // fills the kind icon's box exactly.
      expect(after, 'row geometry unchanged by the mark').toEqual(before);

      await advance(harness, mockId, GATE_REPLY);
      await completed;

      await expectMarked(timeline, kind, theme);
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

      // A cold load serves the row marked.
      await page.reload();
      const reloaded = await openThread(page, title);
      await expectMarked(reloaded, kind, theme);
    });
  }
}

// The dim is solved for the theme on screen, not fixed for the default
// palette: under Gruvbox dark the default's strength would put text-row prose
// under the floor (theme/discardedDim.browser.test.ts measures every
// built-in). Seeded, so the row is served marked.
test('the dim is solved for a non-default theme', async ({ harness, page }) => {
  const TEXT_ROW = 'gruvbox-cut-text';
  const THINK_ROW = 'gruvbox-cut-thinking';
  const RECORD = 'notification:0:abandoned:msg_gruvbox';
  await harness.rpc('SetAppearance', { mode: 'dark', uiTheme: 'gruvbox', codeTheme: 'github' });
  try {
    await harness.rpc('HarnessSeed', {
      projects: [{ name: 'abandoned-gruvbox', repo: {}, threads: [{ title: 'Abandoned under Gruvbox', provider: 'claude', turns: [
        { userText: 'say something long', items: [
          { id: THINK_ROW, kind: 'thinking', summary: THINKING, meta: { api_message_id: 'msg_gruvbox', block_index: 0 } },
          { id: TEXT_ROW, kind: 'assistant_text', summary: PARTIAL_HEAD, meta: { api_message_id: 'msg_gruvbox', block_index: 1 } },
          {
            id: RECORD, kind: 'notification', role: 'system', toolName: 'abandoned_blocks',
            summary: 'Discarded by Claude after a stalled response',
            meta: { kind: 'abandoned_blocks', title: 'Discarded by Claude after a stalled response', api_message_id: 'msg_gruvbox', from_block_index: 0, item_ids: [THINK_ROW, TEXT_ROW] },
          },
          { kind: 'assistant_text', summary: RETRY },
        ] },
      ] }] }],
    });
    await page.setViewportSize({ width: 1280, height: 1000 });
    await harness.open(page);
    await page.getByTestId('thread-row').getByText('Abandoned under Gruvbox', { exact: true }).click();
    const timeline = page.getByTestId('message-timeline-scroll');
    await expect(timeline.getByText(RETRY.slice(0, 40))).toBeVisible();
    await expandThinkingRun(timeline);
    const text = timeline.locator(`[data-item-id="${TEXT_ROW}"]`);
    const thinking = timeline.locator(`[data-item-id="${THINK_ROW}"]`);
    await expect(text).toHaveAttribute('data-discarded', 'true');
    await expect(thinking).toHaveAttribute('data-discarded', 'true');
    await expectDiscardedCue(thinking, 'reasoning');
    // Gruvbox dark's fg-muted on its ground: prose 9.66:1 undimmed, 4.58:1 at
    // 0.62 and under the floor at the default's 0.61; reasoning 5.43:1,
    // 4.52:1 at 0.88.
    await expect(text.locator('.discarded-row')).toHaveCSS('opacity', '0.62');
    await expect(thinking.locator('.discarded-reasoning')).toHaveCSS('opacity', '0.88');
  } finally {
    await harness.rpc('SetAppearance', { mode: 'system', uiTheme: 'default', codeTheme: 'github' });
  }
});
