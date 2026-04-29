// Sticky-bottom controller for the chat timeline.
//
// Owns one piece of state — the user's intent to follow the bottom or
// not — and converts that intent into either programmatic scrolls (via
// virtua's VListHandle.scrollToIndex) or no-ops while the user is reading
// older content.
//
// Stickiness is mutated only by user gestures or explicit forceStick().
// Pure content growth never re-sticks (no "user scrolled up but the next
// stream chunk pulls them back down" surprise) and async content arriving
// after a programmatic scroll never unsticks (virtua's per-row
// ResizeObserver + jump algorithm preserves visible content silently —
// our controller doesn't need to retry or settle).
//
// Anchor preservation across height changes (Mermaid mounting, Shiki
// swap, KaTeX render, image attachment load, expand toggles, sidebar
// resize) is virtua's job. This controller is the smallest layer above
// it that decides "follow the bottom" vs "leave them alone".
//
// Replaces stickToBottom.svelte.ts. Drops the rAF coalescing, the 200ms
// settle re-check, and the click-anchor compensation pass — none are
// needed once virtua owns scroll geometry.

import type { VListHandle } from 'virtua/svelte';

export type StickIntent = 'stick' | 'free';

export interface StickyBottomOptions {
  /** Returns the wrapper around <VList> so we can attach gesture listeners. */
  getScrollEl: () => HTMLElement | undefined;
  /** Returns the VList handle so we can read geometry and scrollToIndex. */
  getListHandle: () => VListHandle | undefined;
  /** Returns the index of the last item; used by forceStick / auto-follow. */
  getLastIndex: () => number;
  /**
   * Distance (px) from geometric bottom that counts as "at bottom" — used by
   * gesture-confirmed restick and by snapshot-bottom decisions. Default 8.
   */
  threshold?: number;
  /**
   * Window after a down-gesture during which the next scroll event can
   * confirm restick. Default 250ms.
   */
  gestureWindowMs?: number;
}

export interface StickyBottomController {
  /** Reactive intent state. Read in $derived / $effect / templates. */
  readonly intent: StickIntent;
  /** Reactive shorthand for `intent === 'stick'`. */
  readonly isSticky: boolean;
  /** True when virtua reports we're within `threshold` of the geometric bottom. */
  isAtBottom(): boolean;
  /**
   * "User wants the bottom now" — used on send, scroll-to-bottom button.
   * Bypasses the pause-lease (the lease is for the background follow loop)
   * but defers if a pointer is currently held to avoid yanking a drag.
   */
  forceStick(): void;
  /**
   * Called when items might have grown (length tick, revision tick, etc).
   * If sticky, programmatically scroll to the last index. virtua absorbs
   * any height-delta race; no rAF, no settle re-check.
   */
  notifyContentMaybeGrew(): void;
  /** Depth-counted lease that suspends auto-scroll until released. */
  pauseAutoScroll(): () => void;
  /** Wired to VList.onscroll — drives gesture-confirmed restick math. */
  onScroll(offset: number): void;
  /** Wired to VList.onscrollend — currently a no-op hook for future use. */
  onScrollEnd(): void;
  /** Idempotent and re-attachable: re-call from a $effect after the wrapper is bound. */
  attach(): void;
  destroy(): void;
}

const DEFAULT_THRESHOLD = 8;
const DEFAULT_GESTURE_WINDOW_MS = 250;

const UP_KEYS: ReadonlySet<string> = new Set(['PageUp', 'ArrowUp', 'Home']);
const DOWN_KEYS: ReadonlySet<string> = new Set(['PageDown', 'ArrowDown', 'End']);

function nowMs(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}

