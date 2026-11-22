// A row Claude discarded stays marked when its record is not in the loaded
// window.
//
// The abandonment is its own notification row after the rows it names
// (docs/references/claude-wire.md §abandoned_blocks). A page edge can fall
// between them: a jump to the first message loads a fixed number of rows
// after it, paging older stops at a cursor, a window cut drops the newest
// end. The server attaches the record's id to each row it names when it
// serves the row (`Item.discardedBy`), so the mark never depends on the
// record being loaded; the window derives it from the record only for a
// live row the wire has not re-pushed yet (utils/discardedItems.ts).
//
// Two edges. The first is a cold load, for a text row and for a reasoning
// row: the thread puts the discarded rows exactly at a jump's newest edge
// (the slice around the first message ships 100 rows after it, the last
// discarded row is the 100th and the record the 101st). A record joins the
// activity run of discarded reasoning, and a page ships a run's newest
// members, so the reasoning case discards reasoning and then prose: the
// prose ends the reasoning's run and the record follows it. The
// second is the live path: the row streams, the record lands, the wire
// refresh re-pushes the row with `discardedBy`, and the window cut on the
// next older page (ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS kept from the head)
// drops the record while keeping the row. The re-push must replace the held
// copy (`itemsRenderEqual` compares `discardedBy`), or the mark leaves with
// the record. Both cases scroll to the window's newest edge, which asks for
// newer rows; the spec holds that response at the socket so the row is on
// screen with the record provably unloaded, then releases it and checks the
// mark survives the record's arrival.
//
// The live path also runs with an older page started while the newer one
// is held: each page claims only the edge it extends, so the newer page
// still lands when it is released, and auto-load at the newest edge keeps
// working after it (stores/threadTimelineWindow.svelte.ts, edge claims).
import type { Locator, Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import { test, expect, type SeedResult } from './fixtures.js';
import { RESULT_LINE, claudeScenario, emit, emitBurst, listItems, startMock, toolResultLine, toolUseLine } from './agent-visibility-helpers.js';
import { awaitTurnCompleted } from './thread-tools-helpers.js';

const LIST_ITEMS_BEFORE_CURSOR = 162135710;
const LIST_ITEMS_AFTER_CURSOR = 2915892537;
const RECORD_SUMMARY = 'Discarded by Claude after a stalled response';

const j = (value: unknown): string => JSON.stringify(value);

/**
 * Holds paging answers at the socket. Newer pages are held from the start;
 * older pages pass `olderPagesAllowed` times, then are held too, so a spec
 * that needs exactly one window cut gets exactly one. Counts the paging
 * requests and the item pushes that carried `discardedBy`: the wire refresh
 * after a record.
 */
interface PagingHold {
  olderRequests: number;
  newerRequests: number;
  olderResponses: number;
  markedPushes: number;
  heldNewer: number;
  releaseNewer: () => void;
}

async function holdPaging(page: Page, olderPagesAllowed: number): Promise<PagingHold> {
  const olderIds = new Set<string>();
  const newerIds = new Set<string>();
  const heldFrames: string[] = [];
  const hold: PagingHold = { olderRequests: 0, newerRequests: 0, olderResponses: 0, markedPushes: 0, heldNewer: 0, releaseNewer: () => {} };
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const server = socket.connectToServer();
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.methodId === LIST_ITEMS_BEFORE_CURSOR) {
        olderIds.add(String(frame.id));
        hold.olderRequests += 1;
      }
      if (frame.type === 'rpc' && frame.methodId === LIST_ITEMS_AFTER_CURSOR) {
        newerIds.add(String(frame.id));
        hold.newerRequests += 1;
      }
      server.send(message);
    });
    server.onMessage(message => {
      const text = String(message);
      if ((text.startsWith('{"type":"event"') || text.startsWith('{"type":"batch"')) && text.includes('"discardedBy":')) {
        hold.markedPushes += 1;
      }
      if (text.startsWith('{"type":"rpc"')) {
        const frame = JSON.parse(text);
        const id = String(frame.id);
        if (newerIds.has(id)) {
          hold.heldNewer += 1;
          heldFrames.push(text);
          return;
        }
        if (olderIds.has(id)) {
          hold.olderResponses += 1;
          if (hold.olderResponses > olderPagesAllowed) return;
        }
      }
      socket.send(message);
    });
    hold.releaseNewer = () => {
      newerIds.clear();
      for (const text of heldFrames.splice(0)) socket.send(text);
    };
  });
  return hold;
}

