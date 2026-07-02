// User-intent state machine for the sticky-bottom controller
// (useStickToBottom): escape / re-stick / restore-snap consent, the
// recent-down-intent window, scrollbar-drag sessions, selection
// tracking, and the classification machinery that separates user
// scrolls from programmatic ones (the token ring for controller writes,
// the time-window tag for external writers like virtua scrollToIndex).
//
// Intent is event-sourced, never geometry-inferred: every transition
// here is driven by a wheel/key/touch/pointer/scroll event, not by
// observing where scrollTop happens to be. The reactive intent flags
// (isAtBottom / escapedFromLock) live in the controller — templates
// subscribe to them — and the machine reads and flips them through
// accessor deps. The machine owns everything else: its DOM listeners
// (attach/detach), the down-intent and drag-session windows, and the
// one-shot restore consent.

import { AUTO_FOLLOW_BOTTOM_EPSILON_PX } from './resolver';
import type { SpringChase } from './spring';
import { nowMs } from './time';
import { trace } from './trace';
import { isUiRenderTraceEnabled } from '../uiRenderTrace';

const RECENT_DOWN_INTENT_WINDOW_MS = 250;
const SCROLLBAR_DRAG_SESSION_FAILSAFE_MS = 30_000;
const EXTERNAL_SCROLL_TAG_CLEAR_MS = 100;
const PROGRAMMATIC_SCROLL_EVENT_TOKEN_TTL_MS = 500;
const MAX_PROGRAMMATIC_SCROLL_EVENT_TOKENS = 128;
const PROGRAMMATIC_SCROLL_EVENT_DUPLICATE_BUDGET = 4;
// The 1ms deferral before scroll-intent interpretation, so a concurrent
// ResizeObserver callback can stamp its resize classification first.
// Exported because the observer side schedules its resizeDifference
// clears to land strictly AFTER this window.
export const RESIZE_CLEAR_PADDING_MS = 1;

const UP_KEYS: ReadonlySet<string> = new Set(['PageUp', 'ArrowUp', 'Home']);
const DOWN_KEYS: ReadonlySet<string> = new Set(['PageDown', 'ArrowDown', 'End']);

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
export function resetScrollIntentModuleStateForTest(): void {
  mouseDown = false;
}