export function createStickyBottomController(
  options: StickyBottomOptions,
): StickyBottomController {
  const threshold = options.threshold ?? DEFAULT_THRESHOLD;
  const gestureWindowMs = options.gestureWindowMs ?? DEFAULT_GESTURE_WINDOW_MS;

  let intent = $state<StickIntent>('stick');

  // Non-reactive bookkeeping.
  let pointerDown = false;
  let pointerDownOffsetAtStart = -1;
  let lastDownGestureAt = 0;
  let touchStartY: number | null = null;
  // Last scroll size observed at a scroll event. Used to gate gesture
  // restick: a scroll event that coincided with content growth shouldn't
  // be interpreted as the user scrolling to the bottom — the bottom came
  // up to meet them.
  let lastObservedScrollSize = -1;

  let pauseDepth = 0;

  let attachedEl: HTMLElement | null = null;
  let detachers: Array<() => void> = [];

  // ===== Internal helpers =====

  function setIntent(next: StickIntent): void {
    if (intent !== next) intent = next;
  }

  function noteDownGesture(): void {
    lastDownGestureAt = nowMs();
  }

  function isAtGeometricBottom(handle: VListHandle): boolean {
    const offset = handle.getScrollOffset();
    const viewport = handle.getViewportSize();
    const size = handle.getScrollSize();
    return size - offset - viewport <= threshold;
  }

  function isAtBottom(): boolean {
    const handle = options.getListHandle();
    return handle ? isAtGeometricBottom(handle) : true;
  }

  function canAutoScroll(): boolean {
    return intent === 'stick' && !pointerDown && pauseDepth === 0;
  }

  function scrollToLast(handle: VListHandle): void {
    const last = options.getLastIndex();
    if (last < 0) return;
    handle.scrollToIndex(last, { align: 'end' });
    lastObservedScrollSize = handle.getScrollSize();
  }

  function notifyContentMaybeGrew(): void {
    if (!canAutoScroll()) return;
    const handle = options.getListHandle();
    if (!handle) return;
    scrollToLast(handle);
  }

  function forceStick(): void {
    setIntent('stick');
    // Defer when a pointer is held: yanking the user's scroll position
    // mid-drag would erase their drag work, and pointerup will resume
    // auto-scroll naturally.
    if (pointerDown) return;
    const handle = options.getListHandle();
    if (handle) scrollToLast(handle);
  }

  function pauseAutoScroll(): () => void {
    pauseDepth += 1;
    let released = false;
    return () => {
      if (released) return;
      released = true;
      pauseDepth = Math.max(0, pauseDepth - 1);
    };
  }

  // ===== VList event hooks =====

  function onScroll(_offset: number): void {
    // Steady-state sticky: skip the geometry read. The grewThisFrame race
    // gate is only consulted for restick decisions in the free state.
    if (intent !== 'free' || pointerDown || lastDownGestureAt === 0) {
      // Still seed lastObservedScrollSize so the first relevant scroll
      // event has a valid baseline for the grewThisFrame gate below.
      const handle = options.getListHandle();
      if (handle) lastObservedScrollSize = handle.getScrollSize();
      return;
    }
    const handle = options.getListHandle();
    if (!handle) return;

    const prevSize = lastObservedScrollSize;
    const currentSize = handle.getScrollSize();
    const grewThisFrame = prevSize !== -1 && currentSize !== prevSize;
    lastObservedScrollSize = currentSize;

    if (nowMs() - lastDownGestureAt >= gestureWindowMs) return;
    if (grewThisFrame) return;
    if (!isAtGeometricBottom(handle)) return;

    setIntent('stick');
    lastDownGestureAt = 0;
  }

  function onScrollEnd(): void {
    // Reserved for future use. virtua dispatches this after a scroll
    // settles; we don't currently need to react. Keeping the hook so
    // callers don't have to wire it conditionally.
  }

  // ===== Gesture handlers (DOM listeners on the wrapper) =====

  function handleWheel(e: WheelEvent): void {
    if (e.deltaY < 0) {
      setIntent('free');
    } else if (e.deltaY > 0) {
      noteDownGesture();
    }
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (UP_KEYS.has(e.key)) {
      setIntent('free');
    } else if (DOWN_KEYS.has(e.key)) {
      noteDownGesture();
    }
  }

  function handleTouchStart(e: TouchEvent): void {
    touchStartY = e.touches[0]?.clientY ?? null;
  }

  function handleTouchMove(e: TouchEvent): void {
    if (touchStartY === null) return;
    const y = e.touches[0]?.clientY ?? touchStartY;
    const dy = y - touchStartY;
    touchStartY = y;
    // Finger moves DOWN (dy > 0) → content moves DOWN visually → user
    // wants to see content ABOVE → flip free. Finger moves UP → user
    // is scrolling content up → mark down-gesture.
    if (dy > 1) {
      setIntent('free');
    } else if (dy < -1) {
      noteDownGesture();
    }
  }

  function handleTouchEnd(): void {
    touchStartY = null;
  }

  function handlePointerDown(_e: PointerEvent): void {
    pointerDown = true;
    const handle = options.getListHandle();
    pointerDownOffsetAtStart = handle?.getScrollOffset() ?? -1;
  }

  function handlePointerUp(_e: PointerEvent): void {
    pointerDown = false;
    const handle = options.getListHandle();
    const startOffset = pointerDownOffsetAtStart;
    pointerDownOffsetAtStart = -1;
    if (!handle || startOffset < 0) return;

    const netScroll = handle.getScrollOffset() - startOffset;
    if (netScroll < -1) {
      // Drag scrolled UP — user clearly wants to be free.
      setIntent('free');
    } else if (netScroll > 1) {
      // Drag scrolled DOWN — record as a down-gesture and re-evaluate
      // intent against the current geometry the same way a wheel/key
      // gesture would.
      noteDownGesture();
      onScroll(handle.getScrollOffset());
    }
    // If still sticky after the drag, resume any deferred auto-scroll.
    if (canAutoScroll()) notifyContentMaybeGrew();
  }

  // ===== Listener lifecycle =====

  function attachListeners(el: HTMLElement): void {
    el.addEventListener('wheel', handleWheel, { passive: true });
    el.addEventListener('keydown', handleKeydown);
    el.addEventListener('touchstart', handleTouchStart, { passive: true });
    el.addEventListener('touchmove', handleTouchMove, { passive: true });
    el.addEventListener('touchend', handleTouchEnd, { passive: true });
    el.addEventListener('touchcancel', handleTouchEnd, { passive: true });
    el.addEventListener('pointerdown', handlePointerDown, { passive: true });
    el.addEventListener('pointerup', handlePointerUp, { passive: true });
    el.addEventListener('pointercancel', handlePointerUp, { passive: true });

    detachers = [
      () => el.removeEventListener('wheel', handleWheel),
      () => el.removeEventListener('keydown', handleKeydown),
      () => el.removeEventListener('touchstart', handleTouchStart),
      () => el.removeEventListener('touchmove', handleTouchMove),
      () => el.removeEventListener('touchend', handleTouchEnd),
      () => el.removeEventListener('touchcancel', handleTouchEnd),
      () => el.removeEventListener('pointerdown', handlePointerDown),
      () => el.removeEventListener('pointerup', handlePointerUp),
      () => el.removeEventListener('pointercancel', handlePointerUp),
    ];
  }

  function detachListeners(): void {
    for (const d of detachers) d();
    detachers = [];
    attachedEl = null;
  }

  function attach(): void {
    const el = options.getScrollEl();
    if (!el) {
      if (attachedEl) detachListeners();
      return;
    }
    if (attachedEl === el) return;
    if (attachedEl) detachListeners();
    attachListeners(el);
    attachedEl = el;
    // Seed lastObservedScrollSize so the first onScroll event has a valid
    // baseline for the grewThisFrame race gate.
    const handle = options.getListHandle();
    if (handle) lastObservedScrollSize = handle.getScrollSize();
  }

  function destroy(): void {
    detachListeners();
    pointerDown = false;
    pointerDownOffsetAtStart = -1;
    lastDownGestureAt = 0;
    touchStartY = null;
    lastObservedScrollSize = -1;
  }

  return {
    get intent() {
      return intent;
    },
    get isSticky() {
      return intent === 'stick';
    },
    isAtBottom,
    forceStick,
    notifyContentMaybeGrew,
    pauseAutoScroll,
    onScroll,
    onScrollEnd,
    attach,
    destroy,
  };
}
