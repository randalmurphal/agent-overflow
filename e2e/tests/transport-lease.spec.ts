// The lifecycle lease end to end: a real streaming turn on a real socket,
// with the connection told its client is backgrounded
// (internal/transport/lease.go; docs/specs/remote-access.md § "The phone
// client", "Lifecycle").
//
// WHY THIS LEVEL. Both halves are already pinned in isolation —
// internal/transport/{lease,conn_lease}_test.go for the window, the seq
// rules and the withheld channels, frontend .../wsClient.test.ts for the
// frame and its restatement. What only this level can show is that the
// coalescing survives the REAL producer: that a provider streaming at its
// own cadence through triage's own buffers reaches a backgrounded client as
// a few frames instead of dozens, that the text is all there afterwards, and
// that the completion frames a phone's badges and push mapping depend on are
// untouched by any of it.
//
// HOW THE LEASE IS SENT. The transport module's door is
// `setClientLease` (frontend/src/lib/transport/lease.ts), and its producer
// is a native app-lifecycle plugin that lands with the phone shell in 6f-c.
// No browser has one, so this spec writes the frame on the page's own live
// socket through the shared recorder's `sendClientFrame`. The bytes and the
// backend are real; only the caller is the test.
//
// ASSERTIONS ARE COUNTS AND PRESENCE, never a clock reading. The property is
// "a burst arrives merged" and "a resumed client is streamed again", and
// both are ratios: renderer and provider timing move between runs, and the
// specs that asserted on either have been the flaky ones (e2e/AGENTS.md).
import type { HarnessApp } from '../src/harness.js';
import { test, expect, type SeedResult } from './fixtures.js';
import { RESULT_LINE, type ScenarioStep, startMock } from './agent-visibility-helpers.js';
import { readWire, recordWire, sendClientFrame, watchedNow } from './transport-watch-helpers.js';

/**
 * How many delta chunks each turn streams. Large enough that one frame per
 * 250ms is unmistakably fewer than one frame per chunk, small enough that a
 * turn is a second of wall clock rather than a minute.
 */
const CHUNKS = 60;
/** Pause between wire lines. CHUNKS × this is roughly the turn's length. */
const CHUNK_DELAY_MS = 10;

function chunkText(turn: string, index: number): string {
  return `${turn}${index} `;
}

function chunks(turn: string): string[] {
  return Array.from({ length: CHUNKS }, (_, i) => chunkText(turn, i));
}

/**
 * One assistant message streamed as many `content_block_delta` lines, which
 * is what makes triage emit one `provider:item_event` delta per chunk. The
 * single-delta `textLines` helper next door cannot produce a burst.
 */
function streamedTextLines(messageId: string, pieces: string[]): string[] {
  const j = (envelope: Record<string, unknown>) => JSON.stringify(envelope);
  return [
    j({
      type: 'stream_event',
      event: 'message_start',
      data: { type: 'message_start', message: { id: messageId, role: 'assistant' } },
    }),
    j({
      type: 'stream_event',
      event: 'content_block_start',
      data: {
        type: 'content_block_start',
        index: 0,
        content_block: { type: 'text', text: '' },
      },
    }),
    ...pieces.map((text) =>
      j({
        type: 'stream_event',
        event: 'content_block_delta',
        data: { type: 'content_block_delta', delta: { type: 'text_delta', text } },
      }),
    ),
    j({ type: 'stream_event', event: 'content_block_stop', data: { type: 'content_block_stop', index: 0 } }),
    j({ type: 'stream_event', event: 'message_stop', data: { type: 'message_stop' } }),
    j({
      type: 'assistant',
      message: {
        id: messageId,
        role: 'assistant',
        model: 'claude-mock-1',
        content: [{ type: 'text', text: pieces.join('') }],
      },
    }),
  ];
}

function streamingTurn(messageId: string, pieces: string[]): ScenarioStep[] {
  return [{ emit: { lines: [...streamedTextLines(messageId, pieces), RESULT_LINE], delayBetweenMs: CHUNK_DELAY_MS } }];
}

/** Two scripted turns: the first runs backgrounded, the second resumed. */
function leaseScenario(): unknown {
  return {
    version: 1,
    name: 'lease-stream',
    provider: 'claude',
    turns: [
      { label: 'lease-background', steps: streamingTurn('msg-bg', chunks('bg')) },
      { label: 'lease-active', steps: streamingTurn('msg-fg', chunks('fg')) },
    ],
    afterTurns: 'silent',
  };
}