/** Scrolls to the window's newest edge until `row` is mounted and on screen. */
async function scrollToNewestEdge(scroller: Locator, row: Locator): Promise<void> {
  await expect.poll(async () => {
    await scroller.evaluate(el => { el.scrollTop = el.scrollHeight; });
    return row.count();
  }).toBe(1);
  await expect(row).toBeInViewport();
}

/** Scrolls to the window's newest edge until `row` is mounted, then reads it with the record absent. */
async function expectMarkedAtNewestEdge(scroller: Locator, row: Locator, record: Locator, hold: PagingHold, text: string): Promise<void> {
  await reachNewestEdge(scroller, row, record, hold, text);
  await releaseAndExpectMarked(scroller, row, record, hold);
}

async function reachNewestEdge(scroller: Locator, row: Locator, record: Locator, hold: PagingHold, text: string): Promise<void> {
  await scrollToNewestEdge(scroller, row);
  await expect(record).toHaveCount(0);
  await expect.poll(() => hold.heldNewer).toBeGreaterThan(0);

  await expect(row).toHaveAttribute('data-discarded', 'true');
  await expect(row.getByTestId('assistant-message-discarded')).toHaveText('Discarded');
  await expect(row).toContainText(text);
  await expect(record).toHaveCount(0);
}

async function releaseAndExpectMarked(scroller: Locator, row: Locator, record: Locator, hold: PagingHold): Promise<void> {
  // The record and what follows land below; the row keeps its mark.
  hold.releaseNewer();
  await expect(record).toHaveCount(1);
  await record.scrollIntoViewIfNeeded();
  await expect(record).toContainText(RECORD_SUMMARY);
  await expect(row).toHaveAttribute('data-discarded', 'true');
  await expect(scroller.locator('[data-item-id][data-discarded="true"]')).toHaveCount(1);
}

const answer = (text: string) => ({ kind: 'assistant_text', summary: text });

test('a discarded row served without its record is still marked', async ({ harness, page }) => {
  const CUT_TURN = 49;
  const API_MESSAGE_ID = 'msg_cut';
  const CUT_ROW = 'cut-row';
  const RECORD = `notification:${CUT_TURN}:abandoned:${API_MESSAGE_ID}`;
  const CUT_TEXT = 'The answer Claude streamed and then dropped from its transcript.';
  const RETRY_TEXT = 'The answer Claude gave after the retry.';
  const hold = await holdPaging(page, Number.POSITIVE_INFINITY);

  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'abandoned-paging', repo: {}, threads: [{ title: 'Cut at the edge', provider: 'claude', turns: [
      // Rows 0..2: the first message and two answers.
      { userText: 'First question', items: [answer('First answer.'), answer('First answer, continued.')] },
      // Rows 3..98.
      { userText: 'Earlier question', items: [answer('Earlier answer.')], repeat: 48 },
      // Rows 99..102: the question, the discarded row, the record, the retry.
      { userText: 'Cut question', items: [
        { id: CUT_ROW, kind: 'assistant_text', summary: CUT_TEXT, meta: { api_message_id: API_MESSAGE_ID, block_index: 0 } },
        {
          id: RECORD, kind: 'notification', role: 'system', toolName: 'abandoned_blocks',
          summary: RECORD_SUMMARY,
          meta: { kind: 'abandoned_blocks', title: RECORD_SUMMARY, api_message_id: API_MESSAGE_ID, from_block_index: 0, item_ids: [CUT_ROW] },
        },
        { kind: 'assistant_text', summary: RETRY_TEXT, meta: { api_message_id: 'msg_retry', block_index: 0 } },
      ] },
      // Rows 103..322: enough newer history that the tail window holds none of the above.
      { userText: 'Later question', items: [answer('Later answer.')], repeat: 110 },
    ] }] }],
  });

  await harness.open(page);
  await page.getByTestId('thread-row').getByText('Cut at the edge', { exact: true }).click();
  const scroller = page.getByTestId('message-timeline-scroll');
  await expect(scroller.getByText('Later answer.', { exact: true }).last()).toBeVisible();
  const cutRow = scroller.locator(`[data-item-id="${CUT_ROW}"]`);
  const record = scroller.locator(`[data-item-id="${RECORD}"]`);
  await expect(cutRow).toHaveCount(0);

  await page.getByTestId('message-nav-rail').getByRole('button', { name: 'Jump to first message', exact: true }).click();
  await expect(scroller.getByText('First question', { exact: true })).toBeInViewport();

  // The window's newest edge: the discarded row is its last row, and the
  // rows after it (the record first) are asked for and held.
  await expectMarkedAtNewestEdge(scroller, cutRow, record, hold, CUT_TEXT);
  await expect(scroller.getByText(RETRY_TEXT, { exact: true })).toBeVisible();
});

