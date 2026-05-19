// Sticky-bottom controller, shared by chat MessageTimeline and
// Discussion ChannelView.
//
// Port of stackblitz-labs/use-stick-to-bottom adapted to Svelte 5. Owns
// the user's intent ("glued to bottom" or "free") and a single
// ResizeObserver on the content element. Two animation behaviors for
// autonomous content growth, selected per-fire by the consumer via the
// `animationMode` option:
//
//   - 'instant' (default): sync-pin. The same paint frame where
//     contentEl grows also lands scrollTop at the new target, so the
//     user sees content arriving at the bottom with no perceptible
//     scroll motion. Used by Discussion's ChannelView and by chat
//     whenever a turn is NOT actively streaming — late Streamdown
//     typesetting on settled content, virtua row remeasurement on a
//     freshly-mounted thread, etc.
//
//   - 'spring': velocity-spring chase. The viewport interpolates toward
//     the moving bottom across rAF ticks so the user sees a smooth
//     scroll-follow. Used by chat MessageTimeline while a turn is
//     running (`getActiveTurn(threadId) != null`) so streaming chunks
//     flow in with a smooth animation. Gated by a quiescence-based warm
//     state: spring stays off until contentRO has been quiet for
//     QUIET_MS or the FAILSAFE_MS deadline trips, whichever comes
//     first. The warm gate defends against the original 80LoC-spring-
//     delete regression (commit e00723f) where mount-time virtua
//     remeasurement and async Streamdown typesetting would spring-
//     chase a thread restore visibly.
//
// User-initiated snaps (the scroll-to-bottom chip) and thread restores
// go through `forceStick()` which writes scrollTop directly.
// Restore snaps also reset the warm gate so post-thread-switch
// measurement settling stays silent; user snaps keep already-settled
// content visible.
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

import { tick } from 'svelte';
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

// Three-band geometry — see frontend/AGENTS.md "Three-band geometry"
// for the full rationale. Tightening any one of these affects a
// different UX surface; the asymmetry is load-bearing.
//
// Visual near-bottom band: drives the scroll-to-bottom chip and the
// negative-delta repin's geometric branch. Loose so a user within 70px
// doesn't see the chip flicker.
const STICK_TO_BOTTOM_OFFSET_PX = 70;
// Auto-follow re-stick band: a DOWN-direction scroll that lands within
// this many pixels of the bottom flips the user back to sticky. Matches
// react-virtuoso's `atBottomThreshold` default — tolerates virtua row-
// height estimation + browser scrollTop rounding that routinely lands
// 1-3px short during streaming.
const AUTO_FOLLOW_BOTTOM_EPSILON_PX = 4;
const RESIZE_CLEAR_PADDING_MS = 1;
const DEFAULT_PROGRAMMATIC_SCROLL_DURATION_MS = 420;
const PROGRAMMATIC_SCROLL_DISTANCE_THRESHOLD_PX = 1;
const USER_SCROLL_INTENT_WINDOW_MS = 160;
// Escape threshold: any non-zero upward movement during a gesture
// window escapes. Strict `>` against 0 so zero-movement scrolls (sub-
// pixel wheels rounded by the browser) don't spuriously escape.
const USER_SCROLL_ESCAPE_THRESHOLD_PX = 0;
const EXTERNAL_SCROLL_TAG_CLEAR_MS = 100;

// ===== Spring chase tuning =====
// Defaults match upstream stackblitz-labs/use-stick-to-bottom:
// damping 0.7, stiffness 0.05, mass 1.25. These produce a smooth
// scroll-follow that feels responsive without overshooting visibly.
const DEFAULT_SPRING = { damping: 0.7, stiffness: 0.05, mass: 1.25 } as const;
const SIXTY_FPS_INTERVAL_MS = 1000 / 60;
// Keep chasing for this long after the last positive grow event. Without
// this, the spring would consider itself "arrived" between streaming
// chunks and stop, then have to spin up again on the next chunk —
// visibly jittery at chunk boundaries.
const RETAIN_ANIMATION_DURATION_MS = 350;
// Spring arrival thresholds: distance ≤1px from target AND velocity
// below 0.5 px-per-60fps-frame means we've effectively settled.
const ARRIVAL_DISTANCE_PX = 1;
const ARRIVAL_VELOCITY_THRESHOLD = 0.5;

