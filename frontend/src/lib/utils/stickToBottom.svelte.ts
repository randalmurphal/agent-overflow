// Intent-based scroll stickiness. Stickiness is mutated only by user
// gestures or explicit forceStick() — pure content growth never sticks
// and async layout never unsticks. Replaces the geometry-derived
// userPinnedToBottom pattern, which raced async content (Shiki/KaTeX)
// landing AFTER a programmatic scroll-to-bottom and silently lost the
// pin every time.

export type StickIntent = 'stick' | 'free';

export interface StickToBottomOptions {
  /** Returns the current scroll container element, or undefined if not yet mounted. */
  getContainer: () => HTMLElement | undefined;
  /**
   * Distance (px) from geometric bottom that counts as "at bottom" — used by
   * gesture-confirmed restick and by snapshot-bottom decisions. Default 8.
   */
  threshold?: number;
  /**
   * After a content-grew rAF, schedule a one-shot re-check this many ms
   * later to absorb async layout (Shiki/KaTeX) that lands a tick or two
   * after the initial paint. Default 200ms.
   */
  settleTimeoutMs?: number;
  /**
   * When true, scrolling DOWN to within `threshold` of the geometric
   * bottom re-arms stickiness. Gated on `scrollHeight didn't grow this
   * frame` to avoid sticking when content arrived simultaneously with
   * the user's scroll. Default true.
   */
  gestureRestick?: boolean;
  /**
   * Window after a down-gesture during which the next scroll event can
   * confirm restick. Default 250ms.
   */
  gestureWindowMs?: number;
  /** Selector for click-anchor preservation targets. Defaults to common interactive elements. */
  clickAnchorTargets?: string;
  /**
   * Maximum click-anchor delta we will apply. Beyond this we skip — the
   * layout change is too large for click-anchor to be safe. Default 2000px.
   */
  clickAnchorMaxDelta?: number;
}

export interface StickToBottomController {
  /** Reactive intent state. Read in $derived / $effect / templates. */
  readonly intent: StickIntent;
  /** Reactive shorthand for `intent === 'stick'`. */
  readonly isSticky: boolean;
  /** True when the container's `scrollTop` is within `threshold` of geometric bottom. */
  isAtBottom(): boolean;
  forceStick(): void;
  notifyContentMaybeGrew(): void;
  /** Depth-counted lease that suspends auto-scroll until released. */
  pauseAutoScroll(): () => void;
  /** Idempotent and re-attachable: re-call from a `$effect` after the container is bound. */
  attach(): void;
  destroy(): void;
}

const DEFAULT_THRESHOLD = 8;
const DEFAULT_SETTLE_TIMEOUT_MS = 200;
const DEFAULT_GESTURE_WINDOW_MS = 250;
const DEFAULT_CLICK_ANCHOR_TARGETS = "button, summary, [role='button']";
const DEFAULT_CLICK_ANCHOR_MAX_DELTA = 2000;

/** Attribute that opts an element (or any descendant) out of click-anchor. */
export const SCROLL_ANCHOR_IGNORE_ATTR = 'data-scroll-anchor-ignore';
/** CSS escape used inside the closest() lookup for the opt-out attribute. */
const ANCHOR_IGNORE_SELECTOR = `[${SCROLL_ANCHOR_IGNORE_ATTR}]`;

const UP_KEYS: ReadonlySet<string> = new Set(['PageUp', 'ArrowUp', 'Home']);
const DOWN_KEYS: ReadonlySet<string> = new Set(['PageDown', 'ArrowDown', 'End']);

function nowMs(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}

