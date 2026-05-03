// Intent-based scroll stickiness for plain DOM scroll containers.
// Used by Discussion-mode ChannelView which scrolls a regular div.
// (The chat timeline uses useStickToBottom.svelte.ts — a spring-driven
// controller with a content-element ResizeObserver for the virtua
// `<Virtualizer>` integration. Distinct surfaces, distinct algorithms.)
//
// Stickiness is mutated only by user gestures or explicit forceStick() —
// pure content growth never sticks and async layout never unsticks.
// Replaces the geometry-derived userPinnedToBottom pattern, which raced
// async content (Shiki/KaTeX) landing AFTER a programmatic scroll-to-
// bottom and silently lost the pin every time.
//
// Intent state, gesture interpretation (wheel/key/touch), and the
// pause-lease live in scrollIntentCore.svelte.ts. This file is the
// DOM-container glue: scrollHeight/scrollTop/clientHeight reads, the
// rAF + settle re-check for async layout, the pointer handler that
// interprets net-scroll, and the click-anchor compensation pass that
// keeps a clicked summary/button pinned while expand/collapse changes
// layout.

import {
  createScrollIntentCore,
  type ScrollIntentCore,
} from './scrollIntentCore.svelte';

export type { StickIntent } from './scrollIntentCore.svelte';

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
  readonly intent: 'stick' | 'free';
  readonly isSticky: boolean;
  isAtBottom(): boolean;
  forceStick(): void;
  notifyContentMaybeGrew(): void;
  pauseAutoScroll(): () => void;
  attach(): void;
  destroy(): void;
}

const DEFAULT_THRESHOLD = 8;
const DEFAULT_SETTLE_TIMEOUT_MS = 200;
const DEFAULT_CLICK_ANCHOR_TARGETS = "button, summary, [role='button']";
const DEFAULT_CLICK_ANCHOR_MAX_DELTA = 2000;

/** Attribute that opts an element (or any descendant) out of click-anchor. */
export const SCROLL_ANCHOR_IGNORE_ATTR = 'data-scroll-anchor-ignore';
/** CSS escape used inside the closest() lookup for the opt-out attribute. */
const ANCHOR_IGNORE_SELECTOR = `[${SCROLL_ANCHOR_IGNORE_ATTR}]`;