// ===== Warm-up (quiescence) gate =====
// After attach() or forceStick(), the controller stays in sync-pin mode
// until contentRO has been quiet for QUIET_MS or the FAILSAFE_MS
// deadline trips. This defends against the mount-time spring-chase
// regression: virtua's per-row ResizeObservers and svelte-streamdown's
// async typesetting (shiki/KaTeX/mermaid) fire many positive deltas
// while content is settling, and we don't want those to look like
// "real" content arrival.
//
// QUIET_MS is shorter than a typical streaming chunk interval so we
// adapt to "fast settle, then real streaming starts" — chunks arrive
// after the quiet window closes, and spring engages.
//
// FAILSAFE_MS bounds the worst case: re-entering a thread that's
// already mid-stream produces continuous contentRO fires, so the
// quiet window never closes. Without the failsafe we'd be stuck in
// sync-pin for the rest of the turn. 2500ms covers slow machines
// where shiki/mermaid worker startup + virtua first-paint can
// genuinely take >1s, while still letting the spring engage for the
// bulk of a typical multi-second response.
const QUIET_MS = 100;
const FAILSAFE_MS = 2500;

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
  /** True when the user has explicitly moved the outer scroller away from bottom. */
  readonly escapedFromLock: boolean;
  /**
   * True once the warm-up gate has cleared — either QUIET_MS of
   * contentRO silence, or FAILSAFE_MS elapsed (whichever first). Use
   * as a "virtua's measurement cascade has settled" signal: consumers
   * can hide content during the cascade and reveal here to avoid
   * showing the user a brief estimated-size paint before the
   * measured-size correction lands. Reset to false on attach,
   * restore-reason forceStick, and explicit armWarmup() — the latter
   * is the seam for "I'm about to render fundamentally different
   * content (e.g. thread switch) and the DOM update will happen BEFORE
   * my next forceStick / attach call, so reset the gate now."
   */
  readonly isWarm: boolean;

  /** Depth-counted lease that suspends auto-scroll until released. */
  pauseAutoScroll(): () => void;
  /**
   * Notify the controller that the geometry around the content might
   * have changed without contentEl resizing — composer height growth/
   * shrink is the canonical case. Re-pins to the new target if sticky.
   */
  notifyContentMaybeGrew(): void;
  /**
   * Notify the controller that live transcript content may have advanced
   * without the content ResizeObserver producing a usable positive delta.
   * Uses the same escape/pause/user-intent gates as notifyContentMaybeGrew,
   * but honors animationMode: active chat turns spring-chase instead of
   * sync-pinning.
   */
  notifyLiveContentMaybeGrew(): void;
  /**
   * Run an explicit user disclosure action while preserving the user's
   * current follow intent. Sticky users stay pinned to bottom; escaped
   * users keep the clicked anchor at the same viewport position.
   */
  preserveScrollAnchor(anchor: HTMLElement, action: () => void | Promise<void>): Promise<void>;

  attach(scrollEl: HTMLElement, contentEl: HTMLElement): void;
  detach(): void;

  /**
   * Snap to the bottom and resume auto-follow. Two reasons:
   *
   * - `'user'` (default): an explicit bottom-follow action such as the
   *   scroll-to-bottom chip. Always clears `escapedFromLock` and lands
   *   scrollTop at the target.
   * - `'restore'`: a thread-restore-style snap. Honored ONLY if
   *   `armRestoreSnap()` was called since the last user-initiated
   *   escape; otherwise NO-OP. This prevents a stale or duplicated
   *   restore $effect from clobbering an existing user escape with a
   *   snap-to-bottom they didn't ask for (the seq-509 trace bug).
   *
   * Callers writing "user clicked to go to bottom" should pass `'user'`
   * (or nothing). Callers writing "I just landed on this thread and
   * the saved snapshot says bottom" should pass `'restore'` AND have
   * paired their call with an `armRestoreSnap()` from the same
   * thread-switch entry point.
   */
  forceStick(opts?: { reason?: 'user' | 'restore' }): void;
  /**
   * Arm a one-shot consent for the next `forceStick({reason:'restore'})`
   * call. Set by the thread-switch entry point (MessageTimeline's
   * `$effect.pre`, ChannelView's initial-poll path) immediately
   * before the restore $effect runs. Auto-clears on consume by the
   * next restore-forceStick call, on outer-scroll escape intent
   * (wheel / key / touch that can reach the chat scroller),
   * animateScrollTo, stopScroll, and on any
   * user-reason `forceStick()`. This is
   * the load-bearing distinguisher between "the user is explicitly
   * escaped" and "I just defensively set escape=true while preparing
   * the new thread for restore."
   */
  armRestoreSnap(): void;
  /**
   * Flip intent flags to sticky-bottom WITHOUT writing scrollTop.
   * Use only when the caller has already established bottom geometry
   * or when the timeline is empty and there is no geometry to write.
   */
  markAtBottom(): void;
  /**
   * Controlled non-native scroll animation for arbitrary timeline jumps.
   * This owns the scrollTop writes so programmatic scroll tagging stays
   * in one place. Used by handleLoadOlder / scrollToItem.
   */
  animateScrollTo(targetTop: number, opts?: { durationMs?: number }): Promise<'completed' | 'cancelled'>;
  /**
   * Run an external programmatic scroll, such as virtua's
   * `listRef.scrollToIndex(...)`, under the controller's scroll-intent
   * tag. This is the escape hatch for scroll writers the controller
   * cannot perform itself.
   *
   * Most external jumps are explicit navigation and should escape bottom
   * follow. Host-layout reconciliation is different: it rewrites the
   * virtualizer's current offset after a pane move without changing user
   * intent, so it passes `preserveIntent`.
   */
  runExternalScroll(action: () => void, opts?: { preserveIntent?: boolean }): void;
  /**
   * Cancel any active controller-owned animation and mark the user as
   * escaped. Kept for callers that need to stop motion without performing
   * a scroll write.
   */
  stopScroll(): void;
  /**
   * Set the escape flag. Public so `handleLoadOlder` / `scrollToItem`
   * can opt out of auto-restick on programmatic jumps.
   *
   * Calling with `next=true` also (a) cancels any in-flight spring
   * chase, (b) cancels any in-flight `animateScrollTo`, and (c) clears
   * any pending `armRestoreSnap()` consent — a fresh escape
   * invalidates a yet-to-be-consumed restore-snap. The thread-switch
   * entry point that legitimately wants the restore-snap calls
   * `armRestoreSnap()` AFTER its defensive `setEscapedFromLock(true)`
   * so the clear-then-set order still leaves the arm valid when the
   * restore `$effect` runs.
   *
   * Calling with `next=false` flips intent only — it does not consume
   * the restore-snap consent (that's `forceStick({reason:'restore'})`
   * or `markAtBottom()`'s job).
   */
  setEscapedFromLock(next: boolean): void;
  /**
   * Re-arm the warm-up gate WITHOUT writing scrollTop or changing
   * intent / escape flags. Sets `isWarm` to false and restarts the
   * QUIET_MS / FAILSAFE_MS timers, exactly as `attach()` and
   * restore-reason `forceStick()` do.
   *
   * The use case is a consumer that needs `isWarm` to be false BEFORE
   * the next DOM flush, where calling `forceStick()` would be wrong
   * because it has unwanted side effects (writes scrollTop, clears
   * escape). Chat's MessageTimeline calls this from `$effect.pre` on
   * thread switch: contentEl and scrollEl don't change across switches
   * (so `attach()` early-returns), and the restore-effect `forceStick()`
   * runs in `$effect` which fires AFTER the DOM update — meaning the
   * first paint of the new thread would otherwise inherit the previous
   * thread's settled `isWarm=true`, defeating the cascade-hide gate.
   */
  armWarmup(): void;
}

export interface UseStickToBottomOptions {
  /**
   * Picks animation behavior for autonomous content growth (contentRO
   * positive deltas). Called per-fire — return 'spring' to chase the
   * new bottom with a velocity spring, 'instant' to sync-pin. Defaults
   * to () => 'instant' (sync-pin everywhere) so existing callers behave
   * identically to the pre-spring-restoration controller.
   *
   * Chat's MessageTimeline wires this to
   * `() => getActiveTurn(pane.threadId) != null ? 'spring' : 'instant'`
   * so streaming chunks animate; idle threads and Discussion's polled
   * channel surface stay on sync-pin.
   */
  animationMode?: () => 'spring' | 'instant';
}

