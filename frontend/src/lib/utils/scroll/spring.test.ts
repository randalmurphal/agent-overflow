// Unit tests for the spring chase kinematics (createSpringChase) that
// need frame-precise control over rAF timing and target geometry:
// the hard velocity cap, the deceleration envelope, the
// integer-rounding remainder carry (glide-residue continuity),
// the clamp-not-zero momentum carry, and the per-chase telemetry
// summary. Controller-level
// choreography (when a chase runs) lives in index.svelte.test.ts; these
// tests pin HOW a chase advances, driven through a fake deps harness.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createSpringChase, type ArrivalReadback, type SpringChaseDeps } from './spring';
import { ARRIVAL_DISTANCE_PX } from './resolver';
import {
  clearUiRenderTrace,
  getUiRenderTraceRecords,
  setUiRenderTraceEnabled,
} from '../uiRenderTrace';

// Deterministic clock + rAF queue. `performance.now` is stubbed to the
// same counter the rAF callbacks receive, matching the production
// contract (scroll/time.ts nowMs reads the clock rAF timestamps are on).
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
  writes: { caller: string; value: number }[];
  getScrollTop(): number;
  setTarget(value: number): void;
  getTarget(): number;
  setAnimationMode(mode: 'spring' | 'instant'): void;
  residueSettles(): number;
}

/**
 * `quantize: true` models the browser's scrollTop contract at 1× DPI:
 * a fractional write lands on a whole pixel, and readbacks return that
 * rounded value. The default (fractional storage) keeps the kinematic
 * assertions exact; the quantized variant exercises the
 * remainder-carry path (which is inert when writes store exactly).
 */
function makeHarness(opts: { quantize?: boolean } = {}): Harness {
  let scrollTop = 0;
  let target = 0;
  let animationMode: 'spring' | 'instant' = 'spring';
  let residueSettles = 0;
  const writes: { caller: string; value: number }[] = [];
  const store = (value: number): void => {
    scrollTop = opts.quantize ? Math.round(value) : value;
  };

  const el = {
    get scrollTop() {
      return scrollTop;
    },
  } as unknown as HTMLElement;

  const arrival: ArrivalReadback = {
    matches: () => false,
    record: () => {},
    shouldWriteExact: (t) => scrollTop !== t,
    writeExact: (caller, t) => {
      writes.push({ caller, value: t });
      store(t);
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
    writeScrollTop: (caller, value) => {
      writes.push({ caller, value });
      store(value);
    },
    animationMode: () => animationMode,
    prefersReducedMotion: () => false,
    forceNextSpringTickTrace: () => {},
    settleGlideResidue: () => {
      residueSettles += 1;
    },
  };

  return {
    spring: createSpringChase(deps),
    writes,
    getScrollTop: () => scrollTop,
    setTarget: (value) => {
      target = value;
    },
    getTarget: () => target,
    setAnimationMode: (mode) => {
      animationMode = mode;
    },
    residueSettles: () => residueSettles,
  };
}

/** Per-frame scrollTop displacements over `frames` ticks. */
function displacements(h: Harness, frames: number): number[] {
  const out: number[] = [];
  for (let i = 0; i < frames; i++) {
    const before = h.getScrollTop();
    frame();
    out.push(h.getScrollTop() - before);
  }
  return out;
}

beforeEach(() => {
  now = 0;
  rafQueue = [];
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
  // Clear BEFORE disabling: setUiRenderTraceEnabled(false) force-flushes
  // pending lines through the (unavailable-in-unit-tests) file binding.
  clearUiRenderTrace();
  setUiRenderTraceEnabled(false);
});

describe('spring velocity cap', () => {
  it('bounds per-frame displacement on a large chase instead of a distance-proportional zoom', () => {
    const h = makeHarness();
    h.setTarget(900);
    h.spring.markTargetChanged();
    h.spring.start();

    const moves = displacements(h, 60);
    // Uncapped, a 900px chase peaks near ~0.12·D ≈ 100px/frame. The cap
    // holds every frame to SPRING_MAX_VELOCITY_PX_PER_FRAME (18) — small
    // epsilon for the fractional-step integration.
    for (const move of moves) {
      expect(move).toBeLessThanOrEqual(18.5);
    }
    // Still actually gets there: 900px at ≤18px/frame needs ≥50 frames.
    expect(h.getScrollTop()).toBeGreaterThan(700);
  });

  it('bounds a stalled frame to the catch-up burst instead of paying the whole gap', () => {
    const h = makeHarness();
    h.setTarget(900);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 10; i++) frame(); // reach the capped cruise speed

    const before = h.getScrollTop();
    frame(100); // stall: dtFrames = 6, clamped to SPRING_MAX_CATCHUP_STEPS (3)
    const move = h.getScrollTop() - before;
    // Three capped steps: ≤ 3 × 18px (+ integration epsilon), far short of
    // the remaining ~700px gap — a blocked frame never becomes a teleport.
    expect(move).toBeLessThanOrEqual(54.5);
    expect(move).toBeGreaterThan(36); // multiple steps actually ran
  });

  it('caps downward (shrink-follow) chases symmetrically', () => {
    const h = makeHarness();
    h.setTarget(900);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 80; i++) frame();
    expect(h.getScrollTop()).toBeGreaterThan(850);

    // Content shrinks far below the current position mid-follow.
    h.setTarget(100);
    h.spring.markTargetChanged();
    const moves = displacements(h, 40);
    for (const move of moves) {
      expect(move).toBeGreaterThanOrEqual(-18.5);
    }
  });
});

