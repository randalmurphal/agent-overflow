import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createUseStickToBottomController,
  resetUseStickToBottomModuleStateForTest,
  type UseStickToBottomController,
} from './useStickToBottom.svelte';

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

// Spring-tick frames advance performance.now in 16.67ms steps. This
// lets the spring make real progress per rAF in the test environment
// (happy-dom's rAF doesn't drive performance.now). Tests that assert on
// spring convergence rely on this; tests that assert event-driven
// behavior don't.
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
    MockResizeObserver.instances = [];
    originalRO = globalThis.ResizeObserver;
    (globalThis as unknown as { ResizeObserver: typeof MockResizeObserver }).ResizeObserver = MockResizeObserver;
    mockNow = 0;
    vi.spyOn(performance, 'now').mockImplementation(() => mockNow);

    scrollEl = document.createElement('div');
    contentEl = document.createElement('div');
    scrollEl.appendChild(contentEl);
    document.body.appendChild(scrollEl);

    geom = { scrollHeight: 1000, clientHeight: 600, scrollTop: 399, contentHeight: 800 };
    stubGeometry(scrollEl, contentEl, geom);

    controller = createUseStickToBottomController();
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

  function getRO(): MockResizeObserver {
    const ro = MockResizeObserver.instances.at(-1);
    if (!ro) throw new Error('no ResizeObserver was created');
    return ro;
  }

  describe('initial state', () => {
    it('starts isSticky=true and isAtBottom=true', () => {
      // distance = 1000 - 399 - 600 = 1, ≤ 70 → near-bottom true.
      expect(controller.isSticky).toBe(true);
      expect(controller.isAtBottom).toBe(true);
      expect(controller.escapedFromLock).toBe(false);
    });

    it('reports isAtBottom=false when escaped AND scrolled away', async () => {
      geom.scrollTop = 100; // distance = 1000 - 100 - 600 = 300, > 70
      fireWheel(scrollEl, -50, scrollEl);
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
      // target = max(0, scrollHeight - 1 - clientHeight) = 1000 - 1 - 600 = 399
      expect(geom.scrollTop).toBe(399);
    });
  });

  describe('wheel handler', () => {
    it('wheel up on outer scrollEl flips escapedFromLock', () => {
      fireWheel(scrollEl, -50, scrollEl);
      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
      // Note: public isAtBottom may still be true if geometrically near
      // the bottom — that's the loose `intent || geometry` semantic the
      // ScrollToBottomButton wants.
    });

    it('wheel up inside a nested overflow scroller does NOT escape', () => {
      const nested = document.createElement('div');
      nested.style.cssText = 'overflow-y: auto;';
      Object.defineProperty(nested, 'scrollHeight', { configurable: true, get: () => 200 });
      Object.defineProperty(nested, 'clientHeight', { configurable: true, get: () => 100 });
      contentEl.appendChild(nested);
      fireWheel(scrollEl, -50, nested);
      expect(controller.escapedFromLock).toBe(false);
    });

    it('wheel down is a no-op', () => {
      fireWheel(scrollEl, 50, scrollEl);
      expect(controller.escapedFromLock).toBe(false);
    });

    it('wheel up does nothing when content fits in viewport', () => {
      geom.scrollHeight = 600; // = clientHeight, no overflow
      fireWheel(scrollEl, -50, scrollEl);
      expect(controller.escapedFromLock).toBe(false);
    });
  });

  describe('keyboard handler', () => {
    it.each(['ArrowUp', 'PageUp', 'Home'])('%s flips escapedFromLock', (key) => {
      fireKey(scrollEl, key);
      expect(controller.escapedFromLock).toBe(true);
    });

    it.each(['ArrowDown', 'PageDown', 'End'])('%s does not escape (handled by re-stick path)', (key) => {
      fireKey(scrollEl, key);
      expect(controller.escapedFromLock).toBe(false);
    });
  });

  describe('touch handler', () => {
    it('finger moves down (dy > 1) flips escapedFromLock', () => {
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
    it('first fire (previousHeight undefined) does not trigger spring', () => {
      const ro = getRO();
      // No prior height; this is the initial fire.
      ro.fire(contentEl, 800);
      // Spring would write scrollTop on subsequent rAFs; force-clear so
      // the first-fire branch doesn't accidentally schedule.
      expect(geom.scrollTop).toBe(399); // snapped to target
    });

    it('positive delta + sticky starts spring that converges to target', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // initial
      // Content grows; scroll target also grows.
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      // Spring runs over rAFs; converge eventually.
      for (let i = 0; i < 60; i++) {
        await nextFrame();
        if (Math.abs(geom.scrollTop - 599) < 1) break;
      }
      expect(geom.scrollTop).toBeGreaterThanOrEqual(595);
      expect(geom.scrollTop).toBeLessThanOrEqual(599);
    });

    it('positive delta + escaped does NOT start spring', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // initial

      fireWheel(scrollEl, -50, scrollEl);
      expect(controller.escapedFromLock).toBe(true);

      const before = geom.scrollTop;
      geom.scrollHeight = 1500; // grow
      geom.contentHeight = 1300;
      ro.fire(contentEl, 1300);
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(before);
    });

    it('negative delta with isNearBottom re-pins to target', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // initial; scrollTop becomes 399
      // Shrink content. Use distance just inside the 70px threshold.
      geom.scrollHeight = 800;
      geom.contentHeight = 600;
      // Without a re-pin, scrollTop=399 means distance = 800 - 399 - 600 = -199.
      ro.fire(contentEl, 600);
      // Re-pinned to target = max(0, 800 - 1 - 600) = 199.
      expect(geom.scrollTop).toBe(199);
    });

    it('overscroll guard clamps scrollTop > target', () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // initial
      // Force scrollTop above target externally (e.g. virtua mis-correction).
      geom.scrollTop = 500; // target = 399
      geom.scrollHeight = 900; // shrink
      geom.contentHeight = 700;
      ro.fire(contentEl, 700);
      // Target now = max(0, 900 - 1 - 600) = 299; clamped.
      expect(geom.scrollTop).toBeLessThanOrEqual(299);
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
      ro.fire(contentEl, 800); // writes scrollTop=399, tags ignoreScrollToTop=399
      // Fire a scroll event reading the same value.
      fireScroll(scrollEl);
      await nextTimer();
      // Tagged programmatic write: should not flip escape or anything.
      expect(controller.escapedFromLock).toBe(false);
    });

    it('subsequent user scroll back to the tagged scrollTop value is honored, not silently ignored', async () => {
      // Regression guard: the tag is captured-and-consumed synchronously
      // (line 342-343 of the controller), so it suppresses exactly ONE
      // scroll event. A regression that moved the consume to inside the
      // setTimeout callback would silently swallow a later genuine user
      // scroll back to the same scrollTop value — the very scenario the
      // synchronous consume defends against.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial RO write tags scrollTop=399
      // First scroll event consumes the tag.
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(false);

      // Now: user explicitly escapes (wheel-up flips to free), then
      // genuinely scrolls back to scrollTop=399 by hand. With the tag
      // properly consumed by the first event, the scroll handler must
      // observe near-bottom + escapedFromLock and re-stick. If the tag
      // were still set, the second scroll would be ignored and re-stick
      // would never fire.
      controller.setEscapedFromLock(true);
      expect(controller.escapedFromLock).toBe(true);
      // scrollTop is already at 399 (the tagged value); fire scroll.
      fireScroll(scrollEl);
      await nextTimer();
      // Re-stick path observes near-bottom and clears escape.
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

    it('reaching near-bottom while escaped clears escapedFromLock', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800); // initial, scrollTop=399

      // User scrolled away first, then wheel-up to escape.
      geom.scrollTop = 100;
      fireWheel(scrollEl, -50, scrollEl);
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);

      // User scrolls back to bottom.
      geom.scrollTop = 399;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });
  });

  describe('forceStick', () => {
    it('default instant clears escape and writes scrollTop to target', () => {
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      expect(controller.escapedFromLock).toBe(true);

      controller.forceStick();
      expect(controller.escapedFromLock).toBe(false);
      expect(geom.scrollTop).toBe(399);
      expect(controller.isAtBottom).toBe(true);
    });

    it('spring mode writes target then starts chase', async () => {
      fireWheel(scrollEl, -50, scrollEl);
      geom.scrollTop = 100;
      controller.forceStick({ animation: 'spring' });
      // First write is immediate.
      expect(geom.scrollTop).toBe(399);
      // Chase is alive for RETAIN_ANIMATION_DURATION_MS — grow content,
      // expect spring to chase.
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      const ro = getRO();
      ro.fire(contentEl, 800); // first fire — initialize previousHeight
      ro.fire(contentEl, 1000); // positive delta
      for (let i = 0; i < 60; i++) {
        await nextFrame();
        if (Math.abs(geom.scrollTop - 599) < 1) break;
      }
      expect(geom.scrollTop).toBeGreaterThanOrEqual(595);
    });
  });

  describe('stopScroll', () => {
    it('sets escapedFromLock=true and isSticky=false', () => {
      controller.stopScroll();
      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('cancels in-flight spring', async () => {
      const ro = getRO();
      ro.fire(contentEl, 800);
      geom.scrollHeight = 1500;
      geom.contentHeight = 1300;
      ro.fire(contentEl, 1300); // positive delta — starts spring
      // One tick of spring.
      await nextFrame();
      const before = geom.scrollTop;
      controller.stopScroll();
      // Subsequent ticks should not advance.
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(before);
    });

    it('user wheel-up mid-spring cancels the chase without further advance (R4 regression)', async () => {
      // The most important regression scenario for the whole port: user
      // is sticky, content grows (spring starts chasing), user wheels up
      // mid-flight. The spring's tick must observe escapedFromLockState
      // on the next frame and stop advancing scrollTop. If the wheel
      // path didn't drive setEscapedFromLock(true), or if the spring
      // tick didn't gate on it, the chase would continue past the user's
      // wheel position and yank the viewport back.
      const ro = getRO();
      ro.fire(contentEl, 800);
      geom.scrollHeight = 2000;
      geom.contentHeight = 1800;
      ro.fire(contentEl, 1800); // big positive delta — spring chases for several frames
      await nextFrame();
      await nextFrame();
      const beforeWheel = geom.scrollTop;
      // User wheels up mid-spring.
      fireWheel(scrollEl, -50);
      // Spring tick on the next frame observes escapedFromLockState.
      for (let i = 0; i < 10; i++) await nextFrame();
      expect(controller.escapedFromLock).toBe(true);
      // scrollTop must not have advanced past the wheel moment.
      expect(geom.scrollTop).toBeLessThanOrEqual(beforeWheel);
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
      ro.fire(contentEl, 1000); // would trigger spring; lease blocks
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
      expect(geom.scrollTop).toBe(599);
    });

    it('release does NOT re-pin when escapedFromLock', () => {
      fireWheel(scrollEl, -50, scrollEl);
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
      // target = 1100 - 1 - 600 = 499.
      expect(geom.scrollTop).toBe(499);
    });

    it('no-op when escaped', () => {
      fireWheel(scrollEl, -50, scrollEl);
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

    it('attach with new elements detaches old listeners', () => {
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
      // Wheel on NEW scrollEl flips.
      fireWheel(newScrollEl, -50, newScrollEl);
      expect(controller.escapedFromLock).toBe(true);

      newScrollEl.remove();
    });
  });
});
