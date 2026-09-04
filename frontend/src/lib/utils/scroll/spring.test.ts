// Unit tests for the spring chase kinematics (createSpringChase) that
// need frame-precise control over rAF timing and target geometry:
// the hard velocity cap, the deceleration envelope, the acceleration
// slew (onset ramp, parked carry decay, moving-target retarget bridge,
// seed exemption, integrator
// composability), the integer-rounding error carry, the clamp-not-zero
// momentum carry, the write-refusal
// guard (latch, probe backoff, three-way write classification, heal /
// abandon reporting), and the per-chase
// telemetry summary. Controller-level
// choreography (when a chase runs) lives in index.svelte.test.ts; these
// tests pin HOW a chase advances, driven through a fake deps harness.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  __resetSpringFrameBatcherForTest,
  createSpringChase,
  SPRING_QUANTIZED_MOTION_FLOOR_PX_PER_FRAME,
  quantizedFloorRung,
  SPRING_WRITE_REFUSAL_LATCH_TICKS,
  SPRING_WRITE_REFUSAL_PROBE_INTERVAL_MS,
  type ArrivalReadback,
  type SpringChaseDeps,
  type SpringWriteRefusalEvent,
} from './spring';
import { setDocumentResumeAtForTest } from './documentResume';
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
  refusalEvents: SpringWriteRefusalEvent[];
  getScrollTop(): number;
  /** The model's velocity after the last tick (px per 60Hz frame). */
  velocity(): number;
  setTarget(value: number): void;
  getTarget(): number;
  setLiveContentActive(active: boolean): void;
  setSelectionActive(active: boolean): void;
  setRefuseWrites(refuse: boolean): void;
}

/**
 * `quantize: true` models the browser's scrollTop contract: a write lands
 * on the engine's pixel grid — whole CSS pixels at the default `dpr` of
 * 1, whole DEVICE pixels otherwise (the Pixel 9a probe, 2026-09-04:
 * 1/2.625 CSS px steps) — and readbacks return that snapped value. The
 * default (fractional storage) keeps the kinematic assertions exact.
 * `dpr` is also what the spring's `devicePixelRatio` dep reports.
 *
 * `clientHeight` (default 0 = unmeasured) arms the chase-distance clamp;
 * the kinematic tests leave it off so their long-glide assertions stay
 * exact.
 */