describe('momentum carry across catch-up', () => {
  // Force a mid-chase catch-up with a known above-floor remnant: chase a
  // distant target for a few frames, then move the target to exactly the
  // current position (content shrank to where the viewport is). The next
  // tick runs the caught-up branch with the remnant velocity intact.
  function catchUpWithRemnant(h: Harness, chaseTarget: number, buildFrames: number): void {
    h.setTarget(chaseTarget);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < buildFrames; i++) frame();
    h.setTarget(h.getScrollTop());
    h.spring.markTargetChanged();
    frame(); // caught-up tick — carry rule applies here
  }

  it('clamps an above-ceiling remnant to the carry ceiling instead of zeroing it (no dead stop)', () => {
    const h = makeHarness();
    // ~5 frames into a 300px chase the velocity is well above the
    // ceiling (4).
    catchUpWithRemnant(h, 300, 5);

    // Next line-sized growth: the carried remnant (4) integrates to
    // (0.7·4 + 0.08·20)/1.25 ≈ 3.52, which the decel envelope shapes
    // down to 0.11·20 = 2.2 on the first frame. A cold start (the old
    // zeroing) would move only (0.08·20)/1.25 = 1.28 — carry is what
    // keeps the next quantum measurably in motion.
    h.setTarget(h.getTarget() + 20);
    h.spring.markTargetChanged();
    const before = h.getScrollTop();
    frame();
    const firstFrameMove = h.getScrollTop() - before;
    expect(firstFrameMove).toBeGreaterThan(2.1);
    expect(firstFrameMove).toBeLessThan(2.3);
  });

  it('sheds all momentum once the retain window has lapsed', () => {
    const h = makeHarness();
    catchUpWithRemnant(h, 300, 5);
    // Idle past the retain window (350ms) with no target changes; the
    // spring settles (sentinel under 'spring' mode keeps it alive).
    for (let i = 0; i < 30; i++) frame();

    // A fresh growth after the gap starts cold — no carried velocity.
    // Cold spring physics move (0.08·20)/1.25 = 1.28 on the first
    // frame (the envelope, 2.2, doesn't bind), clearly below the
    // envelope-clamped carried-momentum value (2.2) asserted above.
    h.setTarget(h.getTarget() + 20);
    h.spring.markTargetChanged();
    const before = h.getScrollTop();
    frame();
    const firstFrameMove = h.getScrollTop() - before;
    expect(firstFrameMove).toBeGreaterThan(1.2);
    expect(firstFrameMove).toBeLessThan(1.4);
  });
});

