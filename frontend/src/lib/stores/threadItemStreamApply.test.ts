// stores/threadItemStreamApply.test.ts
//
// threadItemStreamApply.ts through the pane: the replace-pattern that lands
// a streamed delta or a field patch on an existing row, the reveal settings
// that make a delta arrive whole, and the drops (stale delta, unknown row,
// wrong thread). How the revealed text is PACED is threadPaneRevealSmoothing;
// the ordering gate over it is threadRevealSequencer.

import { beforeEach, describe, expect, it } from 'vitest';
import { __setSmoothingClockForTest, createThreadPane } from './thread.svelte';
import { getSettings, resetSettingsForTest } from './settings.svelte';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import {
  FakeSmoothingClock,
  installThreadPaneTestEnv,
  nextFrame,
} from '../../test/helpers/threadPane';

describe('threadItemStreamApply', () => {
  beforeEach(installThreadPaneTestEnv);

  it('applies streaming deltas in place via replace-pattern', async () => {
    const pane = createThreadPane();
    pane.upsertItem(
      makeItem({
        id: 'text:0:0',
        kind: 'assistant_text',
        status: 'streaming',
        summary: 'hello',
      }),
    );
    const initialItems = pane.items;
    const initialRevision = pane.timelineRevision;
    const initialLength = initialItems.length;

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: ' world',
      updatedAt: 123,
    });
    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: '!',
      updatedAt: 124,
    });
    // Smoothing routes streaming text through a per-item rAF smoother;
    // flush it synchronously so the assertion sees the fully revealed
    // accumulated text rather than the partial mid-reveal.
    pane.__flushItemSmoothersForTest();
    await nextFrame();

    // Replace-pattern semantics: deltas write `items[index] = { ...current, summary }`,
    // so the array proxy reference is stable, length is stable, and
    // timelineRevision does NOT bump (no insertions or sort). The summary
    // at the streaming row's slot reflects the accumulated deltas.
    expect(pane.items).toBe(initialItems);
    expect(pane.items.length).toBe(initialLength);
    expect(pane.timelineRevision).toBe(initialRevision);
    expect(pane.items.find((item) => item.id === 'text:0:0')?.summary).toBe(
      'hello world!',
    );
    expect(pane.items.find((item) => item.id === 'text:0:0')?.updatedAt).toBe(
      124,
    );
  });

  it('keeps assistant text full even when the row has a payload link', async () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    try {
      const pane = createThreadPane();
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          kind: 'assistant_text',
          status: 'streaming',
          summary: 'seed',
          payloadId: 'assistant-text:thread-1:text:0:0',
          payloadKind: 'assistant_text',
        }),
      );

      const delta = Array.from({ length: 200 }, (_, index) => `word${index}`).join(' ');
      const expected = `seed${delta}`;
      pane.applyItemDelta({
        threadId: 'thread-1',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta,
        updatedAt: 125,
      });
      for (let frame = 0; frame < 500; frame += 1) {
        if ((pane.items.find((item) => item.id === 'text:0:0')?.summary ?? '') === expected) {
          break;
        }
        clock.tickFrame(16);
      }

      const summary =
        pane.items.find((item) => item.id === 'text:0:0')?.summary ?? '';
      expect(summary).toBe(expected);
    } finally {
      __setSmoothingClockForTest(undefined);
    }
  });

  it('low power mode reveals a streamed delta whole on the next frame', async () => {
    // Guards the revealImmediately wiring in threadStreamingReveal: with
    // the setting on, a fat delta lands as ONE summary write on the next
    // scheduled tick instead of animating across hundreds of frames.
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    getSettings().lowPowerMode = true;
    try {
      const pane = createThreadPane();
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          kind: 'assistant_text',
          status: 'streaming',
          summary: 'seed',
        }),
      );

      const delta = Array.from({ length: 200 }, (_, index) => `word${index}`).join(' ');
      pane.applyItemDelta({
        threadId: 'thread-1',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta,
        updatedAt: 125,
      });
      clock.tickFrame(1);
      expect(pane.items.find((item) => item.id === 'text:0:0')?.summary).toBe(
        `seed${delta}`,
      );
    } finally {
      __setSmoothingClockForTest(undefined);
      resetSettingsForTest();
    }
  });

  it('disabling streaming reveals a streamed delta whole on the next frame', async () => {
    // The "Streaming enabled" setting is separate from low power: with it
    // OFF the smoother must also pass received straight through (one
    // summary write on the next tick), so ChatMarkdown's committed-block
    // gate reflects wire arrival rather than a rate-limited crawl. Guards
    // the streamingEnabled arm of threadStreamingReveal's revealImmediately.
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    getSettings().streamingEnabled = false;
    try {
      const pane = createThreadPane();
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          kind: 'assistant_text',
          status: 'streaming',
          summary: 'seed',
        }),
      );

      const delta = Array.from({ length: 200 }, (_, index) => `word${index}`).join(' ');
      pane.applyItemDelta({
        threadId: 'thread-1',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta,
        updatedAt: 125,
      });
      clock.tickFrame(1);
      expect(pane.items.find((item) => item.id === 'text:0:0')?.summary).toBe(
        `seed${delta}`,
      );
    } finally {
      __setSmoothingClockForTest(undefined);
      resetSettingsForTest();
    }
  });

  it('thinking-row deltas trim to the 400-rune tail in place', async () => {
    // The frontend mirrors the server-side `thinkingPreviewRunes = 400`
    // cap so the completion upsert (which carries the same tail) does
    // not visibly shrink the row at settle. Full thinking content stays
    // on-demand via the expansion handle.
    const pane = createThreadPane();
    pane.upsertItem(
      makeItem({
        id: 'think:0:0',
        kind: 'thinking',
        status: 'streaming',
        summary: 'seed',
        payloadId: 'thinking-payload',
      }),
    );

    // Send an 800-rune block; only the last 400 should survive.
    const bigChunk = 'a'.repeat(800);
    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'think:0:0',
      kind: 'thinking',
      delta: bigChunk,
      updatedAt: 100,
    });
    // Drain the smoother so the trim-to-tail logic in its reveal
    // callback has actually written through to the row.
    pane.__flushItemSmoothersForTest();

    const after =
      pane.items.find((item) => item.id === 'think:0:0')?.summary ?? '';
    expect([...after].length).toBe(400);
    expect(after.endsWith('a'.repeat(400))).toBe(true);
    expect(pane.items.find((item) => item.id === 'think:0:0')?.updatedAt).toBe(
      100,
    );
  });

  it('drains a completion upsert through the existing streaming cursor', async () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    try {
      const pane = createThreadPane();
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          kind: 'assistant_text',
          status: 'streaming',
          summary: 'hello',
        }),
      );
      pane.applyItemDelta({
        threadId: 'thread-1',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta: ' world',
        updatedAt: 123,
      });
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          kind: 'assistant_text',
          status: 'completed',
          summary: 'hello world',
        }),
      );

      const current = () =>
        pane.items.find((item) => item.id === 'text:0:0');
      expect(current()?.status).toBe('completed');
      expect(current()?.summary).toBe('hello');
      expect(pane.isItemSmoothing('text:0:0')).toBe(true);

      let framesRemaining = 20;
      while (
        pane.isItemSmoothing('text:0:0') &&
        framesRemaining-- > 0
      ) {
        clock.tickFrame(16);
      }

      expect(framesRemaining).toBeGreaterThan(0);
      expect(current()?.summary).toBe('hello world');
      expect(pane.isItemSmoothing('text:0:0')).toBe(false);
    } finally {
      __setSmoothingClockForTest(undefined);
    }
  });

  it('ignores stale deltas for an item that already settled', async () => {
    const pane = createThreadPane();
    pane.upsertItem(
      makeItem({
        id: 'text:0:0',
        kind: 'assistant_text',
        status: 'completed',
        summary: 'yield timeouts',
      }),
    );

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: 'outs',
      updatedAt: 124,
    });

    expect(pane.items.find((item) => item.id === 'text:0:0')?.summary).toBe(
      'yield timeouts',
    );
  });

  it('drops deltas that arrive before the row exists', async () => {
    // With single-source-of-truth, deltas append in place to
    // `pane.items[i].summary`. A delta whose itemId has no entry in
    // `itemIndexById` is a no-op; events.ts batch ordering at
    // `flushPendingUpserts()` before `queueDelta` ensures the upsert
    // creates the row before any production delta touches the pane.
    const pane = createThreadPane();

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      delta: ' world',
      updatedAt: 123,
    });

    expect(pane.items.find((item) => item.id === 'text:0:0')).toBeUndefined();
  });

  it('drops wrong-thread upserts for an active pane', async () => {
    const pane = createThreadPane();
    await pane.switchThread(makeThread({ id: 'thread-a' }));

    pane.upsertItem(makeItem({ id: 'leaked', threadId: 'thread-b' }));
    pane.upsertItem(makeItem({ id: 'current', threadId: 'thread-a' }));

    expect(pane.items.map((item) => item.id)).toEqual(['current']);
  });

  describe('applyItemPatch', () => {
    it('applies status-only patch while preserving all other fields', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      const original = makeItem({
        id: 'text:0:0',
        threadId: 'thread-patch',
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: 'hello world, this is a long response',
        meta: '{"pathRefs":[]}',
        updatedAt: 1000,
      });
      pane.upsertItem(original);
      expect(pane.items).toHaveLength(1);

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        patch: { status: 'completed', updatedAt: 2000 },
      });

      const patched = pane.items[0];
      expect(patched.status).toBe('completed');
      expect(patched.updatedAt).toBe(2000);
      expect(patched.summary).toBe('hello world, this is a long response');
      expect(patched.meta).toBe('{"pathRefs":[]}');
      expect(patched.kind).toBe('assistant_text');
      expect(patched.role).toBe('assistant');
    });

    it('is a no-op for an unknown item id', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      pane.upsertItem(makeItem({ id: 'text:0:0', threadId: 'thread-patch' }));
      const before = pane.items[0];

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'nonexistent',
        kind: 'assistant_text',
        patch: { status: 'completed' },
      });

      expect(pane.items[0]).toBe(before);
    });

    it('is a no-op when patch changes nothing', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      const original = makeItem({
        id: 'text:0:0',
        threadId: 'thread-patch',
        status: 'completed',
        summary: 'hello',
      });
      pane.upsertItem(original);
      const before = pane.items[0];

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        patch: { status: 'completed', summary: 'hello' },
      });

      expect(pane.items[0]).toBe(before);
    });

    it('ignores patches for a different thread', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-a' }));
      pane.upsertItem(
        makeItem({ id: 'text:0:0', threadId: 'thread-a', status: 'streaming' }),
      );

      pane.applyItemPatch({
        threadId: 'thread-b',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        patch: { status: 'completed' },
      });

      expect(pane.items[0].status).toBe('streaming');
    });

    it('applies meta and decision patch fields', async () => {
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      pane.upsertItem(
        makeItem({
          id: 'tool:0:0',
          threadId: 'thread-patch',
          kind: 'tool_call',
          meta: '{"toolName":"Bash"}',
        }),
      );

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'tool:0:0',
        kind: 'tool_call',
        patch: {
          meta: '{"toolName":"Bash","task_id":"t1"}',
          decision: 'approved',
        },
      });

      expect(pane.items[0].meta).toBe('{"toolName":"Bash","task_id":"t1"}');
      expect(pane.items[0].decision).toBe('approved');
    });

    it('reveals the full extending summary when status flips to completed mid-smooth', async () => {
      // A completed-status patch is intentionally NOT in the snap set:
      // the smoother is left running so the trailing characters reveal
      // naturally instead of snapping. The patch's summary, if it
      // extends what the smoother has already received, is appended as
      // a delta — and the patch's `summary` field is not written
      // directly to items[index] because the smoother now owns the
      // visible summary. Once the stream is fully revealed, the
      // smoother disposes itself. Without that handoff the row would
      // be stuck at the mid-stream cursor when the smoother eventually
      // ticked the auto-cleanup branch.
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          threadId: 'thread-patch',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: 'initial',
          updatedAt: 1,
        }),
      );

      pane.applyItemDelta({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta: ' middle',
        updatedAt: 2,
      });

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        patch: {
          status: 'completed',
          summary: 'initial middle and the final tail',
          updatedAt: 3,
        },
      });

      pane.__flushItemSmoothersForTest();

      expect(pane.items[0].summary).toBe('initial middle and the final tail');
      expect(pane.items[0].status).toBe('completed');
      expect(pane.items[0].updatedAt).toBe(3);
    });

    it('snaps and lets the patch summary win on a non-extending completion overwrite', async () => {
      // When `completed` arrives with a summary that does NOT extend
      // what the smoother already received (a backwards correction or
      // a wholesale rewrite), the smoother snaps so its in-flight
      // reveal doesn't trample the patch, and the patch summary is
      // written through to items[index] as the final wire shape.
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          threadId: 'thread-patch',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: 'initial received',
          updatedAt: 1,
        }),
      );

      pane.applyItemDelta({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta: ' more streamed',
        updatedAt: 2,
      });

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        patch: {
          status: 'completed',
          summary: 'completely different final wording',
          updatedAt: 3,
        },
      });

      expect(pane.items[0].summary).toBe('completely different final wording');
      expect(pane.items[0].status).toBe('completed');
    });

    it('snaps on errored status and lets the interrupted-prefix patch summary win', async () => {
      // Snap-status terminal patches (errored / killed / declined)
      // synchronously reveal the smoother's full received text before
      // writing the patch summary. The patch summary often carries an
      // "[interrupted] …" prefix or similar; it must land as the final
      // visible text, not be overwritten by a trailing reveal.
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          threadId: 'thread-patch',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: 'partial reveal so far',
          updatedAt: 1,
        }),
      );

      pane.applyItemDelta({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta: ' more',
        updatedAt: 2,
      });

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        patch: {
          status: 'errored',
          summary: '[interrupted] partial reveal so far',
          updatedAt: 3,
        },
      });

      expect(pane.items[0].summary).toBe('[interrupted] partial reveal so far');
      expect(pane.items[0].status).toBe('errored');
    });

    it('handles a bare status-only completion patch (no summary) on a smoothing row', async () => {
      // A completion patch may arrive with only `status` and `updatedAt`
      // — no `summary`. The smoother is left running with the items
      // status already flipped; on the next natural rAF tick (or
      // synchronous flush) the smoother reveals the remaining received
      // characters and the onReveal auto-cleanup branch disposes the
      // entry once `current.status !== 'streaming' && isCaughtUp()`.
      const pane = await buildPane(makeThread({ id: 'thread-patch' }));
      pane.upsertItem(
        makeItem({
          id: 'text:0:0',
          threadId: 'thread-patch',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: 'seed',
          updatedAt: 1,
        }),
      );

      pane.applyItemDelta({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        delta: ' more',
        updatedAt: 2,
      });

      pane.applyItemPatch({
        threadId: 'thread-patch',
        itemId: 'text:0:0',
        kind: 'assistant_text',
        patch: { status: 'completed', updatedAt: 3 },
      });

      pane.__flushItemSmoothersForTest();

      expect(pane.items[0].summary).toBe('seed more');
      expect(pane.items[0].status).toBe('completed');
      expect(pane.items[0].updatedAt).toBe(3);
    });
  });
});
