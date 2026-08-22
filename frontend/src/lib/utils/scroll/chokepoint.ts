// The programmatic-write chokepoint and its satellites, extracted from
// the controller (index.svelte.ts) as one unit: every piece here exists
// BECAUSE every programmatic scrollTop write flows through one site.
//
// - `writeScrollTop` — the single write site. Tags the write for the
//   intent machine's scroll-event classification, updates the
//   provenance ledger, samples spring-tick trace records, and manages
//   the fractional glide residue that rides the content transform.
// - Provenance ledger — the last EXPLAINED scrollTop (authored write
//   readback, or a user-classified scroll event via `noteUserScroll`).
//   While the spring sentinel idles nothing else may move scrollTop, so
//   a live value off the ledger is WITNESSED evidence of the one
//   unexplained mover left: the browser's max-scroll clamp. The
//   spring's oscillation guards and the resolver's stranded predicate
//   require that evidence before snapping; authored displacements (a
//   head-splice compensation's anchor hold) update the ledger here and
//   therefore can never read as a clamp (bug-report-20260801T213259Z).
// - Arrival-readback acceptance — some engines reject the exact max
//   scrollTop by one CSS pixel; the accepted readback is recorded so
//   arrival checks stop re-writing a target the browser will keep
//   rejecting. Shared by the spring chase (via deps.arrival) and the
//   controller's notify paths.
// - Glide residue — the sub-pixel remainder of each spring write,
//   rendered as a compositor transform so slow spring tails stay
//   continuous instead of stepping whole pixels (full rationale on the
//   member below).
//
// No $state lives here: the reactive flags stay in the controller and
// are reached through accessor deps, the same seam shape as
// spring/intent/observers.

import { isUiRenderTraceEnabled } from '../uiRenderTrace';
import { withinArrivalBand } from './resolver';
import type { ArrivalReadback } from './spring';
import { trace } from './trace';
import type { ScrollWriteCaller } from './types';

// Spring-tick writes fire at display rate during a chase — predictable
// increment-by-1px from the spring solver, and each record is
// ~300 bytes. Without sampling, they were 5% of the 10 MB rotation
// file and crowded out the rare gesture/escape events that actually
// matter for diagnosing scroll regressions. We trace one in every
// SPRING_TICK_TRACE_SAMPLE writes (~5Hz at 60Hz display) plus the very
// first tick of each chase via `forceNextSpringTickTrace` (called from
// spring.start()).
const SPRING_TICK_TRACE_SAMPLE = 12;

// Per-frame decay factor for the residue's no-write release: 0.5px
// falls below the 0.02 snap threshold in ~6 frames (~100ms at 60Hz).
const RESIDUE_SETTLE_DECAY = 0.55;

export interface WriteChokepointDeps {
  getScrollEl(): HTMLElement | undefined;
  getContentEl(): HTMLElement | undefined;
  /** ≤1px arrival tolerance over the live scrollTop (controller geometry helper). */
  scrollTopIsAtTarget(target: number): boolean;
  /** Post-write near-bottom refresh — touches the controller's $state flag. */
  refreshIsNearBottom(): number;
  /**
   * The intent machine's programmatic-write tag (late-bound: the
   * machine is constructed after the chokepoint; the thunk is only
   * invoked at write time).
   */
  noteProgrammaticWrite(top: number): void;
  /** Controller flag snapshot for the write trace record. */
  traceState(): {
    isAtBottomState: boolean;
    escapedFromLockState: boolean;
    pauseDepth: number;
    isNearBottomState: boolean;
  };
}

export interface WriteChokepoint {
  writeScrollTop(caller: ScrollWriteCaller, value: number): void;
  /** Ledger check: live scrollTop differs from the last explained position. */
  scrollTopUnexplained(): boolean;
  /** Intent-machine hook: a user-classified scroll event explains its position. */
  noteUserScroll(top: number): void;
  /** Detach-path ledger reset (null until the next write/classified scroll). */
  resetLedger(): void;
  arrivalReadback: ArrivalReadback;
  /** Chase boundaries force the next tick write to record (spring dep). */
  forceNextSpringTickTrace(): void;
  /** Instant residue set/clear — for glide writes and clears that accompany a real write. */
  applyGlideResidue(residue: number): void;
  /** No-write release — ease the residue to zero instead of popping. */
  settleGlideResidue(): void;
}

