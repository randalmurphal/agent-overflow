import { createRetargetAccelerationBridge } from './retarget';
import { sampleScrollPosition, scrollReadbackTolerance } from './position';
import type { ScrollGrid } from './grid';

// Units are CSS pixels and 60Hz-equivalent time, independent of display cadence.
const RETENTION = 0.7 / 1.25;
const FOLLOW_RATIO = 0.08 / (1.25 - 0.7);
const SLEW = 1.1;
const FLOOR = 1;
const FLOOR_RELEASE_DISTANCE = 3;
const CARRY_CEILING = 4;
export const SPRING_MAX_CATCHUP_STEPS = 1;
export const SPRING_MAX_VELOCITY_PX_PER_FRAME = 27;

/** Continuous chase state. Each step samples the trajectory and reconciles its own readback. */
export class SpringMotion {
  private speed = 0;
  private residual = 0;
  private modeled = 0;
  private floorEngaged = false;
  private readback: number | null = null;
  private quantum = 0;
  private offset = 0;
  private readonly retarget = createRetargetAccelerationBridge();

  get velocity(): number { return this.speed; }

  reset(): void {
    this.speed = 0;
    this.rebase();
    this.readback = null;
    this.retarget.reset();
  }

  private rebase(): void {
    this.residual = 0;
    this.floorEngaged = false;
    this.retarget.breakMotion();
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
    if (overshot) this.retarget.breakMotion();
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
    const direction = Math.sign(difference);
    const before = this.speed;
    const slew = Math.max(FLOOR, before * direction) * SLEW ** fraction;
    const envelope = Math.min(SPRING_MAX_VELOCITY_PX_PER_FRAME, Math.max(1.6, Math.abs(difference) * 0.09));
    const retention = RETENTION ** fraction;
    let candidate = retention * before + (1 - retention) * FOLLOW_RATIO * difference;
    candidate = Math.max(-SPRING_MAX_VELOCITY_PX_PER_FRAME, Math.min(SPRING_MAX_VELOCITY_PX_PER_FRAME, candidate));
    if (candidate * direction > 0) candidate = direction * Math.min(Math.abs(candidate), envelope, slew);
    if (Math.abs(candidate) >= FLOOR) this.floorEngaged = true;
    else if (this.floorEngaged && candidate * direction > 0) {
      // A square-root speed envelope is constant-deceleration braking in
      // distance space. It reaches the endpoint without an asymptotic crawl.
      const landingFloor = FLOOR * Math.sqrt(Math.min(1, Math.abs(difference) / FLOOR_RELEASE_DISTANCE));
      candidate = direction * Math.max(Math.abs(candidate), landingFloor);
    }
    this.speed = this.retarget.step(target, difference, before, candidate, fraction, SPRING_MAX_VELOCITY_PX_PER_FRAME);
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
    this.retarget.breakMotion();
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
