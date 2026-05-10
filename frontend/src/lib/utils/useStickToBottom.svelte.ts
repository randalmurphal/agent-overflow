// Sync-pin sticky-bottom controller, shared by chat MessageTimeline
// and Discussion ChannelView.
//
// Port of stackblitz-labs/use-stick-to-bottom adapted to Svelte 5,
// stripped of the upstream velocity-spring chase loop. Owns the user's
// intent ("glued to bottom" or "free") and a single ResizeObserver on
// the content element. Autonomous content growth pins synchronously
// inside the RO callback: the same paint frame where contentEl grows
// also lands scrollTop at the new target, so the user sees content
// arriving at the bottom with no perceptible scroll motion. There's no
// rAF gap between content arriving and the scroll position catching
// up, and no parallel animation loop that could fight a programmatic
// jump. User-initiated snaps (the scroll-to-bottom chip, send) go
// through `forceStick()` which writes scrollTop directly.
//
// Unlike the previous controller, this owns the scroll element directly.
// MessageTimeline pairs it with virtua's <Virtualizer scrollRef={scrollEl}>
// so virtua does its measurement work without owning the scroll container.
// ChannelView is virtua-free — the contentEl is just a `<div>` wrapping
// the `{#each}` over channel messages — and the same controller works
// because the algorithm is agnostic to what's inside contentEl.
//
// External consumers (sidebar resizers, ChatView composer-height
// publication, scrollLeaseDuringTransition helper) speak to this through
// the PaneScrollController interface — pauseAutoScroll() returns a
// depth-counted lease, notifyContentMaybeGrew() handles geometry changes
// outside the content element. Both ChatView (composer overlay RO) and
// ChannelView (composer flex-section RO) call notifyContentMaybeGrew
// when their out-of-content height changes; the seam is identical on
// both surfaces.

import { isUiRenderTraceEnabled, recordUiTrace } from './uiRenderTrace';

// Diagnostic trace helper — no-op in production (gated by
// `isUiRenderTraceEnabled` which only returns true in dev with
// `VITE_AGENT_OVERFLOW_UI_TRACE=1`, set by `make dev DEBUG=1`). The
// thunk skips object construction when disabled. Records flow into
// `${configDir}/ui-trace/ui-render.jsonl` via the same batched
// `AppendUIRenderTraceBatch` binding the timeline render trace uses.
function trace(label: string, build: () => Record<string, unknown>): void {
  if (!isUiRenderTraceEnabled()) return;
  recordUiTrace(label, build());
}

// "Near bottom" threshold for the geometric flag (button visibility,
// negative-delta repin). Loose on purpose: when the user is within 70px,
// the bottom is essentially in view and the scroll-to-bottom chip is
// noise.
const STICK_TO_BOTTOM_OFFSET_PX = 70;
// Re-stick threshold for the SCROLL HANDLER's "user scrolled back" path.
// Must be small: a 70px tolerance means a user wheel-up of 30–50px lands
// inside the threshold, so the same scroll handler that observed the
// escape immediately re-sticks them — their gesture is undone. Keep this
// near-zero so re-stick only triggers when the user has actually scrolled
// essentially all the way back to the bottom. At sticky-bottom,
// `targetScrollTop()` returns `scrollHeight - clientHeight` exactly, so
// the distance-to-bottom from a sticky session is 0 px. 5 leaves margin
// for sub-pixel rounding while still excluding any deliberate
// wheel/touch gesture wider than a few pixels.
const RE_STICK_OFFSET_PX = 5;
const RESIZE_CLEAR_PADDING_MS = 1;
const DEFAULT_PROGRAMMATIC_SCROLL_DURATION_MS = 420;
const PROGRAMMATIC_SCROLL_DISTANCE_THRESHOLD_PX = 1;

const UP_KEYS: ReadonlySet<string> = new Set(['PageUp', 'ArrowUp', 'Home']);
// Down-keys (PageDown / ArrowDown / End) are deliberately NOT enumerated
// here. Re-stick happens geometrically through the scroll handler — when
// the user reaches near-bottom, escapedFromLock is cleared. We don't want
// pressing PageDown to immediately re-stick before any geometry catches up.

let mouseDown = false;
let listenersInstalled = false;

function installModuleSelectionListeners(): void {
  if (listenersInstalled) return;
  if (typeof document === 'undefined') return;
  listenersInstalled = true;
  document.addEventListener('mousedown', () => {
    mouseDown = true;
  }, { capture: true });
  document.addEventListener('mouseup', () => {
    mouseDown = false;
  }, { capture: true });
  document.addEventListener('click', () => {
    mouseDown = false;
  }, { capture: true });
}

