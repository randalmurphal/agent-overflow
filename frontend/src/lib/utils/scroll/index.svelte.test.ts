import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createUseStickToBottomController,
  type UseStickToBottomController,
  type UseStickToBottomOptions,
} from './index.svelte';
import { resetScrollIntentModuleStateForTest } from './intent';
import { RETAIN_ANIMATION_DURATION_MS } from './spring';
import { latchedSpringMode, SPRING_MODE_HOLD_MS } from '../springAnimationLatch';
import { clearUiRenderTrace, getUiRenderTraceRecords, setUiRenderTraceEnabled } from '../uiRenderTrace';

// happy-dom doesn't measure layout, so tests stub scrollHeight /
// clientHeight / scrollTop on the scroll element via Object.defineProperty.
// Tests then mutate the underlying numbers to simulate content growth,
// composer height changes, viewport resizes, etc.
interface Geometry {
  scrollHeight: number;
  clientHeight: number;
  scrollTop: number;
  contentHeight: number;
}

interface StubGeometryOptions {
  setScrollTop?: (value: number, geom: Geometry) => void;
}

function stubGeometry(
  scrollEl: HTMLElement,
  contentEl: HTMLElement,
  geom: Geometry,
  options: StubGeometryOptions = {},
): void {
  Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => geom.scrollHeight });
  Object.defineProperty(scrollEl, 'clientHeight', { configurable: true, get: () => geom.clientHeight });
  Object.defineProperty(scrollEl, 'scrollTop', {
    configurable: true,
    get: () => geom.scrollTop,
    set: (v: number) => {
      if (options.setScrollTop) {
        options.setScrollTop(v, geom);
        return;
      }
      geom.scrollTop = Math.max(0, Math.min(v, geom.scrollHeight - geom.clientHeight));
    },
  });
  Object.defineProperty(contentEl, 'scrollHeight', { configurable: true, get: () => geom.contentHeight });
}

// Stub the geometry that `handlePointerDown` reads to classify a
// pointerdown as "in the scrollbar gutter": offsetWidth (200) wider
// than clientWidth (180) so scrollbarWidth=20, with a bounding rect
// whose right edge is 200. Tests that synthesize scrollbar pointer
// taps use clientX=195 to land inside the 180..200 gutter.
function stubScrollbarGutter(scrollEl: HTMLElement): void {
  Object.defineProperty(scrollEl, 'offsetWidth', { configurable: true, get: () => 200 });
  Object.defineProperty(scrollEl, 'clientWidth', { configurable: true, get: () => 180 });
  vi.spyOn(scrollEl, 'getBoundingClientRect').mockReturnValue({
    x: 0, y: 0, top: 0, left: 0, right: 200, bottom: 600,
    width: 200, height: 600, toJSON: () => ({}),
  } as DOMRect);
}

class MockResizeObserver {
  static instances: MockResizeObserver[] = [];
  callback: ResizeObserverCallback;
  observed: Element[] = [];
  constructor(cb: ResizeObserverCallback) {
    this.callback = cb;
    MockResizeObserver.instances.push(this);
  }
  observe(el: Element): void {
    this.observed.push(el);
  }
  unobserve(): void {}
  disconnect(): void {
    this.observed = [];
  }
  /** Fire the callback synchronously with a single entry for the given element, height, and optional width. */
  fire(el: Element, height: number, width = 0): void {
    this.callback(
      [
        {
          target: el,
          contentRect: { height, width, top: 0, left: 0, right: width, bottom: height, x: 0, y: 0, toJSON: () => ({}) } as DOMRectReadOnly,
          borderBoxSize: [],
          contentBoxSize: [],
          devicePixelContentBoxSize: [],
        } as ResizeObserverEntry,
      ],
      this as unknown as ResizeObserver,
    );
  }
}

// rAF frames advance performance.now in 16.67ms steps so the
// scroll-follow spring makes real
// progress per tick in the test environment (happy-dom's rAF doesn't drive
// performance.now on its own). Tests that assert event-driven behavior
// (sync-pin, scroll handler, gesture handlers) don't depend on this — those
// happen synchronously without rAF.
let mockNow = 0;
function nextFrame(): Promise<void> {
  return new Promise<void>((resolve) =>
    requestAnimationFrame(() => {
      mockNow += 16.67;
      resolve();
    }),
  );
}
/** A rAF whose wall-clock gap differs from the steady 16.67ms — models
 * high-refresh frames (8.33), dropped frames (33.34), or a long stall. */
function nextFrameAfter(ms: number): Promise<void> {
  return new Promise<void>((resolve) =>
    requestAnimationFrame(() => {
      mockNow += ms;
      resolve();
    }),
  );
}
function nextTimer(): Promise<void> {
  // Resolves after the 1ms scroll-handler / RO-clear setTimeout.
  return new Promise<void>((resolve) => setTimeout(resolve, 5));
}
function waitRealMs(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function fireWheel(el: HTMLElement, deltaY: number, target?: HTMLElement): void {
  const event = new WheelEvent('wheel', { deltaY, bubbles: true });
  if (target) Object.defineProperty(event, 'target', { value: target });
  el.dispatchEvent(event);
}
function fireKey(el: HTMLElement, key: string): void {
  el.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true }));
}
function fireTouchStart(el: HTMLElement, clientY: number): void {
  el.dispatchEvent(new TouchEvent('touchstart', { bubbles: true, touches: [{ clientY } as Touch] }));
}
function fireTouchMove(el: HTMLElement, clientY: number): void {
  el.dispatchEvent(new TouchEvent('touchmove', { bubbles: true, touches: [{ clientY } as Touch] }));
}
function fireScroll(el: HTMLElement): void {
  el.dispatchEvent(new Event('scroll'));
}
function fireScrollEnd(el: HTMLElement): void {
  el.dispatchEvent(new Event('scrollend'));
}

