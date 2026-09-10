import { afterEach, beforeEach, vi } from 'vitest';
import {
  __resetSpringFrameBatcherForTest,
  createSpringChase,
} from './spring';
import type { ArrivalReadback, SpringChaseDeps, SpringWriteRefusalEvent } from './springTypes';
import { setDocumentResumeAtForTest } from './documentResume';
import { ARRIVAL_DISTANCE_PX } from './resolver';
import {
  clearUiRenderTrace,
  setUiRenderTraceEnabled,
} from '../uiRenderTrace';

// Deterministic clock + rAF queue. `performance.now` is stubbed to the
// same counter the rAF callbacks receive, matching the production
// contract (scroll/time.ts nowMs reads the clock rAF timestamps are on).
export let now = 0;
export function advanceClock(ms: number): void { now += ms; }
export let rafQueue: FrameRequestCallback[] = [];

export function frame(ms = 16.67): void {
  now += ms;
  const callbacks = rafQueue;
  rafQueue = [];
  for (const cb of callbacks) cb(now);
}

export interface Harness {
  spring: ReturnType<typeof createSpringChase>;
  writes: { caller: string; value: number }[];
  refusalEvents: SpringWriteRefusalEvent[];
  getScrollTop(): number;
  /** The model's velocity after the last tick (px per 60Hz frame). */
  velocity(): number;
  setTarget(value: number): void;
  setAttached(attached: boolean): void;
  getTarget(): number;
  setLiveContentActive(active: boolean): void;
  setSelectionActive(active: boolean): void;
  setRefuseWrites(refuse: boolean): void;
}

/**
 * `quantize: true` models the browser's scrollTop contract: a write lands
 * on the engine's pixel grid — whole CSS pixels at the default `dpr` of
 * 1, whole DEVICE pixels otherwise (the Pixel 9a measurement, 2026-09-04:
 * 1/2.625 CSS px steps) — and readbacks return that snapped value. The
 * default (fractional storage) keeps the kinematic assertions exact.
 * The dependency reports this simulated engine's measured grid.
 *
 * `clientHeight` (default 0 = unmeasured) arms the chase-distance clamp;
 * the kinematic tests leave it off so their long-glide assertions stay
 * exact.
 */
