// Regression guard for the end-of-turn drain judder (investigation
// 2026-07-21): after the wire turn settles, the smoother drains its
// backlog at the adaptive ceiling (~1000 cps ≈ a wrapped text line
// every ~6 frames), so the bottom target advances in LINE QUANTA
// (~26 px) at 8–16 Hz rather than continuously. The deceleration
// envelope alone keyed speed to remaining distance, so each quantum
// produced a sharp-attack / exponential-decay velocity pulse — a ~1.5×
// peak/trough sawtooth in the visible judder band — where the SAME
// average growth delivered continuously followed at near-constant
// speed. The growth-rate feedforward (SPRING_FEEDFORWARD_* in
// spring.ts) closes that gap: sustained quantized growth now follows at
// the delivery's average rate, matching the continuous reference. These
// tests pin both profiles with the deterministic harness so the motion
// shape is reproducible without the app.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createSpringChase,
  type ArrivalReadback,
  type SpringChaseDeps,
} from './spring';
import { setDocumentResumeAtForTest } from './documentResume';
import { ARRIVAL_DISTANCE_PX } from './resolver';

// Deterministic clock + rAF queue, mirroring spring.test.ts's harness
// (kept local: that file's harness is module-private by design).
let now = 0;
let rafQueue: FrameRequestCallback[] = [];

function frame(ms = 16.67): void {
  now += ms;
  const callbacks = rafQueue;
  rafQueue = [];
  for (const cb of callbacks) cb(now);
}

interface Harness {
  spring: ReturnType<typeof createSpringChase>;
  getScrollTop(): number;
  setTarget(value: number): void;
  /** External coordinate shift (an applied engine compensation). */
  shiftScrollTop(delta: number): void;
}

function makeHarness(): Harness {
  let scrollTop = 0;
  let target = 0;

  const el = {
    get scrollTop() {
      return scrollTop;
    },
    get clientHeight() {
      return 0; // unmeasured: chase-distance clamp stays inert
    },
  } as unknown as HTMLElement;

  const arrival: ArrivalReadback = {
    matches: () => false,
    record: () => {},
    shouldWriteExact: (t) => scrollTop !== t,
    writeExact: (_caller, t) => {
      scrollTop = t;
    },
    clear: () => {},
    invalidateStale: () => {},
  };

  const deps: SpringChaseDeps = {
    getScrollEl: () => el,
    isPaused: () => false,
    isAtBottom: () => true,
    isEscaped: () => false,
    selectionActive: () => false,
    targetScrollTop: () => target,
    scrollTopIsAtTarget: (t) => Math.abs(scrollTop - t) <= ARRIVAL_DISTANCE_PX,
    arrival,
    writeScrollTop: (_caller, value) => {
      scrollTop = value;
    },
    animationMode: () => 'spring',
    prefersReducedMotion: () => false,
    forceNextSpringTickTrace: () => {},
    settleGlideResidue: () => {},
    devicePixelRatio: () => 1,
  };

  return {
    spring: createSpringChase(deps),
    getScrollTop: () => scrollTop,
    setTarget: (value) => {
      target = value;
    },
    shiftScrollTop: (delta) => {
      scrollTop += delta;
    },
  };
}

/**
 * Drive sustained bottom growth and record per-frame displacements.
 * `quantumPx` total growth lands every `framesPerQuantum` frames —
 * either as one bump at the quantum boundary (`quantized`, the drain's
 * line-wrap shape) or spread evenly across every frame (`continuous`,
 * the reference profile at the same average px/s). The first two
 * quanta are discarded as spin-up — the feedforward detector's warmup
 * (SPRING_FEEDFORWARD_MIN_EVENTS growth observations).
 */
function followProfile(
  mode: 'quantized' | 'continuous',
  quantumPx: number,
  framesPerQuantum: number,
  quanta: number,
): number[] {
  const h = makeHarness();
  // Park at an established position first so the profile measures
  // steady-state follow, not the initial catch-up.
  h.setTarget(500);
  h.spring.markTargetChanged();
  h.spring.start();
  for (let i = 0; i < 80; i++) frame();

  let target = 500;
  const moves: number[] = [];
  const warmupFrames = 2 * framesPerQuantum;
  for (let q = 0; q < quanta; q++) {
    for (let f = 0; f < framesPerQuantum; f++) {
      if (mode === 'continuous') {
        target += quantumPx / framesPerQuantum;
        h.setTarget(target);
        h.spring.markTargetChanged();
        h.spring.start();
      } else if (f === 0) {
        target += quantumPx;
        h.setTarget(target);
        h.spring.markTargetChanged();
        h.spring.start();
      }
      const before = h.getScrollTop();
      frame();
      moves.push(h.getScrollTop() - before);
    }
  }
  return moves.slice(warmupFrames);
}

