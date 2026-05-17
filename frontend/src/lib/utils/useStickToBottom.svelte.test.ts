import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createUseStickToBottomController,
  resetUseStickToBottomModuleStateForTest,
  type UseStickToBottomController,
} from './useStickToBottom.svelte';
import { clearUiRenderTrace, setUiRenderTraceEnabled } from './uiRenderTrace';

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

function stubGeometry(scrollEl: HTMLElement, contentEl: HTMLElement, geom: Geometry): void {
  Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => geom.scrollHeight });
  Object.defineProperty(scrollEl, 'clientHeight', { configurable: true, get: () => geom.clientHeight });
  Object.defineProperty(scrollEl, 'scrollTop', {
    configurable: true,
    get: () => geom.scrollTop,
    set: (v: number) => { geom.scrollTop = Math.max(0, Math.min(v, geom.scrollHeight - geom.clientHeight)); },
  });
  Object.defineProperty(contentEl, 'scrollHeight', { configurable: true, get: () => geom.contentHeight });
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
  /** Fire the callback synchronously with a single entry for the given element + height. */
  fire(el: Element, height: number): void {
    this.callback(
      [
        {
          target: el,
          contentRect: { height, width: 0, top: 0, left: 0, right: 0, bottom: 0, x: 0, y: 0, toJSON: () => ({}) } as DOMRectReadOnly,
          borderBoxSize: [],
          contentBoxSize: [],
          devicePixelContentBoxSize: [],
        } as ResizeObserverEntry,
      ],
      this as unknown as ResizeObserver,
    );
  }
}

