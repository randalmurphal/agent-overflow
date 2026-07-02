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
//     whenever no live content has advanced recently — late Streamdown
//     typesetting on settled content, virtua row remeasurement on a
//     freshly-mounted thread, etc.
//
//   - 'spring': velocity-spring chase. The viewport interpolates toward
//     the moving bottom across rAF ticks so the user sees a smooth
//     scroll-follow. Chat MessageTimeline selects it via a content-keyed
//     latch (`latchedSpringMode` over `pane.lastLiveContentAt`), so
//     streaming chunks — and the end-of-turn drain after the turn signal
//     clears — flow in with a smooth animation. Gated by a quiescence-based warm
//     state: spring stays off until contentRO has been quiet for
//     QUIET_MS or the FAILSAFE_MS deadline trips, whichever comes
//     first. A one-shot structural-append mark can also make the next
//     near-term command/tool row growth spring-eligible while
//     animationMode is 'instant'; that path cancels after arrival and does
//     not enter the streaming sentinel. The warm gate defends against the
//     original 80LoC-spring-delete regression (commit e00723f) where
//     mount-time virtua remeasurement and async Streamdown typesetting
//     would spring-chase a thread restore visibly.
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
// depth-counted lease, observe(kind) reports geometry changes outside
// the content element. Both ChatView and ChannelView observe
// 'composer-geometry' from their composer ResizeObservers when
// out-of-content height changes; the seam is identical on both surfaces.

import { tick } from 'svelte';
import { isUiRenderTraceEnabled, recordUiTrace } from './uiRenderTrace';
import {
  ARRIVAL_DISTANCE_PX,
  AUTO_FOLLOW_BOTTOM_EPSILON_PX,
  SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX,
  resolveContentDelivery,
  resolveVirtuaCompensation,
  withinArrivalBand,
  type ContentDeltaObservation,
  type ResolverState,
  type VirtuaCompensationObservation,
} from './scroll/resolver';
import { createSpringChase } from './scroll/spring';
import { nowMs } from './scroll/time';
import type {
  ScrollObservationKind,
  ScrollWriteCaller,
  UseStickToBottomController,
  UseStickToBottomOptions,
} from './scroll/types';

// Public types re-exported so this module remains the single import
// surface for consumers until the Stage-4 rename to scroll/index.svelte.ts
// updates their import paths.
export type {
  ScrollObservationKind,
  UseStickToBottomController,
  UseStickToBottomOptions,
} from './scroll/types';
// Re-exported for the controller test's retain-window assertions; the
// constant lives with the spring kinematics.
export { RETAIN_ANIMATION_DURATION_MS } from './scroll/spring';

// Diagnostic trace helper — no-op in production (gated by
// `isUiRenderTraceEnabled` which only returns true in dev with
// `VITE_AGENT_OVERFLOW_UI_TRACE=1`, set by `make dev DEBUG=1`). The
// thunk skips object construction when disabled. Records flow into
// `${configDir}/ui-trace/ui-render.jsonl` via the same batched
// `AppendUIRenderTraceBatch` binding the timeline render trace uses.
//
// Call sites double-gate with `if (isUiRenderTraceEnabled()) trace(…)`
// because Rolldown inlines the gate but does not eliminate the closure
// allocation — the outer guard prevents ~120 closures/sec during
// spring animation in production builds.
function trace(label: string, build: () => Record<string, unknown>): void {
  if (!isUiRenderTraceEnabled()) return;
  recordUiTrace(label, build());
}

// Three-band geometry — see docs/architecture/frontend-scroll.md for
// the full rationale. Tightening any one of these affects a
// different UX surface; the asymmetry is load-bearing.
//
// Visual near-bottom band: drives the scroll-to-bottom chip and the
// negative-delta repin's geometric branch. Loose so a user within 70px
// doesn't see the chip flicker.
const STICK_TO_BOTTOM_OFFSET_PX = 70;
// AUTO_FOLLOW_BOTTOM_EPSILON_PX (the down-scroll re-stick band) lives in
// scroll/resolver.ts — the virtua compensation resolver shares it for the
// anchor-redirect "already pinned" tolerance.
// The idle re-pin deadband (IDLE_REPIN_DEADBAND_PX, scroll/resolver.ts)
// deliberately equals AUTO_FOLLOW_BOTTOM_EPSILON_PX — "close enough to
// count as at-bottom" and "close enough not to fight a fractional-DPR
// wobble" are the same tolerance.
// ResizeObserver width jitter below half a CSS pixel is usually rounding
// noise. Wider changes mean the content column reflowed; any paired height
// delta is layout correction, not new live transcript content.
const CONTENT_REFLOW_WIDTH_EPSILON_PX = 0.5;
// Width and height can arrive in separate ResizeObserver deliveries. Keep
// the layout-correction classification alive briefly so a width-only fire
// followed by renderer height settle still sync-pins.
const CONTENT_REFLOW_SETTLE_WINDOW_MS = 250;
const RESIZE_CLEAR_PADDING_MS = 1;
const RECENT_DOWN_INTENT_WINDOW_MS = 250;
const SCROLLBAR_DRAG_SESSION_FAILSAFE_MS = 30_000;
const EXTERNAL_SCROLL_TAG_CLEAR_MS = 100;
const PROGRAMMATIC_SCROLL_EVENT_TOKEN_TTL_MS = 500;
const MAX_PROGRAMMATIC_SCROLL_EVENT_TOKENS = 128;
const PROGRAMMATIC_SCROLL_EVENT_DUPLICATE_BUDGET = 4;

