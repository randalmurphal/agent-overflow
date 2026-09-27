import type { SmoothingClock } from '../../lib/markdown/smoothing/PerItemSmoother';

/**
 * A reveal clock a test advances by hand (`__setSmoothingClockForTest`).
 * `tickFrame` fires every callback scheduled so far, so a loop over it
 * drains a smoother deterministically, independent of the reveal rate.
 */
export class FakeSmoothingClock implements SmoothingClock {
  private current = 0;
  private nextHandle = 1;
  private pending = new Map<number, () => void>();
  now(): number { return this.current; }
  schedule(cb: () => void): number {
    const h = this.nextHandle++;
    this.pending.set(h, cb);
    return h;
  }
  cancel(h: number): void { this.pending.delete(h); }
  tickFrame(ms: number): void {
    this.current += ms;
    const toFire = [...this.pending.values()];
    this.pending.clear();
    for (const cb of toFire) cb();
  }
}