function makeHarness(
  opts: {
    quantize?: boolean;
    dpr?: number;
    /**
     * The engine rounds every write to a whole CSS pixel whatever the
     * device ratio (desktop Chromium at DPR 2, per the browser test).
     * Implies `quantize`.
     */
    cssGrid?: boolean;
    clientHeight?: number;
    refuse?: boolean;
    /** Engine max-scroll clamp: stored scrollTop never exceeds this. */
    clampMax?: number;
  } = {},
): Harness {
  const dpr = opts.dpr ?? 1;
  let scrollTop = 0;
  let target = 0;
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
      const quantized = opts.cssGrid
        ? Math.round(value)
        : opts.quantize
          ? Math.round(value * dpr) / dpr
          : value;
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
    selectionActive: () => selectionActive,
    targetScrollTop: () => target,
    currentScrollTop: () => scrollTop,
    scrollTopIsAtTarget: (t) => Math.abs(scrollTop - t) <= ARRIVAL_DISTANCE_PX,
    arrival,
    writeScrollTop: (caller, value) => {
      writes.push({ caller, value });
      store(value);
      return scrollTop;
    },
    liveContentActive: () => liveContentActive,
    prefersReducedMotion: () => false,
    devicePixelRatio: () => dpr,
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
function displacements(h: Harness, frames: number): number[] {
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
 * suites (ramp ratios, envelope, retarget bridge, parked decay) pin the
 * MODEL: what reaches scrollTop is whole device pixels, which cannot
 * show a 10% per-frame ratio (see SpringChase.velocityForTest).
 */
function velocities(h: Harness, frames: number, ms = 16.67): number[] {
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

describe('spring frame ownership', () => {
  it('coalesces concurrent pane chases onto one native animation frame', () => {
    const left = makeHarness();
    const right = makeHarness();
    left.setTarget(300);
    right.setTarget(450);
    left.spring.markTargetChanged();
    right.spring.markTargetChanged();

    left.spring.start();
    right.spring.start();

    expect(rafQueue).toHaveLength(1);
    frame();
    expect(left.getScrollTop()).toBeGreaterThan(0);
    expect(right.getScrollTop()).toBeGreaterThan(0);
    expect(rafQueue).toHaveLength(1);
  });
});

describe('spring velocity cap', () => {
  it('bounds per-frame displacement on a large chase instead of a distance-proportional zoom', () => {
    const h = makeHarness();
    h.setTarget(900);
    h.spring.markTargetChanged();
    h.spring.start();

    const moves = displacements(h, 90);
    // Uncapped, a 900px chase peaks near ~0.12·D ≈ 100px/frame. The cap
    // holds every frame to SPRING_MAX_VELOCITY_PX_PER_FRAME (27) — small
    // epsilon for the fractional-step integration.
    for (const move of moves) {
      expect(move).toBeLessThanOrEqual(27.5);
    }
    // The slew ramp spools floor→cap over ~29 frames; the chase then
    // genuinely cruises AT the cap and still gets there.
    expect(Math.max(...moves)).toBeGreaterThan(26.5);
    expect(h.getScrollTop()).toBeGreaterThan(850);
  });

  it('bounds a stalled frame to a single step instead of paying the whole gap', () => {
    const h = makeHarness();
    h.setTarget(900);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 35; i++) frame(); // spool past the slew ramp to capped cruise

    const before = h.getScrollTop();
    frame(100); // stall: dtFrames = 6, clamped to SPRING_MAX_CATCHUP_STEPS (1)
    const move = h.getScrollTop() - before;
    // ONE capped step: the resume tick advances no further than an
    // ordinary cruising frame, so a stall can never present as a jump.
    // Stalls are routine on WebKit (2026-08-05 probe: 6–7 per chase), and
    // the lost time is recovered by the spring's distance term over the
    // following frames, not paid off in this write.
    expect(move).toBeLessThanOrEqual(27.5);
    // Still real motion — a stall must not stall the chase.
    expect(move).toBeGreaterThan(20);
  });

  it('caps downward (shrink-follow) chases symmetrically', () => {
    const h = makeHarness();
    h.setTarget(900);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 70; i++) frame();
    expect(h.getScrollTop()).toBeGreaterThan(850);

    // Content shrinks far below the current position mid-follow.
    h.setTarget(100);
    h.spring.markTargetChanged();
    const moves = displacements(h, 40);
    for (const move of moves) {
      expect(move).toBeGreaterThanOrEqual(-27.5);
    }
  });
});

describe('resume snap', () => {
  function catchupSnaps(h: Harness): { caller: string; value: number }[] {
    return h.writes.filter((write) => write.caller === 'spring.catchupSnap');
  }

  it('a live >viewport structural mount NEVER snaps — full bounded glide (hard requirement)', () => {
    const h = makeHarness({ clientHeight: 600 });
    // A huge diff card mounts in one frame mid-follow: rAF is ticking
    // normally and the document never went hidden. Distance alone must
    // not be read as a stall.
    h.setTarget(5000);
    h.spring.markTargetChanged();
    h.spring.start();

    const moves = displacements(h, 200);
    expect(catchupSnaps(h)).toHaveLength(0);
    for (const move of moves) {
      expect(move).toBeLessThanOrEqual(27.5);
    }
    expect(h.getScrollTop()).toBeGreaterThan(4700);
  });

  it('snaps the whole backlog when the tick carries a real rAF stall gap', () => {
    const h = makeHarness({ clientHeight: 600 });
    h.setTarget(300);
    h.spring.markTargetChanged();
    h.spring.start();
    frame();
    frame(); // chase established; lastTickAt is live

    // Occlusion: rAF frozen for 5s while content grew far past the
    // viewport. The first resumed tick carries the whole gap; nothing in
    // the backlog was watched, so nothing of it animates.
    h.setTarget(6000);
    h.spring.markTargetChanged();
    frame(5000);

    const snaps = catchupSnaps(h);
    expect(snaps).toHaveLength(1);
    expect(snaps[0].value).toBe(6000); // the full target — zero residual glide
    expect(h.getScrollTop()).toBe(6000);
    // Nothing left to chase: the frames after the snap hold still.
    const moves = displacements(h, 10);
    for (const move of moves) {
      expect(Math.abs(move)).toBeLessThanOrEqual(1);
    }
  });

  it('growth arriving after the snap ramps up as a cold onset, not carried cruise', () => {
    const h = makeHarness({ clientHeight: 600 });
    h.setTarget(300);
    h.spring.markTargetChanged();
    h.spring.start();
    frame();
    frame();
    h.setTarget(6000);
    h.spring.markTargetChanged();
    frame(5000); // snap lands at 6000, velocity reset to standstill

    // Live content resumes: the next growth is ordinary follow and must
    // start from the acceleration ramp, not inherit cap speed.
    h.setTarget(6400);
    h.spring.markTargetChanged();
    const moves = displacements(h, 3);
    expect(Math.abs(moves[0])).toBeLessThan(10);
  });

  it('snaps a fresh chase started shortly after the document resumed visibility', () => {
    const h = makeHarness({ clientHeight: 600 });
    // visibilitychange → visible fired just now (text smoothers snapped
    // to the wire, creating the backlog); the chase starts fresh with no
    // prior tick to carry the rAF gap.
    setDocumentResumeAtForTest(now);
    h.setTarget(5000);
    h.spring.markTargetChanged();
    h.spring.start();

    frame();
    expect(catchupSnaps(h)[0]?.value).toBe(5000);
    expect(h.getScrollTop()).toBe(5000);
  });

  it('stops snapping a fresh chase once the resume window has passed', () => {
    // The resume snap treats every fresh >viewport chase within
    // RESUME_CLAMP_WINDOW_MS (2000ms) of a visibilitychange→visible as
    // tab-return backlog and places it instantly — a deliberate tradeoff
    // that also snaps a legitimate large structural mount (big diff
    // card) landing in that window. This pins the window's EDGE: past
    // 2000ms the same chase is a real mount again and must glide from
    // its true start with no cut.
    const h = makeHarness({ clientHeight: 600 });
    setDocumentResumeAtForTest(now);
    now += 2001; // idle past the window, no spring running
    h.setTarget(5000);
    h.spring.markTargetChanged();
    h.spring.start();
    frame();
    expect(catchupSnaps(h)).toHaveLength(0);
  });

  it('never engages on a sub-viewport backlog even under a stall gap', () => {
    const h = makeHarness({ clientHeight: 600 });
    h.setTarget(100);
    h.spring.markTargetChanged();
    h.spring.start();
    frame();

    h.setTarget(500);
    h.spring.markTargetChanged();
    frame(5000);
    for (let i = 0; i < 60; i++) frame();
    expect(catchupSnaps(h)).toHaveLength(0);
    expect(h.getScrollTop()).toBeGreaterThan(450);
  });

  it('snaps a shrink-follow backlog symmetrically', () => {
    const h = makeHarness({ clientHeight: 600 });
    h.setTarget(900);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 70; i++) frame();
    expect(h.getScrollTop()).toBeGreaterThan(850);

    // Content collapses far above the current position while rAF was
    // frozen (stall gap on the resumed tick).
    h.setTarget(100);
    h.spring.markTargetChanged();
    frame(5000);
    expect(catchupSnaps(h)[0]?.value).toBe(100);
    expect(h.getScrollTop()).toBe(100);
  });

  it('stays inert when the viewport is unmeasured (clientHeight 0)', () => {
    const h = makeHarness();
    h.setTarget(300);
    h.spring.markTargetChanged();
    h.spring.start();
    frame();

    h.setTarget(5000);
    h.spring.markTargetChanged();
    frame(5000);
    for (let i = 0; i < 8; i++) frame();
    expect(catchupSnaps(h)).toHaveLength(0);
    // Old behavior preserved: bounded glide from the original position.
    expect(h.getScrollTop()).toBeLessThan(600);
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
    // ~20 frames into a 300px chase the slew ramp has the velocity well
    // above the ceiling (4).
    catchUpWithRemnant(h, 300, 20);

    // Next line-sized growth: the carried remnant (clamped to 4, one
    // parked-frame decay → ~3.57) integrates well above the decel
    // envelope, which shapes the first frame to 0.09·20 = 1.8. A cold
    // start (the old zeroing) would enter at the floor-based slew ramp
    // (~1.10, asserted below) — carry is what keeps the next quantum
    // measurably in motion.
    h.setTarget(h.getTarget() + 20);
    h.spring.markTargetChanged();
    frame();
    const firstFrameVelocity = h.velocity();
    expect(firstFrameVelocity).toBeGreaterThan(1.7);
    expect(firstFrameVelocity).toBeLessThan(1.9);
  });

  it('sheds all momentum once the retain window has lapsed', () => {
    const h = makeHarness();
    catchUpWithRemnant(h, 300, 20);
    // Idle past the retain window (350ms) with no target changes; the
    // spring settles (sentinel under 'spring' mode keeps it alive).
    for (let i = 0; i < 30; i++) frame();

    // A fresh growth after the gap starts cold — no carried velocity.
    // A cold start's first frame is the floor-based slew ramp: the
    // 60Hz phase-lock floor (1.0) × the ramp factor (1.10) ≈ 1.10,
    // clearly below the envelope-clamped carried-momentum value (1.8)
    // asserted above.
    h.setTarget(h.getTarget() + 20);
    h.spring.markTargetChanged();
    frame();
    const firstFrameVelocity = h.velocity();
    expect(firstFrameVelocity).toBeGreaterThan(1.05);
    expect(firstFrameVelocity).toBeLessThan(1.2);
  });
});

