// Spring-driven sticky-bottom controller, shared by chat MessageTimeline
// and Discussion ChannelView.
//
// Port of stackblitz-labs/use-stick-to-bottom adapted to Svelte 5. Owns
// the user's intent ("glued to bottom" or "free") and a velocity spring
// that smoothly chases the bottom while content grows. A ResizeObserver
// on the content element triggers the spring within the same render
// cycle as a layout change — there's no rAF gap between content arriving
// and the scroll position catching up.
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
// outside the content element (composer growth/shrink). Discussion does
// not currently call notifyContentMaybeGrew because its textarea sits in
// a separate `shrink-0` flex section that doesn't change the scroll
// container's clientHeight; if Discussion ever grows a similar
// composer-height story it would call notify the same way ChatView does.

const DEFAULT_SPRING = { damping: 0.7, stiffness: 0.05, mass: 1.25 } as const;
const SIXTY_FPS_INTERVAL_MS = 1000 / 60;
const STICK_TO_BOTTOM_OFFSET_PX = 70;
const RETAIN_ANIMATION_DURATION_MS = 350;
const RESIZE_CLEAR_PADDING_MS = 1;
// Spring arrival thresholds: distance ≤1px from target AND velocity below
// 0.5 px-per-60fps-frame means we've effectively settled.
const ARRIVAL_DISTANCE_PX = 1;
const ARRIVAL_VELOCITY_THRESHOLD = 0.5;

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
  forceStick(opts?: { animation?: 'instant' | 'spring' }): void;
  /** Cancel any in-flight spring. Call before virtua scrollToIndex. */
  stopScroll(): void;
  /** Public so handleLoadOlder / scrollToItem can opt out of auto-restick. */
  setEscapedFromLock(next: boolean): void;
}

export interface UseStickToBottomOptions {
  /**
   * Spring tuning. Defaults match upstream use-stick-to-bottom
   * (damping 0.7, stiffness 0.05, mass 1.25). Override only if the
   * default chase feels wrong against our event cadence.
   */
  spring?: Partial<typeof DEFAULT_SPRING>;
}