/** Test-only escape hatch to reset the module-global mouseDown flag. */
export function resetUseStickToBottomModuleStateForTest(): void {
  mouseDown = false;
}

function isSelectingInside(scrollEl: HTMLElement): boolean {
  if (!mouseDown) return false;
  if (typeof window === 'undefined') return false;
  const sel = window.getSelection?.();
  if (!sel || sel.rangeCount === 0) return false;
  const range = sel.getRangeAt(0);
  // Match upstream: a selection counts if it crosses the scroll element
  // in either direction. Drag-select inside the timeline OR a selection
  // whose anchor sits in the timeline both pause auto-scroll.
  return (
    range.commonAncestorContainer.contains(scrollEl) ||
    scrollEl.contains(range.commonAncestorContainer)
  );
}

function nowMs(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}

export interface UseStickToBottomController {
  /** True when sticky AND no lease is held. Drives auto-follow gating. */
  readonly isSticky: boolean;
  /**
   * True when sticky-by-intent OR within STICK_TO_BOTTOM_OFFSET_PX of
   * the geometric bottom — i.e., any reason the ScrollToBottomButton
   * should be hidden. Mirrors upstream `use-stick-to-bottom`'s return.
   */
  readonly isAtBottom: boolean;
  /** True when the user has explicitly scrolled away (wheel/key/touch/select). */
  readonly escapedFromLock: boolean;

  /** Depth-counted lease that suspends auto-scroll until released. */
  pauseAutoScroll(): () => void;
  /**
   * Notify the controller that the geometry around the content might
   * have changed without contentEl resizing — composer height growth/
   * shrink is the canonical case. Re-pins to the new target if sticky.
   */
  notifyContentMaybeGrew(): void;

  attach(scrollEl: HTMLElement, contentEl: HTMLElement): void;
  detach(): void;

  /** "User wants the bottom now" — called by ScrollToBottomButton, send. */
  forceStick(): void;
  /**
   * Flip intent flags to sticky-bottom WITHOUT writing scrollTop. Pairs
   * with `listRef.scrollToIndex(last, 'end')` — virtua positioned the
   * geometry, this just resumes streaming follow.
   */
  markAtBottom(): void;
  /**
   * Controlled non-native scroll animation for arbitrary timeline jumps.
   * This owns the scrollTop writes so programmatic scroll tagging stays
   * in one place. Used by handleLoadOlder / scrollToItem.
   */
  animateScrollTo(targetTop: number, opts?: { durationMs?: number }): Promise<'completed' | 'cancelled'>;
  /**
   * Mark the upcoming external scroll as not user-driven. Call before
   * `listRef.scrollToIndex(...)` so the controller doesn't auto-restick
   * if virtua's jump happens to land near the bottom.
   */
  stopScroll(): void;
  /** Public so handleLoadOlder / scrollToItem can opt out of auto-restick. */
  setEscapedFromLock(next: boolean): void;
}