describe('acceleration slew (onset ramp + retarget bridge + parked decay)', () => {
  it('ramps a standstill onset geometrically from the motion floor instead of jumping to the envelope peak', () => {
    const h = makeHarness();
    h.setTarget(60);
    h.spring.markTargetChanged();
    h.spring.start();

    const speeds = velocities(h, 6);
    // Pre-slew, the first frame jumped straight to min(raw spring
    // ≈3.8, envelope 6.6). Slewed: the 1px-per-60Hz-frame motion floor
    // × the ramp factor, compounding ~10% per frame.
    expect(speeds[0]).toBeGreaterThan(1.0);
    expect(speeds[0]).toBeLessThan(1.2);
    for (let i = 1; i < speeds.length; i++) {
      const ratio = speeds[i] / speeds[i - 1];
      expect(ratio).toBeGreaterThan(1.08);
      expect(ratio).toBeLessThan(1.16);
    }
  });

  it('shapes a paragraph-sized quantum as ease-in-out: rise to a mid-glide peak, then fall', () => {
    const h = makeHarness();
    h.setTarget(100);
    h.spring.markTargetChanged();
    h.spring.start();

    const moves: number[] = [];
    for (let i = 0; i < 60 && Math.abs(h.getScrollTop() - 100) > 1; i++) {
      const before = h.getScrollTop();
      frame();
      moves.push(h.getScrollTop() - before);
    }
    expect(Math.abs(h.getScrollTop() - 100)).toBeLessThanOrEqual(1);
    // The peak is the slew↔envelope crossover — mid-glide, not frame
    // one. Rise is monotone (the ramp), fall is monotone (the
    // envelope); the final arrival frame is excluded since it folds in
    // the sentinel-entry exact snap.
    const peakIndex = moves.indexOf(Math.max(...moves));
    expect(peakIndex).toBeGreaterThan(5);
    for (let i = 1; i <= peakIndex; i++) {
      expect(moves[i]).toBeGreaterThan(moves[i - 1] - 0.01);
    }
    for (let i = peakIndex + 1; i < moves.length - 1; i++) {
      expect(moves[i]).toBeLessThanOrEqual(moves[i - 1] + 0.01);
    }
  });

  it('rounds a streamed retarget from deceleration back into acceleration', () => {
    const h = makeHarness();
    h.setTarget(100);
    h.spring.markTargetChanged();
    h.spring.start();

    let priorSpeed = 0;
    let priorAcceleration = 0;
    let consecutiveDecelFrames = 0;
    for (let i = 0; i < 60 && consecutiveDecelFrames < 3; i++) {
      const speed = velocities(h, 1)[0];
      const acceleration = speed - priorSpeed;
      consecutiveDecelFrames = acceleration < -0.05 ? consecutiveDecelFrames + 1 : 0;
      priorSpeed = speed;
      priorAcceleration = acceleration;
    }
    expect(consecutiveDecelFrames).toBe(3);
    expect(priorSpeed).toBeGreaterThan(1);
    expect(priorAcceleration).toBeLessThan(-0.05);

    // A streamed burst extends the target while the viewport is still
    // moving forward but already braking. Position and velocity remain
    // continuous. Acceleration must round through zero before becoming
    // positive again, rather than flipping sign in one frame as a pulse.
    h.setTarget(h.getTarget() + 100);
    h.spring.markTargetChanged();

    const accelerations: number[] = [];
    const speeds: number[] = [];
    for (let i = 0; i < 12; i++) {
      if (i === 2) {
        // A second flush lands before the first bridge reaches zero
        // acceleration. Retargeting an active bridge must update its
        // destination without restarting its curvature.
        h.setTarget(h.getTarget() + 100);
        h.spring.markTargetChanged();
      }
      const speed = velocities(h, 1)[0];
      accelerations.push(speed - priorSpeed);
      speeds.push(speed);
      priorSpeed = speed;
    }
    expect(accelerations[0]).toBeLessThanOrEqual(0);
    expect(accelerations.some((value) => value > 0.05)).toBe(true);
    expect(speeds.every((value) => value > 0)).toBe(true);
    const handoffAccelerations = [priorAcceleration, ...accelerations];
    for (let i = 1; i < handoffAccelerations.length; i++) {
      expect(Math.abs(handoffAccelerations[i] - handoffAccelerations[i - 1]))
        .toBeLessThanOrEqual(0.125);
    }
  });

  it('scales the retarget bridge without nearly stopping a cap-speed glide', () => {
    const h = makeHarness();
    h.setTarget(1200);
    h.spring.markTargetChanged();
    h.spring.start();

    let speed = 0;
    let acceleration = 0;
    let consecutiveDecelFrames = 0;
    for (let i = 0; i < 150 && consecutiveDecelFrames < 3; i++) {
      const nextSpeed = velocities(h, 1)[0];
      acceleration = nextSpeed - speed;
      speed = nextSpeed;
      consecutiveDecelFrames = acceleration < -0.05 ? consecutiveDecelFrames + 1 : 0;
    }
    expect(speed).toBeGreaterThan(15);
    expect(acceleration).toBeLessThan(-1);

    h.setTarget(h.getTarget() + 1200);
    h.spring.markTargetChanged();
    const handoffAccelerations = [acceleration];
    const handoffSpeeds: number[] = [];
    for (let i = 0; i < 18; i++) {
      const nextSpeed = velocities(h, 1)[0];
      handoffAccelerations.push(nextSpeed - speed);
      handoffSpeeds.push(nextSpeed);
      speed = nextSpeed;
    }

    expect(handoffAccelerations[1]).toBeLessThan(0);
    expect(handoffAccelerations.some((value) => value > 0)).toBe(true);
    expect(Math.min(...handoffSpeeds)).toBeGreaterThan(10);
    // Relative jerk scaling keeps a large handoff responsive while still
    // taking many intermediate acceleration steps. The old one-frame
    // flip had only the two endpoint values.
    const increasingSteps = handoffAccelerations
      .slice(1)
      .filter((value, index) => value > handoffAccelerations[index]);
    expect(increasingSteps.length).toBeGreaterThanOrEqual(8);
  });

  it.each([60, 120, 165])(
    'bounds every braking-to-driving retarget at %iHz during repeated line growth',
    (refreshHz) => {
      const h = makeHarness();
      const frameMs = 1000 / refreshHz;
      const stepFraction = 60 / refreshHz;
      const framesPerGrowth = Math.round(8 / stepFraction);
      h.setTarget(20);
      h.spring.markTargetChanged();
      h.spring.start();

      let priorSpeed = 0;
      const accelerations: number[] = [];
      const speeds: number[] = [];
      const steps: number[] = [];
      const frameCount = framesPerGrowth * 8;
      for (let i = 0; i < frameCount; i++) {
        if (i > 0 && i % framesPerGrowth === 0) {
          h.setTarget(h.getTarget() + 20);
          h.spring.markTargetChanged();
        }
        const before = h.getScrollTop();
        frame(frameMs);
        // The first spring tick always integrates one full 60Hz step.
        // It is outside the warm repeated-retarget window below.
        const speed = h.velocity();
        accelerations.push((speed - priorSpeed) / stepFraction);
        speeds.push(speed);
        steps.push(h.getScrollTop() - before);
        priorSpeed = speed;
      }

      const warmStart = framesPerGrowth * 4;
      // The velocity is the model; the ladder rounds what it writes to a
      // whole pixel a tick, and the deceleration envelope reads the
      // remaining distance off that grid. So a tick right after the
      // written rung changed carries the envelope's own stair — the
      // one acceleration corner whole-pixel feedback licenses — and
      // every other tick, the retargets included, stays under the
      // bridge's jerk.
      for (let i = warmStart + 1; i < accelerations.length; i++) {
        if (steps[i - 1] !== steps[i - 2]) continue;
        const upwardJerk = (accelerations[i] - accelerations[i - 1]) / stepFraction;
        expect(upwardJerk, `tick ${i}`).toBeLessThanOrEqual(0.105);
      }
      expect(speeds.slice(warmStart).every((value) => value > 0)).toBe(true);
      // And the grid itself: whole pixels forward, rungs changing by one
      // at a time, no tick idle longer than the rate's floor cadence.
      const floorRung = quantizedFloorRung(1, stepFraction);
      let idleRun = 0;
      for (let i = warmStart; i < steps.length; i++) {
        expect(Number.isInteger(steps[i]), `tick ${i}`).toBe(true);
        expect(steps[i], `tick ${i}`).toBeGreaterThanOrEqual(0);
        expect(Math.abs(steps[i] - steps[i - 1]), `tick ${i}`).toBeLessThanOrEqual(1);
        idleRun = steps[i] === 0 ? idleRun + 1 : 0;
        expect(idleRun, `tick ${i}`).toBeLessThan(floorRung.framesPerEvent);
      }
    },
  );

  it('keeps the rounded handoff moving under whole-pixel scrollTop quantization', () => {
    const h = makeHarness({ quantize: true });
    h.setTarget(20);
    h.spring.markTargetChanged();
    h.spring.start();

    const moves: number[] = [];
    for (let i = 0; i < 64; i++) {
      if (i > 0 && i % 8 === 0) {
        h.setTarget(h.getTarget() + 20);
        h.spring.markTargetChanged();
      }
      moves.push(displacements(h, 1)[0]);
    }

    // Error diffusion turns the fractional curve into 1/2/3px writes,
    // but the bridge must not introduce a visible pause or reversal at
    // the retarget boundaries once the steady stream is established.
    for (const move of moves.slice(32)) {
      expect(move).toBeGreaterThan(0);
    }
  });

  it('decays parked carry toward the floor: a long pause re-enters at the floor ramp, not at speed', () => {
    const h = makeHarness();
    // Build real speed, then park caught-up (target moved to the
    // current position) while target changes keep the retain window
    // open — the carry is licensed the whole time, but each parked
    // frame divides it down toward the floor.
    h.setTarget(300);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 20; i++) frame();
    h.setTarget(h.getScrollTop());
    for (let i = 0; i < 16; i++) {
      h.spring.markTargetChanged();
      frame();
    }

    // 16 parked frames decay the carried ceiling (4) to the base, so
    // the next quantum enters at the standstill ramp (~1.10) — a pause
    // long enough to read as stillness must not relaunch at speed.
    h.setTarget(h.getTarget() + 20);
    h.spring.markTargetChanged();
    frame();
    const entry = h.velocity();
    expect(entry).toBeGreaterThan(1.0);
    expect(entry).toBeLessThan(1.3);
  });

  it('advances the ramp by wall time, not tick count, on high-refresh displays', () => {
    // 60Hz vs 120Hz drives of the same chase land at ~the same position
    // after the same wall time: the ramp compounds G^stepFraction. In
    // this ceiling-dominated regime the tolerance pins the RAMP's time
    // scaling only — the integrator's own composability is pinned
    // separately by the ceiling-free test below. The motion floor is
    // display-independent, isolating time scaling from quantized motion.
    // 1s window over a long chase: ramp (~0.5s) + cruise. Bounded
    // one-off artifacts — the first tick integrates a full 60Hz step in
    // both drives (no prior timestamp), handing the finer drive ~half a
    // frame of ramp head start — stay constant while distance grows, so
    // the tolerance is meaningful here where a broken time scaling
    // (e.g. per-tick instead of per-fraction compounding) would diverge
    // by the ramp's whole shape.
    const run = (frameMs: number): number => {
      const h = makeHarness();
      h.setTarget(2000);
      h.spring.markTargetChanged();
      h.spring.start();
      for (let elapsed = 0; elapsed < 1000; elapsed += frameMs) frame(frameMs);
      return h.getScrollTop();
    };
    const at60 = run(1000 / 60);
    const at120 = run(1000 / 120);
    expect(at60).toBeGreaterThan(600); // ramp + genuine cruise both ran
    expect(Math.abs(at60 - at120)).toBeLessThan(at60 * 0.05);
  });

  it('integrates composably across fractional steps (60Hz vs 120Hz, ceiling-free regime)', () => {
    // A 6px quantum keeps every velocity below the slew base ramp
    // (1.10), the envelope min (1.6), and the motion floor — pure
    // spring physics, so this directly pins the integrator's
    // fractional-step composability: retention must be
    // (damping/mass)^f, not (damping^f)/mass. The historical form's
    // effective 120Hz retention was 0.448/frame vs the tuned 0.56 —
    // ~20% velocity bleed per extra step — which diverges far outside
    // this tolerance.
    // A 100× grid (device pixels of 0.01 CSS px) keeps the ladder's
    // whole-pixel rounding out of a position comparison that is about
    // the integrator alone.
    const run = (frameMs: number): number => {
      const h = makeHarness({ dpr: 100 });
      h.setTarget(6);
      h.spring.markTargetChanged();
      h.spring.start();
      // Identical first tick in both drives: a chase's first tick has
      // no prior timestamp and integrates one full frame regardless of
      // cadence — a start transient, not a composability property.
      frame();
      // Then exactly 100ms of wall time at each cadence.
      const count = Math.round(100 / frameMs);
      for (let i = 0; i < count; i++) frame(frameMs);
      return h.getScrollTop();
    };
    const at60 = run(1000 / 60);
    const at120 = run(1000 / 120);
    expect(at60).toBeGreaterThan(2); // meaningfully mid-glide, not arrived
    expect(at60).toBeLessThan(5.5);
    // Velocity composes exactly; the ~2% residual is position sampling
    // (displacement per step is end-of-step velocity × fraction, so
    // finer steps undershoot slightly while velocity ramps — bounded,
    // decaying, sub-pixel). The historical integrator's per-step mass
    // divide bled ~20% velocity per extra step and lands far outside
    // this bound.
    expect(Math.abs(at60 - at120)).toBeLessThan(at60 * 0.04);
  });

  it('re-ramps a reversal from the base: the flipped direction never opens with a hard attack', () => {
    const h = makeHarness();
    // Build real upward speed (~9.6 px/frame at frame 20), then flip
    // the target far below the current position mid-chase.
    h.setTarget(300);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 20; i++) frame();
    expect(h.getScrollTop()).toBeGreaterThan(50);
    h.setTarget(0);
    h.spring.markTargetChanged();

    // The old velocity sheds through zero on the spring curve
    // (deceleration is never slew-limited), then the downward leg
    // ramps geometrically from the base — without the negative-side
    // clamp the first downward frame would be ~-5 (raw spring), a
    // visible kick.
    const speeds = velocities(h, 6);
    const firstDown = speeds.findIndex((v) => v < 0);
    expect(firstDown).toBeGreaterThanOrEqual(0);
    expect(firstDown).toBeLessThanOrEqual(2);
    expect(speeds[firstDown]).toBeGreaterThanOrEqual(-1.2);
    for (let i = firstDown + 1; i < speeds.length; i++) {
      const ratio = speeds[i] / speeds[i - 1];
      expect(ratio).toBeGreaterThan(1.05);
      expect(ratio).toBeLessThan(1.2);
    }
  });

  it('starts from the one-pixel motion floor at standstill', () => {
    const h = makeHarness();
    h.setTarget(60);
    h.spring.markTargetChanged();
    h.spring.start();
    const speeds = velocities(h, 3);
    expect(speeds[0]).toBeGreaterThan(1.0);
    expect(speeds[0]).toBeLessThan(1.2);
    expect(speeds[1] / speeds[0]).toBeGreaterThan(1.08);
    expect(speeds[1] / speeds[0]).toBeLessThan(1.16);
  });

  it('counts a long-task gap while parked in full: decay uses real elapsed time, not the catch-up cap', () => {
    const h = makeHarness();
    h.setTarget(300);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 20; i++) frame();
    // Park caught-up, then one 250ms stalled tick (15 frames of real
    // time in a single rAF callback). The carried ceiling (4) must
    // decay over all 15 frames — the 3-step integration cap bounds
    // motion, not decay — reaching the ramp base.
    h.setTarget(h.getScrollTop());
    h.spring.markTargetChanged();
    frame(250);

    h.setTarget(h.getTarget() + 20);
    h.spring.markTargetChanged();
    frame();
    const entry = h.velocity();
    expect(entry).toBeGreaterThan(1.0);
    expect(entry).toBeLessThan(1.3);
  });
});

