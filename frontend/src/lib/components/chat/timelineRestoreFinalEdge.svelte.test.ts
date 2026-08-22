// The size-priors capture at the timeline's FINAL edges.
//
// The scroll-driven capture is rate-bounded (it rides `saveScrollSnapshot`,
// which fires per scroll frame), so a capture landing inside the cooldown is
// skipped. That is fine for a cadence and wrong for a last chance: unmount
// is the end of this timeline, and nothing after it will capture. The
// switch-away edge is covered in the store (`thread.svelte.test.ts`), which
// is where it has to happen — this suite covers the unmount half.

import { describe, expect, it, vi } from 'vitest';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { UseStickToBottomController } from '../../utils/scroll/index.svelte';
import { createTimelineRestore } from './timelineRestore.svelte';

function makeHarness(threadId: string) {
  const persistSizePriors = vi.fn();
  const persistSizePriorsExact = vi.fn();
  const stick = {
    isAtBottom: true,
    isSticky: true,
    escapedFromLock: false,
    markAtBottom: vi.fn(),
    skipWarmup: vi.fn(),
    armRestoreSnap: vi.fn(),
    forceStick: vi.fn(),
    setEscapedFromLock: vi.fn(),
    pauseAutoScroll: vi.fn(() => () => {}),
    observe: vi.fn(),
  } as unknown as UseStickToBottomController;

  const restore = createTimelineRestore({
    // An empty, settled thread: `maybeRestoreAfterFlush` takes its
    // no-rows branch, which is enough to make the module consider the
    // thread restored — the only precondition the destroy path has.
    getPane: () => ({ threadId, items: [], loading: false }) as unknown as ThreadPane,
    stick,
    getListRef: () => undefined,
    getScrollEl: () => undefined,
    getRevealedNodes: () => [],
    getGroupedNodes: () => [],
    findTimelineNodeIndex: () => -1,
    persistSizePriors,
    persistSizePriorsExact,
    armWarmupWithReset: () => {},
    resetAutoLoadGates: () => {},
  });

  return { restore, persistSizePriors, persistSizePriorsExact };
}

describe('timeline restore final edges', () => {
  it('captures size priors exactly on unmount', () => {
    const h = makeHarness('thread-destroy');
    h.restore.maybeRestoreAfterFlush();
    expect(h.restore.restoredThreadId).toBe('thread-destroy');
    h.persistSizePriors.mockClear();
    h.persistSizePriorsExact.mockClear();

    h.restore.saveSnapshotOnDestroy();

    // Exact, not the rate-bounded variant: the reader's last scroll frame
    // may have armed a cooldown a moment ago, and swallowing this capture
    // loses everything measured since the last one that got through.
    expect(h.persistSizePriorsExact).toHaveBeenCalledTimes(1);
    expect(h.persistSizePriors).not.toHaveBeenCalled();
  });

  it('captures nothing on unmount before the thread has been restored', () => {
    // The guard the position snapshot already carries, and for the same
    // reason: nothing has been measured under this thread yet, so a capture
    // would replace its stored entry with an empty one.
    const h = makeHarness('thread-never-restored');

    h.restore.saveSnapshotOnDestroy();

    expect(h.persistSizePriorsExact).not.toHaveBeenCalled();
  });

  it('keeps the scroll cadence on the rate-bounded capture', () => {
    const h = makeHarness('thread-cadence');
    h.restore.maybeRestoreAfterFlush();
    h.persistSizePriors.mockClear();
    h.persistSizePriorsExact.mockClear();

    h.restore.saveScrollSnapshot();

    expect(h.persistSizePriors).toHaveBeenCalledTimes(1);
    expect(h.persistSizePriorsExact).not.toHaveBeenCalled();
  });
});