export function createUseStickToBottomController(): UseStickToBottomController {
  installModuleSelectionListeners();

  // ===== Reactive state (consumed by templates / $derived) =====
  // Intent flag: "we want to be glued to the bottom". Mirrors upstream's
  // state.isAtBottom — set true on initial mount, on forceStick, and when
  // a re-stick condition fires from the scroll handler. Set false on
  // explicit escape (wheel/key/touch/select) and on stopScroll. Crucially
  // this is NOT geometry-derived; the contentRO sync-pin path relies on
  // it staying true even when content grew the bottom out from under us
  // — that's the gate that keeps the pin from running after the user
  // explicitly scrolled away.
  let isAtBottomState = $state(true);
  // Geometric ≤70px-from-bottom flag. Updated by refreshIsNearBottom on
  // every scroll event and after every programmatic write. The public
  // `isAtBottom` getter returns intent OR geometry — both are reasons to
  // hide the ScrollToBottomButton.
  let isNearBottomState = $state(true);
  let escapedFromLockState = $state(false);
  let pauseDepth = $state(0);

  // ===== Internal bookkeeping (non-reactive) =====
  let scrollEl: HTMLElement | undefined;
  let contentEl: HTMLElement | undefined;
  let contentRO: ResizeObserver | undefined;
  let detachWheel: (() => void) | undefined;
  let detachScroll: (() => void) | undefined;
  let detachKeyTouch: (() => void) | undefined;

  let targetAnimationFrame: number | null = null;
  let targetAnimationResolve: ((result: 'completed' | 'cancelled') => void) | null = null;
  let restoreTargetScrollBehavior: (() => void) | null = null;
  let resizeDifference = 0;
  let resizeClearTimer: ReturnType<typeof setTimeout> | null = null;
  let ignoreScrollToTop = -1;
  let previousHeight: number | undefined;
  let touchStartY: number | null = null;
  // Last user-driven (untagged) scrollTop seen by the scroll handler.
  // Used by the re-stick path to gate on direction: only DOWN-direction
  // scrolls (scrollTop INCREASING) can re-engage auto-follow. Without
  // this, the scroll event triggered by a wheel-up gesture itself would
  // observe `escapedFromLockState=true && distanceFromBottom<=threshold`
  // (because the user just barely moved away from the bottom) and
  // immediately re-stick — undoing the escape on the same gesture that
  // set it. Seeded to the current scrollTop on attach so the very first
  // user scroll already has a meaningful direction baseline; reset to
  // -1 on detach (re-seeded by the next attach).
  let lastUntaggedScrollTop = -1;

  // ===== Geometry =====
  function targetScrollTop(): number {
    if (!scrollEl) return 0;
    // Land at the actual bottom (scrollHeight - clientHeight). Upstream
    // use-stick-to-bottom subtracts an extra -1 px to avoid sub-pixel
    // rounding flipping their geometric isAtBottom check, but this
    // controller's isAtBottom uses a 70 px STICK_TO_BOTTOM_OFFSET_PX
    // band (button visibility) and re-stick uses RE_STICK_OFFSET_PX
    // (5 px) — both wide enough that one sub-pixel doesn't matter.
    // Subtracting -1 here just left the user 1 px above the actual
    // bottom; the scrollbar showed a one-tick gap and the snap felt
    // incomplete.
    return Math.max(0, scrollEl.scrollHeight - scrollEl.clientHeight);
  }
  function distanceFromBottom(): number {
    if (!scrollEl) return 0;
    return scrollEl.scrollHeight - scrollEl.scrollTop - scrollEl.clientHeight;
  }
  function refreshIsNearBottom(): void {
    const dist = distanceFromBottom();
    const next = dist <= STICK_TO_BOTTOM_OFFSET_PX;
    if (next !== isNearBottomState) isNearBottomState = next;
  }

  // ===== Programmatic scroll write =====
  // Diagnostic: `writeCaller` is set by the public-facing scrollTop
  // writer (forceStick / notifyContentMaybeGrew / contentRO /
  // animateScrollTo / overscroll-guard) before delegating to
  // `writeScrollTop` so the trace can attribute every write to its
  // origin. No semantic effect; production builds short-circuit at the
  // `isUiRenderTraceEnabled` check inside `trace()`.
  let writeCaller: string = 'unknown';
  function writeProgrammaticScrollTop(value: number): void {
    if (!scrollEl) return;
    const beforeTop = scrollEl.scrollTop;
    const beforeHeight = scrollEl.scrollHeight;
    const beforeClient = scrollEl.clientHeight;
    scrollEl.scrollTop = value;
    // Tag using the BROWSER-rounded read so the scroll handler's
    // `scrollTop === ignoreScrollToTop` check matches.
    ignoreScrollToTop = scrollEl.scrollTop;
    refreshIsNearBottom();
    trace('scroll.write', () => ({
      caller: writeCaller,
      requested: Math.round(value),
      beforeTop: Math.round(beforeTop),
      afterTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: Math.round(beforeHeight),
      clientHeight: Math.round(beforeClient),
      maxTarget: Math.round(Math.max(0, beforeHeight - beforeClient)),
      ignoreScrollToTop,
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
      isNearBottomState,
    }));
  }

  function writeScrollTop(value: number): void {
    if (!scrollEl) return;
    const computed = window.getComputedStyle(scrollEl);
    const original = computed.scrollBehavior;
    if (original !== 'auto') scrollEl.style.scrollBehavior = 'auto';
    writeProgrammaticScrollTop(value);
    if (original !== 'auto') scrollEl.style.scrollBehavior = original;
  }

  function requestFrame(callback: FrameRequestCallback): number {
    return typeof requestAnimationFrame === 'function'
      ? requestAnimationFrame(callback)
      : window.setTimeout(() => callback(nowMs()), 0);
  }

  function cancelFrame(handle: number): void {
    if (typeof cancelAnimationFrame === 'function') {
      cancelAnimationFrame(handle);
    } else {
      window.clearTimeout(handle);
    }
  }

  function prefersReducedMotion(): boolean {
    return typeof window !== 'undefined'
      && typeof window.matchMedia === 'function'
      && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  }

  function maxScrollTop(): number {
    if (!scrollEl) return 0;
    return Math.max(0, scrollEl.scrollHeight - scrollEl.clientHeight);
  }

  function clampScrollTop(value: number): number {
    return Math.max(0, Math.min(value, maxScrollTop()));
  }

  function easeOutCubic(t: number): number {
    const remaining = 1 - t;
    return 1 - remaining * remaining * remaining;
  }

  function finishTargetAnimation(result: 'completed' | 'cancelled'): void {
    if (targetAnimationFrame !== null) {
      cancelFrame(targetAnimationFrame);
      targetAnimationFrame = null;
    }
    restoreTargetScrollBehavior?.();
    restoreTargetScrollBehavior = null;
    const resolve = targetAnimationResolve;
    targetAnimationResolve = null;
    if (resolve) resolve(result);
  }

  function cancelTargetAnimation(): void {
    finishTargetAnimation('cancelled');
  }

  // ===== Content RO =====
  function setupContentRO(): void {
    if (!contentEl) return;
    if (typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (!entry || !contentEl || !scrollEl) return;
      const nextHeight = entry.contentRect.height;
      const prev = previousHeight;
      previousHeight = nextHeight;

      if (prev === undefined) {
        // First fire: snap to bottom synchronously so the initial paint
        // lands at the right place. Matches upstream's `initial` behavior
        // when isAtBottom starts true.
        const willPin = isAtBottomState && !escapedFromLockState;
        trace('scroll.contentRO.firstFire', () => ({
          nextHeight: Math.round(nextHeight),
          willPin,
          isAtBottomState,
          escapedFromLockState,
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
          scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
          clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        }));
        if (willPin) {
          writeCaller = 'contentRO.firstFire';
          writeScrollTop(targetScrollTop());
        }
        refreshIsNearBottom();
        return;
      }

      const delta = nextHeight - prev;
      // Common case: virtua re-measures a same-height row, padding-bottom
      // CSS variable updates with identical computed value, etc. No
      // geometry change → nothing to chase, no scroll-event tagging needed.
      if (delta === 0) return;
      resizeDifference = delta;

      const overshoot = scrollEl.scrollTop > targetScrollTop();
      const positiveWillPin = delta > 0
        && isAtBottomState && !escapedFromLockState && pauseDepth === 0;
      const negativeWillPin = delta < 0
        && (() => {
          // Mirror the negative branch's near-bottom check WITHOUT
          // mutating refreshIsNearBottom side effects in the trace.
          const dist = scrollEl.scrollHeight - scrollEl.scrollTop - scrollEl.clientHeight;
          return dist <= STICK_TO_BOTTOM_OFFSET_PX
            && !escapedFromLockState && pauseDepth === 0;
        })();
      trace('scroll.contentRO', () => ({
        prev: Math.round(prev),
        next: Math.round(nextHeight),
        delta: Math.round(delta),
        overshoot,
        positiveWillPin,
        negativeWillPin,
        isAtBottomState,
        escapedFromLockState,
        pauseDepth,
        isNearBottomState,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
        clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        target: scrollEl ? Math.round(targetScrollTop()) : null,
      }));

      // Overscroll guard: if browser auto-clamping or virtua corrections
      // pushed us past the target, snap back.
      if (overshoot) {
        writeCaller = 'contentRO.overshoot';
        writeScrollTop(targetScrollTop());
      }

      if (delta > 0) {
        // Positive delta: re-pin to the new bottom synchronously, before
        // paint. The browser only paints the final state per frame, so
        // contentEl growing AND scrollTop catching up happen in the same
        // visual frame — no perceptible scroll motion, just content
        // arriving at the bottom. Works regardless of WHY the bottom is
        // moving: streaming chunks, svelte-streamdown async typesetting
        // (shiki/KaTeX/mermaid), virtua's per-row remeasurement after a
        // cache-hit mount, parseIncompleteMarkdown rebalance.
        if (isAtBottomState && !escapedFromLockState && pauseDepth === 0) {
          writeCaller = 'contentRO.positiveDelta';
          writeScrollTop(targetScrollTop());
        }
      } else if (delta < 0) {
        // Negative delta: re-stick if we're now near bottom and not
        // explicitly escaped. Matches upstream's negative-resize branch.
        refreshIsNearBottom();
        if (isNearBottomState && !escapedFromLockState && pauseDepth === 0) {
          isAtBottomState = true;
          writeCaller = 'contentRO.negativeDelta';
          writeScrollTop(targetScrollTop());
        }
      }

      refreshIsNearBottom();

      // Schedule resizeDifference clear AFTER the scroll handler's 1ms.
      // The scroll event fired by the layout change above must observe
      // resizeDifference !== 0 so it bails the up-direction inference.
      if (resizeClearTimer) clearTimeout(resizeClearTimer);
      const myDelta = delta;
      resizeClearTimer = setTimeout(() => {
        requestAnimationFrame(() => {
          if (resizeDifference === myDelta) resizeDifference = 0;
        });
      }, RESIZE_CLEAR_PADDING_MS);
    });
    ro.observe(contentEl);
    contentRO = ro;
  }

  // ===== Wheel handler =====
  function isOverflowAncestor(el: Element): boolean {
    if (!(el instanceof HTMLElement)) return false;
    const cs = window.getComputedStyle(el);
    return /(auto|scroll)/.test(cs.overflowY) || /(auto|scroll)/.test(cs.overflow);
  }
  function handleWheel(e: WheelEvent): void {
    if (!scrollEl) return;
    if (e.deltaY >= 0) return; // only up-wheel can break the lock
    // Walk parents from event target; if first overflow ancestor is the
    // scroll element, the wheel landed on us. If a nested scroller (e.g.
    // a code block with overflow-y:auto) is encountered first, ignore —
    // the user is scrolling that nested element, not us.
    let cur: Element | null = e.target instanceof Element ? e.target : null;
    while (cur && cur !== scrollEl) {
      if (isOverflowAncestor(cur) && cur.scrollHeight > cur.clientHeight) return;
      cur = cur.parentElement;
    }
    if (cur !== scrollEl) return;
    if (scrollEl.scrollHeight <= scrollEl.clientHeight) return;
    trace('scroll.wheel.escape', () => ({
      deltaY: e.deltaY,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      isAtBottomState,
      escapedFromLockState,
    }));
    setEscapedFromLock(true);
  }

  // ===== Scroll handler =====
  function handleScroll(): void {
    if (!scrollEl) return;
    const scrollTopAtEvent = scrollEl.scrollTop;
    // Capture and consume the programmatic-write tag synchronously so
    // it only suppresses ONE scroll event. Otherwise a later genuine
    // user scroll back to the same scrollTop value would be ignored.
    const tag = ignoreScrollToTop;
    ignoreScrollToTop = -1;
    refreshIsNearBottom();
    const tagged = scrollTopAtEvent === tag;
    trace('scroll.scrollEvent', () => ({
      scrollTop: Math.round(scrollTopAtEvent),
      tag: Math.round(tag),
      tagged,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      resizeDifference: Math.round(resizeDifference),
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
      isNearBottomState,
    }));
    // Tagged programmatic write — bail synchronously without scheduling
    // the deferral timer. Steady-state streaming fires a sync-pin write
    // on every contentRO positive delta; allocating a closure + timer
    // registration for each one just to no-op inside the callback was
    // hundreds of throwaway allocs/sec on long assistant turns. The 1 ms
    // RO-race deferral below isn't needed for tagged writes — the tag is
    // set synchronously by writeScrollTop, so we already know this event
    // reflects our own write, not user intent.
    if (tagged) return;
    cancelTargetAnimation();
    // Capture direction baseline BEFORE the deferral. We're inside the
    // synchronous handler for the current scroll event; this scrollTop
    // is what the user just produced. Used by the deferred re-stick
    // check to distinguish "user scrolled DOWN toward bottom" (re-stick
    // candidate) from "user scrolled UP" (must NOT re-stick — undoing
    // the wheel handler's just-set escape on the same gesture).
    const previousUntagged = lastUntaggedScrollTop;
    lastUntaggedScrollTop = scrollTopAtEvent;
    // Defer 1ms so a concurrent RO callback can update resizeDifference
    // before we interpret direction. Mirrors upstream.
    setTimeout(() => {
      if (!scrollEl) return;
      // RO race — content just resized; the scroll event reflects layout,
      // not user intent. Most importantly: virtua's $fixScrollJump can
      // adjust scrollTop to keep above-viewport rows stable, which would
      // otherwise look like an up-gesture. For non-virtua consumers
      // (Discussion's ChannelView) this gate is a 1ms suppression window
      // after each content-RO fire — vanishingly unlikely to swallow a
      // real user gesture, since the window only opens immediately after
      // a layout change.
      if (resizeDifference !== 0) {
        trace('scroll.scrollEvent.deferred.bailRO', () => ({
          resizeDifference: Math.round(resizeDifference),
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        }));
        return;
      }

      if (isSelectingInside(scrollEl)) {
        trace('scroll.scrollEvent.deferred.escapeSelection', () => ({
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        }));
        setEscapedFromLock(true);
        return;
      }

      // Re-stick: user scrolled BACK essentially to the bottom by hand
      // (touch, scrollbar drag, keyboard). Restore intent flag so the
      // contentRO sync-pin can resume on the next content grow. No
      // scrollTop write here — they're already at the bottom.
      //
      // Two gates:
      //   1. Direction. The scroll event from a wheel-up gesture itself
      //      arrives RIGHT AFTER handleWheel set escape; if we re-sticked
      //      whenever the user happens to land near the bottom, that
      //      same event would undo the escape. Skip re-stick when
      //      scrollTop is decreasing (UP direction); only DOWN-direction
      //      scrolls toward the bottom can re-engage auto-follow.
      //   2. RE_STICK_OFFSET_PX (small) rather than the looser
      //      isNearBottomState (70px). Even on a DOWN scroll, only
      //      essentially-at-bottom positions count.
      const scrolledDown = previousUntagged < 0
        ? false
        : scrollTopAtEvent > previousUntagged;
      const distFromBottom = distanceFromBottom();
      const willRestick = scrolledDown
        && escapedFromLockState
        && distFromBottom <= RE_STICK_OFFSET_PX;
      trace('scroll.scrollEvent.deferred', () => ({
        scrollTop: Math.round(scrollTopAtEvent),
        previousUntagged: Math.round(previousUntagged),
        scrolledDown,
        distanceFromBottom: Math.round(distFromBottom),
        willRestick,
        isAtBottomState,
        escapedFromLockState,
      }));
      if (willRestick) {
        escapedFromLockState = false;
        isAtBottomState = true;
      }
    }, RESIZE_CLEAR_PADDING_MS);
  }

  // ===== Keydown / touch handlers (intent signals) =====
  function handleKeydown(e: KeyboardEvent): void {
    if (UP_KEYS.has(e.key)) setEscapedFromLock(true);
    // DOWN_KEYS deliberately not handled here — see the comment near the
    // UP_KEYS declaration for why down-direction is geometric, not gestural.
  }
  function handleTouchStart(e: TouchEvent): void {
    touchStartY = e.touches[0]?.clientY ?? null;
  }
  function handleTouchMove(e: TouchEvent): void {
    if (touchStartY === null) return;
    const y = e.touches[0]?.clientY ?? touchStartY;
    const dy = y - touchStartY;
    touchStartY = y;
    // Finger moves DOWN visually → content moves DOWN → user wants to
    // see content above → escape. Finger moves UP → leave to scroll
    // handler's re-stick path.
    if (dy > 1) setEscapedFromLock(true);
  }
  function handleTouchEnd(): void {
    touchStartY = null;
  }

  // ===== Public actions =====
  function setEscapedFromLock(next: boolean): void {
    if (next) cancelTargetAnimation();
    if (escapedFromLockState === next) return;
    const previousIsAtBottom = isAtBottomState;
    escapedFromLockState = next;
    if (next) {
      isAtBottomState = false;
    }
    trace('scroll.escape.set', () => ({
      next,
      previousIsAtBottom,
      isAtBottomState,
      pauseDepth,
      isNearBottomState,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
    }));
  }

  function stopScroll(): void {
    // Mark the upcoming external scroll as not user-driven so the
    // controller doesn't auto-restick if virtua's jump (or another
    // programmatic write) lands near the bottom. Also cancels any
    // in-flight animateScrollTo via setEscapedFromLock's
    // cancelTargetAnimation call.
    setEscapedFromLock(true);
  }

  function animateScrollTo(
    rawTargetTop: number,
    opts?: { durationMs?: number },
  ): Promise<'completed' | 'cancelled'> {
    if (!scrollEl) return Promise.resolve('cancelled');
    const targetScrollEl = scrollEl;
    cancelTargetAnimation();

    const targetTop = clampScrollTop(rawTargetTop);
    const startTop = targetScrollEl.scrollTop;
    const distance = targetTop - startTop;
    if (Math.abs(distance) <= PROGRAMMATIC_SCROLL_DISTANCE_THRESHOLD_PX) {
      return Promise.resolve('completed');
    }
    setEscapedFromLock(true);
    const durationMs = opts?.durationMs ?? DEFAULT_PROGRAMMATIC_SCROLL_DURATION_MS;
    if (
      prefersReducedMotion()
      || durationMs <= 0
    ) {
      writeCaller = 'animateScrollTo.instant';
      writeScrollTop(targetTop);
      return Promise.resolve('completed');
    }

    return new Promise((resolve) => {
      targetAnimationResolve = resolve;
      const startedAt = nowMs();
      const originalInlineScrollBehavior = targetScrollEl.style.scrollBehavior;
      if (window.getComputedStyle(targetScrollEl).scrollBehavior !== 'auto') {
        targetScrollEl.style.scrollBehavior = 'auto';
        restoreTargetScrollBehavior = () => {
          targetScrollEl.style.scrollBehavior = originalInlineScrollBehavior;
        };
      }

      const tick = (now: number): void => {
        if (!scrollEl || targetAnimationResolve !== resolve) return;
        const elapsed = Math.max(0, now - startedAt);
        const progress = Math.min(1, elapsed / durationMs);
        const eased = easeOutCubic(progress);
        writeCaller = 'animateScrollTo.tick';
        writeProgrammaticScrollTop(startTop + distance * eased);
        if (progress >= 1 || Math.abs(scrollEl.scrollTop - targetTop) <= PROGRAMMATIC_SCROLL_DISTANCE_THRESHOLD_PX) {
          writeCaller = 'animateScrollTo.finish';
          writeProgrammaticScrollTop(targetTop);
          finishTargetAnimation('completed');
          return;
        }
        targetAnimationFrame = requestFrame(tick);
      };

      targetAnimationFrame = requestFrame(tick);
    });
  }

  function forceStick(): void {
    trace('scroll.forceStick.entry', () => ({
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      target: scrollEl ? Math.round(targetScrollTop()) : null,
    }));
    setEscapedFromLock(false);
    if (!scrollEl) return;
    isAtBottomState = true;
    writeCaller = 'forceStick';
    writeScrollTop(targetScrollTop());
  }

  function markAtBottom(): void {
    // Flag-only counterpart to forceStick: caller already positioned
    // the geometry (typically via virtua's listRef.scrollToIndex(last,
    // 'end')), we just resume streaming follow.
    trace('scroll.markAtBottom', () => ({
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
    }));
    setEscapedFromLock(false);
    isAtBottomState = true;
    refreshIsNearBottom();
  }

  function notifyContentMaybeGrew(): void {
    const gateScrollEl = scrollEl !== undefined;
    const gateEscape = escapedFromLockState;
    const gatePause = pauseDepth > 0;
    const gateNotAtBottom = !isAtBottomState;
    const willPin = gateScrollEl && !gateEscape && !gatePause && !gateNotAtBottom;
    trace('scroll.notifyContentMaybeGrew', () => ({
      willPin,
      gateScrollEl,
      gateEscape,
      gatePause,
      gateNotAtBottom,
      pauseDepth,
      isNearBottomState,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      target: scrollEl ? Math.round(targetScrollTop()) : null,
    }));
    if (!scrollEl) return;
    if (escapedFromLockState || pauseDepth > 0) return;
    if (!isAtBottomState) return;
    // Stamp resizeDifference BEFORE writing scrollTop so the resulting
    // scroll event is treated as RO-correlated, not user-driven. Without
    // this, a textarea-shrink could cause the scroll handler's re-stick
    // path to flip isAtBottom in a way that surprises the user.
    resizeDifference = 1;
    if (resizeClearTimer) clearTimeout(resizeClearTimer);
    resizeClearTimer = setTimeout(() => {
      requestAnimationFrame(() => {
        if (resizeDifference === 1) resizeDifference = 0;
      });
    }, RESIZE_CLEAR_PADDING_MS);
    writeCaller = 'notifyContentMaybeGrew';
    writeScrollTop(targetScrollTop());
  }

  function pauseAutoScroll(): () => void {
    pauseDepth += 1;
    trace('scroll.pause.acquire', () => ({
      pauseDepth,
      isAtBottomState,
      escapedFromLockState,
    }));
    let released = false;
    return () => {
      if (released) return;
      released = true;
      pauseDepth = Math.max(0, pauseDepth - 1);
      const willRepin = pauseDepth === 0 && !escapedFromLockState && isAtBottomState;
      trace('scroll.pause.release', () => ({
        pauseDepth,
        willRepin,
        isAtBottomState,
        escapedFromLockState,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
        clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        target: scrollEl ? Math.round(targetScrollTop()) : null,
      }));
      if (willRepin) {
        // Re-pin on lease release: layout-changing surfaces (sidebar
        // resize, terminal toggle) shrink/grow the chat column during
        // the lease; without this re-pin, sticky users drift.
        writeCaller = 'pauseAutoScroll.release';
        writeScrollTop(targetScrollTop());
      }
    };
  }

  // ===== Lifecycle =====
  function attach(nextScrollEl: HTMLElement, nextContentEl: HTMLElement): void {
    if (scrollEl === nextScrollEl && contentEl === nextContentEl) return;
    detach();
    scrollEl = nextScrollEl;
    contentEl = nextContentEl;
    trace('scroll.attach', () => ({
      surface: nextScrollEl.dataset?.testid ?? '',
      scrollTop: Math.round(nextScrollEl.scrollTop),
      scrollHeight: Math.round(nextScrollEl.scrollHeight),
      clientHeight: Math.round(nextScrollEl.clientHeight),
      contentHeight: Math.round(nextContentEl.getBoundingClientRect().height),
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
    }));
    setupContentRO();
    nextScrollEl.addEventListener('wheel', handleWheel, { passive: true });
    nextScrollEl.addEventListener('scroll', handleScroll, { passive: true });
    nextScrollEl.addEventListener('keydown', handleKeydown);
    nextScrollEl.addEventListener('touchstart', handleTouchStart, { passive: true });
    nextScrollEl.addEventListener('touchmove', handleTouchMove, { passive: true });
    nextScrollEl.addEventListener('touchend', handleTouchEnd, { passive: true });
    nextScrollEl.addEventListener('touchcancel', handleTouchEnd, { passive: true });
    detachWheel = () => nextScrollEl.removeEventListener('wheel', handleWheel);
    detachScroll = () => nextScrollEl.removeEventListener('scroll', handleScroll);
    detachKeyTouch = () => {
      nextScrollEl.removeEventListener('keydown', handleKeydown);
      nextScrollEl.removeEventListener('touchstart', handleTouchStart);
      nextScrollEl.removeEventListener('touchmove', handleTouchMove);
      nextScrollEl.removeEventListener('touchend', handleTouchEnd);
      nextScrollEl.removeEventListener('touchcancel', handleTouchEnd);
    };
    // Seed the direction baseline from the live scrollTop so the very
    // first user scroll already has something to compare against; the
    // re-stick direction gate would otherwise treat the first scroll
    // as "no direction info" and never fire on it.
    lastUntaggedScrollTop = nextScrollEl.scrollTop;
    refreshIsNearBottom();
  }

  function detach(): void {
    contentRO?.disconnect();
    contentRO = undefined;
    detachWheel?.();
    detachWheel = undefined;
    detachScroll?.();
    detachScroll = undefined;
    detachKeyTouch?.();
    detachKeyTouch = undefined;
    if (resizeClearTimer) {
      clearTimeout(resizeClearTimer);
      resizeClearTimer = null;
    }
    cancelTargetAnimation();
    resizeDifference = 0;
    previousHeight = undefined;
    touchStartY = null;
    lastUntaggedScrollTop = -1;
    scrollEl = undefined;
    contentEl = undefined;
  }

  return {
    get isSticky() {
      return isAtBottomState && !escapedFromLockState && pauseDepth === 0;
    },
    get isAtBottom() {
      // Intent OR geometry — both are reasons to hide ScrollToBottomButton.
      // Mirrors upstream's `isAtBottom: isAtBottom || isNearBottom` return.
      return isAtBottomState || isNearBottomState;
    },
    get escapedFromLock() {
      return escapedFromLockState;
    },
    pauseAutoScroll,
    notifyContentMaybeGrew,
    attach,
    detach,
    forceStick,
    markAtBottom,
    animateScrollTo,
    stopScroll,
    setEscapedFromLock,
  };
}
