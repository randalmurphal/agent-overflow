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

/** Continuous chase state. Each step samples the trajectory and reconciles its own readback. */
export class SpringMotion {
  private speed = 0;
  private residual = 0;
  private modeled = 0;
  private acceleration: number | null = null;
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
    this.acceleration = null;
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
    if (overshot) this.acceleration = null;
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
    this.speed = this.advanceVelocity(difference, before, fraction);
    this.residual += this.speed * fraction;
    this.modeled = current + this.residual;
    return this.modeled;
  }

  private advanceVelocity(difference: number, velocity: number, fraction: number): number {
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
    this.acceleration = null;
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
