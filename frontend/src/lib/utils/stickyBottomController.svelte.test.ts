import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { VListHandle } from 'virtua/svelte';
import { createStickyBottomController, type StickyBottomController } from './stickyBottomController.svelte';

// Mock VListHandle: tests mutate _offset / _size / _viewport on the
// returned object to simulate scroll geometry changes. scrollToIndex is
// a spy so we can assert auto-follow fired. Methods that aren't exercised
// by the controller are stubbed with no-ops so the type assignability
// holds.
type MockHandle = VListHandle & {
  _offset: number;
  _size: number;
  _viewport: number;
};

function makeHandle(initial: { offset?: number; size?: number; viewport?: number } = {}): MockHandle {
  const handle = {
    _offset: initial.offset ?? 400,
    _size: initial.size ?? 1000,
    _viewport: initial.viewport ?? 600,
    getScrollOffset: () => handle._offset,
    getScrollSize: () => handle._size,
    getViewportSize: () => handle._viewport,
    getCache: vi.fn(() => ({}) as unknown as ReturnType<VListHandle['getCache']>),
    findItemIndex: vi.fn((_offset: number) => 0),
    getItemOffset: vi.fn((_index: number) => 0),
    getItemSize: vi.fn((_index: number) => 0),
    scrollToIndex: vi.fn((idx: number, _opts?: unknown) => {
      void idx;
      handle._offset = Math.max(0, handle._size - handle._viewport);
    }),
    scrollTo: vi.fn((offset: number) => {
      handle._offset = offset;
    }),
    scrollBy: vi.fn((delta: number) => {
      handle._offset += delta;
    }),
  } as MockHandle;
  return handle;
}

function fireWheel(el: HTMLElement, deltaY: number): void {
  el.dispatchEvent(new WheelEvent('wheel', { deltaY, bubbles: true }));
}

function fireKey(el: HTMLElement, key: string): void {
  el.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true }));
}

function firePointerDown(el: HTMLElement): void {
  el.dispatchEvent(new PointerEvent('pointerdown', { pointerId: 1, bubbles: true }));
}

function firePointerUp(el: HTMLElement): void {
  el.dispatchEvent(new PointerEvent('pointerup', { pointerId: 1, bubbles: true }));
}

function fireTouchStart(el: HTMLElement, clientY: number): void {
  el.dispatchEvent(new TouchEvent('touchstart', { bubbles: true, touches: [{ clientY } as Touch] }));
}

function fireTouchMove(el: HTMLElement, clientY: number): void {
  el.dispatchEvent(new TouchEvent('touchmove', { bubbles: true, touches: [{ clientY } as Touch] }));
}

/**
 * Wait for the next animation frame. notifyContentMaybeGrew defers
 * scrollToLast to rAF (so virtua's per-row ResizeObserver has a chance
 * to update its cache before the controller reads geometry); tests that
 * assert on the scroll having happened need to await this.
 */
function nextFrame(): Promise<void> {
  return new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
}

/**
 * Hoisted to module scope so Svelte's compiler doesn't warn about
 * declaring a class inside a function (perf_avoid_nested_class).
 * The `onCreate` static slot lets the surrounding test capture each
 * instance's callback as it's constructed.
 */
