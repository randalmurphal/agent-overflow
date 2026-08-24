// Real-Chromium outcome test for the settle-time recent-window prune's
// quiet deferral (docs/architecture/scroll-arbitration-plan.md §2).
// Wire settle is not visual quiet — the reveal drain and its glide
// outlive the turn — so `settleTurn` records the prune as pending and
// the quiet scheduler lands the head-drop only once nothing is
// animating. The user-visible contract: the turn settles with the
// window intact, the prune arrives on its own shortly after quiet, and
// the viewport never leaves the bottom.

import { describe, expect, it } from 'vitest';
import '../../../app.css';
import { makeItem } from '../../../test/helpers/chat';
import { waitFor } from '../../../test/helpers/browserFrames';
import {
  mountTimeline,
  setupTimelineHarness,
  waitForQuietBottom,
  distanceToBottom,
  type QuietBottomOptions,
} from '../../../test/helpers/timelineBrowserHarness';
import type { Item } from '../../types/models';

setupTimelineHarness();

const QUIET_BOTTOM: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };
const THREAD_ID = 'prune-quiet-deferral';

function shortRow(id: string, turnIndex: number): Item {
  return makeItem({
    id,
    threadId: THREAD_ID,
    turnIndex,
    itemIndex: 0,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'completed',
    summary: `Reply ${turnIndex}, kept short so an 800-row transcript mounts fast.`,
    createdAt: turnIndex,
    updatedAt: turnIndex,
  });
}

describe('recent-window prune quiet deferral', () => {
  it('settle leaves the window intact; the prune lands after quiet with the viewport still at the bottom', { timeout: 30000 }, async () => {
    // One over ACTIVE_TIMELINE_WINDOW_MAX_ITEMS (800), seeded through the
    // initial-load path (which never prunes).
    const seed: Item[] = [];
    for (let i = 0; i < 801; i++) seed.push(shortRow(`s-${i}`, i));
    const { pane, scrollEl } = await mountTimeline(THREAD_ID, seed, QUIET_BOTTOM);
    expect(pane.items).toHaveLength(801);

    // A turn streams one more row — the append arms the structural
    // spring and stamps liveness, so a glide (and then the sentinel) is
    // in flight when the wire settles a beat later.
    pane.setActiveTurn({ turnId: 'turn-live', turnIndex: 801, startedAt: 1 });
    pane.applyProviderItemUpserts([shortRow('s-live', 801)]);
    pane.settleTurn({
      turnId: 'turn-live',
      turnIndex: 801,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'end_turn',
      assistantMessageId: null,
      tokenUsage: null,
      aborted: false,
      errorMessage: '',
    });

    // The settle itself must not have flushed the head-drop.
    expect(pane.items).toHaveLength(802);
    expect(pane.hasDeferredRecentWindowPrune).toBe(true);
    const plane = scrollEl.querySelector('[data-virtual-row-plane]') as HTMLElement | null;
    const liveRow = (scrollEl.querySelector('[data-item-id="s-800"]')
      ?.closest('[data-virtual-row]') as HTMLElement | undefined) ?? null;
    expect(plane).not.toBeNull();
    expect(liveRow).not.toBeNull();
    expect(plane?.style.transform).toBe('');
    expect(getComputedStyle(plane!).willChange).toBe('auto');

    // The scheduler owes the prune once the glide and its sentinel die
    // (liveness hold ~500ms, recheck cadence 200ms) — no further wire
    // activity may be required to trigger it.
    await waitFor(
      () => !pane.hasDeferredRecentWindowPrune && pane.items.length < 802,
      'quiet scheduler to land the deferred prune',
      600,
    );
    expect(pane.items.length).toBeLessThanOrEqual(501);
    expect(pane.hasMoreHistory).toBe(true);
    expect(scrollEl.querySelector('[data-virtual-row-plane]')).toBe(plane);
    expect(
      scrollEl.querySelector('[data-item-id="s-800"]')?.closest('[data-virtual-row]'),
    ).toBe(liveRow);

    // And the reader never left the bottom: the head-drop was absorbed
    // above the viewport.
    await waitForQuietBottom(scrollEl, 'post-prune bottom', QUIET_BOTTOM);
    expect(distanceToBottom(scrollEl)).toBeLessThanOrEqual(QUIET_BOTTOM.epsilonPx);
  });
});