// The same cold-load edge with a reasoning row. Its only visible marks are
// the dim, which a theme can leave at nothing (reasoning text already at the
// contrast floor is not dimmed), and the scissors that take its kind icon's
// place; the record below is not loaded, so the cue has to carry it.
test('a discarded reasoning row served without its record still shows the cue', async ({ harness, page }) => {
  const CUT_TURN = 49;
  const API_MESSAGE_ID = 'msg_cut_thinking';
  const CUT_ROW = 'cut-thinking-row';
  const CUT_PROSE_ROW = 'cut-thinking-prose-row';
  const RECORD = `notification:${CUT_TURN}:abandoned:${API_MESSAGE_ID}`;
  const CUT_TEXT = 'Reasoning Claude streamed and then dropped from its transcript.';
  const RETRY_TEXT = 'The answer Claude gave after the retry.';
  const LABEL = 'Discarded by Claude after the response stalled';
  const hold = await holdPaging(page, Number.POSITIVE_INFINITY);

  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'abandoned-paging-thinking', repo: {}, threads: [{ title: 'Reasoning cut at the edge', provider: 'claude', turns: [
      // Rows 0..1: the first message and its answer.
      { userText: 'First question', items: [answer('First answer.')] },
      // Rows 2..97.
      { userText: 'Earlier question', items: [answer('Earlier answer.')], repeat: 48 },
      // Rows 98..102: the question, the discarded reasoning and prose, the
      // record, the retry.
      { userText: 'Cut question', items: [
        { id: CUT_ROW, kind: 'thinking', summary: CUT_TEXT, meta: { api_message_id: API_MESSAGE_ID, block_index: 0 } },
        { id: CUT_PROSE_ROW, kind: 'assistant_text', summary: 'Prose Claude streamed after it and dropped too.', meta: { api_message_id: API_MESSAGE_ID, block_index: 1 } },
        {
          id: RECORD, kind: 'notification', role: 'system', toolName: 'abandoned_blocks',
          summary: RECORD_SUMMARY,
          meta: { kind: 'abandoned_blocks', title: RECORD_SUMMARY, api_message_id: API_MESSAGE_ID, from_block_index: 0, item_ids: [CUT_ROW, CUT_PROSE_ROW] },
        },
        { kind: 'assistant_text', summary: RETRY_TEXT, meta: { api_message_id: 'msg_retry', block_index: 0 } },
      ] },
      { userText: 'Later question', items: [answer('Later answer.')], repeat: 110 },
    ] }] }],
  });

  // Light mode: the default theme leaves this reasoning undimmed there.
  await page.emulateMedia({ colorScheme: 'light' });
  await harness.open(page);
  await page.getByTestId('thread-row').getByText('Reasoning cut at the edge', { exact: true }).click();
  const scroller = page.getByTestId('message-timeline-scroll');
  await expect(scroller.getByText('Later answer.', { exact: true }).last()).toBeVisible();
  const record = scroller.locator(`[data-item-id="${RECORD}"]`);

  await page.getByTestId('message-nav-rail').getByRole('button', { name: 'Jump to first message', exact: true }).click();
  await expect(scroller.getByText('First question', { exact: true })).toBeInViewport();

  // The window's newest edge holds the reasoning's run; the record is held.
  const run = scroller.getByTestId('activity-run');
  await expect.poll(async () => {
    await scroller.evaluate(el => { el.scrollTop = el.scrollHeight; });
    return run.count();
  }).toBe(1);
  if ((await run.getAttribute('data-collapsed')) === 'true') {
    await run.getByTestId('activity-run-header').click();
    await expect(run).toHaveAttribute('data-collapsed', 'false');
  }
  const row = scroller.locator(`[data-item-id="${CUT_ROW}"]`);
  await row.scrollIntoViewIfNeeded();
  await expect(row).toBeInViewport();
  await expect.poll(() => hold.heldNewer).toBeGreaterThan(0);
  await expect(record).toHaveCount(0);

  await expect(row).toHaveAttribute('data-discarded', 'true');
  await expect(row.locator('.discarded-reasoning')).toHaveCSS('opacity', '1');
  const slot = row.getByTestId('thinking-toggle-icon-slot');
  const cue = slot.getByTestId('thinking-discarded');
  await expect(cue).toBeVisible();
  await expect(cue).toHaveAttribute('title', LABEL);
  await expect(row.getByTestId('thinking-toggle')).toHaveAccessibleName('Toggle Thinking Block, discarded by Claude after the response stalled');
  await expect(slot.locator('[data-icon="brain"]')).toHaveCount(0);
  await expect(record).toHaveCount(0);

  hold.releaseNewer();
  await expect(record).toHaveCount(1);
  await expect(row).toHaveAttribute('data-discarded', 'true');
  await expect(cue).toBeVisible();
});

