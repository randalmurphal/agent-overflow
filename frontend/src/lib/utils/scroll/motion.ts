import { sampleScrollPosition, scrollReadbackTolerance } from './position';
import type { ScrollGrid } from './grid';

// Units are CSS pixels and 60Hz-equivalent time, independent of display cadence.
const RETENTION = 0.7 / 1.25;
const FOLLOW_RATIO = 0.08 / (1.25 - 0.7);
const SLEW = 1.1;
const FLOOR = 1;
const RESPONSE = 0.18;
const LANDING_ALLOWANCE = 5;
const JERK_FLOOR = 0.1;
const JERK_SPEED_RATIO = 0.02;
const JERK_SPEED_BASE = 3;
const CARRY_CEILING = 4;
export const SPRING_MAX_CATCHUP_STEPS = 1;
export const SPRING_MAX_VELOCITY_PX_PER_FRAME = 27;
/** A spring value this close to its target, moving this slowly, has arrived. */
const VALUE_LANDING = 0.5;

/**
 * The spring's velocity law: one step of velocity toward a distance, with
 * the acceleration it carries between steps. Shared by every motion that
 * must read as the same spring.
 */
export class SpringVelocity {
  acceleration: number | null = null;

  advance(difference: number, velocity: number, fraction: number): number {
    const direction = Math.sign(difference);
    if (direction === 0) {
      this.acceleration = null;
      return 0;
    }
    const toward = velocity * direction;
    if (this.acceleration === null || toward <= 0) {
      const retention = RETENTION ** fraction;
      let candidate = retention * velocity + (1 - retention) * FOLLOW_RATIO * difference;
      if (candidate * direction > 0) {
        const onset = Math.max(FLOOR, toward) * SLEW ** fraction;
        candidate = direction * Math.min(Math.abs(candidate), onset, Math.max(1.6, Math.abs(difference) * 0.09));
      }
      this.acceleration = candidate * direction > 0 ? 0 : null;
      return Math.max(-SPRING_MAX_VELOCITY_PX_PER_FRAME, Math.min(SPRING_MAX_VELOCITY_PX_PER_FRAME, candidate));
    }

    // The unconstrained response has three equal damping poles. Braking
    // depends on velocity as well as distance, before reaching the endpoint.
    const desired = RESPONSE ** 2 / 3 * (Math.abs(difference) + LANDING_ALLOWANCE) - RESPONSE * toward;
    const previous = this.acceleration * direction;
    const response = previous + (desired - previous) * (1 - Math.exp(-3 * RESPONSE * fraction));
    const slew = Math.max(FLOOR, toward) * (SLEW ** fraction - 1) / fraction;
    const jerk = Math.max(JERK_FLOOR, Math.abs(previous) * 0.125, (toward - JERK_SPEED_BASE) * JERK_SPEED_RATIO);
    // Reserve the velocity needed to bring acceleration back to zero at the cap.
    const margin = SPRING_MAX_VELOCITY_PX_PER_FRAME - toward;
    const capAcceleration = Math.sqrt(2 * jerk * margin + (jerk * fraction) ** 2) - jerk * fraction;
    const wanted = Math.min(slew, response, capAcceleration);
    const acceleration = Math.max(previous - jerk * fraction, Math.min(previous + jerk * fraction, wanted));
    const next = Math.max(0, Math.min(SPRING_MAX_VELOCITY_PX_PER_FRAME, toward + acceleration * fraction));
    this.acceleration = direction * (next - toward) / fraction;
    return direction * next;
  }
}

/**
 * A length chasing a target on the scroll spring's velocity law, for
 * frame-driven motion that is not a scroll position (a box height). It has
 * no pixel grid to reconcile, so it lands exactly on the target.
 */
export class SpringValue {
  private speed = 0;
  private readonly law = new SpringVelocity();

