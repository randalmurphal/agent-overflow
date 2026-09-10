import { describe, expect, it } from 'vitest';
import { frame, makeHarness, velocities, displacements } from './springTestHarness';

describe('onset, acceleration continuity, and parked decay', () => {
  it('ramps a standstill onset geometrically from the motion floor instead of jumping to the envelope peak', () => {
    const h = makeHarness();
    h.setTarget(60);
    h.spring.markTargetChanged();
    h.spring.start();

    const speeds = velocities(h, 6);
    // The onset compounds from the base speed at roughly 10% per frame.
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
    // The peak occurs mid-glide. Exclude the final exact arrival write
    // when checking the monotone acceleration and braking legs.
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
        // Another flush preserves the acceleration already in progress.
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

  it('rounds a large retarget without nearly stopping a cap-speed glide', () => {
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
    expect(acceleration).toBeLessThan(-0.4);

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
    expect(Math.max(...handoffSpeeds)).toBeCloseTo(27);
    for (let i = 1; i < handoffAccelerations.length; i++) {
      expect(Math.abs(handoffAccelerations[i] - handoffAccelerations[i - 1])).toBeLessThan(0.5);
    }
  });

  it.each([60, 120, 144, 165, 240])(
    'bounds acceleration changes in both directions at %iHz during repeated line growth',
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
      for (let i = warmStart + 1; i < accelerations.length; i++) {
        const jerk = (accelerations[i] - accelerations[i - 1]) / stepFraction;
        expect(Math.abs(jerk), `tick ${i}`).toBeLessThanOrEqual(0.105);
      }
      expect(speeds.slice(warmStart).every((value) => value > 0)).toBe(true);
      expect(steps.slice(warmStart).every((value) => value > 0)).toBe(true);
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
    // but acceleration shaping must not introduce a visible pause or reversal at
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
    // The same wall time must advance both the onset ramp and cruise.
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

  it('keeps a small glide comparable across fractional steps', () => {
    const run = (frameMs: number): number => {
      const h = makeHarness();
      h.setTarget(6);
      h.spring.markTargetChanged();
      h.spring.start();
      // Both drives begin with the same initial full step.
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
    expect(Math.abs(at60 - at120)).toBeLessThan(at60 * 0.04);
  });

  it('re-ramps a reversal from the base: the flipped direction never opens with a hard onset', () => {
    const h = makeHarness();
    // Reverse a moving glide mid-chase.
    h.setTarget(300);
    h.spring.markTargetChanged();
    h.spring.start();
    for (let i = 0; i < 20; i++) frame();
    expect(h.getScrollTop()).toBeGreaterThan(50);
    h.setTarget(0);
    h.spring.markTargetChanged();

    // Direction changes shed the old velocity before a bounded onset.
    const speeds = velocities(h, 6);
    const firstDown = speeds.findIndex((v) => v < 0);
    expect(firstDown).toBeGreaterThanOrEqual(0);
    expect(firstDown).toBeLessThanOrEqual(2);
    expect(speeds[firstDown]).toBeGreaterThanOrEqual(-1.2);
    let acceleration = 0;
    for (let i = firstDown + 1; i < speeds.length; i++) {
      const nextAcceleration = speeds[i] - speeds[i - 1];
      expect(nextAcceleration).toBeLessThan(0);
      expect(Math.abs(nextAcceleration - acceleration)).toBeLessThanOrEqual(0.101);
      expect(-nextAcceleration).toBeLessThanOrEqual(Math.max(1, -speeds[i - 1]) * 0.101);
      acceleration = nextAcceleration;
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
