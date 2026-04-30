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
// The intent state machine + gesture interpretation + pause-lease live
// in scrollIntentCore.svelte.ts and are shared with stickToBottom (the
// DOM-container variant used by Discussion-mode ChannelView). This file
// is the virtua-specific glue: geometry adapter, pointer handlers, and
// the onScroll/onScrollEnd hooks virtua's VList calls back into.

import type { VListHandle } from 'virtua/svelte';
import {
  createScrollIntentCore,
  type ScrollIntentCore,
  type StickIntent,
} from './scrollIntentCore.svelte';

export type { StickIntent } from './scrollIntentCore.svelte';

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
  /**
   * Fires after every settle of an auto-scroll path: the deferred
   * `notifyContentMaybeGrew` rAF (both the success branch that ran
   * `scrollToLast` and the early-bail branch where `canAutoScroll()` flipped
   * to false during the rAF gap), and the synchronous `forceStick` path used
   * by the scroll-to-bottom button. Used by the timeline to unmask rows that
   * were rendered `visibility: hidden` to suppress the one-frame
   * wrong-position flash between content paint and scroll catch-up. Always
   * called after the scroll write so the unmask paints in the same frame as
   * the corrected scroll position.
   */
  onScrollSettled?: () => void;
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

export function createStickyBottomController(
  options: StickyBottomOptions,
): StickyBottomController {
  const threshold = options.threshold ?? DEFAULT_THRESHOLD;

  const core: ScrollIntentCore = createScrollIntentCore({
    gestureWindowMs: options.gestureWindowMs,
  });

  // Last scroll size observed at a scroll event. Used to gate gesture
  // restick: a scroll event that coincided with content growth shouldn't
  // be interpreted as the user scrolling to the bottom — the bottom came
  // up to meet them.
  let lastObservedScrollSize = -1;
  let pointerDownOffsetAtStart = -1;

  // Coalesced rAF token for deferred scrollToLast. notifyContentMaybeGrew
  // can fire many times per frame during streaming (each delta flush
  // bumps liveDeltaRevision); we want at most one scrollToLast per
  // frame, scheduled AFTER virtua's per-row ResizeObserver has updated
  // its cache from the just-rendered DOM. Without the deferral, the
  // controller reads stale geometry and the scroll lands at the
  // pre-grow bottom.
  let pendingScrollFrame: number | null = null;

  let attachedEl: HTMLElement | null = null;
  let detachGestureListeners: (() => void) | null = null;
  let detachPointerListeners: (() => void) | null = null;
  // ResizeObserver on the scroll wrapper. When the chat column's
  // clientHeight shrinks (terminal drawer opens, window resized smaller,
  // sidebar fly-in narrows the column) the auto-follow $effect doesn't
  // fire because no data changed — we have to schedule a re-pin
  // explicitly.
  let viewportResizeObserver: ResizeObserver | null = null;

  // ===== Geometry =====

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

  function scrollToLast(handle: VListHandle): void {
    const last = options.getLastIndex();
    if (last < 0) return;
    handle.scrollToIndex(last, { align: 'end' });
    lastObservedScrollSize = handle.getScrollSize();
  }

  // ===== Auto-follow / forceStick =====

  function notifyContentMaybeGrew(): void {
    if (!core.canAutoScroll()) return;
    if (pendingScrollFrame !== null) return;
    pendingScrollFrame = requestAnimationFrame(() => {
      pendingScrollFrame = null;
      // Both branches must invoke onScrollSettled — the bail path (user
      // gestured to free during the rAF gap) still needs to release any
      // visibility masks the timeline put in place when the rAF was
      // scheduled, otherwise the rows stay hidden until the next thread
      // switch wipes the registry. Order is scroll-first-then-settle so the
      // unmask paints in the same frame as the corrected scroll position.
      if (!core.canAutoScroll()) {
        options.onScrollSettled?.();
        return;
      }
      const handle = options.getListHandle();
      if (handle) scrollToLast(handle);
      options.onScrollSettled?.();
    });
  }

  // Wraps the core lease so the release path can re-pin to the bottom
  // when the user is still sticky. Layout-changing surfaces (terminal
  // drawer toggle, sidebar fly-in/out, RHS resizer drag) hold a lease
  // across the transition; during that window the auto-follow $effect
  // is blocked. Without a post-release re-pin, a sticky user drifts off
  // the bottom whenever any of those surfaces reflows the chat column.
  function pauseAutoScroll(): () => void {
    const releaseInner = core.pauseAutoScroll();
    let released = false;
    return () => {
      if (released) return;
      released = true;
      releaseInner();
      if (core.canAutoScroll()) notifyContentMaybeGrew();
    };
  }

  function forceStick(): void {
    core.setIntent('stick');
    // Defer when a pointer is held: yanking the user's scroll position
    // mid-drag would erase their drag work, and pointerup will resume
    // auto-scroll naturally. Any pending visibility masks unmask via the
    // pointerup handler's `notifyContentMaybeGrew` → rAF → onScrollSettled
    // chain, so we don't need to settle here.
    if (core.isPointerDown()) return;
    const handle = options.getListHandle();
    if (handle) scrollToLast(handle);
    // Bypass the deferred-rAF path entirely — `forceStick` is synchronous —
    // so settle notification still has to fire here. Without it, items the
    // timeline masked while free would never unmask after the user clicks
    // scroll-to-bottom.
    options.onScrollSettled?.();
  }

  // ===== VList event hooks =====

  function onScroll(_offset: number): void {
    // Steady-state sticky: skip the geometry read. The grewThisFrame race
    // gate is only consulted for restick decisions in the free state.
    if (core.intent !== 'free' || core.isPointerDown() || !core.inDownGestureWindow()) {
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

    if (grewThisFrame) return;
    if (!isAtGeometricBottom(handle)) return;

    core.setIntent('stick');
    core.clearDownGesture();
  }

  function onScrollEnd(): void {
    // Reserved for future use. virtua dispatches this after a scroll
    // settles; we don't currently need to react. Keeping the hook so
    // callers don't have to wire it conditionally.
  }

  // ===== Pointer handlers (geometry-specific; gesture handlers live in core) =====

  function handlePointerDown(_e: PointerEvent): void {
    core.setPointerDown(true);
    const handle = options.getListHandle();
    pointerDownOffsetAtStart = handle?.getScrollOffset() ?? -1;
  }

  function handlePointerUp(_e: PointerEvent): void {
    core.setPointerDown(false);
    const handle = options.getListHandle();
    const startOffset = pointerDownOffsetAtStart;
    pointerDownOffsetAtStart = -1;
    if (!handle || startOffset < 0) return;

    const netScroll = handle.getScrollOffset() - startOffset;
    if (netScroll < -1) {
      // Drag scrolled UP — user clearly wants to be free.
      core.setIntent('free');
    } else if (netScroll > 1) {
      // Drag scrolled DOWN — record as a down-gesture and re-evaluate
      // intent against the current geometry the same way a wheel/key
      // gesture would.
      core.noteDownGesture();
      onScroll(handle.getScrollOffset());
    }
    // If still sticky after the drag, resume any deferred auto-scroll.
    if (core.canAutoScroll()) notifyContentMaybeGrew();
  }

  function attachPointerListeners(el: HTMLElement): () => void {
    el.addEventListener('pointerdown', handlePointerDown, { passive: true });
    el.addEventListener('pointerup', handlePointerUp, { passive: true });
    el.addEventListener('pointercancel', handlePointerUp, { passive: true });
    return () => {
      el.removeEventListener('pointerdown', handlePointerDown);
      el.removeEventListener('pointerup', handlePointerUp);
      el.removeEventListener('pointercancel', handlePointerUp);
    };
  }

  // ===== Listener lifecycle =====

  function detachListeners(): void {
    detachGestureListeners?.();
    detachGestureListeners = null;
    detachPointerListeners?.();
    detachPointerListeners = null;
    viewportResizeObserver?.disconnect();
    viewportResizeObserver = null;
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
    detachGestureListeners = core.bindGestureListeners(el);
    detachPointerListeners = attachPointerListeners(el);
    if (typeof ResizeObserver !== 'undefined') {
      viewportResizeObserver = new ResizeObserver(() => {
        notifyContentMaybeGrew();
      });
      viewportResizeObserver.observe(el);
    }
    attachedEl = el;
    // Seed lastObservedScrollSize so the first onScroll event has a valid
    // baseline for the grewThisFrame race gate.
    const handle = options.getListHandle();
    if (handle) lastObservedScrollSize = handle.getScrollSize();
  }

  function destroy(): void {
    detachListeners();
    core.resetTransientState();
    pointerDownOffsetAtStart = -1;
    lastObservedScrollSize = -1;
    if (pendingScrollFrame !== null) {
      cancelAnimationFrame(pendingScrollFrame);
      pendingScrollFrame = null;
      // The pending rAF would have invoked onScrollSettled; firing it here
      // keeps any visibility masks the timeline put in place from outliving
      // the controller across HMR/remount.
      options.onScrollSettled?.();
    }
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
    pauseAutoScroll,
    onScroll,
    onScrollEnd,
    attach,
    destroy,
  };
}
