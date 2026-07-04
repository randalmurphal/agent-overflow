// Unit tests for the spring chase kinematics (createSpringChase) that
// need frame-precise control over rAF timing and target geometry:
// the hard velocity cap, the clamp-not-zero momentum carry with its
// adaptive ceiling, and the per-chase telemetry summary. Controller-level
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
}

function makeHarness(): Harness {
  let scrollTop = 0;
  let target = 0;
  let animationMode: 'spring' | 'instant' = 'spring';
  const writes: { caller: string; value: number }[] = [];

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
    writeScrollTop: (caller, value) => {
      writes.push({ caller, value });
      scrollTop = value;
    },
    animationMode: () => animationMode,
    prefersReducedMotion: () => false,
    forceNextSpringTickTrace: () => {},
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

  it('clamps an above-ceiling remnant to the carry floor instead of zeroing it (no dead stop)', () => {
    const h = makeHarness();
    // ~5 frames into a 300px chase the velocity is well above the floor
    // (and mid-chase target moves never feed the quantum EMA, so the
    // adaptive ceiling stays at its floor of 4).
    catchUpWithRemnant(h, 300, 5);

    // Next line-sized growth: first-frame move from carried v=4 is
    // (0.7·4 + 0.08·20)/1.25 ≈ 3.52; a cold start from rest would move
    // (0.08·20)/1.25 = 1.28. The old zeroing produced the cold value.
    h.setTarget(h.getTarget() + 20);
    h.spring.markTargetChanged();
    const before = h.getScrollTop();
    frame();
    const firstFrameMove = h.getScrollTop() - before;
    expect(firstFrameMove).toBeGreaterThan(3.0);
    expect(firstFrameMove).toBeLessThan(4.5);
  });

  it('raises the carry ceiling from growth quanta observed while parked at the bottom', () => {
    const h = makeHarness();
    // Arrive and settle at 100 so subsequent growths sample the quantum
    // EMA (samples only count when the target moves while parked at the
    // previous target).
    h.setTarget(100);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 40; i++) frame();
    expect(Math.abs(h.getScrollTop() - 100)).toBeLessThanOrEqual(ARRIVAL_DISTANCE_PX);

    // Two 100px growths from parked position: EMA → ~100, adaptive
    // ceiling → its max (12).
    h.setTarget(200);
    h.spring.markTargetChanged();
    for (let i = 0; i < 30; i++) frame();
    h.setTarget(300);
    h.spring.markTargetChanged();
    // Build velocity into this chase, then force a catch-up with the
    // remnant (which sits above the floor of 4 after 4 frames).
    for (let i = 0; i < 4; i++) frame();
    h.setTarget(h.getScrollTop());
    h.spring.markTargetChanged();
    frame(); // caught-up tick — remnant clamps to the ADAPTIVE ceiling

    // Next 100px growth: a floor-clamped carry (v=4) would move
    // (0.7·4 + 0.08·100)/1.25 = 8.64 on the first frame. A carry above
    // the floor moves more — proving the adaptive ceiling engaged. The
    // ceiling max (12) bounds it: (0.7·12 + 0.08·100)/1.25 = 13.1.
    h.setTarget(h.getTarget() + 100);
    h.spring.markTargetChanged();
    const before = h.getScrollTop();
    frame();
    const firstFrameMove = h.getScrollTop() - before;
    expect(firstFrameMove).toBeGreaterThan(9.0);
    expect(firstFrameMove).toBeLessThanOrEqual(13.5);
  });

  it('ignores parked shrinks when learning the carry ceiling', () => {
    const h = makeHarness();
    // Park at 100, then let a shrink land while parked (typesetting
    // oscillation). Sampled via |Δ| it would push the adaptive ceiling to
    // its max; shrinks say nothing about the next growth quantum.
    h.setTarget(100);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 40; i++) frame();
    h.setTarget(40);
    h.spring.markTargetChanged();
    frame(); // tick observes the parked shrink

    // Growth resumes before the shrink-follow settles — a mid-chase move,
    // never sampled — then a catch-up leaves an above-floor remnant.
    h.setTarget(340);
    h.spring.markTargetChanged();
    for (let i = 0; i < 5; i++) frame();
    h.setTarget(h.getScrollTop());
    h.spring.markTargetChanged();
    frame(); // caught-up tick — remnant clamps to the ceiling

    // Next line-sized growth: carry must be the floor (4 → first-frame
    // move ≈ 3.52), not a shrink-inflated ceiling (12 → ≈ 8.0).
    h.setTarget(h.getTarget() + 20);
    h.spring.markTargetChanged();
    const before = h.getScrollTop();
    frame();
    const firstFrameMove = h.getScrollTop() - before;
    expect(firstFrameMove).toBeGreaterThan(3.0);
    expect(firstFrameMove).toBeLessThan(4.5);
  });

  it('sheds all momentum once the retain window has lapsed', () => {
    const h = makeHarness();
    catchUpWithRemnant(h, 300, 5);
    // Idle past the retain window (350ms) with no target changes; the
    // spring settles (sentinel under 'spring' mode keeps it alive).
    for (let i = 0; i < 30; i++) frame();

    // A fresh growth after the gap starts cold — no carried velocity.
    h.setTarget(h.getTarget() + 20);
    h.spring.markTargetChanged();
    const before = h.getScrollTop();
    frame();
    const firstFrameMove = h.getScrollTop() - before;
    expect(firstFrameMove).toBeLessThan(1.6); // cold ≈ 1.28
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
