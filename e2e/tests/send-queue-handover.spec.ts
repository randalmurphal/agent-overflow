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
import type { Page } from '@playwright/test';
import { expect, test } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  claudeScenario,
  claudeTurnsScenario,
  emit,
  seedAgentThread,
  startMock,
  textLines,
  toolResultLine,
  toolUseLine,
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

interface ZoneSamplerWindow {
  __aoZoneSamples: ZoneSample[];
  __aoZoneStop: () => void;
}

// Sample both homes of the message on every frame. Counting by text covers
// Zone 1 (queued), Zone 2 (flushed) and the timeline row with one
// predicate, so "in two places at once" fails the same assertion as "in
// none".
async function startZoneSampler(page: Page, message: string): Promise<void> {
  await page.evaluate((text) => {
    const w = window as unknown as ZoneSamplerWindow;
    w.__aoZoneSamples = [];
    let running = true;
    const count = (selector: string) =>
      Array.from(document.querySelectorAll(selector)).filter((el) =>
        (el.textContent ?? '').includes(text),
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
  }, message);
}

function lastZoneSample(page: Page): () => Promise<string> {
  return () =>
    page.evaluate(() => {
      const samples = (window as unknown as ZoneSamplerWindow).__aoZoneSamples;
      const last = samples[samples.length - 1];
      return last ? `${last.preview}/${last.timeline}` : 'none';
    });
}

async function stopZoneSampler(page: Page): Promise<ZoneSample[]> {
  return await page.evaluate(() => {
    const w = window as unknown as ZoneSamplerWindow;
    w.__aoZoneStop();
    return w.__aoZoneSamples;
  });
}

// Once visible, the message is in exactly one home on every frame, and the
// frames where the preview held it while the timeline did not are the window
// the defects lived in: without them the run proved nothing.
function expectOneHomePerFrame(samples: ZoneSample[]): void {
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
  const withheld = afterSight.filter((s) => s.preview === 1 && s.timeline === 0).length;
  expect(withheld, 'the preview never held the row while the timeline lacked it, so the gap was not exercised')
    .toBeGreaterThan(0);
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

  await startZoneSampler(page, QUEUED);

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
  await expect.poll(lastZoneSample(page), { timeout: 30_000 }).toBe('0/1');
  expectOneHomePerFrame(await stopZoneSampler(page));

  await advance(harness, mockId, 'finish');
  await harness.waitForEvent('provider:turn_completed');
  await expect(page.getByTestId('send-queue-preview-row')).toHaveCount(0);
  await expect(page.getByText(QUEUED, { exact: true })).toHaveCount(1);
});