export function createUseStickToBottomController(
  options: UseStickToBottomOptions = {},
): UseStickToBottomController {
  installModuleSelectionListeners();

  // ===== Reactive state (consumed by templates / $derived) =====
  // Intent flag: "we want to be glued to the bottom". Mirrors upstream's
  // state.isAtBottom — set true on initial mount, on forceStick, and when
  // a re-stick condition fires from the scroll handler. Set false on
  // explicit escape (outer wheel/key/touch scroll, select) and on stopScroll. Crucially
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
  let detachPointer: (() => void) | undefined;
  let detachKeyTouch: (() => void) | undefined;

  let targetAnimationFrame: number | null = null;
  let targetAnimationResolve: ((result: 'completed' | 'cancelled') => void) | null = null;
  let restoreTargetScrollBehavior: (() => void) | null = null;
  let resizeDifference = 0;
  let resizeClearTimer: ReturnType<typeof setTimeout> | null = null;
  let ignoreScrollToTop = -1;
  let externalScrollIgnoreUntil = 0;
  let externalScrollClearTimer: ReturnType<typeof setTimeout> | null = null;
  // Per-gesture state read by the direction-aware pin gates and the
  // gesture-expire path. `direction` is set from the input signal
  // (wheel/key/touch from sign; pointer starts 'unknown' and refines
  // on the next scroll event) and gates contentRO / notifyContent /
  // spring pins so only UP intent blocks. `lastObservedScrollTop`
  // anchors the direction-refinement for 'unknown'.
  interface GestureSession {
    source: 'wheel' | 'key' | 'touch' | 'pointer';
    startScrollTop: number;
    lastObservedScrollTop: number;
    direction: 'up' | 'down' | 'unknown';
  }
  let gestureSession: GestureSession | null = null;
  let gestureSessionTimer: ReturnType<typeof setTimeout> | null = null;
  let previousHeight: number | undefined;
  let touchStartY: number | null = null;
  let resizeCorrelatedUntaggedScrollBudget = 0;

  // ===== Spring chase state =====
  let velocity = 0;
  let accumulated = 0;
  let lastTickAt: number | null = null;
  // Monotonic counter (cheaper than `Symbol('spring')` per start). 0 means
  // no spring in flight; positive values identify the current spring run.
  let springToken = 0;
  let springGen = 0;
  let springFrameHandle: number | null = null;
  let lastGrewAt = 0;
  let springStopRequested = false;

  // ===== Restore-snap consent state =====
  // One-shot flag the thread-switch entry point arms immediately before
  // the restore $effect runs (after the defensive `setEscapedFromLock`).
  // `forceStick({reason: 'restore'})` consumes the flag and proceeds;
  // when the flag is unset, that call NO-OPs. Any outer-scroll escape
  // intent (wheel / key / touch that can reach the scroll element), plus
  // selection, animateScrollTo, stopScroll, or explicit user-reason forceStick,
  // also clears the flag, so a stale restore $effect that fires after
  // a user escape cannot clobber it. This is
  // the load-bearing distinguisher between "the user has explicitly
  // escaped" and "the $effect.pre just defensively set escape=true
  // while preparing the new thread for restore."
  let restoreSnapArmed = false;

  // ===== Warm-up (quiescence) state =====
  // `warm` flips true once the controller observes a quiet period of
  // QUIET_MS on contentRO, OR the FAILSAFE_MS deadline trips (whichever
  // comes first). Reset to false on attach, explicit armWarmup(), and
  // restore-reason forceStick. Backed by $state so consumers can
  // subscribe to the transition — chat's MessageTimeline hides contentEl
  // while warm is false, which is the canonical
  // "virtua's measurement cascade has settled" signal. Without this,
  // an uncached thread's first paint renders rows at virtua's
  // ESTIMATED_ROW_SIZE × N offsets; the RO-correction pass then shifts
  // every row by a fraction-of-a-page (the larger the thread, the
  // bigger the shift) producing the visible "lands wrong, then jumps"
  // regression.
  let warm = $state(false);
  let quietTimer: ReturnType<typeof setTimeout> | null = null;
  let failsafeTimer: ReturnType<typeof setTimeout> | null = null;

  function clearWarmupTimers(): void {
    if (quietTimer) {
      clearTimeout(quietTimer);
      quietTimer = null;
    }
    if (failsafeTimer) {
      clearTimeout(failsafeTimer);
      failsafeTimer = null;
    }
  }

  function markWarm(reason: 'quiet' | 'failsafe'): void {
    if (warm) return;
    warm = true;
    clearWarmupTimers();
    trace(`scroll.warmup.${reason}`, () => ({
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
    }));
  }

  function beginWarmup(): void {
    clearWarmupTimers();
    warm = false;
    // ONLY arm the failsafe. The quiet timer is armed by `bumpQuietTimer`
    // on the FIRST contentRO event — gating the "quiet" signal on actual
    // RO evidence is what defends against the load-bearing case where
    // contentEl is absent when the gate is armed: a thread switch where
    // the slice fetch is in flight, MessageTimeline is rendering the
    // loading-spinner / empty branch, and there is no contentEl for the
    // RO to observe. If we armed quietTimer here, it would fire at
    // QUIET_MS without any cascade evidence; once items finally arrived
    // and contentEl mounted, the consumer's hide-gate (gated on isWarm)
    // would be already open and the cascade would be visible. The
    // failsafe still bounds the worst case (slow shiki / mermaid / KaTeX
    // typesetting that keeps ROs continuously firing for > FAILSAFE_MS).
    failsafeTimer = setTimeout(() => markWarm('failsafe'), FAILSAFE_MS);
  }

  function bumpQuietTimer(): void {
    if (warm) return;
    if (quietTimer) clearTimeout(quietTimer);
    quietTimer = setTimeout(() => markWarm('quiet'), QUIET_MS);
  }

  // ===== Geometry =====
  function targetScrollTop(): number {
    if (!scrollEl) return 0;
    // Land at the actual bottom (scrollHeight - clientHeight). Upstream
    // use-stick-to-bottom subtracts an extra -1 px to avoid sub-pixel
    // rounding flipping their geometric isAtBottom check, but this
    // controller's public isAtBottom uses a 70 px visual band while
    // auto-follow re-stick uses a sub-pixel epsilon. Neither needs the
    // target itself to sit short of the actual browser bottom.
    // Subtracting -1 here just left the user 1 px above the actual
    // bottom; the scrollbar showed a one-tick gap and the snap felt
    // incomplete.
    return Math.max(0, scrollEl.scrollHeight - scrollEl.clientHeight);
  }
  function distanceFromBottom(): number {
    if (!scrollEl) return 0;
    return scrollEl.scrollHeight - scrollEl.scrollTop - scrollEl.clientHeight;
  }
  function movedUpEnoughToEscape(startScrollTop: number, nextScrollTop: number): boolean {
    // Strict `>` against threshold 0: any actual upward movement escapes;
    // a zero-movement scroll event (sub-pixel wheel rounded to 0 by the
    // browser) does not, so the gesture resolves via the 160ms timer /
    // scrollend instead of escaping spuriously.
    return startScrollTop - nextScrollTop > USER_SCROLL_ESCAPE_THRESHOLD_PX;
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
    // Tagged programmatic scroll events intentionally return before the
    // user-direction path, so keep the active gesture's direction
    // baseline current here. Otherwise a later plain scrollbar/native
    // scroll could compare against the previous bottom and fail to
    // register a small escape.
    if (gestureSession) gestureSession.lastObservedScrollTop = scrollEl.scrollTop;
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
    // Hot path: spring follow can call this every frame. The app contract is
    // that controller-owned scrollers do not get CSS-authored smooth scroll;
    // only inline values need temporary suppression here.
    const original = scrollEl.style.scrollBehavior;
    if (original && original !== 'auto') scrollEl.style.scrollBehavior = 'auto';
    writeProgrammaticScrollTop(value);
    if (original && original !== 'auto') scrollEl.style.scrollBehavior = original;
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

  // Cached MediaQueryList — `matchMedia('(prefers-reduced-motion: reduce)')`
  // is called inside both the contentRO positive-delta branch and the
  // spring tick (which runs at 60Hz). Parsing the query and constructing
  // a fresh `MediaQueryList` per call is wasted work; query the cached
  // list's `matches` instead. Cached lazily so SSR / non-window contexts
  // don't blow up.
  let reducedMotionQuery: MediaQueryList | null | undefined;
  function prefersReducedMotion(): boolean {
    if (reducedMotionQuery === undefined) {
      reducedMotionQuery = typeof window !== 'undefined'
        && typeof window.matchMedia === 'function'
        ? window.matchMedia('(prefers-reduced-motion: reduce)')
        : null;
    }
    return reducedMotionQuery?.matches ?? false;
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

  // ===== Spring chase =====
  function cancelSpring(): void {
    if (springFrameHandle !== null) {
      cancelFrame(springFrameHandle);
      springFrameHandle = null;
    }
    springToken = 0;
    velocity = 0;
    accumulated = 0;
    lastTickAt = null;
    // Reset the grow timestamp so a stale value can't trick a fresh
    // chase into thinking it's "stillChasing" right out of the gate
    // (matches the historical 80LoC-spring cleanup semantics).
    lastGrewAt = 0;
  }

  // Shared gate predicate. Used by both `startSpringIfNeeded` and the
  // contentRO positive-delta branch so the two sites can't drift on
  // which conditions allow the spring. The `warm` check is intentionally
  // omitted here — startSpringIfNeeded is called from inside the
  // already-warm branch of contentRO; warm-checking inside it would
  // double-gate and confuse the read.
  //
  // Direction-aware pending-intent gate (Change 3): only an UP gesture
  // blocks the spring. 'down' and 'unknown' intents (pointer-tap on the
  // scrollbar before drag, sub-pixel wheel rounded to 0, a wheel-down
  // toward the bottom) MUST allow the spring to keep following so the
  // user doesn't experience the 160ms blackout described in Bug C.
  function springGateOpen(): boolean {
    return !springStopRequested
      && gestureSession?.direction !== 'up'
      && pauseDepth === 0
      && isAtBottomState
      && !escapedFromLockState
      && !prefersReducedMotion()
      && options.animationMode?.() === 'spring';
  }

  function startSpringIfNeeded(): void {
    if (springToken !== 0) return;
    if (!springGateOpen()) return;
    const myToken = ++springGen;
    springToken = myToken;
    lastTickAt = null;

    const tick = (now: number): void => {
      springFrameHandle = null;
      if (springToken !== myToken || !scrollEl) return;
      // Bail conditions: lease acquired, escape set, or stop requested.
      // All three are handled by `cancelSpring()` cleanup at exit.
      if (springStopRequested || pauseDepth > 0 || !isAtBottomState || escapedFromLockState) {
        cancelSpring();
        return;
      }
      if (isSelectingInside(scrollEl)) {
        // Selection drag should never fight the user — re-rAF without
        // advancing scrollTop so the spring effectively pauses.
        springFrameHandle = requestFrame(tick);
        return;
      }

      const dt = lastTickAt === null ? 1 : (now - lastTickAt) / SIXTY_FPS_INTERVAL_MS;
      lastTickAt = now;

      // Cache per-tick. `targetScrollTop()` reads `scrollHeight` /
      // `clientHeight` — both force layout. Compute once per frame.
      const target = targetScrollTop();
      const current = scrollEl.scrollTop;
      const diff = target - current;

      if (current < target) {
        velocity = (DEFAULT_SPRING.damping * velocity + DEFAULT_SPRING.stiffness * diff) / DEFAULT_SPRING.mass;
        accumulated += velocity * dt;
        const next = current + accumulated;
        // Pre-clamp in JS so we know the post-state without a second
        // layout read just to check whether the browser clamped.
        const clamped = next > target ? target : next;
        writeCaller = next > target ? 'spring.overshoot' : 'spring.tick';
        writeScrollTop(clamped);
        if (scrollEl.scrollTop !== current) accumulated = 0;
      }

      // Arrival check uses the cached `target` for the position
      // comparison; the time delta uses rAF's `now` (matches
      // `nowMs()` in test environments because `performance.now` is
      // mocked to read the same source rAF passes the callback).
      // Mode flip mid-flight (turn ended) makes `stillChasing` false,
      // so the spring lands on its next arrival check rather than
      // chasing for another RETAIN_ANIMATION_DURATION_MS.
      const wantsSpringNow = options.animationMode?.() === 'spring';
      const stillChasing = wantsSpringNow && now - lastGrewAt < RETAIN_ANIMATION_DURATION_MS;
      const arrived =
        Math.abs(scrollEl.scrollTop - target) < ARRIVAL_DISTANCE_PX
        && Math.abs(velocity) < ARRIVAL_VELOCITY_THRESHOLD;
      if (arrived && !stillChasing) {
        // Snap to the exact target on arrival so the final paint lands
        // pixel-perfect rather than 0.5px above the bottom.
        writeCaller = 'spring.arrive';
        writeScrollTop(target);
        cancelSpring();
        return;
      }
      springFrameHandle = requestFrame(tick);
    };
    springFrameHandle = requestFrame(tick);
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

      // Every RO activity counts as "still settling" — reset the quiet
      // timer regardless of delta direction. virtua's per-row
      // remeasurement, Streamdown's typesetting backfill, and
      // parseIncompleteMarkdown rebalance all fire multiple RO callbacks
      // in close succession during mount; we want warm to fire only
      // once they're done.
      bumpQuietTimer();

      if (prev === undefined) {
        // First fire: snap to bottom synchronously so the initial paint
        // lands at the right place. Matches upstream's `initial` behavior
        // when isAtBottom starts true.
        const willPin = isAtBottomState && !escapedFromLockState && gestureSession === null;
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
      // Virtua can emit one untagged scroll jump as part of the same
      // measurement correction that produced this content resize. The
      // timer/rAF `resizeDifference` guard catches the normal ordering;
      // this one-event budget covers environments where the clear races
      // ahead of the scroll handler. Pending user intent still wins.
      resizeCorrelatedUntaggedScrollBudget = 1;

      // Refresh the geometric near-bottom flag BEFORE computing any
      // pin predicate so the trace and the gate both see the same
      // post-resize geometry. Without the lift, the negative branch
      // had to mirror the geometric check via an IIFE to avoid the
      // refresh side effect; with the lift, the trace and gate read
      // the same `isNearBottomState` and the IIFE disappears.
      refreshIsNearBottom();
      const overshoot = scrollEl.scrollTop > targetScrollTop();
      // Direction-aware pending-intent gate (Change 3): block ONLY an
      // active UP gesture. 'down' / 'unknown' intents still permit the
      // pin — that's Bug C's fix (sticky users tap the scrollbar or
      // make a sub-pixel trackpad nudge during streaming and the
      // bottom drifts away for the 160ms timer window). The strict
      // restore gate in forceStick({reason:'restore'}) stays unchanged.
      const positiveWillPin = delta > 0
        && isAtBottomState
        && !escapedFromLockState
        && gestureSession?.direction !== 'up'
        && pauseDepth === 0;
      const negativeWillPin = delta < 0
        && (isAtBottomState || isNearBottomState)
        && !escapedFromLockState
        && gestureSession === null
        && pauseDepth === 0;
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
      // pushed us past the target, snap back. Gated on the same
      // escape / pause flags as the positive / negative pin branches
      // below — when the user has escaped, the browser's own clamp
      // will fix any out-of-range scrollTop on the next paint, and we
      // must not yank them back to the bottom under any condition.
      // Without this gate, an `applyJump` shift past the new bottom
      // while the user was reading mid-history (esp. with shrinking
      // content above the viewport) could snap them to bottom.
      if (
        overshoot
        && !escapedFromLockState
        && gestureSession === null
        && pauseDepth === 0
      ) {
        writeCaller = 'contentRO.overshoot';
        writeScrollTop(targetScrollTop());
      }

      if (delta > 0) {
        // Positive delta: choose between sync-pin (default) and spring
        // chase (when the consumer signals "real content streaming and
        // the controller has warmed past mount settle").
        //
        // Sync-pin path: writes scrollTop in the same paint frame as
        // contentEl growth — no perceptible scroll motion, just content
        // arriving at the bottom. Used when animationMode is 'instant',
        // when we haven't warmed yet (mount-time virtua remeasurement +
        // Streamdown typesetting still settling), or when the user has
        // requested reduced motion.
        //
        // Spring path: starts a velocity-spring chase that interpolates
        // toward the moving bottom across rAF frames. The user sees the
        // viewport smoothly follow streaming content. Each subsequent
        // positive delta during the chase bumps `lastGrewAt` so the
        // spring keeps chasing across chunk boundaries instead of
        // arriving-then-restarting (visibly jittery).
        if (positiveWillPin) {
          if (warm && springGateOpen()) {
            lastGrewAt = nowMs();
            startSpringIfNeeded();
          } else {
            writeCaller = 'contentRO.positiveDelta';
            writeScrollTop(targetScrollTop());
          }
        }
      } else if (delta < 0) {
        // Negative delta: re-stick when the controller's intent is
        // "stay at bottom" — EITHER the logical flag (isAtBottomState)
        // OR the geometric near-bottom band (isNearBottomState) says
        // so. The geometric branch matches upstream's negative-resize
        // re-stick. The intent branch defends against virtua's jump
        // correction during the layout-measurement cascade: when
        // rows above the viewport remeasure, virtua may shift
        // scrollTop hundreds of pixels off the bottom and flip
        // isNearBottomState=false purely as a downstream effect of
        // layout — not user intent. Without the isAtBottomState
        // disjunct, the controller abandoned the pin in that case
        // and left the viewport stuck mid-cascade until the next
        // shrink happened to land scrollTop at the new bottom by
        // coincidence. User-visible as a "half-screen jump to
        // bottom" on heavy uncached threads — see frontend/AGENTS.md
        // "Negative-delta re-pin honors logical intent, not just
        // geometry" for the cascade pattern this defends.
        if (negativeWillPin) {
          isAtBottomState = true;
          // Spring carve-out: suppress this sync write while a spring
          // is chasing (springToken !== 0) so virtua's +ESTIMATE /
          // -CORRECTION pair on row-append (e.g. +90 then -56 within
          // ~5ms) doesn't race the spring. Without it, the negative
          // write lands scrollTop at the corrected target before the
          // spring's first paint and the spring ticks against
          // current==target with no perceptible motion. The spring
          // reads targetScrollTop() each tick and absorbs the
          // corrected target naturally. Note: the overshoot guard at
          // lines 698-700 above is also synchronous and CAN fire
          // mid-spring when the spring has chased past the new
          // (lower) target — by design (the existing "negative delta
          // mid-spring lets the spring converge" test relies on it
          // to clamp `scrollTop > target` once the spring would
          // otherwise stop). The carve-out only addresses the case
          // where overshoot=false (the virtua estimate→measured
          // pair, since the +90 spring barely moves before -56
          // arrives). Bug A defense (sync-pin running during the
          // !warm cascade) is preserved by warm-gate ordering: the
          // cascade fires while `!warm`, springGateOpen requires
          // `warm`, so springToken stays 0 during the cascade and
          // the sync-pin runs as before. See frontend/AGENTS.md
          // "Negative-delta re-pin honors logical intent".
          if (springToken === 0) {
            writeCaller = 'contentRO.negativeDelta';
            writeScrollTop(targetScrollTop());
          }
        }
      }

      refreshIsNearBottom();

      // Schedule resizeDifference clear AFTER the scroll handler's 1ms.
      // The scroll event fired by the layout change above must observe
      // resizeDifference !== 0 so it is treated as layout, not input.
      if (resizeClearTimer) clearTimeout(resizeClearTimer);
      const myDelta = delta;
      resizeClearTimer = setTimeout(() => {
        requestAnimationFrame(() => {
          if (resizeDifference === myDelta) {
            resizeDifference = 0;
            resizeCorrelatedUntaggedScrollBudget = 0;
          }
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

  function canConsumeWheelDelta(el: HTMLElement, deltaY: number): boolean {
    if (deltaY < 0) return el.scrollTop > 0;
    if (deltaY > 0) return el.scrollTop + el.clientHeight < el.scrollHeight - 1;
    return false;
  }

  function clearGestureSession(
    session: GestureSession | null = gestureSession,
    opts: { repinIfStillSticky?: boolean } = {},
  ): void {
    if (session && gestureSession !== session) return;
    gestureSession = null;
    if (gestureSessionTimer) {
      clearTimeout(gestureSessionTimer);
      gestureSessionTimer = null;
    }
    if (!escapedFromLockState && isAtBottomState) {
      springStopRequested = false;
      // Rare-case safety net: intent armed but no scroll event fired (a
      // pointer-tap with no drag, a sub-pixel wheel rounded to 0) AND
      // content grew during the window AND the pin couldn't run because
      // the gate was pending. With Change 3's direction-aware gate this
      // is a no-op in the common case (the pin keeps firing for 'down'
      // / 'unknown' intent), but it remains correct for the edge cases.
      if (opts.repinIfStillSticky && scrollEl && distanceFromBottom() > AUTO_FOLLOW_BOTTOM_EPSILON_PX) {
        writeCaller = 'gestureSession.expire';
        writeScrollTop(targetScrollTop());
      }
    }
  }

  function expireGestureSession(session: GestureSession): void {
    if (gestureSession !== session) return;
    if (scrollEl && movedUpEnoughToEscape(session.startScrollTop, scrollEl.scrollTop)) {
      trace('scroll.gestureSession.expireEscape', () => ({
        source: session.source,
        direction: session.direction,
        startScrollTop: Math.round(session.startScrollTop),
        scrollTop: Math.round(scrollEl?.scrollTop ?? 0),
        isAtBottomState,
        escapedFromLockState,
      }));
      clearGestureSession(session);
      setEscapedFromLock(true);
      return;
    }
    clearGestureSession(session, { repinIfStillSticky: true });
  }

  function armUserScrollIntent(
    source: 'wheel' | 'key' | 'touch' | 'pointer',
    direction: 'up' | 'down' | 'unknown',
  ): void {
    if (!scrollEl) return;
    cancelTargetAnimation();
    cancelSpring();
    springStopRequested = true;
    restoreSnapArmed = false;
    const start = scrollEl.scrollTop;
    const session: GestureSession = {
      source,
      direction,
      startScrollTop: start,
      lastObservedScrollTop: start,
    };
    gestureSession = session;
    if (gestureSessionTimer) clearTimeout(gestureSessionTimer);
    gestureSessionTimer = setTimeout(() => {
      expireGestureSession(session);
    }, USER_SCROLL_INTENT_WINDOW_MS);
    trace('scroll.gestureSession.arm', () => ({
      source,
      direction,
      startScrollTop: Math.round(session.startScrollTop),
      isAtBottomState,
      escapedFromLockState,
    }));
  }

  function handleWheel(e: WheelEvent): void {
    if (!scrollEl) return;
    // Pinch-to-zoom on Mac trackpads arrives as `wheel + ctrlKey=true`
    // (Change 6). Without this filter, a zoom gesture at the bottom of
    // a streaming thread would spuriously arm a wheel intent and could
    // escape the lock once any subsequent scroll event landed.
    if (e.ctrlKey) return;
    if (e.deltaY === 0) return;
    if (e.deltaY > 0 && !escapedFromLockState) return;
    // Walk parents from event target; if first overflow ancestor is the
    // scroll element, the wheel landed on us. If a nested scroller can
    // consume this delta, ignore — the user is scrolling that nested
    // element, not us. If the nested scroller is already at its boundary,
    // the wheel can chain to the chat scroll element, so this is real
    // outer-timeline scroll intent.
    let cur: Element | null = e.target instanceof Element ? e.target : null;
    while (cur && cur !== scrollEl) {
      if (
        isOverflowAncestor(cur)
        && cur instanceof HTMLElement
        && cur.scrollHeight > cur.clientHeight
        && canConsumeWheelDelta(cur, e.deltaY)
      ) {
        return;
      }
      cur = cur.parentElement;
    }
    if (cur !== scrollEl) return;
    if (scrollEl.scrollHeight <= scrollEl.clientHeight) return;
    trace('scroll.wheel.armGesture', () => ({
      deltaY: e.deltaY,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      isAtBottomState,
      escapedFromLockState,
    }));
    armUserScrollIntent('wheel', e.deltaY < 0 ? 'up' : 'down');
  }

  function handlePointerDown(e: PointerEvent): void {
    if (!scrollEl) return;
    if (e.isPrimary === false) return;
    if (scrollEl.scrollHeight <= scrollEl.clientHeight) return;

    const scrollbarWidth = scrollEl.offsetWidth - scrollEl.clientWidth;
    if (scrollbarWidth <= 0) return;

    const rect = scrollEl.getBoundingClientRect();
    const style = window.getComputedStyle(scrollEl);
    const inRightGutter = e.clientX >= rect.right - scrollbarWidth;
    const inLeftGutter = style.direction === 'rtl' && e.clientX <= rect.left + scrollbarWidth;
    if (!inRightGutter && !inLeftGutter) return;

    trace('scroll.pointer.armGesture', () => ({
      clientX: Math.round(e.clientX),
      scrollbarWidth: Math.round(scrollbarWidth),
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      isAtBottomState,
      escapedFromLockState,
    }));
    // Scrollbar tap with no drag yet — direction will be refined on the
    // first scroll event that follows.
    armUserScrollIntent('pointer', 'unknown');
  }

  // ===== Scroll handler =====
  function handleScroll(): void {
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
    const distFromBottomAtEvent =
      scrollEl.scrollHeight - scrollTopAtEvent - scrollEl.clientHeight;
    // Capture and consume the programmatic-write tag synchronously so
    // it only suppresses ONE scroll event. Otherwise a later genuine
    // user scroll back to the same scrollTop value would be ignored.
    const tag = ignoreScrollToTop;
    ignoreScrollToTop = -1;
    const externalTagged = externalScrollIgnoreUntil > nowMs();
    refreshIsNearBottom();
    const session = gestureSession;
    const hadUserScrollIntent = session !== null;
    const pendingUserIntentMovedUp = !!session
      && movedUpEnoughToEscape(session.startScrollTop, scrollTopAtEvent);
    const tagged = (scrollTopAtEvent === tag || externalTagged) && !pendingUserIntentMovedUp;
    trace('scroll.scrollEvent', () => ({
      scrollTop: Math.round(scrollTopAtEvent),
      tag: Math.round(tag),
      externalTagged,
      tagged,
      pendingUserIntentMovedUp,
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
    const resizeCorrelatedScroll = resizeDifference !== 0 || resizeCorrelatedUntaggedScrollBudget > 0;
    if (resizeCorrelatedUntaggedScrollBudget > 0) resizeCorrelatedUntaggedScrollBudget -= 1;
    // Capture direction baseline BEFORE the deferral. We're inside the
    // synchronous handler for the current scroll event; this scrollTop
    // is what the user just produced. Used by the deferred re-stick
    // check to distinguish "user scrolled DOWN toward bottom" (re-stick
    // candidate) from "user scrolled UP" (must NOT re-stick — undoing
    // the wheel handler's just-set escape on the same gesture). The
    // baseline lives on the active gesture session; outside a gesture
    // there is no direction comparison to make.
    const previousObserved = session?.lastObservedScrollTop ?? -1;
    if (session) {
      session.lastObservedScrollTop = scrollTopAtEvent;
      // Pointer-tap arms with direction='unknown' because the user
      // hasn't moved yet. The first scroll event refines that to a
      // concrete direction so the pin gates (Change 3) can read it.
      if (session.direction === 'unknown' && previousObserved >= 0) {
        if (scrollTopAtEvent > previousObserved) session.direction = 'down';
        else if (scrollTopAtEvent < previousObserved) session.direction = 'up';
      }
    }
    // Defer 1ms so a concurrent RO callback can update resizeDifference
    // before we interpret direction. Mirrors upstream.
    setTimeout(() => {
      if (!scrollEl) return;
      if (session && pendingUserIntentMovedUp) {
        trace('scroll.scrollEvent.deferred.escapeUserScroll', () => ({
          source: session.source,
          direction: session.direction,
          startScrollTop: Math.round(session.startScrollTop),
          scrollTop: Math.round(scrollTopAtEvent),
          resizeDifference: Math.round(resizeDifference),
        }));
        clearGestureSession(session);
        setEscapedFromLock(true);
        return;
      }

      // RO race — content just resized; the scroll event reflects layout,
      // not user intent. Most importantly: virtua's $fixScrollJump can
      // adjust scrollTop to keep above-viewport rows stable, which would
      // otherwise look like an up-gesture. For non-virtua consumers
      // (Discussion's ChannelView) this gate is a 1ms suppression window
      // after each content-RO fire — vanishingly unlikely to swallow a
      // real user gesture, since the window only opens immediately after
      // a layout change.
      //
      // Gate on `!hadUserScrollIntent`: virtua's applyJump and other
      // layout-driven scroll writes can't produce wheel / key / touch /
      // pointer signals, so a pending intent is proof the scroll event
      // reflects a real user gesture, not the cascade. Without this
      // guard the bail also swallowed input-backed re-stick gestures
      // on heavy threads where the contentRO seam fires continuously
      // (virtua remeasurement + Streamdown async typesetting), leaving
      // escape stuck true after the user manually wheeled back to the
      // bottom.
      if (resizeCorrelatedScroll && !hadUserScrollIntent) {
        trace('scroll.scrollEvent.deferred.bailRO', () => ({
          resizeDifference: Math.round(resizeDifference),
          resizeCorrelatedScroll,
          hadUserScrollIntent,
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
      // (wheel, touch, keyboard). Restore intent flag so the
      // contentRO sync-pin can resume on the next content grow. No
      // scrollTop write here — they're already at the bottom.
      //
      // Three gates:
      //   1. Input-backed intent. Layout/virtua can also emit untagged
      //      scroll events while measuring rows; those must not mutate
      //      follow intent. A wheel/key/touch signal has to precede the
      //      scroll event.
      //   2. Direction. The scroll event from a wheel-up gesture is the
      //      event that confirms escape; if we also re-sticked whenever
      //      the user happens to land near the bottom, that same event
      //      would undo the escape. Skip re-stick when scrollTop is
      //      decreasing (UP direction); only DOWN-direction scrolls
      //      toward the bottom can re-engage auto-follow.
      //   3. AUTO_FOLLOW_BOTTOM_EPSILON_PX (4px, widened from 0.5 in
      //      Change 1) rather than the looser isNearBottomState (70px).
      //      Even on a DOWN scroll, only the actual bottom (within
      //      browser-rounding tolerance) counts.
      //
      // Bug A fix (Change 1): use `distFromBottomAtEvent` captured
      // synchronously above, NOT a fresh distanceFromBottom() read.
      // A concurrent streaming chunk may have grown scrollHeight in
      // the 1ms timer window; the user's scrollTop is unchanged but
      // the bottom moved away. The captured value reflects what the
      // user actually saw when they scrolled.
      const scrolledDown = previousObserved < 0
        ? false
        : scrollTopAtEvent > previousObserved;
      const shouldKeepIntentForRestick = !!session
        && escapedFromLockState
        && scrollTopAtEvent > session.startScrollTop
        && distFromBottomAtEvent > AUTO_FOLLOW_BOTTOM_EPSILON_PX;
      const willRestick = scrolledDown
        && hadUserScrollIntent
        && escapedFromLockState
        && distFromBottomAtEvent <= AUTO_FOLLOW_BOTTOM_EPSILON_PX;
      trace('scroll.scrollEvent.deferred', () => ({
        scrollTop: Math.round(scrollTopAtEvent),
        previousObserved: Math.round(previousObserved),
        scrolledDown,
        hadUserScrollIntent,
        sessionDirection: session?.direction ?? null,
        shouldKeepIntentForRestick,
        distFromBottomAtEvent: Math.round(distFromBottomAtEvent),
        distFromBottomNow: scrollEl ? Math.round(distanceFromBottom()) : null,
        willRestick,
        isAtBottomState,
        escapedFromLockState,
      }));
      if (willRestick) {
        if (session) clearGestureSession(session);
        escapedFromLockState = false;
        isAtBottomState = true;
        // Re-arm the spring after user gestures back to the bottom —
        // setEscapedFromLock(true) set springStopRequested on the
        // escape that started this round, and the re-stick is the
        // user's "I want to follow again" signal. Without this, future
        // streaming chunks would sync-pin instead of spring-chase.
        springStopRequested = false;
      } else if (session && !shouldKeepIntentForRestick) {
        clearGestureSession(session);
      }
    }, RESIZE_CLEAR_PADDING_MS);
  }

  // ===== scrollend handler (gesture-termination signal, Change 5) =====
  //
  // The native scrollend event fires when the scrolling stops — after
  // wheel-momentum settles, after a scrollbar drag releases, after a
  // keystroke completes. For laptop trackpads where momentum routinely
  // tails 200-500ms past fingertip release, this is a strictly better
  // gesture-termination signal than the 160ms wall-clock timer in
  // armUserScrollIntent. Baseline since Safari 26.2 (Dec 2025); all
  // desktop Wails targets meet it.
  //
  // Programmatic writes (sync-pin, spring chase, animateScrollTo) DO
  // fire scrollend per spec, but the gestureSession === null gate
  // below silently drops them — controller-owned writes never arm a
  // gesture, so there's nothing to resolve.
  //
  // The 160ms timer remains as the fallback for pointer-tap without
  // subsequent motion, sub-pixel wheel rounded to no-op, and any
  // environment that drops the event. Both paths funnel through
  // expireGestureSession's idempotent guard so dual-firing is safe.
  function handleScrollEnd(): void {
    if (!scrollEl) return;
    const session = gestureSession;
    if (session === null) return;
    trace('scroll.scrollend.expireGestureSession', () => ({
      source: session.source,
      direction: session.direction,
      startScrollTop: Math.round(session.startScrollTop),
      scrollTop: Math.round(scrollEl?.scrollTop ?? 0),
    }));
    expireGestureSession(session);
  }

  // ===== Keydown / touch handlers (intent signals) =====
  function handleKeydown(e: KeyboardEvent): void {
    if (UP_KEYS.has(e.key)) armUserScrollIntent('key', 'up');
    if (escapedFromLockState && DOWN_KEYS.has(e.key)) armUserScrollIntent('key', 'down');
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
    if (dy > 1) armUserScrollIntent('touch', 'up');
    if (dy < -1 && escapedFromLockState) armUserScrollIntent('touch', 'down');
  }
  function handleTouchEnd(): void {
    touchStartY = null;
  }

  // ===== Public actions =====
  function setEscapedFromLock(next: boolean): void {
    clearGestureSession();
    if (next) {
      cancelTargetAnimation();
      // User explicitly broke from auto-follow — bail any in-flight
      // spring chase. The tick observes springStopRequested + new
      // state and clears the token on the next frame, but we also
      // null it here for the "no rAF before next attach" edge case.
      springStopRequested = true;
      cancelSpring();
      // Clear any pending restore-snap consent: a fresh user escape
      // invalidates a yet-to-be-consumed restore-snap. The thread-
      // switch entry point that legitimately wants a restore-snap
      // calls `armRestoreSnap()` AFTER its defensive setEscape, so
      // this clear is the right default — a stale consent left over
      // from an earlier path can't slip through.
      restoreSnapArmed = false;
    }
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

  function armRestoreSnap(): void {
    restoreSnapArmed = true;
    trace('scroll.restoreSnap.arm', () => ({
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
    }));
  }

  function stopScroll(): void {
    setEscapedFromLock(true);
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
    if (scrollEl) {
      // Active gesture keeps its baseline aligned with the post-write
      // position so the next user scroll's direction inference compares
      // against the new top.
      if (gestureSession) gestureSession.lastObservedScrollTop = scrollEl.scrollTop;
      refreshIsNearBottom();
    }
  }

  function runExternalScroll(action: () => void, opts: { preserveIntent?: boolean } = {}): void {
    if (!opts.preserveIntent) setEscapedFromLock(true);
    tagExternalProgrammaticScroll(action);
  }

  async function preserveScrollAnchor(
    anchor: HTMLElement,
    action: () => void | Promise<void>,
  ): Promise<void> {
    if (!scrollEl || !anchor.isConnected) {
      await action();
      return;
    }

    const targetScrollEl = scrollEl;
    const hadBottomFollowIntent = isAtBottomState && !escapedFromLockState;
    const release = pauseAutoScroll();
    let actionError: unknown;
    let actionPromise: Promise<void> | undefined;
    try {
      const beforeTop = hadBottomFollowIntent ? null : anchor.getBoundingClientRect().top;
      actionPromise = (async () => {
        await action();
      })().catch((err: unknown) => {
        actionError = err;
      });
      await tick();
      if (
        !hadBottomFollowIntent &&
        beforeTop !== null &&
        scrollEl === targetScrollEl &&
        anchor.isConnected
      ) {
        const afterTop = anchor.getBoundingClientRect().top;
        const delta = afterTop - beforeTop;
        if (Number.isFinite(delta) && Math.abs(delta) >= 0.5) {
          writeCaller = 'preserveScrollAnchor';
          writeScrollTop(targetScrollEl.scrollTop + delta);
        }
      }
    } finally {
      release();
    }
    await actionPromise;
    if (actionError !== undefined) throw actionError;
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

  function forceStick(opts: { reason?: 'user' | 'restore' } = {}): void {
    const reason = opts.reason ?? 'user';
    // Restore-reason consent gate: when called from a restore $effect
    // (thread switch, channel initial poll), only proceed if the
    // caller's entry point armed the consent first. This stops a
    // stale or duplicate restore from clobbering an existing user
    // escape with a snap-to-bottom they didn't ask for (the seq-509
    // trace bug — a `restoreToBottom()` firing 17s after the user
    // wheel-escaped, slamming them to the bottom and wiping escape).
    // User-reason calls always proceed (chip click / explicit
    // intent), and they ALSO consume any pending restore consent so
    // the next call doesn't see a stale arm.
    if (reason === 'restore' && !restoreSnapArmed) {
      trace('scroll.forceStick.skipRestore', () => ({
        reason,
        restoreSnapArmed,
        isAtBottomState,
        escapedFromLockState,
        pauseDepth,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      }));
      return;
    }
    // DO NOT relax this gate to be direction-aware (cf. Change 3).
    // Restore-snap MUST NO-OP on any pending intent regardless of
    // direction — this is the seq-509 trace bug defense: a stale
    // restore $effect racing a user gesture mid-stream would otherwise
    // slam the user to bottom on any 'down' or 'unknown' intent.
    const activeSession = gestureSession;
    if (reason === 'restore' && activeSession !== null) {
      trace('scroll.forceStick.skipGestureSession', () => ({
        reason,
        source: activeSession.source,
        direction: activeSession.direction,
        startScrollTop: Math.round(activeSession.startScrollTop),
        isAtBottomState,
        escapedFromLockState,
        pauseDepth,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      }));
      return;
    }
    restoreSnapArmed = false;
    trace('scroll.forceStick.entry', () => ({
      reason,
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      target: scrollEl ? Math.round(targetScrollTop()) : null,
    }));
    setEscapedFromLock(false);
    cancelSpring();
    // Reset the stop flag AFTER cancel — cancelSpring observes
    // springStopRequested via the rAF tick guard, but the value at the
    // current synchronous frame doesn't affect cancellation (the token
    // mismatch on the next tick handles it). We clear it now so the
    // next streaming chunk can re-engage the spring.
    springStopRequested = false;
    // Only restore/thread-switch snaps should re-hide content for the
    // measurement warmup. A user click on the scroll-to-bottom chip is
    // an explicit visible action in an already-mounted thread; blanking
    // the transcript until the failsafe fires is worse than the small
    // chance of a post-snap measurement correction.
    if (reason === 'restore') beginWarmup();
    if (!scrollEl) return;
    isAtBottomState = true;
    writeCaller = 'forceStick';
    writeScrollTop(targetScrollTop());
  }

  function markAtBottom(): void {
    // Flag-only counterpart to forceStick: caller already established
    // bottom geometry, or there is no geometry yet because the
    // timeline is empty. Used by chat's restoreToBottom empty-timeline
    // branch — so the restore-snap consent must be consumed here too,
    // otherwise the arm leaks past a completed empty-thread restore
    // and admits a later stale restore-stick.
    trace('scroll.markAtBottom', () => ({
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
    }));
    restoreSnapArmed = false;
    setEscapedFromLock(false);
    isAtBottomState = true;
    refreshIsNearBottom();
  }

  function readNotifyContentGate(): {
    gateScrollEl: boolean;
    gateEscape: boolean;
    gatePendingUserIntent: boolean;
    gatePause: boolean;
    gateNotAtBottom: boolean;
    canPin: boolean;
  } {
    const gateScrollEl = scrollEl !== undefined;
    const gateEscape = escapedFromLockState;
    // Direction-aware pending-intent gate (Change 3): block ONLY an
    // active UP gesture. 'down' / 'unknown' intents still permit
    // composer-height / live-content pins so the bottom doesn't drift
    // away from a user who tapped the scrollbar or made a no-op
    // trackpad nudge during streaming. The strict restore gate in
    // forceStick({reason:'restore'}) stays unchanged.
    const gatePendingUserIntent = gestureSession?.direction === 'up';
    const gatePause = pauseDepth > 0;
    const gateNotAtBottom = !isAtBottomState;
    const canPin = gateScrollEl
      && !gateEscape
      && !gatePendingUserIntent
      && !gatePause
      && !gateNotAtBottom;

    return {
      gateScrollEl,
      gateEscape,
      gatePendingUserIntent,
      gatePause,
      gateNotAtBottom,
      canPin,
    };
  }

  function instantPinAfterExternalGeometryChange(caller: string): void {
    // Stamp resizeDifference BEFORE writing scrollTop so the resulting
    // scroll event is treated as RO-correlated, not user-driven. Without
    // this, a textarea-shrink could cause the scroll handler's re-stick
    // path to flip isAtBottom in a way that surprises the user.
    resizeDifference = 1;
    if (resizeClearTimer) clearTimeout(resizeClearTimer);
    resizeClearTimer = setTimeout(() => {
      requestAnimationFrame(() => {
        if (resizeDifference === 1) {
          resizeDifference = 0;
          resizeCorrelatedUntaggedScrollBudget = 0;
        }
      });
    }, RESIZE_CLEAR_PADDING_MS);
    writeCaller = caller;
    writeScrollTop(targetScrollTop());
  }

  function notifyContentMaybeGrew(): void {
    const gate = readNotifyContentGate();
    trace('scroll.notifyContentMaybeGrew', () => ({
      willPin: gate.canPin,
      gateScrollEl: gate.gateScrollEl,
      gateEscape: gate.gateEscape,
      gatePendingUserIntent: gate.gatePendingUserIntent,
      gatePause: gate.gatePause,
      gateNotAtBottom: gate.gateNotAtBottom,
      pauseDepth,
      isNearBottomState,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      target: scrollEl ? Math.round(targetScrollTop()) : null,
    }));
    if (!gate.canPin) return;
    instantPinAfterExternalGeometryChange('notifyContentMaybeGrew');
  }

  function notifyLiveContentMaybeGrew(): void {
    const gate = readNotifyContentGate();
    const willSpring = gate.canPin && warm && springGateOpen();
    trace('scroll.notifyLiveContentMaybeGrew', () => ({
      canPin: gate.canPin,
      willSpring,
      gateScrollEl: gate.gateScrollEl,
      gateEscape: gate.gateEscape,
      gatePendingUserIntent: gate.gatePendingUserIntent,
      gatePause: gate.gatePause,
      gateNotAtBottom: gate.gateNotAtBottom,
      pauseDepth,
      isNearBottomState,
      warm,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      target: scrollEl ? Math.round(targetScrollTop()) : null,
    }));
    if (!gate.canPin) return;

    const target = targetScrollTop();
    if (willSpring && scrollEl && scrollEl.scrollTop < target) {
      lastGrewAt = nowMs();
      startSpringIfNeeded();
      return;
    }

    // Same instant fallback as notifyContentMaybeGrew for non-spring
    // modes, warm-up, reduced-motion users, and no-distance/overshoot
    // nudges where a spring has nothing useful to chase.
    instantPinAfterExternalGeometryChange('notifyLiveContentMaybeGrew');
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
      const willRepin = pauseDepth === 0
        && !escapedFromLockState
        && gestureSession === null
        && isAtBottomState;
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
    // Start the warm gate at attach. The first contentRO callback
    // fires for whatever content is already there, then virtua's per-
    // row ROs and Streamdown's typesetting cascade — all of which
    // should sync-pin (silent) regardless of animationMode until the
    // controller observes contentRO quiet for QUIET_MS or the
    // FAILSAFE_MS deadline trips.
    beginWarmup();
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
    nextScrollEl.addEventListener('pointerdown', handlePointerDown, { passive: true });
    nextScrollEl.addEventListener('keydown', handleKeydown);
    nextScrollEl.addEventListener('touchstart', handleTouchStart, { passive: true });
    nextScrollEl.addEventListener('touchmove', handleTouchMove, { passive: true });
    nextScrollEl.addEventListener('touchend', handleTouchEnd, { passive: true });
    nextScrollEl.addEventListener('touchcancel', handleTouchEnd, { passive: true });
    // scrollend: Baseline since Safari 26.2 (Dec 2025). Feature-detect
    // on the window because not every host (older Wails webviews on
    // long-tail Linux distros) ships it; missing it falls back to the
    // 160ms timer, which is what shipped before Change 5.
    const scrollEndSupported = typeof window !== 'undefined' && 'onscrollend' in window;
    if (scrollEndSupported) {
      nextScrollEl.addEventListener('scrollend', handleScrollEnd, { passive: true });
    }
    detachWheel = () => nextScrollEl.removeEventListener('wheel', handleWheel);
    detachScroll = () => nextScrollEl.removeEventListener('scroll', handleScroll);
    detachPointer = () => nextScrollEl.removeEventListener('pointerdown', handlePointerDown);
    detachKeyTouch = () => {
      nextScrollEl.removeEventListener('keydown', handleKeydown);
      nextScrollEl.removeEventListener('touchstart', handleTouchStart);
      nextScrollEl.removeEventListener('touchmove', handleTouchMove);
      nextScrollEl.removeEventListener('touchend', handleTouchEnd);
      nextScrollEl.removeEventListener('touchcancel', handleTouchEnd);
      if (scrollEndSupported) {
        nextScrollEl.removeEventListener('scrollend', handleScrollEnd);
      }
    };
    // Direction baseline now lives on the active gesture session
    // (seeded in armUserScrollIntent from the live scrollTop), so
    // attach() no longer needs a module-scope seed.
    refreshIsNearBottom();
  }

  function detach(): void {
    contentRO?.disconnect();
    contentRO = undefined;
    detachWheel?.();
    detachWheel = undefined;
    detachScroll?.();
    detachScroll = undefined;
    detachPointer?.();
    detachPointer = undefined;
    detachKeyTouch?.();
    detachKeyTouch = undefined;
    if (resizeClearTimer) {
      clearTimeout(resizeClearTimer);
      resizeClearTimer = null;
    }
    if (externalScrollClearTimer) {
      clearTimeout(externalScrollClearTimer);
      externalScrollClearTimer = null;
    }
    externalScrollIgnoreUntil = 0;
    clearGestureSession();
    cancelTargetAnimation();
    cancelSpring();
    springStopRequested = false;
    lastGrewAt = 0;
    clearWarmupTimers();
    warm = false;
    resizeDifference = 0;
    resizeCorrelatedUntaggedScrollBudget = 0;
    previousHeight = undefined;
    touchStartY = null;
    // DELIBERATELY leave `restoreSnapArmed` untouched. attach() calls
    // detach() up-front when scrollEl / contentEl change, and on first
    // mount that wipe ran BETWEEN the consumer's $effect.pre arm and
    // the restore $effect's forceStick({reason:'restore'}), making
    // the consent effectively unusable for the initial-mount path.
    // The flag is invalidated by outer-scroll escape intent (wheel / key /
    // touch that can reach the scroll element), selection, animateScrollTo /
    // stopScroll, explicit user-reason forceStick, and the
    // consume-on-restore path itself — that's
    // enough to keep it from leaking stale consent across legitimate
    // lifecycles. True teardown also discards the entire controller
    // instance, so a residual `true` here has no observable effect.
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
    get isWarm() {
      return warm;
    },
    pauseAutoScroll,
    notifyContentMaybeGrew,
    notifyLiveContentMaybeGrew,
    preserveScrollAnchor,
    attach,
    detach,
    forceStick,
    markAtBottom,
    animateScrollTo,
    runExternalScroll,
    stopScroll,
    setEscapedFromLock,
    armWarmup: beginWarmup,
    armRestoreSnap,
  };
}