describe('glide shaping (decel envelope + fractional tail)', () => {
  // The envelope (0.11 · remaining) bounds the peak and shapes the
  // ease-out; the exponential tail below it is deliberately UNSHAPED —
  // sub-pixel motion renders continuously through the controller's
  // fractional glide residue, so the historical anti-judder floor and
  // settle taper are gone. These tests pin the envelope bound, the
  // natural cradled tail (sub-1px frames the floor used to erase), and
  // the remainder-carry continuity under integer scrollTop rounding.
  function parkAt(h: Harness, target: number): void {
    h.setTarget(target);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 60; i++) frame();
    expect(Math.abs(h.getScrollTop() - target)).toBeLessThanOrEqual(ARRIVAL_DISTANCE_PX);
  }

  /** Frame the chase until within 1px of `target`, returning per-frame moves. */
  function movesUntilNear(h: Harness, target: number, budget: number): number[] {
    const moves: number[] = [];
    for (let i = 0; i < budget && Math.abs(h.getScrollTop() - target) > 1; i++) {
      const before = h.getScrollTop();
      frame();
      moves.push(h.getScrollTop() - before);
    }
    expect(Math.abs(h.getScrollTop() - target)).toBeLessThanOrEqual(1);
    return moves;
  }

  it('bounds the peak with the envelope and lands through a natural sub-pixel tail', () => {
    const h = makeHarness();
    parkAt(h, 100);

    h.setTarget(160); // one sparse ~3-line quantum
    h.spring.markTargetChanged();
    const moves = movesUntilNear(h, 160, 60);

    // Envelope: a 60px quantum peaks near 0.11·remaining (integration
    // peaks ≈5.7 before the clamp binds), never the raw spring's
    // distance-proportional zoom (≈8.7 for 60px).
    for (const move of moves) {
      expect(move).toBeLessThanOrEqual(7);
    }
    // Ease-out: once past the peak, per-frame moves only decelerate —
    // the envelope tracks remaining distance down, then hands off to
    // the spring's own exponential decay. The final frame is excluded:
    // it combines the last decay step with the sentinel-entry exact
    // snap (≤1px arrival band, invisible), so it reads larger than the
    // step before it.
    const peakIndex = moves.indexOf(Math.max(...moves));
    for (let i = peakIndex + 1; i < moves.length - 1; i++) {
      expect(moves[i]).toBeLessThanOrEqual(moves[i - 1] + 0.01);
    }
    expect(moves[moves.length - 1]).toBeLessThanOrEqual(1.05);
    // Cradled tail: the glide ends with genuinely sub-1px frames (the
    // phase the removed anti-judder floor used to truncate). The
    // fractional glide residue is what makes these render smoothly.
    const subPixelTail = moves.filter((m) => m > 0 && m < 1);
    expect(subPixelTail.length).toBeGreaterThanOrEqual(3);
  });

  it('eases a tiny growth in gently without a snap', () => {
    const h = makeHarness();
    parkAt(h, 100);

    h.setTarget(103); // sub-line growth
    h.spring.markTargetChanged();
    const moves = movesUntilNear(h, 103, 20);
    // Cold first frame is pure spring physics, (0.08·3)/1.25 ≈ 0.19 —
    // the envelope min (1.6) is an upper bound, never a forced speed.
    expect(moves[0]).toBeGreaterThan(0.15);
    expect(moves[0]).toBeLessThan(0.25);
    for (const move of moves) {
      expect(move).toBeLessThanOrEqual(1);
      expect(move).toBeGreaterThan(0);
    }
  });

  it('keeps written values continuous under integer scrollTop rounding (remainder carry)', () => {
    // Quantized harness: writes land on whole pixels like the browser.
    // Without the remainder carry, each rounded-DOWN readback dropped
    // the sub-pixel progress and the next written value could regress
    // (write 100.4 → readback 100 → next write 100.2), a ±0.5px
    // sawtooth in the rendered (residue-composited) position at tail
    // speeds. With carry, the written sequence never moves backwards.
    const h = makeHarness({ quantize: true });
    parkAt(h, 100);

    const writesBefore = h.writes.length;
    h.setTarget(160);
    h.spring.markTargetChanged();
    for (let i = 0; i < 80 && Math.abs(h.getScrollTop() - 160) > 0; i++) frame();
    expect(h.getScrollTop()).toBe(160);

    const glideWrites = h.writes
      .slice(writesBefore)
      .filter((w) => w.caller === 'spring.tick' || w.caller === 'spring.overshoot')
      .map((w) => w.value);
    expect(glideWrites.length).toBeGreaterThan(10);
    for (let i = 1; i < glideWrites.length; i++) {
      expect(glideWrites[i]).toBeGreaterThanOrEqual(glideWrites[i - 1]);
    }
  });
});

