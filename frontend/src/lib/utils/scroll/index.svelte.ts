// Sticky-bottom controller, shared by chat MessageTimeline and
// Discussion ChannelView.
//
// Port of stackblitz-labs/use-stick-to-bottom adapted to Svelte 5. Owns
// the user's intent ("glued to bottom" or "free") and the content-
// geometry pipeline, fed by one of two sources: a single ResizeObserver
// on the content element (the default — ChannelView), or engine-sourced
// samples via `deliverContentGeometry` when the consumer sets
// `externalContentGeometry` (chat — see observers.ts). Two animation
// behaviors for autonomous content growth, selected per-fire by the
// consumer via the `animationMode` option:
//
//   - 'instant' (default): sync-pin. The same paint frame where
//     contentEl grows also lands scrollTop at the new target, so the
//     user sees content arriving at the bottom with no perceptible
//     scroll motion. Used by Discussion's ChannelView and by chat
//     whenever no live content has advanced recently — late Streamdown
//     typesetting on settled content, row remeasurement on a
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
//     mount-time row remeasurement and async Streamdown typesetting
//     would spring-chase a thread restore visibly.
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
import { getSettings } from '../../stores/settings.svelte';
import { isUiRenderTraceEnabled } from '../uiRenderTrace';
import { createScrollIntent, isSelectingInside } from './intent';
import { createContentObserver } from './observers';
import type { EngineCompensation } from '../virtual/types';
import {
  ARRIVAL_DISTANCE_PX,
  SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX,
  resolveEngineCompensation,
  withinArrivalBand,
  type EngineCompensationObservation,
  type ResolverState,
} from './resolver';
import { createSpringChase, type ArrivalReadback } from './spring';
import { trace } from './trace';
import type {
  ScrollObservationKind,
  ScrollWriteCaller,
  UseStickToBottomController,
  UseStickToBottomOptions,
  WarmReason,
} from './types';

