// Unit tests for the spring chase kinematics (createSpringChase) that
// need frame-precise control over rAF timing and target geometry:
// the hard velocity cap, the deceleration envelope, the acceleration
// slew (onset ramp, parked carry decay, seed exemption, integrator
// composability), the integer-rounding remainder carry (glide-residue
// continuity), the clamp-not-zero momentum carry, and the per-chase
// telemetry summary. Controller-level
// choreography (when a chase runs) lives in index.svelte.test.ts; these
// tests pin HOW a chase advances, driven through a fake deps harness.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createSpringChase,
  fusionFloorPxPerFrame,
  type ArrivalReadback,
  type SpringChaseDeps,
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
  getScrollTop(): number;
  setTarget(value: number): void;
  getTarget(): number;
  setLiveContentActive(active: boolean): void;
  residueSettles(): number;
}

/**
 * `quantize: true` models the browser's scrollTop contract at 1× DPI:
 * a fractional write lands on a whole pixel, and readbacks return that
 * rounded value. The default (fractional storage) keeps the kinematic
 * assertions exact; the quantized variant exercises the
 * remainder-carry path (which is inert when writes store exactly).
 *
 * `clientHeight` (default 0 = unmeasured) arms the chase-distance clamp;
 * the kinematic tests leave it off so their long-glide assertions stay
 * exact.
 */