describe('glide shaping (decel envelope + quantized tail)', () => {
  // The peak sits at the slew-ramp ↔ envelope crossover; the envelope
  // (0.09 · remaining) shapes the ease-out. The motion floor holds its
  // whole-pixel tail until the landing (owner ruling 2026-09-04: no
  // sub-pixel cradle, the floor stops rather than jitters). These tests
  // pin the envelope bound, the floor, and write continuity under
  // integer scrollTop rounding.
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

  it('bounds the peak with the envelope and holds the motion floor through the tail', () => {
    const h = makeHarness();
    parkAt(h, 100);

    h.setTarget(160); // one sparse ~3-line quantum
    h.spring.markTargetChanged();
    const moves = movesUntilNear(h, 160, 60);

    // Envelope: a 60px quantum's peak is where the slew ramp crosses
    // the falling envelope (≈2.5–3), never the raw spring's
    // distance-proportional zoom (≈8.7 for 60px).
    for (const move of moves) {
      expect(move).toBeLessThanOrEqual(7);
    }
    // Ease-out: once past the peak, per-frame moves only decelerate —
    // the envelope tracks remaining distance down, decays naturally,
    // then plateaus at the motion floor (equal frames allowed). The
    // final frame is excluded: it combines the last decay step with
    // the sentinel-entry exact snap (≤1px arrival band, invisible), so
    // it reads larger than the step before it.
    const peakIndex = moves.indexOf(Math.max(...moves));
    for (let i = peakIndex + 1; i < moves.length - 1; i++) {
      expect(moves[i]).toBeLessThanOrEqual(moves[i - 1] + 0.01);
    }
    expect(moves[moves.length - 1]).toBeLessThanOrEqual(1.55);
    // Motion-floor hold: the deceleration parks at 1.0 CSS px per
    // 60Hz-equivalent frame at this harness's
    // 16.67ms cadence) instead of decaying through it.
    const floorHold = moves.filter((m) => m > 0.98 && m < 1.02);
    expect(floorHold.length).toBeGreaterThanOrEqual(2);
    // A decelerating glide never dwells below one CSS pixel per 60Hz
    // frame: the floor holds to the landing, and the only write under a
    // pixel is the exact landing itself (the final frame, excluded).
    const breathingBand = moves.slice(0, -1).filter((m) => m > 0.05 && m < 0.95);
    expect(breathingBand).toEqual([]);
  });

  it('eases a tiny growth in gently without a snap', () => {
    const h = makeHarness();
    parkAt(h, 100);

    h.setTarget(103); // sub-line growth
    h.spring.markTargetChanged();
    const before = h.getScrollTop();
    frame();
    // Cold first frame is pure spring physics, (0.08·3)/1.25 ≈ 0.19 —
    // the envelope min (1.6) is an upper bound, never a forced speed —
    // under half a pixel, so the grid shows nothing yet.
    expect(h.velocity()).toBeGreaterThan(0.15);
    expect(h.velocity()).toBeLessThan(0.25);
    expect(h.getScrollTop()).toBe(before);
    // Then whole pixels accrue from that curve: never the 3px in one
    // frame, never a step backwards.
    const moves = movesUntilNear(h, 103, 20);
    expect(moves.length).toBeGreaterThanOrEqual(2);
    for (const move of moves) {
      expect(move).toBeLessThanOrEqual(1);
      expect(move).toBeGreaterThanOrEqual(0);
    }
  });

  it('keeps written values continuous under integer scrollTop rounding (remainder carry)', () => {
    // Quantized harness: writes land on whole pixels like the browser.
    // Without the remainder carry, each rounded-DOWN readback dropped
    // the sub-pixel progress and the next written value could regress
    // (write 100.4 → readback 100 → next write 100.2), a ±0.5px
    // sawtooth in requested positions at tail speeds. With carry, the
    // written sequence never moves backwards.
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

describe('quantized motion floor', () => {
  it('derives the floor rung from the 60px/s reference on each grid and cadence', () => {
    expect(SPRING_QUANTIZED_MOTION_FLOOR_PX_PER_FRAME).toBe(1);
    // [dpr, refreshHz, device px per event, frames per event]: the
    // regular cadence closest in ratio to 60px/s, never under 60
    // position changes a second.
    const table: [number, number, number, number][] = [
      [1, 60, 1, 1], // 60px/s
      [2, 60, 2, 1], // 60px/s
      [1, 120, 1, 2], // 60px/s
      [1, 240, 1, 4], // 60px/s
      [1, 144, 1, 2], // 72px/s
      [1, 165, 1, 2], // 82px/s: one in three would be 55 changes/s
      [1, 90, 1, 1], // 90px/s: one in two would be 45 changes/s
      [1.25, 60, 1, 1], // 48px/s
      [1.5, 60, 2, 1], // 80px/s: one device pixel would be 40
      [2.625, 120, 1, 1], // 46px/s, the Pixel 9a
      [2.625, 60, 3, 1], // 69px/s
      [3, 120, 2, 1], // 80px/s
      [0.8, 240, 1, 4], // zoomed out: 75px/s, held to 60 changes/s
    ];
    for (const [dpr, hz, pxPerEvent, framesPerEvent] of table) {
      expect(quantizedFloorRung(dpr, 60 / hz), `${dpr}× at ${hz}Hz`).toEqual({
        pxPerEvent,
        framesPerEvent,
      });
    }
  });

  it('on a 2.625× 120Hz phone the glide steps whole device pixels, ends on a steady one-a-frame floor, and lands exactly', () => {
    const dpr = 2.625;
    const refreshInterval = 1000 / 120;
    const h = makeHarness({ quantize: true, dpr });
    h.setTarget(100);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 240; i++) frame(refreshInterval);

    h.setTarget(160);
    h.spring.markTargetChanged();
    const steps: number[] = [];
    for (let i = 0; i < 480 && h.getScrollTop() < 160; i++) {
      const before = h.getScrollTop();
      frame(refreshInterval);
      steps.push((h.getScrollTop() - before) * dpr);
    }
    expect(h.getScrollTop()).toBe(160);
    // Every step but the exact landing is a whole number of device
    // pixels — the 1,2,1,2 the phone showed was a fractional model
    // snapped by the engine.
    const interior = steps.slice(0, -1);
    for (const step of interior) {
      expect(Math.abs(step - Math.round(step))).toBeLessThan(1e-6);
    }
    // Past the peak the rungs only step down (3,3,2,2,1 — once each).
    const peak = interior.indexOf(Math.max(...interior));
    for (let i = peak + 1; i < interior.length; i++) {
      expect(interior[i]).toBeLessThanOrEqual(interior[i - 1] + 1e-6);
    }
    // The tail is the rung — one device pixel every frame, no
    // stationary frame, no sub-floor cradle — all the way to the landing.
    const tail = interior.slice(-10);
    for (const step of tail) expect(Math.round(step)).toBe(1);
  });

  it('a 2× grid at 60Hz keeps the one-CSS-pixel floor (two device pixels a frame)', () => {
    const h = makeHarness({ quantize: true, dpr: 2 });
    h.setTarget(100);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 60; i++) frame();
    expect(h.getScrollTop()).toBe(100);

    h.setTarget(160);
    h.spring.markTargetChanged();
    const moves: number[] = [];
    for (let i = 0; i < 80 && h.getScrollTop() < 160; i++) {
      const before = h.getScrollTop();
      frame();
      moves.push(h.getScrollTop() - before);
    }
    expect(h.getScrollTop()).toBe(160);
    for (const move of moves) {
      expect(Math.abs(move * 2 - Math.round(move * 2))).toBeLessThan(1e-6);
    }
    const floorHold = moves.filter((m) => Math.abs(m - 1) < 1e-6);
    expect(floorHold.length).toBeGreaterThanOrEqual(2);
  });

  it('witnesses a CSS-pixel engine at DPR 2 on its first half-pixel write and moves the ladder to CSS pixels', () => {
    // Desktop Chromium rounds every scrollTop write to a whole CSS
    // pixel at DPR 2 (scrollTopQuantization.browser.test.ts). A ladder
    // that kept writing device pixels there would have the engine paint
    // its odd rungs (1.5 CSS px) as 2,1,2,1.
    const h = makeHarness({ cssGrid: true, dpr: 2 });
    h.setTarget(400);
    h.spring.markTargetChanged();
    h.spring.start();
    const requested: number[] = [];
    const steps: number[] = [];
    for (let i = 0; i < 200 && h.getScrollTop() < 400; i++) {
      const before = h.getScrollTop();
      const writesBefore = h.writes.length;
      frame();
      if (h.writes.length > writesBefore) requested.push(h.writes[h.writes.length - 1].value);
      steps.push(h.getScrollTop() - before);
    }
    expect(h.getScrollTop()).toBe(400);
    // Exactly one write ever lands off the CSS grid: the witness.
    const offGrid = requested.filter((value) => !Number.isInteger(value));
    expect(offGrid).toHaveLength(1);
    const witnessAt = requested.indexOf(offGrid[0]);
    for (const value of requested.slice(witnessAt + 1)) expect(Number.isInteger(value)).toBe(true);
    // And after it the painted steps are the ladder's own rungs: below
    // cruise (the diffusion band, where the envelope sheds several
    // pixels a frame), down by at most one CSS pixel a frame and
    // monotone, the 60px/s floor (one CSS px a frame) through to the
    // landing.
    const interior = steps.slice(0, -1);
    const peak = interior.indexOf(Math.max(...interior));
    for (let i = Math.max(peak + 1, witnessAt + 2); i < interior.length; i++) {
      if (interior[i - 1] >= 8) continue;
      expect(interior[i], `frame ${i}`).toBeLessThanOrEqual(interior[i - 1]);
      expect(interior[i - 1] - interior[i], `frame ${i}`).toBeLessThanOrEqual(1);
    }
    for (const step of interior.slice(-4)) expect(step).toBe(1);

    // The witness is an engine property: a second chase writes CSS
    // pixels from its first tick.
    h.setTarget(460);
    h.spring.markTargetChanged();
    const secondChase = h.writes.length;
    for (let i = 0; i < 100 && h.getScrollTop() < 460; i++) frame();
    expect(h.getScrollTop()).toBe(460);
    for (const write of h.writes.slice(secondChase)) expect(Number.isInteger(write.value)).toBe(true);
  });

  it('a 1.25× grid at 60Hz steps whole device pixels, never the 1,1,1,2 alternation', () => {
    const dpr = 1.25;
    const h = makeHarness({ quantize: true, dpr });
    h.setTarget(100);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 60; i++) frame();
    expect(h.getScrollTop()).toBe(100);

    h.setTarget(160);
    h.spring.markTargetChanged();
    const steps: number[] = [];
    for (let i = 0; i < 100 && h.getScrollTop() < 160; i++) {
      const before = h.getScrollTop();
      frame();
      steps.push((h.getScrollTop() - before) * dpr);
    }
    expect(h.getScrollTop()).toBe(160);
    const interior = steps.slice(0, -1);
    for (const step of interior) {
      expect(Math.abs(step - Math.round(step))).toBeLessThan(1e-6);
    }
    const peak = interior.indexOf(Math.max(...interior));
    for (let i = peak + 1; i < interior.length; i++) {
      expect(interior[i]).toBeLessThanOrEqual(interior[i - 1] + 1e-6);
    }
    // The 48px/s rung: one device pixel (0.8 CSS px) a frame.
    for (const step of interior.slice(-4)) expect(Math.round(step)).toBe(1);
  });

  it('submits only writes that move a whole device pixel', () => {
    const refreshInterval = 1000 / 165;
    const h = makeHarness({ quantize: true });
    h.setTarget(100);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 330; i++) frame(refreshInterval);

    const writesBefore = h.writes.length;
    h.setTarget(160);
    h.spring.markTargetChanged();
    let movingFrames = 0;
    for (let i = 0; i < 495 && h.getScrollTop() < 158; i++) {
      const before = h.getScrollTop();
      frame(refreshInterval);
      if (h.getScrollTop() !== before) movingFrames++;
    }

    expect(h.getScrollTop()).toBeGreaterThanOrEqual(158);
    const submittedWrites = h.writes.length - writesBefore;
    // A tick whose displacement rounds to no device pixel writes
    // nothing, so writes never outnumber moving frames beyond the exact
    // landing write.
    expect(submittedWrites).toBeLessThanOrEqual(movingFrames + 1);
  });

  /**
   * Drive a 60px quantum at `chaseHz` after two seconds of `warmHz`
   * frames (the cadence EMA re-converges during the chase), and assert
   * the last five pixels — the floor holds from about six pixels out,
   * where the spring's own velocity drops under the rung's rate — are
   * the rung: whole pixels, one every `framesPerEvent` frames exactly,
   * through to the landing. Returns the tail's period in ms per pixel.
   */
  function assertQuantizedTailCadence(warmHz: number, chaseHz: number): number {
    const warmInterval = 1000 / warmHz;
    const chaseInterval = 1000 / chaseHz;
    const h = makeHarness({ quantize: true });
    h.setTarget(100);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < warmHz * 2; i++) frame(warmInterval);

    h.setTarget(160);
    h.spring.markTargetChanged();
    const tailMoves: number[] = [];
    for (let i = 0; i < chaseHz * 3 && h.getScrollTop() < 160; i++) {
      const before = h.getScrollTop();
      frame(chaseInterval);
      if (before >= 155) tailMoves.push(h.getScrollTop() - before);
    }
    expect(h.getScrollTop()).toBe(160);
    const rung = quantizedFloorRung(1, 60 / chaseHz);
    expect(rung.pxPerEvent).toBe(1);
    const moveTicks: number[] = [];
    tailMoves.forEach((move, tick) => {
      expect([0, 1]).toContain(move);
      if (move === 1) moveTicks.push(tick);
    });
    expect(moveTicks).toHaveLength(5);
    for (let i = 1; i < moveTicks.length; i++) {
      expect(moveTicks[i] - moveTicks[i - 1]).toBe(rung.framesPerEvent);
    }
    return rung.framesPerEvent;
  }

  it('runs the tail at its rung from 60 through 240Hz: whole pixels, evenly spaced, near 60px/s', () => {
    for (const refreshHz of [60, 90, 120, 144, 165, 240]) {
      const framesPerPixel = assertQuantizedTailCadence(refreshHz, refreshHz);
      const pxPerSecond = refreshHz / framesPerPixel;
      expect(pxPerSecond, `${refreshHz}Hz`).toBeGreaterThanOrEqual(60);
      expect(pxPerSecond, `${refreshHz}Hz`).toBeLessThanOrEqual(90);
    }
  });

  it.each([
    [60, 240],
    [240, 60],
    [120, 165],
    [165, 120],
  ])('changes from %iHz to %iHz without a quantized-tail pulse', (warmHz, chaseHz) => {
    // The rung is re-derived from the cadence EMA every tick, so a chase
    // that starts on the old monitor's EMA settles onto the new rung
    // long before its tail — the same cadence as a chase warmed there.
    const transitionedCadence = assertQuantizedTailCadence(warmHz, chaseHz);
    const steadyCadence = assertQuantizedTailCadence(chaseHz, chaseHz);
    expect(transitionedCadence).toBe(steadyCadence);
  });
});