// The live path. 210 seeded exchanges (420 rows) and a live turn of 814
// rows: 398 tool calls, the streamed block the CLI abandons (row 399 of the
// turn), its record (row 400), the retry, 398 more tool calls in runs of 30
// with a line of prose between runs, and the closing answer. Tool calls
// before the cut, not prose, because prose rows reveal at a reading pace
// and a run collapses to a header. The tail window holds the newest 200
// seeded rows plus the turn (1014 rows); the first older page makes 1214,
// over ACTIVE_TIMELINE_WINDOW_MAX_ITEMS, and the cut keeps the oldest
// ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS (800): the 400 paged-in rows and the
// turn through the discarded row. The record is the first row dropped;
// neither cut edge lands inside a run, so nothing snaps. The rows after it
// take more than one newer page.
//
// The page shows the turn's end only after the reveal gate has released its
// prose one row at a time (stores/threadRevealGate.svelte.ts): the retry,
// 13 notes and the closing answer, each with the run of tool calls behind
// it. That sequence grows with the turn and with how slowly the page
// paints, so the spec waits for the turn to complete and then for each
// prose row in order, each step under the default timeout.
const LIVE_CUT_TEXT = 'The live answer Claude streamed before the stall and then dropped.';
const LIVE_RETRY_TEXT = 'The live answer Claude gave after the retry.';
const LIVE_LAST_TEXT = 'The live turn ends here.';
/** Where each run of tool calls after the retry starts; a note follows every run but the last. */
const LIVE_RUN_STARTS = Array.from({ length: Math.ceil(398 / 30) }, (_, i) => 398 + i * 30);
const liveNote = (runStart: number): string => `Checked ${runStart + 30}.`;
const LIVE_NOTES = LIVE_RUN_STARTS.filter(from => from + 30 < 796).map(liveNote);