export function makeHarness(
  opts: {
    quantize?: boolean;
    quantum?: number;
    readbackPrecision?: number;
    float32?: boolean;
    dpr?: number;
    /**
     * The engine rounds every write to a whole CSS pixel whatever the
     * device ratio (desktop Chromium at DPR 2, per the browser test).
     * Implies `quantize`.
     */
    cssGrid?: boolean;
    /**
     * The engine FLOORS every write to a whole CSS pixel whatever the
     * device ratio (macOS WKWebView, the 2026-09-04 spike: 100.75 reads
     * back 100, and +0.5 never moves). Implies `quantize`.
     */
    floorGrid?: boolean;
    clientHeight?: number;
    refuse?: boolean;
    /** Engine max-scroll clamp: stored scrollTop never exceeds this. */
    clampMax?: number;
  } = {},
): Harness {
  const dpr = () => opts.dpr ?? 1;
  const quantum = () => opts.quantum ?? (opts.cssGrid || opts.floorGrid ? 1 : opts.quantize ? 1 / dpr() : 1e-8);
  let scrollTop = 0;
  let target = 0;
  let attached = true;
  let liveContentActive = true;
  let selectionActive = false;
  // Write-refusal mode: models the wedged non-scroll-container element
  // from bug-report-20260818T003129Z — writes are received but move
  // nothing, and (mirroring the real chokepoint) each one still stamps
  // the ledger with the unmoved readback.
  let refuseWrites = opts.refuse ?? false;
  // Faithful mini provenance ledger: writes explain their readback, so
  // `scrollTopUnexplained` only reports true if a test mutates scrollTop
  // outside the write path (a simulated browser clamp) — mirroring the
  // controller's ledger contract.
  let lastExplainedScrollTop: number | null = null;
  const writes: { caller: string; value: number }[] = [];
  const refusalEvents: SpringWriteRefusalEvent[] = [];
  const store = (value: number): void => {
    if (!refuseWrites) {
      let quantized = opts.floorGrid
        ? Math.floor(value / quantum()) * quantum()
        : opts.cssGrid || opts.quantize || opts.quantum
          ? Math.round(value / quantum()) * quantum()
          : value;
      if (opts.readbackPrecision) quantized = Math.round(quantized / opts.readbackPrecision) * opts.readbackPrecision;
      if (opts.float32) quantized = Math.fround(quantized);
      scrollTop = opts.clampMax === undefined ? quantized : Math.min(quantized, opts.clampMax);
    }
    lastExplainedScrollTop = scrollTop;
  };

  const el = {
    get scrollTop() {
      return scrollTop;
    },
    get clientHeight() {
      return opts.clientHeight ?? 0;
    },
  } as unknown as HTMLElement;

  let acceptedTarget: number | null = null;
  const arrival: ArrivalReadback = {
    matches: (t) => acceptedTarget !== null && Math.abs(acceptedTarget - t) <= ARRIVAL_DISTANCE_PX
      && Math.abs(scrollTop - t) <= ARRIVAL_DISTANCE_PX,
    record: (t) => { acceptedTarget = scrollTop !== t && Math.abs(scrollTop - t) <= ARRIVAL_DISTANCE_PX ? t : null; },
    shouldWriteExact: (t) => scrollTop !== t && !arrival.matches(t),
    writeExact: (caller, t) => {
      writes.push({ caller, value: t });
      store(t);
      arrival.record(t);
    },
    clear: () => { acceptedTarget = null; },
    invalidateStale: (t) => {
      if (acceptedTarget !== null && Math.abs(acceptedTarget - t) > ARRIVAL_DISTANCE_PX) acceptedTarget = null;
    },
  };

  const deps: SpringChaseDeps = {
    getScrollEl: () => attached ? el : undefined,
    isPaused: () => false,
    isAtBottom: () => true,
    isEscaped: () => false,
    selectionActive: () => selectionActive,
    targetScrollTop: () => target,
    currentScrollTop: () => scrollTop,
    arrival,
    writeScrollTop: (caller, value) => {
      writes.push({ caller, value });
      store(value);
      return scrollTop;
    },
    liveContentActive: () => liveContentActive,
    prefersReducedMotion: () => false,
    scrollGrid: () => ({ quantum: quantum(), writeOffset: opts.floorGrid ? quantum() / 2 : 0, readbackError: (opts.readbackPrecision ?? 0) / 2 }),
    forceNextSpringTickTrace: () => {},
    scrollTopUnexplained: () =>
      lastExplainedScrollTop !== null
      && Math.abs(scrollTop - lastExplainedScrollTop) > ARRIVAL_DISTANCE_PX,
    reportWriteRefusal: (event) => {
      refusalEvents.push({ ...event });
    },
  };

  const spring = createSpringChase(deps);
  return {
    spring,
    writes,
    refusalEvents,
    getScrollTop: () => scrollTop,
    velocity: () => spring.velocityForTest(),
    setAttached: (value) => { attached = value; },
    setTarget: (value) => {
      target = value;
    },
    getTarget: () => target,
    setLiveContentActive: (active: boolean) => {
      liveContentActive = active;
    },
    setSelectionActive: (active: boolean) => {
      selectionActive = active;
    },
    setRefuseWrites: (refuse: boolean) => {
      refuseWrites = refuse;
    },
  };
}

/** Per-frame scrollTop displacements over `frames` ticks. */
export function displacements(h: Harness, frames: number): number[] {
  const out: number[] = [];
  for (let i = 0; i < frames; i++) {
    const before = h.getScrollTop();
    frame();
    out.push(h.getScrollTop() - before);
  }
  return out;
}

/**
 * The model's velocity after each of `frames` ticks. The kinematic
 * suites (onset, acceleration continuity, parked decay) pin the
 * MODEL: what reaches scrollTop is whole device pixels, which cannot
 * show a 10% per-frame ratio (see SpringChase.velocityForTest).
 */
export function velocities(h: Harness, frames: number, ms = 16.67): number[] {
  const out: number[] = [];
  for (let i = 0; i < frames; i++) {
    frame(ms);
    out.push(h.velocity());
  }
  return out;
}

beforeEach(() => {
  now = 0;
  rafQueue = [];
  setDocumentResumeAtForTest(null);
  vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
    rafQueue.push(cb);
    return rafQueue.length;
  });
  vi.stubGlobal('cancelAnimationFrame', () => {});
  __resetSpringFrameBatcherForTest();
  vi.spyOn(performance, 'now').mockImplementation(() => now);
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  // Clear BEFORE disabling: setUiRenderTraceEnabled(false) force-flushes
  // pending lines through the (unavailable-in-unit-tests) file binding.
  clearUiRenderTrace();
  setUiRenderTraceEnabled(false);
});