export function createStickToBottomController(
  options: StickToBottomOptions,
): StickToBottomController {
  const threshold = options.threshold ?? DEFAULT_THRESHOLD;
  const settleTimeoutMs = options.settleTimeoutMs ?? DEFAULT_SETTLE_TIMEOUT_MS;
  const gestureRestick = options.gestureRestick ?? true;
  const gestureWindowMs = options.gestureWindowMs ?? DEFAULT_GESTURE_WINDOW_MS;
  const clickAnchorTargets = options.clickAnchorTargets ?? DEFAULT_CLICK_ANCHOR_TARGETS;
  const clickAnchorMaxDelta = options.clickAnchorMaxDelta ?? DEFAULT_CLICK_ANCHOR_MAX_DELTA;

  let intent = $state<StickIntent>('stick');

  // Non-reactive bookkeeping.
  // -1 sentinel means "not yet observed" — first scroll event seeds it
  // without falsely flagging "scrollHeight grew this frame".
  let lastObservedScrollHeight = -1;
  let pointerDown = false;
  let pointerDownScrollTopAtStart = -1;
  let lastDownGestureAt = 0;
  let touchStartY: number | null = null;

  let pauseDepth = 0;
  let pendingContentGrewRAF: number | null = null;
  let pendingSettleTimeout: ReturnType<typeof setTimeout> | null = null;
  let pendingClickAnchor: { element: HTMLElement; top: number } | null = null;
  let pendingClickAnchorRAF: number | null = null;

  let attachedContainer: HTMLElement | null = null;
  let detachers: Array<() => void> = [];

  // ===== Internal helpers =====

  function setIntent(next: StickIntent): void {
    if (intent !== next) intent = next;
  }

  function noteDownGesture(): void {
    lastDownGestureAt = nowMs();
  }

  function isAtGeometricBottom(container: HTMLElement): boolean {
    return container.scrollHeight - container.scrollTop - container.clientHeight <= threshold;
  }

  function isAtBottom(): boolean {
    const container = options.getContainer();
    return container ? isAtGeometricBottom(container) : true;
  }

  function clearPendingContentGrew(): void {
    if (pendingContentGrewRAF !== null) {
      cancelAnimationFrame(pendingContentGrewRAF);
      pendingContentGrewRAF = null;
    }
    if (pendingSettleTimeout !== null) {
      clearTimeout(pendingSettleTimeout);
      pendingSettleTimeout = null;
    }
  }

  function clearPendingClickAnchor(): void {
    if (pendingClickAnchorRAF !== null) {
      cancelAnimationFrame(pendingClickAnchorRAF);
      pendingClickAnchorRAF = null;
    }
    pendingClickAnchor = null;
  }

  function performScrollToBottom(container: HTMLElement): void {
    container.scrollTop = Math.max(0, container.scrollHeight - container.clientHeight);
    lastObservedScrollHeight = container.scrollHeight;
  }

  function canAutoScroll(): boolean {
    return intent === 'stick' && !pointerDown && pauseDepth === 0;
  }

  function scheduleSettleReCheck(): void {
    if (pendingSettleTimeout !== null) return;
    pendingSettleTimeout = setTimeout(() => {
      pendingSettleTimeout = null;
      if (!canAutoScroll()) return;
      const container = options.getContainer();
      if (!container) return;
      if (isAtGeometricBottom(container)) return;
      performScrollToBottom(container);
    }, settleTimeoutMs);
  }

  function notifyContentMaybeGrew(): void {
    if (!canAutoScroll()) return;
    if (pendingContentGrewRAF !== null) return;
    pendingContentGrewRAF = requestAnimationFrame(() => {
      pendingContentGrewRAF = null;
      // Re-check: state may have changed between schedule and rAF firing.
      if (!canAutoScroll()) return;
      const container = options.getContainer();
      if (!container) return;
      performScrollToBottom(container);
      scheduleSettleReCheck();
    });
  }

  function forceStick(): void {
    setIntent('stick');
    // forceStick is the explicit "user wants the bottom now" path. It
    // bypasses the pauseAutoScroll lease (the lease is for the
    // background loop only) but defers when a pointer is held: yanking
    // the user's scroll position mid-drag would erase their drag work,
    // and pointerup will resume auto-scroll naturally.
    if (pointerDown) {
      scheduleSettleReCheck();
      return;
    }
    // Cancel any in-flight rAF from a prior notifyContentMaybeGrew so
    // we don't double-scroll on the same intent.
    if (pendingContentGrewRAF !== null) {
      cancelAnimationFrame(pendingContentGrewRAF);
      pendingContentGrewRAF = null;
    }
    const container = options.getContainer();
    if (container) {
      performScrollToBottom(container);
    }
    scheduleSettleReCheck();
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

  // ===== Event handlers =====

  function handleScroll(): void {
    // Steady-state sticky: skip the scrollHeight read entirely. The
    // grewThisFrame race gate is only consulted for restick decisions
    // in the free state, so reading layout 60+ times/sec during a
    // sticky stream just to update lastObservedScrollHeight is wasted
    // work. We re-seed lastObservedScrollHeight inside the gate below.
    if (intent !== 'free' || !gestureRestick || pointerDown || lastDownGestureAt === 0) {
      return;
    }
    const container = options.getContainer();
    if (!container) return;

    const prevScrollHeight = lastObservedScrollHeight;
    const currentScrollHeight = container.scrollHeight;
    const grewThisFrame = prevScrollHeight !== -1 && currentScrollHeight !== prevScrollHeight;
    lastObservedScrollHeight = currentScrollHeight;

    if (nowMs() - lastDownGestureAt >= gestureWindowMs) return;
    if (grewThisFrame) return;
    if (!isAtGeometricBottom(container)) return;

    setIntent('stick');
    lastDownGestureAt = 0;
  }

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
    const container = options.getContainer();
    pointerDownScrollTopAtStart = container?.scrollTop ?? -1;
  }

  function handlePointerUp(_e: PointerEvent): void {
    pointerDown = false;
    const container = options.getContainer();
    const startScrollTop = pointerDownScrollTopAtStart;
    pointerDownScrollTopAtStart = -1;
    if (!container || startScrollTop < 0) return;

    const netScroll = container.scrollTop - startScrollTop;
    if (netScroll < -1) {
      // Drag scrolled UP — user clearly wants to be free.
      setIntent('free');
    } else if (netScroll > 1) {
      // Drag scrolled DOWN — record as a down-gesture and re-evaluate
      // intent against the current geometry the same way a wheel/key
      // gesture would.
      noteDownGesture();
      handleScroll();
    }
    // If we're still sticky, resume any deferred auto-scroll.
    if (canAutoScroll()) notifyContentMaybeGrew();
  }

  function handleClickCapture(e: Event): void {
    const target = e.target;
    if (!(target instanceof Element)) return;
    const container = options.getContainer();
    if (!container) return;
    const trigger = target.closest(clickAnchorTargets) as HTMLElement | null;
    if (!trigger) return;
    if (!container.contains(trigger)) return;
    if (trigger.closest(ANCHOR_IGNORE_SELECTOR)) return;

    // Snapshot BEFORE the click handler runs (we're in the capture phase).
    const top = trigger.getBoundingClientRect().top;

    clearPendingClickAnchor();
    pendingClickAnchor = { element: trigger, top };

    // Clicking an interactive control inside the timeline always means
    // "I'm interacting here, don't auto-pull me away."
    setIntent('free');

    pendingClickAnchorRAF = requestAnimationFrame(() => {
      pendingClickAnchorRAF = null;
      const anchor = pendingClickAnchor;
      pendingClickAnchor = null;
      const c = options.getContainer();
      if (!anchor || !c) return;
      if (!anchor.element.isConnected || !c.contains(anchor.element)) return;

      const nextTop = anchor.element.getBoundingClientRect().top;
      const delta = nextTop - anchor.top;
      // Skip insignificant deltas, no useless writes.
      if (Math.abs(delta) < 0.5) return;
      // Skip wholesale layout changes; click-anchor isn't safe at that scale.
      if (Math.abs(delta) > clickAnchorMaxDelta) return;

      c.scrollTop += delta;
      lastObservedScrollHeight = c.scrollHeight;
    });
  }

  // ===== Listener lifecycle =====

  function attachListeners(container: HTMLElement): void {
    container.addEventListener('scroll', handleScroll, { passive: true });
    container.addEventListener('wheel', handleWheel, { passive: true });
    container.addEventListener('keydown', handleKeydown);
    container.addEventListener('touchstart', handleTouchStart, { passive: true });
    container.addEventListener('touchmove', handleTouchMove, { passive: true });
    container.addEventListener('touchend', handleTouchEnd, { passive: true });
    container.addEventListener('touchcancel', handleTouchEnd, { passive: true });
    container.addEventListener('pointerdown', handlePointerDown, { passive: true });
    container.addEventListener('pointerup', handlePointerUp, { passive: true });
    container.addEventListener('pointercancel', handlePointerUp, { passive: true });
    container.addEventListener('click', handleClickCapture, { capture: true });

    detachers = [
      () => container.removeEventListener('scroll', handleScroll),
      () => container.removeEventListener('wheel', handleWheel),
      () => container.removeEventListener('keydown', handleKeydown),
      () => container.removeEventListener('touchstart', handleTouchStart),
      () => container.removeEventListener('touchmove', handleTouchMove),
      () => container.removeEventListener('touchend', handleTouchEnd),
      () => container.removeEventListener('touchcancel', handleTouchEnd),
      () => container.removeEventListener('pointerdown', handlePointerDown),
      () => container.removeEventListener('pointerup', handlePointerUp),
      () => container.removeEventListener('pointercancel', handlePointerUp),
      () => container.removeEventListener('click', handleClickCapture, true),
    ];
  }

  function detachListeners(): void {
    for (const d of detachers) d();
    detachers = [];
    attachedContainer = null;
  }

  function attach(): void {
    const container = options.getContainer();
    if (!container) {
      if (attachedContainer) detachListeners();
      return;
    }
    if (attachedContainer === container) return;
    if (attachedContainer) detachListeners();
    attachListeners(container);
    attachedContainer = container;
    // Seed the lastObservedScrollHeight so the first scroll event has a
    // valid baseline for the grewThisFrame race gate.
    lastObservedScrollHeight = container.scrollHeight;
  }

  function destroy(): void {
    detachListeners();
    clearPendingContentGrew();
    clearPendingClickAnchor();
    // Reset transient gesture state so a future attach() starts clean.
    pointerDown = false;
    pointerDownScrollTopAtStart = -1;
    lastDownGestureAt = 0;
    touchStartY = null;
    lastObservedScrollHeight = -1;
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
    attach,
    destroy,
  };
}