export function createUseStickToBottomController(
  options: UseStickToBottomOptions = {},
): UseStickToBottomController {
  installModuleSelectionListeners();

  const spring = { ...DEFAULT_SPRING, ...(options.spring ?? {}) };

  // ===== Reactive state (consumed by templates / $derived) =====
  // Intent flag: "we want to be glued to the bottom". Mirrors upstream's
  // state.isAtBottom — set true on initial mount, on forceStick, and when
  // a re-stick condition fires from the scroll handler. Set false on
  // explicit escape (wheel/key/touch/select) and on stopScroll. Crucially
  // this is NOT geometry-derived; the spring relies on it staying true
  // even when content grew the bottom out from under us.
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

  let velocity = 0;
  let accumulated = 0;
  let lastTickAt: number | null = null;
  let animationToken: symbol | null = null;
  let resizeDifference = 0;
  let resizeClearTimer: ReturnType<typeof setTimeout> | null = null;
  let ignoreScrollToTop = -1;
  let previousHeight: number | undefined;
  let lastGrewAt = 0;
  let stopRequested = false;
  let touchStartY: number | null = null;

  // ===== Geometry =====
  function targetScrollTop(): number {
    if (!scrollEl) return 0;
    // The -1 is load-bearing: matches upstream so the spring always has
    // a sub-pixel diff to chase, keeping isAtBottom from oscillating.
    return Math.max(0, scrollEl.scrollHeight - 1 - scrollEl.clientHeight);
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
  function writeScrollTop(value: number): void {
    if (!scrollEl) return;
    const computed = window.getComputedStyle(scrollEl);
    const original = computed.scrollBehavior;
    if (original !== 'auto') scrollEl.style.scrollBehavior = 'auto';
    scrollEl.scrollTop = value;
    // Tag using the BROWSER-rounded read so the scroll handler's
    // `scrollTop === ignoreScrollToTop` check matches.
    ignoreScrollToTop = scrollEl.scrollTop;
    if (original !== 'auto') scrollEl.style.scrollBehavior = original;
    refreshIsNearBottom();
  }

  // ===== Spring tick =====
  function startSpringIfNeeded(): void {
    if (animationToken) return;
    if (stopRequested || pauseDepth > 0) return;
    if (!isAtBottomState || escapedFromLockState) return;
    const myToken = Symbol('spring');
    animationToken = myToken;
    lastTickAt = null;

    const tick = (now: number): void => {
      if (animationToken !== myToken) return;
      if (!scrollEl || stopRequested || pauseDepth > 0) {
        animationToken = null;
        return;
      }
      if (!isAtBottomState || escapedFromLockState) {
        animationToken = null;
        velocity = 0;
        accumulated = 0;
        return;
      }
      if (isSelectingInside(scrollEl)) {
        // Re-rAF without advancing — selection should never fight the user.
        requestAnimationFrame(tick);
        return;
      }

      const dt = lastTickAt === null ? 1 : (now - lastTickAt) / SIXTY_FPS_INTERVAL_MS;
      lastTickAt = now;

      const target = targetScrollTop();
      const current = scrollEl.scrollTop;
      const diff = target - current;

      if (current < target) {
        velocity = (spring.damping * velocity + spring.stiffness * diff) / spring.mass;
        accumulated += velocity * dt;
        const before = scrollEl.scrollTop;
        writeScrollTop(current + accumulated);
        if (scrollEl.scrollTop !== before) accumulated = 0;
        // Overscroll guard.
        if (scrollEl.scrollTop > target) writeScrollTop(target);
      }

      const stillChasing = nowMs() - lastGrewAt < RETAIN_ANIMATION_DURATION_MS;
      const arrived =
        Math.abs(scrollEl.scrollTop - targetScrollTop()) < ARRIVAL_DISTANCE_PX &&
        Math.abs(velocity) < ARRIVAL_VELOCITY_THRESHOLD;
      if (arrived && !stillChasing) {
        animationToken = null;
        velocity = 0;
        accumulated = 0;
        return;
      }
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
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
        if (isAtBottomState && !escapedFromLockState) {
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

      // Overscroll guard: if browser auto-clamping or virtua corrections
      // pushed us past the target, snap back.
      if (scrollEl.scrollTop > targetScrollTop()) {
        writeScrollTop(targetScrollTop());
      }

      if (delta > 0) {
        // Positive delta: chase the new bottom. The spring closes any
        // gap smoothly; instant write here would fight virtua's
        // $fixScrollJump on rows above the viewport. The intent flag
        // (isAtBottomState) carries through the chase even though
        // geometric near-bottom (isNearBottomState) momentarily flipped
        // false when the bottom moved out from under us.
        if (isAtBottomState && !escapedFromLockState && pauseDepth === 0) {
          lastGrewAt = nowMs();
          stopRequested = false;
          startSpringIfNeeded();
        }
      } else if (delta < 0) {
        // Negative delta: re-stick if we're now near bottom and not
        // explicitly escaped. Matches upstream's negative-resize branch.
        refreshIsNearBottom();
        if (isNearBottomState && !escapedFromLockState && pauseDepth === 0) {
          isAtBottomState = true;
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
    // Defer 1ms so a concurrent RO callback can update resizeDifference
    // before we interpret direction. Mirrors upstream.
    setTimeout(() => {
      if (!scrollEl) return;
      // Tagged programmatic write — ignore.
      if (scrollTopAtEvent === tag) return;
      // RO race — content just resized; the scroll event reflects layout,
      // not user intent. Most importantly: virtua's $fixScrollJump can
      // adjust scrollTop to keep above-viewport rows stable, which would
      // otherwise look like an up-gesture. For non-virtua consumers
      // (Discussion's ChannelView) this gate is a 1ms suppression window
      // after each content-RO fire — vanishingly unlikely to swallow a
      // real user gesture, since the window only opens immediately after
      // a layout change.
      if (resizeDifference !== 0) return;

      if (isSelectingInside(scrollEl)) {
        setEscapedFromLock(true);
        return;
      }

      // Re-stick: user scrolled BACK near the bottom by hand (touch,
      // scrollbar drag, keyboard). Restore intent flag so the spring
      // can resume on the next content grow. Don't start the spring
      // here — they're already there.
      if (isNearBottomState && escapedFromLockState) {
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
    if (escapedFromLockState === next) return;
    escapedFromLockState = next;
    if (next) {
      // Cancel chase: the spring tick will observe and bail.
      velocity = 0;
      accumulated = 0;
      lastGrewAt = 0;
      isAtBottomState = false;
    }
  }

  function stopScroll(): void {
    setEscapedFromLock(true);
    stopRequested = true;
    // Next rAF tick observes stopRequested and clears animationToken.
  }

  function forceStick(opts?: { animation?: 'instant' | 'spring' }): void {
    setEscapedFromLock(false);
    stopRequested = false;
    if (!scrollEl) return;
    isAtBottomState = true;
    const target = targetScrollTop();
    const animation = opts?.animation ?? 'instant';
    if (animation === 'instant') {
      writeScrollTop(target);
    } else {
      lastGrewAt = nowMs();
      // Slam first, then spring chases any further growth in the chase
      // window. Otherwise the click-to-stick feels laggy on large gaps.
      writeScrollTop(target);
      startSpringIfNeeded();
    }
  }

  function notifyContentMaybeGrew(): void {
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
    writeScrollTop(targetScrollTop());
  }

  function pauseAutoScroll(): () => void {
    pauseDepth += 1;
    let released = false;
    return () => {
      if (released) return;
      released = true;
      pauseDepth = Math.max(0, pauseDepth - 1);
      if (pauseDepth === 0 && !escapedFromLockState && isAtBottomState) {
        // Re-pin on lease release: layout-changing surfaces (sidebar
        // resize, terminal toggle) shrink/grow the chat column during
        // the lease; without this re-pin, sticky users drift.
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
    animationToken = null;
    velocity = 0;
    accumulated = 0;
    resizeDifference = 0;
    previousHeight = undefined;
    lastTickAt = null;
    touchStartY = null;
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
    stopScroll,
    setEscapedFromLock,
  };
}
