// A message queued mid-turn is on screen the whole time: in the composer's
// send-queue preview until its timeline row actually renders, and in the
// timeline afterwards. Exactly one of the two, at every sampled frame.
//
// The defect this pins (2026-09-16): the preview dropped the message the
// instant `provider:item_event` delivered its row, but the reveal gate
// withholds a row that lands behind a still-draining assistant smoother.
// The flush row lands at the turn tail, so with multi-second prose still
// revealing, the message was in NEITHER place for as long as the drain
// lasted — the user pressed Enter and watched their text disappear.
//
// Only a browser can show this: the gap is between a wire event and a
// mounted DOM row, and its width is the reveal animation's own duration.
// The sampler runs inside the page on every animation frame, so a
// one-frame regression is caught rather than smoothed over by polling.
import { expect, test } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  claudeScenario,
  emit,
  seedAgentThread,
  startMock,
  textLines,
  waitForGate,
} from './agent-visibility-helpers.js';

// ~1.6KB of prose. MAX_ADAPTIVE_CHARS_PER_SEC is 320, so the reveal gate
// holds the turn tail for roughly five seconds after this lands — the
// window the queued message used to vanish into.
const PROSE = Array.from(
  { length: 8 },
  (_, i) =>
    `Paragraph ${i + 1}: the reveal gate animates this prose at a bounded character rate, ` +
    'which is what keeps a burst of provider text from snapping onto the screen all at once. ' +
    'While it is still drawing, every later row of the turn is withheld behind the boundary.',
).join(' ');

const QUEUED = 'Queued mid-turn: also update the changelog.';

interface ZoneSample {
  preview: number;
  timeline: number;
}