// The same invariant for a row the pane already holds. The user leaves the
// thread and comes back (or reloads) while the message is still unconsumed,
// so the quiet row loaded from SQLite sits in the pane, hidden behind the
// preview. Claude consumes queued input at the turn boundary, after every
// tool row the running turn still wrote, so the echo that confirms the row
// MOVES it past those rows: a rewrite in place, not an append.
//
// The defect this pins (2026-09-25): the window's newest cursor followed
// appends only, so the moved row stayed outside the loaded window while the
// preview let it go on confirmation. The message was in neither place until
// an unrelated append pulled the window forward, and then it jumped into
// view instead of gliding. Turn 2 waits at a gate here, so nothing else
// appends: the hand-off alone has to show the row.
test('a queued message the pane already holds hands over when Claude picks it up at the turn boundary', async ({
  harness,
  page,
}) => {
  test.setTimeout(90_000);

  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('queued-at-boundary', [
      [
        emit(textLines('msg-working', 'Checking the tree first.')),
        { waitSignal: { name: 'hold' } },
        emit([
          toolUseLine('msg-tool', 'tu-status', 'Bash', { command: 'git status --short' }),
          toolResultLine('tu-status', ' M README.md'),
        ]),
        { waitSignal: { name: 'finish' } },
        emit([...textLines('msg-final', 'The tree has one change.'), RESULT_LINE]),
      ],
      [
        { waitSignal: { name: 'reply' } },
        emit([...textLines('msg-reply', 'Changelog updated.'), RESULT_LINE]),
      ],
    ], { queuedInputAtBoundary: true }),
  });
  const threadId = await seedAgentThread(harness, 'queued-at-boundary', 'Queued at boundary');
  await harness.open(page);
  await page.getByText('Queued at boundary', { exact: true }).click();
  const mockId = await startMock(harness, threadId);
  const input = page.getByLabel('Message Input');
  await input.fill('Update the tree.');
  await input.press('Enter');
  await waitForGate(harness, 'hold');

  // Queue while the turn is out at its tool. Claude's active turn makes the
  // backend flush at once (a quiet row at the provisional tail); the mock
  // acks `queued` and holds the envelope for the boundary.
  const flushed = harness.waitForEvent('provider:queue_flushed', (ev: any) => ev.threadId === threadId);
  await input.fill(QUEUED);
  await input.press('Enter');
  await flushed;
  const preview = page.getByTestId('send-queue-preview-row').filter({ hasText: QUEUED });
  const bubble = page.getByTestId('user-message-bubble').filter({ hasText: QUEUED });
  await expect(preview).toBeVisible();
  await expect(bubble).toHaveCount(0);

  // Leave and return: the pane reloads its window from SQLite, quiet row
  // included, and the preview still owns the message.
  await seedAgentThread(harness, 'queue-elsewhere', 'Queue elsewhere');
  await page.getByText('Queue elsewhere', { exact: true }).click();
  await page.getByText('Queued at boundary', { exact: true }).click();
  await expect(preview).toBeVisible();
  await expect(bubble).toHaveCount(0);

  await startZoneSampler(page, QUEUED);

  // The tool rows land past the quiet row's provisional slot.
  await advance(harness, mockId, 'hold');
  await waitForGate(harness, 'finish');
  const toolRow = page.getByTestId('command-output-row').filter({ hasText: 'git status --short' });
  await expect(toolRow).toBeVisible();
  await expect(preview).toBeVisible();
  await expect(bubble).toHaveCount(0);

  // The turn ends, the mock picks the held envelope up, and the echo
  // confirms the row at its new place after the tool rows.
  await advance(harness, mockId, 'finish');
  await harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await waitForGate(harness, 'reply');

  await expect.poll(lastZoneSample(page), { timeout: 30_000 }).toBe('0/1');
  expectOneHomePerFrame(await stopZoneSampler(page));

  // The row rendered where the backend placed it: after the tool rows, in
  // view, without an unrelated append to pull the window there.
  await expect(bubble).toBeInViewport();
  const bubbleFollowsTool = await page.evaluate((message) => {
    const tool = document.querySelector('[data-testid="command-output-row"]');
    const row = Array.from(document.querySelectorAll('[data-testid="user-message-bubble"]'))
      .find((el) => (el.textContent ?? '').includes(message));
    if (!tool || !row) return 'missing';
    return (tool.compareDocumentPosition(row) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
  }, QUEUED);
  expect(bubbleFollowsTool, 'the confirmed row sits after the tool row that streamed before its pickup').toBe(true);

  await advance(harness, mockId, 'reply');
  await harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await expect(page.getByTestId('send-queue-preview-row')).toHaveCount(0);
  await expect(page.getByText(QUEUED, { exact: true })).toHaveCount(1);
  await expect(page.getByText('Changelog updated.', { exact: true })).toBeVisible();
});

// A message sent after the turn completed on the wire but while its text is
// still revealing. The backend is idle, so nothing queues there; the pane's
// reveal frontier is still up, so a directly sent optimistic row would be
// withheld behind the draining prose with everything else past the frontier,
// and the person's own message would leave the composer and appear nowhere
// until the drain ended. The composer routes the send through the queue
// path while the frontier stands: the preview holds the message and the
// hand-off lands it when the gate releases the row.
test('a message sent while the previous turn is still revealing stays in the preview until its row renders', async ({
  harness,
  page,
}) => {
  test.setTimeout(90_000);

  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('send-during-drain', [
      [emit([...textLines('msg-prose', PROSE), RESULT_LINE])],
      [
        { waitSignal: { name: 'reply' } },
        emit([...textLines('msg-reply', 'Changelog updated.'), RESULT_LINE]),
      ],
    ]),
  });
  const threadId = await seedAgentThread(harness, 'send-during-drain', 'Send during drain');
  await harness.open(page);
  await page.getByText('Send during drain', { exact: true }).click();
  const mockId = await startMock(harness, threadId);

  const input = page.getByLabel('Message Input');
  await input.fill('Write the long answer.');
  await input.press('Enter');
  // The wire is done with turn 1 while the prose has barely started
  // revealing: the first paragraph is on screen, the last is not.
  await harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await expect(page.getByText('Paragraph 1:', { exact: false })).toBeVisible();
  await expect(page.getByText('Paragraph 8:', { exact: false })).toHaveCount(0);

  await startZoneSampler(page, QUEUED);

  // The send is accepted into the queue (and dispatched at once by the
  // idle backend) rather than appended as an optimistic row.
  const flushed = harness.waitForEvent('provider:queue_flushed', (ev: any) => ev.threadId === threadId);
  await input.fill(QUEUED);
  await input.press('Enter');
  await flushed;
  await waitForGate(harness, 'reply');

  await expect.poll(lastZoneSample(page), { timeout: 30_000 }).toBe('0/1');
  expectOneHomePerFrame(await stopZoneSampler(page));
  // The row landed after the prose it waited for.
  await expect(page.getByText('Paragraph 8:', { exact: false })).toBeVisible();
  const bubble = page.getByTestId('user-message-bubble').filter({ hasText: QUEUED });
  await expect(bubble).toBeInViewport();

  await advance(harness, mockId, 'reply');
  await harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await expect(page.getByText('Changelog updated.', { exact: true })).toBeVisible();
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
