// Cursor watermarks end to end: a page whose pane shows an idle thread keeps
// its `provider:item_event` cursor at the channel head while another thread
// streams, so the replay it asks for after a reconnect starts at the head.
//
// WHY THIS LEVEL. internal/transport/watermark_test.go covers the marks, the
// pump's ordering, the visibility gate and the wire shape, and
// wsClient.test.ts covers the client cursor. Only this level proves that the
// shipped backend's pump sends watermarks on its `WatermarkEvery` ticker and
// that the SPA's next replay frame carries the cursor they moved.
//
// Without watermarks the page's cursor stays at the last frame it was sent,
// below every frame withheld from it. Once those frames leave the ring (by
// age, `RingRetainFor`, or by eviction) the reconnect answers `gap:true` and
// the page reloads everything.
import { test, expect } from './fixtures.js';
import { RESULT_LINE, claudeScenario, emit, seedAgentThread, startMock, textLines } from './agent-visibility-helpers.js';
import { readWire, recordWire, watchedNow } from './transport-watch-helpers.js';

const ITEMS = 'provider:item_event';
// internal/transport WatermarkEvery.
const WATERMARK_EVERY_MS = 30_000;

test('a page watching an idle thread reconnects from the head of a channel another thread streamed on', async ({
  harness,
  page,
}) => {
  test.setTimeout(120_000);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('watermark-stream', [
      emit([...textLines('msg-stream', 'Streamed while no pane showed it.'), RESULT_LINE]),
    ]),
  });
  const idle = await seedAgentThread(harness, 'watermark-idle', 'Idle thread');
  const streaming = await seedAgentThread(harness, 'watermark-stream', 'Streaming thread');
  await recordWire(page);
  await harness.open(page);
  await page.getByText('Idle thread').click();
  await expect.poll(async () => watchedNow(await readWire(page))).toEqual([idle]);

  await startMock(harness, streaming);
  const settled = harness.waitForEvent('provider:turn_completed', undefined, 30_000);
  await harness.rpc('SendMessage', streaming, 'stream while unwatched', null);
  await settled;

  // The harness client states no watch set, so its newest item_event seq is
  // the head. The first tick after the stream's last frame carries it.
  let head = 0;
  await expect
    .poll(async () => {
      head = harness.lastSeq(ITEMS);
      const wire = await readWire(page);
      return wire.watermarks.some((mark) => mark.channel === ITEMS && mark.socket === 0 && mark.seq === head);
    }, { timeout: WATERMARK_EVERY_MS + 15_000, message: 'no watermark reached the head' })
    .toBe(true);

  // The streamed rows were withheld from this page, so no data frame or
  // baseline it was sent reaches the head: only the watermark did.
  const before = await readWire(page);
  const told = Math.max(
    before.hellos[0]?.[ITEMS] ?? 0,
    ...before.received.filter((event) => event.channel === ITEMS).map((event) => event.seq),
  );
  expect(told, 'the page was sent the streaming thread’s rows, so nothing was withheld').toBeLessThan(head);

  const socketsBefore = before.sockets.length;
  await page.evaluate(() => (window as unknown as { __aoWireSocket: WebSocket }).__aoWireSocket.close());
  await expect
    .poll(async () => {
      const wire = await readWire(page);
      if (wire.sockets.length <= socketsBefore || wire.hellos.length <= socketsBefore) return false;
      return wire.sent.slice(wire.sockets.at(-1)).some((frame) => frame.type === 'replay');
    }, { timeout: 15_000 })
    .toBe(true);
  const after = await readWire(page);
  const replay = after.sent.slice(after.sockets.at(-1)).find((frame) => frame.type === 'replay')!;
  const cursors = (JSON.parse(replay.text) as { lastSeqByChannel: Record<string, number> }).lastSeqByChannel;
  expect(cursors[ITEMS], 'the reconnect asked for replay from below the head').toBe(head);
  expect(after.hellos.at(-1)?.[ITEMS], 'the channel moved after the watermark').toBe(head);
});