// Spring kinematics, tuning constants, and the sentinel/structural-append
// windows live in scroll/spring.ts. Only the trace sampling stays here,
// because it belongs to the writeScrollTop chokepoint:
// Spring tick writes fire at 60Hz during a chase. Sample so the
// dev-only trace file isn't dominated by predictable +1px increments.
// 12 ≈ 5Hz, which is enough to see the spring is running without
// crowding the rare gesture/escape events that diagnose scroll
// regressions. First and last ticks of every chase are always
// recorded via the springTickSinceLastTrace reset at chase boundaries.
const SPRING_TICK_TRACE_SAMPLE = 12;

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
// Shortened quiet window used when the consumer's `quietContextSignal`
// is truthy at bump time — that signal means "I have first-hand evidence
// the visible async typesetting (svelte-streamdown's
// shiki / katex / mermaid) is done." We still want ONE frame of
// contentRO silence after the last event so the post-typesetting layout
// has time to settle, but the conservative 100ms wait that defends
// against late typesetting is no longer needed. 16ms ≈ one rAF tick.
//
// CRITICAL: the shortened window is only sound once the SURFACE has
// stopped moving. `quietContextSignal` reports svelte-streamdown's async
// state — it is blind to virtua's estimate→measure cascade, which mounts
// rows at ESTIMATED_ROW_SIZE and grows scrollHeight over a sequence of
// contentRO fires spaced WIDER than SETTLED_QUIET_MS. If we shorten the
// window while that cascade is in flight, the timer fires in the gap
// between two corrections and reveals a still-growing surface — the
// "lands right, flickers, lands right again" idle-thread regression. So
// the shortened window is gated on geometry stability
// (`quietWindowForGeometry`): a large contentRO height delta keeps us on
// the conservative QUIET_MS window (which each cascade fire resets, so it
// only closes once the cascade goes quiet — geometry-driven, not a
// cascade-duration guess); only a small delta admits the shortcut.
// Granularity: the gate keys on the AGGREGATE contentRO height delta, so a
// lone row measuring within the epsilon of its estimate and correcting a
// frame ahead of a later large step could still admit the shortcut early.
// Accepted as an unlikely residual — per-row tracking would need the
// second observer the scroll contract forbids, and a real cascade fire
// measures many rows at once, so the aggregate step dwarfs any single
// in-band row.
const SETTLED_QUIET_MS = 16;
// A contentRO height delta at or below this counts as "the surface has
// effectively settled" — small enough that revealing on SETTLED_QUIET_MS
// cannot show a perceptible bottom-shift. Anything larger is treated as
// virtua's estimate→measure cascade still in flight. Absolute px, not
// viewport-relative: the threshold is human-perceptible scroll
// displacement, which does not scale with viewport height. virtua mounts
// rows at ESTIMATED_ROW_SIZE (56px, see MessageTimeline.svelte), so each
// per-row correction is |measured − 56| — tens-to-hundreds of px for any
// real multi-line chat row, comfortably clear of this floor.
const WARMUP_SETTLE_EPSILON_PX = 8;

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

function roundCssPx(value: number): number {
  return Math.round(value * 100) / 100;
}

