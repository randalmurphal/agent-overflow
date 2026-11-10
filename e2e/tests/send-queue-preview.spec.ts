// The send-queue preview under a pile of pending messages, and what it
// costs the timeline behind it.
//
// The preview sits in the composer overlay, and the overlay's height is the
// clearance the timeline pads its last row with. So the preview is bounded
// (the newest four messages and a count of the rest), a resize of it does
// not restyle the mounted timeline rows, and a thread opened after its
// queued messages were delivered shows none of them.
//
// Only a browser can show these: the bound is a rendered height against the
// viewport, the restyle scope is the computed value of an inherited custom
// property, and the delivered messages would show for the frames between a
// pane's mount and its live-state read.
import type { HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  claudeScenario,
  emit,
  seedAgentThread,
  startMock,
  textLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';

// A tool held at a gate, then the held input consumed into the turn.
function heldTurnScenario(name: string): unknown {
  return {
    ...(claudeScenario(name, [
      emit([
        ...textLines('msg-working', 'Waiting on the tool.'),
        toolUseLine('msg-tool', 'tu-wait', 'Bash', { command: 'sleep 60' }),
      ]),
      { waitSignal: { name: 'hold' } },
      emit([toolResultLine('tu-wait', '')]),
      { drainQueuedInput: true },
      emit([...textLines('msg-final', 'Done.'), RESULT_LINE]),
    ]) as object),
    claude: { holdQueuedInput: true },
  };
}

// The mock holds every message the backend has sent the thread: each
// flushed row has the provider's `queued` ack. Released earlier, the gate's
// drain would miss a message still on its way to the provider.
async function mockHoldsEveryFlushedMessage(harness: HarnessApp, threadId: string, count: number): Promise<void> {
  await expect
    .poll(async () => {
      const live = await harness.rpc<{ queueItems: unknown[]; flushedItems: Array<{ userItemId: string }> }>(
        'GetThreadLiveState',
        threadId,
      );
      if (live.queueItems.length > 0 || live.flushedItems.length !== count) return false;
      return live.flushedItems.every(
        (item) =>
          harness.countEvents(
            'provider:command_lifecycle',
            (ev: any) => ev.threadId === threadId && ev.state === 'queued' && ev.userItemId === item.userItemId,
          ) > 0,
      );
    })
    .toBe(true);
}

const message = (n: number) =>
  `Agent answer ${n}: ` + 'the agent reports what it changed, what it checked and what is left. '.repeat(8);

test('a pile of pending messages shows the newest four and leaves the timeline in view', async ({
  harness,
  page,
}) => {
  test.setTimeout(90_000);

  await harness.rpc('HarnessSetScenario', { scenario: heldTurnScenario('queue-pile') });
  const threadId = await seedAgentThread(harness, 'queue-pile', 'Queue pile');
  await harness.open(page);
  await page.getByText('Queue pile', { exact: true }).click();
  const mockId = await startMock(harness, threadId);
  const input = page.getByLabel('Message Input');
  await input.fill('Coordinate the agents.');
  await input.press('Enter');
  await waitForGate(harness, 'hold');

  const rows = page.getByTestId('send-queue-preview-row');
  const earlier = page.getByTestId('send-queue-preview-earlier');
  const scroller = page.getByTestId('message-timeline-scroll');
  const overlay = page.getByTestId('composer-overlay');

  // Four fit without a count.
  for (let n = 1; n <= 4; n++) await harness.rpc('RegisterQueueItem', threadId, message(n), {});
  await expect(rows).toHaveCount(4);
  await expect(earlier).toHaveCount(0);
  const heightAtFour = (await overlay.boundingBox())!.height;

  // Eight more: still four rows, the newest, and the rest counted.
  for (let n = 5; n <= 12; n++) await harness.rpc('RegisterQueueItem', threadId, message(n), {});
  await expect(earlier).toHaveText('8 earlier messages waiting');
  await expect(rows).toHaveCount(4);
  await expect(rows.first()).toContainText('Agent answer 9:');
  await expect(rows.last()).toContainText('Agent answer 12:');
  // The backend flushed all twelve and the mock holds each.
  await mockHoldsEveryFlushedMessage(harness, threadId, 12);

  // The overlay grew by the count line only, and the turn's last row is
  // still on screen above it.
  const overlayBox = (await overlay.boundingBox())!;
  expect(overlayBox.height - heightAtFour).toBeLessThan(32);
  const working = page.getByText('Waiting on the tool.', { exact: true });
  await expect(working).toBeInViewport({ ratio: 1 });
  const workingBox = (await working.boundingBox())!;
  expect(workingBox.y + workingBox.height).toBeLessThanOrEqual(overlayBox.y);

  // The clearance still follows the overlay, and the rows do not inherit it.
  await expect
    .poll(async () => await scroller.evaluate((el) => getComputedStyle(el).getPropertyValue('--composer-height')))
    .toBe(`${Math.round(overlayBox.height)}px`);
  expect(await scroller.evaluate((el) => parseFloat(getComputedStyle(el).paddingBottom))).toBeGreaterThanOrEqual(
    Math.round(overlayBox.height),
  );
  expect(
    await page
      .getByTestId('user-message-bubble')
      .filter({ hasText: 'Coordinate the agents.' })
      .evaluate((el) => getComputedStyle(el).getPropertyValue('--composer-height')),
  ).toBe('0px');

  // The turn consumes all twelve.
  await advance(harness, mockId, 'hold');
  await harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await expect(rows).toHaveCount(0);
  await expect(earlier).toHaveCount(0);
  await expect(page.getByTestId('user-message-bubble').filter({ hasText: 'Agent answer 12:' })).toHaveCount(1);
});

test('a thread whose queued messages were delivered while it had no pane shows none on open', async ({
  harness,
  page,
}) => {
  test.setTimeout(90_000);

  await harness.rpc('HarnessSetScenario', { scenario: heldTurnScenario('queue-delivered-unwatched') });
  const workerId = await seedAgentThread(harness, 'queue-delivered-unwatched', 'Queue worker');
  await seedAgentThread(harness, 'queue-delivered-lead', 'Queue lead');
  await harness.open(page);
  await page.getByText('Queue lead', { exact: true }).click();
  await expect(page.getByLabel('Message Input')).toBeVisible();

  // The worker runs, takes two queued messages and finishes, all with no
  // pane on it. This page sees its queue frames and none of its rows.
  const mockId = await startMock(harness, workerId);
  await harness.rpc('SendMessage', workerId, 'Start the work.', null);
  await waitForGate(harness, 'hold');
  await harness.rpc('RegisterQueueItem', workerId, 'Queued for the worker 1', {});
  await harness.rpc('RegisterQueueItem', workerId, 'Queued for the worker 2', {});
  await mockHoldsEveryFlushedMessage(harness, workerId, 2);
  // The page shows the worker running off its pending sends and its turn.
  const workerRow = page.getByTestId('thread-row').filter({ hasText: 'Queue worker' }).first();
  const running = workerRow.locator('[data-testid="thread-row-status-dot"][data-status="running"]');
  await expect(running).toHaveCount(1);
  await advance(harness, mockId, 'hold');
  await harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === workerId);
  await expect
    .poll(async () => (await harness.rpc<{ flushedItems: unknown[] }>('GetThreadLiveState', workerId)).flushedItems.length)
    .toBe(0);
  // The page has applied the settled answer: the row no longer reads running.
  await expect(running).toHaveCount(0);

  // Count preview rows on every frame across the open.
  await page.evaluate(() => {
    const w = window as unknown as { __aoPreviewRows: number[]; __aoPreviewStop: () => void };
    w.__aoPreviewRows = [];
    let running = true;
    const tick = () => {
      if (!running) return;
      w.__aoPreviewRows.push(document.querySelectorAll('[data-testid="send-queue-preview-row"]').length);
      requestAnimationFrame(tick);
    };
    w.__aoPreviewStop = () => {
      running = false;
    };
    requestAnimationFrame(tick);
  });
  const sampledFrames = () =>
    page.evaluate(() => (window as unknown as { __aoPreviewRows: number[] }).__aoPreviewRows.length);
  await expect.poll(sampledFrames).toBeGreaterThan(0);
  await workerRow.click();
  await expect(page.getByTestId('user-message-bubble').filter({ hasText: 'Queued for the worker 2' })).toBeVisible();
  await expect(page.getByText('Done.', { exact: true })).toBeVisible();
  // Keep sampling past the pane's live-state read.
  const framesAtOpen = await sampledFrames();
  await expect.poll(sampledFrames).toBeGreaterThan(framesAtOpen + 30);
  const samples = await page.evaluate(() => {
    const w = window as unknown as { __aoPreviewRows: number[]; __aoPreviewStop: () => void };
    w.__aoPreviewStop();
    return w.__aoPreviewRows;
  });
  expect(Math.max(...samples), 'delivered messages showed in the preview on open').toBe(0);
});
