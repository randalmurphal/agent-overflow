// The programmatic-write chokepoint and its satellites, extracted from
// the controller (index.svelte.ts) as one unit: every piece here exists
// BECAUSE every programmatic scrollTop write flows through one site.
//
// - `writeScrollTop` — the single write site. Tags the write for the
//   intent machine's scroll-event classification, updates the
//   provenance ledger, and samples spring-tick trace records.
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

export interface WriteChokepointDeps {
  getScrollEl(): HTMLElement | undefined;
  /** ≤1px arrival tolerance over the live scrollTop (controller geometry helper). */
  scrollTopIsAtTarget(target: number): boolean;
  /** Post-write near-bottom refresh — touches the controller's $state flag. */
  refreshIsNearBottom(scrollTop: number, bottomTarget?: number): number;
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
  /** Write and return the browser-rounded readback. A caller that just
   * sampled the bottom target can pass it to avoid repeating layout reads. */
  writeScrollTop(caller: ScrollWriteCaller, value: number, bottomTarget?: number): number | undefined;
  /** Ledger check: live scrollTop differs from the last explained position. */
  scrollTopUnexplained(): boolean;
  /** Intent-machine hook: a user-classified scroll event explains its position. */
  noteUserScroll(top: number): void;
  /** Detach-path ledger reset (null until the next write/classified scroll). */
  resetLedger(): void;
  arrivalReadback: ArrivalReadback;
  /** Chase boundaries force the next tick write to record (spring dep). */
  forceNextSpringTickTrace(): void;
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
      writeScrollTop(caller, target, target);
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

  // ===== The write =====
  function writeScrollTop(caller: ScrollWriteCaller, value: number, bottomTarget?: number): number | undefined {
    const scrollEl = deps.getScrollEl();
    if (!scrollEl) return undefined;
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
    deps.refreshIsNearBottom(taggedTop, bottomTarget);
    deps.noteProgrammaticWrite(taggedTop);
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
        // Browser quantization or clamping visible at the authored-write
        // boundary. Unlike the former content transform, this is
        // diagnostic only and cannot create a second rendered position.
        quantizationError: Math.round((value - taggedTop) * 100) / 100,
        ...deps.traceState(),
      }));
    }
    if (suppressScrollBehavior) scrollEl.style.scrollBehavior = originalScrollBehavior;
    return taggedTop;
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
  };
}
