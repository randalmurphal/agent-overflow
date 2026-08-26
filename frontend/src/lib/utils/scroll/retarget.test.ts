import { describe, expect, it } from 'vitest';
import {
  createRetargetAccelerationBridge,
  type RetargetAccelerationBridge,
} from './retarget';

interface StepInput {
  target: number;
  difference: number;
  velocity: number;
  candidateVelocity: number;
  stepFraction: number;
  maxVelocity: number;
}

function apply(
  bridge: RetargetAccelerationBridge,
  overrides: Partial<StepInput> = {},
): number {
  const input: StepInput = {
    target: 100,
    difference: 100,
    velocity: 5,
    candidateVelocity: 4,
    stepFraction: 1,
    maxVelocity: 27,
    ...overrides,
  };
  return bridge.step(
    input.target,
    input.difference,
    input.velocity,
    input.candidateVelocity,
    input.stepFraction,
    input.maxVelocity,
  );
}

describe('retarget acceleration bridge', () => {
  it('leaves a fixed-target curve unchanged', () => {
    const bridge = createRetargetAccelerationBridge();
    expect(apply(bridge)).toBe(4);
    expect(bridge.active()).toBe(false);
    expect(apply(bridge, { velocity: 4, candidateVelocity: 3.5 })).toBe(3.5);
  });

  it('rounds braking through zero when the target extends in the same direction', () => {
    const bridge = createRetargetAccelerationBridge();
    apply(bridge); // acceleration -1, moving toward target

    let velocity = apply(bridge, {
      target: 200,
      difference: 196,
      velocity: 4,
      candidateVelocity: 5,
    });
    expect(velocity).toBeCloseTo(3.125); // acceleration -0.875, not +1
    expect(bridge.active()).toBe(true);

    const accelerations: number[] = [-0.875];
    for (let i = 0; i < 16 && bridge.active(); i++) {
      const before = velocity;
      velocity = apply(bridge, {
        target: i === 1 ? 300 : 200,
        difference: 300,
        velocity,
        candidateVelocity: velocity + 1,
      });
      accelerations.push(velocity - before);
    }
    expect(accelerations.some((value) => value < 0)).toBe(true);
    expect(accelerations.some((value) => value > 0)).toBe(true);
    for (let i = 1; i < accelerations.length; i++) {
      expect(accelerations[i] - accelerations[i - 1]).toBeLessThanOrEqual(0.126);
    }
  });

  it('scales jerk from large endpoint accelerations', () => {
    const bridge = createRetargetAccelerationBridge();
    apply(bridge, { velocity: 20, candidateVelocity: 18 }); // acceleration -2
    const velocity = apply(bridge, {
      target: 1200,
      difference: 1000,
      velocity: 18,
      candidateVelocity: 20,
    });
    // Endpoint scale chooses 0.25 jerk instead of the 0.1 floor.
    expect(velocity).toBeCloseTo(16.25);
  });

  it('applies the same bridge to a downward same-direction retarget', () => {
    const bridge = createRetargetAccelerationBridge();
    apply(bridge, {
      target: 0,
      difference: -100,
      velocity: -5,
      candidateVelocity: -4,
    });
    const velocity = apply(bridge, {
      target: -100,
      difference: -196,
      velocity: -4,
      candidateVelocity: -5,
    });
    expect(velocity).toBeCloseTo(-3.125);
    expect(bridge.active()).toBe(true);
  });

  it('returns immediately to stronger braking so the bridge cannot cause overshoot', () => {
    const bridge = createRetargetAccelerationBridge();
    apply(bridge);
    const bridged = apply(bridge, {
      target: 200,
      difference: 196,
      velocity: 4,
      candidateVelocity: 5,
    });
    expect(bridge.active()).toBe(true);

    const braked = apply(bridge, {
      target: 200,
      difference: 10,
      velocity: bridged,
      candidateVelocity: bridged - 2,
    });
    expect(braked).toBeCloseTo(bridged - 2);
    expect(bridge.active()).toBe(false);
  });

  it('scales the jerk bound by elapsed wall time', () => {
    const bridge = createRetargetAccelerationBridge();
    apply(bridge, { velocity: 5, candidateVelocity: 4.6 }); // acceleration -0.4
    const velocity = apply(bridge, {
      target: 200,
      difference: 195.4,
      velocity: 4.6,
      candidateVelocity: 4.8,
      stepFraction: 0.5,
    });
    // The 0.1-per-60Hz-frame floor advances acceleration by 0.05 over
    // this half-frame. It moves from -0.4 to -0.35, then integrates for
    // half a frame.
    expect(velocity).toBeCloseTo(4.425);
  });

  it('clears motion history on catch-up and all history on reset', () => {
    const bridge = createRetargetAccelerationBridge();
    apply(bridge);
    bridge.breakMotion();
    expect(apply(bridge, {
      target: 200,
      difference: 200,
      velocity: 0,
      candidateVelocity: 1.1,
    })).toBeCloseTo(1.1);
    expect(bridge.active()).toBe(false);

    bridge.reset();
    expect(apply(bridge, {
      target: 300,
      difference: 300,
      velocity: 0,
      candidateVelocity: 1.1,
    })).toBeCloseTo(1.1);
    expect(bridge.active()).toBe(false);
  });

  it('clears motion on an in-tick zero-distance snap and rejects invalid bounds', () => {
    const bridge = createRetargetAccelerationBridge();
    apply(bridge);
    expect(apply(bridge, { difference: 0, candidateVelocity: 0 })).toBe(0);
    expect(bridge.active()).toBe(false);
    expect(() => apply(bridge, { stepFraction: 0 })).toThrow(/stepFraction/);
    expect(() => apply(bridge, { maxVelocity: Number.NaN })).toThrow(/maxVelocity/);
  });
});