export function createStickToBottomController(
  options: StickToBottomOptions,
): StickToBottomController {
  const threshold = options.threshold ?? DEFAULT_THRESHOLD;
  const settleTimeoutMs = options.settleTimeoutMs ?? DEFAULT_SETTLE_TIMEOUT_MS;
  const gestureRestick = options.gestureRestick ?? true;
  const clickAnchorTargets = options.clickAnchorTargets ?? DEFAULT_CLICK_ANCHOR_TARGETS;
  const clickAnchorMaxDelta = options.clickAnchorMaxDelta ?? DEFAULT_CLICK_ANCHOR_MAX_DELTA;

  const core: ScrollIntentCore = createScrollIntentCore({
    gestureWindowMs: options.gestureWindowMs,
  });

  // Non-reactive DOM-side bookkeeping.
  // -1 sentinel means "not yet observed" — first scroll event seeds it
  // without falsely flagging "scrollHeight grew this frame".
  let lastObservedScrollHeight = -1;
  let pointerDownScrollTopAtStart = -1;

  let pendingContentGrewRAF: number | null = null;
  let pendingSettleTimeout: ReturnType<typeof setTimeout> | null = null;
  let pendingClickAnchor: { element: HTMLElement; top: number } | null = null;
  let pendingClickAnchorRAF: number | null = null;

  let attachedContainer: HTMLElement | null = null;
  let detachGestureListeners: (() => void) | null = null;
  let detachContainerListeners: (() => void) | null = null;

  // ===== Geometry =====

  function isAtGeometricBottom(container: HTMLElement): boolean {
    return container.scrollHeight - container.scrollTop - container.clientHeight <= threshold;
  }

  function isAtBottom(): boolean {
    const container = options.getContainer();
    return container ? isAtGeometricBottom(container) : true;
  }

  function performScrollToBottom(container: HTMLElement): void {
    container.scrollTop = Math.max(0, container.scrollHeight - container.clientHeight);
    lastObservedScrollHeight = container.scrollHeight;
  }

  // ===== Auto-follow / forceStick =====

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

  function scheduleSettleReCheck(): void {
    if (pendingSettleTimeout !== null) return;
    pendingSettleTimeout = setTimeout(() => {
      pendingSettleTimeout = null;
      if (!core.canAutoScroll()) return;
      const container = options.getContainer();
      if (!container) return;
      if (isAtGeometricBottom(container)) return;
      performScrollToBottom(container);
    }, settleTimeoutMs);
  }

  function notifyContentMaybeGrew(): void {
    if (!core.canAutoScroll()) return;
    if (pendingContentGrewRAF !== null) return;
    pendingContentGrewRAF = requestAnimationFrame(() => {
      pendingContentGrewRAF = null;
      // Re-check: state may have changed between schedule and rAF firing.
      if (!core.canAutoScroll()) return;
      const container = options.getContainer();
      if (!container) return;
      performScrollToBottom(container);
      scheduleSettleReCheck();
    });
  }

  function forceStick(): void {
    core.setIntent('stick');
    // forceStick is the explicit "user wants the bottom now" path. It
    // bypasses the pauseAutoScroll lease (the lease is for the
    // background loop only) but defers when a pointer is held: yanking
    // the user's scroll position mid-drag would erase their drag work,
    // and pointerup will resume auto-scroll naturally.
    if (core.isPointerDown()) {
      scheduleSettleReCheck();
      return;
    }
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

  // ===== Container event handlers =====

  function handleScroll(): void {
    // Steady-state sticky: skip the scrollHeight read entirely. The
    // grewThisFrame race gate is only consulted for restick decisions in
    // the free state; reading layout 60+ times/sec during a sticky
    // stream just to update lastObservedScrollHeight is wasted work. We
    // re-seed lastObservedScrollHeight inside the gate below.
    if (
      core.intent !== 'free'
      || !gestureRestick
      || core.isPointerDown()
      || !core.inDownGestureWindow()
    ) {
      return;
    }
    const container = options.getContainer();
    if (!container) return;

    const prevScrollHeight = lastObservedScrollHeight;
    const currentScrollHeight = container.scrollHeight;
    const grewThisFrame = prevScrollHeight !== -1 && currentScrollHeight !== prevScrollHeight;
    lastObservedScrollHeight = currentScrollHeight;

    if (grewThisFrame) return;
    if (!isAtGeometricBottom(container)) return;

    core.setIntent('stick');
    core.clearDownGesture();
  }

  function handlePointerDown(_e: PointerEvent): void {
    core.setPointerDown(true);
    const container = options.getContainer();
    pointerDownScrollTopAtStart = container?.scrollTop ?? -1;
  }

  function handlePointerUp(_e: PointerEvent): void {
    core.setPointerDown(false);
    const container = options.getContainer();
    const startScrollTop = pointerDownScrollTopAtStart;
    pointerDownScrollTopAtStart = -1;
    if (!container || startScrollTop < 0) return;

    const netScroll = container.scrollTop - startScrollTop;
    if (netScroll < -1) {
      // Drag scrolled UP — user clearly wants to be free.
      core.setIntent('free');
    } else if (netScroll > 1) {
      // Drag scrolled DOWN — record as a down-gesture and re-evaluate
      // intent against the current geometry the same way a wheel/key
      // gesture would.
      core.noteDownGesture();
      handleScroll();
    }
    // If still sticky after the drag, resume any deferred auto-scroll.
    if (core.canAutoScroll()) notifyContentMaybeGrew();
  }

  // ===== Click-anchor preservation =====

  function clearPendingClickAnchor(): void {
    if (pendingClickAnchorRAF !== null) {
      cancelAnimationFrame(pendingClickAnchorRAF);
      pendingClickAnchorRAF = null;
    }
    pendingClickAnchor = null;
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

    // Clicking an interactive control inside the container always means
    // "I'm interacting here, don't auto-pull me away."
    core.setIntent('free');

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

  function attachContainerListeners(container: HTMLElement): () => void {
    container.addEventListener('scroll', handleScroll, { passive: true });
    container.addEventListener('pointerdown', handlePointerDown, { passive: true });
    container.addEventListener('pointerup', handlePointerUp, { passive: true });
    container.addEventListener('pointercancel', handlePointerUp, { passive: true });
    container.addEventListener('click', handleClickCapture, { capture: true });
    return () => {
      container.removeEventListener('scroll', handleScroll);
      container.removeEventListener('pointerdown', handlePointerDown);
      container.removeEventListener('pointerup', handlePointerUp);
      container.removeEventListener('pointercancel', handlePointerUp);
      container.removeEventListener('click', handleClickCapture, true);
    };
  }

  function detachListeners(): void {
    detachGestureListeners?.();
    detachGestureListeners = null;
    detachContainerListeners?.();
    detachContainerListeners = null;
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
    detachGestureListeners = core.bindGestureListeners(container);
    detachContainerListeners = attachContainerListeners(container);
    attachedContainer = container;
    // Seed the lastObservedScrollHeight so the first scroll event has a
    // valid baseline for the grewThisFrame race gate.
    lastObservedScrollHeight = container.scrollHeight;
  }

  function destroy(): void {
    detachListeners();
    clearPendingContentGrew();
    clearPendingClickAnchor();
    core.resetTransientState();
    pointerDownScrollTopAtStart = -1;
    lastObservedScrollHeight = -1;
  }

  return {
    get intent() {
      return core.intent;
    },
    get isSticky() {
      return core.isSticky;
    },
    isAtBottom,
    forceStick,
    notifyContentMaybeGrew,
    pauseAutoScroll: core.pauseAutoScroll,
    attach,
    destroy,
  };
}
