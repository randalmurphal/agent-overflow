import { describe, expect, it } from 'vitest';
import { frame, makeHarness, displacements, now, advanceClock, rafQueue, type Harness } from './springTestHarness';
import { setDocumentResumeAtForTest } from './documentResume';

describe('spring frame ownership', () => {
  it('retires a detached element and allows the next attachment to start a fresh chase', () => {
    const h = makeHarness();
    h.setTarget(300);
    h.setAttached(false);
    h.spring.start();
    frame();
    expect(h.spring.isActive()).toBe(false);
    expect(h.writes).toHaveLength(0);

    h.setAttached(true);
    h.spring.start();
    frame();
    expect(h.spring.isActive()).toBe(true);
    expect(h.getScrollTop()).toBeGreaterThan(0);
    h.setAttached(false);
    frame();
    expect(h.spring.isActive()).toBe(false);
    expect(h.velocity()).toBe(0);
    h.setAttached(true);
    h.spring.start();
    frame();
    expect(h.spring.isActive()).toBe(true);
  });

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
    // Stalls are routine on WebKit (2026-08-05 measurement: 6–7 per chase), and
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
    advanceClock(2001); // idle past the window, no spring running
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