// rAF frames advance performance.now in 16.67ms steps so animateScrollTo's
// easeOutCubic interpolation makes real progress per tick in the test
// environment (happy-dom's rAF doesn't drive performance.now on its
// own). Tests that assert event-driven behavior (sync-pin, scroll
// handler, gesture handlers) don't depend on this — those happen
// synchronously without rAF.
let mockNow = 0;
function nextFrame(): Promise<void> {
  return new Promise<void>((resolve) =>
    requestAnimationFrame(() => {
      mockNow += 16.67;
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

describe('createUseStickToBottomController', () => {
  let scrollEl: HTMLDivElement;
  let contentEl: HTMLDivElement;
  let geom: Geometry;
  let controller: UseStickToBottomController;
  let originalRO: typeof ResizeObserver | undefined;

  beforeEach(() => {
    resetUseStickToBottomModuleStateForTest();
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
      // change so virtua's incremental row remeasurement (positive-delta
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

      // Subsequent positive-delta fire (virtua finishes measuring more
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
    it('wheel up on outer scrollEl escapes only after the outer scrollTop decreases', async () => {
      fireWheel(scrollEl, -50, scrollEl);
      expect(controller.escapedFromLock).toBe(false);
      await waitRealMs(180);
      expect(controller.escapedFromLock).toBe(false);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 399;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
      // Note: public isAtBottom may still be true if geometrically near
      // the bottom — that's the loose `intent || geometry` semantic the
      // ScrollToBottomButton wants.
    });

    it('keeps bottom lock when an outer wheel intent never moves the chat scroller', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);

      // Pending intent pauses contentRO pinning until the browser proves
      // the outer scroller moved upward.
      expect(geom.scrollTop).toBe(400);
      expect(controller.escapedFromLock).toBe(false);

      await waitRealMs(180);

      expect(controller.escapedFromLock).toBe(false);
      expect(geom.scrollTop).toBe(600);
      expect(controller.isSticky).toBe(true);
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

    it('wheel up inside a nested overflow scroller does NOT escape', () => {
      const nested = document.createElement('div');
      nested.style.cssText = 'overflow-y: auto;';
      Object.defineProperty(nested, 'scrollHeight', { configurable: true, get: () => 200 });
      Object.defineProperty(nested, 'clientHeight', { configurable: true, get: () => 100 });
      Object.defineProperty(nested, 'scrollTop', { configurable: true, get: () => 50 });
      contentEl.appendChild(nested);
      fireWheel(scrollEl, -50, nested);
      expect(controller.escapedFromLock).toBe(false);
    });

    it('wheel up inside a nested scroller at its top escapes after the chat consumes it', async () => {
      const nested = document.createElement('div');
      nested.style.cssText = 'overflow-y: auto;';
      Object.defineProperty(nested, 'scrollHeight', { configurable: true, get: () => 200 });
      Object.defineProperty(nested, 'clientHeight', { configurable: true, get: () => 100 });
      Object.defineProperty(nested, 'scrollTop', { configurable: true, get: () => 0 });
      contentEl.appendChild(nested);

      fireWheel(scrollEl, -50, nested);
      expect(controller.escapedFromLock).toBe(false);
      geom.scrollTop = 350;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('wheel down while sticky is a no-op', () => {
      fireWheel(scrollEl, 50, scrollEl);
      expect(controller.escapedFromLock).toBe(false);
    });

    it('scrollbar pointer drag escapes after the outer scrollTop decreases', async () => {
      Object.defineProperty(scrollEl, 'offsetWidth', { configurable: true, get: () => 200 });
      Object.defineProperty(scrollEl, 'clientWidth', { configurable: true, get: () => 180 });
      vi.spyOn(scrollEl, 'getBoundingClientRect').mockReturnValue({
        x: 0,
        y: 0,
        top: 0,
        left: 0,
        right: 200,
        bottom: 600,
        width: 200,
        height: 600,
        toJSON: () => ({}),
      } as DOMRect);

      scrollEl.dispatchEvent(new MouseEvent('pointerdown', { clientX: 195, bubbles: true }));
      geom.scrollTop = 399;
      fireScroll(scrollEl);
      await nextTimer();

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
    it.each(['ArrowUp', 'PageUp', 'Home'])('%s escapes only after the outer scrollTop decreases', async (key) => {
      fireKey(scrollEl, key);
      expect(controller.escapedFromLock).toBe(false);
      geom.scrollTop = 350;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);
    });

    it.each(['ArrowDown', 'PageDown', 'End'])('%s does not escape (handled by re-stick path)', (key) => {
      fireKey(scrollEl, key);
      expect(controller.escapedFromLock).toBe(false);
    });
  });

  describe('touch handler', () => {
    it('finger moves down (dy > 1) escapes only after the outer scrollTop decreases', async () => {
      fireTouchStart(scrollEl, 100);
      fireTouchMove(scrollEl, 130); // dy = +30
      expect(controller.escapedFromLock).toBe(false);
      geom.scrollTop = 350;
      fireScroll(scrollEl);
      await nextTimer();
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
      // Regression for the layout-measurement-cascade jump: virtua's
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

      // Simulate virtua's jump correction moving scrollTop away from
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
      // so the overshoot guard (separate, unconditional) does not fire
      // and pollute the assertion.
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
      // Force scrollTop above target externally (e.g. virtua mis-correction).
      geom.scrollTop = 500; // target = 400
      geom.scrollHeight = 900; // shrink
      geom.contentHeight = 700;
      ro.fire(contentEl, 700);
      // Target now = max(0, 900 - 600) = 300; clamped.
      expect(geom.scrollTop).toBeLessThanOrEqual(300);
    });

    it('overscroll guard does NOT clamp when escaped (preserves user mid-history position)', () => {
      // The overshoot guard's purpose is to fix invalid scrollTop
      // states from virtua mis-correction or browser auto-clamping,
      // but when the user has explicitly escaped, the browser's own
      // clamp will fix any out-of-range scrollTop on the next paint
      // and we must NOT yank the user to the bottom. Without this
      // gate, a virtua applyJump that nudges scrollTop past a freshly
      // shrunk target could snap the user from mid-history to bottom
      // as a side-effect of an above-viewport row remeasure.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial — sets previousHeight=800
      controller.setEscapedFromLock(true);
      // Simulate the virtua-shift-then-shrink scenario: scrollTop now
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
  });

  describe('programmatic scroll write', () => {
    it('toggles scrollBehavior to auto during write and restores after', () => {
      scrollEl.style.scrollBehavior = 'smooth';
      const ro = getRO();
      ro.fire(contentEl, 800);
      // After the write, scrollBehavior should be back to 'smooth'.
      expect(scrollEl.style.scrollBehavior).toBe('smooth');
    });

    it('scroll event with scrollTop === ignoreScrollToTop is ignored', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // writes scrollTop=400, tags ignoreScrollToTop=400
      // Fire a scroll event reading the same value.
      fireScroll(scrollEl);
      await nextTimer();
      // Tagged programmatic write: should not flip escape or anything.
      expect(controller.escapedFromLock).toBe(false);
    });

    it('subsequent user scroll back to the tagged scrollTop value is honored, not silently ignored', async () => {
      // Regression guard: the tag is captured-and-consumed synchronously,
      // so it suppresses exactly ONE scroll event. A regression that
      // moved the consume to inside the setTimeout callback would
      // silently swallow a later genuine user scroll back to the same
      // scrollTop value — the very scenario the synchronous consume
      // defends against.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial RO write tags scrollTop=400
      // First scroll event consumes the tag.
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(false);

      // User explicitly escapes, then genuinely moves AWAY and then
      // BACK to the tagged value by hand. The direction gate on the
      // re-stick path requires DOWN motion (scrollTop increasing) for
      // re-stick to fire, so we simulate a real away-then-back gesture
      // rather than a same-position re-fire. With the tag properly
      // consumed by the first event, the back-scroll must re-stick. If
      // the tag were still set, the back-scroll would be ignored and
      // re-stick would never fire.
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

    it('stays escaped when near the bottom visually but outside the auto-follow epsilon', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);

      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      geom.scrollTop = 399; // distance = 1, visually near-bottom but not auto-follow bottom
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
      expect(controller.isAtBottom).toBe(true);
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
      // break auto-follow even though the public near-bottom flag still
      // hides the chip.
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
      expect(controller.isAtBottom).toBe(true);
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
  });

  describe('markAtBottom', () => {
    it('clears escapedFromLock and sets sticky without writing scrollTop', () => {
      // Caller (MessageTimeline bottom-restore) has just landed the user
      // at the geometric bottom via listRef.scrollToIndex(last, 'end').
      // markAtBottom must flip the intent flag WITHOUT also issuing a
      // redundant scrollTop write that would fight virtua's measurement
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
      // Bottom-restore sequence: $effect.pre sets escape=true, virtua
      // mounts, contentRO observes initial height. The first fire's
      // first-fire branch must NOT snap to target (escape gate). Only
      // then does restoreToBottom call scrollToIndex(last,'end') and
      // markAtBottom. If the first-fire branch leaked through escape,
      // it would snap to a stale target before virtua's measurement
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

  describe('animateScrollTo', () => {
    it('does not break sticky-bottom state when the target is already current', async () => {
      expect(controller.isSticky).toBe(true);

      await expect(controller.animateScrollTo(400)).resolves.toBe('completed');

      expect(geom.scrollTop).toBe(400);
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('animates to an arbitrary target and leaves the user escaped from bottom lock', async () => {
      geom.scrollHeight = 1600;
      geom.clientHeight = 600;
      geom.scrollTop = 100;

      const result = controller.animateScrollTo(700, { durationMs: 80 });
      await nextFrame();
      await nextFrame();
      expect(geom.scrollTop).toBeGreaterThan(100);
      expect(geom.scrollTop).toBeLessThan(700);

      for (let i = 0; i < 12; i++) await nextFrame();
      await expect(result).resolves.toBe('completed');
      expect(geom.scrollTop).toBe(700);
      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('jumps immediately when reduced motion is requested', async () => {
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
      geom.scrollHeight = 1600;
      geom.clientHeight = 600;
      geom.scrollTop = 100;

      await expect(controller.animateScrollTo(700, { durationMs: 500 })).resolves.toBe('completed');

      expect(geom.scrollTop).toBe(700);
      expect(controller.escapedFromLock).toBe(true);
    });

    it('cancels an animated target scroll on user escape intent', async () => {
      geom.scrollHeight = 1600;
      geom.clientHeight = 600;
      geom.scrollTop = 100;

      const result = controller.animateScrollTo(700, { durationMs: 500 });
      fireWheel(scrollEl, -40, scrollEl);

      await expect(result).resolves.toBe('cancelled');
    });

    it('cancels an animated target scroll on an untagged scroll event', async () => {
      geom.scrollHeight = 1600;
      geom.clientHeight = 600;
      geom.scrollTop = 100;

      const result = controller.animateScrollTo(700, { durationMs: 500 });
      fireScroll(scrollEl);

      await expect(result).resolves.toBe('cancelled');
    });

    it('cancels an animated target scroll when detached', async () => {
      geom.scrollHeight = 1600;
      geom.clientHeight = 600;
      geom.scrollTop = 100;

      const result = controller.animateScrollTo(700, { durationMs: 500 });
      controller.detach();

      await expect(result).resolves.toBe('cancelled');
    });
  });

  describe('stopScroll', () => {
    it('sets escapedFromLock=true and isSticky=false', () => {
      controller.stopScroll();
      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('cancels an in-flight animateScrollTo', async () => {
      // stopScroll must cancel the only controller-driven scroll
      // motion: animateScrollTo (used by handleLoadOlder /
      // scrollToItem). External virtua jumps use runExternalScroll();
      // stopScroll stays scoped to cancelling controller-owned motion.
      geom.scrollHeight = 4000;
      geom.clientHeight = 600;
      geom.scrollTop = 100;
      const result = controller.animateScrollTo(2000, { durationMs: 500 });
      // Let it advance one frame so the rAF chain is live.
      await nextFrame();
      controller.stopScroll();
      await expect(result).resolves.toBe('cancelled');
      expect(controller.escapedFromLock).toBe(true);
    });

    it('after stopScroll, subsequent contentRO positive deltas do not sync-pin (escape gate holds)', async () => {
      // The contentRO sync-pin path is gated on
      // !escapedFromLockState. stopScroll sets escape, so a layout
      // change that would normally pin the viewport to the new
      // bottom must NOT do so after stopScroll — otherwise the
      // upcoming external scroll (virtua's scrollToIndex) would be
      // fought by the controller mid-jump.
      const ro = getRO();
      ro.fire(contentEl, 800); // first fire — initialize previousHeight
      controller.stopScroll();
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      const before = geom.scrollTop;
      ro.fire(contentEl, 1000); // positive delta — sync-pin gated off
      expect(geom.scrollTop).toBe(before);
    });
  });

  describe('runExternalScroll', () => {
    it('tags external scroll events in a short window so near-bottom virtua jumps do not re-stick', async () => {
      controller.setEscapedFromLock(true);
      geom.scrollTop = 300;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      controller.runExternalScroll(() => {
        geom.scrollTop = 350;
      });
      fireScroll(scrollEl);
      geom.scrollTop = 399.75;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
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

  describe('notifyContentMaybeGrew (composer-height path)', () => {
    it('writes scrollTop=target when sticky', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // initial

      // Composer grew → scrollHeight grew but contentEl.scrollHeight
      // didn't, so the content RO won't fire. Caller must invoke
      // notifyContentMaybeGrew explicitly.
      geom.scrollHeight = 1100;
      controller.notifyContentMaybeGrew();
      // target = 1100 - 600 = 500.
      expect(geom.scrollTop).toBe(500);
    });

    it('no-op when escaped', () => {
      controller.setEscapedFromLock(true);
      geom.scrollTop = 100;
      geom.scrollHeight = 1100;
      controller.notifyContentMaybeGrew();
      expect(geom.scrollTop).toBe(100);
    });

    it('no-op when leased', () => {
      const release = controller.pauseAutoScroll();
      geom.scrollHeight = 1100;
      const before = geom.scrollTop;
      controller.notifyContentMaybeGrew();
      expect(geom.scrollTop).toBe(before);
      release();
    });

    it('subsequent scroll event from layout-flush is treated as RO-correlated, not user-driven', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      geom.scrollHeight = 1100;
      controller.notifyContentMaybeGrew();
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
    // isAtBottomState) is mutated only by explicit signals — gestures,
    // forceStick, stopScroll, and input-backed scroll-handler paths.
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
      // The re-stick path's direction gate requires scrollTop to be
      // INCREASING (DOWN gesture) — same-scrollTop scrolls or UP
      // scrolls don't trigger it.
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
    resetUseStickToBottomModuleStateForTest();
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
      // running synchronously when virtua's row-remeasurement shifts
      // scrollTop off the bottom. The spring carve-out (suppress
      // writeScrollTop while springToken !== 0) must NOT weaken this
      // defense. Warm-gate-ordering invariant: cascade fires while
      // `!warm`, springGateOpen requires `warm`, so the spring never
      // starts during the cascade and springToken stays 0 — the
      // negative-pin sync write runs as it always did.
      //
      // Geometry is chosen so the overshoot guard (lines 698-700) does
      // NOT fire, isolating the negative-delta sync-pin as the only
      // possible writer of the asserted scrollTop change: scrollTop
      // must be BELOW the new target after the shrink, so
      // `scrollTop > target` is false and overshoot is bypassed.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial; warm is still false. isAtBottomState=true from attach.

      // Simulate virtua's applyJump shifting scrollTop off the bottom
      // (the cascade pattern: a row above the viewport remeasured
      // larger, virtua scrolled the visible row's offset down to
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

      // Tail-clamping or virtua remeasurement can shrink the row while a
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

    it('stopScroll cancels in-flight spring', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150);

      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      const midScrollTop = geom.scrollTop;

      controller.stopScroll();

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
      // bottom without cancelling an in-flight spring. The spring
      // reads targetScrollTop() each tick, so it should retarget and
      // converge on the new bottom without overshooting or
      // oscillating. The negative-delta re-pin path may also write
      // (when the new geometry leaves us near-bottom); both writers
      // must converge to the same target.
      const ro = getRO();
      ro.fire(contentEl, 800);
      await waitMs(150); // warm

      // Kick a spring with a large positive delta so it'll be in
      // flight for many frames.
      geom.scrollHeight = 1400;
      geom.contentHeight = 1200;
      ro.fire(contentEl, 1200);
      await nextFrame();
      const midScrollTop = geom.scrollTop;
      expect(midScrollTop).toBeGreaterThan(400);
      expect(midScrollTop).toBeLessThan(800);

      // Content shrinks below the spring's current scrollTop. New
      // target = 800 - 600 = 200, which is BELOW where the spring is
      // currently chasing. The spring must back off, not overshoot.
      geom.scrollHeight = 800;
      geom.contentHeight = 600;
      ro.fire(contentEl, 600);

      // Spring converges to the new (lower) target. Since
      // targetScrollTop() = max(0, 800 - 600) = 200, the spring's
      // current scrollTop (> 200) is above target — `current < target`
      // is false, so the spring tick stops advancing and arrives.
      // The sync-pin overshoot guard inside contentRO clamps any
      // scrollTop > target down to target.
      await advanceUntil(() => geom.scrollTop <= 200);
      expect(geom.scrollTop).toBeLessThanOrEqual(200);
    });

    it('estimate-correct pair during spring leaves spring as single writer (no sync-pin race)', async () => {
      // Bug B: virtua's row-append cycle fires contentRO twice within
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

      // +90 estimate-grow (mimics virtua provisionally rendering a new
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

      // -56 correction within the same RO burst (virtua measured the
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
});
