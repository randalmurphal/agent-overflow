// Acceleration continuity for a spring whose target moves while it is
// already gliding. Fixed-target velocity shaping stays in spring.ts.
// This state machine owns only the discontinuity introduced when a new
// same-direction target interrupts braking — or a held speed (the
// envelope's minimum, the motion floor's rung), where the jump from zero
// acceleration straight to the slew ramp's is the same kick.

const RETARGET_JERK_FLOOR_PX_PER_FRAME_CUBED = 0.1;
const RETARGET_JERK_ENDPOINT_FRACTION = 0.125;

export interface RetargetAccelerationBridge {
  /**
   * Resolve one candidate velocity and commit the acceleration actually
   * taken. A zero-distance step is valid after an in-tick resume snap.
   * It clears motion history before returning the candidate. Positional
   * arguments avoid allocating an options object on every display frame.
   */
  step(
    target: number,
    difference: number,
    velocity: number,
    candidateVelocity: number,
    stepFraction: number,
    maxVelocity: number,
  ): number;
  /** Visible motion ended, but the current target remains the baseline. */
  breakMotion(): void;
  /** A new spring lifecycle starts with no target or motion history. */
  reset(): void;
  /** Test and trace observability only. */
  active(): boolean;
}

export function createRetargetAccelerationBridge(): RetargetAccelerationBridge {
  let lastAcceleration = 0;
  let bridgeAcceleration: number | null = null;
  let jerkLimit = 0;
  let wasMovingTowardTarget = false;
  let lastTarget: number | null = null;

  function breakMotion(): void {
    lastAcceleration = 0;
    bridgeAcceleration = null;
    jerkLimit = 0;
    wasMovingTowardTarget = false;
  }

  function reset(): void {
    breakMotion();
    lastTarget = null;
  }

  function step(
    target: number,
    difference: number,
    velocity: number,
    candidateVelocity: number,
    stepFraction: number,
    maxVelocity: number,
  ): number {
    if (!(stepFraction > 0) || !Number.isFinite(stepFraction)) {
      throw new RangeError(
        `retarget stepFraction must be finite and positive, got ${stepFraction}`,
      );
    }
    if (!(maxVelocity > 0) || !Number.isFinite(maxVelocity)) {
      throw new RangeError(
        `retarget maxVelocity must be finite and positive, got ${maxVelocity}`,
      );
    }
    const direction = Math.sign(difference);
    if (direction === 0) {
      breakMotion();
      lastTarget = target;
      return Math.max(
        -maxVelocity,
        Math.min(maxVelocity, candidateVelocity),
      );
    }

    const targetChanged = lastTarget !== null && target !== lastTarget;
    lastTarget = target;
    const candidateAcceleration =
      (candidateVelocity - velocity) / stepFraction;
    const lastTowardAcceleration = lastAcceleration * direction;
    const candidateTowardAcceleration = candidateAcceleration * direction;

    if (
      bridgeAcceleration === null
      && targetChanged
      && wasMovingTowardTarget
      && velocity * direction > 0
      && lastTowardAcceleration <= 0
      && candidateTowardAcceleration > lastTowardAcceleration
    ) {
      bridgeAcceleration = lastAcceleration;
      jerkLimit = Math.max(
        RETARGET_JERK_FLOOR_PX_PER_FRAME_CUBED,
        Math.max(
          Math.abs(lastTowardAcceleration),
          Math.abs(candidateTowardAcceleration),
        ) * RETARGET_JERK_ENDPOINT_FRACTION,
      );
    }

    let integratedAcceleration = candidateAcceleration;
    if (bridgeAcceleration !== null) {
      const bridgeTowardAcceleration = bridgeAcceleration * direction;
      // Landing safely outranks smoothing. The bridge rounds only the
      // braking-to-driving change caused by an extended target. Equal or
      // stronger braking returns immediately to the fixed-target curve.
      if (candidateTowardAcceleration <= bridgeTowardAcceleration) {
        bridgeAcceleration = null;
        jerkLimit = 0;
      } else {
        const nextTowardAcceleration = Math.min(
          candidateTowardAcceleration,
          bridgeTowardAcceleration + jerkLimit * stepFraction,
        );
        integratedAcceleration = nextTowardAcceleration * direction;
        bridgeAcceleration = integratedAcceleration;
        if (nextTowardAcceleration === candidateTowardAcceleration) {
          bridgeAcceleration = null;
          jerkLimit = 0;
        }
      }
    }

    const nextVelocity = Math.max(
      -maxVelocity,
      Math.min(
        maxVelocity,
        velocity + integratedAcceleration * stepFraction,
      ),
    );
    lastAcceleration = (nextVelocity - velocity) / stepFraction;
    wasMovingTowardTarget = nextVelocity * direction > 0;
    return nextVelocity;
  }

  return {
    step,
    breakMotion,
    reset,
    active: () => bridgeAcceleration !== null,
  };
}