class MockResizeObserver {
  static onCreate: ((cb: ResizeObserverCallback) => void) | null = null;
  callback: ResizeObserverCallback;
  constructor(cb: ResizeObserverCallback) {
    this.callback = cb;
    MockResizeObserver.onCreate?.(cb);
  }
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

describe('createStickyBottomController', () => {
  let scrollEl: HTMLDivElement;
  let handle: MockHandle;
  let lastIndex: number;
  let controller: StickyBottomController;

  beforeEach(() => {
    scrollEl = document.createElement('div');
    document.body.appendChild(scrollEl);
    handle = makeHandle();
    lastIndex = 49;
    controller = createStickyBottomController({
      getScrollEl: () => scrollEl,
      getListHandle: () => handle,
      getLastIndex: () => lastIndex,
    });
    controller.attach();
  });

  afterEach(() => {
    controller.destroy();
    scrollEl.remove();
  });

  describe('initial state', () => {
    it('starts sticky', () => {
      expect(controller.intent).toBe('stick');
      expect(controller.isSticky).toBe(true);
    });

    it('reports at-bottom from virtua geometry', () => {
      // Default: offset 400, viewport 600, size 1000 → 1000 - 400 - 600 = 0.
      expect(controller.isAtBottom()).toBe(true);
      handle._offset = 0;
      expect(controller.isAtBottom()).toBe(false);
    });
  });

  describe('gesture intent flips', () => {
    it('wheel up flips to free', () => {
      fireWheel(scrollEl, -50);
      expect(controller.intent).toBe('free');
    });

    it('wheel down marks down-gesture but does not flip from sticky', () => {
      fireWheel(scrollEl, 50);
      expect(controller.intent).toBe('stick');
    });

    it('arrow up / page up / home flip to free', () => {
      fireKey(scrollEl, 'ArrowUp');
      expect(controller.intent).toBe('free');

      controller.forceStick();
      fireKey(scrollEl, 'PageUp');
      expect(controller.intent).toBe('free');

      controller.forceStick();
      fireKey(scrollEl, 'Home');
      expect(controller.intent).toBe('free');
    });

    it('arrow down / page down / end mark down-gesture from free', () => {
      fireKey(scrollEl, 'ArrowUp');
      expect(controller.intent).toBe('free');
      // Down gesture alone does not restick — needs scroll to bottom too.
      fireKey(scrollEl, 'ArrowDown');
      expect(controller.intent).toBe('free');
    });

    it('touch finger-down flips to free; finger-up marks down-gesture', () => {
      fireTouchStart(scrollEl, 100);
      fireTouchMove(scrollEl, 120); // dy = +20 → finger moved down
      expect(controller.intent).toBe('free');

      controller.forceStick();
      fireTouchStart(scrollEl, 200);
      fireTouchMove(scrollEl, 180); // dy = -20 → finger moved up
      expect(controller.intent).toBe('stick'); // down-gesture noted, not yet restick
    });

    it('ignores tiny touch deltas (≤ 1px)', () => {
      fireTouchStart(scrollEl, 100);
      fireTouchMove(scrollEl, 100.5);
      expect(controller.intent).toBe('stick');
    });
  });

  describe('gesture-confirmed restick', () => {
    it('wheel-down + at-bottom in window flips back to stick', () => {
      // Start free.
      fireWheel(scrollEl, -50);
      expect(controller.intent).toBe('free');
      // Move handle so we're NOT at bottom yet, fire down-gesture.
      handle._offset = 200;
      fireWheel(scrollEl, 50);
      // Simulate user reaching the bottom via scroll.
      handle._offset = 400; // at bottom (1000 - 600 - 400 = 0)
      controller.onScroll(handle._offset);
      expect(controller.intent).toBe('stick');
    });

    it('wheel-down + scrollSize grew this frame does NOT restick', () => {
      // Initial scroll seeds lastObservedScrollSize.
      controller.onScroll(handle._offset);
      // Flip to free.
      fireWheel(scrollEl, -50);
      // Down gesture.
      fireWheel(scrollEl, 50);
      // Content grew (scrollSize bumped) AND we're at bottom.
      handle._size = 1100;
      handle._offset = 500; // at bottom of new size (1100 - 600 - 500 = 0)
      controller.onScroll(handle._offset);
      // grewThisFrame gate prevents restick.
      expect(controller.intent).toBe('free');
    });

    it('down-gesture without bottom-reaching does not restick', () => {
      fireWheel(scrollEl, -50);
      expect(controller.intent).toBe('free');
      fireWheel(scrollEl, 50);
      handle._offset = 200; // not at bottom
      controller.onScroll(handle._offset);
      expect(controller.intent).toBe('free');
    });

    it('does not restick after the gesture window expires', async () => {
      const c = createStickyBottomController({
        getScrollEl: () => scrollEl,
        getListHandle: () => handle,
        getLastIndex: () => lastIndex,
        gestureWindowMs: 50,
      });
      c.attach();
      try {
        fireWheel(scrollEl, -50);
        fireWheel(scrollEl, 50);
        await new Promise((r) => setTimeout(r, 80));
        handle._offset = 400;
        c.onScroll(handle._offset);
        expect(c.intent).toBe('free');
      } finally {
        c.destroy();
      }
    });
  });

  describe('forceStick', () => {
    it('flips to stick and calls scrollToIndex(last, end)', () => {
      fireWheel(scrollEl, -50);
      expect(controller.intent).toBe('free');

      controller.forceStick();
      expect(controller.intent).toBe('stick');
      expect(handle.scrollToIndex).toHaveBeenCalledWith(lastIndex, { align: 'end' });
    });

    it('defers scrollToIndex while a pointer is down', async () => {
      firePointerDown(scrollEl);
      fireWheel(scrollEl, -50);

      controller.forceStick();
      expect(controller.intent).toBe('stick');
      expect(handle.scrollToIndex).not.toHaveBeenCalled();

      // Bump size so the rAF-deferred path the pointerUp resume goes
      // through actually has growth to react to. Without this, the
      // controller short-circuits the no-op scroll (we're already at
      // the geometric bottom and nothing changed) — covered separately.
      handle._size += 100;
      firePointerUp(scrollEl);
      // pointerUp triggers notifyContentMaybeGrew on auto-scroll resume —
      // which is rAF-deferred, so flush a frame before asserting.
      await nextFrame();
      expect(handle.scrollToIndex).toHaveBeenCalledWith(lastIndex, { align: 'end' });
    });

    it('no-ops when there are no items (lastIndex < 0)', () => {
      lastIndex = -1;
      fireWheel(scrollEl, -50);
      controller.forceStick();
      expect(handle.scrollToIndex).not.toHaveBeenCalled();
      expect(controller.intent).toBe('stick');
    });
  });

  describe('notifyContentMaybeGrew', () => {
    it('scrolls to last when sticky AND content grew (after rAF)', async () => {
      // Simulate a new row landing in virtua's cache.
      handle._size += 100;
      controller.notifyContentMaybeGrew();
      // Deferred: not called synchronously.
      expect(handle.scrollToIndex).not.toHaveBeenCalled();
      await nextFrame();
      expect(handle.scrollToIndex).toHaveBeenCalledWith(lastIndex, { align: 'end' });
    });

    it('skips the scroll write when at-bottom and no growth', async () => {
      // Default mock geometry already has us at the bottom (offset 400 +
      // viewport 600 = size 1000). Nothing has grown since `attach()`
      // seeded lastObservedScrollSize, so the rAF should short-circuit
      // without touching scrollToIndex. Auto-follow that fires on every
      // streaming chunk and every status flip would otherwise issue a
      // no-op scrollToIndex per frame, which churns virtua's layout.
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(handle.scrollToIndex).not.toHaveBeenCalled();
    });

    it('still scrolls when at-bottom drifts away (e.g. row above shrank)', async () => {
      // Distance-from-bottom > threshold despite no scroll-size growth
      // (e.g. an above-viewport row collapsed). The controller should
      // re-pin to the new bottom rather than treat it as a no-op.
      handle._offset = 100;
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(handle.scrollToIndex).toHaveBeenCalledWith(lastIndex, { align: 'end' });
    });

    it('coalesces multiple calls in the same frame into one scroll', async () => {
      handle._size += 100;
      controller.notifyContentMaybeGrew();
      controller.notifyContentMaybeGrew();
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(handle.scrollToIndex).toHaveBeenCalledTimes(1);
    });

    it('does nothing when free', async () => {
      fireWheel(scrollEl, -50);
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(handle.scrollToIndex).not.toHaveBeenCalled();
    });

    it('does nothing when pause-lease is held', async () => {
      const release = controller.pauseAutoScroll();
      handle._size += 100; // pretend content grew during the lease
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(handle.scrollToIndex).not.toHaveBeenCalled();
      release();
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(handle.scrollToIndex).toHaveBeenCalled();
    });

    it('does nothing when a pointer is held', async () => {
      firePointerDown(scrollEl);
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(handle.scrollToIndex).not.toHaveBeenCalled();
      firePointerUp(scrollEl);
    });

    it('re-checks lease at the deferred rAF callback (released between schedule and fire)', async () => {
      // Schedule the deferred scroll while sticky-with-no-lease.
      controller.notifyContentMaybeGrew();
      // Acquire a lease before the rAF fires.
      const release = controller.pauseAutoScroll();
      await nextFrame();
      // Lease was held when the rAF callback ran → no scroll.
      expect(handle.scrollToIndex).not.toHaveBeenCalled();
      release();
    });
  });

  describe('pauseAutoScroll', () => {
    it('lease is depth-counted and idempotent', async () => {
      const r1 = controller.pauseAutoScroll();
      const r2 = controller.pauseAutoScroll();
      handle._size += 100; // ensure the post-release rAF has growth to react to
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(handle.scrollToIndex).not.toHaveBeenCalled();

      r1();
      r1(); // idempotent — second call is no-op
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(handle.scrollToIndex).not.toHaveBeenCalled();

      r2();
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(handle.scrollToIndex).toHaveBeenCalled();
    });

    it('release re-pins to the bottom when sticky (covers viewport-shrink-while-leased)', async () => {
      // Layout-changing surfaces (terminal drawer toggle, sidebar
      // fly-in transition, RHS resizer drag) hold a lease across the
      // transition. While the lease is held, auto-follow can't fire,
      // so a viewport that shrinks during that window leaves the user
      // off the bottom. The release path must re-pin so they land
      // back at the new last row instead of drifting away.
      const release = controller.pauseAutoScroll();
      // Notifications during the lease are no-ops.
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(handle.scrollToIndex).not.toHaveBeenCalled();

      // Simulate the chat column shrinking while the lease was held —
      // viewport drops, so the user is no longer at the geometric
      // bottom. The release path should observe the drift and re-pin.
      handle._viewport = 400;

      release();
      await nextFrame();
      // Release alone should have scheduled the re-pin — no extra
      // notifyContentMaybeGrew call was made between release and frame.
      expect(handle.scrollToIndex).toHaveBeenCalledTimes(1);
    });

    it('release does NOT re-pin when intent is free', async () => {
      // User is reading older content (intent='free'). A lease release
      // must not yank them back to the bottom — the lease's purpose was
      // to suppress auto-follow, not to opt the user back in.
      fireWheel(scrollEl, -50);
      expect(controller.intent).toBe('free');

      const release = controller.pauseAutoScroll();
      release();
      await nextFrame();
      expect(handle.scrollToIndex).not.toHaveBeenCalled();
    });
  });

  describe('viewport ResizeObserver', () => {
    // Install a controllable ResizeObserver mock so we can simulate a
    // chat-column resize and assert the controller responds. happy-dom
    // doesn't fire RO callbacks on actual element resize, so without
    // this mock the wiring would be untested at the unit level.
    let lastROCallback: ResizeObserverCallback | null = null;
    let originalRO: typeof ResizeObserver | undefined;

    beforeEach(() => {
      lastROCallback = null;
      originalRO = globalThis.ResizeObserver;
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (globalThis as any).ResizeObserver = MockResizeObserver;
      MockResizeObserver.onCreate = (cb) => {
        lastROCallback = cb;
      };

      // Re-create the controller so it picks up the mocked RO.
      controller.destroy();
      controller = createStickyBottomController({
        getScrollEl: () => scrollEl,
        getListHandle: () => handle,
        getLastIndex: () => lastIndex,
      });
      controller.attach();
    });

    afterEach(() => {
      MockResizeObserver.onCreate = null;
      if (originalRO) {
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        (globalThis as any).ResizeObserver = originalRO;
      }
    });

    it('re-pins to the bottom when the wrapper resizes while sticky', async () => {
      expect(lastROCallback).not.toBeNull();
      // Simulate the chat column shrinking (terminal drawer opens,
      // window dragged smaller, sidebar fly-in narrows the column) —
      // viewport drops, the user is no longer at the geometric bottom,
      // and the controller's no-op short-circuit should NOT fire.
      // The RO entries argument is unused by the controller — it just
      // calls notifyContentMaybeGrew unconditionally and lets the
      // controller decide whether to scroll based on intent + lease.
      handle._viewport = 400;
      lastROCallback!([], {} as ResizeObserver);
      await nextFrame();
      expect(handle.scrollToIndex).toHaveBeenCalledTimes(1);
    });

    it('does NOT re-pin on resize when intent is free', async () => {
      fireWheel(scrollEl, -50);
      expect(controller.intent).toBe('free');

      lastROCallback!([], {} as ResizeObserver);
      await nextFrame();
      expect(handle.scrollToIndex).not.toHaveBeenCalled();
    });
  });

  describe('pointer drag interpretation', () => {
    it('drag scrolling UP flips to free on pointerup', () => {
      handle._offset = 400;
      firePointerDown(scrollEl);
      handle._offset = 100; // user dragged scrollbar up
      firePointerUp(scrollEl);
      expect(controller.intent).toBe('free');
    });

    it('drag scrolling DOWN to bottom resticks via gesture restick path', () => {
      // Start free, scrolled away from bottom.
      fireWheel(scrollEl, -50);
      handle._offset = 100;
      controller.onScroll(handle._offset); // seed lastObservedScrollSize
      expect(controller.intent).toBe('free');

      firePointerDown(scrollEl);
      handle._offset = 400; // dragged scrollbar to bottom
      firePointerUp(scrollEl);
      expect(controller.intent).toBe('stick');
    });

    it('drag with no net scroll leaves intent unchanged', () => {
      firePointerDown(scrollEl);
      // No offset change.
      firePointerUp(scrollEl);
      expect(controller.intent).toBe('stick');
    });
  });

  describe('attach / destroy lifecycle', () => {
    it('attach is idempotent', () => {
      controller.attach();
      controller.attach();
      // No throws; second attach is a no-op since the wrapper element
      // is already bound.
      fireWheel(scrollEl, -50);
      expect(controller.intent).toBe('free');
    });

    it('destroy detaches all listeners', () => {
      controller.destroy();
      fireWheel(scrollEl, -50);
      // Without listeners, gesture has no effect.
      expect(controller.intent).toBe('stick');
    });

    it('attach after destroy re-binds listeners', () => {
      controller.destroy();
      controller.attach();
      fireWheel(scrollEl, -50);
      expect(controller.intent).toBe('free');
    });
  });
});
