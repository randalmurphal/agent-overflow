// A row Claude discarded stays marked when its record is not in the loaded
// window.
//
// The abandonment is its own notification row after the rows it names
// (docs/references/claude-wire.md §abandoned_blocks). A page edge can fall
// between them: a jump to the first message loads a fixed number of rows
// after it, paging older stops at a cursor, trimming drops the newest end.
// The server attaches the record's id to each row it names when it serves
// the row (`Item.discardedBy`), so the mark never depends on the record
// being loaded; the window derives it from the record only for a live row
// the wire has not re-pushed yet (utils/discardedItems.ts).
//
// The thread puts the discarded row exactly at the jump's newest edge: the
// slice around the first message ships 100 rows after it (half of
// SLICE_AROUND_ITEM_BUDGET), the discarded row is the 100th and the record
// the 101st. Scrolling to that edge asks for newer rows; the spec holds
// that response at the socket, so the row is on screen with the record
// provably unloaded, then releases it and checks the mark survives the
// record's arrival.
import { test, expect, type SeedResult } from './fixtures.js';

const LIST_ITEMS_AFTER_CURSOR = 2915892537;
const CUT_TURN = 49;
const API_MESSAGE_ID = 'msg_cut';
const CUT_ROW = 'cut-row';
const RECORD = `notification:${CUT_TURN}:abandoned:${API_MESSAGE_ID}`;
const CUT_TEXT = 'The answer Claude streamed and then dropped from its transcript.';
const RETRY_TEXT = 'The answer Claude gave after the retry.';

test('a discarded row served without its record is still marked', async ({ harness, page }) => {
  let holdNewer = false;
  const heldIds = new Set<string>();
  const heldFrames: string[] = [];
  let release: () => void = () => {};
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const server = socket.connectToServer();
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (holdNewer && frame.type === 'rpc' && frame.methodId === LIST_ITEMS_AFTER_CURSOR) heldIds.add(String(frame.id));
      server.send(message);
    });
    server.onMessage(message => {
      const text = String(message);
      if (heldIds.size > 0 && text.startsWith('{"type":"rpc"')) {
        const frame = JSON.parse(text);
        if (heldIds.has(String(frame.id))) {
          heldFrames.push(text);
          return;
        }
      }
      socket.send(message);
    });
    release = () => {
      holdNewer = false;
      heldIds.clear();
      for (const text of heldFrames.splice(0)) socket.send(text);
    };
  });

  const answer = (text: string) => ({ kind: 'assistant_text', summary: text });
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
          summary: 'Discarded by Claude after a stalled response',
          meta: { kind: 'abandoned_blocks', title: 'Discarded by Claude after a stalled response', api_message_id: API_MESSAGE_ID, from_block_index: 0, item_ids: [CUT_ROW] },
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

  holdNewer = true;
  await page.getByTestId('message-nav-rail').getByRole('button', { name: 'Jump to first message', exact: true }).click();
  await expect(scroller.getByText('First question', { exact: true })).toBeInViewport();

  // The window's newest edge: the discarded row is its last row, and the
  // rows after it (the record first) are asked for and held.
  await expect.poll(async () => {
    await scroller.evaluate(el => { el.scrollTop = el.scrollHeight; });
    return cutRow.count();
  }).toBe(1);
  await expect(cutRow).toBeInViewport();
  await expect(record).toHaveCount(0);
  await expect.poll(() => heldIds.size).toBeGreaterThan(0);

  await expect(cutRow).toHaveAttribute('data-discarded', 'true');
  await expect(cutRow.getByTestId('assistant-message-discarded')).toHaveText('Discarded');
  await expect(cutRow).toContainText(CUT_TEXT);
  await expect(record).toHaveCount(0);

  // The record and the retry land below; the row keeps its mark.
  release();
  await expect(record).toHaveCount(1);
  await record.scrollIntoViewIfNeeded();
  await expect(record).toContainText('Discarded by Claude after a stalled response');
  await expect(scroller.getByText(RETRY_TEXT, { exact: true })).toBeVisible();
  await expect(cutRow).toHaveAttribute('data-discarded', 'true');
  await expect(scroller.locator('[data-item-id][data-discarded="true"]')).toHaveCount(1);
});