/**
 * Waits for the reveal gate to release `texts`, the turn's prose in
 * timeline order, through the last one. Each step, one prose row and the
 * run behind it, gets the default timeout; a gate that stops moving fails
 * naming the last row it released.
 */
async function expectRevealedInOrder(scroller: Locator, texts: string[]): Promise<void> {
  const furthest = async (): Promise<number> => {
    for (let index = texts.length - 1; index >= 0; index--) {
      if ((await scroller.getByText(texts[index], { exact: true }).count()) > 0) return index;
    }
    return -1;
  };
  for (let reached = -1; reached < texts.length - 1; reached = await furthest()) {
    await expect.poll(furthest, { message: `the reveal stopped after "${texts[reached] ?? 'the turn start'}"` }).toBeGreaterThan(reached);
  }
}

/** Streams the live turn, then pages older once so the cut drops the record; returns at the window's top. */
async function openLiveCut(harness: HarnessApp, page: Page, hold: PagingHold): Promise<{ scroller: Locator; cutRow: Locator; record: Locator }> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'abandoned-cut', repo: {}, threads: [{ title: 'Cut live', provider: 'claude', turns: [
      { userText: 'Earlier question', items: [answer('Earlier answer.')], repeat: 210 },
    ] }] }],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.rpc('HarnessSetScenario', { scenario: liveScenario(LIVE_CUT_TEXT, LIVE_RETRY_TEXT, LIVE_LAST_TEXT) });

  await harness.open(page);
  await page.getByTestId('thread-row').getByText('Cut live', { exact: true }).click();
  const scroller = page.getByTestId('message-timeline-scroll');
  await expect(scroller.getByText('Earlier answer.', { exact: true }).last()).toBeVisible();

  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'Cut question', null);
  await awaitTurnCompleted(harness, threadId);
  await expectRevealedInOrder(scroller, [LIVE_RETRY_TEXT, ...LIVE_NOTES, LIVE_LAST_TEXT]);
  await expect(scroller.getByText(LIVE_LAST_TEXT, { exact: true })).toBeVisible();
  await expect.poll(async () => (await listItems(harness, threadId)).filter(item => item.kind === 'tool_call' && item.status === 'completed').length).toBe(796);
  const records = (await listItems(harness, threadId)).filter(item => item.kind === 'notification' && item.toolName === 'abandoned_blocks');
  expect(records).toHaveLength(1);
  // The refresh after the record pushed the row marked; the window must
  // have taken that push, not kept the copy it held.
  await expect.poll(() => hold.markedPushes).toBeGreaterThan(0);

  // One older page: the window passes its cap and the cut drops the newest
  // end, the record first.
  await expect.poll(async () => {
    await scroller.evaluate(el => { el.scrollTop = 0; });
    return hold.olderResponses;
  }).toBeGreaterThan(0);

  const cutRow = scroller.locator('[data-item-id][data-item-kind="assistant_text"]', { hasText: LIVE_CUT_TEXT });
  const record = scroller.getByTestId('abandoned-blocks-notification');
  return { scroller, cutRow, record };
}

test('a live discarded row keeps its mark after the window cut drops its record', async ({ harness, page }) => {
  const hold = await holdPaging(page, 1);
  const { scroller, cutRow, record } = await openLiveCut(harness, page, hold);
  await expectMarkedAtNewestEdge(scroller, cutRow, record, hold, LIVE_CUT_TEXT);
  await expect(scroller.getByText(LIVE_RETRY_TEXT, { exact: true })).toBeVisible();
});

