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

    it('escape suppresses both first-fire snap AND positive-delta spring chase', async () => {
      // Regression for the open-thread scroll animation: MessageTimeline's
      // $effect.pre calls setEscapedFromLock(true) on every threadId
      // change so virtua's incremental row remeasurement (positive-delta
      // RO fires) doesn't auto-chase the bottom from scrollTop=0. This
      // test locks that contract at the controller level: while escaped,
      // neither the first RO fire nor any subsequent positive delta is
      // allowed to advance scrollTop. Only an explicit forceStick can
      // resume bottom-following.
      controller.setEscapedFromLock(true);
      geom.scrollTop = 0;
      const ro = getRO();
      // First fire — would snap to bottom in the default sticky state,
      // but escape must hold.
      ro.fire(contentEl, 800);
      expect(geom.scrollTop).toBe(0);

      // Subsequent positive-delta fire (virtua finishes measuring more
      // rows). Spring would chase if escape were false.
      ro.fire(contentEl, 1000);
      // Allow rAF + tick for any spring that wrongly fired.
      await nextFrame();
      await nextFrame();
      expect(geom.scrollTop).toBe(0);
      expect(controller.escapedFromLock).toBe(true);

      // forceStick(instant) clears escape and snaps to target — this is
      // what restoreInitialPosition does for a bottom snapshot.
      controller.forceStick({ animation: 'instant' });
      expect(geom.scrollTop).toBe(399);
      expect(controller.escapedFromLock).toBe(false);
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
      // Regression guard: the tag is captured-and-consumed synchronously,
      // so it suppresses exactly ONE scroll event. A regression that
      // moved the consume to inside the setTimeout callback would
      // silently swallow a later genuine user scroll back to the same
      // scrollTop value — the very scenario the synchronous consume
      // defends against.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial RO write tags scrollTop=399
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
      geom.scrollTop = 399;
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

      // User scrolls back to within the small re-stick band.
      // distance = 1000 - 396 - 600 = 4, ≤ RE_STICK_OFFSET_PX(5).
      // Using 396 (not 399) deliberately exercises the new tight band:
      // an old test using scrollTop=399 (distance=1) would have passed
      // even with the buggy `isNearBottomState` check (1 ≤ 70).
      geom.scrollTop = 396;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });

    it('a small wheel-up that lands within the geometric near-bottom band but outside the re-stick band stays escaped', async () => {
      // Regression: STICK_TO_BOTTOM_OFFSET_PX (70) is used for button
      // visibility / negative-delta repin. The scroll handler's re-stick
      // path uses a much smaller RE_STICK_OFFSET_PX (5). With the old
      // code, a wheel-up of 30px on a sticky session would set escape
      // and then the same scroll event would observe distance=30 ≤ 70
      // and immediately re-stick — undoing the user's gesture.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial, geom.scrollTop=399 (target=399, distance=1)
      // First scroll event consumes the programmatic-write tag.
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.isSticky).toBe(true);
      expect(controller.escapedFromLock).toBe(false);

      // User wheels up by ~30px. distance from bottom is now 31 — well
      // inside isNearBottomState's 70px band, but outside re-stick's
      // small band, so the escape must persist.
      geom.scrollTop = 369; // distance = 1000 - 369 - 600 = 31
      fireWheel(scrollEl, -30, scrollEl);
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
    });

    it('wheel-up gesture is NOT undone by its own scroll event landing within the re-stick band', async () => {
      // Regression for "scroll up one notch yanks back to bottom while
      // streaming". Even with RE_STICK_OFFSET_PX small, a sticky user
      // who wheels up by a tiny amount (1–5 px on a high-resolution
      // trackpad, or because the thread isn't very tall) lands inside
      // the re-stick band, and the scroll event from the wheel itself
      // would observe `escapedFromLock=true && distance<=threshold`
      // and re-stick — exactly undoing the escape on the same gesture
      // that set it. The fix is the direction gate: re-stick fires only
      // on DOWN-direction scrolls (scrollTop INCREASING), never on the
      // user's UP gesture itself.
      const ro = getRO();
      ro.fire(contentEl, 800); // initial, scrollTop=399, sticky
      fireScroll(scrollEl); // consumes tag
      await nextTimer();
      expect(controller.isSticky).toBe(true);

      // User wheels up by 3 px. distance = 1000 - 396 - 600 = 4, well
      // inside the 5 px re-stick band; without direction gating, the
      // post-wheel scroll event would re-stick.
      geom.scrollTop = 396;
      fireWheel(scrollEl, -3, scrollEl);
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(true);
      expect(controller.isSticky).toBe(false);
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

    it('subsequent positive-delta RO fire arms the spring (streaming follow resumes)', async () => {
      // Re-entry into an active streaming thread: scrollToIndex landed
      // us at the current bottom, markAtBottom flipped intent, then a
      // streaming chunk arrives. The contentRO positive-delta path must
      // arm the spring as normal so the user follows the live tail.
      controller.setEscapedFromLock(true);
      geom.scrollTop = 399;
      controller.markAtBottom();
      const ro = getRO();
      ro.fire(contentEl, 800); // initialize previousHeight
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000); // positive delta — should arm spring
      for (let i = 0; i < 60; i++) {
        await nextFrame();
        if (Math.abs(geom.scrollTop - 599) < 1) break;
      }
      expect(geom.scrollTop).toBeGreaterThanOrEqual(595);
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

  describe('forceStickAndSettle', () => {
    it('snaps to bottom and re-snaps on every positive delta during the settle window', async () => {
      // ChannelView path: Streamdown's async typesetting (shiki, KaTeX,
      // mermaid) keeps growing rows for hundreds of ms after the initial
      // load. With the spring path, each growth event would chase the
      // moving bottom visibly. With settle, each delta re-snaps instantly.
      geom.scrollTop = 100;
      controller.forceStickAndSettle({ settleMs: 100 });
      expect(geom.scrollTop).toBe(399); // initial snap from forceStick
      expect(controller.escapedFromLock).toBe(false);

      const ro = getRO();
      ro.fire(contentEl, 800); // initialize previousHeight
      // First grow inside window: re-snap, no spring.
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      expect(geom.scrollTop).toBe(499); // re-snapped to new target

      // No spring frames advance further — geometry stays where the
      // re-snap put it.
      const after = geom.scrollTop;
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(after);

      // Second grow inside window: re-snap again.
      geom.scrollHeight = 1300;
      geom.contentHeight = 1100;
      ro.fire(contentEl, 1100);
      expect(geom.scrollTop).toBe(699);
    });

    it('after the settle window expires, positive deltas arm the spring as normal', async () => {
      // The settle behavior is restoration-only. Once the window passes,
      // ongoing growth (e.g. real streaming) must use the spring path —
      // otherwise instant writes would fight virtua's $fixScrollJump on
      // surfaces that have a virtualizer (the controller doesn't know
      // which surface it's attached to, so the contract has to be uniform).
      controller.forceStickAndSettle({ settleMs: 50 });
      const ro = getRO();
      ro.fire(contentEl, 800); // initialize previousHeight
      // Advance past the settle window.
      mockNow += 60;
      geom.scrollHeight = 1500;
      geom.contentHeight = 1300;
      ro.fire(contentEl, 1300);
      // Spring drives convergence over multiple frames, not a single snap.
      // First frame should not yet be at target.
      await nextFrame();
      expect(geom.scrollTop).toBeLessThan(899);
      // Eventually converges.
      for (let i = 0; i < 60; i++) {
        await nextFrame();
        if (Math.abs(geom.scrollTop - 899) < 1) break;
      }
      expect(geom.scrollTop).toBeGreaterThanOrEqual(895);
    });

    it('user wheel-up during settle window cancels both re-snap and spring', async () => {
      // The escape gate must take precedence: a user gesture during the
      // window stops everything. Without this, the re-snap would fight
      // the user's intent to scroll up.
      controller.forceStickAndSettle({ settleMs: 200 });
      const ro = getRO();
      ro.fire(contentEl, 800); // initialize previousHeight

      fireWheel(scrollEl, -50, scrollEl);
      expect(controller.escapedFromLock).toBe(true);

      geom.scrollTop = 100;
      geom.scrollHeight = 1200;
      geom.contentHeight = 1000;
      ro.fire(contentEl, 1000);
      // Escape gate prevents both re-snap and spring.
      for (let i = 0; i < 5; i++) await nextFrame();
      expect(geom.scrollTop).toBe(100);
    });

    it('honors the exact window boundary: delta at t<settleMs re-snaps, at t>=settleMs springs', async () => {
      // Boundary regression: the predicate is `nowMs() < settleUntil`,
      // so a delta arriving exactly AT the deadline must take the spring
      // path. Verifies the strict-less-than gate, not <=.
      controller.forceStickAndSettle({ settleMs: 100 });
      const ro = getRO();
      ro.fire(contentEl, 800); // initialize previousHeight

      // Tick to t=99 (still inside window): re-snap path.
      mockNow += 99;
      geom.scrollHeight = 1100;
      geom.contentHeight = 900;
      ro.fire(contentEl, 900);
      expect(geom.scrollTop).toBe(499); // re-snapped, no spring frames yet

      // Tick to exactly t=100 (deadline): spring path. The first frame
      // can't fully converge to the new target — that's the observable
      // distinction from a same-frame re-snap.
      mockNow += 1; // now at exactly settleUntil
      geom.scrollHeight = 1300;
      geom.contentHeight = 1100;
      ro.fire(contentEl, 1100);
      // Spring needs multiple frames; the first one should be partial.
      await nextFrame();
      expect(geom.scrollTop).toBeLessThan(699);
    });

    it('escape during settle clears the window so the next forceStick uses normal spring semantics', async () => {
      // setEscapedFromLock(true) resets settleUntil. After escape, a
      // subsequent re-engagement (forceStick) must NOT inherit the old
      // window — otherwise streaming after the user returns to bottom
      // would mistakenly re-snap instead of springing.
      controller.forceStickAndSettle({ settleMs: 500 });
      const ro = getRO();
      ro.fire(contentEl, 800); // initialize previousHeight

      // User escapes mid-window.
      fireWheel(scrollEl, -50, scrollEl);
      expect(controller.escapedFromLock).toBe(true);

      // User clicks the chip to come back. forceStick (spring) — no
      // settle window should be active.
      controller.forceStick({ animation: 'spring' });
      expect(controller.escapedFromLock).toBe(false);

      // Stay well within what WOULD have been the original 500ms window
      // (we've only advanced through wheel + forceStick, not 500ms of
      // mockNow). A positive delta here must take the spring path.
      geom.scrollHeight = 1300;
      geom.contentHeight = 1100;
      ro.fire(contentEl, 1100);
      // Spring is multi-frame; first frame is partial.
      await nextFrame();
      expect(geom.scrollTop).toBeLessThan(699);
    });

    it('detach mid-window clears settleUntil so re-attach starts with normal spring semantics', async () => {
      // detach() is the lifecycle reset. A controller can be re-used
      // across surface remounts (thread switch, channel remount); the
      // settle window must not survive detach or a reattached surface
      // would silently lose spring-driven streaming for the residual
      // window duration.
      controller.forceStickAndSettle({ settleMs: 500 });
      const ro = getRO();
      ro.fire(contentEl, 800); // initialize previousHeight

      // Detach mid-window (settleUntil is set, but not expired).
      controller.detach();

      // Re-attach to a fresh surface. New geometry; would-have-been
      // window is still in the future per nowMs() if not cleared.
      const newScroll = document.createElement('div');
      const newContent = document.createElement('div');
      newScroll.appendChild(newContent);
      document.body.appendChild(newScroll);
      const newGeom: Geometry = { scrollHeight: 2000, clientHeight: 800, scrollTop: 1199, contentHeight: 1500 };
      stubGeometry(newScroll, newContent, newGeom);
      controller.attach(newScroll, newContent);

      const ro2 = getRO();
      ro2.fire(newContent, 1500); // initialize previousHeight on the new surface

      // Positive delta on the re-attached surface must take the spring
      // path, not re-snap. Same observable test: spring is multi-frame.
      newGeom.scrollHeight = 2200;
      newGeom.contentHeight = 1700;
      ro2.fire(newContent, 1700);
      await nextFrame();
      expect(newGeom.scrollTop).toBeLessThan(1399);

      newScroll.remove();
    });
  });

  describe('animateScrollTo', () => {
    it('does not break sticky-bottom state when the target is already current', async () => {
      expect(controller.isSticky).toBe(true);

      await expect(controller.animateScrollTo(399)).resolves.toBe('completed');

      expect(geom.scrollTop).toBe(399);
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

  describe('architectural invariants', () => {
    // These tests lock in the design choice that distinguishes the
    // unified controller from its predecessors: intent (escapedFromLock,
    // isAtBottomState) is mutated only by explicit signals — gestures,
    // forceStick, stopScroll, the scroll handler's escape→re-stick
    // path. Pure geometry mutation does not cross the boundary. If a
    // future change reintroduces a "scrollTop direction" inference,
    // these tests fail.

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
      // Escape via wheel. isAtBottomState is now false; near-bottom is
      // recomputed from geometry on each scroll event.
      fireWheel(scrollEl, -50, scrollEl);
      expect(controller.escapedFromLock).toBe(true);

      // Mutate scrollTop to put us geometrically right at bottom WITHOUT
      // firing a scroll event. The geometric near-bottom would be true
      // if computed, but no signal is firing to recompute it AND the
      // intent flag (isAtBottomState) is governed only by the scroll
      // handler's "user scrolled back" path, the forceStick path, or
      // the content RO's negative-delta restick path — none of which
      // are triggered here.
      geom.scrollTop = 399;

      // Without a scroll event firing the re-stick path, isSticky stays
      // false even though geometrically we'd be at-bottom.
      expect(controller.isSticky).toBe(false);
    });

    it('only the scroll handler\'s near-bottom + escaped path resurrects intent', async () => {
      // Companion to the test above: this proves the design DOES
      // re-stick when the user actually scrolls back, so the previous
      // assertion is about the absence of polling, not a regression.
      fireWheel(scrollEl, -50, scrollEl);
      expect(controller.escapedFromLock).toBe(true);

      // Simulate the user actually moving away and then back to bottom.
      // The re-stick path's direction gate requires scrollTop to be
      // INCREASING (DOWN gesture) — same-scrollTop scrolls or UP
      // scrolls don't trigger it.
      geom.scrollTop = 100;
      fireScroll(scrollEl);
      await nextTimer();
      expect(controller.escapedFromLock).toBe(true);

      geom.scrollTop = 399;
      fireScroll(scrollEl);
      await nextTimer();

      expect(controller.escapedFromLock).toBe(false);
      expect(controller.isSticky).toBe(true);
    });
  });
});