test('a message queued mid-turn is in the preview or the timeline, never neither', async ({
  harness,
  page,
}) => {
  test.setTimeout(90_000);

  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('send-queue-handover', [
      emit(textLines('msg-prose', PROSE)),
      { waitSignal: { name: 'finish' } },
      emit([...textLines('msg-final', 'Done.'), RESULT_LINE]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'send-queue-handover', 'Send queue handover');
  await harness.open(page);
  await page.getByText('Send queue handover').click();
  const mockId = await startMock(harness, threadId);

  // Turn 1 starts from the composer so the pane is the one driving it.
  const input = page.getByLabel('Message Input');
  await input.fill('Write the long answer.');
  await page.getByRole('button', { name: 'Send message', exact: true }).click();
  await waitForGate(harness, 'finish');

  // The prose is revealing now: its first words are on screen and its last
  // ones are not, which is the state the queued row has to survive.
  await expect(page.getByText('Paragraph 1:', { exact: false })).toBeVisible();

  // Sample both homes of the message on every frame, from before the send
  // until the handover settles. Counting by text covers Zone 1 (queued),
  // Zone 2 (flushed) and the timeline row with one predicate, so "in two
  // places at once" fails the same assertion as "in none".
  await page.evaluate((message) => {
    const w = window as unknown as {
      __aoZoneSamples: ZoneSample[];
      __aoZoneStop: () => void;
    };
    w.__aoZoneSamples = [];
    let running = true;
    const count = (selector: string) =>
      Array.from(document.querySelectorAll(selector)).filter((el) =>
        (el.textContent ?? '').includes(message),
      ).length;
    const tick = () => {
      if (!running) return;
      w.__aoZoneSamples.push({
        preview: count('[data-testid="send-queue-preview-row"]'),
        timeline: count('[data-testid="user-message-bubble"]'),
      });
      requestAnimationFrame(tick);
    };
    w.__aoZoneStop = () => {
      running = false;
    };
    requestAnimationFrame(tick);
  }, QUEUED);

  // Mid-turn Enter routes through the composer's queue path
  // (RegisterQueueItem), and Claude's active turn makes the backend flush
  // it immediately: queue_flushed, then the row's item_event.
  await input.fill(QUEUED);
  await input.press('Enter');
  await harness.waitForEvent(
    'provider:queue_flushed',
    (ev: any) => ev.threadId === threadId,
  );

  // Wait for the handover to complete: the timeline has the row and the
  // preview has let it go.
  await expect
    .poll(
      async () =>
        await page.evaluate(() => {
          const samples = (window as unknown as { __aoZoneSamples: ZoneSample[] }).__aoZoneSamples;
          const last = samples[samples.length - 1];
          return last ? `${last.preview}/${last.timeline}` : 'none';
        }),
      { timeout: 30_000 },
    )
    .toBe('0/1');

  const samples = await page.evaluate(() => {
    const w = window as unknown as { __aoZoneSamples: ZoneSample[]; __aoZoneStop: () => void };
    w.__aoZoneStop();
    return w.__aoZoneSamples;
  });

  const first = samples.findIndex((s) => s.preview + s.timeline > 0);
  expect(first, 'the queued message never appeared anywhere').toBeGreaterThanOrEqual(0);
  const afterSight = samples.slice(first);
  const wrong = afterSight
    .map((s, i) => ({ ...s, frame: first + i }))
    .filter((s) => s.preview + s.timeline !== 1);
  expect(
    wrong.slice(0, 5),
    'once visible, the message is in exactly one home on every frame',
  ).toEqual([]);

  // The window the defect lived in has to have been exercised: the
  // preview held the message while the reveal gate still withheld its
  // timeline row. Without it this test would pass on a turn that revealed
  // instantly and prove nothing.
  const withheld = afterSight.filter((s) => s.preview === 1 && s.timeline === 0).length;
  expect(withheld, 'the reveal gate never withheld the row, so the gap was not exercised')
    .toBeGreaterThan(0);

  await advance(harness, mockId, 'finish');
  await harness.waitForEvent('provider:turn_completed');
  await expect(page.getByTestId('send-queue-preview-row')).toHaveCount(0);
  await expect(page.getByText(QUEUED, { exact: true })).toHaveCount(1);
});

// Hold only this fixture's mock process, so the queued stdin write succeeds
// without a replay echo. This models Claude waiting on a foreground tool and
// avoids the mock adapter's automatic immediate acknowledgement.
for (const loseDispatch of [false, true]) {
  test(`an unconsumed Claude message stays pending across navigation and reload${loseDispatch ? ' after a lost dispatch event' : ''}`, async ({ harness, page }) => {
    let droppedDispatches = 0;
    await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
      const server = socket.connectToServer();
      socket.onMessage(message => server.send(message));
      server.onMessage(message => {
        const frame = JSON.parse(String(message));
        const lose = (event: any) => {
          if (!loseDispatch || event.channel !== 'provider:queue_flushed') return event;
          droppedDispatches++;
          return { type: 'event', channel: event.channel, seq: event.seq, gap: true };
        };
        if (frame.type === 'event') socket.send(JSON.stringify(lose(frame)));
        else if (frame.type === 'batch') socket.send(JSON.stringify({ ...frame, events: frame.events.map(lose) }));
        else socket.send(message);
      });
    });
    await harness.rpc('HarnessSetScenario', {
      scenario: claudeScenario('pending-queue-return', [
        emit(textLines('working', 'Waiting for the foreground agent.')),
        { waitSignal: { name: 'finish' } },
        emit([RESULT_LINE]),
      ]),
    });
    const threadId = await seedAgentThread(harness, 'pending-queue-return', 'Pending queue return');
    await harness.open(page);
    await page.getByText('Pending queue return', { exact: true }).click();
    const mockId = await startMock(harness, threadId);
    const input = page.getByLabel('Message Input');
    await input.fill('Run the foreground agent.');
    await input.press('Enter');
    await waitForGate(harness, 'finish');

    const mocks = await harness.rpc<Array<{ mockId: string; registration: { pid: number } }>>('HarnessListMocks');
    const pid = mocks.find((mock) => mock.mockId === mockId)?.registration.pid;
    expect(pid).toBeGreaterThan(0);
    process.kill(pid!, 'SIGSTOP');
    try {
      const flushed = harness.waitForEvent('provider:queue_flushed', (ev: any) => ev.threadId === threadId);
      await input.fill(QUEUED);
      await input.press('Enter');
      await flushed;
      if (loseDispatch) await expect.poll(() => droppedDispatches).toBeGreaterThan(0);
      await expect.poll(async () => {
        const rows = await harness.rpc<Array<{ summary: string }>>('ListItems', threadId, false);
        return rows.some((row) => row.summary === QUEUED);
      }).toBe(true);
      const live = await harness.rpc<{ flushedItems: Array<{ message: string }> }>('GetThreadLiveState', threadId);
      expect(live.flushedItems.some((item) => item.message === QUEUED)).toBe(true);
      const preview = page.getByTestId('send-queue-preview-row').filter({ hasText: QUEUED });
      const bubble = page.getByTestId('user-message-bubble').filter({ hasText: QUEUED });
      await expect(preview).toBeVisible();
      await expect(bubble).toHaveCount(0);

      // A second thread gives the original pane a real detach/attach cycle.
      await seedAgentThread(harness, 'queue-other', 'Queue other thread');
      await page.getByText('Queue other thread', { exact: true }).click();
      await page.getByText('Pending queue return', { exact: true }).click();
      await expect(preview).toBeVisible();
      await expect(bubble).toHaveCount(0);

      await page.reload();
      await expect(preview).toBeVisible();
      await expect(bubble).toHaveCount(0);
    } finally {
      process.kill(pid!, 'SIGCONT');
    }
    await expect(page.getByTestId('user-message-bubble').filter({ hasText: QUEUED })).toBeVisible();
    await expect(page.getByTestId('send-queue-preview-row').filter({ hasText: QUEUED })).toHaveCount(0);
    await advance(harness, mockId, 'finish');
  });

}
