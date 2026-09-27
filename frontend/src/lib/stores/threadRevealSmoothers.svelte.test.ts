// stores/threadRevealSmoothers.svelte.test.ts
//
// Reactive reads of the retained reasoning tails: a reveal frame of one row
// wakes only that row's readers. Retention and settle behavior are
// threadPaneRevealSmoothing.test.ts.

import { beforeEach, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import { __setSmoothingClockForTest } from './thread.svelte';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { FakeSmoothingClock, installThreadPaneTestEnv } from '../../test/helpers/threadPane';

beforeEach(installThreadPaneTestEnv);

it('wakes only the streaming row\'s readers on its reveal frames', async () => {
  const clock = new FakeSmoothingClock();
  __setSmoothingClockForTest(clock);
  const cleanups: (() => void)[] = [];
  try {
    const threadId = 'thread-tail-signals';
    const pane = await buildPane(makeThread({ id: threadId }));
    const thinking = (itemIndex: number, status: 'streaming' | 'completed', summary = '') => makeItem({
      id: `think:0:${itemIndex}`, threadId, kind: 'thinking', role: 'assistant', status, summary,
      turnIndex: 0, itemIndex, payloadId: `thinking:think:0:${itemIndex}`, updatedAt: 1,
    });
    const drain = () => {
      let safety = 500;
      while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
    };
    // Rows with no tail entry, and rows whose settled tail is retained.
    const bare = Array.from({ length: 10 }, (_, index) => thinking(index, 'completed', `settled ${index}`));
    for (const item of bare) pane.upsertItem(item);
    const retained = [10, 11].map(index => thinking(index, 'streaming'));
    for (const item of retained) {
      pane.upsertItem(item);
      pane.applyItemDelta({ threadId, itemId: item.id, kind: 'thinking', delta: `retained ${item.id} `, updatedAt: 2 });
      drain();
      pane.applyItemPatch({ threadId, itemId: item.id, kind: 'thinking', patch: { rev: 0, status: 'completed', updatedAt: 3 } });
      expect(pane.liveThinkingWindowForItem(item.id)?.text).toBe(`retained ${item.id} `);
    }
    const live = thinking(12, 'streaming');
    pane.upsertItem(live);

    let otherRuns = 0;
    let liveRuns = 0;
    cleanups.push($effect.root(() => {
      for (const item of [...bare, ...retained]) {
        $effect(() => {
          pane.liveThinkingWindowForItem(item.id);
          otherRuns += 1;
        });
      }
      $effect(() => {
        pane.liveThinkingWindowForItem(live.id);
        liveRuns += 1;
      });
    }));
    flushSync();
    // The live row's first reveal creates its entry, which wakes every row
    // whose read found none once.
    pane.applyItemDelta({ threadId, itemId: live.id, kind: 'thinking', delta: 'first ', updatedAt: 4 });
    drain();
    flushSync();
    otherRuns = 0;
    liveRuns = 0;

    for (let chunk = 0; chunk < 40; chunk++) {
      pane.applyItemDelta({ threadId, itemId: live.id, kind: 'thinking', delta: `word${chunk} `, updatedAt: 5 + chunk });
      for (let frame = 0; frame < 3; frame++) {
        clock.tickFrame(16);
        flushSync();
      }
    }
    expect(pane.isItemSmoothing(live.id)).toBe(true);
    expect(liveRuns).toBeGreaterThanOrEqual(40);
    expect(otherRuns).toBe(0);
  } finally {
    for (const cleanup of cleanups) cleanup();
    __setSmoothingClockForTest(undefined);
  }
});