describe('createUseStickToBottomController', () => {
  let scrollEl: HTMLDivElement;
  let contentEl: HTMLDivElement;
  let geom: Geometry;
  let controller: UseStickToBottomController;
  let originalRO: typeof ResizeObserver | undefined;

  beforeEach(() => {
    resetScrollIntentModuleStateForTest();
    setUiRenderTraceEnabled(false);
    clearUiRenderTrace();
    MockResizeObserver.instances = [];
    originalRO = globalThis.ResizeObserver;
    (globalThis as unknown as { ResizeObserver: typeof MockResizeObserver }).ResizeObserver = MockResizeObserver;
    mockNow = 0;
    vi.spyOn(performance, 'now').mockImplementation(() => mockNow);

    scrollEl = document.createElement('div');
    contentEl = document.createElement('div');
    scrollEl.appendChild(contentEl);
    document.body.appendChild(scrollEl);

    geom = { scrollHeight: 1000, clientHeight: 600, scrollTop: 400, contentHeight: 800 };
    stubGeometry(scrollEl, contentEl, geom);

    controller = createUseStickToBottomController();
    controller.attach(scrollEl, contentEl);
  });

  afterEach(() => {
    controller.detach();
    setUiRenderTraceEnabled(false);
    clearUiRenderTrace();
    scrollEl.remove();
    if (originalRO) {
      (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver = originalRO;
    }
    vi.restoreAllMocks();
  });

  function getRO(): MockResizeObserver {
    const ro = MockResizeObserver.instances.at(-1);
    if (!ro) throw new Error('no ResizeObserver was created');
    return ro;
  }

  describe('initial state', () => {
    it('starts isSticky=true and isAtBottom=true', () => {
      // distance = 1000 - 400 - 600 = 0, ≤ 70 → near-bottom true.
      expect(controller.isSticky).toBe(true);
      expect(controller.isAtBottom).toBe(true);
      expect(controller.escapedFromLock).toBe(false);
    });

    it('reports isAtBottom=false when escaped AND scrolled away', async () => {
      geom.scrollTop = 100; // distance = 1000 - 100 - 600 = 300, > 70
      controller.setEscapedFromLock(true);
      fireScroll(scrollEl);
      await nextTimer();
      // isSticky=false (escaped), isNearBottom=false (geometrically away).
      // Public isAtBottom is intent || geometry → false.
      expect(controller.isSticky).toBe(false);
      expect(controller.isAtBottom).toBe(false);
    });

    it('first content RO fire snaps scrollTop to target when sticky', () => {
      geom.scrollTop = 0; // start at top
      const ro = getRO();
      ro.fire(contentEl, 800);
      // target = max(0, scrollHeight - clientHeight) = 1000 - 600 = 400
      expect(geom.scrollTop).toBe(400);
    });

    it('escape suppresses both first-fire snap AND positive-delta sync-pin', async () => {
      // Regression for the open-thread scroll animation: MessageTimeline's
      // $effect.pre calls setEscapedFromLock(true) on every threadId
      // change so the engine's incremental row remeasurement (positive-delta
      // RO fires) doesn't sync-pin the viewport to the bottom from
      // scrollTop=0. This test locks that contract at the controller
      // level: while escaped, neither the first RO fire nor any
      // subsequent positive delta is allowed to advance scrollTop. Only
      // an explicit forceStick (chip click) can resume bottom-following.
      controller.setEscapedFromLock(true);
      geom.scrollTop = 0;
      const ro = getRO();
      // First fire — would snap to bottom in the default sticky state,
      // but escape must hold.
      ro.fire(contentEl, 800);
      expect(geom.scrollTop).toBe(0);

      // Subsequent positive-delta fire (the engine finishes measuring more
      // rows). Sync-pin would write scrollTop if escape were false.
      ro.fire(contentEl, 1000);
      // Allow rAF + tick for any pin that wrongly fired.
      await nextFrame();
      await nextFrame();
      expect(geom.scrollTop).toBe(0);
      expect(controller.escapedFromLock).toBe(true);

      // forceStick clears escape and snaps to target — this is what
      // the scroll-to-bottom chip does (and what ChannelView does on
      // initial poll completion).
      controller.forceStick();
      expect(geom.scrollTop).toBe(400);
      expect(controller.escapedFromLock).toBe(false);
    });
  });

  describe('wheel handler', () => {
    it('wheel up on outer scrollEl escapes immediately', async () => {
      fireWheel(scrollEl, -50, scrollEl);
      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
      expect(controller.isAtBottom).toBe(false);
    });

    it('wheel-up escape blocks same-frame content growth even when the chat scroller never moves', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);

      // Escape is synchronous, so the streaming/contentRO growth that
      // lands in the same frame must not pin.
      expect(geom.scrollTop).toBe(400);
      expect(controller.escapedFromLock).toBe(true);

      await waitRealMs(180);

      expect(controller.escapedFromLock).toBe(true);
      expect(geom.scrollTop).toBe(400);
      expect(controller.isSticky).toBe(false);
    });

    it('keeps bottom lock when a resize-correlated upward jump has no user scroll movement', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);

      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);

      geom.scrollTop = 350;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(false);

      await waitRealMs(180);

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
      expect(geom.scrollTop).toBe(350);
    });

    it('confirms wheel-up escape even when the scroll event overlaps content resize', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 399;
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      expect(geom.scrollTop).toBe(399);
    });

    it('pending wheel intent expiry escapes instead of repinning if the scroller moved up', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 399;
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);

      await waitRealMs(180);

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
      expect(geom.scrollTop).toBe(399);
    });

    it('wheel up inside a nested overflow scroller escapes outer auto-follow', () => {
      const nested = document.createElement('div');
      nested.style.cssText = 'overflow-y: auto;';
      Object.defineProperty(nested, 'scrollHeight', { configurable: true, get: () => 200 });
      Object.defineProperty(nested, 'clientHeight', { configurable: true, get: () => 100 });
      Object.defineProperty(nested, 'scrollTop', { configurable: true, get: () => 50 });
      contentEl.appendChild(nested);
      fireWheel(scrollEl, -50, nested);
      expect(controller.escapedFromLock).toBe(true);
    });

    it('wheel outside the scroll element does not escape', () => {
      const outside = document.createElement('div');
      document.body.appendChild(outside);
      try {
        fireWheel(scrollEl, -50, outside);
        expect(controller.escapedFromLock).toBe(false);
      } finally {
        outside.remove();
      }
    });

    it('wheel up inside a nested scroller at its top escapes immediately', async () => {
      const nested = document.createElement('div');
      nested.style.cssText = 'overflow-y: auto;';
      Object.defineProperty(nested, 'scrollHeight', { configurable: true, get: () => 200 });
      Object.defineProperty(nested, 'clientHeight', { configurable: true, get: () => 100 });
      Object.defineProperty(nested, 'scrollTop', { configurable: true, get: () => 0 });
      contentEl.appendChild(nested);

      fireWheel(scrollEl, -50, nested);
      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('wheel down while sticky is a no-op', () => {
      fireWheel(scrollEl, 50, scrollEl);
      expect(controller.escapedFromLock).toBe(false);
    });

    it('scrollbar pointer interaction escapes immediately', async () => {
      stubScrollbarGutter(scrollEl);

      scrollEl.dispatchEvent(
        new PointerEvent('pointerdown', { bubbles: true, isPrimary: true, clientX: 195, clientY: 300 }),
      );
      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('scrollbar drag down to the bottom re-sticks', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      stubScrollbarGutter(scrollEl);
      scrollEl.dispatchEvent(
        new PointerEvent('pointerdown', { bubbles: true, isPrimary: true, clientX: 195, clientY: 300 }),
      );
      geom.scrollTop = 400;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('scrollbar drag final scroll can re-stick after pointerup but before the deferred check', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      stubScrollbarGutter(scrollEl);
      scrollEl.dispatchEvent(
        new PointerEvent('pointerdown', { bubbles: true, isPrimary: true, clientX: 195, clientY: 300 }),
      );
      geom.scrollTop = 400;
      fireScroll(scrollEl);
      document.dispatchEvent(new PointerEvent('pointerup', { bubbles: true, isPrimary: true }));
      await nextTimer();

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('scrollbar drag upward escapes after an earlier drag-to-bottom re-stick', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();

      stubScrollbarGutter(scrollEl);
      scrollEl.dispatchEvent(
        new PointerEvent('pointerdown', { bubbles: true, isPrimary: true, clientX: 195, clientY: 300 }),
      );
      geom.scrollTop = 400;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(false);

      geom.scrollTop = 300;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it.each(['pointerup', 'pointercancel'])(
      '%s ends scrollbar drag restick consent for later bottom scrolls',
      async (eventName) => {
        const ro = getRO();
        ro.fire(contentEl, 800);
        fireWheel(scrollEl, -50, scrollEl);
        geom.scrollTop = 300;
        fireScroll(scrollEl);
        await nextTimer();
        expect(controller.escapedFromLock).toBe(true);

        stubScrollbarGutter(scrollEl);
        scrollEl.dispatchEvent(
          new PointerEvent('pointerdown', { bubbles: true, isPrimary: true, clientX: 195, clientY: 300 }),
        );
        document.dispatchEvent(new PointerEvent(eventName, { bubbles: true, isPrimary: true }));

        geom.scrollTop = 400;
        fireScroll(scrollEl);
        await nextTimer();

        expect(controller.escapedFromLock).toBe(true);
        expect(controller.isSticky).toBe(false);
      },
    );

    it('middle-button autoscroll pointer input escapes immediately', () => {
      scrollEl.dispatchEvent(
        new PointerEvent('pointerdown', { bubbles: true, button: 1, isPrimary: true, clientX: 80, clientY: 300 }),
      );

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('wheel up does nothing when content fits in viewport', () => {
      geom.scrollHeight = 600; // = clientHeight, no overflow
      fireWheel(scrollEl, -50, scrollEl);
      expect(controller.escapedFromLock).toBe(false);
    });
  });

  describe('keyboard handler', () => {
    it.each(['ArrowUp', 'PageUp', 'Home'])('%s escapes immediately', async (key) => {
      fireKey(scrollEl, key);
      expect(controller.escapedFromLock).toBe(true);
    });

    it.each(['ArrowDown', 'PageDown', 'End'])('%s does not escape (handled by re-stick path)', (key) => {
      fireKey(scrollEl, key);
      expect(controller.escapedFromLock).toBe(false);
    });
  });

  describe('touch handler', () => {
    it('finger moves down (dy > 1) escapes immediately', async () => {
      fireTouchStart(scrollEl, 100);
      fireTouchMove(scrollEl, 130); // dy = +30
      expect(controller.escapedFromLock).toBe(true);
    });

    it('finger moves up (dy < -1) is a no-op (re-stick path handles it)', () => {
      fireTouchStart(scrollEl, 200);
      fireTouchMove(scrollEl, 150); // dy = -50
      expect(controller.escapedFromLock).toBe(false);
    });

    it('ignores tiny touch deltas (≤ 1px)', () => {
      fireTouchStart(scrollEl, 100);
      fireTouchMove(scrollEl, 100.5);
      expect(controller.escapedFromLock).toBe(false);
    });
  });

  describe('content ResizeObserver', () => {
    it('first fire (previousHeight undefined) snaps synchronously without scheduling rAF', () => {
      const ro = getRO();
      // No prior height; this is the initial fire.
      ro.fire(contentEl, 800);
      expect(geom.scrollTop).toBe(400); // snapped to target
    });

    it('positive delta + sticky sync-pins scrollTop to the new target in the same callback', async () => {
      // Sync pin: contentEl growing AND scrollTop catching up happen in
      // the same paint frame. No rAF gap, no perceptible motion.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial
      // Content grows; scroll target also grows.
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      // Single synchronous write inside the RO callback.
      expect(geom.scrollTop).toBe(600);
      // No rAF tick advances scrollTop further.
      const after = geom.scrollTop;
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(after);
    });

    it('positive delta + escaped does NOT sync-pin', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // initial

      controller.setEscapedFromLock(true);
      expect(controller.escapedFromLock).toBe(true);

      const before = geom.scrollTop;
      geom.scrollHeight = 1500; // grow
      geom.contentHeight = 1300;
      ro.fire(contentEl, 1300);
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(before);
    });

    it('escaped thinking-style grow, same-height, shrink, and grow updates do not re-stick', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      controller.setEscapedFromLock(true);
      geom.scrollTop = 200;

      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      ro.fire(contentEl, 1000);

      geom.scrollHeight = 1160;
      geom.contentHeight = 960;
      ro.fire(contentEl, 960);

      geom.scrollHeight = 1320;
      geom.contentHeight = 1120;
      ro.fire(contentEl, 1120);
      await nextFrame();

      expect(geom.scrollTop).toBe(200);
      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('negative delta with isNearBottom re-pins to target', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // initial; scrollTop becomes 400
      // Shrink content. Use distance just inside the 70px threshold.
      geom.scrollHeight = 800;
      geom.contentHeight = 600;
      // Without a re-pin, scrollTop=400 means distance = 800 - 400 - 600 = -200.
      ro.fire(contentEl, 600);
      // Re-pinned to target = max(0, 800 - 600) = 200.
      expect(geom.scrollTop).toBe(200);
    });

    it('negative delta with isAtBottomState=true but scrollTop bumped outside the near-bottom band still re-pins', async () => {
      // Regression for the layout-measurement-cascade jump: the virtualizer's
      // applyJump can shift scrollTop hundreds of pixels away from
      // bottom during row remeasurement, flipping the geometric
      // near-bottom check to false. Before the fix, the negative-
      // delta branch gated on isNearBottomState alone, so it bailed
      // and left the viewport stuck mid-cascade. After the fix, the
      // logical isAtBottomState intent is honored regardless of the
      // geometric flicker — the pin always lands at the new bottom.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial; isAtBottomState=true, scrollTop=400 (at bottom)
      expect(controller.isAtBottom).toBe(true);

      // Simulate a virtualizer compensation moving scrollTop away from
      // bottom AND the content shrinking at the same layout pass.
      // distance = 900 - 200 - 600 = 100, outside the 70px band.
      geom.scrollTop = 200;
      geom.scrollHeight = 900;
      geom.contentHeight = 700;
      ro.fire(contentEl, 700);

      // Pre-fix this would have stayed at 200 (negativeWillPin=false
      // because dist=100 > 70). Post-fix scrollTop is at the new
      // target = max(0, 900 - 600) = 300 because isAtBottomState=true
      // is honored.
      expect(geom.scrollTop).toBe(300);
    });

    it('negative delta does NOT re-pin when escapedFromLock=true (geometric disjunct does not bypass escape)', async () => {
      // Companion to the isAtBottomState disjunct test above. The
      // new gate is `(isAtBottomState || isNearBottomState) &&
      // !escaped && pauseDepth === 0`. Verify the escape guard still
      // wins when the geometric disjunct (isNearBottomState=true)
      // would otherwise fire the pin. setEscapedFromLock flips
      // isAtBottomState=false, so this isolates the isNearBottomState
      // branch — without the !escaped guard, the pre-fix behavior
      // (gate on isNearBottomState alone) would have written scrollTop
      // to the new target.
      //
      // Geometry: scrollTop sits 60 px above target so the geometric
      // near-bottom check is true, but new target stays above scrollTop
      // so the overshoot guard (separate, gated on escape/pause/spring)
      // does not fire and pollute the assertion.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial; scrollTop=400, target=400
      geom.scrollTop = 340; // distance = 1000 - 340 - 600 = 60 (near-bottom)
      controller.setEscapedFromLock(true);
      expect(controller.escapedFromLock).toBe(true);

      // Shrink contentEl without changing scrollHeight: delta=-100,
      // target unchanged at 400, scrollTop=340 < target so no overshoot.
      geom.contentHeight = 700;
      ro.fire(contentEl, 700);
      await nextFrame();
      expect(geom.scrollTop).toBe(340);
    });

    it('negative delta does NOT re-pin while pauseAutoScroll lease is held', async () => {
      // Resizer / drawer drags acquire pauseAutoScroll to keep
      // auto-follow from yanking the viewport mid-gesture. Verify
      // the new (isAtBottomState || isNearBottomState) disjunct does
      // not bypass the pauseDepth guard. Both disjuncts evaluate true
      // here (isAtBottomState retained from initial fire,
      // isNearBottomState=true from geometry), so the lease is the
      // only thing blocking the write.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial; scrollTop=400
      geom.scrollTop = 340; // distance=60, isNearBottomState=true
      const release = controller.pauseAutoScroll();

      geom.contentHeight = 700;
      ro.fire(contentEl, 700);
      await nextFrame();
      expect(geom.scrollTop).toBe(340);

      release();
    });

    it('overscroll guard clamps scrollTop > target', () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // initial
      // Force scrollTop above target externally (e.g. a compensation mis-landing).
      geom.scrollTop = 500; // target = 400
      geom.scrollHeight = 900; // shrink
      geom.contentHeight = 700;
      ro.fire(contentEl, 700);
      // Target now = max(0, 900 - 600) = 300; clamped.
      expect(geom.scrollTop).toBeLessThanOrEqual(300);
    });

    it('overscroll guard does NOT clamp when escaped (preserves user mid-history position)', () => {
      // The overshoot guard's purpose is to fix invalid scrollTop
      // states from compensation mis-landings or browser auto-clamping,
      // but when the user has explicitly escaped, the browser's own
      // clamp will fix any out-of-range scrollTop on the next paint
      // and we must NOT yank the user to the bottom. Without this
      // gate, a compensation write that nudges scrollTop past a freshly
      // shrunk target could snap the user from mid-history to bottom
      // as a side-effect of an above-viewport row remeasure.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial — sets previousHeight=800
      controller.setEscapedFromLock(true);
      // Simulate the shift-then-shrink scenario: scrollTop now
      // sits past the new target.
      geom.scrollTop = 500;
      geom.scrollHeight = 900;
      geom.contentHeight = 700;
      ro.fire(contentEl, 700); // delta = -100, overshoot would fire
      // Escape gate: NO write. scrollTop stays at 500. The browser
      // will clamp it itself on the next paint, or the user can
      // intentionally scroll. The controller does NOT slam them down.
      expect(geom.scrollTop).toBe(500);
      // And escape is still set.
      expect(controller.escapedFromLock).toBe(true);
    });

    it('overscroll guard does NOT clamp while pauseAutoScroll lease is held', () => {
      // Symmetric guard: the resizer/drawer-during-drag lease should
      // also suppress the overshoot snap, matching the positive and
      // negative pin branches that already check pauseDepth.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial
      const release = controller.pauseAutoScroll();
      geom.scrollTop = 500;
      geom.scrollHeight = 900;
      geom.contentHeight = 700;
      ro.fire(contentEl, 700);
      expect(geom.scrollTop).toBe(500);
      release();
    });

    it('clears resizeDifference after rAF + 1ms', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // initial, sets previousHeight
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      // resizeDifference is private — exercise the consequence: a scroll
      // event during the window is ignored, after the window the scroll
      // handler runs normally. Tested in the scroll handler section.
      await nextTimer();
      await nextFrame();
      // Should not throw or leak; nothing observable here, but the timer
      // must run cleanly.
      expect(true).toBe(true);
    });

    it('stops re-pinning an idle sub-pixel oscillation at the bottom (settle-vibration deadband)', () => {
      // Reproduces the idle full-viewport "vibration" limit cycle confirmed
      // from bug-report-20260701T012813Z (WSLg, fractional DPR). While pinned
      // at the bottom with NO spring in flight (springToken === 0),
      // `contentRect.height` lands on an X.5 boundary and flips ±2px every
      // ResizeObserver delivery; the bottom target flips with it. The sync-pin
      // used to re-pin scrollTop on EVERY nonzero delta — that write perturbed
      // fractional layout, the RO re-fired the opposite delta, and the cycle
      // sustained itself (~2 writes per cycle, forever; the whole viewport
      // shimmers). The IDLE_REPIN_DEADBAND_PX gate stops re-pinning once
      // scrollTop is already within the deadband of the bottom, breaking the
      // feedback at its source. Without the gate this asserts FAILS (~24
      // writes across 12 cycles).
      const ro = getRO();
      // Pin at the exact bottom (scrollTop 400 === scrollHeight - clientHeight).
      ro.fire(contentEl, 800);
      expect(geom.scrollTop).toBe(400);

      // Count the controller's scrollTop write DECISIONS — each writeScrollTop
      // assigns scrollEl.scrollTop exactly once. The test's own browser-clamp
      // simulation writes the `geom` field directly and is NOT counted.
      let controllerWrites = 0;
      const desc = Object.getOwnPropertyDescriptor(scrollEl, 'scrollTop')!;
      const realGet = desc.get as () => number;
      const realSet = desc.set as (value: number) => void;
      Object.defineProperty(scrollEl, 'scrollTop', {
        configurable: true,
        get: realGet,
        set(value: number) {
          controllerWrites += 1;
          realSet.call(this, value);
        },
      });

      // 12 net-zero ±2px cycles. scrollHeight tracks contentHeight (constant
      // padding). On each shrink the browser clamps scrollTop into range
      // synchronously before the RO fires — modeled by writing geom directly.
      for (let i = 0; i < 12; i++) {
        geom.contentHeight = 802; // grow +2
        geom.scrollHeight = 1002;
        ro.fire(contentEl, 802);
        geom.contentHeight = 800; // shrink -2
        geom.scrollHeight = 1000;
        geom.scrollTop = Math.min(geom.scrollTop, geom.scrollHeight - geom.clientHeight);
        ro.fire(contentEl, 800);
      }

      // With the deadband the controller recognizes it is within the band of
      // the bottom and stops re-pinning. Without it, ~24 writes accumulate.
      expect(controllerWrites).toBeLessThanOrEqual(2);
    });

    it('still pins line-sized growth beyond the deadband (deadband is sub-line)', () => {
      // Guards the upper bound: the deadband must not swallow a genuine line
      // of streaming content. A wrapped line (~24px, gap ≫ IDLE_REPIN_DEADBAND
      // _PX) still pins exactly to the new bottom on the sync-pin path — real
      // growth moves the target a line-height away, clearing the deadband.
      const ro = getRO();
      ro.fire(contentEl, 800);
      expect(geom.scrollTop).toBe(400);
      geom.contentHeight = 824;
      geom.scrollHeight = 1024;
      ro.fire(contentEl, 824);
      expect(geom.scrollTop).toBe(424); // target = 1024 - 600, pinned exactly
    });
  });

  describe('programmatic scroll write', () => {
    it('toggles scrollBehavior to auto during write and restores after', () => {
      scrollEl.style.scrollBehavior = 'smooth';
      const ro = getRO();
      ro.fire(contentEl, 800);
      // After the write, scrollBehavior should be back to 'smooth'.
      expect(scrollEl.style.scrollBehavior).toBe('smooth');
    });

    it('does not read computed styles during scroll writes', () => {
      const getComputedStyleSpy = vi.spyOn(window, 'getComputedStyle');
      const ro = getRO();
      ro.fire(contentEl, 800);
      expect(getComputedStyleSpy).not.toHaveBeenCalled();
    });

    it('scroll event matching a recorded programmatic-write token is ignored', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // writes scrollTop=400, records a token for top=400
      setUiRenderTraceEnabled(true);
      clearUiRenderTrace();
      // Fire a scroll event reading the same value.
      fireScroll(scrollEl);
      await nextTimer();
      // Tagged programmatic write: bails before the scroll.scrollEvent
      // trace, so zero records is the observable proof of suppression
      // (escapedFromLock alone would also hold on the untagged path).
      const scrollEvents = getUiRenderTraceRecords().filter((record) =>
        record.label.startsWith('scroll.scrollEvent'),
      );
      expect(scrollEvents).toEqual([]);
      expect(controller.escapedFromLock).toBe(false);
    });

    it('user scroll at a previously written value after token TTL expiry is processed, not swallowed', async () => {
      // Regression guard for the deleted `ignoreScrollToTop` exact tag:
      // it had no TTL, so a write whose scroll event never fired left a
      // tag armed indefinitely, silently swallowing a later genuine
      // user scroll (e.g. browser find-in-page / focus scroll, which
      // bypasses the wheel/pointer handlers that clear scroll state)
      // that happened to land at the same scrollTop value. Tokens
      // expire after PROGRAMMATIC_SCROLL_EVENT_TOKEN_TTL_MS, so the
      // late event must reach the untagged path. Fails on the legacy
      // exact-tag code (tag stays armed, event bailed → zero records).
      const ro = getRO();
      ro.fire(contentEl, 800); // writes scrollTop=400, records a token for top=400
      // No scroll event consumes the token; let it expire (TTL 500ms).
      mockNow += 600;
      setUiRenderTraceEnabled(true);
      clearUiRenderTrace();
      fireScroll(scrollEl); // genuine scroll landing at the same 400
      await nextTimer();
      const scrollEvents = getUiRenderTraceRecords().filter((record) =>
        record.label.startsWith('scroll.scrollEvent'),
      );
      expect(scrollEvents).toHaveLength(1);
      // Position is still at bottom, so processing it changes no intent.
      expect(controller.escapedFromLock).toBe(false);
    });

    it('ignores delayed duplicate scroll events from controller-owned writes', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      expect(geom.scrollTop).toBe(500);

      setUiRenderTraceEnabled(true);
      clearUiRenderTrace();

      fireScroll(scrollEl);
      fireScroll(scrollEl);
      await nextTimer();

      const scrollEvents = getUiRenderTraceRecords().filter((record) =>
        record.label.startsWith('scroll.scrollEvent'),
      );
      expect(scrollEvents).toEqual([]);
      expect(controller.escapedFromLock).toBe(false);
    });

    it('user scroll back to a previously token-tagged value is honored, not silently ignored', async () => {
      // Regression guard: a token's suppression is bounded — TTL plus a
      // small per-token duplicate budget for browser-coalesced re-fires
      // — and, critically, genuine user input clears it early: the
      // wheel-down below runs recordRecentDownIntent →
      // clearProgrammaticScrollState(), wiping the token FIFO. If a
      // regression kept tokens alive across explicit user gestures, the
      // back-scroll to the previously written value would be swallowed
      // and re-stick would never fire.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial RO write records a token for scrollTop=400
      // First scroll event consumes one duplicate-budget unit.
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(false);

      // User explicitly escapes, then genuinely moves AWAY and then
      // BACK to the previously written value by hand. The direction
      // gate on the re-stick path requires DOWN motion (scrollTop
      // increasing) for re-stick to fire, so we simulate a real
      // away-then-back gesture rather than a same-position re-fire.
      // The wheel-down while escaped clears the token FIFO, so the
      // back-scroll must re-stick.
      controller.setEscapedFromLock(true);
      expect(controller.escapedFromLock).toBe(true);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      fireWheel(scrollEl, 100, scrollEl);
      geom.scrollTop = 400;
      fireScroll(scrollEl);
      await nextTimer();
      // Re-stick path observes near-bottom + DOWN direction and clears
      // escape.
      expect(controller.escapedFromLock).toBe(false);
    });
  });

  describe('scroll handler', () => {
    it('selection during scroll flips escapedFromLock', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // initial setup

      // Simulate a selection inside scrollEl. happy-dom's selection API
      // is limited; we simulate the necessary state by stubbing both
      // mouseDown (via the module reset) and getSelection.
      document.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
      // Fake a selection range whose commonAncestorContainer === scrollEl.
      const fakeRange = { commonAncestorContainer: scrollEl } as unknown as Range;
      const realGetSelection = window.getSelection;
      vi.spyOn(window, 'getSelection').mockReturnValue({
        rangeCount: 1,
        getRangeAt: () => fakeRange,
      } as unknown as Selection);

      // Move scrollTop and fire scroll.
      geom.scrollTop = 200;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);

      // Cleanup
      vi.spyOn(window, 'getSelection').mockImplementation(realGetSelection);
      document.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });

    it('reaching the auto-follow bottom epsilon while escaped clears escapedFromLock', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // initial, scrollTop=400

      // User wheel-up moves the outer scroller away from bottom.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);

      // User scrolls back to within the sub-pixel auto-follow epsilon.
      // Public near-bottom still has a 70px visual band, but autonomous
      // following should only re-arm at the actual bottom.
      fireWheel(scrollEl, 50, scrollEl);
      geom.scrollTop = 250;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);

      geom.scrollTop = 399.75; // distance = 0.25
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('user wheel-down to bottom during a content-RO cascade clears escape', async () => {
      // Regression for "auto-follow stops on a thread until refresh".
      // On heavy threads the contentRO seam fires continuously (the engine
      // row remeasurement, Streamdown async typesetting completing in
      // rows above the viewport). The deferred scroll handler's
      // `resizeCorrelatedScroll` bail defends against the virtualizer's
      // applyJump producing an untagged scroll event mid-cascade, but
      // it was too broad — it also blocked input-backed scroll events
      // that happened to land while the cascade was active, leaving
      // escapedFromLock stuck true even when the user manually wheeled
      // back to the bottom. The fix lets a fresh recent down intent
      // through to the re-stick check while layout-only scrolls still
      // bail.
      //
      // Covers wheel; key and touch down-input share the same recent
      // down-intent window and have source-specific coverage above.
      const ro = getRO();
      ro.fire(contentEl, 800);
      fireScroll(scrollEl); // consume first-fire programmatic-write tag
      await nextTimer();
      expect(controller.escapedFromLock).toBe(false);

      // 1. Wheel up away from the bottom → escape.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // 2. ContentRO fires mid-cascade — a row above the viewport
      //    finishes typesetting or remeasures. resizeDifference is
      //    now non-zero with a 1ms+rAF clear scheduled.
      ro.fire(contentEl, 820);

      // 3. While the cascade flag is still set, the user wheels
      //    back down to the bottom.
      fireWheel(scrollEl, 50, scrollEl);
      geom.scrollTop = 400; // distance from bottom = 0
      fireScroll(scrollEl);
      await nextTimer();

      // The user's down-input landing at bottom must re-stick.
      // Without the fix, resizeCorrelatedScroll=true bails the
      // deferred before willRestick runs and escape stays true.
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('stays escaped when near the bottom visually but outside the auto-follow epsilon', async () => {
      // A scroll-event trajectory that lands close to the bottom but
      // outside AUTO_FOLLOW_BOTTOM_EPSILON_PX (4) must not re-stick.
      // The visual near-bottom band (70px) is wider on purpose, but
      // explicit escape wins over that band. Auto-follow only re-engages
      // once the user has recent down intent and actually REACHED the
      // bottom within browser-rounding tolerance.
      const ro = getRO();
      ro.fire(contentEl, 800);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // distance = 1000 - 395 - 600 = 5. Inside the visual 70px band
      // but outside the 4px auto-follow epsilon.
      geom.scrollTop = 395;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
      expect(controller.isAtBottom).toBe(false);
    });

    it('a small wheel-up that lands within the geometric near-bottom band but outside the auto-follow epsilon stays escaped', async () => {
      // Regression: STICK_TO_BOTTOM_OFFSET_PX (70) is used for button
      // visibility / negative-delta repin. The scroll handler's re-stick
      // path uses an actual-bottom epsilon. With the old code, a
      // wheel-up of 30px on a sticky session would set escape and then
      // the same scroll event would observe distance=30 ≤ 70 and
      // immediately re-stick — undoing the user's gesture.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial, geom.scrollTop=400 (target=400, distance=0)
      // First scroll event consumes the programmatic-write tag.
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.isSticky).toBe(true);
      expect(controller.escapedFromLock).toBe(false);

      // User wheels up by ~30px. distance from bottom is now 31 — well
      // inside isNearBottomState's 70px band, but outside re-stick's
      // small band, so the escape must persist.
      fireWheel(scrollEl, -30, scrollEl);
      geom.scrollTop = 369; // distance = 1000 - 369 - 600 = 31
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('wheel-up gesture is NOT undone by its own scroll event landing near bottom', async () => {
      // Regression for "scroll up one notch yanks back to bottom while
      // streaming". A sticky user who wheels up by exactly 1px should
      // break auto-follow and surface the chip even inside the old
      // near-bottom visual band.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial, scrollTop=400, sticky
      fireScroll(scrollEl); // consumes tag
      await nextTimer();
      expect(controller.isSticky).toBe(true);

      fireWheel(scrollEl, -1, scrollEl);
      geom.scrollTop = 399;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
      expect(controller.isAtBottom).toBe(false);
    });

    it('plain untagged upward scroll without input preserves bottom-follow intent', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      fireScroll(scrollEl); // consumes programmatic tag from first-fire pin
      await nextTimer();

      geom.scrollTop = 399;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);

      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      expect(geom.scrollTop).toBe(600);
    });

    it('wheel-backed upward scroll after a sticky content-growth pin compares against the new bottom', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      fireScroll(scrollEl); // consumes first-fire programmatic tag
      await nextTimer();

      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000); // sticky pin from 400 -> 600
      expect(geom.scrollTop).toBe(600);
      fireScroll(scrollEl); // tagged programmatic event, should still refresh baseline via write path
      await nextTimer();

      fireWheel(scrollEl, -1, scrollEl);
      geom.scrollTop = 599;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      expect(geom.scrollTop).toBe(599);
    });

  });

  describe('forceStick', () => {
    it('clears escape and writes scrollTop to target', () => {
      controller.setEscapedFromLock(true);
      geom.scrollTop = 100;
      expect(controller.escapedFromLock).toBe(true);

      controller.forceStick();
      expect(controller.escapedFromLock).toBe(false);
      expect(geom.scrollTop).toBe(400);
      expect(controller.isAtBottom).toBe(true);
    });

    it('subsequent contentRO positive deltas sync-pin without rAF gap', async () => {
      // forceStick is the click-to-snap entry; this test pins down the
      // sequel — once forceStick lands at target, subsequent autonomous
      // content growth (streaming, async typesetting) must sync-pin
      // synchronously inside the contentRO callback, with no rAF gap
      // that would let scrollTop fall behind. A regression that
      // re-introduced an autonomous chase loop would let scrollTop trail
      // target by a frame or more between ro.fire and the next
      // nextFrame.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      controller.forceStick();
      expect(geom.scrollTop).toBe(400);

      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      const ro = getRO();
      ro.fire(contentEl, 800); // initialize previousHeight
      ro.fire(contentEl, 1000); // positive delta — sync pin
      expect(geom.scrollTop).toBe(600);

      // No rAF tick advances scrollTop further — there's no chase loop
      // running in the background that could overshoot the sync pin.
      const after = geom.scrollTop;
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(after);
    });
  });

  describe("forceStick reason: 'restore' consent gate", () => {
    it("forceStick({reason:'restore'}) without armRestoreSnap NO-OPs (user escape preserved)", () => {
      // Reproduces the seq-509 trace bug: a stale or duplicated
      // `restoreToBottom()` calling forceStick() AFTER the user has
      // wheel-escaped previously slammed scrollTop to the bottom and
      // cleared escape. With the consent gate, the restore-reason
      // call no longer fires unless the entry point armed consent.
      controller.setEscapedFromLock(true);
      geom.scrollTop = 100;
      expect(controller.escapedFromLock).toBe(true);

      controller.forceStick({ reason: 'restore' });

      expect(controller.escapedFromLock).toBe(true); // unchanged
      expect(geom.scrollTop).toBe(100); // not slammed to 400
    });

    it("forceStick({reason:'restore'}) with armRestoreSnap proceeds", () => {
      // Legitimate thread-switch restore: entry point arms consent,
      // restore $effect's forceStick consumes it.
      controller.setEscapedFromLock(true);
      geom.scrollTop = 100;
      expect(controller.escapedFromLock).toBe(true);

      controller.armRestoreSnap();
      controller.forceStick({ reason: 'restore' });

      expect(controller.escapedFromLock).toBe(false);
      expect(geom.scrollTop).toBe(400); // snapped to target
    });

    it("armRestoreSnap is one-shot: second restore-reason call NO-OPs", () => {
      controller.setEscapedFromLock(true);
      geom.scrollTop = 100;

      controller.armRestoreSnap();
      controller.forceStick({ reason: 'restore' }); // consumes
      expect(geom.scrollTop).toBe(400);

      // Caller re-escapes, then a stale restore fires again.
      controller.setEscapedFromLock(true);
      geom.scrollTop = 100;
      controller.forceStick({ reason: 'restore' });
      expect(controller.escapedFromLock).toBe(true);
      expect(geom.scrollTop).toBe(100);
    });

    it('user-reason forceStick (default) always proceeds AND consumes any pending restore-snap', () => {
      // Chip click / send: explicit user intent always wins. Also
      // clears any pending arm so a follow-up stale restore can't
      // piggy-back on it.
      controller.armRestoreSnap();
      controller.forceStick(); // default reason: 'user'
      expect(geom.scrollTop).toBe(400);

      // Re-escape and verify the arm was consumed.
      controller.setEscapedFromLock(true);
      geom.scrollTop = 100;
      controller.forceStick({ reason: 'restore' });
      expect(controller.escapedFromLock).toBe(true);
      expect(geom.scrollTop).toBe(100);
    });

    it('user wheel-up clears armRestoreSnap (race between arm and restore $effect)', async () => {
      // Realistic race: MessageTimeline's $effect.pre arms consent,
      // then the user wheel-ups before the restore $effect runs (rare
      // since both happen inside the same flush, but possible with an
      // asynchronously dispatched event). The user's gesture must
      // invalidate the consent so the restore NO-OPs.
      controller.armRestoreSnap();
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 350;
      fireScroll(scrollEl);
      await nextTimer();

      const beforeTop = geom.scrollTop;
      controller.forceStick({ reason: 'restore' });
      expect(controller.escapedFromLock).toBe(true); // preserved
      expect(geom.scrollTop).toBe(beforeTop);
    });

    it('keyboard PageUp clears armRestoreSnap after the outer scroller moves', async () => {
      controller.armRestoreSnap();
      fireKey(scrollEl, 'PageUp');
      geom.scrollTop = 350;
      fireScroll(scrollEl);
      await nextTimer();

      controller.forceStick({ reason: 'restore' });
      expect(controller.escapedFromLock).toBe(true);
    });

    it('touch-move-down clears armRestoreSnap after the outer scroller moves', async () => {
      controller.armRestoreSnap();
      fireTouchStart(scrollEl, 100);
      fireTouchMove(scrollEl, 200); // dy=+100, finger down → escape
      geom.scrollTop = 350;
      fireScroll(scrollEl);
      await nextTimer();

      controller.forceStick({ reason: 'restore' });
      expect(controller.escapedFromLock).toBe(true);
    });

    it('detach DELIBERATELY preserves armRestoreSnap (load-bearing for initial-mount path)', () => {
      // attach() calls detach() up-front when scrollEl/contentEl
      // change, including on first mount. If detach cleared the arm,
      // the consumer's `$effect.pre → armRestoreSnap` would be wiped
      // by the immediately-following `$effect → attach` before the
      // restore `$effect → forceStick({reason:'restore'})` could
      // consume it. The flag survives detach; outer-scroll intent and
      // the consume path itself are responsible for clearing it.
      controller.armRestoreSnap();
      controller.detach();
      controller.attach(scrollEl, contentEl);

      geom.scrollTop = 100;
      controller.forceStick({ reason: 'restore' });

      // The arm survived; restore proceeded; user was snapped to bottom.
      expect(controller.escapedFromLock).toBe(false);
      expect(geom.scrollTop).toBe(400);
    });

    it('attach(same scrollEl, same contentEl) preserves armRestoreSnap (real MessageTimeline thread-switch path)', () => {
      // The previous test exercises explicit detach+attach. The real
      // MessageTimeline path is different: scrollEl/contentEl never
      // change across thread switches (the Virtualizer is keyed on
      // threadId but the outer scroll container is not), so attach()
      // takes the early-return branch (`scrollEl === nextScrollEl &&
      // contentEl === nextContentEl`). The arm must survive that path
      // too. A regression that moves the `restoreSnapArmed = false`
      // line into attach()'s early-return would silently break the
      // thread-switch restore for every user. This test pins it down.
      controller.armRestoreSnap();
      controller.attach(scrollEl, contentEl); // SAME refs → early return

      geom.scrollTop = 100;
      controller.forceStick({ reason: 'restore' });

      expect(controller.escapedFromLock).toBe(false);
      expect(geom.scrollTop).toBe(400);
    });

    it('markAtBottom consumes armRestoreSnap (empty-timeline restore branch)', () => {
      // restoreToBottom's empty-timeline branch (chat thread with
      // zero rows) calls markAtBottom() instead of forceStick — but
      // it's still a completed restore and must consume the arm. If
      // not, a later stale path that takes the restore branch could
      // pick up the leftover consent and slam the user.
      controller.armRestoreSnap();
      controller.markAtBottom();

      // Arm consumed; a follow-up restore-stick must NO-OP.
      controller.setEscapedFromLock(true); // user escapes between
      geom.scrollTop = 100;
      controller.forceStick({ reason: 'restore' });

      expect(controller.escapedFromLock).toBe(true);
      expect(geom.scrollTop).toBe(100);
    });

    it('arm → re-arm idempotent (no double-consume risk)', () => {
      controller.armRestoreSnap();
      controller.armRestoreSnap(); // calling twice is a no-op (still one-shot)

      geom.scrollTop = 100;
      // First restore-stick consumes the arm.
      controller.forceStick({ reason: 'restore' });
      expect(geom.scrollTop).toBe(400);

      // Second restore-stick — arm was already consumed by the first
      // call, so this NO-OPs even though we never invoked any user
      // escape path between them.
      geom.scrollTop = 100;
      controller.forceStick({ reason: 'restore' });
      expect(geom.scrollTop).toBe(100);
    });

    it('armRestoreSnap sets the defensive escape (suspends auto-follow until the restore commits)', () => {
      // Both consumers (MessageTimeline's thread-switch $effect.pre,
      // ChannelView's initial-poll setup) always paired a defensive
      // setEscapedFromLock(true) with armRestoreSnap(), in that exact
      // order — the escape clears any prior arm, so arming must come
      // second. The escape is folded into armRestoreSnap so the
      // ordering cannot be gotten wrong. Without it, content growth
      // between arm and restore would sync-pin against the outgoing
      // surface's geometry (the perceived "snap under the cursor").
      // This test also pins the internal order: escape-then-arm. If a
      // regression flipped it, the escape would clear the fresh arm
      // and the restore below would NO-OP.
      expect(controller.escapedFromLock).toBe(false);
      controller.armRestoreSnap();
      expect(controller.escapedFromLock).toBe(true);

      // Auto-follow is suspended: a content-growth nudge does not pin.
      geom.scrollTop = 100;
      geom.scrollHeight = 1100;
      controller.observe('content');
      expect(geom.scrollTop).toBe(100);

      // The armed restore is the single intentional commitment.
      controller.forceStick({ reason: 'restore' });
      expect(controller.escapedFromLock).toBe(false);
      expect(geom.scrollTop).toBe(500); // 1100 - 600
    });
  });

  describe('markAtBottom', () => {
    it('clears escapedFromLock and sets sticky without writing scrollTop', () => {
      // Caller (MessageTimeline bottom-restore) has just landed the user
      // at the geometric bottom via listRef.scrollToIndex(last, 'end').
      // markAtBottom must flip the intent flag WITHOUT also issuing a
      // redundant scrollTop write that would fight the virtualizer's measurement
      // loop still in progress.
      controller.setEscapedFromLock(true);
      geom.scrollTop = 250;
      controller.markAtBottom();
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
      expect(geom.scrollTop).toBe(250); // unchanged
    });

    it('subsequent positive-delta RO fire sync-pins to the new target (streaming follow resumes)', async () => {
      // Re-entry into an active streaming thread: scrollToIndex landed
      // us at the current bottom, markAtBottom flipped intent, then a
      // streaming chunk arrives. The contentRO positive-delta path must
      // sync-pin to the new target so the user follows the live tail
      // without any visible scroll motion.
      controller.setEscapedFromLock(true);
      geom.scrollTop = 400;
      controller.markAtBottom();
      const ro = getRO();
      ro.fire(contentEl, 800); // initialize previousHeight
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000); // positive delta — sync pin
      expect(geom.scrollTop).toBe(600); // single-write convergence
      const after = geom.scrollTop;
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(after);
    });

    it('first contentRO fire BEFORE markAtBottom does not advance scrollTop while escaped', () => {
      // Bottom-restore sequence: $effect.pre sets escape=true, the engine
      // mounts, contentRO observes initial height. The first fire's
      // first-fire branch must NOT snap to target (escape gate). Only
      // then does restoreToBottom call scrollToIndex(last,'end') and
      // markAtBottom. If the first-fire branch leaked through escape,
      // it would snap to a stale target before the engine's measurement
      // loop ran — exactly the bug the new path avoids.
      controller.setEscapedFromLock(true);
      geom.scrollTop = 250;
      const ro = getRO();
      ro.fire(contentEl, 800);
      expect(geom.scrollTop).toBe(250); // first-fire snap suppressed by escape
      controller.markAtBottom();
      expect(controller.escapedFromLock).toBe(false);
      expect(geom.scrollTop).toBe(250); // markAtBottom doesn't write either
    });
  });

  describe('preserveScrollAnchor', () => {
    it('keeps sticky users pinned to bottom across explicit row growth', async () => {
      const anchor = document.createElement('button');
      contentEl.appendChild(anchor);
      let anchorTop = 200;
      vi.spyOn(anchor, 'getBoundingClientRect').mockImplementation(() => ({
        top: anchorTop,
        bottom: anchorTop + 20,
        left: 0,
        right: 100,
        width: 100,
        height: 20,
        x: 0,
        y: anchorTop,
        toJSON: () => ({}),
      } as DOMRect));

      const ro = getRO();
      await controller.preserveScrollAnchor(anchor, () => {
        geom.scrollHeight = 1200;
        geom.contentHeight = 1000;
        anchorTop = 260;
        ro.fire(contentEl, 1000);
      });

      expect(geom.scrollTop).toBe(600);
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('keeps the anchor at the same viewport position for escaped users', async () => {
      const anchor = document.createElement('button');
      contentEl.appendChild(anchor);
      let anchorTop = 200;
      vi.spyOn(anchor, 'getBoundingClientRect').mockImplementation(() => ({
        top: anchorTop,
        bottom: anchorTop + 20,
        left: 0,
        right: 100,
        width: 100,
        height: 20,
        x: 0,
        y: anchorTop,
        toJSON: () => ({}),
      } as DOMRect));

      controller.setEscapedFromLock(true);
      const ro = getRO();
      await controller.preserveScrollAnchor(anchor, () => {
        geom.scrollHeight = 1200;
        geom.contentHeight = 1000;
        anchorTop = 260;
        ro.fire(contentEl, 1000);
      });

      expect(geom.scrollTop).toBe(460);
      expect(controller.escapedFromLock).toBe(true);
    });

    it('releases the scroll pause after the immediate disclosure flush, before slow payload work settles', async () => {
      const anchor = document.createElement('button');
      contentEl.appendChild(anchor);
      let anchorTop = 200;
      vi.spyOn(anchor, 'getBoundingClientRect').mockImplementation(() => ({
        top: anchorTop,
        bottom: anchorTop + 20,
        left: 0,
        right: 100,
        width: 100,
        height: 20,
        x: 0,
        y: anchorTop,
        toJSON: () => ({}),
      } as DOMRect));
      let resolvePayloadWork: (() => void) | undefined;

      const ro = getRO();
      const preserve = controller.preserveScrollAnchor(anchor, () => {
        geom.scrollHeight = 1200;
        geom.contentHeight = 1000;
        anchorTop = 260;
        ro.fire(contentEl, 1000);
        return new Promise<void>((resolve) => {
          resolvePayloadWork = resolve;
        });
      });
      await Promise.resolve();
      await Promise.resolve();

      expect(geom.scrollTop).toBe(600);
      expect(controller.isSticky).toBe(true);

      resolvePayloadWork?.();
      await preserve;
    });
  });

  describe('pauseAutoScroll', () => {
    it('depth-counted; idempotent dispose', async () => {
      const r1 = controller.pauseAutoScroll();
      const r2 = controller.pauseAutoScroll();
      // Even sticky+grew, no scroll happens during lease.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000); // would sync-pin; lease blocks
      const before = geom.scrollTop;
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(before);

      r1();
      r1(); // idempotent
      // Still leased by r2.
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(before);

      r2();
      // Now released — re-pinned synchronously.
      expect(geom.scrollTop).toBe(600);
    });

    it('release does NOT re-pin when escapedFromLock', () => {
      controller.setEscapedFromLock(true);
      geom.scrollTop = 100;
      const release = controller.pauseAutoScroll();
      release();
      expect(geom.scrollTop).toBe(100); // unchanged
    });
  });

  describe("observe('content') (composer-height path)", () => {
    it('writes scrollTop=target when sticky', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // initial

      // Composer grew → scrollHeight grew but contentEl.scrollHeight
      // didn't, so the content RO won't fire. Caller must invoke
      // observe('content') explicitly.
      geom.scrollHeight = 1100;
      controller.observe('content');
      // target = 1100 - 600 = 500.
      expect(geom.scrollTop).toBe(500);
    });

    it('no-op when escaped', () => {
      controller.setEscapedFromLock(true);
      geom.scrollTop = 100;
      geom.scrollHeight = 1100;
      controller.observe('content');
      expect(geom.scrollTop).toBe(100);
    });

    it('no-op when leased', () => {
      const release = controller.pauseAutoScroll();
      geom.scrollHeight = 1100;
      const before = geom.scrollTop;
      controller.observe('content');
      expect(geom.scrollTop).toBe(before);
      release();
    });

    it('subsequent scroll event from layout-flush is treated as RO-correlated, not user-driven', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      geom.scrollHeight = 1100;
      controller.observe('content');
      // Simulate a scroll event firing from the resulting layout flush.
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(false);
    });
  });

  describe('lifecycle', () => {
    it('re-attach is a no-op with same elements', () => {
      const beforeCount = MockResizeObserver.instances.length;
      controller.attach(scrollEl, contentEl);
      expect(MockResizeObserver.instances.length).toBe(beforeCount);
    });

    it('detach disconnects RO + clears handlers', () => {
      controller.detach();
      // Listeners removed: a wheel event must not flip escape.
      fireWheel(scrollEl, -50, scrollEl);
      expect(controller.escapedFromLock).toBe(false);
    });

    it('clears only its own debug stick-state hook on detach', () => {
      setUiRenderTraceEnabled(true);
      try {
        controller.detach();
        controller.attach(scrollEl, contentEl);
        expect(window.__stickState).toBeTypeOf('function');

        controller.detach();
        expect(window.__stickState).toBeUndefined();

        controller.attach(scrollEl, contentEl);
        const replacementHook = () => ({ owner: 'newer-controller' });
        window.__stickState = replacementHook;

        controller.detach();
        expect(window.__stickState).toBe(replacementHook);
      } finally {
        delete window.__stickState;
      }
    });

    it('attach with new elements detaches old listeners', async () => {
      const newScrollEl = document.createElement('div');
      const newContentEl = document.createElement('div');
      newScrollEl.appendChild(newContentEl);
      document.body.appendChild(newScrollEl);
      const newGeom = { scrollHeight: 500, clientHeight: 400, scrollTop: 99, contentHeight: 400 };
      stubGeometry(newScrollEl, newContentEl, newGeom);

      controller.attach(newScrollEl, newContentEl);
      // Wheel on OLD scrollEl should be ignored now.
      fireWheel(scrollEl, -50, scrollEl);
      expect(controller.escapedFromLock).toBe(false);
      // Wheel on NEW scrollEl escapes after its scrollTop moves.
      fireWheel(newScrollEl, -50, newScrollEl);
      newGeom.scrollTop = 49;
      fireScroll(newScrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      newScrollEl.remove();
    });
  });

  describe('architectural invariants', () => {
    // These tests lock in the design choice that distinguishes the
    // unified controller from its predecessors: intent (escapedFromLock,
    // isAtBottomState) is mutated only by explicit signals — input events,
    // forceStick, setEscapedFromLock, and input-backed scroll-handler paths.
    // Pure geometry mutation does not cross the boundary. If a future
    // change reintroduces a bare "scrollTop direction" inference, these
    // tests fail.

    it('mutating scrollTop without firing any event never flips escapedFromLock', () => {
      // Start sticky + at-bottom.
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);

      // Pure geometry mutation: change scrollTop directly. No scroll
      // event, no wheel event, no key, no touch.
      geom.scrollTop = 50;
      // (no fireScroll / fireWheel / fireKey / fireTouchMove here)

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('mutating scrollHeight without firing any event never flips escapedFromLock', () => {
      expect(controller.escapedFromLock).toBe(false);

      // Pure geometry mutation: content extended, but no RO callback,
      // no scroll event triggered. The controller doesn't poll
      // geometry on a timer; it observes signals.
      geom.scrollHeight = 5000;

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('geometric near-bottom alone never flips isAtBottomState true after escape', async () => {
      // Escape explicitly. isAtBottomState is now false; near-bottom is
      // recomputed from geometry on each scroll event.
      controller.setEscapedFromLock(true);
      expect(controller.escapedFromLock).toBe(true);

      // Mutate scrollTop to put us geometrically right at bottom WITHOUT
      // firing a scroll event. The geometric near-bottom would be true
      // if computed, but no signal is firing to recompute it AND the
      // intent flag (isAtBottomState) is governed only by the scroll
      // handler's "user scrolled back" path, the forceStick path, or
      // the content RO's negative-delta restick path — none of which
      // are triggered here.
      geom.scrollTop = 400;

      // Without a scroll event firing the re-stick path, isSticky stays
      // false even though geometrically we'd be at-bottom.
      expect(controller.isSticky).toBe(false);
    });

    it('only an input-backed near-bottom scroll resurrects intent', async () => {
      // Companion to the test above: this proves the design DOES
      // re-stick when the user actually scrolls back, so the previous
      // assertion is about the absence of polling, not a regression.
      controller.setEscapedFromLock(true);
      expect(controller.escapedFromLock).toBe(true);

      // Simulate the user actually moving away and then back to bottom.
      // The re-stick path requires scrollTop to be INCREASING after
      // recent down intent — same-scrollTop scrolls or UP scrolls
      // don't trigger it.
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      fireWheel(scrollEl, 100, scrollEl);
      geom.scrollTop = 400;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });
  });

  describe('scroll-intent regressions — immediate escape and recent-down re-stick', () => {
    // Pins the controller refinement from the 2026-05 gentle-mango
    // plan. Headers below mark which change each test guards; the
    // describe-block name is the anchor referenced from
    // `docs/architecture/frontend-scroll.md` for diagnosing scroll regressions, so the
    // label stays even when the plan is archived.
    //
    //   - Change 1: distFromBottom captured at scroll-event time +
    //     widened auto-follow epsilon (0.5 → 4 px). Bug A defense.
    //   - Change 2: upward input escapes synchronously instead of
    //     waiting for browser-confirmed scrollTop movement.
    //   - Change 3: recent down intent is the only re-stick consent;
    //     layout/applyJump/clamp scrolls cannot re-stick by themselves.
    //   - Change 6: pinch-zoom (wheel + ctrlKey) does not arm intent.

    it('re-stick succeeds when a streaming chunk grows scrollHeight between the scroll event and the deferred check', async () => {
      // Reproduces the Opus-stream regression: user wheels DOWN to
      // the bottom; the synchronous handler captures
      // distFromBottomAtEvent; a contentRO fire BEFORE the 1ms
      // deferred check grows scrollHeight. Pre-fix the deferred
      // re-read of distanceFromBottom() missed the bottom (the bottom
      // moved away in the 1ms window) and re-stick failed. Post-fix
      // the captured value lets re-stick proceed.
      const ro = getRO();
      ro.fire(contentEl, 800);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      fireWheel(scrollEl, 100, scrollEl);
      // target=400 (scrollHeight=1000 - clientHeight=600); landing at
      // 398 is distFromBottom=2, within Change 1's 4 px epsilon.
      geom.scrollTop = 398;
      fireScroll(scrollEl);
      // Sync handler has captured distFromBottomAtEvent=2. Grow
      // scrollHeight before the 1ms deferred fires — the regression
      // timing.
      geom.scrollHeight = 1050;
      geom.contentHeight = 850;
      ro.fire(contentEl, 850);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('re-stick succeeds when scrollTop lands within the 4 px auto-follow epsilon', async () => {
      // The new AUTO_FOLLOW_BOTTOM_EPSILON_PX is 4 (was 0.5). A user
      // who lands 3 px short of the actual bottom (common with estimated
      // row-height estimation + browser scrollTop rounding) re-sticks.
      const ro = getRO();
      ro.fire(contentEl, 800);
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      fireWheel(scrollEl, 100, scrollEl);
      geom.scrollTop = 397; // distFromBottom = 3 (within 4 px epsilon)
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('scrollTop 5 px short of the bottom stays escaped (epsilon-widening guard)', async () => {
      // Not a regression test for any change in the gentle-mango plan
      // — both the old 0.5 px and the new 4 px epsilon reject 5 px.
      // Locks the boundary so a future widening past 5 px lands a
      // failing test before it lands a regression.
      const ro = getRO();
      ro.fire(contentEl, 800);
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      fireWheel(scrollEl, 100, scrollEl);
      geom.scrollTop = 395; // distFromBottom = 5 (outside the epsilon)
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('pointer-tap on the scrollbar escapes immediately and does not safety-net repin', async () => {
      stubScrollbarGutter(scrollEl);

      const ro = getRO();
      ro.fire(contentEl, 800);

      scrollEl.dispatchEvent(
        new PointerEvent('pointerdown', { bubbles: true, isPrimary: true, clientX: 195, clientY: 300 }),
      );

      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      expect(controller.escapedFromLock).toBe(true);
      expect(geom.scrollTop).toBe(400);

      let writesAfterPin = 0;
      Object.defineProperty(scrollEl, 'scrollTop', {
        configurable: true,
        get: () => geom.scrollTop,
        set: (v: number) => {
          writesAfterPin++;
          geom.scrollTop = Math.max(0, Math.min(v, geom.scrollHeight - geom.clientHeight));
        },
      });

      await waitRealMs(180);

      expect(writesAfterPin).toBe(0);
      expect(geom.scrollTop).toBe(400);
    });

    it('contentRO positive-delta does not pin after scrollbar pointer escape', async () => {
      stubScrollbarGutter(scrollEl);

      const ro = getRO();
      ro.fire(contentEl, 800);

      scrollEl.dispatchEvent(
        new PointerEvent('pointerdown', { bubbles: true, isPrimary: true, clientX: 195, clientY: 300 }),
      );

      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      expect(controller.escapedFromLock).toBe(true);
      expect(geom.scrollTop).toBe(400);
    });

    it("observe('content') does not pin after scrollbar pointer escape", async () => {
      stubScrollbarGutter(scrollEl);

      const ro = getRO();
      ro.fire(contentEl, 800);

      scrollEl.dispatchEvent(
        new PointerEvent('pointerdown', { bubbles: true, isPrimary: true, clientX: 195, clientY: 300 }),
      );

      geom.scrollHeight = 1100;
      controller.observe('content');
      expect(controller.escapedFromLock).toBe(true);
      expect(geom.scrollTop).toBe(400);
    });

    it('sub-pixel upward input escapes before browser scroll movement confirms it', async () => {
      // The controller treats the upward wheel itself as intent, so
      // small trackpad deltas do not need to cross a pixel boundary
      // before auto-follow breaks.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 399.5; // sub-pixel trackpad nudge
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);
    });

    it('zero-movement wheel-up escapes immediately', async () => {
      fireWheel(scrollEl, -50, scrollEl);
      fireScroll(scrollEl); // no scrollTop change
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);
    });

    it('native scrollend after wheel-up is ignored after immediate escape', async () => {
      // The controller no longer installs a scrollend listener. A stale
      // native scrollend from prior browser/programmatic motion must not
      // undo or modify the synchronous escape.
      const ro = getRO();
      ro.fire(contentEl, 800);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 350;
      fireScroll(scrollEl);
      fireScrollEnd(scrollEl);
      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);

      // Subsequent contentRO does NOT pin because the user is escaped.
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      expect(geom.scrollTop).toBe(350);
    });

    it('wheel-only escape blocks content growth without a fallback repin', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);

      fireWheel(scrollEl, -50, scrollEl);

      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      expect(geom.scrollTop).toBe(400);

      await waitRealMs(180);
      expect(controller.escapedFromLock).toBe(true);
      expect(geom.scrollTop).toBe(400);

      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      expect(geom.scrollTop).toBe(400);
    });

    it('native scrollend dispatch is ignored', () => {
      // Programmatic writes (spring chase, sync-pin)
      // can fire native scrollend per CSSOM View spec. The controller
      // has no scrollend listener, so the event is inert.
      const before = geom.scrollTop;
      fireScrollEnd(scrollEl);
      expect(geom.scrollTop).toBe(before);
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('pinch-zoom (wheel + ctrlKey) does not arm intent', async () => {
      // Mac trackpad pinch-zoom arrives as `wheel + ctrlKey=true`.
      // handleWheel early-returns on ctrlKey so pinch never escapes.
      //
      // happy-dom 20.9 doesn't honor ctrlKey from WheelEventInit (spike-
      // verified: new WheelEvent('wheel',{ctrlKey:true}).ctrlKey ===
      // undefined), so defineProperty it explicitly.
      const event = new WheelEvent('wheel', { deltaY: -50, bubbles: true });
      Object.defineProperty(event, 'target', { value: scrollEl });
      Object.defineProperty(event, 'ctrlKey', { value: true });
      scrollEl.dispatchEvent(event);

      // No escape intent recorded: a follow-up scroll event with
      // movement does not escape.
      geom.scrollTop = 350;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });
  });
});

// Spring chase + warm-up gate. These tests construct their own controller
// with animationMode='spring' so they can exercise the path that the
// default (no options) controller suppresses.
describe('createUseStickToBottomController — spring chase', () => {
  let scrollEl: HTMLDivElement;
  let contentEl: HTMLDivElement;
  let geom: Geometry;
  let controller: UseStickToBottomController;
  let originalRO: typeof ResizeObserver | undefined;
  let mode: 'spring' | 'instant' = 'spring';

  function getRO(): MockResizeObserver {
    const ro = MockResizeObserver.instances.at(-1);
    if (!ro) throw new Error('no ResizeObserver was created');
    return ro;
  }

  // QUIET_MS is 100ms — past it (so warm fires on the quiet timer).
  // FAILSAFE_MS is 2500ms — past it (so warm fires on the failsafe).
  async function waitMs(ms: number): Promise<void> {
    return new Promise((resolve) => setTimeout(resolve, ms));
  }

  // Bounded "advance until" helper. Replaces the fixed `for (let i = 0;
  // i < 100; i++) await nextFrame()` pattern with an early-exit loop:
  // we stop as soon as the predicate holds (typical case — saves
  // dozens of unneeded frames per test) and throw a useful error if
  // the cap is hit (catches regressions where the spring never
  // arrives). Frame cap matches the slowest historical case (multi-
  // chunk spring); raise per-call if needed.
  async function advanceUntil(predicate: () => boolean, maxFrames = 200): Promise<void> {
    for (let i = 0; i < maxFrames; i++) {
      if (predicate()) return;
      await nextFrame();
    }
    throw new Error(`advanceUntil: predicate not satisfied within ${maxFrames} frames`);
  }

  beforeEach(() => {
    resetScrollIntentModuleStateForTest();
    MockResizeObserver.instances = [];
    originalRO = globalThis.ResizeObserver;
    (globalThis as unknown as { ResizeObserver: typeof MockResizeObserver }).ResizeObserver = MockResizeObserver;
    mockNow = 0;
    vi.spyOn(performance, 'now').mockImplementation(() => mockNow);

    scrollEl = document.createElement('div');
    contentEl = document.createElement('div');
    scrollEl.appendChild(contentEl);
    document.body.appendChild(scrollEl);

    // Geometry: a viewport with room to grow. Initial scrollTop sits
    // exactly at bottom so isAtBottomState stays true on attach.
    geom = { scrollHeight: 1000, clientHeight: 600, scrollTop: 400, contentHeight: 800 };
    stubGeometry(scrollEl, contentEl, geom);

    mode = 'spring';
    controller = createUseStickToBottomController({
      animationMode: () => mode,
    });
    controller.attach(scrollEl, contentEl);
  });

  afterEach(() => {
    controller.detach();
    scrollEl.remove();
    if (originalRO) {
      (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver = originalRO;
    }
    vi.restoreAllMocks();
  });

  // The cold-thread-switch flicker: after the controller has pinned the DOM
  // to true bottom, the engine reports an anchor-preserving compensation
  // BELOW bottom (its delta only compensates above-viewport remeasure,
  // not at/below-fold growth). The routed compensation must be redirected to
  // true bottom instead of painting one frame short. These lock the resolver
  // DECISION as exercised through the controller's applier entry point; the
  // actual no-flicker is only observable in the real app (happy-dom has no
  // layout), so the assertions are on where the redirected write lands.
  describe('engine compensation anchor redirect (routed)', () => {
    it('redirects a stale below-bottom compensation to true bottom (instant mode, dormant)', async () => {
      // Idle cold-switch thread is animationMode==='instant' with no spring
      // in flight; without the redirect the compensation resolves through
      // the dormant pass tier and the short frame lands.
      mode = 'instant';
      const ro = getRO();
      ro.fire(contentEl, 800); // initial; warm still false
      await waitMs(150);
      expect(controller.isWarm).toBe(true);
      // Content grows; the controller sync-pins the DOM to the new bottom.
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      expect(geom.scrollTop).toBe(600); // DOM at true bottom → domAlreadyPinned
      // The engine now reports a stale anchor 390px short.
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: -390, target: 210 })).toBe(true);
      // Redirected to true bottom, not left at the stale 210.
      expect(geom.scrollTop).toBe(600);
    });

    it('redirects a stale below-bottom compensation to true bottom (spring mode, no chase)', async () => {
      // mode='spring' with no chase in flight (springToken===0) would exit
      // through the resolver's final pass tier — the second exit the
      // redirect must also cover.
      mode = 'spring';
      const ro = getRO();
      ro.fire(contentEl, 800); // initial; DOM already at bottom (400)
      await waitMs(150);
      expect(controller.isWarm).toBe(true);
      expect(geom.scrollTop).toBe(400); // at bottom, no spring engaged
      // Engine stale anchor, 350px short.
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: -350, target: 50 })).toBe(true);
      expect(geom.scrollTop).toBe(400); // redirected to bottom
    });

    it('does NOT redirect when the DOM is not yet pinned to bottom (legit compensation passes)', async () => {
      // Mid-cascade: the controller has not pinned yet, so the DOM sits below
      // bottom. The engine's compensation here is legitimate above-viewport
      // anchoring and must land untouched — the redirect is narrow to
      // domAlreadyPinned.
      mode = 'instant';
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      expect(controller.isWarm).toBe(true);
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      geom.scrollTop = 300; // position DOM below bottom (600) directly
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: 50, target: 350 })).toBe(true);
      expect(geom.scrollTop).toBe(350); // not redirected — lands as the engine asked
    });

    it('does NOT redirect a compensation that stops just short of the epsilon band', async () => {
      // Boundary: a target exactly AUTO_FOLLOW_BOTTOM_EPSILON_PX (4px) short
      // of bottom is NOT "moving away" (movesAwayFromBottom is strict
      // `> epsilon`), so it must pass through, not snap to bottom. Pins that
      // threshold — without it a predicate mutation to always-true survives.
      mode = 'instant';
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      expect(geom.scrollTop).toBe(600); // pinned at true bottom
      // 4px short — exactly the epsilon boundary.
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: -4, target: 596 })).toBe(true);
      expect(geom.scrollTop).toBe(596); // passes through, not redirected to 600
    });
  });

  describe('warm-up gate', () => {
    it('sync-pins (no spring) during the warm-up window even when mode=spring', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // initial; warm is still false
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      // Sync-pin: scrollTop already at the new target inside the RO callback.
      expect(geom.scrollTop).toBe(600);
      // No spring rAF was scheduled. Advance frames; scrollTop is stable.
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(600);
    });

    it('exposes isWarm=false during warm-up, isWarm=true after quiet timer fires', async () => {
      // Consumers (chat's MessageTimeline) read isWarm to hide
      // contentEl during the measurement cascade on uncached loads.
      // The signal must be false at attach and after forceStick, and
      // must flip true exactly when the controller decides
      // measurements have settled.
      expect(controller.isWarm).toBe(false);
      const ro = getRO();
      ro.fire(contentEl, 800);
      // Still warming — quiet timer not yet fired.
      expect(controller.isWarm).toBe(false);
      await waitMs(150);
      expect(controller.isWarm).toBe(true);
    });

    it('user forceStick keeps already-warm content visible', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      expect(controller.isWarm).toBe(true);

      controller.forceStick();
      expect(controller.isWarm).toBe(true);
    });

    it('restore forceStick resets isWarm back to false', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      expect(controller.isWarm).toBe(true);

      controller.armRestoreSnap();
      controller.forceStick({ reason: 'restore' });
      expect(controller.isWarm).toBe(false);

      // The quiet timer is gated on contentRO evidence — without an RO
      // fire, isWarm stays false. Fire a second RO to start the next
      // cascade window, then wait past QUIET_MS for warm to flip back.
      ro.fire(contentEl, 810);
      await waitMs(150);
      expect(controller.isWarm).toBe(true);
    });

    it('detach resets isWarm', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      expect(controller.isWarm).toBe(true);

      controller.detach();
      expect(controller.isWarm).toBe(false);
    });

    it('armWarmup() flips isWarm back to false WITHOUT writing scrollTop or clearing escape', async () => {
      // Public-API counterpart of attach()/restore forceStick's internal
      // warm-gate re-arm. Used by MessageTimeline's $effect.pre on
      // thread switch — by the time forceStick() runs in $effect, the
      // DOM has already flushed with the OLD thread's settled
      // isWarm=true, so the cascade-hide gate stays open during the
      // new thread's first paint. armWarmup() closes the gate
      // synchronously without touching scroll geometry or escape
      // flags, so the next DOM flush sees isWarm=false.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      expect(controller.isWarm).toBe(true);

      // Set up a known geometry + escape state to confirm armWarmup
      // doesn't touch them.
      controller.setEscapedFromLock(true);
      geom.scrollTop = 123;
      expect(controller.escapedFromLock).toBe(true);

      controller.armWarmup();

      expect(controller.isWarm).toBe(false);
      expect(controller.escapedFromLock).toBe(true);
      expect(geom.scrollTop).toBe(123);

      // The quiet timer is gated on contentRO evidence — without an RO
      // fire, isWarm stays false (only the FAILSAFE_MS=2500ms ceiling
      // would trip it, which we don't wait for here).
      await waitMs(150);
      expect(controller.isWarm).toBe(false);

      // Once an RO fires (cascade kicks off), the quiet timer arms;
      // QUIET_MS later, warm flips true.
      ro.fire(contentEl, 800);
      await waitMs(150);
      expect(controller.isWarm).toBe(true);
    });

    it('armWarmup() called twice in close succession holds the gate closed; only a final RO + quiet window flips warm', async () => {
      // beginWarmup no longer arms the quiet timer eagerly — repeated
      // armWarmup() calls cannot, by themselves, ever flip warm true;
      // only a contentRO event followed by QUIET_MS of silence can.
      // This test pins that guarantee: across two armWarmup() bursts
      // and 250ms of wall clock with no RO, warm stays false.
      controller.armWarmup();
      expect(controller.isWarm).toBe(false);
      await waitMs(50);
      controller.armWarmup();
      expect(controller.isWarm).toBe(false);
      await waitMs(200);
      expect(controller.isWarm).toBe(false);

      // RO fires; QUIET_MS=100ms later, warm flips.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      expect(controller.isWarm).toBe(true);
    });

    it('beginWarmup with no contentRO activity keeps isWarm=false until FAILSAFE_MS', async () => {
      // Regression: the original "half-screen-high" symptom on uncached
      // long-thread re-entry. Sequence: $effect.pre armWarmup arms the
      // gate; slice fetch is in flight, MessageTimeline renders the
      // loading-spinner / empty branch so contentEl is not yet mounted
      // and no RO can fire. Before the slice arrives, the old
      // beginWarmup armed quietTimer eagerly — it would fire at t=100ms,
      // warm=true, hideContentForWarmup=false. Once items finally
      // arrived and contentEl mounted, the cascade was visible and
      // scrollTop landed at the estimated bottom (not the measured
      // bottom). The fix: beginWarmup only arms the failsafe; quietTimer
      // is gated on actual RO evidence.
      controller.armWarmup();
      // 150ms with no RO — would have flipped warm under the old policy.
      await waitMs(150);
      expect(controller.isWarm).toBe(false);
      // The failsafe is still in place; we don't wait the full 2.5s here,
      // we just confirm the gate held through the original QUIET_MS window.
    });

    it('quiescence: warms after QUIET_MS of no contentRO fires', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      // Wait past the quiet window. Real timers — happy-dom runs them.
      await waitMs(150);

      // Next positive delta should kick the spring instead of sync-pinning.
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);

      // Spring path: scrollTop didn't land instantly at the new target.
      // It's interpolating across rAF ticks.
      expect(geom.scrollTop).toBeLessThan(600);
      expect(geom.scrollTop).toBeGreaterThanOrEqual(400);
    });

    it('failsafe: warms after FAILSAFE_MS of continuous contentRO fires', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);

      // Fire small deltas continuously to keep the quiet timer reset.
      // Each fire bumps the quiet timer; only the failsafe can rescue us.
      for (let i = 1; i <= 30; i++) {
        geom.scrollHeight = 1000 + i;
        geom.contentHeight = 800 + i;
        ro.fire(contentEl, 800 + i);
        await waitMs(80); // < QUIET_MS, keeps the quiet timer alive
      }
      // After ~2.4s of fires, the failsafe (2.5s) is about to fire.
      // Push past it.
      await waitMs(250);

      // Subsequent positive delta now goes through the spring path.
      geom.scrollHeight = 1500;
      geom.contentHeight = 1300;
      ro.fire(contentEl, 1300);

      expect(geom.scrollTop).toBeLessThan(900); // target - clientHeight = 900
    }, 5000);

    it('restore forceStick re-arms the warm gate so post-restore settle stays silent', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      // Cross the quiet window so we're warm.
      await waitMs(150);

      // Re-call forceStick with restore consent — simulates thread restore.
      controller.armRestoreSnap();
      controller.forceStick({ reason: 'restore' });

      // Mount-time settling immediately after restore: positive delta
      // should sync-pin, NOT spring.
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      // Sync-pin landed scrollTop at target in the same callback.
      expect(geom.scrollTop).toBe(600);
    });

    it('negative delta while !warm sync-pins via negative-delta branch (Bug A defense)', async () => {
      // Bug A's cascade defense relies on the negative-delta sync-pin
      // running synchronously when the engine's row-remeasurement shifts
      // scrollTop off the bottom. The spring carve-out (suppress
      // writeScrollTop while springToken !== 0) must NOT weaken this
      // defense. Warm-gate-ordering invariant: cascade fires while
      // `!warm`, springGateOpen requires `warm`, so the spring never
      // starts during the cascade and springToken stays 0 — the
      // negative-pin sync write runs as it always did.
      //
      // Geometry is chosen so the `contentRO.overshoot` write site
      // does NOT fire, isolating the negative-delta sync-pin as the
      // only possible writer of the asserted scrollTop change: scrollTop
      // must be BELOW the new target after the shrink, so
      // `scrollTop > target` is false and overshoot is bypassed.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial; warm is still false. isAtBottomState=true from attach.

      // Simulate a routed compensation shifting scrollTop off the bottom
      // (the cascade pattern: a row above the viewport remeasured
      // larger, the compensation moved the visible row's offset down to
      // preserve it). No scroll event fires — isAtBottomState stays
      // true even though we're now 100 px above the geometric bottom.
      geom.scrollTop = 300;

      // Content shrinks: scrollHeight 1000 -> 940. New target = 340.
      // scrollTop (300) is BELOW the new target (340), so overshoot
      // is false. The only path that can land scrollTop at 340 in
      // this tick is the negative-delta sync-pin.
      geom.scrollHeight = 940;
      geom.contentHeight = 740;
      ro.fire(contentEl, 740);
      expect(geom.scrollTop).toBe(340);
    });
  });

  describe('warm-up gate — quietContextSignal', () => {
    // These tests construct their own controller because they need to
    // pass a quietContextSignal option that the outer beforeEach's
    // controller does not provide.

    let localScrollEl: HTMLDivElement;
    let localContentEl: HTMLDivElement;
    let localGeom: Geometry;
    let localController: UseStickToBottomController;
    let settled = false;

    function getLocalRO(): MockResizeObserver {
      const ro = MockResizeObserver.instances.at(-1);
      if (!ro) throw new Error('no ResizeObserver was created');
      return ro;
    }

    // Returns the options object the controller was constructed with so
    // tests can pin the live-read contract (assigning quietContextSignal
    // after construction must be honored).
    function buildLocalController(
      opts: { signal: (() => boolean) | undefined },
    ): UseStickToBottomOptions {
      localScrollEl = document.createElement('div');
      localContentEl = document.createElement('div');
      localScrollEl.appendChild(localContentEl);
      document.body.appendChild(localScrollEl);
      localGeom = { scrollHeight: 1000, clientHeight: 600, scrollTop: 400, contentHeight: 800 };
      stubGeometry(localScrollEl, localContentEl, localGeom);
      const options: UseStickToBottomOptions = {
        animationMode: () => 'instant',
        quietContextSignal: opts.signal,
      };
      localController = createUseStickToBottomController(options);
      localController.attach(localScrollEl, localContentEl);
      return options;
    }

    beforeEach(() => {
      settled = false;
    });

    afterEach(() => {
      localController?.detach();
      localScrollEl?.remove();
    });

    it('shortens the quiet window once geometry holds still and the signal is on', async () => {
      // The shortened SETTLED_QUIET_MS (16ms) window is only used once the
      // surface has stopped moving. The FIRST fire has no baseline, so it
      // is treated as still-moving and holds the conservative QUIET_MS
      // (100ms) window; a subsequent small (≤ WARMUP_SETTLE_EPSILON_PX)
      // delta with the signal on then admits the shortcut.
      settled = true;
      buildLocalController({ signal: () => settled });
      const ro = getLocalRO();
      ro.fire(localContentEl, 800); // first fire — unknown geometry
      // 30ms is past SETTLED_QUIET_MS (16ms) but short of QUIET_MS (100ms):
      // a buggy first-fire shortcut would already be warm here.
      await waitMs(30);
      expect(localController.isWarm).toBe(false);
      ro.fire(localContentEl, 804); // +4px — surface settled, signal on
      // 45ms clears SETTLED_QUIET_MS (16ms) with real-timer slack, still
      // well under QUIET_MS (100ms) so a conservative-window regression fails.
      await waitMs(45);
      expect(localController.isWarm).toBe(true);
    });

    it('reads quietContextSignal live off the options object, not a construction-time snapshot', async () => {
      // Pre-split behavior pin: bumpQuietTimer / notifyQuietContextSignalChanged
      // read `options.quietContextSignal` at call time, so a consumer that
      // assigns the option after constructing the controller is honored. A
      // construction-time snapshot would treat the late-assigned signal as
      // absent and warm on the unconditional QUIET_MS path even while the
      // consumer reports "not settled".
      const options = buildLocalController({ signal: undefined });
      options.quietContextSignal = () => false;
      getLocalRO().fire(localContentEl, 800);
      // Past QUIET_MS (100ms): the not-settled signal must hold warm-up
      // open (only the FAILSAFE_MS ceiling may warm it now).
      await waitMs(130);
      expect(localController.isWarm).toBe(false);
      // Flip the same late-assigned option to settled: the notify path
      // must also read it live and arm the quiet window.
      options.quietContextSignal = () => true;
      localController.notifyQuietContextSignalChanged();
      await waitMs(130);
      expect(localController.isWarm).toBe(true);
    });

    it('keeps warm false through an estimate→measure cascade even with the settle signal on', async () => {
      // Idle-thread flicker regression: the engine mounts rows at the
      // ESTIMATED_ROW_SIZE then corrects to measured heights over a series
      // of contentRO fires spaced wider than SETTLED_QUIET_MS. With the
      // settle signal already on (svelte-streamdown idle for an
      // already-rendered thread), a shortened window would fire in the gap
      // between two corrections and reveal a still-growing surface. Warm
      // must stay false until the cascade goes quiet — geometry, not the
      // settle signal, gates the reveal.
      settled = true;
      buildLocalController({ signal: () => settled });
      const ro = getLocalRO();
      // Mount estimate, then large corrections ~30ms apart (each well past
      // SETTLED_QUIET_MS=16ms). After each, warm must still be false.
      ro.fire(localContentEl, 1000);
      await waitMs(30);
      expect(localController.isWarm).toBe(false);
      ro.fire(localContentEl, 2000);
      await waitMs(30);
      expect(localController.isWarm).toBe(false);
      ro.fire(localContentEl, 3000);
      await waitMs(30);
      expect(localController.isWarm).toBe(false);
      // Cascade goes quiet → warm reveals after the conservative QUIET_MS
      // RO-silence window (geometry-driven, not a cascade-duration guess).
      await waitMs(150);
      expect(localController.isWarm).toBe(true);
    });

    it('a width-only (height delta 0) reflow between cascade steps does not admit the short window', async () => {
      // Cold-boot residual: a padding-var / width-only reflow fires a
      // contentRO with height delta exactly 0. It carries no new height
      // information, so it must NOT reset geometry-stability to "settled"
      // mid-cascade — otherwise the shortened window fires before the next
      // correction lands. (The contentRO's own delta===0 early-out runs
      // AFTER the quiet-timer bump, so this RO does reach the gate.) The
      // gate keeps the prior large magnitude → stays conservative.
      settled = true;
      buildLocalController({ signal: () => settled });
      const ro = getLocalRO();
      ro.fire(localContentEl, 1000); // mount estimate (first fire, no baseline)
      await waitMs(30);
      expect(localController.isWarm).toBe(false);
      ro.fire(localContentEl, 2000); // +1000 cascade correction (still moving)
      ro.fire(localContentEl, 2000); // width-only reflow: delta 0, no height info
      // 30ms is past SETTLED_QUIET_MS (16ms): a delta-0 RO that wrongly
      // counted as "settled" would have already revealed here.
      await waitMs(30);
      expect(localController.isWarm).toBe(false);
      // Cascade quiet → conservative QUIET_MS window reveals.
      await waitMs(150);
      expect(localController.isWarm).toBe(true);
    });

    it('signal falsy at first RO fire blocks quiet timer until settled', async () => {
      buildLocalController({ signal: () => settled });
      const ro = getLocalRO();
      ro.fire(localContentEl, 800);
      await waitMs(150);
      // Signal still false → no quiet timer armed → warm stays false.
      expect(localController.isWarm).toBe(false);

      // Flip signal; notify arms the quiet timer. Only the first (baseline)
      // fire has happened, so geometry is still unknown — the conservative
      // QUIET_MS window applies, not the shortened one.
      settled = true;
      localController.notifyQuietContextSignalChanged();
      await waitMs(120);
      expect(localController.isWarm).toBe(true);
    });

    it('notifyQuietContextSignalChanged arms timer after RO evidence', async () => {
      // RO fires while signal is false → no timer. Settled fires later
      // → notify arms the quiet timer from that moment. Only the baseline
      // fire happened, so geometry is unknown → conservative QUIET_MS.
      buildLocalController({ signal: () => settled });
      const ro = getLocalRO();
      ro.fire(localContentEl, 800);
      await waitMs(50);
      expect(localController.isWarm).toBe(false);

      settled = true;
      localController.notifyQuietContextSignalChanged();
      await waitMs(120);
      expect(localController.isWarm).toBe(true);
    });

    it('notifyQuietContextSignalChanged is a no-op when signal is still falsy', async () => {
      buildLocalController({ signal: () => settled });
      const ro = getLocalRO();
      ro.fire(localContentEl, 800);
      await waitMs(50);
      expect(localController.isWarm).toBe(false);

      // Signal remains false; notify is a no-op — no timer armed.
      localController.notifyQuietContextSignalChanged();
      await waitMs(150);
      // No quiet timer ever armed → warm stays false (only failsafe
      // could fire, which is 2500ms out).
      expect(localController.isWarm).toBe(false);
    });

    it('notifyQuietContextSignalChanged is a no-op when already warm', async () => {
      // Start with signal true so the quiet timer can fire and warm. The
      // single baseline fire holds the conservative QUIET_MS window.
      settled = true;
      buildLocalController({ signal: () => settled });
      const ro = getLocalRO();
      ro.fire(localContentEl, 800);
      await waitMs(120);
      expect(localController.isWarm).toBe(true);

      // Notify after warm should be a safe no-op.
      expect(() => localController.notifyQuietContextSignalChanged()).not.toThrow();
      expect(localController.isWarm).toBe(true);
    });

    it('notifyQuietContextSignalChanged is a no-op when no quiet timer is armed', async () => {
      // beginWarmup only arms the failsafe; the quiet timer arms on
      // the first contentRO event. If the consumer notifies before any
      // RO fired (e.g. signal flips while the slice is still loading
      // and contentEl is unmounted), the call must be safe.
      buildLocalController({ signal: () => settled });
      settled = true;
      expect(() => localController.notifyQuietContextSignalChanged()).not.toThrow();
      expect(localController.isWarm).toBe(false);
    });

    it('armWarmup() re-armed cascade: signal toggled mid-cycle does not leak across re-arm', async () => {
      // First cycle: signal true → the baseline fire warms after the
      // conservative QUIET_MS window (geometry not yet proven stable).
      settled = true;
      buildLocalController({ signal: () => settled });
      const ro = getLocalRO();
      ro.fire(localContentEl, 800);
      await waitMs(120);
      expect(localController.isWarm).toBe(true);

      // Second cycle: armWarmup() drops warm. Reset signal to false
      // (mirrors MessageTimeline's armWarmupWithReset). New RO fires
      // but quiet timer won't arm — warm stays false until signal
      // flips or failsafe fires.
      settled = false;
      localController.armWarmup();
      expect(localController.isWarm).toBe(false);
      ro.fire(localContentEl, 810);
      await waitMs(150);
      expect(localController.isWarm).toBe(false);

      // Signal flips → notify arms the quiet timer. The last RO delta
      // (10px > WARMUP_SETTLE_EPSILON_PX) leaves geometry "still moving",
      // so the conservative QUIET_MS window applies.
      settled = true;
      localController.notifyQuietContextSignalChanged();
      await waitMs(120);
      expect(localController.isWarm).toBe(true);
    });

    it('notifyQuietContextSignalChanged holds the conservative window when geometry is still moving', async () => {
      // The notify arming site must pick its window by geometry too: when
      // the settle signal flips on mid-cascade (large last delta), it must
      // NOT take the shortened window. A regression hardcoding
      // SETTLED_QUIET_MS here passes every end-state test (warm eventually
      // true) — this pins the INTERMEDIATE hold that distinguishes the two.
      // signal starts false → the cascade fires arm no quiet timer, but
      // they still record geometry as moving.
      buildLocalController({ signal: () => settled });
      const ro = getLocalRO();
      ro.fire(localContentEl, 1000); // first fire — no baseline, signal off
      ro.fire(localContentEl, 2200); // +1200 cascade step (geometry moving), signal off
      await waitMs(30);
      expect(localController.isWarm).toBe(false); // no timer armed while signal off
      // Settle signal flips on → notify arms the quiet timer. Last delta
      // 1200 > WARMUP_SETTLE_EPSILON_PX → conservative QUIET_MS, not 16ms.
      settled = true;
      localController.notifyQuietContextSignalChanged();
      // 30ms is past SETTLED_QUIET_MS (16ms) but short of QUIET_MS (100ms):
      // a shortened-window regression would already be warm here.
      await waitMs(30);
      expect(localController.isWarm).toBe(false);
      await waitMs(150);
      expect(localController.isWarm).toBe(true);
    });
  });

  describe('spring chase', () => {
    it('engages on positive delta when warm AND mode=spring', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);

      // First spring tick has not yet run; scrollTop unchanged.
      expect(geom.scrollTop).toBe(400);

      // Advance frames; spring should interpolate scrollTop toward 800
      // (= 1400 - 600).
      for (let i = 0; i < 3; i++) await nextFrame();
      expect(geom.scrollTop).toBeGreaterThan(400);
      expect(geom.scrollTop).toBeLessThan(800);
    });

    it('dropped frames keep the same broad motion shape', async () => {
      // Same chase twice: steady 60Hz vs 30Hz (every gap a dropped
      // frame). Sampled at equal wall-clock points the trajectories
      // must match — the old integrator (one velocity update per rAF,
      // position advanced by velocity·dt) converged measurably slower
      // per second at 30Hz, so the chase changed shape exactly when the
      // app was already stuttering.
      //
      // Time alignment: sample both runs at roughly the same wall-clock
      // points. The dropped-frame run integrates multiple bounded internal
      // steps in one rAF, so it should stay close to the steady run without
      // needing byte-identical positions.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);

      const at60: number[] = [];
      await nextFrame();
      at60.push(geom.scrollTop);
      for (let i = 0; i < 6; i++) {
        await nextFrame();
        await nextFrame();
        at60.push(geom.scrollTop);
      }

      // Fresh controller + geometry for the dropped-frames run.
      controller.detach();
      resetScrollIntentModuleStateForTest();
      MockResizeObserver.instances = [];
      controller = createUseStickToBottomController({ animationMode: () => mode });
      geom.scrollHeight = 1000;
      geom.contentHeight = 800;
      geom.scrollTop = 400;
      controller.attach(scrollEl, contentEl);
      const ro2 = getRO();
      ro2.fire(contentEl, 800);
      await waitMs(150); // warm

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro2.fire(contentEl, 1200);

      const at30: number[] = [];
      for (let i = 0; i < 7; i++) {
        await nextFrameAfter(33.34);
        at30.push(geom.scrollTop);
      }

      expect(at60[0]).toBeGreaterThan(400); // both chases actually moved
      for (let i = 0; i < at60.length; i++) {
        expect(Math.abs(at60[i] - at30[i])).toBeLessThan(25);
      }
    });

    it('a long rAF stall integrates a bounded catch-up burst, not the whole gap', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);

      await nextFrame(); // first tick: one step
      const afterFirst = geom.scrollTop;
      expect(afterFirst).toBeGreaterThan(400);

      // 200ms stall ≈ 12 dropped frames. rAF callbacks run in registration
      // order, so the spring tick in a flush executes BEFORE the helper
      // advances mockNow — the stalled delta is observed by the tick in the
      // FOLLOWING frame, hence the extra plain nextFrame before sampling.
      // The cap must prevent paying the whole gap in one visible lurch.
      await nextFrameAfter(200);
      await nextFrame();
      const afterStall = geom.scrollTop;
      expect(afterStall).toBeGreaterThan(afterFirst);
      expect(afterStall).toBeLessThan(600);
    });

    it('advances every sub-step frame at 120Hz', async () => {
      // At ~120Hz each rAF is ~8.33ms. The spring must still write a
      // fractional step every frame; holding alternate frames is visible
      // stair-stepping on ProMotion displays.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);

      const samples: number[] = [];
      for (let i = 0; i < 14; i++) {
        await nextFrameAfter(1000 / 120); // ~8.33ms
        samples.push(geom.scrollTop);
      }

      // The chase really was animating (net forward progress)…
      expect(samples[samples.length - 1]).toBeGreaterThan(samples[0]);
      // …and stayed below the 800px target the whole window, so no clamp
      // collision can fake a monotonic run.
      expect(samples[samples.length - 1]).toBeLessThan(800);
      for (let i = 1; i < samples.length; i += 1) {
        expect(samples[i]).toBeGreaterThan(samples[i - 1]);
      }
    });

    it('sync-pins a positive delta caused by content width reflow instead of spring-chasing it', async () => {
      // Mermaid diagrams rendered with useMaxWidth can change height when
      // the pane width changes. If that row is still mounted in the engine's
      // overscan window while live content recently advanced, the spring
      // latch still reports "spring" even though the resize is layout
      // correction, not new assistant output. That must land instantly;
      // otherwise the user watches the viewport chase a stale bottom by
      // a third or half screen.
      const ro = getRO();
      ro.fire(contentEl, 800, 900);
      await waitMs(150); // warm

      geom.scrollHeight = 1300;
      geom.contentHeight = 1100;
      ro.fire(contentEl, 1100, 640);

      expect(geom.scrollTop).toBe(700);
      for (let i = 0; i < 3; i++) await nextFrame();
      expect(geom.scrollTop).toBe(700);
    });

    it('sync-pins height correction when width and height reflow arrive in separate ResizeObserver deliveries', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800, 900);
      await waitMs(150); // warm

      // Width-only delivery first. This happens on some pane/window
      // reflows before async renderer layout reports the new height.
      ro.fire(contentEl, 800, 640);

      geom.scrollHeight = 1300;
      geom.contentHeight = 1100;
      ro.fire(contentEl, 1100, 640);

      expect(geom.scrollTop).toBe(700);
      for (let i = 0; i < 3; i++) await nextFrame();
      expect(geom.scrollTop).toBe(700);
    });

    it('springs same-width live growth after the width-reflow settle window expires', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800, 900);
      await waitMs(150); // warm

      ro.fire(contentEl, 800, 640);
      for (let i = 0; i < 20; i++) await nextFrame();

      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000, 640);

      expect(geom.scrollTop).toBe(400);
      await nextFrame();
      expect(geom.scrollTop).toBeGreaterThan(400);
      expect(geom.scrollTop).toBeLessThan(600);
    });

    it('sync-pins a negative delta caused by content width reflow even during an active spring', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800, 900);
      await waitMs(150); // warm

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200, 900);
      await nextFrame();
      expect(geom.scrollTop).toBeGreaterThan(400);
      expect(geom.scrollTop).toBeLessThan(800);

      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000, 640);

      expect(geom.scrollTop).toBe(600);
      for (let i = 0; i < 3; i++) await nextFrame();
      expect(geom.scrollTop).toBe(600);
    });

    it("observe('live-content') spring-chases when warm AND mode=spring", async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      controller.observe('live-content');

      // Live-content nudge should not sync-pin to the new target. It uses
      // the same spring policy as contentRO positive deltas.
      expect(geom.scrollTop).toBe(400);

      for (let i = 0; i < 3; i++) await nextFrame();
      expect(geom.scrollTop).toBeGreaterThan(400);
      expect(geom.scrollTop).toBeLessThan(800);
    });

    it("observe('live-content') sync-pins when animation mode is instant", async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      mode = 'instant';

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      controller.observe('live-content');

      expect(geom.scrollTop).toBe(800);
      for (let i = 0; i < 3; i++) await nextFrame();
      expect(geom.scrollTop).toBe(800);
    });

    // The two 'composer-geometry' tests pin the observe() kind→path
    // mapping itself: ChatView reports composer growth with this kind
    // precisely so active output can spring through an activity-rail
    // height change (chat AGENTS.md operational rule). Remapping the
    // kind to the instant path would pass every component-level test
    // (they assert only the kind string at the pane adapter) but break
    // that behavior — these fail instead.
    it("observe('composer-geometry') spring-chases when warm AND mode=spring", async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      controller.observe('composer-geometry');

      expect(geom.scrollTop).toBe(400);

      for (let i = 0; i < 3; i++) await nextFrame();
      expect(geom.scrollTop).toBeGreaterThan(400);
      expect(geom.scrollTop).toBeLessThan(800);
    });

    it("observe('composer-geometry') sync-pins when animation mode is instant", async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      mode = 'instant';

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      controller.observe('composer-geometry');

      expect(geom.scrollTop).toBe(800);
      for (let i = 0; i < 3; i++) await nextFrame();
      expect(geom.scrollTop).toBe(800);
    });

    it('structural append mark spring-chases the next positive delta even when animation mode is instant', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      mode = 'instant';

      controller.markStructuralContentPending();
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);

      expect(geom.scrollTop).toBe(400);
      await nextFrame();
      expect(geom.scrollTop).toBeGreaterThan(400);
      expect(geom.scrollTop).toBeLessThan(600);
    });

    it('structural append spring cancels after arrival in instant mode', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      mode = 'instant';

      controller.markStructuralContentPending();
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);

      await advanceUntil(() => Math.abs(geom.scrollTop - 600) <= 1);
      while (mockNow < 360) await nextFrame();

      // Spring canceled (instant mode never enters the sentinel), so a
      // routed compensation resolves through the pass tier and lands.
      geom.scrollHeight = 1300;
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: 50, target: 650 })).toBe(true);

      expect(geom.scrollTop).toBe(650);
    });

    it('structural append spring absorbs a quick measured-height correction', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      mode = 'instant';

      controller.markStructuralContentPending();
      geom.scrollHeight = 1250;
      geom.contentHeight = 1050;
      ro.fire(contentEl, 1050);
      expect(geom.scrollTop).toBe(400);

      // The command row's estimate is corrected before the first spring
      // paint. The correction must not sync-write to the corrected bottom;
      // the active spring should read the new target and move there.
      geom.scrollHeight = 1180;
      geom.contentHeight = 980;
      ro.fire(contentEl, 980);
      expect(geom.scrollTop).toBe(400);

      await nextFrame();
      expect(geom.scrollTop).toBeGreaterThan(400);
      expect(geom.scrollTop).toBeLessThan(580);
      await advanceUntil(() => Math.abs(geom.scrollTop - 580) <= 1);
    });

    it("observe('live-content') clamps instead of springing when already past target", async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Simulate a structural live-content nudge after content shrank or
      // the browser left scrollTop beyond the new bottom. A spring cannot
      // advance because current >= target, so the hook must use the
      // instant clamp path.
      geom.scrollHeight = 950;
      geom.contentHeight = 750;
      controller.observe('live-content');

      expect(geom.scrollTop).toBe(350);
      for (let i = 0; i < 3; i++) await nextFrame();
      expect(geom.scrollTop).toBe(350);
    });

    it("observe('live-content') does nothing while escaped", async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      controller.setEscapedFromLock(true);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      controller.observe('live-content');

      expect(geom.scrollTop).toBe(400);
      for (let i = 0; i < 3; i++) await nextFrame();
      expect(geom.scrollTop).toBe(400);
    });

    it("observe('live-content') does nothing while auto-scroll is paused", async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      const release = controller.pauseAutoScroll();

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      controller.observe('live-content');

      expect(geom.scrollTop).toBe(400);
      for (let i = 0; i < 3; i++) await nextFrame();
      expect(geom.scrollTop).toBe(400);
      release();
    });

    it("observe('live-content') does nothing while user scroll intent is pending", async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      fireWheel(scrollEl, -10, scrollEl);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      controller.observe('live-content');

      expect(geom.scrollTop).toBe(400);
      for (let i = 0; i < 3; i++) await nextFrame();
      expect(geom.scrollTop).toBe(400);
    });

    it("observe('content') remains an instant layout nudge even in spring mode", async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      controller.observe('content');

      expect(geom.scrollTop).toBe(800);
      for (let i = 0; i < 3; i++) await nextFrame();
      expect(geom.scrollTop).toBe(800);
    });

    it("observe('host-layout') sync-pins even in spring mode (raw-controller mapping)", async () => {
      // MessageTimeline's pane adapter intercepts 'host-layout' before
      // it reaches the controller, but ChannelView registers the raw
      // controller and PaneHost sends 'host-layout' to every pane —
      // this pins that terminal mapping (instant path, not the spring).
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      controller.observe('host-layout');

      expect(geom.scrollTop).toBe(800);
      for (let i = 0; i < 3; i++) await nextFrame();
      expect(geom.scrollTop).toBe(800);
    });

    describe('stuck spring from instant pin during active chase', () => {
      // When observe('content') (composer-height RO) fires during an
      // active spring chase, instantPinAfterExternalGeometryChange writes
      // scrollTop = target. On the next tick: diff = 0, the velocity
      // update block is skipped, velocity stays frozen at its mid-chase
      // value (e.g., 28). When content then grows slightly (+10px), the
      // frozen velocity causes an immediate overshoot+clamp instead of
      // the smooth interpolation a clean sentinel would produce.
      //
      // These three cases also pin the UPPER bound of
      // SPRING_CARRY_VELOCITY_CEILING. The momentum-carry change (see the
      // sibling 'momentum carry across catch-up' describe) keeps a gentle
      // upward velocity across diff === 0 instead of always zeroing it, but
      // only below the ceiling (4). The remnant velocities here (~28, ~8,
      // ~14) all exceed it, so they are still shed and these tests still
      // pass. The cross-target-clamp case (~8) below is the tightest:
      // raising the ceiling to >= 8 would let that remnant be carried into
      // the follow-up growth and reintroduce the overshoot these guard.

      it('content growth after instant pin during spring should chase smoothly, not overshoot+clamp', async () => {
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150);

        // Start spring chase: content grows 400px, spring engages.
        geom.scrollHeight = 1400;
        geom.contentHeight = 1200;
        ro.fire(contentEl, 1200); // target = 800
        // Advance 3 frames to build velocity (~28 px/frame).
        for (let i = 0; i < 3; i++) await nextFrame();
        expect(geom.scrollTop).toBeGreaterThan(400);
        expect(geom.scrollTop).toBeLessThan(800);

        // Composer height change fires instant pin to target.
        controller.observe('content');
        expect(geom.scrollTop).toBe(800);

        // In production, the composer RO and content RO fire in
        // separate delivery loops (~3ms gap). Advance one frame so
        // the spring tick sees diff=0 and zeroes velocity.
        await nextFrame();

        // Small content growth (+10px). If velocity was properly
        // zeroed (diff=0 branch), the spring chases smoothly from
        // 800 toward 810 (~0.4px per frame). If velocity stayed
        // frozen at ~28 (no zeroing), the spring overshoots to 816+
        // and cross-target clamps to 810 on the first frame.
        geom.scrollHeight = 1410;
        geom.contentHeight = 1210;
        ro.fire(contentEl, 1210); // target = 810

        await nextFrame();
        // DESIRED: smooth chase — scrollTop moved partway toward 810
        // but did NOT arrive in a single frame.
        // BUG: frozen velocity causes overshoot → clamp → scrollTop = 810.
        expect(geom.scrollTop).toBeGreaterThan(800);
        expect(geom.scrollTop).toBeLessThan(810);
      });

      it('cross-target clamped spring zeros velocity so subsequent small growth chases smoothly', async () => {
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150);

        // Build velocity with a 200px chase.
        geom.scrollHeight = 1200;
        geom.contentHeight = 1000;
        ro.fire(contentEl, 1000); // target = 600
        for (let i = 0; i < 3; i++) await nextFrame();
        const midChase = geom.scrollTop;
        expect(midChase).toBeGreaterThan(400);

        // Grow to a target just past current position — spring
        // overshoots and cross-target clamps on first tick.
        const nearTarget = Math.ceil(midChase) + 2;
        geom.scrollHeight = nearTarget + 600;
        geom.contentHeight = nearTarget + 400;
        ro.fire(contentEl, nearTarget + 400);
        await advanceUntil(() => geom.scrollTop === nearTarget, 50);

        // Advance frames with diff=0. Without the fix, velocity stays
        // frozen at ~8; with the fix, velocity is zeroed immediately.
        for (let i = 0; i < 3; i++) await nextFrame();

        // Small growth (+3px). With clean velocity (0), the spring
        // chases smoothly (~0.12px/frame). With frozen velocity (~8),
        // accumulated exceeds 3px on the first frame → overshoot+clamp.
        geom.scrollHeight = nearTarget + 603;
        geom.contentHeight = nearTarget + 403;
        ro.fire(contentEl, nearTarget + 403); // target = nearTarget + 3
        await nextFrame();
        expect(geom.scrollTop).toBeGreaterThan(nearTarget);
        expect(geom.scrollTop).toBeLessThan(nearTarget + 3);
      });

      it("observe('content') during spring followed by small growth starts clean chase", async () => {
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150);

        // Start spring with a 200px chase to build velocity.
        geom.scrollHeight = 1200;
        geom.contentHeight = 1000;
        ro.fire(contentEl, 1000); // target = 600
        for (let i = 0; i < 3; i++) await nextFrame();
        expect(geom.scrollTop).toBeGreaterThan(400);

        // Instant pin kills the active chase.
        controller.observe('content');
        expect(geom.scrollTop).toBe(600);

        // Advance frames with diff=0. Without the fix, velocity stays
        // frozen at ~14; with the fix, velocity is zeroed immediately.
        for (let i = 0; i < 3; i++) await nextFrame();

        // Small content growth (+5px). With clean velocity (0), the
        // spring chases smoothly from 600 toward 605 (~0.2px/frame).
        // With frozen velocity (~14), accumulated exceeds 5px on the
        // first frame → overshoot+clamp to 605.
        geom.scrollHeight = 1205;
        geom.contentHeight = 1005;
        ro.fire(contentEl, 1005); // target = 605

        await nextFrame();
        expect(geom.scrollTop).toBeGreaterThan(600);
        expect(geom.scrollTop).toBeLessThan(605);

        await advanceUntil(() => geom.scrollTop === 605);
      });
    });

    describe('momentum carry across catch-up during streaming', () => {
      // Content height grows in line-sized quanta while streaming, so the
      // spring repeatedly reaches the bottom and idles between line wraps.
      // Unconditionally zeroing velocity on every catch-up forced each new
      // line to re-accelerate from rest — a steady stream read as a series
      // of slow-start lurches. While still inside the retain window the
      // spring now KEEPS a gentle (<= SPRING_CARRY_VELOCITY_CEILING) upward
      // follow velocity across the catch-up so the next line continues the
      // existing motion instead of restarting it. Carry is scoped to
      // growth-follow: downward (shrink-follow) remnants are shed so a
      // resumed growth never starts by nudging the viewport the wrong way.

      it('keeps gentle velocity across a catch-up within the retain window so the next line does not restart from rest', async () => {
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150); // warm

        // Build a gentle follow velocity (below the carry ceiling), then a
        // cross-target clamp lands exactly on target so the diff === 0
        // catch-up tick runs quickly — well inside the retain window,
        // where the carry path (not the settle path) applies. A natural
        // single-growth catch-up takes ~24 frames (> the retain window),
        // which is the streaming case where consecutive lines keep
        // refreshing the window; the clamp reproduces "caught up shortly
        // after the last growth" deterministically.
        geom.scrollHeight = 1050;
        geom.contentHeight = 850;
        ro.fire(contentEl, 850); // target = 450
        for (let i = 0; i < 2; i++) await nextFrame();
        const mid = geom.scrollTop;
        const nearTarget = Math.ceil(mid) + 1;
        geom.scrollHeight = nearTarget + 600;
        geom.contentHeight = nearTarget + 400;
        ro.fire(contentEl, nearTarget + 400);
        await advanceUntil(() => geom.scrollTop === nearTarget, 50);
        await nextFrame(); // diff === 0 catch-up tick — velocity carried
        expect(performance.now()).toBeLessThan(RETAIN_ANIMATION_DURATION_MS);

        // Small +3px growth. With momentum carried, the first frame moves
        // noticeably more than a cold start from rest would (cold
        // first-frame move for a 3px diff is ~0.12px); with velocity zeroed
        // (old behavior) it would BE that cold value.
        geom.scrollHeight = nearTarget + 603;
        geom.contentHeight = nearTarget + 403;
        ro.fire(contentEl, nearTarget + 403); // target = nearTarget + 3
        const beforeWarm = geom.scrollTop; // nearTarget
        await nextFrame();
        const warmFirstFrameMove = geom.scrollTop - beforeWarm;

        expect(warmFirstFrameMove).toBeGreaterThan(0.3); // > cold (~0.12)
        // Still a smooth chase, not a snap to the new bottom.
        expect(geom.scrollTop).toBeLessThan(nearTarget + 3);
      });

      it('sheds a downward (shrink-follow) remnant so a resumed growth never starts the wrong way', async () => {
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150); // warm

        // Phase 1: jump up and cross-clamp. The clamp velocity is well
        // above the carry ceiling, so the diff === 0 tick sheds it and the
        // spring is left at rest at `up` — a clean start for a downward
        // chase with no leftover upward momentum to overshoot.
        geom.scrollHeight = 1300;
        geom.contentHeight = 1100;
        ro.fire(contentEl, 1100); // target = 700
        for (let i = 0; i < 2; i++) await nextFrame();
        const up = Math.ceil(geom.scrollTop) + 1;
        geom.scrollHeight = up + 600;
        geom.contentHeight = up + 400;
        ro.fire(contentEl, up + 400); // target = up
        await advanceUntil(() => geom.scrollTop === up, 50);
        await nextFrame(); // diff === 0: above-ceiling velocity shed → at rest

        // Phase 2: shrink so the spring chases DOWN from rest and
        // cross-clamps onto the lower target with a NEGATIVE velocity. The
        // negative delta bumps the retain timestamp, so the catch-up lands
        // inside the retain window — the carry path applies, and the sign
        // gate is what decides shed-vs-carry here. The 12px drop keeps the
        // clamp velocity solidly negative (~-0.8), not a near-zero remnant
        // that both code paths would treat alike.
        const down = up - 12;
        geom.scrollHeight = down + 600;
        geom.contentHeight = down + 400;
        ro.fire(contentEl, down + 400); // target = down (shrink)
        await advanceUntil(() => geom.scrollTop === down, 60);
        await nextFrame(); // diff === 0 catch-up — downward remnant must shed

        // Resumed growth. Carry is scoped to growth-follow, so the negative
        // remnant was dropped and this is a clean cold start that moves
        // TOWARD the new bottom. Without the sign gate the carried negative
        // velocity would pull scrollTop the wrong way (away from bottom)
        // on the first frame.
        geom.scrollHeight = down + 603;
        geom.contentHeight = down + 403;
        ro.fire(contentEl, down + 403); // target = down + 3
        const beforeResume = geom.scrollTop; // down
        await nextFrame();
        expect(geom.scrollTop).toBeGreaterThan(beforeResume);
        // A real (small) move toward the bottom, not a snap.
        expect(geom.scrollTop).toBeLessThan(down + 3);
      });
    });

    describe('spring chase distance invariants', () => {
      // The spring's total visual distance must equal the actual
      // geometry change (scrollHeight delta), not the engine estimate.
      // These tests verify the distance invariant under timing
      // variations that could produce wrong starting positions.

      it('estimate-correct pair: spring lands at corrected target, not the estimate', async () => {
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150);

        const startScrollTop = geom.scrollTop; // 400

        // +90 estimate (mimics the engine provisionally placing a row).
        geom.scrollHeight = 1090;
        geom.contentHeight = 890;
        ro.fire(contentEl, 890); // estimate target = 490

        // -50 correction within the same RO burst.
        geom.scrollHeight = 1040;
        geom.contentHeight = 840;
        ro.fire(contentEl, 840); // corrected target = 440

        // Spring should land at 440 (corrected), not 490 (estimate).
        await advanceUntil(() => geom.scrollTop === 440);
        // Total distance: 440 - 400 = 40px (the actual row height).
        expect(geom.scrollTop - startScrollTop).toBe(40);
      });

      it('two rows in quick succession: total distance is sum of actual heights', async () => {
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150);

        const startScrollTop = geom.scrollTop; // 400

        // Row 1: +90 estimate, then -50 correction (actual 40px).
        geom.scrollHeight = 1090;
        geom.contentHeight = 890;
        ro.fire(contentEl, 890);
        geom.scrollHeight = 1040;
        geom.contentHeight = 840;
        ro.fire(contentEl, 840);

        // Row 2: +90 estimate, then -55 correction (actual 35px).
        geom.scrollHeight = 1130;
        geom.contentHeight = 930;
        ro.fire(contentEl, 930);
        geom.scrollHeight = 1075;
        geom.contentHeight = 875;
        ro.fire(contentEl, 875);

        // Target = 1075 - 600 = 475. Total height: 40+35 = 75px.
        await advanceUntil(() => geom.scrollTop === 475);
        expect(geom.scrollTop - startScrollTop).toBe(75);
      });

      it('row appearing mid-chase does not double-count distance', async () => {
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150);

        const startScrollTop = geom.scrollTop; // 400

        // Row 1 appears (actual 40px after correction).
        geom.scrollHeight = 1090;
        geom.contentHeight = 890;
        ro.fire(contentEl, 890);
        geom.scrollHeight = 1040;
        geom.contentHeight = 840;
        ro.fire(contentEl, 840); // target = 440

        // Spring runs a few frames — partial progress.
        for (let i = 0; i < 3; i++) await nextFrame();
        const midChase = geom.scrollTop;
        expect(midChase).toBeGreaterThan(400);
        expect(midChase).toBeLessThan(440);

        // Row 2 appears mid-chase (actual 30px).
        geom.scrollHeight = 1120;
        geom.contentHeight = 920;
        ro.fire(contentEl, 920);
        geom.scrollHeight = 1070;
        geom.contentHeight = 870;
        ro.fire(contentEl, 870); // target = 470

        // Spring continues from midChase toward 470.
        await advanceUntil(() => geom.scrollTop === 470);
        // Total distance from start: 70px = 40px + 30px. Correct.
        expect(geom.scrollTop - startScrollTop).toBe(70);
      });

      it('new row after sentinel starts fresh chase from correct position (no carryover offset)', async () => {
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150);

        // Row 1: spring chases and arrives.
        geom.scrollHeight = 1100;
        geom.contentHeight = 900;
        ro.fire(contentEl, 900); // target = 500
        await advanceUntil(() => geom.scrollTop === 500);
        while (mockNow < 360) await nextFrame(); // sentinel

        const posBeforeRow2 = geom.scrollTop; // 500

        // Row 2 appears (actual 40px).
        geom.scrollHeight = 1190;
        geom.contentHeight = 990;
        ro.fire(contentEl, 990);
        geom.scrollHeight = 1140;
        geom.contentHeight = 940;
        ro.fire(contentEl, 940); // target = 540

        // Sentinel sees the new target (diff > 0), starts chasing.
        // With the velocity-zero fix, velocity starts at 0 (not
        // carried over from the previous chase).
        await nextFrame();
        // First frame should move a small amount — proportional to
        // the actual 40px diff, not influenced by prior velocity.
        const firstFrameMove = geom.scrollTop - posBeforeRow2;
        // With vel=0, stiffness=0.05, diff=40, mass=1.25:
        // vel = 0.05*40/1.25 = 1.6, move = 1.6px.
        // Allow some tolerance but it should be small, not the ~16px
        // a carryover velocity would produce.
        expect(firstFrameMove).toBeLessThan(5);
        expect(firstFrameMove).toBeGreaterThan(0);

        await advanceUntil(() => geom.scrollTop === 540);
        expect(geom.scrollTop - posBeforeRow2).toBe(40);
      });

      it("observe('live-content') during sentinel does not add phantom distance", async () => {
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150);

        geom.scrollHeight = 1100;
        geom.contentHeight = 900;
        ro.fire(contentEl, 900);
        await advanceUntil(() => geom.scrollTop === 500);
        while (mockNow < 360) await nextFrame();

        const posBeforeNudge = geom.scrollTop;

        // Structural nudge fires but nothing actually grew.
        controller.observe('live-content');
        for (let i = 0; i < 5; i++) await nextFrame();

        // scrollTop must not have moved — no geometry change.
        expect(geom.scrollTop).toBe(posBeforeNudge);

        // Now actual content grows. Distance should be exactly the
        // growth amount, not growth + phantom.
        geom.scrollHeight = 1150;
        geom.contentHeight = 950;
        ro.fire(contentEl, 950); // target = 550

        await advanceUntil(() => geom.scrollTop === 550);
        expect(geom.scrollTop - posBeforeNudge).toBe(50);
      });
    });

    it('lands at target eventually and stops ticking', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);

      // Spring should arrive at 800 (= 1400 - 600).
      await advanceUntil(() => geom.scrollTop === 800);

      // After arrival, additional frames shouldn't change anything.
      const after = geom.scrollTop;
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(after);
    });

    it('keeps chasing across multiple positive deltas (no per-chunk restart)', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // First chunk.
      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);

      await nextFrame();
      await nextFrame();
      const midScrollTop = geom.scrollTop;
      expect(midScrollTop).toBeGreaterThan(400);
      expect(midScrollTop).toBeLessThan(800);

      // Second chunk arrives mid-flight.
      geom.scrollHeight = 1800;
      geom.contentHeight = 1600;
      ro.fire(contentEl, 1600);

      // Spring continues from current position toward new target (1200).
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBeGreaterThan(midScrollTop);

      // Eventually arrives at the moving target.
      await advanceUntil(() => geom.scrollTop === 1200);
    });

    it('keeps sticky intent across thinking-style grow, same-height, shrink, and grow updates', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Thinking text starts streaming and grows the row.
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      await nextFrame();
      const midScrollTop = geom.scrollTop;
      expect(midScrollTop).toBeGreaterThan(400);
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);

      // A tail-scroll/body text update can leave outer row height
      // unchanged. It must not change follow intent.
      ro.fire(contentEl, 1000);
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);

      // Tail-clamping or a row remeasurement can shrink the row while a
      // spring is active. This must still be treated as layout, not user
      // escape.
      geom.scrollHeight = 1160;
      geom.contentHeight = 960;
      ro.fire(contentEl, 960);
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);

      // More thinking text arrives after the shrink. The controller
      // should keep following the new moving bottom.
      geom.scrollHeight = 1320;
      geom.contentHeight = 1120;
      ro.fire(contentEl, 1120);
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
      await advanceUntil(() => geom.scrollTop === 720);
    });

    it('escape (wheel-up) cancels in-flight spring', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      const midScrollTop = geom.scrollTop;
      expect(midScrollTop).toBeGreaterThan(400);
      expect(midScrollTop).toBeLessThan(800);

      // User wheel-ups and the outer scroller actually moves up.
      fireWheel(scrollEl, -40, scrollEl);
      geom.scrollTop = midScrollTop - 40;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // Spring stops; scrollTop should not advance further.
      const afterEscape = geom.scrollTop;
      for (let i = 0; i < 20; i++) await nextFrame();
      expect(geom.scrollTop).toBe(afterEscape);
    });

    it('setEscapedFromLock(true) cancels in-flight spring', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      const midScrollTop = geom.scrollTop;

      controller.setEscapedFromLock(true);

      const afterStop = geom.scrollTop;
      for (let i = 0; i < 20; i++) await nextFrame();
      expect(geom.scrollTop).toBe(afterStop);
      expect(geom.scrollTop).toBeLessThan(800); // didn't arrive after stop
      // Suppress unused-var lint
      expect(midScrollTop).toBeGreaterThanOrEqual(400);
    });

    it('detach cancels in-flight spring and clears warm state', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();

      controller.detach();
      const afterDetach = geom.scrollTop;
      for (let i = 0; i < 20; i++) await nextFrame();
      expect(geom.scrollTop).toBe(afterDetach);
    });

    it('mode=instant takes the sync-pin path even when warm', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      mode = 'instant';

      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);

      // Sync-pin: scrollTop already at target.
      expect(geom.scrollTop).toBe(600);
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(600);
    });

    it('prefers-reduced-motion suppresses the spring', async () => {
      vi.spyOn(window, 'matchMedia').mockImplementation((query: string) => ({
        matches: query === '(prefers-reduced-motion: reduce)',
        media: query,
        onchange: null,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        dispatchEvent: vi.fn(),
      } as unknown as MediaQueryList));

      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);

      // Sync-pin: scrollTop already at target, no spring.
      expect(geom.scrollTop).toBe(800);
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(800);
    });

    it('mode flipping from spring to instant mid-flight lets spring land cleanly', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      const midScrollTop = geom.scrollTop;
      expect(midScrollTop).toBeGreaterThan(400);
      expect(midScrollTop).toBeLessThan(800);

      // Turn ends mid-spring.
      mode = 'instant';

      // Spring should still land at target (no abrupt cancel).
      await advanceUntil(() => geom.scrollTop === 800);
    });

    it('paused lease blocks spring from starting', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      const release = controller.pauseAutoScroll();

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);

      // Lease active — no spring, no sync-pin write.
      expect(geom.scrollTop).toBe(400);
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(400);

      // Release: re-pins via pauseAutoScroll.release path (sync write).
      release();
      expect(geom.scrollTop).toBe(800);
    });
  });

  describe('pauseDepth during active spring — disclosure toggle contract', () => {
    // pauseDepth is the ONLY engagement condition that can flip
    // independently during streaming without canceling the spring. These
    // tests lock the current contract: preserveScrollAnchor disengages
    // arbitration via pauseAutoScroll(); the spring self-cancels on the
    // next rAF tick.

    it('a routed compensation passes during pauseDepth > 0 even with springToken !== 0', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200); // target = 800
      await nextFrame();
      const midChase = geom.scrollTop;
      expect(midChase).toBeGreaterThan(400);

      // Pause lease (simulates disclosure toggle starting).
      const release = controller.pauseAutoScroll();

      // Compensation passes — the resolver sees paused and steps aside
      // even though the spring token is still live for one more tick.
      geom.scrollHeight = 1500;
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: 100, target: 700 })).toBe(true);
      expect(geom.scrollTop).toBe(700);

      release();
    });

    it('spring tick bails and cancels when it sees pauseDepth > 0', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      const midChase = geom.scrollTop;
      expect(midChase).toBeGreaterThan(400);

      const release = controller.pauseAutoScroll();
      // Advance one frame — spring tick sees pauseDepth > 0, cancels.
      await nextFrame();
      const afterCancel = geom.scrollTop;

      // scrollTop stopped advancing (spring dead).
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(afterCancel);

      // Release re-pins to target.
      release();
      expect(geom.scrollTop).toBe(800);
    });

    it('sticky user remains pinned after pause lease completes during spring', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000); // target = 600
      await nextFrame();
      expect(geom.scrollTop).toBeGreaterThan(400);

      // Pause, advance (spring cancels), release (re-pins).
      const release = controller.pauseAutoScroll();
      await nextFrame();
      release();
      expect(geom.scrollTop).toBe(600);

      // Subsequent content growth should resume bottom-following.
      geom.scrollHeight = 1600;
      geom.contentHeight = 1400;
      ro.fire(contentEl, 1400); // target = 1000
      // New spring starts (previous was canceled by pause).
      await advanceUntil(() => geom.scrollTop === 1000);
      expect(controller.isSticky).toBe(true);
    });
  });

  describe('engagement condition coupling invariants — spring cancellation', () => {
    // Each LOW-fragility engagement condition (isAtBottomState,
    // escapedFromLockState, warm) couples its transitions with
    // cancelSpring(). These tests prove the coupling holds.

    it('wheel-up escape during spring cancels spring and stops advancement', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      const midChase = geom.scrollTop;
      expect(midChase).toBeGreaterThan(400);

      // User escape — cancels spring immediately.
      fireWheel(scrollEl, -10, scrollEl);
      expect(controller.escapedFromLock).toBe(true);

      // scrollTop stopped advancing.
      const afterEscape = geom.scrollTop;
      for (let i = 0; i < 10; i++) await nextFrame();
      expect(geom.scrollTop).toBe(afterEscape);

      // Escaped: the user is reading mid-thread, so a routed compensation
      // passes (above-viewport visual stability wins).
      geom.scrollHeight = 1500;
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: -100, target: 300 })).toBe(true);
      expect(geom.scrollTop).toBe(300);
    });

    it('forceStick(restore) re-arms warmup and cancels spring — compensations pass while warm=false', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      expect(geom.scrollTop).toBeGreaterThan(400);

      // Restore-reason forceStick re-arms warm gate → warm=false,
      // coupled with cancelSpring.
      controller.armRestoreSnap();
      controller.forceStick({ reason: 'restore' });
      expect(controller.isWarm).toBe(false);

      // !warm: the engine's post-restore mount-cascade compensations must
      // land, so the resolver passes them through. (The resolver ignores
      // `jump`; it is passed sign-consistent for documentation only.)
      geom.scrollHeight = 1500;
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: -300, target: 500 })).toBe(true);
      expect(geom.scrollTop).toBe(500);
    });

    it('setEscapedFromLock(true) cancels spring and flips isAtBottomState', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      expect(geom.scrollTop).toBeGreaterThan(400);

      controller.setEscapedFromLock(true);
      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);

      // Spring canceled — scrollTop stops.
      const afterEscape = geom.scrollTop;
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(afterEscape);
    });
  });

  describe('scrollbar and interaction during spring', () => {
    it('scrollbar pointer in gutter during spring chase escapes and cancels spring', async () => {
      stubScrollbarGutter(scrollEl);
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      expect(geom.scrollTop).toBeGreaterThan(400);

      // Pointer in scrollbar gutter.
      const ev = new PointerEvent('pointerdown', {
        clientX: 195, isPrimary: true, button: 0, bubbles: true,
      });
      Object.defineProperty(ev, 'target', { value: scrollEl });
      scrollEl.dispatchEvent(ev);

      expect(controller.escapedFromLock).toBe(true);

      // Spring dead — scrollTop stops.
      const afterEscape = geom.scrollTop;
      for (let i = 0; i < 10; i++) await nextFrame();
      expect(geom.scrollTop).toBe(afterEscape);
    });

    it("observe('content') during spring does not leave the spring token stuck live", async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      expect(geom.scrollTop).toBeGreaterThan(400);

      // Instant pin to target.
      controller.observe('content');
      expect(geom.scrollTop).toBe(800);

      // Advance to let the spring arrive (velocity zeroed by the fix).
      for (let i = 0; i < 30; i++) await nextFrame();

      // Turn ends. Mode flips to instant. Spring should cancel.
      mode = 'instant';
      for (let i = 0; i < 10; i++) await nextFrame();

      // Spring canceled (springToken=0): a routed compensation resolves
      // through the pass tier instead of the mid-chase decline.
      geom.scrollHeight = 1500;
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: 50, target: 850 })).toBe(true);
      expect(geom.scrollTop).toBe(850);
    });
  });

  describe('user-reported regression — streaming stops following after send + manual scroll', () => {
    // Reproduces the post-gentle-mango user report:
    //
    //   "i typed a message, it didnt follow i had to scroll manually, and
    //    then i did, and then every message that comes in it does not
    //    follow. ... when i scroll up/down after going back to that thread
    //    it doesnt stick again."
    //
    // The flow involves: (1) restore-to-bottom on mount, (2) composer
    // grow + shrink around send, (3) new user-message contentRO fire,
    // (4) streaming contentRO chunks, (5) wheel-up then wheel-down
    // (re-stick), (6) more streaming chunks. Each step must keep the
    // bottom following.
    it('full send + streaming + wheel-up + re-stick + streaming flow keeps bottom following', async () => {
      const ro = getRO();
      // Mount-time first contentRO fire, warm passes 150ms later.
      ro.fire(contentEl, 800);
      await waitMs(150);
      expect(controller.isWarm).toBe(true);

      // Simulate restore-to-bottom path (thread switch effect: arm
      // restore consent, then forceStick). This is the cache-hit path
      // MessageTimeline takes on every thread switch.
      controller.armRestoreSnap();
      controller.forceStick({ reason: 'restore' });
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);

      // The restore re-armed warm; cross past it again before streaming.
      ro.fire(contentEl, 800);
      await waitMs(150);
      expect(controller.isWarm).toBe(true);

      // 1. Composer grows during typing — ChatView's composer-RO calls
      //    observe('content') with scrollEl padding-bottom larger.
      geom.scrollHeight = 1050;
      controller.observe('content');
      expect(geom.scrollTop).toBe(450); // pinned to new bottom

      // 2. User hits Enter. Composer text clears (shrinks); composer-RO
      //    fires again with smaller height.
      geom.scrollHeight = 1000;
      controller.observe('content');

      // 3. User message added — the engine mounts the row, contentRO fires
      //    positive delta. Spring mode is now active.
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      // Spring should engage; sync-pin would land at 500 immediately.
      // Either way scrollTop must advance toward the new bottom across
      // frames.
      await advanceUntil(() => geom.scrollTop >= 500, 50);
      expect(controller.escapedFromLock).toBe(false);

      // 4. Streaming chunks (3 in a row).
      for (let i = 0; i < 3; i++) {
        geom.scrollHeight += 200;
        geom.contentHeight += 200;
        ro.fire(contentEl, geom.contentHeight);
      }
      await advanceUntil(() => geom.scrollTop >= geom.scrollHeight - geom.clientHeight - 4, 50);
      expect(controller.escapedFromLock).toBe(false);

      // 5. User wheels UP to read. Should escape.
      fireWheel(scrollEl, -50, scrollEl);
      const beforeWheel = geom.scrollTop;
      geom.scrollTop = beforeWheel - 100; // browser scrolled up
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // 6. More streaming chunks arrive while escaped. They must NOT
      //    pin.
      const afterEscape = geom.scrollTop;
      geom.scrollHeight += 200;
      geom.contentHeight += 200;
      ro.fire(contentEl, geom.contentHeight);
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(afterEscape);
      expect(controller.escapedFromLock).toBe(true);

      // 7. User wheels DOWN to the bottom (re-stick).
      fireWheel(scrollEl, 200, scrollEl);
      // Land exactly at bottom.
      geom.scrollTop = geom.scrollHeight - geom.clientHeight;
      fireScroll(scrollEl);
      await nextTimer();
      // Re-stick must fire.
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);

      // 8. More streaming chunks after re-stick. They MUST pin.
      const beforeMoreStream = geom.scrollHeight;
      geom.scrollHeight += 200;
      geom.contentHeight += 200;
      ro.fire(contentEl, geom.contentHeight);
      await advanceUntil(
        () => geom.scrollTop >= geom.scrollHeight - geom.clientHeight - 4,
        50,
      );
      // Final assertion: streaming should follow.
      expect(geom.scrollTop).toBeGreaterThanOrEqual(beforeMoreStream - geom.clientHeight);
      expect(controller.escapedFromLock).toBe(false);
    });

    it('native scrollend before the deferred scroll handler does not clear down intent', async () => {
      // Native scrollend may fire before handleScroll's 1ms deferred
      // branch. Since the controller no longer listens to scrollend,
      // the recent down-intent captured at wheel time must survive
      // until the deferred re-stick check runs.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Establish escape.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // Wheel down to bottom; recent down intent is recorded, then
      // scrollend fires BEFORE the deferred handler.
      fireWheel(scrollEl, 200, scrollEl);
      geom.scrollTop = 400; // at bottom
      fireScroll(scrollEl); // schedules deferred for 1ms

      // Simulate scrollend racing the deferred handler (fires within
      // the 1ms window — in real browsers, scrollend can dispatch
      // immediately when scroll position stabilizes after the wheel).
      fireScrollEnd(scrollEl);

      // Wait past the deferred handler.
      await nextTimer();

      // Re-stick should have fired regardless.
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('separate wheel-down steps across native scrollend re-stick at bottom', async () => {
      // Two-step re-stick: user can't make it to bottom in one wheel.
      // Step 1 remains escaped. Step 2 records fresh down intent,
      // scrolls to bottom, and re-sticks. The native scrollend between
      // steps must not change either outcome.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Establish escape.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // Step 1: wheel down by 100. Land 200px from bottom.
      fireWheel(scrollEl, 100, scrollEl);
      geom.scrollTop = 200;
      fireScroll(scrollEl);
      await nextTimer();
      // Not at bottom yet; should stay escaped.
      expect(controller.escapedFromLock).toBe(true);

      // Scrollend fires after wheel scroll settles.
      fireScrollEnd(scrollEl);

      // Step 2: wheel down more. Land at bottom.
      fireWheel(scrollEl, 200, scrollEl);
      geom.scrollTop = 400;
      fireScroll(scrollEl);
      await nextTimer();
      // Re-stick must fire.
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('native scrollend during spring chase mid-stream does not escape', async () => {
      // Spring writes scrollTop every rAF (60Hz). Some browsers may fire
      // scrollend between frames if the spring isn't writing fast
      // enough. The event is not observed by the controller and must
      // never escape.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Start a spring chase with a streaming chunk.
      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      const mid = geom.scrollTop;
      expect(mid).toBeGreaterThan(400);
      expect(mid).toBeLessThan(800);
      expect(controller.escapedFromLock).toBe(false);

      // Scrollend fires mid-spring. Must NOT escape.
      fireScrollEnd(scrollEl);
      await nextFrame();
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);

      // Spring continues to land.
      await advanceUntil(() => geom.scrollTop === 800);
    });

    it('stale native scrollend from prior spring write does not affect immediate wheel-up escape', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // settle warm-up gate so spring path engages

      // Content grows during streaming. Spring begins chasing
      // target=500 (1100-600). One nextFrame advances the spring
      // tick — scrollTop lands mid-flight, distance > 4 epsilon
      // (otherwise safety-net repin wouldn't fire and the test
      // wouldn't isolate the bug).
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      await nextFrame();
      const midScrollTop = geom.scrollTop;
      expect(midScrollTop).toBeGreaterThan(400);
      expect(midScrollTop).toBeLessThan(500);
      expect(500 - midScrollTop).toBeGreaterThan(4);

      fireWheel(scrollEl, -100, scrollEl);
      expect(controller.escapedFromLock).toBe(true);

      fireScroll(scrollEl);

      fireScrollEnd(scrollEl);

      expect(geom.scrollTop).toBe(midScrollTop);

      geom.scrollTop = Math.max(0, midScrollTop - 100);
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);

      const scrollTopAfterEscape = geom.scrollTop;
      fireScrollEnd(scrollEl);
      await waitRealMs(180);
      expect(controller.escapedFromLock).toBe(true);
      expect(geom.scrollTop).toBe(scrollTopAfterEscape);
    });

    it('continuous wheel-up events (trackpad momentum) keep escape latched', async () => {
      // Trackpad momentum fires wheel events at high frequency. Each
      // upward wheel is an escape input. During this whole window,
      // escape must stay set.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Initial wheel sets escape.
      fireWheel(scrollEl, -10, scrollEl);
      geom.scrollTop = 390;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // Simulate momentum: 10 more wheels at 16ms intervals (each one
      // moves up further by 5px).
      for (let i = 0; i < 10; i++) {
        fireWheel(scrollEl, -5, scrollEl);
        geom.scrollTop = Math.max(0, geom.scrollTop - 5);
        fireScroll(scrollEl);
        await nextTimer();
      }

      expect(controller.escapedFromLock).toBe(true);

      // Streaming chunk arrives during momentum. Must NOT pin.
      const beforeChunk = geom.scrollTop;
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(beforeChunk);
    });

    it('composer-RO after scrollbar pointer escape does not pin', async () => {
      stubScrollbarGutter(scrollEl);

      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      scrollEl.dispatchEvent(
        new PointerEvent('pointerdown', { bubbles: true, isPrimary: true, clientX: 195, clientY: 300 }),
      );

      geom.scrollHeight = 1100;
      controller.observe('content');
      expect(controller.escapedFromLock).toBe(true);
      expect(geom.scrollTop).toBe(400);
    });

    it('two wheel-down steps across native scrollend use fresh down intent and re-stick', async () => {
      // User wheel-down lands near-bottom but outside the 4px epsilon,
      // then a later wheel-down reaches the actual bottom. Re-stick is
      // driven by the second wheel's fresh down intent plus downward
      // scroll movement from the controller-scope baseline.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Escape.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // Wheel #1: down by 100. Land at 200 (200px from bottom).
      fireWheel(scrollEl, 100, scrollEl);
      geom.scrollTop = 200;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // Scrollend fires after wheel #1's scroll settles.
      fireScrollEnd(scrollEl);

      // Wheel #2: down by 200. Land at 400 (at bottom).
      fireWheel(scrollEl, 200, scrollEl);
      geom.scrollTop = 400;
      fireScroll(scrollEl);
      await nextTimer();

      // Should re-stick.
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });
  });

  describe('user-reported regression — re-stick invariants across production-like flows', () => {
    // Battery of invariants the controller must satisfy in the scenarios
    // the user reported as broken. Each test exercises a different path
    // back to the bottom and asserts that re-stick fires. If one of these
    // fails, it pinpoints the specific path that strands the user.
    //
    // User intent (verbatim across reports):
    //   "by default it should stick to the bottom, if i scroll away at all
    //    i expect to unstick even if i very slightly scroll away at any
    //    time, and if i scroll to the bottom i expect for it to stick
    //    always."

    it('wheel-up + wait + wheel-down to absolute bottom re-sticks', async () => {
      // User wheels up, waits, then wheels down to the absolute bottom.
      // No momentum interference. Re-stick MUST fire from the fresh
      // down intent.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      await waitMs(200);

      // Fresh wheel-down to absolute bottom (scrollHeight - clientHeight = 400).
      fireWheel(scrollEl, 200, scrollEl);
      geom.scrollTop = 400;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('wheel-up + quick wheel-down with momentum overshoot still re-sticks at bottom', async () => {
      // The seq 371-374 dump pattern: leftover momentum from wheel-up
      // carries scrollTop UP after wheel-down records down intent.
      // Trajectory then reverses and reaches the bottom inside the
      // recent intent window. Re-stick MUST fire.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // User wheels DOWN from scrollTop=200.
      geom.scrollTop = 200;
      fireWheel(scrollEl, 100, scrollEl);

      // Leftover upward momentum: first scroll event lands BELOW the
      // wheel-down's startScrollTop. The deferred handler must NOT treat
      // this as a fresh escape — the user explicitly signaled DOWN.
      geom.scrollTop = 195;
      fireScroll(scrollEl);
      await nextTimer();

      // Trajectory reverses, reaches absolute bottom (400 = max).
      geom.scrollTop = 400;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('wheel-up + incremental wheel-downs re-stick at bottom', async () => {
      // User wheels down in multiple separate steps. The final wheel
      // reaches the bottom and MUST re-stick.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 50;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // Step 1: small wheel-down, not enough to reach bottom.
      fireWheel(scrollEl, 100, scrollEl);
      geom.scrollTop = 150;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // Step 2: another small wheel-down, still not bottom.
      fireWheel(scrollEl, 100, scrollEl);
      geom.scrollTop = 250;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // Step 3: final wheel-down reaches absolute bottom.
      fireWheel(scrollEl, 200, scrollEl);
      geom.scrollTop = 400;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('forceStick(user) after escape + multiple in-flight scrolls clears all transient state', async () => {
      // User wheels up + several momentum scroll events, then clicks the
      // chip. The chip's forceStick MUST clear EVERY transient flag so
      // subsequent streaming chunks pin correctly (via spring chase in
      // this describe block's animationMode='spring').
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Wheel up + 5 momentum scroll events going up.
      fireWheel(scrollEl, -50, scrollEl);
      for (let i = 0; i < 5; i++) {
        geom.scrollTop = Math.max(0, geom.scrollTop - 20);
        fireScroll(scrollEl);
        await nextTimer();
      }
      expect(controller.escapedFromLock).toBe(true);

      // Chip click.
      controller.forceStick();
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);

      // Next streaming chunk arrives. The spring should chase to the
      // new bottom over the next several rAF ticks.
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);

      // Spring is mode='spring' inside this describe — wait for it to land.
      await advanceUntil(() => geom.scrollTop >= 500, 100);
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('forceStick(user) + multiple streaming chunks all pin (no leaked block state)', async () => {
      // Cumulative variant: 5 streaming chunks after a chip click. Each
      // MUST pin (eventually, via spring) — proves no state leaks between chunks.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Escape, then chip click.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      controller.forceStick();
      expect(controller.isSticky).toBe(true);

      // 5 streaming chunks. Each must pin to the new bottom (via spring).
      for (let i = 0; i < 5; i++) {
        geom.scrollHeight += 100;
        geom.contentHeight += 100;
        ro.fire(contentEl, geom.contentHeight);
        const expectedTop = geom.scrollHeight - geom.clientHeight;
        await advanceUntil(() => geom.scrollTop >= expectedTop, 100);
      }
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('pauseAutoScroll lease released during escape does not strand the user', async () => {
      // RHS panel mount holds a leaseDuringSettle while the user is
      // escaped. The lease release MUST NOT silently re-pin the user
      // to the bottom and MUST NOT leak pauseDepth.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Establish escape.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // RHS panel mounts: acquires lease, then releases after settle.
      const release = controller.pauseAutoScroll();
      release();

      // Escape must hold; scrollTop must not jump.
      expect(controller.escapedFromLock).toBe(true);
      expect(geom.scrollTop).toBe(100);
    });

    it('overlapping pauseAutoScroll leases all release; pauseDepth returns to 0', async () => {
      // Multiple concurrent RHS / sidebar leases. Each must decrement
      // pauseDepth independently — none should leak.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Sticky, at bottom.
      expect(controller.isSticky).toBe(true);

      const r1 = controller.pauseAutoScroll();
      const r2 = controller.pauseAutoScroll();
      const r3 = controller.pauseAutoScroll();

      // While leased, contentRO positive delta MUST NOT pin (pauseDepth>0).
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      expect(geom.scrollTop).toBe(400); // unchanged

      // Release in arbitrary order. r3() re-pins via the lease-release
      // path (scrollTop = current target = 500).
      r2();
      r1();
      r3();
      expect(geom.scrollTop).toBe(500);

      // pauseDepth back to 0. Next chunk must pin (eventually, via spring).
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      await advanceUntil(() => geom.scrollTop >= 600, 100);
    });

    it('wheel-down trajectory lands at bottom while scrollHeight grew during the scroll (moving-bottom case)', async () => {
      // Production reality: during the user's wheel-down, a streaming
      // chunk arrives and grows scrollHeight. The user reaches what
      // they SAW as the bottom (sync-captured distFromBottomAtEvent),
      // but distanceFromBottom NOW reports a larger value.
      // The Bug A fix (distFromBottomAtEvent captured sync) means re-stick
      // MUST fire based on what the user saw at scroll-event time.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Escape.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // User wheels down. arms 'down' at startScrollTop=100.
      fireWheel(scrollEl, 300, scrollEl);

      // Scroll event fires at what is RIGHT NOW the absolute bottom
      // (scrollHeight=1000, scrollTop=400, distFromBottom=0). Bug A
      // captures distFromBottomAtEvent = 0 synchronously here.
      geom.scrollTop = 400;
      fireScroll(scrollEl);

      // BEFORE the 1ms deferred handler runs, a streaming chunk grows
      // scrollHeight by 100. distFromBottom NOW = 100, but the
      // captured value at scroll time was 0 — re-stick must use the
      // captured value (Bug A fix), so it MUST fire.
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      await nextTimer();

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('wheel-down stops short of bottom by AUTO_FOLLOW_BOTTOM_EPSILON_PX (4px tolerance)', async () => {
      // Browser scrollTop rounding can land the user 1-3 px short of
      // the absolute max. The widened epsilon must tolerate this.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();

      fireWheel(scrollEl, 300, scrollEl);
      geom.scrollTop = 397; // 3 px short of max (400). Within 4 px epsilon.
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('thread-switch-style reset (armWarmup + armRestoreSnap + forceStick(restore)) clears stuck escape', async () => {
      // What thread-switch does that chip click does NOT do. If this
      // path uniquely unblocks the user, the chip-click path is missing
      // some reset step.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Get user stranded escaped near bottom.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // Thread-switch sequence as MessageTimeline performs it.
      controller.setEscapedFromLock(true);
      controller.armWarmup();
      controller.armRestoreSnap();
      controller.forceStick({ reason: 'restore' });

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);

      // Streaming chunk MUST pin (with warm gate open via sync-pin path).
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      expect(geom.scrollTop).toBe(500);
    });

    it('repeated wheel-up events without intervening scroll events still allow later re-stick', async () => {
      // Trackpad sends many wheel events at high frequency. Upward
      // inputs keep escape latched. A later down wheel to the bottom
      // must still re-stick.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Burst of wheel-up + scroll events going up (10 micro-deltas).
      for (let i = 0; i < 10; i++) {
        fireWheel(scrollEl, -5, scrollEl);
        geom.scrollTop = Math.max(0, geom.scrollTop - 5);
        fireScroll(scrollEl);
        await nextTimer();
      }
      expect(controller.escapedFromLock).toBe(true);

      // Now wheel down all the way to bottom.
      fireWheel(scrollEl, 500, scrollEl);
      geom.scrollTop = 400;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('untagged scroll-event trajectory landing at distFromBottom=0 does not re-stick without recent down intent', async () => {
      // The rewritten intent model requires a current down input before
      // a bottom-landing scroll event can re-engage auto-follow. That
      // keeps layout/applyJump/clamp scrolls from silently clearing an
      // escaped reader.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Establish escape.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      await waitMs(200);

      const trajectory = [200, 250, 300, 350, 400];
      for (const top of trajectory) {
        geom.scrollTop = top;
        fireScroll(scrollEl);
        await nextTimer();
      }

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('wheel-down at distFromBottom=0 + escaped re-sticks without a follow-up scroll event (clamp-then-wheel lockout)', async () => {
      // Bug #2 (bug-report-20260520T010930Z, seq 4953-5317). A content
      // shrink clamped scrollTop to the new max — the clamp's scroll
      // event fired BEFORE the contentRO microtask, so `resizeDifference`
      // was still 0 in handleScroll and there was no recent down intent.
      // User landed at distFromBottom=0 but escaped. They then wheeled
      // down to re-engage — but the browser refused to scroll past the
      // absolute bottom, so ZERO scroll events fired. 180 wheel events
      // could not reach the scroll-handler re-stick path. User stranded.
      //
      // Fix: handleWheel detects wheel-down + escaped + at-bottom and
      // re-sticks synchronously. The wheel itself is the explicit user
      // consent — no scroll event needed.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Simulate the trapped state directly. Reproducing the full
      // clamp-from-shrink-then-no-bailRO trap requires precise event
      // ordering between scroll + contentRO that happy-dom doesn't model;
      // the bug from the wheel-handler's perspective is exactly: escape
      // is true and the user is at distFromBottom <= 4.
      controller.setEscapedFromLock(true);
      // geom.scrollTop is already 400 (per beforeEach). distFromBottom =
      // 1000 - 400 - 600 = 0.
      expect(controller.escapedFromLock).toBe(true);
      expect(geom.scrollTop).toBe(400);

      // Wheel down. Browser would refuse to scroll past max → no scroll
      // event will fire. Re-stick MUST happen from the wheel handler.
      fireWheel(scrollEl, 100, scrollEl);

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('wheel-down at distFromBottom=0 + escaped re-sticks AND a subsequent streaming chunk pins', async () => {
      // Bug #2 continuation: after the wheel-handler re-stick, ordinary
      // streaming must follow. Confirms isAtBottomState + springStopRequested
      // are correctly reset so the next contentRO positive delta engages
      // the spring (mode='spring' in this describe block) — the user-visible
      // signal that re-stick "worked."
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      controller.setEscapedFromLock(true);
      expect(controller.escapedFromLock).toBe(true);

      fireWheel(scrollEl, 100, scrollEl);
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);

      // Streaming chunk arrives — spring should chase to new bottom.
      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await advanceUntil(() => geom.scrollTop === 800);
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('content shrink that clamps scrollTop DOWN to the bottom does NOT auto-re-stick (no scrolledDown trajectory)', async () => {
      // Adjacent invariant to bug #2: a content shrink moves scrollTop
      // DOWN to the new max. scrolledDown is false for any clamp event
      // (current scrollTop < previous), so willRestick=false even when
      // distFromBottomAtEvent=0. Layout-driven changes must not yank
      // an escaped reader to the bottom — only explicit user action
      // (wheel-at-boundary, chip click, etc.) re-engages.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Escape mid-thread, then scroll DOWN to within the visual band
      // but above the auto-follow epsilon so a future clamp at the
      // bottom has a baseline above the clamp value.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // Move forward to scrollTop=395 (baseline=395, distance=5,
      // outside the 4px auto-follow epsilon — does NOT re-stick yet).
      geom.scrollTop = 395;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // Content shrink: scrollHeight 1000 → 800. Browser clamps
      // scrollTop from 395 down to new max 200 (800 - 600). distance
      // is now 0 but scrolledDown = 200 > 395 = FALSE.
      geom.scrollHeight = 800;
      geom.contentHeight = 600;
      geom.scrollTop = 200;
      fireScroll(scrollEl);
      await nextTimer();

      // No re-stick from the clamp alone. User stays escaped — to
      // re-engage, they must wheel down (handleWheel re-stick path).
      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('wheel-down at distFromBottom > 4 + escaped records down intent without immediate re-stick', async () => {
      // Bug #2 fix scope: the wheel-handler re-stick is ONLY for the
      // at-bottom-lockout case. If the user is escaped but not at the
      // bottom, a wheel-down must record down intent so the deferred
      // scroll handler can run its normal re-stick logic when the
      // trajectory actually lands at the bottom. Otherwise the fix
      // would re-stick mid-thread on every downward wheel, defeating
      // the user's escape.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Escape with scrollTop well above the bottom.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100; // distFromBottom = 1000 - 100 - 600 = 300
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // Wheel down, but still far from bottom. Re-stick MUST NOT fire
      // from the wheel handler alone — recent down intent is reserved
      // for the next scroll event to interpret.
      fireWheel(scrollEl, 50, scrollEl);
      expect(controller.escapedFromLock).toBe(true);
    });

    it('recent down intent expires before a later bottom-landing scroll', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      fireWheel(scrollEl, 50, scrollEl);
      await waitMs(300);

      geom.scrollTop = 400;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('up input clears recent down intent before a bottom-landing scroll', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      fireWheel(scrollEl, 50, scrollEl);
      fireWheel(scrollEl, -10, scrollEl);
      geom.scrollTop = 400;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('up input before the deferred scroll handler prevents stale down-intent re-stick', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      fireWheel(scrollEl, 50, scrollEl);
      geom.scrollTop = 400;
      fireScroll(scrollEl);

      // The browser has queued the bottom-landing scroll event's
      // deferred handler, but the user immediately scrolls up again.
      // That fresh up input must clear the down-intent version so the
      // pending callback cannot re-stick from stale consent.
      fireWheel(scrollEl, -10, scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });
  });

  describe('edge cases', () => {
    it('forceStick called twice during warmup keeps the gate closed', async () => {
      // Idempotency: a second forceStick before warm fires must not
      // accidentally satisfy the quiet timer or leave warm=true. The
      // user reason no longer re-arms warmup, but attach already did
      // and the controller must preserve that gate.
      const ro = getRO();
      ro.fire(contentEl, 800);

      controller.forceStick();
      controller.forceStick();

      // Still warming — positive delta sync-pins, doesn't spring.
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      expect(geom.scrollTop).toBe(600); // sync-pin landed at target

      // Confirm no spring rAF is in flight.
      const after = geom.scrollTop;
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(after);
    });

    it('detach + reattach resets warm state and re-arms the gate', async () => {
      // Symmetric with the "detach cancels spring" test: detach should
      // also reset warm, so a re-attach starts fresh in sync-pin mode
      // even if the previous session warmed long ago.
      const firstRo = getRO();
      firstRo.fire(contentEl, 800);
      await waitMs(150); // warm

      controller.detach();

      // Re-stub geometry on a fresh element pair so attach kicks a new
      // contentRO. Use the same scrollEl — happy-dom doesn't care.
      controller.attach(scrollEl, contentEl);
      const secondRo = getRO();
      expect(secondRo).not.toBe(firstRo);

      // First post-attach positive delta must sync-pin (we're warming
      // again from scratch), not spring.
      secondRo.fire(contentEl, 800); // initial fire on the new RO
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      secondRo.fire(contentEl, 1000);
      expect(geom.scrollTop).toBe(600);

      // No spring rAF; confirm scrollTop stable.
      const after = geom.scrollTop;
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(after);
    });

    it('negative delta mid-spring lets the spring converge on the new (lower) target', async () => {
      // Negative deltas (content shrinking — Streamdown removing a
      // typesetting placeholder, a row collapsing) change the moving
      // bottom without cancelling an in-flight spring. The spring's
      // tick branch is `if (diff !== 0)`, so it chases a lower target
      // with the same damping/stiffness/mass as it chases higher
      // targets. This test isolates the symmetric-down convergence
      // path: pick a shrink whose overshoot magnitude stays under the
      // SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX (50 px) so the
      // overshoot guard does NOT snap, leaving the spring as the sole
      // writer. The complementary "small negative delta" test below
      // checks the synchronous no-snap and the final convergence; this
      // one captures the intermediate frame to prove the spring walks
      // DOWN gradually across rAF ticks instead of arriving in one
      // step.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      // Kick spring then take ONE frame so it's still well in flight
      // (positive velocity, lastTargetChangedAt fresh) when the shrink
      // fires. Avoids the retain-window-expired branch where the spring
      // has already arrived and springToken === 0.
      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      const beforeShrink = geom.scrollTop;
      expect(beforeShrink).toBeGreaterThan(400);
      expect(beforeShrink).toBeLessThan(800);

      // Shrink so the new target sits 40 px below beforeShrink — under
      // the 50 px threshold, so the overshoot guard does NOT snap.
      // Spring sees diff < 0 and damps `current` down toward target.
      const newTarget = beforeShrink - 40;
      const newScrollHeight = newTarget + 600;
      geom.scrollHeight = newScrollHeight;
      geom.contentHeight = newScrollHeight - 200;
      ro.fire(contentEl, geom.contentHeight);

      // Synchronous RO callback did not snap (threshold-gated guard).
      expect(geom.scrollTop).toBe(beforeShrink);

      // Continued damping converges on the new (lower) target. We
      // cannot assert an intermediate scrollTop strictly between
      // beforeShrink and newTarget on the next tick because the
      // spring's residual upward velocity from the kick produces a
      // would-be `next` greater than current — the browser clamps
      // scrollTop to scrollHeight - clientHeight = newTarget, so the
      // first post-shrink frame lands exactly at newTarget. The
      // bidirectional `if (diff !== 0)` branch is what allows velocity
      // to decay and the spring to settle there; the asymmetric
      // pre-fix branch (`if (current < target)`) skipped the velocity
      // update entirely and left the spring chasing forever.
      await advanceUntil(() => Math.abs(geom.scrollTop - newTarget) <= 1);
      expect(Math.abs(geom.scrollTop - newTarget)).toBeLessThanOrEqual(1);
    });

    it('small negative delta mid-spring (<50px overshoot) is absorbed by the symmetric spring', async () => {
      // The streaming jitter regression: parseIncompleteMarkdown
      // auto-closes a partial code fence between chunks, scrollHeight
      // transiently shrinks by a few pixels, then the next chunk
      // reopens it. Old behavior — the unconditional overshoot guard
      // snapped scrollTop downward inside the RO callback, and the
      // spring re-extended on the next tick — visible as per-chunk
      // up-down jitter. New behavior — overshoots below the threshold
      // fall through both guard clauses and are absorbed by the
      // symmetric spring (current > target damps toward target across
      // rAF ticks).
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      // Kick a small spring chase: target 500, spring starts at 400.
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      await nextFrame();
      const midScrollTop = geom.scrollTop;
      expect(midScrollTop).toBeGreaterThan(400);
      expect(midScrollTop).toBeLessThan(500);

      // Shrink so the new target is ~30 px below midScrollTop —
      // overshoot magnitude ≈ 30, comfortably under the 50 px
      // threshold.
      const newTarget = midScrollTop - 30;
      const newScrollHeight = newTarget + 600;
      geom.scrollHeight = newScrollHeight;
      geom.contentHeight = newScrollHeight - 200;
      ro.fire(contentEl, geom.contentHeight);

      // Critical assertion: scrollTop did NOT snap inside the RO
      // callback. The threshold-gated overshoot guard let the spring
      // be the single writer.
      expect(geom.scrollTop).toBe(midScrollTop);

      // The symmetric spring converges to the new (lower) target
      // across ticks.
      await advanceUntil(() => Math.abs(geom.scrollTop - newTarget) <= 1);
    });

    it('large negative delta mid-spring (>50px overshoot) still snaps instantly via threshold bypass', async () => {
      // The "negative delta mid-spring lets the spring converge" test
      // above asserts the post-snap value. This isolates the "snap
      // happens inside the RO callback" timing — the symmetric spring
      // would have taken many frames to converge from a 100+ px
      // overshoot, and the threshold bypasses that path because the
      // user would see the viewport drift down across many frames.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      await nextFrame();
      const midScrollTop = geom.scrollTop;
      expect(midScrollTop).toBeGreaterThan(400);

      // 100 px overshoot — well over threshold.
      const newTarget = midScrollTop - 100;
      const newScrollHeight = newTarget + 600;
      geom.scrollHeight = newScrollHeight;
      geom.contentHeight = newScrollHeight - 200;
      ro.fire(contentEl, geom.contentHeight);

      // Snapped inside the RO callback, before any further rAF.
      expect(geom.scrollTop).toBe(newTarget);
    });

    it('streamdown token-close-then-reopen pattern (~22px shrink mid-chunk) does not jump the viewport', async () => {
      // Reproduces the user-visible streaming bug: while streaming
      // chat text through svelte-streamdown with
      // parseIncompleteMarkdown=true, an unclosed code fence is
      // auto-balanced (DOM transiently shrinks), then the next chunk
      // reopens it (DOM grows again). Pre-fix, the shrink's
      // unconditional overshoot guard snapped scrollTop downward
      // inside the RO callback; the spring was already mid-chase and
      // re-extended on the next tick — visible as per-chunk up-down
      // jitter on plain-text streams that contain partial markdown
      // tokens.
      //
      // For the bug to trigger, the spring must be near its target
      // when the shrink lands so scrollTop > new target (overshoot).
      // Streaming reproduces this naturally because the spring chases
      // each chunk's growth to within a pixel before the next one
      // arrives; we mirror it by advancing many frames after the
      // initial kick so the spring is close to converged.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Small chunk arrives. Smaller kick reaches near-target inside
      // the retain window: the spring is overdamped so it approaches
      // target exponentially and a 50 px chase settles in a handful
      // of frames (well under RETAIN_ANIMATION_DURATION_MS = 350 ms).
      // Use advanceUntil so the test is not coupled to specific
      // spring-tuning constants (damping/stiffness/mass).
      geom.scrollHeight = 1050;
      geom.contentHeight = 850;
      ro.fire(contentEl, 850); // target=450, spring starts at 400

      // Wait for the spring to reach near its target.
      await advanceUntil(() => geom.scrollTop >= 440);
      const beforeShrink = geom.scrollTop;
      expect(beforeShrink).toBeLessThanOrEqual(450);

      // parseIncompleteMarkdown closes an unclosed fence — content
      // shrinks by ~22 px (well below the 50 px threshold). Spring is
      // still chasing (springToken !== 0); new target ≈ beforeShrink
      // - 22, so scrollTop > target → overshoot magnitude ≈ 22.
      const shrinkPx = 22;
      geom.scrollHeight = 1050 - shrinkPx;
      geom.contentHeight = 850 - shrinkPx;
      ro.fire(contentEl, geom.contentHeight);

      // Bug regression: scrollTop must NOT snap inside the RO call.
      // Pre-fix the unconditional overshoot guard would have written
      // scrollTop to the new (lower) target before this line returned.
      expect(geom.scrollTop).toBe(beforeShrink);

      // The next chunk grows scrollHeight back; the spring is still
      // in flight (the carve-out's else branch bumped
      // lastTargetChangedAt on the suppressed negative write, keeping
      // the retain window alive across the shrink) and chases the new
      // (higher) target. The synchronous RO callback for the new
      // chunk does not cause another snap.
      geom.scrollHeight = 1070;
      geom.contentHeight = 870;
      ro.fire(contentEl, 870);
      expect(geom.scrollTop).toBe(beforeShrink);

      // Spring converges to the final target.
      await advanceUntil(() => Math.abs(geom.scrollTop - 470) <= 1);
    });

    it('estimate-correct pair during spring leaves spring as single writer (no sync-pin race)', async () => {
      // Bug B: the engine's row-append cycle fires contentRO twice within
      // ~5ms — first at ESTIMATED_ROW_SIZE (e.g., +90 for chat's
      // estimate), then at the measured size (correction, e.g., -56).
      // Pre-fix, the +90 started a spring and the -56 ran the
      // negative-delta sync-pin synchronously, landing scrollTop at
      // the corrected target before the spring's first paint — the
      // spring then ticked against current==target with no visible
      // motion. New gate: negative-delta sync-pin is suppressed when
      // springToken !== 0, leaving the spring as the single writer.
      // It reads targetScrollTop() each tick and absorbs the corrected
      // target on its next frame.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      // +90 estimate-grow (mimics the engine provisionally placing a new
      // row at ESTIMATED_ROW_SIZE). Spring engages.
      geom.scrollHeight = 1090;
      geom.contentHeight = 890;
      ro.fire(contentEl, 890);
      // Spring not yet ticked — scrollTop unchanged inside the RO call.
      expect(geom.scrollTop).toBe(400);

      // Advance one frame so the spring runs its first tick and
      // scrollTop walks part of the way toward the estimate-target (490).
      await nextFrame();
      const midScrollTop = geom.scrollTop;
      expect(midScrollTop).toBeGreaterThan(400);
      expect(midScrollTop).toBeLessThan(490);

      // -56 correction within the same RO burst (the engine measured the
      // actual row size, totalSize -= 56). negativeWillPin would be
      // true; the spring carve-out must SUPPRESS the sync write.
      // Corrected target = 1034 - 600 = 434.
      geom.scrollHeight = 1034;
      geom.contentHeight = 834;
      ro.fire(contentEl, 834);

      // Critical assertion: scrollTop did NOT jump to the corrected
      // target inside the RO callback. The spring is the single
      // writer; its position is unchanged from the mid-chase value.
      expect(geom.scrollTop).toBe(midScrollTop);

      // Subsequent frames: the spring reads the new targetScrollTop()
      // (434) and walks scrollTop there over multiple ticks — visible
      // motion, not an instantaneous snap.
      await advanceUntil(() => geom.scrollTop === 434);
      expect(geom.scrollTop).toBe(434);
    });

    describe('content height oscillation during sentinel — browser auto-clamp', () => {
      // When contentEl oscillates (-Npx then +Npx from async
      // Streamdown typesetting), the browser auto-clamps scrollTop
      // during the low point (a native engine operation — not a
      // scrollTop write the controller could arbitrate). When
      // contentEl height restores, scrollTop
      // is stranded at the clamped position and the sentinel sees
      // diff > 0. Current behavior: the sentinel enters spring physics
      // and visibly chases back to the original target — a 105px
      // animation for zero net content change.
      //
      // Fix: track the target at sentinel entry. When the sentinel
      // tick sees diff > 0 but target === sentinelEntryTarget, the
      // oscillation returned to the same height — snap instantly
      // instead of spring-chasing. When target !== sentinelEntryTarget,
      // it's real content growth — spring-chase normally.

      it('content oscillation returning to sentinel entry target snaps instead of springing', async () => {
        setUiRenderTraceEnabled(true);
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150);

        // Spring chases to target 500, arrives, enters sentinel.
        geom.scrollHeight = 1100;
        geom.contentHeight = 900;
        ro.fire(contentEl, 900); // target = 500
        await advanceUntil(() => geom.scrollTop === 500);
        while (mockNow < 360) await nextFrame(); // past retain → sentinel
        // Advance well past the sentinel snap age guard (50ms) so the
        // oscillation snap is eligible. The trace showed ~180ms
        // between sentinel entry and the oscillation.
        while (mockNow < 500) await nextFrame();

        // Content oscillation: -105 then +105 (async typesetting).
        // The -105 makes browser auto-clamp scrollTop from 500 to 395.
        geom.scrollHeight = 995; // scrollHeight - 105
        geom.contentHeight = 795;
        geom.scrollTop = 395; // browser auto-clamp: max = 995-600 = 395
        ro.fire(contentEl, 795); // contentRO delta = -105

        // Grow back immediately.
        geom.scrollHeight = 1100;
        geom.contentHeight = 900;
        ro.fire(contentEl, 900); // contentRO delta = +105, target back to 500

        // Next sentinel tick should SNAP back to 500 (target ===
        // sentinel entry target), not start a spring chase.
        await nextFrame();

        // DESIRED: scrollTop snapped to 500 in one frame.
        expect(geom.scrollTop).toBe(500);
      });

      it('oscillation recovery snaps SYNCHRONOUSLY in the contentRO callback (no rAF gap)', async () => {
        // Regression: bug-report-20260615T182227Z. An above-viewport row
        // remeasure (an image user-message row remounted by windowing)
        // transiently dips contentEl, the browser auto-clamps scrollTop
        // during the dip, then the row regrows and strands scrollTop below
        // the restored bottom. The spring-tick oscillationSnap recovers it
        // — but rAF callbacks fire BEFORE ResizeObserver callbacks within a
        // frame (HTML "update the rendering"), so a snap reacting to the
        // regrow's RO delivery always lands one frame late, painting the
        // stranded position as a one-frame jump. Recovery must happen
        // synchronously inside the regrow's contentRO callback, before any
        // rAF tick.
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150);

        // Spring chases to target 500, arrives, enters sentinel
        // (sentinelEntryTarget = 500).
        geom.scrollHeight = 1100;
        geom.contentHeight = 900;
        ro.fire(contentEl, 900); // target = 500
        await advanceUntil(() => geom.scrollTop === 500);
        while (mockNow < 360) await nextFrame(); // past retain → sentinel

        // Dip: above-viewport row shrinks -37, browser auto-clamps
        // scrollTop 500 → 463 (max = 1063 - 600).
        geom.scrollHeight = 1063;
        geom.contentHeight = 863;
        geom.scrollTop = 463; // native clamp during the low point
        ro.fire(contentEl, 863); // contentRO delta = -37
        // The dip itself does not snap (target 463 ≠ sentinel entry 500);
        // the negative-delta carve-out leaves scrollTop at the clamp.
        expect(geom.scrollTop).toBe(463);

        // Regrow: row returns +37, total back to the pre-dip value, target
        // back to the sentinel entry (500), scrollTop still stranded at 463.
        geom.scrollHeight = 1100;
        geom.contentHeight = 900;
        ro.fire(contentEl, 900); // contentRO delta = +37

        // FIX: re-pinned to the bottom synchronously inside the ro.fire
        // above — asserted WITHOUT awaiting a frame. Without the fix
        // scrollTop is still 463 here and only the next spring tick (rAF)
        // would recover it.
        expect(geom.scrollTop).toBe(500);

        // And it stays put — no late spring-tick correction re-moves it.
        for (let i = 0; i < 5; i++) await nextFrame();
        expect(geom.scrollTop).toBe(500);
      });

      it('real content growth during sentinel still spring-chases normally', async () => {
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150);

        geom.scrollHeight = 1100;
        geom.contentHeight = 900;
        ro.fire(contentEl, 900);
        await advanceUntil(() => geom.scrollTop === 500);
        while (mockNow < 360) await nextFrame();

        // Real content growth: target moves to a NEW value (not
        // the sentinel entry value). Should spring-chase, not snap.
        geom.scrollHeight = 1200;
        geom.contentHeight = 1000;
        ro.fire(contentEl, 1000); // target = 600

        await nextFrame();
        // Spring should be interpolating, not snapping.
        expect(geom.scrollTop).toBeGreaterThan(500);
        expect(geom.scrollTop).toBeLessThan(600);

        await advanceUntil(() => geom.scrollTop === 600);
      });

      it('content oscillation to a DIFFERENT height spring-chases (not snap)', async () => {
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150);

        geom.scrollHeight = 1100;
        geom.contentHeight = 900;
        ro.fire(contentEl, 900);
        await advanceUntil(() => geom.scrollTop === 500);
        while (mockNow < 360) await nextFrame();

        // Oscillation that does NOT return to sentinel entry value:
        // target goes 500 → 400 → 520 (net +20px growth).
        geom.scrollHeight = 1000;
        geom.contentHeight = 800;
        geom.scrollTop = 400;
        ro.fire(contentEl, 800);
        geom.scrollHeight = 1120;
        geom.contentHeight = 920;
        ro.fire(contentEl, 920); // target = 520 ≠ sentinelEntry(500)

        // Should spring-chase to 520 (different target), not snap.
        await nextFrame();
        expect(geom.scrollTop).toBeGreaterThan(400);
        expect(geom.scrollTop).toBeLessThan(520);
        await advanceUntil(() => geom.scrollTop === 520);
      });

      it('estimate-correct pair during active chase still uses carve-out (no snap regression)', async () => {
        const ro = getRO();
        ro.fire(contentEl, 800);
        await waitMs(150);

        // Start a fresh spring chase (NOT in sentinel).
        geom.scrollHeight = 1400;
        geom.contentHeight = 1200;
        ro.fire(contentEl, 1200); // target = 800
        await nextFrame();
        const midChase = geom.scrollTop;
        expect(midChase).toBeGreaterThan(400);
        expect(midChase).toBeLessThan(800);

        // Estimate-correct pair mid-chase: +90 then -56.
        geom.scrollHeight = 1490;
        geom.contentHeight = 1290;
        ro.fire(contentEl, 1290); // +90 → target = 890

        geom.scrollHeight = 1434;
        geom.contentHeight = 1234;
        ro.fire(contentEl, 1234); // -56 → target = 834

        // The -56 should NOT sync-pin (carve-out suppresses it
        // because the spring is actively chasing, not in sentinel).
        expect(geom.scrollTop).toBe(midChase);

        // Spring should arrive at the corrected target.
        await advanceUntil(() => geom.scrollTop === 834);
      });
    });

    it('spring stays sentinel-alive during streaming when arrived past the retain window — compensations stay declined', async () => {
      // The inter-chunk dead-zone bug: spring arrives, 350ms pass with
      // no new content (async shiki load, parseIncompleteMarkdown
      // rebalance, slow model cadence), cancelSpring sets springToken=0.
      // In that window a routed engine compensation resolves through the
      // pass tier and negative contentRO deltas sync-pin — both produce
      // a user-visible instant snap mid-stream where the spring should
      // have smoothly chased. Code blocks exacerbate this because shiki
      // language loads are async and parseIncompleteMarkdown rebalances
      // create timing gaps > 350ms.
      //
      // Fix: when arrived but animationMode is still 'spring', the
      // spring re-rAFs as a sentinel (springToken stays non-zero) so the
      // resolver's decline tier and negative-delta carve-out remain
      // engaged. The next chunk's positive contentRO delta bumps
      // lastTargetChangedAt and the chase resumes.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      // Kick a spring chase: content grows, spring starts.
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900); // target = 500

      // Let the spring fully arrive AND advance mockNow past
      // RETAIN_ANIMATION_DURATION_MS (350ms). advanceUntil advances
      // mockNow by 16.67ms per frame; ~25 frames reaches 416ms.
      // lastTargetChangedAt was set when the positive contentRO fired
      // (mockNow=0 at that point), so mockNow > 350 means the retain
      // window has expired. The sentinel branch keeps the spring alive.
      await advanceUntil(() => geom.scrollTop === 500);
      // Ensure we're past the retain window.
      while (mockNow < 360) await nextFrame();

      // Widen the scroll range so the compensation target sits above the
      // current position without being clamped by the stub geometry
      // (maxTarget = scrollHeight - clientHeight). This isolates the
      // decline decision from clamping.
      geom.scrollHeight = 1200;
      // contentHeight unchanged — no contentRO fire.

      // The engine compensates for an above-viewport row remeasure while the
      // model is between chunks. The sentinel keeps springActive true, so
      // the resolver declines — without the sentinel this would resolve
      // through the pass tier and snap (the regression).
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: 10, target: 510 })).toBe(false);
      expect(geom.scrollTop).toBe(500);

      // Similarly, a small negative contentRO delta (parseIncomplete
      // Markdown rebalance) must NOT sync-pin.
      geom.scrollHeight = 1190;
      geom.contentHeight = 890;
      ro.fire(contentEl, 890); // target = 590, current=500, no overshoot
      expect(geom.scrollTop).toBe(500);

      // Next streaming chunk arrives — spring resumes chasing.
      geom.scrollHeight = 1300;
      geom.contentHeight = 1100;
      ro.fire(contentEl, 1100); // target = 700
      await advanceUntil(() => Math.abs(geom.scrollTop - 700) <= 1);
      expect(Math.abs(geom.scrollTop - 700)).toBeLessThanOrEqual(1);
    });

    it('spring sentinel cancels cleanly when animationMode flips to instant (turn ends)', async () => {
      // The sentinel must not keep the spring alive after the turn ends.
      // When animationMode flips to 'instant', the next sentinel tick
      // sees wantsSpringNow=false, the retain window is expired, and the
      // standard cancel branch fires.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      await advanceUntil(() => geom.scrollTop === 500);
      // Ensure past retain window so spring enters sentinel.
      while (mockNow < 360) await nextFrame();

      // Turn ends — mode flips to instant. The sentinel tick's next
      // arrival evaluation sees wantsSpringNow=false and takes the
      // cancel branch (springToken→0). Multiple frames because the
      // sentinel's pending rAF and nextFrame's rAF interleave — the
      // cancel may land one frame after the mode flip is visible.
      mode = 'instant';
      for (let i = 0; i < 10; i++) await nextFrame();

      // Widen scrollHeight so the compensation value isn't clamped by
      // the stub geometry (maxTarget = scrollHeight - clientHeight must
      // exceed the write value).
      geom.scrollHeight = 1200;

      // A routed compensation now passes (springToken should be 0 after
      // the instant-mode cancel, so the decline tier can't fire).
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: 10, target: 510 })).toBe(true);
      expect(geom.scrollTop).toBe(510);
    });

    it('text selection mid-spring pauses scrollTop advancement', async () => {
      // The spring tick checks isSelectingInside() and re-rAFs without
      // advancing scrollTop when the user is dragging a selection
      // across the scroll element. This is the spring counterpart to
      // the scroll-handler escape-on-selection path: gestures the user
      // takes mid-stream must not be fought.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame(); // let spring run one tick
      const beforeSelect = geom.scrollTop;
      expect(beforeSelect).toBeGreaterThan(400);
      expect(beforeSelect).toBeLessThan(800);

      // Begin a selection drag inside scrollEl.
      document.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
      const fakeRange = { commonAncestorContainer: scrollEl } as unknown as Range;
      vi.spyOn(window, 'getSelection').mockReturnValue({
        rangeCount: 1,
        getRangeAt: () => fakeRange,
      } as unknown as Selection);

      // Spring should be paused — additional frames don't advance scrollTop.
      for (let i = 0; i < 10; i++) await nextFrame();
      expect(geom.scrollTop).toBe(beforeSelect);

      // Release the selection: spring resumes and lands at target.
      document.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
      vi.spyOn(window, 'getSelection').mockReturnValue(null);
      await advanceUntil(() => geom.scrollTop === 800);
    });

    it('re-stick after wheel-up escape re-arms the spring for subsequent streaming chunks', async () => {
      // Regression for the springStopRequested re-arm bug. After
      // setEscapedFromLock(true) the controller sets
      // springStopRequested=true, which would permanently disable the
      // spring even if the user later scrolls back to the bottom. The
      // scroll-handler's re-stick path must reset it so the next
      // streaming chunk engages the spring again.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      // User wheel-ups: spring stops, escape sets when the outer scroll moves.
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 200;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      // User scrolls BACK to the bottom (re-stick path).
      geom.scrollTop = 100; // intermediate
      fireScroll(scrollEl);
      await nextTimer();
      fireWheel(scrollEl, 100, scrollEl);
      geom.scrollTop = 400; // right at bottom; distance=0
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);

      // Next streaming chunk arrives — spring should engage again.
      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);

      // Confirm spring is interpolating, not sync-pinning.
      expect(geom.scrollTop).toBeLessThan(800);
      expect(geom.scrollTop).toBeGreaterThanOrEqual(400);
      await advanceUntil(() => geom.scrollTop === 800);
    });
  });

  describe('engine compensation applier (routed compensation writes)', () => {
    // The virtualizer's onCompensation observations land in
    // applyEngineCompensation. Decision policy is exhaustively covered
    // in scroll/resolver.test.ts (resolveEngineCompensation); these
    // tests pin the controller-side choreography — chokepoint routing,
    // attribution, self-tagging, and the decline/detach return
    // contract.

    it('applies a dormant compensation through the write chokepoint (attributed + self-tagged)', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      // Above-viewport growth: the engine reports a +100 delta, absolute
      // target 500 (offset 400 + 100). No chase in flight.
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      setUiRenderTraceEnabled(true);
      clearUiRenderTrace();
      const handled = controller.applyEngineCompensation({ kind: 'remeasure-above', delta: 100, target: 500 });

      expect(handled).toBe(true);
      expect(geom.scrollTop).toBe(500);
      const writes = getUiRenderTraceRecords().filter((r) => r.label.startsWith('scroll.write'));
      expect(writes).toHaveLength(1);
      expect((writes[0].data as { caller: string }).caller).toBe('engine.compensation');

      // Routed writes are controller writes: the scroll event they
      // dispatch is token-tagged and consumed, never read as user intent.
      clearUiRenderTrace();
      fireScroll(scrollEl);
      await nextTimer();
      expect(
        getUiRenderTraceRecords().filter((r) => r.label.startsWith('scroll.scrollEvent')),
      ).toEqual([]);
      expect(controller.escapedFromLock).toBe(false);
    });

    it('applyScrollTarget writes through the chokepoint: attributed, self-tagged, intent preserved', async () => {
      // The virtualizer's scrollToIndex convergence passes route every
      // write here instead of assigning scrollTop themselves, so the
      // resulting scroll event is token-tagged (never read as user
      // intent) and trace-attributed. Intent stays with the caller —
      // the write itself must not flip escape or re-stick.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      setUiRenderTraceEnabled(true);
      clearUiRenderTrace();
      controller.applyScrollTarget(120);

      expect(geom.scrollTop).toBe(120);
      const writes = getUiRenderTraceRecords().filter((r) => r.label.startsWith('scroll.write'));
      expect(writes).toHaveLength(1);
      expect((writes[0].data as { caller: string }).caller).toBe('virtualizer.scrollTarget');
      expect(controller.escapedFromLock).toBe(false);

      clearUiRenderTrace();
      fireScroll(scrollEl);
      await nextTimer();
      expect(
        getUiRenderTraceRecords().filter((r) => r.label.startsWith('scroll.scrollEvent')),
      ).toEqual([]);
      expect(controller.escapedFromLock).toBe(false);
    });

    it('declines a small compensation mid-chase (no write; spring stays the single writer)', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Start a spring chase.
      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      const springStart = geom.scrollTop;
      expect(springStart).toBeGreaterThan(400);
      expect(springStart).toBeLessThan(800);

      // Small mid-chase compensation: declined, nothing written; the
      // engine re-syncs from the next real scroll event on its own.
      const handled = controller.applyEngineCompensation({ kind: 'remeasure-above', delta: 10, target: springStart + 10 });
      expect(handled).toBe(false);
      expect(geom.scrollTop).toBe(springStart);

      // The chase still completes.
      await advanceUntil(() => geom.scrollTop === 800);
    });

    it('redirects a pinned moves-away compensation to the bottom target', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      expect(geom.scrollTop).toBe(400); // pinned at bottom (target 400)

      setUiRenderTraceEnabled(true);
      clearUiRenderTrace();
      // Above-viewport shrink compensation: the engine wants 300,
      // meaningfully above the pinned bottom.
      const handled = controller.applyEngineCompensation({ kind: 'remeasure-above', delta: -100, target: 300 });

      expect(handled).toBe(true);
      expect(geom.scrollTop).toBe(400); // stayed at the bottom
      const writes = getUiRenderTraceRecords().filter((r) => r.label.startsWith('scroll.write'));
      expect(writes).toHaveLength(1);
      expect((writes[0].data as { caller: string; requested: number }).caller).toBe('engine.anchorRedirect');
      expect((writes[0].data as { caller: string; requested: number }).requested).toBe(400);
    });

    it('returns false when detached (nothing to write; the engine needs no re-sync)', () => {
      controller.detach();
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: 100, target: 500 })).toBe(false);
    });

    it('post-restore warm settle re-arms the mid-chase decline (restore does not permanently disarm)', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      controller.armRestoreSnap();
      controller.forceStick({ reason: 'restore' });
      expect(controller.isWarm).toBe(false);

      // Settle the restore warm-up: fire RO once to start the quiet
      // timer, then wait it out.
      ro.fire(contentEl, 800);
      await waitMs(150);
      expect(controller.isWarm).toBe(true);

      // Start a spring chase and verify the decline tier suppresses
      // small compensations again.
      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      const midChase = geom.scrollTop;
      expect(midChase).toBeGreaterThan(400);
      expect(midChase).toBeLessThan(800);

      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: 20, target: midChase + 20 })).toBe(false);
      expect(geom.scrollTop).toBe(midChase);
    });

    it('sentinel + content-latch hold cover a wire-round gap — pinned moves-away compensation is redirected', async () => {
      // The historical "snap up, spring down" cycle: between wire rounds
      // (a tool round-trip, the end-of-turn drain) the pane stops
      // stamping lastLiveContentAt; the content-keyed latch holds
      // 'spring' for SPRING_MODE_HOLD_MS so the sentinel survives the
      // gap and springActive stays true. An engine compensation arriving
      // in the gap that would move a pinned viewport away from the
      // bottom is redirected — and because the resolver's redirect tier
      // outranks both decline and pass, the no-displacement outcome
      // holds even if the sentinel has already died (the reason the
      // HOLD > RETAIN cross-file invariant could be retired). Uses the
      // REAL production latch so this can't drift from MessageTimeline.
      let contentAdvancing = true;
      let lastContentAt = 0;
      controller.detach();
      controller = createUseStickToBottomController({
        animationMode: () => {
          if (contentAdvancing) lastContentAt = mockNow;
          return latchedSpringMode(mockNow, lastContentAt, SPRING_MODE_HOLD_MS);
        },
      });
      controller.attach(scrollEl, contentEl);
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      await advanceUntil(() => geom.scrollTop === 500); // pinned at bottom
      while (mockNow < 360) await nextFrame(); // retain expired → sentinel

      // Wire-round gap: content stops advancing, well within the hold.
      contentAdvancing = false;
      for (let i = 0; i < 5; i++) await nextFrame();

      // The latch-held sentinel is what keeps springActive true through
      // the gap: a small non-moves-away compensation is DECLINED (widen
      // the range so the DOM is not pinned and the target isn't
      // clamped). With a broken latch the sentinel cancels on its next
      // tick and this would resolve through the pass tier instead.
      geom.scrollHeight = 1200;
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: 10, target: 510 })).toBe(false);
      expect(geom.scrollTop).toBe(500);
      geom.scrollHeight = 1100; // pinned again (bottomTarget back to 500)

      // The engine requests a stale anchor 60px above the pinned bottom.
      // Redirect outranks decline, so this holds with or without the
      // sentinel — it pins the no-displacement outcome itself.
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: -60, target: 440 })).toBe(true);
      expect(geom.scrollTop).toBe(500); // redirected to the bottom target
    });

    it('passes a mid-chase compensation during a width-reflow settle window', async () => {
      // Pins the controller-side `widthReflowActive` sampling
      // (contentReflowSettleUntil > nowMs()) on the engine path: during a
      // width reflow the paired contentRO sync-pins, so the compensation
      // must land in the same paint instead of hitting the mid-chase
      // decline. If the sampling regressed (dropped / wrong clock /
      // wrong window), the decline tier would fire and this returns
      // false.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      // Start a chase (height-only growth, no width change).
      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      const midChase = geom.scrollTop;
      expect(midChase).toBeGreaterThan(400);
      expect(midChase).toBeLessThan(800);

      // Width change mid-chase (same height → delta 0, but the reflow
      // settle window opens before the early return).
      ro.fire(contentEl, 1200, 640);

      // Small jump that the decline tier would otherwise suppress.
      expect(controller.applyEngineCompensation({ kind: 'remeasure-above', delta: 20, target: midChase + 20 })).toBe(true);
      expect(geom.scrollTop).toBe(midChase + 20);
    });
  });

  describe("observe('live-content') during sentinel — structural signature nudges", () => {
    it('does not restart spring when scrollTop equals target', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      await advanceUntil(() => geom.scrollTop === 500);
      while (mockNow < 360) await nextFrame();

      // Sentinel running. scrollTop === target === 500.
      const scrollTopBefore = geom.scrollTop;
      controller.observe('live-content');

      // Should not move scrollTop — already at bottom, nothing grew.
      expect(geom.scrollTop).toBe(scrollTopBefore);
      for (let i = 0; i < 10; i++) await nextFrame();
      expect(geom.scrollTop).toBe(scrollTopBefore);
    });

    it("multiple observe('live-content') calls during sentinel do not cause visible scroll motion", async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      await advanceUntil(() => geom.scrollTop === 500);
      while (mockNow < 360) await nextFrame();

      const scrollTopBefore = geom.scrollTop;
      // Simulate 4 structural signature changes (tool call lifecycle:
      // running → streaming → success → tool_completion).
      for (let i = 0; i < 4; i++) {
        controller.observe('live-content');
        await nextFrame();
        await nextFrame();
      }

      // scrollTop must not have moved — no content grew.
      expect(geom.scrollTop).toBe(scrollTopBefore);
    });

    it("observe('live-content') with scrollTop 1px below target bumps retain but does not restart spring", async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      await advanceUntil(() => geom.scrollTop === 500);
      while (mockNow < 360) await nextFrame();

      // Simulate sub-pixel rounding leaving scrollTop 1px short.
      geom.scrollTop = 499;
      controller.observe('live-content');

      // Should NOT start a visible spring animation for 1px.
      // The nudge bumps lastTargetChangedAt and calls
      // startSpringIfNeeded which returns (spring still in sentinel).
      await nextFrame();
      await nextFrame();
      // Spring sentinel absorbs the 1px and lands at 500.
      await advanceUntil(() => geom.scrollTop === 500);
    });

    it('does not keep writing when browser readback stays one pixel short of target', async () => {
      controller.detach();
      const writes: number[] = [];
      stubGeometry(scrollEl, contentEl, geom, {
        setScrollTop: (value, g) => {
          writes.push(value);
          const max = Math.max(0, g.scrollHeight - g.clientHeight);
          const clamped = Math.max(0, Math.min(value, max));
          g.scrollTop = clamped >= max ? Math.max(0, max - 1) : clamped;
        },
      });
      controller = createUseStickToBottomController({ animationMode: () => mode });
      controller.attach(scrollEl, contentEl);

      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);
      writes.length = 0;

      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);

      await advanceUntil(() => Math.abs(geom.scrollTop - 500) <= 1);
      while (mockNow < 360) await nextFrame();

      const writesAfterSettling = writes.length;
      for (let i = 0; i < 20; i++) await nextFrame();

      expect(geom.scrollTop).toBe(499);
      expect(writes.length).toBe(writesAfterSettling);
    });

    it('delayed structural nudge does not snap a small corrected-target overshoot mid-spring', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      // Row identity changed at the tail (for example Read ->
      // read_group). Virtua first publishes the 90px estimate, which
      // starts a spring chase toward target 490.
      geom.scrollHeight = 1090;
      geom.contentHeight = 890;
      ro.fire(contentEl, 890);

      // Let the spring chase far enough that the later measured
      // correction leaves scrollTop slightly above the true target.
      // This is the exact case contentRO's small-overshoot threshold
      // is meant to damp instead of snapping.
      await advanceUntil(() => geom.scrollTop > 445);
      const beforeCorrection = geom.scrollTop;

      // The row measures at 40px. Corrected target = 1040 - 600 =
      // 440. contentRO must not snap the small overshoot; the spring
      // remains the single writer and will damp down naturally.
      geom.scrollHeight = 1040;
      geom.contentHeight = 840;
      ro.fire(contentEl, 840);
      expect(geom.scrollTop).toBe(beforeCorrection);
      expect(geom.scrollTop - 440).toBeGreaterThan(0);
      expect(geom.scrollTop - 440).toBeLessThan(50);

      // MessageTimeline's structural-signature nudge runs after
      // tick+rAF. It must not bypass the contentRO overshoot policy
      // and instantly clamp the same small overshoot.
      controller.observe('live-content');
      expect(geom.scrollTop).toBe(beforeCorrection);

      await advanceUntil(() => Math.abs(geom.scrollTop - 440) <= 1);
    });
  });
});