// The order CI once reached by chance: while the newer page is out, the
// reader reaches the top and an older page starts (held here for good).
// The newer page extends the other edge, so it still lands on release, and
// auto-load at the newest edge keeps firing after it.
test('a newer page held while an older page starts still lands, and paging newer continues', async ({ harness, page }) => {
  const hold = await holdPaging(page, 1);
  const { scroller, cutRow, record } = await openLiveCut(harness, page, hold);
  await reachNewestEdge(scroller, cutRow, record, hold, LIVE_CUT_TEXT);

  // Two offsets inside the top zone, so every write is a scroll event; the
  // older gate re-arms 350ms after the first page.
  const olderBefore = hold.olderRequests;
  await expect.poll(async () => {
    await scroller.evaluate(el => { el.scrollTop = el.scrollTop === 0 ? 200 : 0; });
    return hold.olderRequests;
  }).toBeGreaterThan(olderBefore);
  await scrollToNewestEdge(scroller, cutRow);
  await expect(record).toHaveCount(0);

  await releaseAndExpectMarked(scroller, cutRow, record, hold);
  await expect(scroller.getByText(LIVE_RETRY_TEXT, { exact: true })).toBeVisible();

  // The window's newest edge asks for the next page.
  const newerBefore = hold.newerRequests;
  await expect.poll(async () => {
    await scroller.evaluate(el => {
      const bottom = el.scrollHeight - el.clientHeight;
      el.scrollTop = el.scrollTop >= bottom - 1 ? bottom - 50 : bottom;
    });
    return hold.newerRequests;
  }).toBeGreaterThan(newerBefore);
});

/** The live turn, in the 2.1.300 envelope: `api_message_id` beside the inner event. */
function liveScenario(cutText: string, retryText: string, lastText: string): unknown {
  const stream = (apiMessageId: string, event: Record<string, unknown>, extra: Record<string, unknown> = {}): string =>
    j({ type: 'stream_event', event, session_id: 's', parent_tool_use_id: null, api_message_id: apiMessageId, ...extra });
  const snapshot = (id: string, text: string, stopReason: string | null = null): string =>
    j({ type: 'assistant', message: { id, role: 'assistant', model: 'claude-mock-1', content: [{ type: 'text', text }], stop_reason: stopReason }, parent_tool_use_id: null });
  const fillers = (from: number, count: number): string[] =>
    Array.from({ length: count }, (_, i) => [
      toolUseLine(`msg_fill_${from + i}`, `tool_fill_${from + i}`, 'Bash', { command: `echo live-fill-${from + i}` }),
      toolResultLine(`tool_fill_${from + i}`, 'done'),
    ]).flat();
  const msgCut = 'msg_live_cut';
  const chunk = (text: string): string[] => text.match(/[\s\S]{1,24}/g) ?? [];
  // After the retry, 398 more tool calls in runs of 30, a line of prose
  // between runs: a newer page ships at most 30 members of a run, so runs
  // this short ship whole and the newer side spans more than one page.
  const trailing: string[] = [];
  for (const from of LIVE_RUN_STARTS) {
    trailing.push(...fillers(from, Math.min(30, 796 - from)));
    if (from + 30 < 796) trailing.push(snapshot(`msg_live_note_${from}`, liveNote(from)));
  }
  return claudeScenario('claude-abandoned-live-cut', [
    emitBurst(fillers(0, 398)),
    emit([
      stream(msgCut, { type: 'message_start', message: { id: msgCut, role: 'assistant', model: 'claude-mock-1', content: [], stop_reason: null, usage: { input_tokens: 10, output_tokens: 1 } } }),
      stream(msgCut, { type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } }),
      ...chunk(cutText).map(text => stream(msgCut, { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text } })),
      // The CLI's own close of the response it stopped reading.
      stream(msgCut, { type: 'content_block_stop', index: 0 }),
      stream(msgCut, { type: 'message_stop' }, { abandoned_blocks: { api_message_id: msgCut, from_block_index: 0 } }),
      j({ type: 'user', message: { role: 'user', content: [{ type: 'text', text: 'Your response above was cut off mid-stream. Resume directly from where it stops.' }] }, parent_tool_use_id: null, isSynthetic: true }),
      snapshot('msg_live_retry', retryText),
    ]),
    emitBurst([...trailing, snapshot('msg_live_last', lastText, 'end_turn'), RESULT_LINE]),
  ]);
}
