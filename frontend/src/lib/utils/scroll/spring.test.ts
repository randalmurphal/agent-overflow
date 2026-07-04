// Unit tests for the spring chase kinematics (createSpringChase) that
// need frame-precise control over rAF timing and target geometry:
// the hard velocity cap, the minimum glide speed (anti-judder floor),
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

  it('clamps an above-ceiling remnant to the carry ceiling instead of zeroing it (no dead stop)', () => {
    const h = makeHarness();
    // ~5 frames into a 300px chase the velocity is well above the
    // ceiling (4).
    catchUpWithRemnant(h, 300, 5);

    // Next line-sized growth: first-frame move from carried v=4 is
    // (0.7·4 + 0.08·20)/1.25 ≈ 3.52; a cold start would move only the
    // speed floor (2.5). The old zeroing produced the cold value.
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
    // Cold spring physics would move (0.08·20)/1.25 = 1.28; the speed
    // floor lifts that first frame to exactly 2.5, still clearly below
    // the carried-momentum value (≈3.52) asserted above.
    h.setTarget(h.getTarget() + 20);
    h.spring.markTargetChanged();
    const before = h.getScrollTop();
    frame();
    const firstFrameMove = h.getScrollTop() - before;
    expect(firstFrameMove).toBeGreaterThan(2.3);
    expect(firstFrameMove).toBeLessThan(2.7);
  });
});

describe('minimum glide speed', () => {
  // scrollTop is integer-quantized by the engine, so any sustained
  // sub-2.5px/frame (60Hz units) phase renders as 1px steps at a low
  // effective rate — the judder a 2026-07-04 165Hz capture pinned as
  // the perceived "low fps". These tests pin that the ease-out tail is
  // held at the floor and terminates with a bounded landing step.
  function parkAt(h: Harness, target: number): void {
    h.setTarget(target);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 40; i++) frame();
    expect(Math.abs(h.getScrollTop() - target)).toBeLessThanOrEqual(ARRIVAL_DISTANCE_PX);
  }

  it('holds the ease-out tail at the floor instead of a sub-pixel crawl', () => {
    const h = makeHarness();
    parkAt(h, 100);

    h.setTarget(160); // one sparse ~3-line quantum
    h.spring.markTargetChanged();
    const moves = displacements(h, 40).filter((m) => m !== 0);
    expect(h.getScrollTop()).toBe(160);
    // Every animated frame except the final landing remainder moves at
    // least the floor (2.5px per 60Hz frame). Without the floor the
    // exponential tail spends ~15 frames below 1px/frame.
    const landing = moves[moves.length - 1];
    for (const move of moves.slice(0, -1)) {
      expect(move).toBeGreaterThanOrEqual(2.4);
    }
    // The landing step is the leftover distance — never larger than one
    // floor-speed step, so it reads like any other step.
    expect(landing).toBeLessThanOrEqual(2.6);
    expect(landing).toBeGreaterThan(0);
  });

  it('lands a tiny growth in a couple of floor-speed steps without a snap', () => {
    const h = makeHarness();
    parkAt(h, 100);

    h.setTarget(103); // sub-line growth
    h.spring.markTargetChanged();
    const moves = displacements(h, 5).filter((m) => m !== 0);
    expect(h.getScrollTop()).toBe(103);
    expect(moves.length).toBeLessThanOrEqual(2);
    for (const move of moves) {
      expect(move).toBeLessThanOrEqual(2.6); // no single-frame teleport
    }
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
