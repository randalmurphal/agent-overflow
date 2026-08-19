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
import { createNavRailViewportSync, type NavRailViewportSync } from './messageNavRailSync';

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
  let tickEls: HTMLElement[];

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
    sync = createNavRailViewportSync({
      getListRef: () => list,
      getTicks: () => merged,
      getMarkerEl: () => marker,
      getVisibleCenterY: () => visibleCenterY,
      isEnabled: () => true,
    });
    tickEls = merged.ticks.map((_, i) => {
      const el = document.createElement('div');
      el.dataset.current = 'false';
      sync.registerTick(el, i);
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
    const wide = createNavRailViewportSync({
      getListRef: () => list,
      getTicks: () => merged,
      getMarkerEl: () => marker,
      getVisibleCenterY: () => visibleCenterY,
      isEnabled: () => true,
    });
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
    const wide = createNavRailViewportSync({
      getListRef: () => list,
      getTicks: () => merged,
      getMarkerEl: () => marker,
      getVisibleCenterY: () => visibleCenterY,
      isEnabled: () => true,
    });
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
    const wide = createNavRailViewportSync({
      getListRef: () => list,
      getTicks: () => merged,
      getMarkerEl: () => marker,
      getVisibleCenterY: () => visibleCenterY,
      isEnabled: () => true,
    });
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
    const wide = createNavRailViewportSync({
      getListRef: () => list,
      getTicks: () => withHistory,
      getMarkerEl: () => marker,
      getVisibleCenterY: () => visibleCenterY,
      isEnabled: () => true,
    });
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
    const wide = createNavRailViewportSync({
      getListRef: () => list,
      getTicks: () => merged,
      getMarkerEl: () => marker,
      getVisibleCenterY: () => visibleCenterY,
      isEnabled: () => true,
    });
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
    const wide = createNavRailViewportSync({
      getListRef: () => list,
      getTicks: () => withNewer,
      getMarkerEl: () => marker,
      getVisibleCenterY: () => visibleCenterY,
      isEnabled: () => true,
    });
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
    const small = createNavRailViewportSync({
      getListRef: () => list,
      getTicks: () => tiny,
      getMarkerEl: () => marker,
      getVisibleCenterY: () => visibleCenterY,
      isEnabled: () => true,
    });
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
});