export function createWriteChokepoint(deps: WriteChokepointDeps): WriteChokepoint {
  // ===== Provenance ledger =====
  // Null until the first write/classified scroll after attach; band
  // tolerance rather than equality because fractional-DPR sub-pixel
  // wobble at max scroll is not clamp evidence, and the snap sites'
  // own arrival-band conditions ignore ≤1px strands anyway.
  let lastExplainedScrollTop: number | null = null;

  function scrollTopUnexplained(): boolean {
    const scrollEl = deps.getScrollEl();
    if (!scrollEl || lastExplainedScrollTop === null) return false;
    return !withinArrivalBand(scrollEl.scrollTop, lastExplainedScrollTop);
  }

  // ===== Arrival-readback acceptance =====
  let arrivalReadbackAcceptedTarget: number | null = null;

  const arrivalReadback: ArrivalReadback = {
    matches(target: number): boolean {
      return arrivalReadbackAcceptedTarget !== null
        && withinArrivalBand(arrivalReadbackAcceptedTarget, target)
        && deps.scrollTopIsAtTarget(target);
    },
    record(target: number): void {
      const scrollEl = deps.getScrollEl();
      if (scrollEl && scrollEl.scrollTop !== target && deps.scrollTopIsAtTarget(target)) {
        arrivalReadbackAcceptedTarget = target;
        return;
      }
      arrivalReadbackAcceptedTarget = null;
    },
    shouldWriteExact(target: number): boolean {
      const scrollEl = deps.getScrollEl();
      if (!scrollEl) return false;
      if (scrollEl.scrollTop === target) return false;
      if (!deps.scrollTopIsAtTarget(target)) return true;
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

  // ===== Spring-tick trace sampling =====
  // Starts at `SAMPLE - 1` so the first write is recorded (the gating
  // predicate is `<`, so equal-or-greater values record and reset).
  let springTickSinceLastTrace = SPRING_TICK_TRACE_SAMPLE - 1;
  function forceNextSpringTickTrace(): void {
    springTickSinceLastTrace = SPRING_TICK_TRACE_SAMPLE - 1;
  }

  // ===== Fractional glide residue =====
  // scrollTop is quantized to whole CSS pixels by the engine, so a slow
  // spring tail (< ~1px per display frame) rendered through scrollTop
  // alone becomes 1px steps at a low effective rate — measured as
  // 14–55Hz stepping on a 165Hz panel (2026-07-04 capture), perceived
  // as "low fps" judder. The compositor has no such quantum: the
  // sub-pixel remainder of each spring write rides as the individual
  // `translate` property on contentEl, so the rendered position is the
  // spring's fractional output and sub-pixel motion is genuinely continuous (bilinear
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
  // For that translation to ride the compositor rather than trigger
  // main-thread repaints, contentEl must be composited: every
  // controller-owned content element carries a static
  // `scroll-composited-content` class in its own markup (MessageTimeline,
  // ChannelView, ActivityRun's clip content — the last on EVERY
  // mounted expanded run, not just the live one that gets a
  // controller, because liveness can land on an already-mounted run
  // and a class that appears then is itself a raster transition). The
  // hint is deliberately permanent — it was once a promote/demote
  // lease that reclaimed idle-pane tile memory (~27MB across four
  // parked panes, measured 2026-07-21 on Windows/WebView2; that
  // figure, plus whatever mounted run clips add, is the steady-state
  // cost this trade accepts), but every lease transition re-rasters a
  // layer the reader may be looking at, and three separate
  // visible-flicker incidents (2026-08-03 demote under contention,
  // 2026-08-05 promote at glide start, 2026-08-10 clear at a run's
  // live falling edge after long idle) traced back to exactly those
  // transitions. A hint that never changes on a mounted element
  // cannot flicker. attach() reports a contentEl missing the class to
  // the frontend-errors channel, so a consumer that forgets it is loud
  // instead of silently repainting every glide frame.
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
    const contentEl = deps.getContentEl();
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
    if (next === 0) {
      contentEl.style.removeProperty('translate');
      contentEl.style.removeProperty('rotate');
      return;
    }
    // Individual transform properties compose with the virtualizer's
    // plane-origin `transform` instead of overwriting it. Flat scroll
    // surfaces have no base transform and use the same sink unchanged.
    contentEl.style.setProperty('translate', `0px ${-next}px`);
    contentEl.style.setProperty('rotate', '0.0001deg');
  }
  function applyGlideResidue(residue: number): void {
    stopResidueSettle();
    setGlideResidue(residue);
  }
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

  // ===== The write =====
  function writeScrollTop(caller: ScrollWriteCaller, value: number): void {
    const scrollEl = deps.getScrollEl();
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
    // on the hot path — spring ticks fire at display rate and the trace
    // is sampled to ~5Hz, so ~92% of ticks skip the reads entirely.
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
    lastExplainedScrollTop = taggedTop;
    deps.noteProgrammaticWrite(taggedTop);
    deps.refreshIsNearBottom();
    if (shouldTrace) {
      trace('scroll.write', () => ({
        caller,
        requested: Math.round(value),
        beforeTop: Math.round(beforeTop),
        afterTop: Math.round(scrollEl.scrollTop),
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
        ...deps.traceState(),
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

  return {
    writeScrollTop,
    scrollTopUnexplained,
    noteUserScroll: (top: number): void => {
      lastExplainedScrollTop = top;
    },
    resetLedger: (): void => {
      lastExplainedScrollTop = null;
    },
    arrivalReadback,
    forceNextSpringTickTrace,
    applyGlideResidue,
    settleGlideResidue,
  };
}
