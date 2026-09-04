// Sticky-bottom controller, shared by chat MessageTimeline and
// Discussion ChannelView.
//
// Port of stackblitz-labs/use-stick-to-bottom adapted to Svelte 5. Owns
// the user's intent ("glued to bottom" or "free") and the content-
// geometry pipeline, fed by one of two sources: a single ResizeObserver
// on the content element (the default — ChannelView), or engine-sourced
// samples via `deliverContentGeometry` when the consumer sets
// `externalContentGeometry` (chat — see observers.ts).
//
// Autonomous content growth while pinned at the bottom has ONE
// behavior: a velocity-spring chase. The viewport interpolates toward
// the moving bottom across rAF ticks, so streaming chunks, the
// end-of-turn drain, a late-settling row and a background completion
// all flow in the same way. Both routes would end at the same
// scrollTop — the destination is the bottom whatever moved it — so the
// route is chosen once, and the glide is the readable one. The decision
// takes no guess at what produced the growth; the cases that must not
// animate carry their own signal and their own gate:
//
//   - quiescence-based warm state: growth stays sync-pinned until
//     contentRO has been quiet for QUIET_MS or the FAILSAFE_MS deadline
//     trips. This is what keeps a thread restore from chasing its mount
//     cascade (the 80LoC-spring-delete regression, commit e00723f).
//   - width reflow: a content-column width change makes every row
//     re-wrap; the paired height delta is layout correction for
//     already-rendered content, not the bottom advancing, so it
//     sync-pins (see observers.ts's settle window).
//   - reduced motion (OS preference or the app's low-power setting).
//   - the idle re-pin deadband, which suppresses the sub-pixel
//     content-box wobble entirely rather than choosing a route for it.
//
// Programmatic placements are a separate concern and stay instant by
// construction: they go through forceStick / applyScrollTarget, not the
// growth path.
//
// User-initiated snaps (the scroll-to-bottom chip) and thread restores
// go through `forceStick()` which writes scrollTop directly.
// Restore snaps also reset the warm gate so post-thread-switch
// measurement settling stays silent; user snaps keep already-settled
// content visible.
//
// Unlike the previous controller, this owns the scroll element directly.
// MessageTimeline pairs it with <TimelineVirtualizer scrollRef={scrollEl}>
// so the windowing engine does its measurement work without owning the
// scroll container. ChannelView has no virtualizer — the contentEl is
// just a `<div>` wrapping the `{#each}` over channel messages — and the
// same controller works because the algorithm is agnostic to what's
// inside contentEl.
//
// External consumers (sidebar resizers, ChatView composer-height
// publication, scrollLeaseDuringTransition helper) speak to this through
// the PaneScrollController interface — pauseAutoScroll() returns a
// depth-counted lease, observe(kind) reports geometry changes outside
// the content element. Both ChatView and ChannelView observe
// 'composer-geometry' from their composer ResizeObservers when
// out-of-content height changes; the seam is identical on both surfaces.

import { tick } from 'svelte';
import { motionReduced } from '../reducedMotion';
import { isUiRenderTraceEnabled } from '../uiRenderTrace';
import { reportFrontendDiagnostic } from '../frontendErrorCapture';
import { createScrollIntent, isSelectingInside } from './intent';
import { createContentObserver } from './observers';
import type { EngineCompensation } from '../virtual/types';
import {
  ARRIVAL_DISTANCE_PX,
  AUTO_FOLLOW_BOTTOM_EPSILON_PX,
  SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX,
  resolveEngineCompensation,
  withinArrivalBand,
  type EngineCompensationObservation,
  type ResolverState,
} from './resolver';
import { createWriteChokepoint } from './chokepoint';
import { createSpringChase, type SpringWriteRefusalEvent } from './spring';
import { nowMs } from './time';
import { trace } from './trace';
import type {
  RequestBottomOptions,
  ScrollObservationKind,
  ScrollWriteCaller,
  UseStickToBottomController,
  UseStickToBottomOptions,
  WarmReason,
} from './types';

// Public types re-exported so consumers import the controller and its
// contract from one place; the definitions live in ./types.
export type {
  RequestBottomOptions,
  RequestBottomTakeover,
  ScrollObservationKind,
  UseStickToBottomController,
  UseStickToBottomOptions,
  WarmReason,
} from './types';

