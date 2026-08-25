// The sync module's behavioral rules beyond plumbing: the rail makes
// exactly one position claim at a time — a single "current" tick (the
// on-screen message nearest the visible-band center) OR the gap dot,
// never both — and the center it measures against is the VISIBLE band's
// (the component's getter), not the raw viewport's. The gap/range/
// nearest math itself is covered in messageNavRail.test.ts — these
// tests drive the rAF-coalesced writer.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { MergedNavTicks } from './messageNavRail';
import {
  createNavRailViewportSync,
  type NavRailSyncCtx,
  type NavRailTickRegistration,
  type NavRailViewportSync,
} from './messageNavRailSync';

// Three loaded ticks at nodes 0 / 5 / 10; each node is 100px tall, so
// findItemIndex is floor(offset / 100) and a tick's offset is node·100.
const merged: MergedNavTicks = {
  ticks: [
    { id: 'u1', turnIndex: 0, itemIndex: 0, nodeIndex: 0 },
    { id: 'u2', turnIndex: 1, itemIndex: 0, nodeIndex: 5 },
    { id: 'u3', turnIndex: 2, itemIndex: 0, nodeIndex: 10 },
  ],
  loadedStart: 0,
  loadedEnd: 2,
};

describe('messageNavRailSync single position claim', () => {
  let frames: FrameRequestCallback[];
  let scrollOffset: number;
  let visibleCenterY: number;
  let sync: NavRailViewportSync;
  let marker: HTMLElement;
  let strip: HTMLElement;
  let firstArrow: HTMLElement;
  let latestArrow: HTMLElement;
  let availableHeight: number;
  let tickEls: HTMLElement[];
  let tickRegs: NavRailTickRegistration[];
  // The fixture sync reads its ticks through this, so a test can rebuild
  // the list the way a paging pass does. Everything else leaves it at
  // the three-tick `merged` baseline.
  let ticksNow: MergedNavTicks;

  function ctxFor(
    list: TimelineVirtualizerHandle,
    ticks: MergedNavTicks,
    onClipChange?: () => void,
  ): NavRailSyncCtx {
    return {
      getListRef: () => list,
      getTicks: () => ticks,
      getMarkerEl: () => marker,
      getStripEl: () => strip,
      getFirstArrowEl: () => firstArrow,
      getLatestArrowEl: () => latestArrow,
      getAvailableHeightPx: () => availableHeight,
      getVisibleCenterY: () => visibleCenterY,
      isEnabled: () => true,
      onClipChange,
    };
  }

  function drainFrames(): void {
    while (frames.length > 0) {
      const batch = frames;
      frames = [];
      for (const cb of batch) cb(0);
    }
  }

  function currents(): string[] {
    return tickEls.map((el) => el.dataset.current ?? 'false');
  }

  beforeEach(() => {
    frames = [];
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      frames.push(cb);
      return frames.length;
    });
    vi.stubGlobal('cancelAnimationFrame', () => {
      frames = [];
    });
    scrollOffset = 0;
    visibleCenterY = 0; // raw viewport-center fallback unless a test sets it
    const list = {
      getScrollOffset: () => scrollOffset,
      getViewportSize: () => 300,
      findItemIndex: (offset: number) => Math.floor(offset / 100),
      getItemOffset: (nodeIndex: number) => nodeIndex * 100,
      sizeAt: () => 100,
      getTotalSize: () => 1100,
    } as unknown as TimelineVirtualizerHandle;
    marker = document.createElement('div');
    marker.style.visibility = 'hidden';
    strip = document.createElement('div');
    firstArrow = document.createElement('button');
    firstArrow.style.visibility = 'hidden';
    latestArrow = document.createElement('button');
    latestArrow.style.visibility = 'hidden';
    // Large enough that the 3-tick strip (16px) fits: no clip, no arrows.
    availableHeight = 300;
    ticksNow = merged;
    sync = createNavRailViewportSync({ ...ctxFor(list, merged), getTicks: () => ticksNow });
    tickRegs = [];
    tickEls = merged.ticks.map((_, i) => {
      const el = document.createElement('div');
      el.dataset.current = 'false';
      tickRegs.push(sync.registerTick(el, i));
      return el;
    });
  });

  afterEach(() => {
    sync.cancel();
    vi.unstubAllGlobals();
  });

  it('mid-gap: no tick in view → dot shows in the gap, nothing current', () => {
    // Viewport covers nodes 1..3 — between the ticks at 0 and 5. The
    // viewport center (250px) sits past tick u1's offset (0), so the
    // dot lands in gap 0-1: (0 + 0.5) / 2 = 25%.
    scrollOffset = 100;
    sync.schedule();
    drainFrames();
    expect(marker.style.visibility).toBe('');
    expect(marker.style.top).toBe('25%');
    expect(currents()).toEqual(['false', 'false', 'false']);
  });

  it('messages on screen: only the nearest-to-center lights and the dot yields', () => {
    // Tall viewport seeing both u1 (node 0) and u2 (node 5): rebuild
    // with viewport 700 via a fresh sync instance.
    const list = {
      getScrollOffset: () => scrollOffset,
      getViewportSize: () => 700,
      findItemIndex: (offset: number) => Math.floor(offset / 100),
      getItemOffset: (nodeIndex: number) => nodeIndex * 100,
      sizeAt: () => 100,
      getTotalSize: () => 1100,
    } as unknown as TimelineVirtualizerHandle;
    const wide = createNavRailViewportSync(ctxFor(list, merged));
    tickEls.forEach((el, i) => wide.registerTick(el, i));
    // Offset 50 (off the top, so the thread-top override stays out of
    // it) → viewport 50..749 sees nodes 0..7 (both u1 and u2). Center
    // 400: u1's row center 50 is 350 away, u2's 550 is 150 away → u2 is
    // current, u1 stays off, dot hidden.
    scrollOffset = 50;
    wide.schedule();
    drainFrames();
    expect(currents()).toEqual(['false', 'true', 'false']);
    expect(marker.style.visibility).toBe('hidden');
    wide.cancel();
  });

  it('the visible-band center decides, not the raw viewport center', () => {
    const list = {
      getScrollOffset: () => scrollOffset,
      getViewportSize: () => 700,
      findItemIndex: (offset: number) => Math.floor(offset / 100),
      getItemOffset: (nodeIndex: number) => nodeIndex * 100,
      sizeAt: () => 100,
      getTotalSize: () => 1100,
    } as unknown as TimelineVirtualizerHandle;
    const wide = createNavRailViewportSync(ctxFor(list, merged));
    tickEls.forEach((el, i) => wide.registerTick(el, i));
    // Same geometry as above (offset 50, off the thread-top override),
    // but a composer eats the bottom: the visible band's center is at
    // y=200 → absolute 250. u1's row center 50 is 200 away, u2's 550 is
    // 300 away → u1 is current now (the raw center 400 would pick u2).
    visibleCenterY = 200;
    scrollOffset = 50;
    wide.schedule();
    drainFrames();
    expect(currents()).toEqual(['true', 'false', 'false']);
    expect(marker.style.visibility).toBe('hidden');
    wide.cancel();
  });

  it('at the very top of the thread the first tick lights regardless of center', () => {
    const list = {
      getScrollOffset: () => scrollOffset,
      getViewportSize: () => 700,
      findItemIndex: (offset: number) => Math.floor(offset / 100),
      getItemOffset: (nodeIndex: number) => nodeIndex * 100,
      sizeAt: () => 100,
      getTotalSize: () => 1100,
    } as unknown as TimelineVirtualizerHandle;
    const wide = createNavRailViewportSync(ctxFor(list, merged));
    tickEls.forEach((el, i) => wide.registerTick(el, i));
    // Same geometry as the nearest-to-center test: at offset 0 the
    // center favors u2, but scrollTop 0 with the thread's first message
    // loaded (loadedStart 0) is the top of the CONVERSATION — u1 wins.
    scrollOffset = 0;
    wide.schedule();
    drainFrames();
    expect(currents()).toEqual(['true', 'false', 'false']);
    wide.cancel();
  });

  it('the top override does not fire at the top of a mid-history loaded window', () => {
    // Unloaded history above: tick 0 is baseline-only, loaded ticks
    // start at index 1. scrollTop 0 here is a transient paging edge,
    // so nearest-to-center still decides.
    const withHistory: MergedNavTicks = {
      ticks: [
        { id: 'old', turnIndex: 0, itemIndex: 0, nodeIndex: null },
        { id: 'u2', turnIndex: 1, itemIndex: 0, nodeIndex: 0 },
        { id: 'u3', turnIndex: 2, itemIndex: 0, nodeIndex: 5 },
      ],
      loadedStart: 1,
      loadedEnd: 2,
    };
    const list = {
      getScrollOffset: () => scrollOffset,
      getViewportSize: () => 700,
      findItemIndex: (offset: number) => Math.floor(offset / 100),
      getItemOffset: (nodeIndex: number) => nodeIndex * 100,
      sizeAt: () => 100,
      getTotalSize: () => 1100,
    } as unknown as TimelineVirtualizerHandle;
    const wide = createNavRailViewportSync(ctxFor(list, withHistory));
    tickEls.forEach((el, i) => wide.registerTick(el, i));
    // Center 350: u2's row center 50 is 300 away, u3's 550 is 200 away
    // → u3 stays current even at scrollTop 0.
    scrollOffset = 0;
    wide.schedule();
    drainFrames();
    expect(currents()).toEqual(['false', 'false', 'true']);
    wide.cancel();
  });

  it('at the very bottom of the thread the last tick lights regardless of center', () => {
    const list = {
      getScrollOffset: () => scrollOffset,
      getViewportSize: () => 700,
      findItemIndex: (offset: number) => Math.floor(offset / 100),
      getItemOffset: (nodeIndex: number) => nodeIndex * 100,
      sizeAt: () => 100,
      getTotalSize: () => 1100,
    } as unknown as TimelineVirtualizerHandle;
    const wide = createNavRailViewportSync(ctxFor(list, merged));
    tickEls.forEach((el, i) => wide.registerTick(el, i));
    // Max offset 400 → viewport 400..1099 sees u2 and u3. Center 750:
    // u2's row center 550 is 200 away, u3's 1050 is 300 away — nearest
    // says u2, but this is the bottom of the CONVERSATION (loadedEnd is
    // the final tick) → u3 wins.
    scrollOffset = 400;
    wide.schedule();
    drainFrames();
    expect(currents()).toEqual(['false', 'false', 'true']);
    wide.cancel();
  });

  it('the bottom override does not fire when newer messages are unloaded', () => {
    // Unloaded suffix: the loaded window's bottom is a paging seam, so
    // nearest-to-center still decides at max scroll.
    const withNewer: MergedNavTicks = {
      ticks: [
        { id: 'u1', turnIndex: 0, itemIndex: 0, nodeIndex: 4 },
        { id: 'u2', turnIndex: 1, itemIndex: 0, nodeIndex: 7 },
        { id: 'newer', turnIndex: 2, itemIndex: 0, nodeIndex: null },
      ],
      loadedStart: 0,
      loadedEnd: 1,
    };
    const list = {
      getScrollOffset: () => scrollOffset,
      getViewportSize: () => 700,
      findItemIndex: (offset: number) => Math.floor(offset / 100),
      getItemOffset: (nodeIndex: number) => nodeIndex * 100,
      sizeAt: () => 100,
      getTotalSize: () => 800,
    } as unknown as TimelineVirtualizerHandle;
    const wide = createNavRailViewportSync(ctxFor(list, withNewer));
    tickEls.forEach((el, i) => wide.registerTick(el, i));
    // Max offset 100 → viewport 100..799 sees u1 and u2. Center 450 is
    // exactly u1's row center → u1, not the override's u2.
    scrollOffset = 100;
    wide.schedule();
    drainFrames();
    expect(currents()).toEqual(['true', 'false', 'false']);
    wide.cancel();
  });

  it('an unscrollable thread ignores the edge overrides', () => {
    // Everything fits one screen: no scroll position exists, both edges
    // hold at once, so nearest-to-center decides (the top override
    // would have answered u1).
    const tiny: MergedNavTicks = {
      ticks: [
        { id: 'u1', turnIndex: 0, itemIndex: 0, nodeIndex: 0 },
        { id: 'u2', turnIndex: 1, itemIndex: 0, nodeIndex: 1 },
      ],
      loadedStart: 0,
      loadedEnd: 1,
    };
    const list = {
      getScrollOffset: () => scrollOffset,
      getViewportSize: () => 300,
      findItemIndex: (offset: number) => Math.floor(offset / 100),
      getItemOffset: (nodeIndex: number) => nodeIndex * 100,
      sizeAt: () => 100,
      getTotalSize: () => 300,
    } as unknown as TimelineVirtualizerHandle;
    const small = createNavRailViewportSync(ctxFor(list, tiny));
    small.registerTick(tickEls[0], 0);
    small.registerTick(tickEls[1], 1);
    // Center 150 is u2's row center exactly → u2.
    scrollOffset = 0;
    small.schedule();
    drainFrames();
    expect(currents()).toEqual(['false', 'true', 'false']);
    small.cancel();
  });

  it('leaving the message re-shows the dot in the next gap', () => {
    scrollOffset = 450; // nodes 4..7: u2 on screen
    sync.schedule();
    drainFrames();
    expect(currents()).toEqual(['false', 'true', 'false']);
    expect(marker.style.visibility).toBe('hidden');
    // Nodes 6..9: past u2, before u3 → gap 1-2: (1 + 0.5) / 2 = 75%.
    scrollOffset = 600;
    sync.schedule();
    drainFrames();
    expect(currents()).toEqual(['false', 'false', 'false']);
    expect(marker.style.visibility).toBe('');
    expect(marker.style.top).toBe('75%');
  });

  // The claim is keyed on the ELEMENT, not its index, so a structural
  // pass is an ordinary resync. These two pin what that buys: a rebuild
  // must not sweep the ticks it is not changing (the sweep was
  // O(thread length) attribute writes per pass), and a torn-down tick
  // must take the claim with it.
  it('a rebuilt tick list keeps the claim and leaves every other tick alone', () => {
    // Nodes 4..7: u2 (node 5) is the only tick on screen.
    scrollOffset = 450;
    sync.schedule();
    drainFrames();
    expect(currents()).toEqual(['false', 'true', 'false']);

    // A value no writer in the module produces: surviving it proves the
    // resync touched only the tick whose state actually changed.
    tickEls[0].dataset.current = 'untouched';
    tickEls[2].dataset.current = 'untouched';

    // Older messages page in: one tick prepended, the other three
    // reused at shifted indices — u2's claim moves from index 1 to 2
    // while staying the same element.
    const prepended: MergedNavTicks = {
      ticks: [
        { id: 'u0', turnIndex: 0, itemIndex: 0, nodeIndex: 0 },
        { id: 'u1', turnIndex: 1, itemIndex: 0, nodeIndex: 3 },
        { id: 'u2', turnIndex: 2, itemIndex: 0, nodeIndex: 5 },
        { id: 'u3', turnIndex: 3, itemIndex: 0, nodeIndex: 10 },
      ],
      loadedStart: 0,
      loadedEnd: 3,
    };
    ticksNow = prepended;
    tickRegs[0].update(1);
    tickRegs[1].update(2);
    tickRegs[2].update(3);
    const prependedEl = document.createElement('div');
    prependedEl.dataset.current = 'false';
    sync.registerTick(prependedEl, 0);

    sync.schedule();
    drainFrames();
    expect(tickEls[1].dataset.current, 'the claim rides its element').toBe('true');
    expect(tickEls[0].dataset.current).toBe('untouched');
    expect(tickEls[2].dataset.current).toBe('untouched');
    expect(prependedEl.dataset.current).toBe('false');
  });

  it('destroying the lit tick releases the claim instead of stranding it', () => {
    scrollOffset = 450;
    sync.schedule();
    drainFrames();
    const lit = tickEls[1];
    expect(lit.dataset.current).toBe('true');

    tickRegs[1].destroy();
    // Nodes 8..10 at the thread's bottom edge → u3 lights. It must do so
    // without a stale clear reaching the torn-down element, which is
    // detached by now: writing to it would be work on a dead node.
    scrollOffset = 800;
    sync.schedule();
    drainFrames();
    expect(tickEls[2].dataset.current).toBe('true');
    expect(lit.dataset.current, 'never written after teardown').toBe('true');
  });

  it('a strip that fits is actively cleared, not merely left alone', () => {
    // Seed the elements with a stale overflow state (what a remount
    // inheriting old inline styles would look like): the writer must
    // WRITE the fitting answer, not skip because its cache matches.
    strip.style.transform = 'translateY(-99px)';
    firstArrow.style.visibility = '';
    latestArrow.style.visibility = '';
    scrollOffset = 100; // mid-gap, position fraction 0.25
    sync.schedule();
    drainFrames();
    expect(strip.style.transform).toBe('');
    expect(sync.getClipOffsetPx()).toBe(0);
    expect(firstArrow.style.visibility).toBe('hidden');
    expect(latestArrow.style.visibility).toBe('hidden');
  });

  it('mid-gap with no message on screen the dot fraction drives the clip', () => {
    // 16px strip in a 6px window → maxClip 10. Viewport covers nodes
    // 1..3 (between u1 and u2): gap 0 → fraction (0+0.5)/2 = 0.25 →
    // clip = 0.25·16 − 3 = 1, unclamped, both ends clipped out.
    availableHeight = 6;
    scrollOffset = 100;
    sync.schedule();
    drainFrames();
    expect(marker.style.visibility).toBe('');
    expect(marker.style.top).toBe('25%');
    expect(sync.getClipOffsetPx()).toBe(1);
    expect(strip.style.transform).toBe('translateY(-1px)');
    expect(firstArrow.style.visibility).toBe('');
    expect(latestArrow.style.visibility).toBe('');
  });

  it('with nothing loaded the raw scroll proportion still slides the strip', () => {
    // Baseline-only ticks (no geometry at all): railGapLow answers
    // null, so the position falls back to offset / maxOffset.
    const noneLoaded: MergedNavTicks = {
      ticks: [
        { id: 'u1', turnIndex: 0, itemIndex: 0, nodeIndex: null },
        { id: 'u2', turnIndex: 1, itemIndex: 0, nodeIndex: null },
        { id: 'u3', turnIndex: 2, itemIndex: 0, nodeIndex: null },
      ],
      loadedStart: -1,
      loadedEnd: -1,
    };
    availableHeight = 6;
    const list = {
      getScrollOffset: () => scrollOffset,
      getViewportSize: () => 300,
      findItemIndex: (offset: number) => Math.floor(offset / 100),
      getItemOffset: (nodeIndex: number) => nodeIndex * 100,
      sizeAt: () => 100,
      getTotalSize: () => 1100,
    } as unknown as TimelineVirtualizerHandle;
    const cold = createNavRailViewportSync(ctxFor(list, noneLoaded));
    tickEls.forEach((el, i) => cold.registerTick(el, i));
    // maxOffset 800, offset 400 → proportion 0.5 → clip 0.5·16 − 3 = 5.
    scrollOffset = 400;
    cold.schedule();
    drainFrames();
    expect(currents()).toEqual(['false', 'false', 'false']);
    expect(marker.style.visibility).toBe('hidden');
    expect(cold.getClipOffsetPx()).toBe(5);
    expect(strip.style.transform).toBe('translateY(-5px)');
    cold.cancel();
  });

  it('onClipChange fires only when the clip actually moves', () => {
    availableHeight = 6;
    const list = {
      getScrollOffset: () => scrollOffset,
      getViewportSize: () => 300,
      findItemIndex: (offset: number) => Math.floor(offset / 100),
      getItemOffset: (nodeIndex: number) => nodeIndex * 100,
      sizeAt: () => 100,
      getTotalSize: () => 1100,
    } as unknown as TimelineVirtualizerHandle;
    const onClipChange = vi.fn();
    const watched = createNavRailViewportSync(ctxFor(list, merged, onClipChange));
    tickEls.forEach((el, i) => watched.registerTick(el, i));
    scrollOffset = 100; // gap 0 → clip 1 (moved from the initial 0)
    watched.schedule();
    drainFrames();
    expect(onClipChange).toHaveBeenCalledTimes(1);
    // Same position again: clip unchanged, no callback.
    watched.schedule();
    drainFrames();
    expect(onClipChange).toHaveBeenCalledTimes(1);
    scrollOffset = 600; // gap 1 → clip 9 → fires again
    watched.schedule();
    drainFrames();
    expect(onClipChange).toHaveBeenCalledTimes(2);
    watched.cancel();
  });

  it('an overflowing strip slides with the position claim and each arrow tracks its clipped end', () => {
    // 3 ticks · 8px = a 16px strip in a 10px window → maxClip 6.
    availableHeight = 10;
    const list = {
      getScrollOffset: () => scrollOffset,
      getViewportSize: () => 700,
      findItemIndex: (offset: number) => Math.floor(offset / 100),
      getItemOffset: (nodeIndex: number) => nodeIndex * 100,
      sizeAt: () => 100,
      getTotalSize: () => 1100,
    } as unknown as TimelineVirtualizerHandle;
    const wide = createNavRailViewportSync(ctxFor(list, merged));
    tickEls.forEach((el, i) => wide.registerTick(el, i));
    // Thread top: u1 current (fraction 0) → no slide; the first tick is
    // in the window so its arrow yields, the latest is clipped out.
    scrollOffset = 0;
    wide.schedule();
    drainFrames();
    expect(strip.style.transform).toBe('');
    expect(wide.getClipOffsetPx()).toBe(0);
    expect(firstArrow.style.visibility).toBe('hidden');
    expect(latestArrow.style.visibility).toBe('');
    // Mid: u2 current (fraction 0.5) → clip 0.5·16 − 5 = 3, both ends
    // clipped out, both arrows on.
    scrollOffset = 50;
    wide.schedule();
    drainFrames();
    expect(strip.style.transform).toBe('translateY(-3px)');
    expect(wide.getClipOffsetPx()).toBe(3);
    expect(firstArrow.style.visibility).toBe('');
    expect(latestArrow.style.visibility).toBe('');
    // Thread bottom: the edge override claims u3 (fraction 1) → max
    // clip; the latest tick is in the window now, its arrow yields.
    scrollOffset = 400;
    wide.schedule();
    drainFrames();
    expect(strip.style.transform).toBe('translateY(-6px)');
    expect(firstArrow.style.visibility).toBe('');
    expect(latestArrow.style.visibility).toBe('hidden');
    wide.cancel();
  });
});