/** Every delta frame this page was pushed for one thread, from `since`. */
function deltasFor(
  received: Array<{ channel: string; threadId: string; action?: string }>,
  since: number,
  threadId: string,
): number {
  return received
    .slice(since)
    .filter(
      (event) =>
        event.channel === 'provider:item_event'
        && event.threadId === threadId
        && event.action === 'delta',
    ).length;
}

function channelFrames(
  received: Array<{ channel: string; threadId: string }>,
  since: number,
  channel: string,
  threadId: string,
): number {
  return received
    .slice(since)
    .filter((event) => event.channel === channel && event.threadId === threadId).length;
}

/** Run one scripted turn and answer the wire index it started at. */
async function runTurn(harness: HarnessApp, threadId: string, prompt: string): Promise<void> {
  await harness.rpc('SendMessage', threadId, prompt, null);
  await harness.waitForEvent<{ threadId: string }>(
    'provider:turn_completed',
    (event) => event.threadId === threadId,
  );
}

test('a backgrounded connection is streamed in merged frames, and resuming restores the rate', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'transport-lease',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Lease\n' } }] },
        threads: [
          {
            title: 'Leased thread',
            turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }],
          },
        ],
      },
    ],
  });
  const project = seed.projects[0]!;
  const threadId = project.threadIds[0]!;
  await harness.rpc('HarnessSetScenario', { cwd: project.path, scenario: leaseScenario() });

  await recordWire(page);
  await harness.open(page);
  await page.getByText('Leased thread').click();
  // The pane is what watches the thread; without it the transcript stream is
  // narrowed away and there would be no deltas to count either way.
  await expect.poll(async () => watchedNow(await readWire(page))).toEqual([threadId]);

  // ---- turn one: the client says it is backgrounded ----
  await sendClientFrame(page, { type: 'lease', state: 'background' });
  const backgroundFrom = (await readWire(page)).received.length;
  await startMock(harness, threadId);
  await runTurn(harness, threadId, 'stream while I am asleep');
  // The pane still renders, so the whole streamed answer is the proof no
  // text was lost in the merge: the merged frames carry every chunk's text,
  // concatenated in arrival order.
  await expect(page.getByText(chunkText('bg', CHUNKS - 1))).toBeVisible();

  const afterBackground = await readWire(page);
  const backgroundDeltas = deltasFor(afterBackground.received, backgroundFrom, threadId);
  expect(backgroundDeltas, 'a backgrounded client still receives its text').toBeGreaterThan(0);
  // The RATIO is the assertion. One frame per 250ms against a turn of
  // roughly CHUNKS × CHUNK_DELAY_MS milliseconds is a handful; one frame per
  // chunk is CHUNKS. A third of CHUNKS sits far above the former and far
  // below the latter, so neither provider jitter nor a slow box moves it.
  expect(
    backgroundDeltas,
    `a backgrounded connection must be merged, got ${backgroundDeltas} of ${CHUNKS} chunks`,
  ).toBeLessThan(CHUNKS / 3);
  // Everything else flows unchanged: this is what the phone's badges and its
  // push mapping ride on, and a lease must not touch them.
  expect(
    channelFrames(afterBackground.received, backgroundFrom, 'provider:turn_completed', threadId),
    'the turn completion must reach a backgrounded client',
  ).toBeGreaterThan(0);
  expect(
    channelFrames(afterBackground.received, backgroundFrom, 'thread:updated', threadId),
    'the wildcard thread-row carrier must reach a backgrounded client',
  ).toBeGreaterThan(0);
  // Highlight seeds are withheld from a backgrounded connection. A FLOOR,
  // not the proof: the channel is remote-only, so this loopback page would
  // not be sent one either way, and its producer is gated on a remote client
  // being attached at all. The withholding itself is proven at the seam it
  // runs on, in internal/transport/lease_test.go.
  expect(
    channelFrames(afterBackground.received, backgroundFrom, 'highlight:seed', threadId),
    'no highlight seed reaches a backgrounded connection',
  ).toBe(0);

  // ---- turn two: resumed ----
  await sendClientFrame(page, { type: 'lease', state: 'active' });
  const activeFrom = (await readWire(page)).received.length;
  await runTurn(harness, threadId, 'stream while I am watching');
  await expect(page.getByText(chunkText('fg', CHUNKS - 1))).toBeVisible();

  const activeDeltas = deltasFor((await readWire(page)).received, activeFrom, threadId);
  // The same script, streamed live: the frames are back to one per chunk,
  // minus whatever the provider itself coalesced.
  expect(
    activeDeltas,
    `a resumed connection is streamed again, got ${activeDeltas} of ${CHUNKS} chunks`,
  ).toBeGreaterThan(CHUNKS / 2);
});