describe('chase telemetry', () => {
  it('a selection pause is counted and marked, and a chase that only paused still reports', () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const h = makeHarness();
    h.setTarget(200);
    h.spring.markTargetChanged();
    h.spring.start();
    h.setSelectionActive(true);

    for (let i = 0; i < 4; i++) frame();
    expect(h.writes, 'a paused spring writes nothing').toHaveLength(0);
    expect(h.spring.isActive(), 'and stays active').toBe(true);
    h.spring.cancel();

    const records = getUiRenderTraceRecords();
    const pauses = records.filter((r) => r.label === 'scroll.spring.selectionPause');
    expect(pauses, 'one entry mark per pause session').toHaveLength(1);
    const chases = records.filter((r) => r.label === 'scroll.spring.chase');
    expect(chases, 'zero integrated ticks, but the pause explains the silence').toHaveLength(1);
    const data = chases[0].data as { ticks: number; selectionPausedTicks: number };
    expect(data.ticks).toBe(0);
    expect(data.selectionPausedTicks).toBe(4);
  });

  it('a pause marks once, resumes silently, and marks again on the next session', () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const h = makeHarness();
    h.setTarget(200);
    h.spring.markTargetChanged();
    h.spring.start();

    h.setSelectionActive(true);
    frame();
    frame();
    h.setSelectionActive(false);
    frame();
    expect(h.writes.length, 'the release resumes the chase').toBeGreaterThan(0);
    h.setSelectionActive(true);
    frame();
    h.spring.cancel();

    const pauses = getUiRenderTraceRecords().filter((r) => r.label === 'scroll.spring.selectionPause');
    expect(pauses).toHaveLength(2);
  });

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

  it('counts stall ticks at 3 frames while integrating only 1 — the two thresholds are separate', () => {
    // `catchupClamps` is a STALL counter pinned at 3 frames so traces stay
    // comparable across the SPRING_MAX_CATCHUP_STEPS 3→1 change; the
    // integration cap is now stricter than it. Both are pinned here
    // because the two used to be one constant, and re-coupling them would
    // either silently redefine the metric or undo the 1-step cap.
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const h = makeHarness();
    h.setTarget(900);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 35; i++) frame(); // capped cruise

    // A 2-frame gap: past the integration cap (1), short of the stall
    // threshold (3). Physics clamps, telemetry stays silent.
    const beforeShort = h.getScrollTop();
    frame(33.34);
    expect(h.getScrollTop() - beforeShort).toBeLessThanOrEqual(27.5);

    // A 6-frame gap: past both.
    frame(100);
    h.spring.cancel();

    const records = getUiRenderTraceRecords().filter(
      (r) => r.label === 'scroll.spring.chase',
    );
    expect(records).toHaveLength(1);
    const data = records[0].data as { catchupClamps: number };
    // Exactly one — the 2-frame gap was clamped by the integrator but is
    // not a stall, so it must not appear in the counter.
    expect(data.catchupClamps).toBe(1);
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

describe('write-refusal guard', () => {
  // Models bug-report-20260818T003129Z: an element with real geometry
  // that refuses every scrollTop write (a non-scroll-container in the
  // engine) while the target keeps growing. The pre-guard behavior was
  // a display-rate busy-loop whose writes coasted to the FULL target,
  // so the first accepted write after healing teleported the viewport.

  function startWedgedChase(h: Harness, target: number): void {
    h.setTarget(target);
    h.spring.markTargetChanged();
    h.spring.start();
  }

  it('latches after consecutive refusals and drops to probe cadence', () => {
    const h = makeHarness({ refuse: true, clientHeight: 288 });
    startWedgedChase(h, 900);
    for (let i = 0; i < 60; i++) frame(); // ~1s at 60Hz

    const latched = h.refusalEvents.filter((e) => e.phase === 'latched');
    expect(latched).toHaveLength(1);
    expect(latched[0].consecutiveRefusals).toBe(SPRING_WRITE_REFUSAL_LATCH_TICKS);
    expect(latched[0].wedgeMs).toBe(0);
    expect(h.refusalEvents.filter((e) => e.phase === 'healed')).toHaveLength(0);
    // Un-guarded, this second produced 60 writes. The guard allows the
    // pre-latch ramp plus ~4 probes (one per 250ms).
    expect(h.writes.length).toBeLessThan(20);
    expect(h.writes.length).toBeGreaterThan(5);
  });

  it('re-anchors the model: no write ever coasts toward the far target', () => {
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 900);
    for (let i = 0; i < 120; i++) frame(); // 2s wedged

    // The pre-guard failure mode wrote `target` (900) every tick once
    // the simulated position crossed it. Re-anchoring bounds every
    // write to one velocity-capped step from the element's TRUE
    // position (0): max 27px/frame + fractional-step epsilon.
    for (const w of h.writes) {
      expect(w.value).toBeLessThanOrEqual(28);
    }
  });

  it('heals as a bounded glide, never a teleport, and reports the bookend', () => {
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 900);
    for (let i = 0; i < 40; i++) frame(); // latched + a few probes
    expect(h.getScrollTop()).toBe(0);

    h.setRefuseWrites(false);
    const moves = displacements(h, 150);
    // Every frame of the heal glide stays inside the velocity cap —
    // the no-teleport invariant this guard exists for.
    for (const move of moves) {
      expect(move).toBeLessThanOrEqual(27.5);
    }
    expect(h.getScrollTop()).toBeGreaterThan(850);
    const healed = h.refusalEvents.filter((e) => e.phase === 'healed');
    expect(healed).toHaveLength(1);
    expect(healed[0].consecutiveRefusals).toBeGreaterThan(0);
    expect(healed[0].wedgeMs).toBeGreaterThan(0);
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);
  });

  it('probe cadence honors the interval while wedged', () => {
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 900);
    for (let i = 0; i < 30; i++) frame(); // reach the latch
    const writesAtLatch = h.writes.length;
    // 60 more frames = ~1000ms → at most ⌈1000 / interval⌉ + 1 probes.
    for (let i = 0; i < 60; i++) frame();
    const probes = h.writes.length - writesAtLatch;
    const maxProbes = Math.ceil(1000 / SPRING_WRITE_REFUSAL_PROBE_INTERVAL_MS) + 1;
    expect(probes).toBeLessThanOrEqual(maxProbes);
    expect(probes).toBeGreaterThan(0);
  });

  it('latch survives a caught-up excursion and heals instantly on the next chase', () => {
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 400);
    for (let i = 0; i < 30; i++) frame(); // latched
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);

    // Content collapses onto the wedged position: the caught-up branch
    // runs (sentinel keeps the spring alive; liveContentActive true).
    h.setTarget(0);
    for (let i = 0; i < 25; i++) frame(); // > probe interval of idle

    // Element heals while idle; content grows again.
    h.setRefuseWrites(false);
    h.setTarget(300);
    h.spring.markTargetChanged();
    // The gate consumes probe slots even during the caught-up excursion,
    // so the heal probe lands within one probe interval of the new
    // chase — 20 frames (~333ms) guarantees one.
    for (let i = 0; i < 20; i++) frame();

    // The probe landed and the chase resumed — no re-latch, no
    // duplicate 'latched' report, no waiting out refusal counting.
    expect(h.getScrollTop()).toBeGreaterThan(0);
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);
    expect(h.refusalEvents.filter((e) => e.phase === 'healed')).toHaveLength(1);
  });

  it('cancel while latched reports abandoned and resets the guard for a fresh chase', () => {
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 900);
    for (let i = 0; i < 30; i++) frame();
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);
    expect(h.spring.refusalLatched()).toBe(true);

    h.spring.cancel();
    // The chase ended while still latched: no heal was observed, so the
    // episode closes with 'abandoned', never a false 'healed'.
    const abandoned = h.refusalEvents.filter((e) => e.phase === 'abandoned');
    expect(abandoned).toHaveLength(1);
    expect(abandoned[0].consecutiveRefusals).toBeGreaterThan(0);
    expect(abandoned[0].wedgeMs).toBeGreaterThan(0);
    expect(abandoned[0].requested).toBe(-1);
    expect(h.spring.refusalLatched()).toBe(false);

    startWedgedChase(h, 900);
    for (let i = 0; i < 30; i++) frame();
    // Fresh chase, fresh evidence: a second latch with no healed event
    // in between.
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(2);
    expect(h.refusalEvents.filter((e) => e.phase === 'healed')).toHaveLength(0);
  });

  it('a healthy quantizing engine never latches', () => {
    const h = makeHarness({ quantize: true });
    h.setTarget(900);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 90; i++) frame();
    expect(h.refusalEvents).toHaveLength(0);
    expect(h.getScrollTop()).toBeGreaterThan(850);
  });

  it('counts refused writes in the chase telemetry summary', () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 900);
    for (let i = 0; i < 30; i++) frame();
    h.spring.cancel();

    const records = getUiRenderTraceRecords().filter(
      (r) => r.label === 'scroll.spring.chase',
    );
    expect(records).toHaveLength(1);
    const data = records[0].data as { refusedWrites: number };
    expect(data.refusedWrites).toBeGreaterThanOrEqual(SPRING_WRITE_REFUSAL_LATCH_TICKS);
  });

  it('sub-threshold no-motion writes never falsely heal a still-wedged latch', () => {
    // The false-heal defect the three-way classification exists for:
    // healing keyed on "not refused" let a still-wedged element whose
    // remaining distance drifted under the motion threshold unlatch —
    // its writes moved nothing but requested too little to classify as
    // refused. Healing must require observed MOTION.
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 900);
    for (let i = 0; i < 30; i++) frame(); // latched
    expect(h.spring.refusalLatched()).toBe(true);

    // Target collapses to a sliver above the wedged position: every
    // probe write from here requests < 1.5px of motion (inconclusive).
    h.setTarget(h.getScrollTop() + 1.2);
    h.spring.markTargetChanged();
    for (let i = 0; i < 120; i++) frame(); // 2s of sliver probes

    expect(h.refusalEvents.filter((e) => e.phase === 'healed')).toHaveLength(0);
    expect(h.spring.refusalLatched()).toBe(true);
    // And the backoff held: probe cadence, not display rate.
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);
  });

  it('an engine max-scroll clamp near the target never reads as refusal', () => {
    // Engine clamps writes at target−1 (max-scrollTop race): the final
    // write moves partially (MOVED) and the settled element then only
    // receives sub-threshold requests (inconclusive). Neither leg may
    // count toward a latch.
    const h = makeHarness({ clampMax: 899 });
    h.setTarget(900);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 180; i++) frame(); // 3s — plenty to settle at 899
    expect(h.refusalEvents).toHaveLength(0);
    expect(h.getScrollTop()).toBe(899);
  });

  it('a wedge whose remaining distance sits under the threshold ticks without latching', () => {
    // The documented un-latched corner: target inside (arrival band,
    // threshold) of the wedged position. Writes are too small to
    // teleport — the harm the guard bounds — so no events fire.
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 1.2);
    for (let i = 0; i < 120; i++) frame();
    expect(h.refusalEvents).toHaveLength(0);
  });

  it('a refused catch-up jump does not seed cruise velocity into the heal', () => {
    const h = makeHarness({ refuse: true, clientHeight: 600 });
    setDocumentResumeAtForTest(now);
    startWedgedChase(h, 5000);
    frame(); // catch-up jump fires against the wedge and is refused
    for (let i = 0; i < 30; i++) frame(); // latch + park
    expect(h.spring.refusalLatched()).toBe(true);
    // Idle past the resume-clamp window so the heal itself cannot take
    // the (legitimate, deliberate) catch-up cut — this test pins the
    // refused jump's non-effect, not the cut policy.
    for (let i = 0; i < 120; i++) frame();

    h.setRefuseWrites(false);
    // First accepted movement after the heal must ramp from the slew
    // base — a refused jump that seeded cap velocity (27px/frame) off a
    // position the element never took would launch the heal at cruise.
    const moves = displacements(h, 30).filter((m) => m !== 0);
    expect(moves.length).toBeGreaterThan(0);
    expect(moves[0]).toBeLessThan(4);
  });

  it('latches on refusal count, not wall-clock, at 165Hz', () => {
    const h = makeHarness({ refuse: true });
    startWedgedChase(h, 900);
    // Each 6.06ms tick integrates ~0.36 of a 60Hz frame, so several
    // sub-threshold (inconclusive) ticks separate consecutive refusals
    // — which must NOT reset the count. ~50 ticks (~300ms) comfortably
    // clears five refusals.
    for (let i = 0; i < 50; i++) frame(6.06);
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);
    // Probe cadence is on the ms clock: a further second of 165Hz
    // frames buys the same ≤5 probes a 60Hz second would.
    const writesAtLatch = h.writes.length;
    for (let i = 0; i < 165; i++) frame(6.06);
    const probes = h.writes.length - writesAtLatch;
    expect(probes).toBeLessThanOrEqual(
      Math.ceil(1000 / SPRING_WRITE_REFUSAL_PROBE_INTERVAL_MS) + 1,
    );
  });

  it('a long wedge stays latched once and bounded: 600 frames, one latch, probe-rate writes', () => {
    const h = makeHarness({ refuse: true, clientHeight: 288 });
    startWedgedChase(h, 900);
    for (let i = 0; i < 600; i++) frame(); // 10s wedged
    expect(h.refusalEvents.filter((e) => e.phase === 'latched')).toHaveLength(1);
    expect(h.refusalEvents.filter((e) => e.phase === 'healed')).toHaveLength(0);
    // Pre-latch ramp (5) + ~40 probes over 10s — nowhere near the 600
    // writes the incident's busy-loop produced.
    expect(h.writes.length).toBeLessThan(60);
  });
});