  /** One step from `current` toward `target`; returns the new value. */
  step(current: number, target: number, frames: number): number {
    if (!Number.isFinite(current) || !Number.isFinite(target) || !Number.isFinite(frames) || frames < 0) {
      throw new RangeError('Spring value step requires finite values and nonnegative elapsed time');
    }
    const fraction = Math.min(frames, SPRING_MAX_CATCHUP_STEPS);
    if (fraction === 0) return current;
    const difference = target - current;
    if (Math.abs(difference) <= VALUE_LANDING && Math.abs(this.speed) < VALUE_LANDING) return this.land(target);
    this.speed = this.law.advance(difference, this.speed, fraction);
    const next = current + this.speed * fraction;
    const overshot = (current < target && next > target) || (current > target && next < target);
    return overshot ? this.land(target) : next;
  }

  private land(target: number): number {
    this.speed = 0;
    this.law.acceleration = null;
    return target;
  }
}

/** Continuous chase state. Each step samples the trajectory and reconciles its own readback. */
export class SpringMotion {
  private speed = 0;
  private residual = 0;
  private modeled = 0;
  private readonly law = new SpringVelocity();
  private readback: number | null = null;
  private quantum = 0;
  private offset = 0;

  get velocity(): number { return this.speed; }

  reset(): void {
    this.speed = 0;
    this.rebase();
    this.readback = null;
  }

  private rebase(): void {
    this.residual = 0;
    this.law.acceleration = null;
  }

  step(
    current: number, target: number, frames: number, grid: ScrollGrid,
    write: (current: number, position: number, request: number, target: number, overshot: boolean) => number,
  ): number {
    if (!Number.isFinite(current) || !Number.isFinite(target) || !Number.isFinite(frames) || frames < 0
      || !Number.isFinite(grid.quantum) || grid.quantum <= 0
      || !Number.isFinite(grid.writeOffset) || !Number.isFinite(grid.readbackError) || grid.readbackError < 0) {
      throw new RangeError('Spring step requires finite positions, nonnegative elapsed time and a valid grid');
    }
    this.synchronize(current, grid);
    if (frames === 0) return current;
    const modeled = this.advance(current, target, frames);
    const position = sampleScrollPosition(modeled, current, target, grid);
    if (position === current) return current;
    const overshot = (current < target && modeled > target) || (current > target && modeled < target);
    const readback = write(current, position, position === target ? target : position + grid.writeOffset, target, overshot);
    if (!Number.isFinite(readback)) throw new RangeError('Scroll write returned a nonfinite position');
    this.accept(readback, position, target, grid);
    if (overshot) this.law.acceleration = null;
    return readback;
  }

  private synchronize(current: number, grid: ScrollGrid): void {
    if (grid.quantum !== this.quantum || grid.writeOffset !== this.offset
      || (this.readback !== null && Math.abs(current - this.readback) > scrollReadbackTolerance(grid, current))) {
      this.rebase();
    }
    this.quantum = grid.quantum;
    this.offset = grid.writeOffset;
    this.readback = current;
  }

  private advance(current: number, target: number, frames: number): number {
    const fraction = Math.min(frames, SPRING_MAX_CATCHUP_STEPS);
    const difference = target - (current + this.residual);
    const before = this.speed;
    this.speed = this.law.advance(difference, before, fraction);
    this.residual += this.speed * fraction;
    this.modeled = current + this.residual;
    return this.modeled;
  }

  private accept(readback: number, position: number, target: number, grid: ScrollGrid): void {
    const error = this.modeled - readback;
    if (position !== target && Math.abs(error) <= grid.quantum / 2 + scrollReadbackTolerance(grid, this.modeled)) {
      this.residual = error;
    } else this.rebase();
    this.readback = readback;
    if (position === target && Math.abs(this.speed) <= FLOOR) this.speed = 0;
  }

  decay(frames: number): void {
    if (!Number.isFinite(frames) || frames < 0) throw new RangeError('Spring decay requires nonnegative elapsed time');
    if (Math.abs(this.speed) > FLOOR) {
      this.speed = Math.sign(this.speed) * Math.max(FLOOR, Math.abs(this.speed) / SLEW ** frames);
    }
    this.law.acceleration = null;
  }

  park(frames: number, retain: boolean): void {
    if (!Number.isFinite(frames) || frames < 0) throw new RangeError('Spring parking requires nonnegative elapsed time');
    this.rebase();
    if (retain && this.speed > 0) {
      this.speed = Math.min(this.speed, CARRY_CEILING);
      this.decay(frames);
    } else this.speed = 0;
  }
}