export function createUseStickToBottomController(
  options: UseStickToBottomOptions = {},
): UseStickToBottomController {
  installModuleSelectionListeners();

  // ===== Reactive state (consumed by templates / $derived) =====
  // Intent flag: "we want to be glued to the bottom". Mirrors upstream's
  // state.isAtBottom — set true on initial mount, on forceStick, and when
  // a re-stick condition fires from the scroll handler. Set false on
  // explicit escape (outer wheel/key/touch scroll, select). Crucially
  // this is NOT geometry-derived; the contentRO sync-pin path relies on
  // it staying true even when content grew the bottom out from under us
  // — that's the gate that keeps the pin from running after the user
  // explicitly scrolled away.
  let isAtBottomState = $state(true);
  // Geometric ≤70px-from-bottom flag. Updated by refreshIsNearBottom on
  // every scroll event and after every programmatic write. The public
  // `isAtBottom` getter returns false while escaped even inside this
  // visual band so the ScrollToBottomButton reflects user intent.
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
  let stickStateDevHook: (() => Record<string, unknown>) | undefined;

  let resizeDifference = 0;
  let resizeClearTimer: ReturnType<typeof setTimeout> | null = null;
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
  let previousHeight: number | undefined;
  let previousWidth: number | undefined;
  let contentReflowSettleUntil = 0;
  let touchStartY: number | null = null;
  let resizeCorrelatedUntaggedScrollBudget = 0;
  // Controller-scope baseline so `scrolledDown` can be computed for the
  // re-stick path. Re-stick requires both recent down input and a real
  // downward scroll event landing at the bottom; this baseline keeps
  // layout clamps from masquerading as user-driven down motion.
  let lastObservedScrollTopForRestick = -1;

  // ===== Arrival-readback acceptance state =====
  // Some engines reject the exact max scrollTop by one CSS pixel. When a
  // write lands within the arrival band but not exactly on target, the
  // accepted readback is recorded so arrival checks stop re-writing a
  // target the browser will keep rejecting. Owned here — not by the
  // spring — because notifyLiveContentMaybeGrew shares it; the spring
  // chase (scroll/spring.ts) reaches it through its deps. Helpers live
  // in the Geometry section below.
  let arrivalReadbackAcceptedTarget: number | null = null;

  // ===== Restore-snap consent state =====
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
  let hasFirstContentRO = false;
  // Magnitude of the most recent contentRO height change, shared by both
  // quiet-timer arming sites (`bumpQuietTimer`, `notifyQuietContextSignalChanged`)
  // so the shortened window stays geometry-gated. Reset to +Infinity each
  // warm-up cycle: geometry is assumed to be moving until a small delta is
  // actually observed, so the first fire (which has no baseline) and any
  // large correction both hold us on the conservative window.
  let lastContentHeightDelta = Number.POSITIVE_INFINITY;

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
    if (isUiRenderTraceEnabled()) trace(`scroll.warmup.${reason}`, () => ({
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
    }));
  }

  function beginWarmup(): void {
    clearWarmupTimers();
    warm = false;
    hasFirstContentRO = false;
    lastContentHeightDelta = Number.POSITIVE_INFINITY;
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

  // Pick the quiet window for an arm. The shortened SETTLED_QUIET_MS is
  // only sound once the surface has stopped moving in large steps; while
  // virtua's estimate→measure cascade is still growing scrollHeight we use
  // the conservative QUIET_MS window, which each cascade fire resets so it
  // closes only after the cascade goes quiet. See WARMUP_SETTLE_EPSILON_PX.
  function quietWindowForGeometry(): number {
    return lastContentHeightDelta <= WARMUP_SETTLE_EPSILON_PX
      ? SETTLED_QUIET_MS
      : QUIET_MS;
  }

  // `heightDelta` is the contentRO height change driving this bump
  // (undefined on the first fire — no baseline yet). It keeps the shortened
  // window geometry-gated: until we have observed the surface hold still,
  // a thread's mount cascade cannot trip an early reveal.
  function bumpQuietTimer(heightDelta?: number): void {
    if (warm) return;
    hasFirstContentRO = true;
    // First fire (undefined) has no baseline → treat as still-moving. A
    // height delta of exactly 0 is a spurious / width-only / padding-var
    // reflow (this runs before the contentRO's own `delta === 0` early-out)
    // and carries no new height information — keep the prior magnitude so a
    // reflow interleaved between two large cascade steps cannot masquerade
    // as "settled" and trip the shortened window. That interleave is the
    // cold-boot residual: cascade steps are far apart and font/layout
    // reflows fire in the gaps.
    if (heightDelta === undefined) {
      lastContentHeightDelta = Number.POSITIVE_INFINITY;
    } else if (heightDelta !== 0) {
      lastContentHeightDelta = Math.abs(heightDelta);
    }
    if (quietTimer) clearTimeout(quietTimer);
    if (!options.quietContextSignal) {
      quietTimer = setTimeout(() => markWarm('quiet'), QUIET_MS);
      return;
    }
    const settled = options.quietContextSignal();
    if (!settled) {
      quietTimer = null;
      return;
    }
    quietTimer = setTimeout(() => markWarm('quiet'), quietWindowForGeometry());
  }

  function notifyQuietContextSignalChanged(): void {
    const settled = options.quietContextSignal?.() ?? false;
    const haveTimer = quietTimer !== null;
    let outcome: 'armed' | 'rearmed' | 'noop_warm' | 'noop_no_ro' | 'noop_signal_falsy';
    if (warm) outcome = 'noop_warm';
    else if (!settled) outcome = 'noop_signal_falsy';
    else if (!haveTimer && !hasFirstContentRO) outcome = 'noop_no_ro';
    else if (haveTimer) outcome = 'rearmed';
    else outcome = 'armed';
    if (isUiRenderTraceEnabled()) trace('scroll.warmup.signalChanged', () => ({
      outcome,
      settled,
      haveTimer,
      warm,
    }));
    if (outcome === 'noop_warm' || outcome === 'noop_no_ro' || outcome === 'noop_signal_falsy') return;
    if (quietTimer) clearTimeout(quietTimer);
    // Geometry-gated like bumpQuietTimer: if the surface was still moving in
    // large steps at the last contentRO, the settle signal flipping does not
    // license an early reveal — the cascade outlasts the settle signal.
    quietTimer = setTimeout(() => markWarm('quiet'), quietWindowForGeometry());
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

  function scrollTopIsAtTarget(target: number): boolean {
    return !scrollEl || withinArrivalBand(scrollEl.scrollTop, target);
  }

  function acceptedReadbackMatchesTarget(target: number): boolean {
    return arrivalReadbackAcceptedTarget !== null
      && withinArrivalBand(arrivalReadbackAcceptedTarget, target)
      && scrollTopIsAtTarget(target);
  }

  function shouldWriteExactArrivalTarget(target: number): boolean {
    if (!scrollEl) return false;
    if (scrollEl.scrollTop === target) return false;
    if (!scrollTopIsAtTarget(target)) return true;
    return !acceptedReadbackMatchesTarget(target);
  }

  function recordArrivalReadbackAcceptance(target: number): void {
    if (scrollEl && scrollEl.scrollTop !== target && scrollTopIsAtTarget(target)) {
      arrivalReadbackAcceptedTarget = target;
      return;
    }
    arrivalReadbackAcceptedTarget = null;
  }

  function writeExactArrivalTarget(caller: ScrollWriteCaller, target: number): void {
    writeScrollTop(caller, target);
    recordArrivalReadbackAcceptance(target);
  }

  function clearArrivalReadback(): void {
    arrivalReadbackAcceptedTarget = null;
  }

  // Drop an accepted readback whose target has since moved out of the
  // arrival band — the acceptance only excuses re-writes for the target
  // it was recorded against.
  function invalidateStaleArrivalReadback(target: number): void {
    if (
      arrivalReadbackAcceptedTarget !== null
      && !withinArrivalBand(arrivalReadbackAcceptedTarget, target)
    ) {
      arrivalReadbackAcceptedTarget = null;
    }
  }

  function distanceFromBottom(): number {
    if (!scrollEl) return 0;
    return scrollEl.scrollHeight - scrollEl.scrollTop - scrollEl.clientHeight;
  }
  function refreshIsNearBottom(): number {
    const dist = distanceFromBottom();
    const next = dist <= STICK_TO_BOTTOM_OFFSET_PX;
    if (next !== isNearBottomState) isNearBottomState = next;
    return dist;
  }

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
    escapedFromLockState = false;
    isAtBottomState = true;
    spring.clearStopRequest();
    if (isUiRenderTraceEnabled()) trace('scroll.restick.input', () => ({
      source,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      distFromBottom: scrollEl ? Math.round(distanceFromBottom()) : null,
    }));
  }

  function recordRecentDownIntent(source: 'wheel' | 'key' | 'touch'): void {
    if (!escapedFromLockState) return;
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
    if (isUiRenderTraceEnabled()) trace('scroll.intent.down', () => ({
      source,
      recentDownIntentUntil: Math.round(recentDownIntentUntil),
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      distFromBottom: scrollEl ? Math.round(distanceFromBottom()) : null,
      escapedFromLockState,
    }));
    if (
      scrollEl
      && escapedFromLockState
      && distanceFromBottom() <= AUTO_FOLLOW_BOTTOM_EPSILON_PX
    ) {
      restickFromUserInput(source);
    }
  }

  // ===== Programmatic scroll write =====
  // Spring-tick writes fire at 60Hz during a chase — predictable
  // increment-by-1px from the spring solver, and each record is
  // ~300 bytes. Without sampling, they were 5% of the 10 MB rotation
  // file and crowded out the rare gesture/escape events that actually
  // matter for diagnosing scroll regressions. We trace one in every
  // SPRING_TICK_TRACE_SAMPLE writes (~5Hz) plus the very first tick
  // of each chase via `forceNextSpringTickTrace` (called from
  // spring.start()). Starts at `SAMPLE - 1` so the first write is
  // recorded (the gating predicate is `<`, so equal-or-greater values
  // record and reset).
  let springTickSinceLastTrace = SPRING_TICK_TRACE_SAMPLE - 1;
  // Chase boundaries force the next tick write to record (spring deps) so
  // the trace shows every chase start, not just every ~12th sampled write.
  function forceNextSpringTickTrace(): void {
    springTickSinceLastTrace = SPRING_TICK_TRACE_SAMPLE - 1;
  }
  function writeScrollTop(caller: ScrollWriteCaller, value: number): void {
    if (!scrollEl) return;
    // Hot path: spring follow can call this every frame. The app contract is
    // that controller-owned scrollers do not get CSS-authored smooth scroll;
    // only inline values need temporary suppression around the write.
    const originalScrollBehavior = scrollEl.style.scrollBehavior;
    const suppressScrollBehavior =
      originalScrollBehavior !== '' && originalScrollBehavior !== 'auto';
    if (suppressScrollBehavior) scrollEl.style.scrollBehavior = 'auto';
    // Determine whether this write will be traced BEFORE reading any
    // pre-write geometry. The sampling decision and the
    // isUiRenderTraceEnabled gate are both pure reads with no side
    // effects, so hoisting them above the write is safe. This lets us
    // skip the three layout reads (scrollTop, scrollHeight, clientHeight)
    // on the hot path — spring ticks fire at 60Hz and the trace is
    // sampled to ~5Hz, so ~92% of ticks skip the reads entirely.
    let recordTrace = true;
    if (caller === 'spring.tick') {
      if (springTickSinceLastTrace < SPRING_TICK_TRACE_SAMPLE - 1) {
        springTickSinceLastTrace += 1;
        recordTrace = false;
      } else {
        springTickSinceLastTrace = 0;
      }
    }
    const shouldTrace = recordTrace && isUiRenderTraceEnabled();
    let beforeTop = 0, beforeHeight = 0, beforeClient = 0;
    if (shouldTrace) {
      beforeTop = scrollEl.scrollTop;
      beforeHeight = scrollEl.scrollHeight;
      beforeClient = scrollEl.clientHeight;
    }
    // Let the consumer mark this write as programmatic for any library
    // observing the scroll element (virtua manual-scroll marking) BEFORE
    // the write lands, so the scroll event it dispatches is already
    // classified.
    options.onBeforeScrollTopWrite?.();
    scrollEl.scrollTop = value;
    // Tag using the BROWSER-rounded read so the scroll handler's token
    // match sees the same value the scroll event will report.
    const taggedTop = scrollEl.scrollTop;
    recordProgrammaticScrollEventToken(taggedTop);
    lastObservedScrollTopForRestick = taggedTop;
    refreshIsNearBottom();
    if (shouldTrace) {
      trace('scroll.write', () => ({
        caller,
        requested: Math.round(value),
        beforeTop: Math.round(beforeTop),
        afterTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: Math.round(beforeHeight),
        clientHeight: Math.round(beforeClient),
        maxTarget: Math.round(Math.max(0, beforeHeight - beforeClient)),
        taggedTop,
        isAtBottomState,
        escapedFromLockState,
        pauseDepth,
        isNearBottomState,
      }));
    }
    // Restore LAST: the style write dirties style state, so keeping it
    // after every layout read above avoids forcing an extra recalc
    // mid-sequence on the (rare) suppression path.
    if (suppressScrollBehavior) scrollEl.style.scrollBehavior = originalScrollBehavior;
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
    spring.clearStructuralAppend();
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

  // ===== Spring chase =====
  // Kinematics live in scroll/spring.ts. This wiring hands the spring its
  // geometry reads, the arrival-readback bookkeeping (controller-owned —
  // shared with notifyLiveContentMaybeGrew), and the single scrollTop
  // chokepoint. The controller keeps deciding WHEN a chase runs (resolver
  // decisions + intent handlers); the spring owns HOW it advances.

  // Normalized per-fire animation mode: the consumer's option is optional
  // and may return undefined; every decision site treats anything but
  // 'spring' as 'instant'.
  function animationModeNow(): 'spring' | 'instant' {
    return options.animationMode?.() === 'spring' ? 'spring' : 'instant';
  }

  const spring = createSpringChase({
    getScrollEl: () => scrollEl,
    isPaused: () => pauseDepth > 0,
    isAtBottom: () => isAtBottomState,
    isEscaped: () => escapedFromLockState,
    selectionActive: () => (scrollEl ? isSelectingInside(scrollEl) : false),
    targetScrollTop,
    scrollTopIsAtTarget,
    acceptedReadbackMatchesTarget,
    recordArrivalReadbackAcceptance,
    shouldWriteExactArrivalTarget,
    writeExactArrivalTarget,
    clearArrivalReadback,
    invalidateStaleArrivalReadback,
    writeScrollTop,
    animationMode: animationModeNow,
    prefersReducedMotion,
    forceNextSpringTickTrace,
  });

  // Snapshot of the flags the pure delivery resolver decides over.
  function resolverStateSnapshot(): ResolverState {
    return {
      isAtBottom: isAtBottomState,
      isNearBottom: isNearBottomState,
      escaped: escapedFromLockState,
      paused: pauseDepth > 0,
      warm,
      springActive: spring.isActive(),
      springStopRequested: spring.stopRequested(),
      sentinelEntryTarget: spring.sentinelTarget(),
    };
  }

  // virtua scroll-applier entry point (see the interface doc). virtua's
  // `$fixScrollJump` compensation — the one internal scrollTop write our
  // virtua config produces — no longer reaches the DOM directly: the
  // patched scroll-applier seam (patches/virtua@0.49.1.patch) hands it
  // here, making the controller the single scrollTop writer during
  // follow by construction (the property-descriptor gate that used to
  // arbitrate virtua's direct writes died with this routing; its
  // tier-by-tier regression history lives in the resolver's provenance
  // notes and scroll-contracts.md C10). Gathers the observation,
  // delegates the decision to the pure resolver, applies the one write
  // through the chokepoint. Detached: decline — the poke keeps virtua's
  // model synced to the DOM we are no longer arbitrating.
  function applyVirtuaScrollCompensation(target: number, jump: number, shift: boolean): boolean {
    if (!scrollEl) return false;
    const observation: VirtuaCompensationObservation = {
      target,
      jump,
      shift,
      scrollTop: scrollEl.scrollTop,
      bottomTarget: targetScrollTop(),
      clientHeight: scrollEl.clientHeight,
      widthReflowActive: contentReflowSettleUntil > nowMs(),
    };
    const decision = resolveVirtuaCompensation(resolverStateSnapshot(), observation);
    if (isUiRenderTraceEnabled()) trace('scroll.virtuaJump', () => ({
      target: Math.round(target),
      jump: Math.round(jump),
      shift,
      scrollTop: Math.round(observation.scrollTop),
      bottomTarget: Math.round(observation.bottomTarget),
      writeCaller: decision.write?.caller ?? null,
      writeValue: decision.write ? Math.round(decision.write.value) : null,
      springToken: spring.token(),
      warm,
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
    }));
    if (decision.write === null) return false;
    writeScrollTop(decision.write.caller, decision.write.value);
    return true;
  }

  // ===== Content RO =====
  function setupContentRO(): void {
    if (!contentEl) return;
    if (typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (!entry || !contentEl || !scrollEl) return;
      const nextHeight = entry.contentRect.height;
      const nextWidth = entry.contentRect.width;
      const prev = previousHeight;
      const prevWidth = previousWidth;
      previousHeight = nextHeight;
      previousWidth = nextWidth;
      const widthChanged = prevWidth !== undefined
        && Math.abs(nextWidth - prevWidth) > CONTENT_REFLOW_WIDTH_EPSILON_PX;
      if (widthChanged) {
        contentReflowSettleUntil = nowMs() + CONTENT_REFLOW_SETTLE_WINDOW_MS;
      }

      // Every RO activity counts as "still settling" — reset the quiet
      // timer regardless of delta direction. virtua's per-row
      // remeasurement, Streamdown's typesetting backfill, and
      // parseIncompleteMarkdown rebalance all fire multiple RO callbacks
      // in close succession during mount; we want warm to fire only
      // once they're done. The height delta keeps the shortened settle
      // window gated on geometry stability (undefined on the first fire,
      // which has no baseline — see bumpQuietTimer).
      bumpQuietTimer(prev === undefined ? undefined : nextHeight - prev);

      if (prev === undefined) {
        // First fire: snap to bottom synchronously so the initial paint
        // lands at the right place. Matches upstream's `initial` behavior
        // when isAtBottom starts true.
        const decision = resolveContentDelivery(resolverStateSnapshot(), {
          kind: 'first',
          target: targetScrollTop(),
        });
        if (isUiRenderTraceEnabled()) trace('scroll.contentRO.firstFire', () => ({
          nextHeight: Math.round(nextHeight),
          willPin: decision.write !== null,
          isAtBottomState,
          escapedFromLockState,
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
          scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
          clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        }));
        if (decision.write) {
          writeScrollTop(decision.write.caller, decision.write.value);
        }
        refreshIsNearBottom();
        return;
      }

      const delta = nextHeight - prev;
      const widthReflowActive = widthChanged || contentReflowSettleUntil > nowMs();
      // Common case: virtua re-measures a same-height row, padding-bottom
      // CSS variable updates with identical computed value, etc. No
      // geometry change → nothing to chase, no scroll-event tagging needed.
      if (delta === 0) {
        if (widthChanged && isUiRenderTraceEnabled()) trace('scroll.contentRO.widthReflow', () => ({
          prevWidth: prevWidth === undefined ? null : roundCssPx(prevWidth),
          nextWidth: roundCssPx(nextWidth),
          widthDelta: prevWidth === undefined ? null : roundCssPx(nextWidth - prevWidth),
          widthReflowActive,
          settleWindowMs: CONTENT_REFLOW_SETTLE_WINDOW_MS,
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
          scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
          clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        }));
        return;
      }
      resizeDifference = delta;
      // A browser scroll clamp (scrollHeight shrinking below
      // scrollTop + clientHeight, e.g. from virtua's row-height
      // corrections mutating styles in its own RO callbacks) emits one
      // untagged scroll event correlated with this content resize. The
      // timer/rAF `resizeDifference` guard catches the normal ordering;
      // this one-event budget covers the clear racing ahead of the
      // scroll handler. Pending user intent still wins. (virtua's own
      // compensation writes used to be the main untagged producer here;
      // those are now routed through the controller and token-tagged.)
      resizeCorrelatedUntaggedScrollBudget = 1;

      // Refresh the geometric near-bottom flag BEFORE resolving so the
      // decision and its trace see the same post-resize geometry.
      refreshIsNearBottom();
      // Cache the bottom target once per RO delivery. `targetScrollTop()`
      // reads `scrollHeight` + `clientHeight` (forced layout), and neither
      // changes across this synchronous callback — the only writes here
      // are to `scrollTop`, which don't affect them — so one read serves
      // the whole decision. Mirrors the spring tick's per-frame
      // `const target` discipline.
      const target = targetScrollTop();
      const scrollTopAtDelivery = scrollEl.scrollTop;
      // Every decision about this delivery — overshoot snap, stranded-
      // oscillation recovery, sync-pin vs spring, negative re-stick — is
      // made by the pure resolver (scroll/resolver.ts) over a sampled
      // snapshot; this callback only gathers observations and applies
      // effects. At most ONE scrollTop write leaves a delivery.
      const observation: ContentDeltaObservation = {
        kind: 'delta',
        delta,
        scrollTop: scrollTopAtDelivery,
        target,
        widthReflowActive,
        animationMode: animationModeNow(),
        structuralAppendPending: spring.structuralAppendPending(),
        prefersReducedMotion: prefersReducedMotion(),
      };
      const decision = resolveContentDelivery(resolverStateSnapshot(), observation);
      if (isUiRenderTraceEnabled()) trace('scroll.contentRO', () => ({
        prev: Math.round(prev),
        next: Math.round(nextHeight),
        delta: Math.round(delta),
        overshootMagnitude: Math.round(Math.max(0, scrollTopAtDelivery - target)),
        distanceFromTarget: Math.round(Math.abs(scrollTopAtDelivery - target)),
        writeCaller: decision.write?.caller ?? null,
        startSpring: decision.startSpring,
        bumpTargetChanged: decision.bumpTargetChanged,
        oscillationRecovery: decision.oscillationRecovery,
        setIsAtBottom: decision.setIsAtBottom,
        isAtBottomState,
        escapedFromLockState,
        pauseDepth,
        isNearBottomState,
        prevWidth: prevWidth === undefined ? null : roundCssPx(prevWidth),
        nextWidth: roundCssPx(nextWidth),
        widthDelta: prevWidth === undefined ? null : roundCssPx(nextWidth - prevWidth),
        widthChanged,
        widthReflowActive,
        structuralAppendSpringPending: observation.structuralAppendPending,
        scrollTop: Math.round(scrollTopAtDelivery),
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
        clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        target: Math.round(target),
      }));

      // Apply the decision. Order preserved from the legacy inline
      // logic: intent flip first (the write's trace payload reads it),
      // then the single write, then spring bookkeeping.
      if (decision.setIsAtBottom) isAtBottomState = true;
      if (decision.oscillationRecovery && decision.write) {
        // Route through the shared snap so this synchronous recovery and
        // the spring tick's rAF-side recovery cannot drift on the effect
        // body (see the FOOTGUN comment on spring.snapOscillationToBottom,
        // scroll/spring.ts). The
        // recovery runs here — same RO delivery as the regrow, before
        // paint — because rAF callbacks fire BEFORE ResizeObserver
        // callbacks within a frame, so the tick-side snap always lands
        // one frame late (bug-report-20260615T182227Z: ~37px one-frame
        // upward jolt on an above-viewport image row remeasure).
        spring.snapOscillationToBottom(decision.write.caller, decision.write.value);
      } else if (decision.write) {
        writeScrollTop(decision.write.caller, decision.write.value);
      }
      if (decision.startSpring) {
        spring.markTargetChanged();
        spring.start();
      } else if (decision.bumpTargetChanged) {
        // Spring is the single writer mid-chase and the sync write was
        // suppressed, but the target moved — without the bump the retain
        // window could lapse between chunks and the spring would
        // arrive-and-stop while a follow-up chunk was on its way.
        spring.markTargetChanged();
      }

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
  function targetIsInsideScrollEl(e: Event): boolean {
    if (!scrollEl) return false;
    let cur: Element | null = e.target instanceof Element ? e.target : null;
    while (cur) {
      if (cur === scrollEl) return true;
      cur = cur.parentElement;
    }
    return false;
  }

  function escapeFromUserInput(source: 'wheel' | 'key' | 'touch' | 'pointer'): void {
    if (!scrollEl) return;
    clearProgrammaticScrollState();
    if (source !== 'pointer') clearScrollbarDragSession();
    const targetScrollEl = scrollEl;
    if (!escapedFromLockState && isUiRenderTraceEnabled()) {
      trace('scroll.intent.escape', () => ({
        source,
        scrollTop: Math.round(targetScrollEl.scrollTop),
        isAtBottomState,
        escapedFromLockState,
      }));
    }
    setEscapedFromLock(true);
  }

  function handleWheel(e: WheelEvent): void {
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
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      isAtBottomState,
      escapedFromLockState,
    }));
    escapeFromUserInput('pointer');
    armScrollbarDragSession();
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
    const distFromBottomAtEvent = refreshIsNearBottom();
    // No tagged/externalTagged fields here: the tagged bail above means
    // this record only ever describes untagged (user-attributed) events.
    if (isUiRenderTraceEnabled()) trace('scroll.scrollEvent', () => ({
      scrollTop: Math.round(scrollTopAtEvent),
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      resizeDifference: Math.round(resizeDifference),
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
      isNearBottomState,
    }));
    const resizeCorrelatedScroll = resizeDifference !== 0 || resizeCorrelatedUntaggedScrollBudget > 0;
    if (resizeCorrelatedUntaggedScrollBudget > 0) resizeCorrelatedUntaggedScrollBudget -= 1;
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
      escapedFromLockState
      || mouseDown
      || scrollbarDragSessionActive;
    if (!shouldRunDeferredScrollIntentCheck) return;
    // Defer 1ms so a concurrent RO callback can update resizeDifference
    // before we interpret direction. Mirrors upstream.
    setTimeout(() => {
      if (!scrollEl) return;
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
          resizeDifference: Math.round(resizeDifference),
          resizeCorrelatedScroll,
          hasLiveDownIntent,
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        }));
        return;
      }

      if (isSelectingInside(scrollEl)) {
        if (isUiRenderTraceEnabled()) trace('scroll.scrollEvent.deferred.escapeSelection', () => ({
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        }));
        setEscapedFromLock(true);
        return;
      }

      const willRestick = hasLiveDownIntent
        && scrolledDown
        && escapedFromLockState
        && distFromBottomAtEvent <= AUTO_FOLLOW_BOTTOM_EPSILON_PX;
      if (isUiRenderTraceEnabled()) trace('scroll.scrollEvent.deferred', () => ({
        scrollTop: Math.round(scrollTopAtEvent),
        previousObserved: Math.round(previousObserved),
        scrolledDown,
        hasLiveRecentDownIntent,
        hasLiveScrollbarDragRestickIntent,
        hasLiveDownIntent,
        distFromBottomAtEvent: Math.round(distFromBottomAtEvent),
        distFromBottomNow: scrollEl ? Math.round(distanceFromBottom()) : null,
        willRestick,
        isAtBottomState,
        escapedFromLockState,
      }));
      if (willRestick) {
        restickFromUserInput('scroll');
      }
    }, RESIZE_CLEAR_PADDING_MS);
  }

  // ===== Keydown / touch handlers (intent signals) =====
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

  // ===== Public actions =====
  function setEscapedFromLock(next: boolean): void {
    if (!next && !scrollbarDragSessionActive) clearScrollbarDragSession();
    if (next) {
      clearRecentDownIntent();
      // User explicitly broke from auto-follow — bail any in-flight
      // spring chase. The tick observes the stop request + new state
      // and clears the token on the next frame, but we also cancel
      // here for the "no rAF before next attach" edge case.
      spring.requestStop();
      spring.cancel();
      // Clear any pending restore-snap consent: a fresh user escape
      // invalidates a yet-to-be-consumed restore-snap. armRestoreSnap
      // runs its defensive escape through here BEFORE arming, so this
      // clear is the right default — a stale consent left over from an
      // earlier path can't slip through, and a legitimate arm survives
      // because it is written after this clear.
      restoreSnapArmed = false;
    }
    if (escapedFromLockState === next) return;
    const previousIsAtBottom = isAtBottomState;
    escapedFromLockState = next;
    if (next) {
      isAtBottomState = false;
    }
    if (isUiRenderTraceEnabled()) trace('scroll.escape.set', () => ({
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
    // Defensive escape FIRST (it clears any prior arm), then arm. Folded
    // into one call so the two consumers (thread switch, channel initial
    // poll) cannot get the ordering wrong — see the interface doc for why
    // arm and consume must stay separate calls.
    setEscapedFromLock(true);
    restoreSnapArmed = true;
    if (isUiRenderTraceEnabled()) trace('scroll.restoreSnap.arm', () => ({
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
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
    if (scrollEl) {
      lastObservedScrollTopForRestick = scrollEl.scrollTop;
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
          writeScrollTop('preserveScrollAnchor', targetScrollEl.scrollTop + delta);
        }
      }
    } finally {
      release();
    }
    await actionPromise;
    if (actionError !== undefined) throw actionError;
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
      if (isUiRenderTraceEnabled()) trace('scroll.forceStick.skipRestore', () => ({
        reason,
        restoreSnapArmed,
        isAtBottomState,
        escapedFromLockState,
        pauseDepth,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      }));
      return;
    }
    restoreSnapArmed = false;
    clearRecentDownIntent();
    clearScrollbarDragSession();
    if (isUiRenderTraceEnabled()) trace('scroll.forceStick.entry', () => ({
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
    spring.cancel();
    // Reset the stop flag AFTER cancel — the spring tick observes the
    // stop request via the rAF guard, but the value at the current
    // synchronous frame doesn't affect cancellation (the token
    // mismatch on the next tick handles it). We clear it now so the
    // next streaming chunk can re-engage the spring.
    spring.clearStopRequest();
    // Only restore/thread-switch snaps should re-hide content for the
    // measurement warmup. A user click on the scroll-to-bottom chip is
    // an explicit visible action in an already-mounted thread; blanking
    // the transcript until the failsafe fires is worse than the small
    // chance of a post-snap measurement correction.
    if (reason === 'restore') beginWarmup();
    if (!scrollEl) return;
    isAtBottomState = true;
    writeScrollTop('forceStick', targetScrollTop());
  }

  function markAtBottom(): void {
    // Flag-only counterpart to forceStick: caller already established
    // bottom geometry, or there is no geometry yet because the
    // timeline is empty. Used by chat's restoreToBottom empty-timeline
    // branch — so the restore-snap consent must be consumed here too,
    // otherwise the arm leaks past a completed empty-thread restore
    // and admits a later stale restore-stick.
    if (isUiRenderTraceEnabled()) trace('scroll.markAtBottom', () => ({
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
    }));
    restoreSnapArmed = false;
    clearRecentDownIntent();
    clearScrollbarDragSession();
    setEscapedFromLock(false);
    isAtBottomState = true;
    refreshIsNearBottom();
  }

  function readNotifyContentGate(): {
    gateScrollEl: boolean;
    gateEscape: boolean;
    gatePause: boolean;
    gateNotAtBottom: boolean;
    canPin: boolean;
  } {
    const gateScrollEl = scrollEl !== undefined;
    const gateEscape = escapedFromLockState;
    const gatePause = pauseDepth > 0;
    const gateNotAtBottom = !isAtBottomState;
    const canPin = gateScrollEl
      && !gateEscape
      && !gatePause
      && !gateNotAtBottom;

    return {
      gateScrollEl,
      gateEscape,
      gatePause,
      gateNotAtBottom,
      canPin,
    };
  }

  function instantPinAfterExternalGeometryChange(caller: ScrollWriteCaller): void {
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
    writeScrollTop(caller, targetScrollTop());
  }

  function notifyContentMaybeGrew(): void {
    const gate = readNotifyContentGate();
    if (isUiRenderTraceEnabled()) trace('scroll.notifyContentMaybeGrew', () => ({
      willPin: gate.canPin,
      gateScrollEl: gate.gateScrollEl,
      gateEscape: gate.gateEscape,
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
    const willSpring = gate.canPin && warm && spring.gateOpen();
    if (isUiRenderTraceEnabled()) trace('scroll.notifyLiveContentMaybeGrew', () => ({
      canPin: gate.canPin,
      willSpring,
      gateScrollEl: gate.gateScrollEl,
      gateEscape: gate.gateEscape,
      gatePause: gate.gatePause,
      gateNotAtBottom: gate.gateNotAtBottom,
      pauseDepth,
      isNearBottomState,
      warm,
      structuralAppendSpringPending: spring.structuralAppendPending(),
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      target: scrollEl ? Math.round(targetScrollTop()) : null,
    }));
    if (!gate.canPin) return;

    const target = targetScrollTop();
    if (scrollEl && scrollTopIsAtTarget(target)) {
      if (shouldWriteExactArrivalTarget(target)) {
        writeExactArrivalTarget('notifyLiveContentMaybeGrew.arrive', target);
      }
      refreshIsNearBottom();
      return;
    }
    if (willSpring && scrollEl) {
      const current = scrollEl.scrollTop;
      if (target - current > ARRIVAL_DISTANCE_PX) {
        spring.markTargetChanged();
        spring.start();
        return;
      }
      const overshootMagnitude = current - target;
      if (
        overshootMagnitude > 0
        && spring.isActive()
        && overshootMagnitude <= SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX
      ) {
        // Match contentRO's spring policy: a small corrected-target
        // overshoot while the spring is already chasing should damp
        // through the symmetric spring, not snap via the structural
        // nudge's instant fallback.
        spring.markTargetChanged();
        return;
      }
    }

    // Same instant fallback as notifyContentMaybeGrew for non-spring
    // modes, warm-up, reduced-motion users, and no-distance/overshoot
    // nudges where a spring has nothing useful to chase.
    instantPinAfterExternalGeometryChange('notifyLiveContentMaybeGrew');
  }

  // Public observation entry point — maps the source hint onto the two
  // internal notify paths (see ScrollObservationKind). 'host-layout'
  // takes the instant path here; MessageTimeline's pane adapter
  // intercepts that kind before it reaches the controller. Exhaustive
  // switch so adding a kind forces a deliberate routing decision
  // instead of silently falling through to the instant path.
  function observe(kind: ScrollObservationKind): void {
    switch (kind) {
      case 'live-content':
      case 'composer-geometry':
        notifyLiveContentMaybeGrew();
        return;
      case 'content':
      case 'host-layout':
        notifyContentMaybeGrew();
        return;
      default:
        kind satisfies never;
    }
  }

  function pauseAutoScroll(): () => void {
    pauseDepth += 1;
    if (isUiRenderTraceEnabled()) trace('scroll.pause.acquire', () => ({
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
        && isAtBottomState;
      if (isUiRenderTraceEnabled()) trace('scroll.pause.release', () => ({
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
        writeScrollTop('pauseAutoScroll.release', targetScrollTop());
      }
    };
  }

  // ===== Lifecycle =====
  function attach(nextScrollEl: HTMLElement, nextContentEl: HTMLElement): void {
    if (scrollEl === nextScrollEl && contentEl === nextContentEl) return;
    detach();
    scrollEl = nextScrollEl;
    contentEl = nextContentEl;
    beginWarmup();
    if (isUiRenderTraceEnabled()) trace('scroll.attach', () => ({
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
    detachWheel = () => nextScrollEl.removeEventListener('wheel', handleWheel);
    detachScroll = () => nextScrollEl.removeEventListener('scroll', handleScroll);
    detachPointer = () => nextScrollEl.removeEventListener('pointerdown', handlePointerDown);
    detachKeyTouch = () => {
      nextScrollEl.removeEventListener('keydown', handleKeydown);
      nextScrollEl.removeEventListener('touchstart', handleTouchStart);
      nextScrollEl.removeEventListener('touchmove', handleTouchMove);
      nextScrollEl.removeEventListener('touchend', handleTouchEnd);
      nextScrollEl.removeEventListener('touchcancel', handleTouchEnd);
    };
    lastObservedScrollTopForRestick = nextScrollEl.scrollTop;
    refreshIsNearBottom();
    installStickStateDevHook();
  }

  // Dev-only window hook so a user reproducing a sticky-scroll bug can
  // dump the controller's full internal state in one console call.
  // Active only when uiRenderTrace is enabled (DEBUG=1 build flag) so
  // production builds carry no surface. Each attach() reinstalls the
  // hook against the current closure so multiple panes don't fight; the
  // most recently attached controller wins, which matches the trace
  // semantics. Returns a structured snapshot — copy/paste-friendly for
  // pasting into bug reports.
  function installStickStateDevHook(): void {
    if (typeof window === 'undefined' || !isUiRenderTraceEnabled()) return;
    const hook = () => {
      const dist = scrollEl
        ? scrollEl.scrollHeight - scrollEl.scrollTop - scrollEl.clientHeight
        : null;
      return {
        // Core decision flags — these are the gates that determine whether
        // contentRO positive-delta will pin and whether the spring can start.
        isAtBottomState,
        escapedFromLockState,
        isNearBottomState,
        pauseDepth,
        warm,
        springStopRequested: spring.stopRequested(),
        springToken: spring.token(),
        recentDownIntentActive: hasRecentDownIntent(),
        recentDownIntentUntil: Math.round(recentDownIntentUntil),
        scrollbarDragSessionActive,
        scrollbarDragSessionVersion,
        // Restore-snap consent (consumed by forceStick({reason:'restore'})).
        restoreSnapArmed,
        // Geometry snapshot for cross-referencing the trace.
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
        clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        distanceFromBottom: dist === null ? null : Math.round(dist),
        target: scrollEl ? Math.round(targetScrollTop()) : null,
        // Public getters as the consumer would see them — so the dump
        // makes "isAtBottom returned the wrong value" diagnoses obvious.
        publicIsSticky:
          isAtBottomState && !escapedFromLockState && pauseDepth === 0,
        publicIsAtBottom: !escapedFromLockState && (isAtBottomState || isNearBottomState),
      };
    };
    stickStateDevHook = hook;
    window.__stickState = hook;
  }

  function uninstallStickStateDevHook(): void {
    if (typeof window === 'undefined' || !stickStateDevHook) return;
    if (window.__stickState === stickStateDevHook) {
      delete window.__stickState;
    }
    stickStateDevHook = undefined;
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
    uninstallStickStateDevHook();
    if (resizeClearTimer) {
      clearTimeout(resizeClearTimer);
      resizeClearTimer = null;
    }
    if (externalScrollClearTimer) {
      clearTimeout(externalScrollClearTimer);
      externalScrollClearTimer = null;
    }
    externalScrollIgnoreUntil = 0;
    clearProgrammaticScrollState();
    clearRecentDownIntent();
    clearScrollbarDragSession();
    // spring.cancel() also resets the target-change timestamp and
    // sentinel state; only the stop request needs a separate clear.
    spring.cancel();
    spring.clearStopRequest();
    clearWarmupTimers();
    warm = false;
    resizeDifference = 0;
    resizeCorrelatedUntaggedScrollBudget = 0;
    previousHeight = undefined;
    previousWidth = undefined;
    contentReflowSettleUntil = 0;
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
    scrollEl = undefined;
    contentEl = undefined;
  }

  return {
    get isSticky() {
      return isAtBottomState && !escapedFromLockState && pauseDepth === 0;
    },
    get isAtBottom() {
      return !escapedFromLockState && (isAtBottomState || isNearBottomState);
    },
    get escapedFromLock() {
      return escapedFromLockState;
    },
    get isWarm() {
      return warm;
    },
    pauseAutoScroll,
    observe,
    markStructuralContentPending: spring.markStructuralAppend,
    preserveScrollAnchor,
    attach,
    detach,
    forceStick,
    markAtBottom,
    runExternalScroll,
    setEscapedFromLock,
    armWarmup: beginWarmup,
    skipWarmup(): void {
      if (!warm) {
        warm = true;
        clearWarmupTimers();
      }
    },
    notifyQuietContextSignalChanged,
    armRestoreSnap,
    applyVirtuaScrollCompensation,
  };
}