function coefficientOfVariation(values: number[]): number {
  const mean = values.reduce((a, b) => a + b, 0) / values.length;
  const variance =
    values.reduce((a, b) => a + (b - mean) ** 2, 0) / values.length;
  return Math.sqrt(variance) / mean;
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
  vi.spyOn(performance, 'now').mockImplementation(() => now);
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// Drain-band shape: ~26px line quantum every 6 frames ≈ 260 px/s —
// the adaptive-ceiling drain (~1000 cps at chat line metrics).
const QUANTUM_PX = 26;
const FRAMES_PER_QUANTUM = 6;
const QUANTA = 12;

/** Per-quantum peak/trough ratio over the steady window. */
function quantumPeakTroughRatios(moves: number[], framesPerQuantum: number): number[] {
  const ratios: number[] = [];
  for (let start = 0; start + framesPerQuantum <= moves.length; start += framesPerQuantum) {
    const quantum = moves.slice(start, start + framesPerQuantum);
    ratios.push(Math.max(...quantum) / Math.min(...quantum));
  }
  return ratios;
}

describe('spring follow profile during the end-of-turn drain', () => {
  // Pre-feedforward measured profile (harness, 60Hz): quantized growth
  // followed as 4.86,5.24,4.69,4.18,3.72,3.31 repeating — a ~1.58×
  // peak/trough sawtooth at the 10Hz quantum cadence. With the
  // feedforward the floor holds the chase at the delivery rate, so the
  // quantized profile must now match the continuous reference's
  // near-constant texture.
  it('line-quantized growth at drain rate follows at near-constant speed (no sawtooth)', () => {
    const moves = followProfile('quantized', QUANTUM_PX, FRAMES_PER_QUANTUM, QUANTA);
    const ratios = quantumPeakTroughRatios(moves, FRAMES_PER_QUANTUM);
    for (const ratio of ratios) {
      expect(ratio).toBeLessThan(1.15);
    }
    expect(coefficientOfVariation(moves)).toBeLessThan(0.1);
    // Fast but smooth: the follow keeps pace with the delivery (no lag
    // growth across the steady window), it just stops pulsing.
    const meanSpeed = moves.reduce((a, b) => a + b, 0) / moves.length;
    expect(meanSpeed).toBeGreaterThan((QUANTUM_PX / FRAMES_PER_QUANTUM) * 0.95);
    expect(meanSpeed).toBeLessThan((QUANTUM_PX / FRAMES_PER_QUANTUM) * 1.05);
  });

  it('reference: the same average growth delivered continuously follows at near-constant speed', () => {
    const moves = followProfile('continuous', QUANTUM_PX, FRAMES_PER_QUANTUM, QUANTA);
    expect(coefficientOfVariation(moves)).toBeLessThan(0.1);
    const ratios = quantumPeakTroughRatios(moves, FRAMES_PER_QUANTUM);
    for (const ratio of ratios) {
      expect(ratio).toBeLessThan(1.15);
    }
  });

  /** Drive `quanta` line quanta, calling `inject(q, api)` at each quantum
   *  boundary before the growth lands. Returns post-warmup moves. */
  function followQuantizedWith(
    inject: (quantum: number, h: Harness, bumpTarget: (delta: number) => void) => void,
    quanta: number,
  ): number[] {
    const h = makeHarness();
    h.setTarget(500);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 80; i++) frame();

    let target = 500;
    const bumpTarget = (delta: number): void => {
      target += delta;
      h.setTarget(target);
      h.spring.markTargetChanged();
      h.spring.start();
    };
    const moves: number[] = [];
    for (let q = 0; q < quanta; q++) {
      for (let f = 0; f < FRAMES_PER_QUANTUM; f++) {
        if (f === 0) {
          inject(q, h, bumpTarget);
          bumpTarget(QUANTUM_PX);
        }
        const before = h.getScrollTop();
        frame();
        moves.push(h.getScrollTop() - before);
      }
    }
    return moves.slice(2 * FRAMES_PER_QUANTUM);
  }

  it('an applied engine compensation mid-drain does not read as delivery (no rate-EMA burst)', () => {
    // The fix-interaction defect the review caught: a background
    // completion patches a +290px tool row above the viewport
    // mid-drain; the compensation applies (resolver apply-always), so
    // target AND scrollTop shift together — a pure coordinate shift.
    // Without noteExternalTargetShift the detector reads the target
    // jump as delivery, the rate EMA spikes ~5×, and the cruise pin
    // burst-chases at the inflated rate for several quanta (the pulse
    // train the feedforward exists to kill).
    const moves = followQuantizedWith((q, h, bumpTarget) => {
      if (q === 6) {
        // Post-flush compensation delivery: spacer grew 290 (target
        // shift), controller wrote scrollTop by the same delta and
        // translated the detector's frame.
        bumpTarget(290);
        h.shiftScrollTop(290);
        h.spring.noteExternalTargetShift(290);
      }
    }, QUANTA);
    const ratios = quantumPeakTroughRatios(moves, FRAMES_PER_QUANTUM);
    for (const ratio of ratios) {
      expect(ratio).toBeLessThan(1.15);
    }
    expect(coefficientOfVariation(moves)).toBeLessThan(0.1);
  });

  it('a transient mid-drain shrink (markdown rebalance) does not cold-reset the smooth follow', () => {
    // parseIncompleteMarkdown rebalances and fence seals shrink the
    // target a line's worth mid-drain, then the next quantum re-grows
    // past it. The high-water mark measures delivery peak-to-peak, so
    // the dip costs at most a brief rate dip — never a detector reset
    // back to the envelope sawtooth (peak/trough ≥ 1.4 per quantum).
    const moves = followQuantizedWith((q, _h, bumpTarget) => {
      if (q === 6) bumpTarget(-22);
    }, QUANTA);
    const ratios = quantumPeakTroughRatios(moves, FRAMES_PER_QUANTUM);
    for (const ratio of ratios) {
      expect(ratio).toBeLessThan(1.3);
    }
  });
});