// Three-band geometry — see docs/architecture/frontend-scroll.md for
// the full rationale. Tightening any one of these affects a
// different UX surface; the asymmetry is load-bearing.
//
// Visual near-bottom band: drives the scroll-to-bottom chip and the
// negative-delta repin's geometric branch. Loose so a user within 70px
// doesn't see the chip flicker.
const STICK_TO_BOTTOM_OFFSET_PX = 70;
// AUTO_FOLLOW_BOTTOM_EPSILON_PX (the down-scroll re-stick band) lives in
// scroll/resolver.ts — the engine compensation resolver shares it for the
// anchor-redirect "already pinned" tolerance.
// The idle re-pin deadband (IDLE_REPIN_DEADBAND_PX, scroll/resolver.ts)
// deliberately equals AUTO_FOLLOW_BOTTOM_EPSILON_PX — "close enough to
// count as at-bottom" and "close enough not to fight a fractional-DPR
// wobble" are the same tolerance.
// Intent-classification windows (down-intent, drag sessions, the
// programmatic token ring, RESIZE_CLEAR_PADDING_MS) live in
// scroll/intent.ts; the content-reflow and warm-up (quiescence) tuning
// lives in scroll/observers.ts.

// Spring kinematics, tuning constants, and the sentinel/structural-append
// windows live in scroll/spring.ts. The write chokepoint and its
// satellites (provenance ledger, arrival readback, spring-tick trace
// sampling) live in scroll/chokepoint.ts.

// Frontend-errors rate limit for the spring's write-refusal guard
// promotion: the first latch per window files a persisted diagnostic
// (plus that latch's matching bookend); later episodes inside the
// window still trace. Per controller, deliberately — the diagnostic
// question is "which SURFACE is wedged", and a second controller's
// first latch is new evidence, not a duplicate.
const WRITE_REFUSAL_DIAGNOSTIC_COOLDOWN_MS = 10_000;