export function isSelectingInside(scrollEl: HTMLElement): boolean {
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

// Everything the machine needs from the controller. The reactive flags
// are accessor pairs because they are runes ($state) owned by the
// controller closure; the rest is geometry and the spring instance
// (escape bails the chase, re-stick clears the stop request).
export interface ScrollIntentDeps {
  getScrollEl(): HTMLElement | undefined;
  isAtBottom(): boolean;
  setIsAtBottom(next: boolean): void;
  escaped(): boolean;
  setEscaped(next: boolean): void;
  /** Trace-payload reads. */
  isNearBottom(): boolean;
  pauseDepth(): number;
  distanceFromBottom(): number;
  refreshIsNearBottom(): number;
  spring: SpringChase;
  /**
   * One-shot sample of "this scroll event is correlated with a content
   * resize" (observer-owned resizeDifference plus the one-event untagged
   * budget). Returns the correlation flag and consumes one budget unit.
   */
  sampleResizeCorrelation(): boolean;
  /** Raw resizeDifference read for trace payloads only. */
  resizeDifferenceNow(): number;
}

export interface ScrollIntent {
  /** Install the wheel/scroll/pointer/key/touch listeners on the element. */
  attach(el: HTMLElement): void;
  /** Remove listeners and reset all transient intent state (keeps restore consent). */
  detach(): void;
  setEscapedFromLock(next: boolean): void;
  /** Defensive escape + arm the one-shot restore-snap consent, in that order. */
  armRestoreSnap(): void;
  restoreConsentArmed(): boolean;
  clearRestoreConsent(): void;
  clearRecentDownIntent(): void;
  clearScrollbarDragSession(): void;
  /** Wipe the programmatic-write token ring and the structural-append window. */
  clearProgrammaticScrollState(): void;
  /**
   * Record a controller scrollTop write: tags the resulting scroll event
   * via the token ring and updates the re-stick direction baseline.
   */
  noteProgrammaticWrite(top: number): void;
  runExternalScroll(action: () => void, opts?: { preserveIntent?: boolean }): void;
  /** Snapshot of the machine's windows/consent for the dev-hook dump. */
  debugState(): {
    recentDownIntentActive: boolean;
    recentDownIntentUntil: number;
    scrollbarDragSessionActive: boolean;
    scrollbarDragSessionVersion: number;
    restoreSnapArmed: boolean;
  };
}

export function createScrollIntent(deps: ScrollIntentDeps): ScrollIntent {
  installModuleSelectionListeners();

  let pendingProgrammaticScrollEventTokens: { top: number; expiresAt: number; remaining: number }[] = [];
  let externalScrollIgnoreUntil = 0;
  let externalScrollClearTimer: ReturnType<typeof setTimeout> | null = null;

  let recentDownIntentUntil = 0;
  let recentDownIntentVersion = 0;
  let recentDownIntentClearTimer: ReturnType<typeof setTimeout> | null = null;
  let scrollbarDragSessionActive = false;
  let scrollbarDragSessionVersion = 0;
  let scrollbarDragSessionFailsafeTimer: ReturnType<typeof setTimeout> | null = null;
  let detachScrollbarDragEnd: (() => void) | undefined;
  let touchStartY: number | null = null;
  // Baseline so `scrolledDown` can be computed for the re-stick path.
  // Re-stick requires both recent down input and a real downward scroll
  // event landing at the bottom; this baseline keeps layout clamps from
  // masquerading as user-driven down motion.
  let lastObservedScrollTopForRestick = -1;
  let detachListeners: (() => void) | undefined;

  // ===== Restore-snap consent =====
  // One-shot flag the thread-switch entry point arms (via
  // `armRestoreSnap()`, which sets the defensive escape and THEN arms)
  // immediately before the restore $effect runs.
  // `forceStick({reason: 'restore'})` consumes the flag and proceeds;
  // when the flag is unset, that call NO-OPs. Any outer-scroll escape
  // intent (wheel / key / touch / pointer that can reach the scroll element), plus
  // selection or explicit user-reason forceStick,
  // also clears the flag, so a stale restore $effect that fires after
  // a user escape cannot clobber it. This is
  // the load-bearing distinguisher between "the user has explicitly
  // escaped" and "armRestoreSnap just defensively set escape=true
  // while preparing the new thread for restore."
  let restoreSnapArmed = false;

  function clearRecentDownIntent(opts: { bumpVersion?: boolean } = {}): void {
    recentDownIntentUntil = 0;
    if (opts.bumpVersion ?? true) recentDownIntentVersion += 1;
    if (recentDownIntentClearTimer) {
      clearTimeout(recentDownIntentClearTimer);
      recentDownIntentClearTimer = null;
    }
  }

  function hasRecentDownIntent(): boolean {
    return recentDownIntentUntil > nowMs();
  }

  function clearScrollbarDragSession(opts: { invalidateCapturedScrolls?: boolean } = {}): void {
    scrollbarDragSessionActive = false;
    if (opts.invalidateCapturedScrolls ?? true) scrollbarDragSessionVersion += 1;
    detachScrollbarDragEnd?.();
    detachScrollbarDragEnd = undefined;
    if (scrollbarDragSessionFailsafeTimer) {
      clearTimeout(scrollbarDragSessionFailsafeTimer);
      scrollbarDragSessionFailsafeTimer = null;
    }
  }

  function handleScrollbarDragEnd(): void {
    clearScrollbarDragSession({ invalidateCapturedScrolls: false });
  }

  function armScrollbarDragSession(): void {
    clearScrollbarDragSession({ invalidateCapturedScrolls: false });
    scrollbarDragSessionActive = true;
    scrollbarDragSessionVersion += 1;
    document.addEventListener('pointerup', handleScrollbarDragEnd, { capture: true });
    document.addEventListener('pointercancel', handleScrollbarDragEnd, { capture: true });
    window.addEventListener('blur', handleScrollbarDragEnd, { capture: true });
    detachScrollbarDragEnd = () => {
      document.removeEventListener('pointerup', handleScrollbarDragEnd, { capture: true });
      document.removeEventListener('pointercancel', handleScrollbarDragEnd, { capture: true });
      window.removeEventListener('blur', handleScrollbarDragEnd, { capture: true });
    };
    scrollbarDragSessionFailsafeTimer = setTimeout(
      handleScrollbarDragEnd,
      SCROLLBAR_DRAG_SESSION_FAILSAFE_MS,
    );
  }

  function restickFromUserInput(source: 'wheel' | 'key' | 'touch' | 'scroll'): void {
    clearRecentDownIntent({ bumpVersion: false });
    deps.setEscaped(false);
    deps.setIsAtBottom(true);
    deps.spring.clearStopRequest();
    if (isUiRenderTraceEnabled()) trace('scroll.restick.input', () => {
      const el = deps.getScrollEl();
      return {
        source,
        scrollTop: el ? Math.round(el.scrollTop) : null,
        distFromBottom: el ? Math.round(deps.distanceFromBottom()) : null,
      };
    });
  }

  function recordRecentDownIntent(source: 'wheel' | 'key' | 'touch'): void {
    if (!deps.escaped()) return;
    clearProgrammaticScrollState();
    restoreSnapArmed = false;
    recentDownIntentUntil = nowMs() + RECENT_DOWN_INTENT_WINDOW_MS;
    recentDownIntentVersion += 1;
    if (recentDownIntentClearTimer) clearTimeout(recentDownIntentClearTimer);
    const expiresAt = recentDownIntentUntil;
    const version = recentDownIntentVersion;
    recentDownIntentClearTimer = setTimeout(() => {
      if (
        recentDownIntentVersion === version
        && recentDownIntentUntil === expiresAt
      ) clearRecentDownIntent();
    }, RECENT_DOWN_INTENT_WINDOW_MS);
    if (isUiRenderTraceEnabled()) trace('scroll.intent.down', () => {
      const el = deps.getScrollEl();
      return {
        source,
        recentDownIntentUntil: Math.round(recentDownIntentUntil),
        scrollTop: el ? Math.round(el.scrollTop) : null,
        distFromBottom: el ? Math.round(deps.distanceFromBottom()) : null,
        escapedFromLockState: deps.escaped(),
      };
    });
    if (
      deps.getScrollEl()
      && deps.escaped()
      && deps.distanceFromBottom() <= AUTO_FOLLOW_BOTTOM_EPSILON_PX
    ) {
      restickFromUserInput(source);
    }
  }

  function recordProgrammaticScrollEventToken(top: number): void {
    const now = nowMs();
    pendingProgrammaticScrollEventTokens.push({
      top,
      expiresAt: now + PROGRAMMATIC_SCROLL_EVENT_TOKEN_TTL_MS,
      remaining: PROGRAMMATIC_SCROLL_EVENT_DUPLICATE_BUDGET,
    });
    if (pendingProgrammaticScrollEventTokens.length > MAX_PROGRAMMATIC_SCROLL_EVENT_TOKENS) {
      pendingProgrammaticScrollEventTokens.splice(
        0,
        pendingProgrammaticScrollEventTokens.length - MAX_PROGRAMMATIC_SCROLL_EVENT_TOKENS,
      );
    }
  }

  function consumeProgrammaticScrollEventToken(top: number): boolean {
    if (pendingProgrammaticScrollEventTokens.length === 0) return false;
    const now = nowMs();
    let consumed = false;
    let writeIndex = 0;
    for (const token of pendingProgrammaticScrollEventTokens) {
      if (token.expiresAt < now) continue;
      if (!consumed && token.top === top) {
        consumed = true;
        token.remaining -= 1;
        if (token.remaining > 0) {
          pendingProgrammaticScrollEventTokens[writeIndex] = token;
          writeIndex += 1;
        }
        continue;
      }
      pendingProgrammaticScrollEventTokens[writeIndex] = token;
      writeIndex += 1;
    }
    pendingProgrammaticScrollEventTokens.length = writeIndex;
    return consumed;
  }

  function clearProgrammaticScrollState(): void {
    pendingProgrammaticScrollEventTokens = [];
    deps.spring.clearStructuralAppend();
  }

  function noteProgrammaticWrite(top: number): void {
    recordProgrammaticScrollEventToken(top);
    lastObservedScrollTopForRestick = top;
  }

  function setEscapedFromLock(next: boolean): void {
    if (!next && !scrollbarDragSessionActive) clearScrollbarDragSession();
    if (next) {
      clearRecentDownIntent();
      // User explicitly broke from auto-follow — bail any in-flight
      // spring chase. The tick observes the stop request + new state
      // and clears the token on the next frame, but we also cancel
      // here for the "no rAF before next attach" edge case.
      deps.spring.requestStop();
      deps.spring.cancel();
      // Clear any pending restore-snap consent: a fresh user escape
      // invalidates a yet-to-be-consumed restore-snap. armRestoreSnap
      // runs its defensive escape through here BEFORE arming, so this
      // clear is the right default — a stale consent left over from an
      // earlier path can't slip through, and a legitimate arm survives
      // because it is written after this clear.
      restoreSnapArmed = false;
    }
    if (deps.escaped() === next) return;
    const previousIsAtBottom = deps.isAtBottom();
    deps.setEscaped(next);
    if (next) {
      deps.setIsAtBottom(false);
    }
    if (isUiRenderTraceEnabled()) trace('scroll.escape.set', () => {
      const el = deps.getScrollEl();
      return {
        next,
        previousIsAtBottom,
        isAtBottomState: deps.isAtBottom(),
        pauseDepth: deps.pauseDepth(),
        isNearBottomState: deps.isNearBottom(),
        scrollTop: el ? Math.round(el.scrollTop) : null,
        scrollHeight: el ? Math.round(el.scrollHeight) : null,
        clientHeight: el ? Math.round(el.clientHeight) : null,
      };
    });
  }

  function armRestoreSnap(): void {
    // Defensive escape FIRST (it clears any prior arm), then arm. Folded
    // into one call so the two consumers (thread switch, channel initial
    // poll) cannot get the ordering wrong — see the interface doc for why
    // arm and consume must stay separate calls.
    setEscapedFromLock(true);
    restoreSnapArmed = true;
    if (isUiRenderTraceEnabled()) trace('scroll.restoreSnap.arm', () => ({
      isAtBottomState: deps.isAtBottom(),
      escapedFromLockState: deps.escaped(),
      pauseDepth: deps.pauseDepth(),
    }));
  }

  // Tags a non-controller scroll writer so the scroll handler does not
  // misread it as user intent. This does not change follow/escape state;
  // callers decide that explicitly before invoking it.
  function tagExternalProgrammaticScroll(action: () => void): void {
    externalScrollIgnoreUntil = nowMs() + EXTERNAL_SCROLL_TAG_CLEAR_MS;
    if (externalScrollClearTimer) clearTimeout(externalScrollClearTimer);
    externalScrollClearTimer = setTimeout(() => {
      externalScrollIgnoreUntil = 0;
      externalScrollClearTimer = null;
    }, EXTERNAL_SCROLL_TAG_CLEAR_MS);
    action();
    const el = deps.getScrollEl();
    if (el) {
      lastObservedScrollTopForRestick = el.scrollTop;
      deps.refreshIsNearBottom();
    }
  }

  function runExternalScroll(action: () => void, opts: { preserveIntent?: boolean } = {}): void {
    if (!opts.preserveIntent) setEscapedFromLock(true);
    tagExternalProgrammaticScroll(action);
  }

  // ===== DOM handlers =====
  function targetIsInsideScrollEl(e: Event): boolean {
    const scrollEl = deps.getScrollEl();
    if (!scrollEl) return false;
    let cur: Element | null = e.target instanceof Element ? e.target : null;
    while (cur) {
      if (cur === scrollEl) return true;
      cur = cur.parentElement;
    }
    return false;
  }

  function escapeFromUserInput(source: 'wheel' | 'key' | 'touch' | 'pointer'): void {
    const scrollEl = deps.getScrollEl();
    if (!scrollEl) return;
    clearProgrammaticScrollState();
    if (source !== 'pointer') clearScrollbarDragSession();
    if (!deps.escaped() && isUiRenderTraceEnabled()) {
      trace('scroll.intent.escape', () => ({
        source,
        scrollTop: Math.round(scrollEl.scrollTop),
        isAtBottomState: deps.isAtBottom(),
        escapedFromLockState: deps.escaped(),
      }));
    }
    setEscapedFromLock(true);
  }

  function handleWheel(e: WheelEvent): void {
    const scrollEl = deps.getScrollEl();
    if (!scrollEl) return;
    if (e.ctrlKey) return;
    if (e.deltaY === 0) return;
    if (!targetIsInsideScrollEl(e)) return;
    if (scrollEl.scrollHeight <= scrollEl.clientHeight) return;

    if (e.deltaY < 0) {
      escapeFromUserInput('wheel');
      return;
    }

    recordRecentDownIntent('wheel');
  }

  function handlePointerDown(e: PointerEvent): void {
    const scrollEl = deps.getScrollEl();
    if (!scrollEl) return;
    if (scrollEl.scrollHeight <= scrollEl.clientHeight) return;
    if (!targetIsInsideScrollEl(e)) return;

    if (e.button === 1) {
      escapeFromUserInput('pointer');
      return;
    }

    if (e.isPrimary === false) return;

    const scrollbarWidth = scrollEl.offsetWidth - scrollEl.clientWidth;
    if (scrollbarWidth <= 0) return;

    const rect = scrollEl.getBoundingClientRect();
    const style = window.getComputedStyle(scrollEl);
    const inRightGutter = e.clientX >= rect.right - scrollbarWidth;
    const inLeftGutter = style.direction === 'rtl' && e.clientX <= rect.left + scrollbarWidth;
    if (!inRightGutter && !inLeftGutter) return;

    if (isUiRenderTraceEnabled()) trace('scroll.pointer.intent', () => ({
      clientX: Math.round(e.clientX),
      scrollbarWidth: Math.round(scrollbarWidth),
      scrollTop: Math.round(scrollEl.scrollTop),
      isAtBottomState: deps.isAtBottom(),
      escapedFromLockState: deps.escaped(),
    }));
    escapeFromUserInput('pointer');
    armScrollbarDragSession();
  }

  function handleScroll(): void {
    const scrollEl = deps.getScrollEl();
    if (!scrollEl) return;
    const scrollTopAtEvent = scrollEl.scrollTop;
    // Bug A fix (Change 1): capture distance-from-bottom synchronously
    // at scroll-event time. The deferred re-stick check (below) must
    // use THIS value, not re-read distanceFromBottom() from inside the
    // 1ms timer — by the time the deferred handler runs, a concurrent
    // contentRO fire from a streaming chunk may have grown scrollHeight
    // and pushed the user past the re-stick epsilon, producing a false
    // negative (the user reached the bottom but the bottom moved away
    // in the 1ms window). This was Bug A in long Opus threads.
    // Self-tag consumption: one write records one token; the token FIFO
    // is TTL-bounded, so a genuine user scroll landing at the same
    // scrollTop value long after our write is NOT swallowed, and the
    // per-token duplicate budget absorbs browser-coalesced event
    // duplicates for the same write.
    const tokenTagged = consumeProgrammaticScrollEventToken(scrollTopAtEvent);
    const externalTagged = externalScrollIgnoreUntil > nowMs();
    const tagged = tokenTagged || externalTagged;
    // Tagged programmatic write — bail synchronously without scheduling
    // the deferral timer. Steady-state streaming fires a sync-pin write
    // on every contentRO positive delta; allocating a closure + timer
    // registration for each one just to no-op inside the callback was
    // hundreds of throwaway allocs/sec on long assistant turns. The 1 ms
    // RO-race deferral below isn't needed for tagged writes — the token
    // is recorded synchronously at the write chokepoint, so we already
    // know this event reflects our own write, not user intent. The
    // refreshIsNearBottom call and trace allocation below are also
    // skipped on the tagged path — the spring fires at 60Hz during a
    // chase, and both are wasted work when we already know this is our
    // own write.
    if (tagged) return;
    const distFromBottomAtEvent = deps.refreshIsNearBottom();
    // No tagged/externalTagged fields here: the tagged bail above means
    // this record only ever describes untagged (user-attributed) events.
    if (isUiRenderTraceEnabled()) trace('scroll.scrollEvent', () => ({
      scrollTop: Math.round(scrollTopAtEvent),
      scrollHeight: Math.round(scrollEl.scrollHeight),
      clientHeight: Math.round(scrollEl.clientHeight),
      resizeDifference: Math.round(deps.resizeDifferenceNow()),
      isAtBottomState: deps.isAtBottom(),
      escapedFromLockState: deps.escaped(),
      pauseDepth: deps.pauseDepth(),
      isNearBottomState: deps.isNearBottom(),
    }));
    const resizeCorrelatedScroll = deps.sampleResizeCorrelation();
    const previousObserved = lastObservedScrollTopForRestick;
    const downIntentVersionAtEvent = recentDownIntentVersion;
    const scrollbarDragSessionAtEvent = scrollbarDragSessionActive;
    const scrollbarDragSessionVersionAtEvent = scrollbarDragSessionVersion;
    const scrolledDown = previousObserved < 0
      ? false
      : scrollTopAtEvent > previousObserved;
    const scrolledUp = previousObserved < 0
      ? false
      : scrollTopAtEvent < previousObserved;
    lastObservedScrollTopForRestick = scrollTopAtEvent;
    const shouldRunDeferredScrollIntentCheck =
      deps.escaped()
      || mouseDown
      || scrollbarDragSessionActive;
    if (!shouldRunDeferredScrollIntentCheck) return;
    // Defer 1ms so a concurrent RO callback can update resizeDifference
    // before we interpret direction. Mirrors upstream.
    setTimeout(() => {
      const el = deps.getScrollEl();
      if (!el) return;
      const eventCameFromCurrentScrollbarDrag =
        scrollbarDragSessionAtEvent
        && scrollbarDragSessionVersionAtEvent === scrollbarDragSessionVersion;
      if (eventCameFromCurrentScrollbarDrag && scrolledUp) {
        escapeFromUserInput('pointer');
      }

      // RO race — content just resized; the scroll event reflects layout,
      // not user intent. Most importantly: virtua's $fixScrollJump can
      // adjust scrollTop to keep above-viewport rows stable, which would
      // otherwise look like user scroll movement. For non-virtua consumers
      // (Discussion's ChannelView) this gate is a 1ms suppression window
      // after each content-RO fire — vanishingly unlikely to swallow a
      // real user gesture, since the window only opens immediately after
      // a layout change.
      //
      // A recent down-intent is proof the user is actively trying to
      // re-stick, so let that bottom-landing through even during a
      // measurement cascade. Without recent down intent, layout wins.
      const hasLiveRecentDownIntent =
        hasRecentDownIntent()
        && recentDownIntentVersion === downIntentVersionAtEvent;
      const hasLiveScrollbarDragRestickIntent =
        eventCameFromCurrentScrollbarDrag
        && scrolledDown;
      const hasLiveDownIntent = hasLiveRecentDownIntent || hasLiveScrollbarDragRestickIntent;
      if (resizeCorrelatedScroll && !hasLiveDownIntent) {
        if (isUiRenderTraceEnabled()) trace('scroll.scrollEvent.deferred.bailRO', () => ({
          resizeDifference: Math.round(deps.resizeDifferenceNow()),
          resizeCorrelatedScroll,
          hasLiveDownIntent,
          scrollTop: Math.round(el.scrollTop),
        }));
        return;
      }

      if (isSelectingInside(el)) {
        if (isUiRenderTraceEnabled()) trace('scroll.scrollEvent.deferred.escapeSelection', () => ({
          scrollTop: Math.round(el.scrollTop),
        }));
        setEscapedFromLock(true);
        return;
      }

      const willRestick = hasLiveDownIntent
        && scrolledDown
        && deps.escaped()
        && distFromBottomAtEvent <= AUTO_FOLLOW_BOTTOM_EPSILON_PX;
      if (isUiRenderTraceEnabled()) trace('scroll.scrollEvent.deferred', () => ({
        scrollTop: Math.round(scrollTopAtEvent),
        previousObserved: Math.round(previousObserved),
        scrolledDown,
        hasLiveRecentDownIntent,
        hasLiveScrollbarDragRestickIntent,
        hasLiveDownIntent,
        distFromBottomAtEvent: Math.round(distFromBottomAtEvent),
        distFromBottomNow: Math.round(deps.distanceFromBottom()),
        willRestick,
        isAtBottomState: deps.isAtBottom(),
        escapedFromLockState: deps.escaped(),
      }));
      if (willRestick) {
        restickFromUserInput('scroll');
      }
    }, RESIZE_CLEAR_PADDING_MS);
  }

  // Keydown / touch handlers (intent signals).
  function handleKeydown(e: KeyboardEvent): void {
    if (UP_KEYS.has(e.key)) escapeFromUserInput('key');
    if (DOWN_KEYS.has(e.key)) recordRecentDownIntent('key');
  }
  function handleTouchStart(e: TouchEvent): void {
    touchStartY = e.touches[0]?.clientY ?? null;
  }
  function handleTouchMove(e: TouchEvent): void {
    if (touchStartY === null) return;
    const y = e.touches[0]?.clientY ?? touchStartY;
    const dy = y - touchStartY;
    touchStartY = y;
    // Finger moves DOWN visually → page scrolls UP (scrollTop decreases)
    // → user wants to see content above → escape (UP intent). Finger
    // moves UP while already escaped is the matching "scroll back toward
    // bottom" input signal (DOWN intent).
    if (dy > 1) escapeFromUserInput('touch');
    if (dy < -1) recordRecentDownIntent('touch');
  }
  function handleTouchEnd(): void {
    touchStartY = null;
  }

  function attach(el: HTMLElement): void {
    el.addEventListener('wheel', handleWheel, { passive: true });
    el.addEventListener('scroll', handleScroll, { passive: true });
    el.addEventListener('pointerdown', handlePointerDown, { passive: true });
    el.addEventListener('keydown', handleKeydown);
    el.addEventListener('touchstart', handleTouchStart, { passive: true });
    el.addEventListener('touchmove', handleTouchMove, { passive: true });
    el.addEventListener('touchend', handleTouchEnd, { passive: true });
    el.addEventListener('touchcancel', handleTouchEnd, { passive: true });
    detachListeners = () => {
      el.removeEventListener('wheel', handleWheel);
      el.removeEventListener('scroll', handleScroll);
      el.removeEventListener('pointerdown', handlePointerDown);
      el.removeEventListener('keydown', handleKeydown);
      el.removeEventListener('touchstart', handleTouchStart);
      el.removeEventListener('touchmove', handleTouchMove);
      el.removeEventListener('touchend', handleTouchEnd);
      el.removeEventListener('touchcancel', handleTouchEnd);
    };
    lastObservedScrollTopForRestick = el.scrollTop;
  }

  function detach(): void {
    detachListeners?.();
    detachListeners = undefined;
    if (externalScrollClearTimer) {
      clearTimeout(externalScrollClearTimer);
      externalScrollClearTimer = null;
    }
    externalScrollIgnoreUntil = 0;
    clearProgrammaticScrollState();
    clearRecentDownIntent();
    clearScrollbarDragSession();
    touchStartY = null;
    lastObservedScrollTopForRestick = -1;
    // DELIBERATELY leave `restoreSnapArmed` untouched. attach() calls
    // detach() up-front when scrollEl / contentEl change, and on first
    // mount that wipe ran BETWEEN the consumer's $effect.pre arm and
    // the restore $effect's forceStick({reason:'restore'}), making
    // the consent effectively unusable for the initial-mount path.
    // The flag is invalidated by outer-scroll escape intent (wheel / key /
    // touch / pointer that can reach the scroll element), selection,
    // explicit user-reason forceStick, and the
    // consume-on-restore path itself — that's
    // enough to keep it from leaking stale consent across legitimate
    // lifecycles. True teardown also discards the entire controller
    // instance, so a residual `true` here has no observable effect.
  }

  return {
    attach,
    detach,
    setEscapedFromLock,
    armRestoreSnap,
    restoreConsentArmed: () => restoreSnapArmed,
    clearRestoreConsent: () => {
      restoreSnapArmed = false;
    },
    clearRecentDownIntent: () => clearRecentDownIntent(),
    clearScrollbarDragSession: () => clearScrollbarDragSession(),
    clearProgrammaticScrollState,
    noteProgrammaticWrite,
    runExternalScroll,
    debugState: () => ({
      recentDownIntentActive: hasRecentDownIntent(),
      recentDownIntentUntil: Math.round(recentDownIntentUntil),
      scrollbarDragSessionActive,
      scrollbarDragSessionVersion,
      restoreSnapArmed,
    }),
  };
}