function makeHarness(
  opts: { quantize?: boolean; clientHeight?: number; dpr?: number } = {},
): Harness {
  let scrollTop = 0;
  let target = 0;
  let liveContentActive = true;
  let residueSettles = 0;
  // Faithful mini provenance ledger: writes explain their readback, so
  // `scrollTopUnexplained` only reports true if a test mutates scrollTop
  // outside the write path (a simulated browser clamp) — mirroring the
  // controller's ledger contract.
  let lastExplainedScrollTop: number | null = null;
  const writes: { caller: string; value: number }[] = [];
  const store = (value: number): void => {
    scrollTop = opts.quantize ? Math.round(value) : value;
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
    selectionActive: () => false,
    targetScrollTop: () => target,
    scrollTopIsAtTarget: (t) => Math.abs(scrollTop - t) <= ARRIVAL_DISTANCE_PX,
    arrival,
    writeScrollTop: (caller, value) => {
      writes.push({ caller, value });
      store(value);
    },
    liveContentActive: () => liveContentActive,
    prefersReducedMotion: () => false,
    forceNextSpringTickTrace: () => {},
    settleGlideResidue: () => {
      residueSettles += 1;
    },
    // Display input to the refresh-aware fusion floor; the derivation
    // itself is unit-tested directly (fusionFloorPxPerFrame below). At
    // dpr 1 and the harness's default 16.67ms cadence the floor is the
    // 60Hz phase lock: 1.0 px per frame.
    devicePixelRatio: () => opts.dpr ?? 1,
    scrollTopUnexplained: () =>
      lastExplainedScrollTop !== null
      && Math.abs(scrollTop - lastExplainedScrollTop) > ARRIVAL_DISTANCE_PX,
  };

  return {
    spring: createSpringChase(deps),
    writes,
    getScrollTop: () => scrollTop,
    setTarget: (value) => {
      target = value;
    },
    getTarget: () => target,
    setLiveContentActive: (active: boolean) => {
      liveContentActive = active;
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

  it('bounds a stalled frame to the catch-up burst instead of paying the whole gap', () => {
    const h = makeHarness();
    h.setTarget(900);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 35; i++) frame(); // spool past the slew ramp to capped cruise

    const before = h.getScrollTop();
    frame(100); // stall: dtFrames = 6, clamped to SPRING_MAX_CATCHUP_STEPS (3)
    const move = h.getScrollTop() - before;
    // Three capped steps: ≤ 3 × 27px (+ integration epsilon), far short of
    // the remaining ~600px gap — a blocked frame never becomes a teleport.
    expect(move).toBeLessThanOrEqual(81.5);
    expect(move).toBeGreaterThan(54); // multiple steps actually ran
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

describe('chase-distance clamp', () => {
  function catchupJumps(h: Harness): { caller: string; value: number }[] {
    return h.writes.filter((write) => write.caller === 'spring.catchupJump');
  }

  it('a live >viewport structural mount NEVER clamps — full bounded glide (hard requirement)', () => {
    const h = makeHarness({ clientHeight: 600 });
    // A huge diff card mounts in one frame mid-follow: rAF is ticking
    // normally and the document never went hidden. Distance alone must
    // not be read as a stall.
    h.setTarget(5000);
    h.spring.markTargetChanged();
    h.spring.start();

    const moves = displacements(h, 200);
    expect(catchupJumps(h)).toHaveLength(0);
    for (const move of moves) {
      expect(move).toBeLessThanOrEqual(27.5);
    }
    expect(h.getScrollTop()).toBeGreaterThan(4700);
  });

  it('clamps to one viewport of glide when the tick carries a real rAF stall gap', () => {
    const h = makeHarness({ clientHeight: 600 });
    h.setTarget(300);
    h.spring.markTargetChanged();
    h.spring.start();
    frame();
    frame(); // chase established; lastTickAt is live

    // Occlusion: rAF frozen for 5s while content grew far past the
    // viewport. The first resumed tick carries the whole gap.
    h.setTarget(6000);
    h.spring.markTargetChanged();
    frame(5000);

    const jumps = catchupJumps(h);
    expect(jumps).toHaveLength(1);
    expect(jumps[0].value).toBe(5400); // target − one viewport
    // The cut is instant; everything after it is bounded glide.
    const moves = displacements(h, 40);
    // The seeded cap-speed entry is exempt from the acceleration slew
    // (the seed becomes the ramp's base): the glide continues AT cruise
    // rather than re-ramping from the floor.
    expect(moves[0]).toBeGreaterThan(26);
    for (const move of moves) {
      expect(move).toBeLessThanOrEqual(27.5);
    }
    expect(h.getScrollTop()).toBeGreaterThan(5900);
  });

  it('clamps a fresh chase started shortly after the document resumed visibility', () => {
    const h = makeHarness({ clientHeight: 600 });
    // visibilitychange → visible fired just now (text smoothers snapped
    // to the wire, creating the backlog); the chase starts fresh with no
    // prior tick to carry the rAF gap.
    setDocumentResumeAtForTest(now);
    h.setTarget(5000);
    h.spring.markTargetChanged();
    h.spring.start();

    frame();
    expect(catchupJumps(h)[0]?.value).toBe(4400);
  });

  it('stops clamping a fresh chase once the resume window has passed', () => {
    // The resume clamp treats every fresh >viewport chase within
    // RESUME_CLAMP_WINDOW_MS (2000ms) of a visibilitychange→visible as
    // tab-return backlog and cuts it to one viewport — a deliberate
    // tradeoff that also cuts a legitimate large structural mount (big
    // diff card) landing in that window. This pins the window's EDGE:
    // past 2000ms the same chase is a real mount again and must glide
    // from its true start with no cut.
    const h = makeHarness({ clientHeight: 600 });
    setDocumentResumeAtForTest(now);
    now += 2001; // idle past the window, no spring running
    h.setTarget(5000);
    h.spring.markTargetChanged();
    h.spring.start();
    frame();
    expect(catchupJumps(h)).toHaveLength(0);
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
    expect(catchupJumps(h)).toHaveLength(0);
    expect(h.getScrollTop()).toBeGreaterThan(450);
  });

  it('clamps a shrink-follow backlog symmetrically', () => {
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
    expect(catchupJumps(h)[0]?.value).toBe(700); // target + one viewport
    const moves = displacements(h, 40);
    for (const move of moves) {
      expect(move).toBeGreaterThanOrEqual(-27.5);
    }
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
    expect(catchupJumps(h)).toHaveLength(0);
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
    const before = h.getScrollTop();
    frame();
    const firstFrameMove = h.getScrollTop() - before;
    expect(firstFrameMove).toBeGreaterThan(1.7);
    expect(firstFrameMove).toBeLessThan(1.9);
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
    const before = h.getScrollTop();
    frame();
    const firstFrameMove = h.getScrollTop() - before;
    expect(firstFrameMove).toBeGreaterThan(1.05);
    expect(firstFrameMove).toBeLessThan(1.2);
  });
});

describe('acceleration slew (onset ramp + parked decay)', () => {
  it('ramps a standstill onset geometrically from the fusion floor instead of jumping to the envelope peak', () => {
    const h = makeHarness();
    h.setTarget(60);
    h.spring.markTargetChanged();
    h.spring.start();

    const moves = displacements(h, 6);
    // Pre-slew, the first frame jumped straight to min(raw spring
    // ≈3.8, envelope 6.6). Slewed: the fusion floor (the 60Hz phase
    // lock, 1.0) × the ramp factor, compounding ~10% per frame.
    expect(moves[0]).toBeGreaterThan(1.0);
    expect(moves[0]).toBeLessThan(1.2);
    for (let i = 1; i < moves.length; i++) {
      const ratio = moves[i] / moves[i - 1];
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
    const before = h.getScrollTop();
    frame();
    const entry = h.getScrollTop() - before;
    expect(entry).toBeGreaterThan(1.0);
    expect(entry).toBeLessThan(1.3);
  });

  it('advances the ramp by wall time, not tick count, on high-refresh displays', () => {
    // 60Hz vs 120Hz drives of the same chase land at ~the same position
    // after the same wall time: the ramp compounds G^stepFraction. In
    // this ceiling-dominated regime the tolerance pins the RAMP's time
    // scaling only — the integrator's own composability is pinned
    // separately by the ceiling-free test below. 120Hz is
    // the comparison point because its fusion floor rung equals 60Hz's
    // (both 1.0 px/frame), isolating time-scaling from the
    // refresh-aware floor ladder.
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
    // (1.10), the envelope min (1.6), and the fusion floor — pure
    // spring physics, so this directly pins the integrator's
    // fractional-step composability: retention must be
    // (damping/mass)^f, not (damping^f)/mass. The historical form's
    // effective 120Hz retention was 0.448/frame vs the tuned 0.56 —
    // ~20% velocity bleed per extra step — which diverges far outside
    // this tolerance.
    const run = (frameMs: number): number => {
      const h = makeHarness();
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
    const moves = displacements(h, 6);
    const firstDown = moves.findIndex((m) => m < 0);
    expect(firstDown).toBeGreaterThanOrEqual(0);
    expect(firstDown).toBeLessThanOrEqual(2);
    expect(moves[firstDown]).toBeGreaterThanOrEqual(-1.2);
    for (let i = firstDown + 1; i < moves.length; i++) {
      const ratio = moves[i] / moves[i - 1];
      expect(ratio).toBeGreaterThan(1.05);
      expect(ratio).toBeLessThan(1.2);
    }
  });

  it('keeps the standstill attack display-independent on high-DPR panels (perceptual ramp base)', () => {
    // At dpr 3 the fusion floor clamps to 0.4 — display physics says
    // sub-pixel breathing is negligible there, but a 0.4-based ramp
    // would stretch the same quantum's attack to ~2× its 1× duration.
    // The ramp bases at max(1.0, floor), so the onset matches dpr 1.
    const h = makeHarness({ dpr: 3 });
    h.setTarget(60);
    h.spring.markTargetChanged();
    h.spring.start();
    const moves = displacements(h, 3);
    expect(moves[0]).toBeGreaterThan(1.0);
    expect(moves[0]).toBeLessThan(1.2);
    expect(moves[1] / moves[0]).toBeGreaterThan(1.08);
    expect(moves[1] / moves[0]).toBeLessThan(1.16);
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
    const before = h.getScrollTop();
    frame();
    const entry = h.getScrollTop() - before;
    expect(entry).toBeGreaterThan(1.0);
    expect(entry).toBeLessThan(1.3);
  });
});

describe('glide shaping (decel envelope + fractional tail)', () => {
  // The peak sits at the slew-ramp ↔ envelope crossover; the envelope
  // (0.09 · remaining) shapes the
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

  it('bounds the peak with the envelope and holds the fusion floor through the tail', () => {
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
    // then plateaus at the fusion floor (equal frames allowed). The
    // final frame is excluded: it combines the last decay step with
    // the sentinel-entry exact snap (≤1px arrival band, invisible), so
    // it reads larger than the step before it.
    const peakIndex = moves.indexOf(Math.max(...moves));
    for (let i = peakIndex + 1; i < moves.length - 1; i++) {
      expect(moves[i]).toBeLessThanOrEqual(moves[i - 1] + 0.01);
    }
    expect(moves[moves.length - 1]).toBeLessThanOrEqual(1.55);
    // Fusion-floor hold: the deceleration parks at the floor (the 60Hz
    // phase lock — 1.0 device px per frame at this harness's dpr 1 and
    // 16.67ms cadence) instead of decaying through it.
    const floorHold = moves.filter((m) => m > 0.98 && m < 1.02);
    expect(floorHold.length).toBeGreaterThanOrEqual(2);
    // THE regression assertion: a decelerating glide never DWELLS in
    // the visible-breathing speed band (sub-floor but still moving) —
    // bilinear resampling of the residue renders thin rows as sharp/dim
    // flicker at those speeds (2026-07-04T2026 capture: 49% of glide
    // time spent there). The floor releases inside the last 3px, so the
    // landing ritardando sweeps the band in a bounded handful of frames
    // (~one dim/bright cycle) rather than crawling through it.
    const breathingBand = moves.filter((m) => m > 0.05 && m < 0.95);
    expect(breathingBand.length).toBeLessThanOrEqual(4);
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

describe('fusionFloorPxPerFrame (refresh-aware derivation)', () => {
  // The quantity the rule targets: floor advance in DEVICE px per
  // DISPLAYED frame — the lock ratio r = 1/k from the constant block.
  // (floor is px per 60Hz-equivalent frame; × dtFrames × dpr converts.)
  function devicePxPerDisplayedFrame(dpr: number, intervalMs: number): number {
    return fusionFloorPxPerFrame(dpr, intervalMs) * (intervalMs / (1000 / 60)) * dpr;
  }

  it('phase-locks sub-120Hz displays at one device pixel per frame (zero breathing)', () => {
    expect(devicePxPerDisplayedFrame(1, 1000 / 60)).toBeCloseTo(1, 5);
    expect(devicePxPerDisplayedFrame(1.1, 1000 / 60)).toBeCloseTo(1, 5);
    expect(devicePxPerDisplayedFrame(1, 1000 / 90)).toBeCloseTo(1, 5);
  });

  it('half-locks 120–179Hz so the alternation patterns at or above fusion', () => {
    // 144Hz: alternation at 72Hz — the refresh-blind floor's ~12Hz
    // second-harmonic beat on this refresh is the bug this fixes.
    expect(devicePxPerDisplayedFrame(1.1, 1000 / 144)).toBeCloseTo(0.5, 5);
    expect(devicePxPerDisplayedFrame(1.1, 1000 / 165)).toBeCloseTo(0.5, 5);
    expect(devicePxPerDisplayedFrame(2, 1000 / 120)).toBeCloseTo(0.5, 5);
  });

  it('keeps descending the ladder at very high refresh', () => {
    expect(devicePxPerDisplayedFrame(1, 1000 / 240)).toBeCloseTo(0.25, 5);
  });

  it('falls back to the 60Hz assumption until cadence is measured', () => {
    expect(fusionFloorPxPerFrame(1.1, null)).toBeCloseTo(
      fusionFloorPxPerFrame(1.1, 1000 / 60),
      5,
    );
  });

  it('clamps degenerate inputs', () => {
    // 3× retina: a one-pixel lock is a meaningless 20px/s hold — min binds.
    expect(fusionFloorPxPerFrame(3, 1000 / 60)).toBeCloseTo(0.4, 5);
    // dpr < 1 (zoomed-out webview) at 100Hz would demand ~3.3px/frame —
    // max binds at the envelope's lower cap.
    expect(fusionFloorPxPerFrame(0.5, 1000 / 100)).toBeCloseTo(1.6, 5);
    // Unmeasurable dpr guards to 1.
    expect(fusionFloorPxPerFrame(0, 1000 / 60)).toBeCloseTo(1, 5);
  });

  it('adapts a live chase to measured cadence: a 165Hz chase plateaus at the half lock', () => {
    const h = makeHarness({ dpr: 1.1 });
    // Park at 165Hz cadence so the frame-interval EMA converges there.
    h.setTarget(100);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 300; i++) frame(1000 / 165);
    expect(Math.abs(h.getScrollTop() - 100)).toBeLessThanOrEqual(ARRIVAL_DISTANCE_PX);

    h.setTarget(160);
    h.spring.markTargetChanged();
    const moves: number[] = [];
    for (let i = 0; i < 400 && Math.abs(h.getScrollTop() - 160) > 1; i++) {
      const before = h.getScrollTop();
      frame(1000 / 165);
      moves.push(h.getScrollTop() - before);
    }
    expect(Math.abs(h.getScrollTop() - 160)).toBeLessThanOrEqual(1);
    // Floor = 0.5 device px per displayed frame = 0.5/1.1 ≈ 0.4545 CSS
    // px — NOT the 60Hz-derived value the old refresh-blind floor
    // would hold (1.0/1.1 ≈ 0.909/frame60 → 0.33/frame at 165Hz).
    const floorHold = moves.filter((m) => m > 0.448 && m < 0.462);
    expect(floorHold.length).toBeGreaterThanOrEqual(4);
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
    // Run past arrival + retain lapse; live content stays active, so
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