export function createUseStickToBottomController(
  options: UseStickToBottomOptions = {},
): UseStickToBottomController {
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
  // Intent state (down-intent windows, drag sessions, the programmatic
  // token ring, restore-snap consent) lives in the scroll/intent.ts
  // machine created below.
  let scrollEl: HTMLElement | undefined;
  let contentEl: HTMLElement | undefined;
  let stickStateDevHook: (() => Record<string, unknown>) | undefined;

  // ===== Warm-up (quiescence) state =====
  // `warm` flips true once the observer pipeline (scroll/observers.ts,
  // which owns the gate's timers and tuning) sees a quiet period of
  // QUIET_MS on contentRO, OR the FAILSAFE_MS deadline trips (whichever
  // comes first). Reset to false on attach, explicit armWarmup(), and
  // restore-reason forceStick. Backed by $state so consumers can
  // subscribe to the transition — chat's MessageTimeline hides contentEl
  // while warm is false, which is the canonical
  // "the measurement cascade has settled" signal. Without this,
  // an uncached thread's first paint renders rows at estimated
  // ESTIMATED_ROW_SIZE × N offsets; the RO-correction pass then shifts
  // every row by a fraction-of-a-page (the larger the thread, the
  // bigger the shift) producing the visible "lands wrong, then jumps"
  // regression.
  let warm = $state(false);
  // Reporting-only counterpart to `warm`: which path last opened the
  // gate (see `WarmReason`). Driven by the same observer pipeline via
  // `setWarmReason`; no decision in this controller reads it back.
  let warmReason: WarmReason = $state(null);
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
    const target = Math.max(0, scrollEl.scrollHeight - scrollEl.clientHeight);
    if (options.externalContentGeometry === true) {
      cachedBottomTarget = target;
      cachedBottomTargetValid = true;
    }
    return target;
  }

  function scrollTopIsAtTarget(target: number): boolean {
    return !scrollEl || withinArrivalBand(scrollEl.scrollTop, target);
  }

  function distanceFromBottom(): number {
    if (!scrollEl) return 0;
    return scrollEl.scrollHeight - scrollEl.scrollTop - scrollEl.clientHeight;
  }

  // ===== Read-free content geometry =====
  // scrollHeight/scrollTop/clientHeight are layout-dependent reads: the
  // first one after a style write forces a synchronous layout pass, and
  // the content-delivery path used to pay one per delivery — interleaved
  // with other panes' flush writes, up to four forced passes landed in a
  // single 3-pane streaming frame (storm capture 2026-08-26). But a
  // delivery already has the geometry. The virtualizer sample carries
  // both the total content height and the scroller's content-box viewport
  // height, so its bottom target is `height - viewportHeight`. Padding is
  // present in both DOM scrollHeight and clientHeight and cancels from
  // their difference. The hot path therefore needs no learned target and
  // reads no layout.
  //
  // Cached scrollTop and the external source's absolute bottom target are
  // separate facts. Every path that moves scrollTop resyncs the first. Every
  // virtualizer sample publishes `height - viewportHeight` as the second.
  // Ordinary spring frames can then advance using scrollTop readback plus the
  // published target without re-reading scrollHeight/clientHeight at display
  // rate. Sentinel clamp detection and write-refusal retries force the real
  // target read. A viewport change or width reflow still takes the read path
  // once so a browser clamp that raced the delivery cannot leave cached
  // scrollTop stale. RO-sourced pipelines such as ChannelView have no
  // viewport sample and always take the real-read path.
  //
  // A resync taken while FLOORED (scrollHeight clamped to clientHeight)
  // cannot prove whether a browser clamp raced it, so short threads keep
  // real reads until scrollable content returns. Those are the panes where
  // the read is cheap.
  let cachedScrollTop = 0;
  let cachedGeometryValid = false;
  let cachedGeometryFloored = false;
  let cachedBottomTarget = 0;
  let cachedBottomTargetValid = false;

  function setIsNearBottomFromDistance(dist: number): void {
    const next = dist <= STICK_TO_BOTTOM_OFFSET_PX;
    if (next !== isNearBottomState) isNearBottomState = next;
  }

  function cacheExternalBottomTarget(height: number, viewportHeight: number): void {
    const target = height - viewportHeight;
    if (!Number.isFinite(target)) {
      cachedBottomTargetValid = false;
      return;
    }
    cachedBottomTarget = Math.max(0, target);
    cachedBottomTargetValid = true;
  }

  function springTargetScrollTop(forceLayoutRead = false): number {
    if (
      !forceLayoutRead
      && options.externalContentGeometry === true
      && cachedBottomTargetValid
    ) {
      return cachedBottomTarget;
    }
    return targetScrollTop();
  }

  function springCurrentScrollTop(forceLayoutRead = false): number {
    if (!scrollEl) return 0;
    if (
      !forceLayoutRead
      && options.externalContentGeometry === true
      && cachedGeometryValid
    ) {
      return cachedScrollTop;
    }
    const scrollTop = scrollEl.scrollTop;
    cachedScrollTop = scrollTop;
    cachedGeometryValid = true;
    return scrollTop;
  }

  function refreshIsNearBottom(): number {
    let dist = 0;
    if (scrollEl) {
      const scrollTop = scrollEl.scrollTop;
      const scrollHeight = scrollEl.scrollHeight;
      const clientHeight = scrollEl.clientHeight;
      dist = scrollHeight - scrollTop - clientHeight;
      cachedScrollTop = scrollTop;
      cachedGeometryValid = true;
      cachedGeometryFloored = scrollHeight <= clientHeight;
    }
    // No scrollEl (empty timeline) still refreshes the band from dist 0
    // — markAtBottom's empty-thread branch relies on it.
    setIsNearBottomFromDistance(dist);
    return dist;
  }

  function refreshIsNearBottomAfterWrite(
    caller: ScrollWriteCaller,
    scrollTop: number,
  ): number {
    if (
      options.externalContentGeometry === true
      && cachedBottomTargetValid
      && caller.startsWith('spring.')
    ) {
      cachedScrollTop = scrollTop;
      cachedGeometryValid = true;
      const dist = cachedBottomTarget - scrollTop;
      setIsNearBottomFromDistance(dist);
      return dist;
    }
    return refreshIsNearBottom();
  }

  /**
   * Read-free decision inputs for an authoritative virtualizer sample,
   * or null when cached scrollTop cannot be trusted. The caller then
   * takes the real-read path. Also refreshes the near-bottom band from
   * the same arithmetic, mirroring refreshIsNearBottom.
   */
  function contentGeometryForSample(
    height: number,
    viewportHeight: number,
  ): { target: number; scrollTop: number } | null {
    if (!scrollEl || !cachedGeometryValid || cachedGeometryFloored) {
      return null;
    }
    const target = Math.max(0, height - viewportHeight);
    const next = target - cachedScrollTop <= STICK_TO_BOTTOM_OFFSET_PX;
    if (next !== isNearBottomState) isNearBottomState = next;
    return { target, scrollTop: cachedScrollTop };
  }

  // ===== Write chokepoint =====
  // The single programmatic-write site and its satellites — the
  // provenance ledger, arrival-readback acceptance, spring-tick trace
  // sampling live in scroll/chokepoint.ts as one unit. The late-bound
  // dep closes over `intent` (created below) and is only invoked at
  // write time.
  const chokepoint = createWriteChokepoint({
    getScrollEl: () => scrollEl,
    scrollTopIsAtTarget,
    refreshIsNearBottom: refreshIsNearBottomAfterWrite,
    noteProgrammaticWrite: (top) => {
      intent.noteProgrammaticWrite(top);
      options.onScrollTopWritten?.(top);
    },
    traceState: () => ({
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
      isNearBottomState,
    }),
  });
  const {
    writeScrollTop,
    scrollTopUnexplained,
    arrivalReadback,
    forceNextSpringTickTrace,
  } = chokepoint;
  // OS reduced-motion OR the app's low-power setting, both meaning
  // "place instantly, never spring-glide" — see utils/reducedMotion.ts,
  // the shared gate every JS motion site rides.

  // ===== Spring chase =====
  // Kinematics live in scroll/spring.ts. This wiring hands the spring its
  // geometry reads, the arrival-readback bookkeeping (controller-owned —
  // shared with notifyLiveContentMaybeGrew), and the single scrollTop
  // chokepoint. The controller keeps deciding WHEN a chase runs (resolver
  // decisions + intent handlers); the spring owns HOW it advances.

  // Normalized per-fire liveness read: the consumer's option is optional
  // and may return undefined; a surface that supplies none is treated as
  // never live (sentinel ends at arrival, nudges take the instant path).
  // This does NOT gate whether growth animates — see resolver.ts
  // springGateIsOpen.
  function liveContentActiveNow(): boolean {
    return options.liveContentActive?.() === true;
  }

  // Write-refusal promotion (spring.ts, "Write-refusal guard"). The
  // spring reports the kinematic facts; this handler owns the element
  // diagnostics — computed overflow and connectedness are exactly the
  // discriminating facts bug-report-20260818T003129Z could not answer,
  // so a recurrence of the wedge becomes its own root-cause capture.
  // The dev trace records every transition; the frontend-errors
  // diagnostic (which persists in production) is rate-limited per
  // controller so a hypothetical accept/refuse flapping element cannot
  // overwhelm the error log — episodes inside the cooldown still trace.
  let lastRefusalDiagnosticAt = -Infinity;
  let refusalLatchDiagnosed = false;
  function reportWriteRefusal(event: SpringWriteRefusalEvent): void {
    const el = scrollEl;
    const at = nowMs();
    // Which transitions file a persisted diagnostic: the first latch
    // per cooldown window, and that latch's matching bookend ('healed'
    // or 'abandoned'). A cooldown-suppressed latch leaves the pairing
    // flag ALONE — the flag pairs the DIAGNOSED latch with its
    // bookend, and clearing it here would swallow that bookend.
    const wantDiagnostic =
      event.phase === 'latched'
        ? at - lastRefusalDiagnosticAt >= WRITE_REFUSAL_DIAGNOSTIC_COOLDOWN_MS
        : refusalLatchDiagnosed;
    const traceEnabled = isUiRenderTraceEnabled();
    if (!traceEnabled && !wantDiagnostic) return;
    // DOM diagnostics (computed style is a forced style recompute) only
    // when something below will actually record them.
    const surface = el ? (el.dataset.testid || el.id || 'unknown') : 'detached';
    let overflowY = '';
    let scrollBehavior = '';
    let display = '';
    let position = '';
    let connected = false;
    if (el) {
      connected = el.isConnected;
      try {
        const cs = getComputedStyle(el);
        overflowY = cs.overflowY;
        scrollBehavior = cs.scrollBehavior;
        display = cs.display;
        position = cs.position;
      } catch {
        overflowY = 'unreadable';
      }
    }
    if (traceEnabled) trace('scroll.writeRefusal', () => ({
      phase: event.phase,
      consecutiveRefusals: event.consecutiveRefusals,
      requested: Math.round(event.requested),
      scrollTop: Math.round(event.scrollTop),
      target: Math.round(event.target),
      wedgeMs: event.wedgeMs,
      scrollHeight: el ? Math.round(el.scrollHeight) : null,
      clientHeight: el ? Math.round(el.clientHeight) : null,
      overflowY,
      scrollBehavior,
      display,
      position,
      connected,
      surface,
    }));
    if (!wantDiagnostic) return;
    // reportFrontendDiagnostic contract: the message is CONSTANT (it is
    // the dedupe key) and every variable rides in `detail`, paired
    // with a console.warn carrying the same facts — a non-loopback
    // session cannot persist the record, so the console line is then the
    // only surviving evidence (utils/frontendErrorCapture.ts).
    const detail =
      `surface ${surface}; refused ${event.consecutiveRefusals}; `
      + `wrote ${Math.round(event.requested)}; stayed ${Math.round(event.scrollTop)}; `
      + `target ${Math.round(event.target)}; overflow-y "${overflowY}"; `
      + `behavior "${scrollBehavior}"; display "${display}"; position "${position}"; `
      + `connected ${connected}; wedgeMs ${event.wedgeMs}`;
    let message: string;
    if (event.phase === 'latched') {
      lastRefusalDiagnosticAt = at;
      refusalLatchDiagnosed = true;
      message =
        'scroll: element is refusing scrollTop writes — spring re-anchored and backed off '
        + 'to retry cadence (utils/scroll/spring.ts write-refusal guard)';
    } else if (event.phase === 'healed') {
      refusalLatchDiagnosed = false;
      message =
        'scroll: write-refusing element healed — chase resumed as a bounded glide';
    } else {
      refusalLatchDiagnosed = false;
      message =
        'scroll: chase cancelled while element was still refusing writes — no heal observed';
    }
    console.warn(`[scroll] ${message} — ${detail}`);
    reportFrontendDiagnostic(message, detail);
  }

  const spring = createSpringChase({
    getScrollEl: () => scrollEl,
    isPaused: () => pauseDepth > 0,
    isAtBottom: () => isAtBottomState,
    isEscaped: () => escapedFromLockState,
    selectionActive: () => (scrollEl ? isSelectingInside(scrollEl) : false),
    targetScrollTop: springTargetScrollTop,
    currentScrollTop: springCurrentScrollTop,
    scrollTopIsAtTarget,
    arrival: arrivalReadback,
    writeScrollTop,
    liveContentActive: liveContentActiveNow,
    prefersReducedMotion: motionReduced,
    devicePixelRatio: () => window.devicePixelRatio,
    forceNextSpringTickTrace,
    scrollTopUnexplained,
    reportWriteRefusal,
  });

  // ===== Content observation pipeline =====
  // The content-geometry deliveries (contentEl ResizeObserver by default,
  // engine-sourced samples under `externalContentGeometry`), warm-up
  // (quiescence) gate, and resize-classification state live in
  // scroll/observers.ts. Each delivery is gathered there, decided by the
  // pure resolver, and applied through this controller's write
  // chokepoint and spring.
  const observers = createContentObserver({
    getScrollEl: () => scrollEl,
    getContentEl: () => contentEl,
    hasExternalGeometrySource: () => options.externalContentGeometry === true,
    liveContentActive: liveContentActiveNow,
    getQuietContextSignal: () => options.quietContextSignal,
    warm: () => warm,
    setWarm: (next) => {
      warm = next;
    },
    setWarmReason: (next) => {
      warmReason = next;
    },
    isAtBottom: () => isAtBottomState,
    setIsAtBottom: (next) => {
      isAtBottomState = next;
    },
    escaped: () => escapedFromLockState,
    pauseDepth: () => pauseDepth,
    isNearBottom: () => isNearBottomState,
    targetScrollTop,
    refreshIsNearBottom,
    viewportGeometryChanged: notifyLiveContentMaybeGrew,
    cacheExternalBottomTarget,
    contentGeometryForSample,
    writeScrollTop,
    resolverStateSnapshot,
    prefersReducedMotion: motionReduced,
    contentGeometryProcessed: (scrollable) =>
      options.onContentGeometryProcessed?.(scrollable),
    spring,
  });

  // ===== Intent machine =====
  // Escape / re-stick / restore-snap consent, down-intent and drag-session
  // windows, and user-vs-programmatic scroll classification live in
  // scroll/intent.ts. The reactive flags stay here (templates subscribe);
  // the machine flips them through the accessor pair.
  const intent = createScrollIntent({
    getScrollEl: () => scrollEl,
    isAtBottom: () => isAtBottomState,
    setIsAtBottom: (next) => {
      isAtBottomState = next;
    },
    escaped: () => escapedFromLockState,
    setEscaped: (next) => {
      escapedFromLockState = next;
    },
    isNearBottom: () => isNearBottomState,
    pauseDepth: () => pauseDepth,
    distanceFromBottom,
    refreshIsNearBottom,
    spring,
    sampleResizeCorrelation: observers.sampleResizeCorrelation,
    resizeDifferenceNow: observers.resizeDifferenceNow,
    noteUserScroll: chokepoint.noteUserScroll,
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
      structuralAppendPending: spring.structuralAppendPending(),
      sentinelEntryTarget: spring.sentinelTarget(),
      sentinelClampWitnessed: spring.sentinelClampWitnessed(),
    };
  }

  // Engine compensation entry point (see the interface doc). The bespoke
  // timeline engine (utils/virtual/) reports above-viewport remeasures
  // and head splices as {kind, delta, target} observations instead of
  // writing scrollTop itself (TimelineVirtualizer `onCompensation`),
  // making the controller the single scrollTop writer during follow by
  // construction (the property-descriptor gate that used to arbitrate
  // virtua's direct writes died with the routing this seam grew out of;
  // its tier-by-tier regression history lives in the resolver's
  // provenance notes and scroll-contracts.md C10). Gathers the
  // observation, delegates the decision to the pure resolver, applies
  // the one write through the chokepoint. Detached (no scrollEl): drop —
  // an unapplied compensation cannot desync the engine (its offset
  // follows real scroll events).
  function applyEngineCompensation(compensation: EngineCompensation): boolean {
    if (!scrollEl) return false;
    const observation: EngineCompensationObservation = {
      kind: compensation.kind,
      target: compensation.target,
      scrollTop: scrollEl.scrollTop,
      bottomTarget: targetScrollTop(),
    };
    const decision = resolveEngineCompensation(resolverStateSnapshot(), observation);
    if (isUiRenderTraceEnabled()) trace('scroll.engineCompensation', () => ({
      kind: compensation.kind,
      target: Math.round(compensation.target),
      delta: Math.round(compensation.delta),
      scrollTop: Math.round(observation.scrollTop),
      bottomTarget: Math.round(observation.bottomTarget),
      writeCaller: decision.write.caller,
      writeValue: Math.round(decision.write.value),
      springToken: spring.token(),
      warm,
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
    }));
    writeScrollTop(decision.write.caller, decision.write.value);
    // A DISPLACED redirect — the anchor-redirect tier recovering a
    // pinned viewport that a large post-warm remeasure burst threw off
    // the bottom (bug-report-20260822T020840Z) — proves a measurement-
    // correction wave is in flight. Open the settle window so the wave's
    // trailing height deltas classify as layout correction (sync-pin,
    // never glide). An ordinary already-pinned redirect (displacement
    // within the epsilon, the common streaming case) opens nothing:
    // gating on real displacement is what keeps live-follow glides
    // untouched.
    if (
      decision.write.caller === 'engine.anchorRedirect'
      && observation.bottomTarget - observation.scrollTop > AUTO_FOLLOW_BOTTOM_EPSILON_PX
    ) {
      observers.openPinnedRemeasureSettleWindow();
    }
    // A head splice displaces scrollTop with the content height (and so
    // the bottom target) unchanged — the exact numeric shape of a
    // browser clamp after a dip-restore. No special-casing is needed:
    // the write above updated the provenance ledger, so the sentinel's
    // oscillation guards see an EXPLAINED position, find no clamp
    // evidence, and glide the splice's hidden growth in instead of
    // snapping it (bug-report-20260801T213259Z).
    return true;
  }

  // Virtualizer-requested imperative scrolls (the scrollToIndex
  // convergence passes) — performed here so every write is
  // chokepoint-tagged; intent semantics (escape vs preserve) stay with
  // the navigation call sites.
  function applyScrollTarget(top: number): void {
    writeScrollTop('virtualizer.scrollTarget', top);
  }


  // ===== Public actions =====
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
    if (reason === 'restore' && !intent.restoreConsentArmed()) {
      if (isUiRenderTraceEnabled()) trace('scroll.forceStick.skipRestore', () => ({
        reason,
        restoreSnapArmed: intent.restoreConsentArmed(),
        isAtBottomState,
        escapedFromLockState,
        pauseDepth,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      }));
      return;
    }
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
    // Only restore/thread-switch snaps should re-hide content for the
    // measurement warmup. A user click on the scroll-to-bottom chip is
    // an explicit visible action in an already-mounted thread; blanking
    // the transcript until the failsafe fires is worse than the small
    // chance of a post-snap measurement correction.
    if (reason === 'restore') observers.beginWarmup();
    // The cancel-and-place choreography (intent sweep, escape clear,
    // spring cancel + stop-flag reset, bottom write) exists once, on
    // the claim path. Pre-attach (no scrollEl) this still claims bottom
    // intent via markAtBottom's flag-only path, so a forceStick before
    // the surface mounts follows on attach instead of landing unstuck.
    requestBottom({ takeover: 'claim', caller: 'forceStick' });
  }

  // Bottom-edge arbitration (see the interface doc in ./types.ts). The
  // one rule every out-of-band bottom placement now shares: while the
  // bottom-follow program is engaged, a system-initiated request hands
  // the trip to it; user intent always may retarget. The program
  // predicate is exactly `autoScrollInFlight()` — a glide running, or a
  // structural-append arm holding one ready to start (the armed gap
  // before the spring's first frame is still the reader's animation).
  function requestBottom(opts: RequestBottomOptions): void {
    const programEngaged = spring.isActive() || spring.structuralAppendPending();
    if (isUiRenderTraceEnabled()) trace('scroll.requestBottom', () => ({
      takeover: opts.takeover,
      caller: opts.caller ?? null,
      customWrite: opts.write !== undefined,
      programEngaged,
      springActive: spring.isActive(),
      structuralAppendPending: spring.structuralAppendPending(),
      isAtBottomState,
      escapedFromLockState,
      pauseDepth,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      target: scrollEl ? Math.round(targetScrollTop()) : null,
    }));
    if (opts.takeover === 'yield') {
      // No system-initiated placement may move an escaped viewport —
      // the gate lives HERE, not in caller discipline, so a future
      // caller that forgets to check cannot yank an escaped reader (or
      // corrupt the intent pair by setting isAtBottomState under a
      // standing escape).
      if (escapedFromLockState) return;
      if (programEngaged) {
        // The program owns the trip. The live-content path re-reads the
        // moved bottom and lets the chase (or the armed spring) close
        // the remaining distance — and its own gates keep a paused
        // surface untouched.
        notifyLiveContentMaybeGrew();
        return;
      }
    }
    if (opts.takeover === 'claim') {
      // Reader-asked placement preempts any program and re-establishes
      // bottom-follow intent: an escape ends here, with the same
      // intent-state sweep forceStick and markAtBottom perform (consent
      // consumed, gesture windows cleared). Clear the stop flag after
      // cancel so the next streaming chunk can re-engage the spring.
      markAtBottom();
      spring.cancel();
      spring.clearStopRequest();
    }
    if (opts.write) {
      opts.write();
      return;
    }
    if (!scrollEl) return;
    isAtBottomState = true;
    writeScrollTop(opts.caller ?? 'requestBottom', targetScrollTop());
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
    intent.clearRestoreConsent();
    intent.clearRecentDownIntent();
    intent.clearScrollbarDragSession();
    intent.setEscapedFromLock(false);
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
    // Stamped BEFORE the write so the resulting scroll event is treated
    // as RO-correlated, not user-driven (see the observer pipeline).
    observers.stampSyntheticResizeCorrelation();
    writeScrollTop(caller, targetScrollTop());
  }

  // Mid-flight, the bottom target belongs to the chase. Both notify
  // paths fall back to an instant pin for geometry the spring has no
  // business animating (idle composer growth, warm-up, reduced motion,
  // no-distance nudges) — but when a chase is ALREADY in flight that
  // fallback is an instant write over a running animation: the visible
  // "glide starts, then snaps to the bottom" defect. The gap it fired
  // in is structural: liveness (500ms) and the structural one-shot
  // (250ms) are short clocks that mid-chase retargets deliberately do
  // not refresh, so a glide extended by async row settling (content
  // previews, highlight spans) outlives both while still animating,
  // and a composer-rail resize in that window used to stomp it. Not
  // writing IS handling the observation — the tick re-reads
  // `targetScrollTop()` every frame, so the chase follows the moved
  // bottom on its own; `markTargetChanged` refreshes the retain window
  // for when liveness returns. A large overshoot keeps the instant
  // snap: content collapsed out from under the viewport, and animating
  // upward across it is the same artifact the resolver's mid-spring
  // threshold exists to prevent.
  function absorbedByActiveSpring(): boolean {
    if (!scrollEl || !spring.isActive()) return false;
    const overshoot = scrollEl.scrollTop - targetScrollTop();
    if (overshoot > SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX) return false;
    spring.markTargetChanged();
    return true;
  }

  function notifyContentMaybeGrew(): void {
    const gate = readNotifyContentGate();
    if (isUiRenderTraceEnabled()) trace('scroll.notifyContentMaybeGrew', () => ({
      willPin: gate.canPin,
      springActive: spring.isActive(),
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
    if (absorbedByActiveSpring()) return;
    instantPinAfterExternalGeometryChange('notifyContentMaybeGrew');
  }

  function notifyLiveContentMaybeGrew(): void {
    const gate = readNotifyContentGate();
    // Liveness IS load-bearing here, unlike the growth-delta path. This
    // path also carries VIEWPORT changes ('composer-geometry'): the
    // composer grows the scroller's bottom padding, which shortens its
    // content-box viewport while DOM clientHeight stays fixed. That moves
    // the bottom target without any content arriving. While output is flowing that should
    // ride the in-flight glide rather than snap through it; while idle
    // (the user typing a multi-line draft) the transcript must stay
    // pinned as the box grows, not glide behind it. A structural-append
    // mark counts as live for the same reason the sentinel accepts it.
    const willSpring =
      gate.canPin
      && warm
      && spring.gateOpen()
      && (liveContentActiveNow() || spring.structuralAppendPending());
    if (isUiRenderTraceEnabled()) trace('scroll.notifyLiveContentMaybeGrew', () => ({
      canPin: gate.canPin,
      willSpring,
      springActive: spring.isActive(),
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
      if (arrivalReadback.shouldWriteExact(target)) {
        arrivalReadback.writeExact('notifyLiveContentMaybeGrew.arrive', target);
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
    // nudges where a spring has nothing useful to chase — but never
    // over a chase already in flight (absorbedByActiveSpring).
    if (absorbedByActiveSpring()) return;
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
        // the lease; without this re-pin, sticky users drift. The
        // release acts unasked, so it yields: a structural append that
        // landed during the lease (e.g. inside an auto-collapse
        // transaction) or a spring still in flight across it owns the
        // remaining trip to the bottom, and a direct write here would
        // collapse that glide into an instant hop
        // (bug-report-20260801T214455Z — the recent-window prune's
        // sub-frame lease landing mid-chase). Reader-asked transactions
        // are unaffected either way: their restore claims the bottom
        // AND holds its pause across the measurement flush, so by the
        // time this repin runs the clicked delta is already placed and
        // only growth streamed during the hold is left to glide
        // (bug-report-20260802T011749Z — releasing at tick() let an
        // engaged spring own the toggle's height delta instead).
        requestBottom({ takeover: 'yield', caller: 'pauseAutoScroll.release' });
      }
    };
  }

  // ===== Lifecycle =====
  function attach(nextScrollEl: HTMLElement, nextContentEl: HTMLElement): void {
    if (scrollEl === nextScrollEl && contentEl === nextContentEl) return;
    detach();
    scrollEl = nextScrollEl;
    contentEl = nextContentEl;
    observers.beginWarmup();
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
    observers.attach();
    intent.attach(nextScrollEl);
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
        springWriteRefusalLatched: spring.refusalLatched(),
        // Intent windows + restore-snap consent (consumed by
        // forceStick({reason:'restore'})).
        ...intent.debugState(),
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
    uninstallStickStateDevHook();
    // Disconnects the RO and resets classification + warm-up state
    // (warm → false).
    observers.detach();
    // Removes the intent machine's listeners and resets its transient
    // state; restore-snap consent deliberately survives (see the comment
    // in intent.detach for the first-mount arm-then-attach ordering).
    intent.detach();
    // spring.cancel() also resets the target-change timestamp and
    // sentinel state; only the stop request needs a separate clear.
    spring.cancel();
    spring.clearStopRequest();
    chokepoint.resetLedger();
    scrollEl = undefined;
    contentEl = undefined;
    cachedGeometryValid = false;
    cachedBottomTargetValid = false;
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
    // Scroll-event ATTRIBUTION, not follow gating — see the interface doc
    // for why this must not fold in pauseDepth the way isSticky does.
    get positionOwnerDriven() {
      return !escapedFromLockState;
    },
    get isWarm() {
      return warm;
    },
    get warmReason() {
      return warmReason;
    },
    pauseAutoScroll,
    observe,
    markStructuralContentPending: spring.markStructuralAppend,
    autoScrollInFlight: () => spring.isActive() || spring.structuralAppendPending(),
    preserveScrollAnchor,
    attach,
    detach,
    forceStick,
    requestBottom,
    markAtBottom,
    setEscapedFromLock: intent.setEscapedFromLock,
    armWarmup: observers.beginWarmup,
    skipWarmup: observers.skipWarmup,
    notifyQuietContextSignalChanged: observers.notifyQuietContextSignalChanged,
    armRestoreSnap: intent.armRestoreSnap,
    applyEngineCompensation,
    applyScrollTarget,
    deliverContentGeometry: observers.deliverSample,
  };
}