describe('glide residue clearing', () => {
  it('clears the residue on cancel', () => {
    const h = makeHarness();
    h.setTarget(200);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 5; i++) frame();
    const before = h.residueSettles();
    h.spring.cancel();
    expect(h.residueSettles()).toBe(before + 1);
  });

  it('clears the residue when the chase settles into the sentinel', () => {
    const h = makeHarness();
    h.setTarget(60);
    h.spring.markTargetChanged();
    h.spring.start();
    // Run past arrival + retain lapse; animationMode stays 'spring', so
    // the chase parks in sentinel mode (which must leave text crisp
    // even though no exact write fires once the readback matches).
    for (let i = 0; i < 80; i++) frame();
    expect(Math.abs(h.getScrollTop() - 60)).toBeLessThanOrEqual(ARRIVAL_DISTANCE_PX);
    expect(h.residueSettles()).toBeGreaterThan(0);
    expect(h.spring.isActive()).toBe(true); // sentinel-alive, not cancelled
  });
});

describe('chase telemetry', () => {
  it('emits one scroll.spring.chase summary with frame-gap histogram and clamp counts', () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const h = makeHarness();
    h.setTarget(200);
    h.spring.markTargetChanged();
    h.spring.start();

    for (let i = 0; i < 5; i++) frame(); // healthy 16.67ms cadence
    frame(60); // a stalled frame: dtFrames ≈ 3.6 > catch-up cap, gap > 42ms
    for (let i = 0; i < 3; i++) frame();
    h.spring.cancel();

    const records = getUiRenderTraceRecords().filter(
      (r) => r.label === 'scroll.spring.chase',
    );
    expect(records).toHaveLength(1);
    const data = records[0].data as {
      ticks: number;
      writeTicks: number;
      maxGapMs: number;
      gapBuckets: number[];
      catchupClamps: number;
      targetChanges: number;
      durationMs: number;
    };
    expect(data.ticks).toBe(9);
    expect(data.writeTicks).toBeGreaterThan(0);
    expect(data.maxGapMs).toBeGreaterThanOrEqual(60);
    // Gaps are recorded from the second tick on.
    expect(data.gapBuckets.reduce((a, b) => a + b, 0)).toBe(data.ticks - 1);
    // The 60ms stall lands in the >42ms bucket and clamps catch-up.
    expect(data.gapBuckets[5]).toBe(1);
    expect(data.catchupClamps).toBe(1);
    expect(data.durationMs).toBeGreaterThan(0);
  });

  it('emits nothing when tracing is disabled', () => {
    setUiRenderTraceEnabled(false);
    clearUiRenderTrace();
    const h = makeHarness();
    h.setTarget(200);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 6; i++) frame();
    h.spring.cancel();
    setUiRenderTraceEnabled(true); // records() readable either way; just filter
    expect(
      getUiRenderTraceRecords().filter((r) => r.label === 'scroll.spring.chase'),
    ).toHaveLength(0);
  });
});