// Public types re-exported so consumers import the controller and its
// contract from one place; the definitions live in ./types.
export type {
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
// windows live in scroll/spring.ts. Only the trace sampling stays here,
// because it belongs to the writeScrollTop chokepoint:
// Spring tick writes fire at 60Hz during a chase. Sample so the
// dev-only trace file isn't dominated by predictable +1px increments.
// 12 ≈ 5Hz, which is enough to see the spring is running without
// crowding the rare gesture/escape events that diagnose scroll
// regressions. First and last ticks of every chase are always
// recorded via the springTickSinceLastTrace reset at chase boundaries.
const SPRING_TICK_TRACE_SAMPLE = 12;


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

  // ===== Arrival-readback acceptance state =====
  // Some engines reject the exact max scrollTop by one CSS pixel. When a
  // write lands within the arrival band but not exactly on target, the
  // accepted readback is recorded so arrival checks stop re-writing a
  // target the browser will keep rejecting. Owned here — not by the
  // spring — because notifyLiveContentMaybeGrew shares it; the spring
  // chase (scroll/spring.ts) reaches it through deps.arrival. The
  // helper group (`arrivalReadback`) lives in the Geometry section below.
  let arrivalReadbackAcceptedTarget: number | null = null;

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
    return Math.max(0, scrollEl.scrollHeight - scrollEl.clientHeight);
  }

  function scrollTopIsAtTarget(target: number): boolean {
    return !scrollEl || withinArrivalBand(scrollEl.scrollTop, target);
  }

  // Arrival-readback acceptance helpers over the state above, grouped as
  // one unit (ArrivalReadback in scroll/spring.ts): the spring chase
  // reaches them via deps.arrival; notifyLiveContentMaybeGrew calls them
  // directly.
  const arrivalReadback: ArrivalReadback = {
    matches(target: number): boolean {
      return arrivalReadbackAcceptedTarget !== null
        && withinArrivalBand(arrivalReadbackAcceptedTarget, target)
        && scrollTopIsAtTarget(target);
    },
    record(target: number): void {
      if (scrollEl && scrollEl.scrollTop !== target && scrollTopIsAtTarget(target)) {
        arrivalReadbackAcceptedTarget = target;
        return;
      }
      arrivalReadbackAcceptedTarget = null;
    },
    shouldWriteExact(target: number): boolean {
      if (!scrollEl) return false;
      if (scrollEl.scrollTop === target) return false;
      if (!scrollTopIsAtTarget(target)) return true;
      return !arrivalReadback.matches(target);
    },
    writeExact(caller: ScrollWriteCaller, target: number): void {
      writeScrollTop(caller, target);
      arrivalReadback.record(target);
    },
    clear(): void {
      arrivalReadbackAcceptedTarget = null;
    },
    // Drop an accepted readback whose target has since moved out of the
    // arrival band — the acceptance only excuses re-writes for the target
    // it was recorded against.
    invalidateStale(target: number): void {
      if (
        arrivalReadbackAcceptedTarget !== null
        && !withinArrivalBand(arrivalReadbackAcceptedTarget, target)
      ) {
        arrivalReadbackAcceptedTarget = null;
      }
    },
  };

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
  // ===== Fractional glide residue =====
  // scrollTop is quantized to whole CSS pixels by the engine, so a slow
  // spring tail (< ~1px per display frame) rendered through scrollTop
  // alone becomes 1px steps at a low effective rate — measured as
  // 14–55Hz stepping on a 165Hz panel (2026-07-04 capture), perceived
  // as "low fps" judder. The compositor has no such quantum: the
  // sub-pixel remainder of each spring write rides as a translateY on
  // contentEl, so the rendered position IS the spring's fractional
  // output and sub-pixel motion is genuinely continuous (bilinear
  // resample during motion only). The residue is always < 1px, applies
  // only to 'spring.tick' writes, clears on every other write, is
  // eased out when the spring stops without a write (catch-up /
  // selection pause / sentinel entry / cancel, via the
  // settleGlideResidue dep), and clears instantly on detach — text
  // always comes to rest crisp at translate 0. This is a render
  // detail of the write chokepoint, not a second scroll writer: it
  // never changes scrollTop, fires no scroll events, and does not
  // affect layout offsets or any measurement this package or the
  // virtualizer performs (row sizes come from ResizeObserver content
  // boxes; only an absolute getBoundingClientRect against the viewport
  // would see the <1px offset, and nothing in the scroll path does).
  //
  // Releasing the residue has two shapes, and the split is load-bearing:
  //   - A clear that ACCOMPANIES a real scrollTop write (any non-glide
  //     caller below) is instant — the rendered jump IS the write's
  //     motion, so rendered position must equal the new scrollTop in
  //     the same frame.
  //   - A release with NO accompanying write (spring caught up between
  //     quanta, selection pause, sentinel entry, cancel) EASES the
  //     residue to zero over a few frames instead. The asymptotic tail
  //     parks every landing with up to ~0.5px of live residue; snapping
  //     that with no write is a sub-pixel pop, and during bursty tool
  //     output — one landing per quantum at a few Hz — the repeated
  //     pops read as a faint vibration (2026-07-04 report on the first
  //     residue build). The ease-out is the same "cradle" shape as the
  //     glide itself, ~100ms to crisp.
  let glideResidue = 0;
  let residueSettleHandle: number | null = null;
  // Per-frame decay factor for the no-write release: 0.5px falls below
  // the 0.02 snap threshold in ~6 frames (~100ms at 60Hz).
  const RESIDUE_SETTLE_DECAY = 0.55;
  function stopResidueSettle(): void {
    if (residueSettleHandle !== null) {
      cancelAnimationFrame(residueSettleHandle);
      residueSettleHandle = null;
    }
  }
  function setGlideResidue(residue: number): void {
    // Sub-1/50px residues are visually void; snap them to zero so the
    // transform clears (and the style write is skipped) at rest.
    const next = Math.abs(residue) < 0.02 ? 0 : residue;
    if (next === glideResidue) return;
    glideResidue = next;
    if (!contentEl) return;
    // The epsilon rotation defeats compositor pixel alignment: WebKit
    // snaps axis-aligned composited layers to the device-pixel grid
    // (text-sharpness heuristic), which rounds a pure sub-pixel
    // translateY to 0 or ±1 device px — the applied offset then FLIPS
    // each time the residue crosses the half-pixel mark, oscillating
    // around the smooth trajectory instead of following it (captured
    // 2026-07-04T2016 on a 1.1-DPR grid: rendered math monotone,
    // on-screen motion vibrating, hairline rows worst). A
    // non-axis-aligned matrix cannot be pixel-snapped, so the
    // compositor must resample at the true fractional offset. 1e-4deg
    // shears < 0.01px across a 5000px layer — imperceptible itself.
    contentEl.style.transform =
      next === 0 ? '' : `translateY(${-next}px) rotate(0.0001deg)`;
  }
  /** Instant set/clear — for glide writes and clears that accompany a real scrollTop write. */
  function applyGlideResidue(residue: number): void {
    stopResidueSettle();
    setGlideResidue(residue);
  }
  /** No-write release — ease the residue to zero instead of popping. Idempotent per release. */
  function settleGlideResidue(): void {
    if (glideResidue === 0 || residueSettleHandle !== null) return;
    const step = (): void => {
      residueSettleHandle = null;
      setGlideResidue(glideResidue * RESIDUE_SETTLE_DECAY);
      if (glideResidue !== 0) {
        residueSettleHandle = requestAnimationFrame(step);
      }
    };
    residueSettleHandle = requestAnimationFrame(step);
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
    scrollEl.scrollTop = value;
    // Tag using the BROWSER-rounded read so the scroll handler's token
    // match sees the same value the scroll event will report.
    const taggedTop = scrollEl.scrollTop;
    intent.noteProgrammaticWrite(taggedTop);
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
        // Sub-pixel remainder THIS write rides on the content transform
        // (glide writes only; every other caller clears it below).
        residue:
          caller === 'spring.tick'
            ? Math.round((value - taggedTop) * 100) / 100
            : 0,
        isAtBottomState,
        escapedFromLockState,
        pauseDepth,
        isNearBottomState,
      }));
    }
    // Style writes LAST (residue transform, then scrollBehavior
    // restore): a style write dirties style state, so keeping both
    // after every layout read above avoids forcing an extra recalc
    // mid-sequence — this runs at up to display refresh rate during a
    // chase. Same-frame visually: the transform composites with this
    // frame's paint regardless of its position in the sequence.
    if (caller === 'spring.tick') {
      // Render the engine-rounded remainder via the content transform.
      // |residue| ≥ 1 means the engine clamped the write (max-scrollTop
      // race), not rounding — never smear a clamp onto the transform.
      const residue = value - taggedTop;
      applyGlideResidue(residue > -1 && residue < 1 ? residue : 0);
    } else if (glideResidue !== 0) {
      // Every non-glide write is an exact/instant placement; rendered
      // position must equal scrollTop exactly.
      applyGlideResidue(0);
    }
    if (suppressScrollBehavior) scrollEl.style.scrollBehavior = originalScrollBehavior;
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
  // The app-level low-power setting rides the same gate as the OS
  // reduced-motion preference: both mean "place instantly, never
  // spring-glide". Spring glides are the app's dominant GPU cost —
  // one compositor frame per vsync for the whole chase — so this is
  // the scroll half of low-power mode (the reveal smoother and the
  // working-LED chase gate on the same setting at their own sites).
  // Read live (plain non-reactive read; the spring gate and resolver
  // sample it per event/tick, so a toggle applies to the next
  // decision without any subscription).
  function motionReduced(): boolean {
    return prefersReducedMotion() || getSettings().lowPowerMode;
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
    arrival: arrivalReadback,
    writeScrollTop,
    animationMode: animationModeNow,
    prefersReducedMotion: motionReduced,
    forceNextSpringTickTrace,
    settleGlideResidue,
    // Display input to the spring's refresh-aware fusion floor — the
    // derivation lives beside its sibling constants in spring.ts
    // (fusionFloorPxPerFrame); the spring supplies the other input,
    // its measured rAF cadence. Read live — zoom and monitor moves
    // change dpr between and during chases.
    devicePixelRatio: () =>
      typeof window !== 'undefined' && window.devicePixelRatio > 0
        ? window.devicePixelRatio
        : 1,
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
    animationMode: animationModeNow,
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
    writeScrollTop,
    resolverStateSnapshot,
    prefersReducedMotion: motionReduced,
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
  // the one write through the chokepoint. Detached: decline — a
  // declined compensation cannot desync the engine (its offset follows
  // real scroll events).
  function applyEngineCompensation(compensation: EngineCompensation): boolean {
    if (!scrollEl) return false;
    const observation: EngineCompensationObservation = {
      kind: compensation.kind,
      target: compensation.target,
      scrollTop: scrollEl.scrollTop,
      bottomTarget: targetScrollTop(),
      clientHeight: scrollEl.clientHeight,
      widthReflowActive: observers.widthReflowActive(),
    };
    const decision = resolveEngineCompensation(resolverStateSnapshot(), observation);
    if (isUiRenderTraceEnabled()) trace('scroll.engineCompensation', () => ({
      kind: compensation.kind,
      target: Math.round(compensation.target),
      delta: Math.round(compensation.delta),
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
    intent.clearRestoreConsent();
    intent.clearRecentDownIntent();
    intent.clearScrollbarDragSession();
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
    intent.setEscapedFromLock(false);
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
    if (reason === 'restore') observers.beginWarmup();
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
    // The glide residue is cleared INSTANTLY here (not via cancel's
    // gentle settle) — the settle loop would outlive contentEl below
    // and leave the detached element with a stale transform.
    spring.cancel();
    spring.clearStopRequest();
    applyGlideResidue(0);
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
    get warmReason() {
      return warmReason;
    },
    pauseAutoScroll,
    observe,
    markStructuralContentPending: spring.markStructuralAppend,
    preserveScrollAnchor,
    attach,
    detach,
    forceStick,
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
