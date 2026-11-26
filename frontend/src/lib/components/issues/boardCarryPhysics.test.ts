// The carried card's plate: how it hangs from where it was grabbed, how
// the hand swings it, and the damping that settles it once the hand slows.

import { describe, expect, it } from 'vitest';
import { plateAt, plateResting, stepPlate, type Plate } from './boardCarryPhysics';

const W = 288;
const H = 104;
const FRAME = 1 / 60;

/** Runs `frames` frames with the pin at the positions `at` gives. */
function run(p: Plate, frames: number, at: (i: number) => [number, number], calm = false): void {
  for (let i = 0; i < frames; i += 1) {
    const [x, y] = at(i);
    stepPlate(p, x, y, FRAME, calm);
  }
}

describe('a carried card', () => {
  it('hangs level when held by the grab bar at its top center, and on a slant when grabbed at a corner', () => {
    const bar = plateAt(W, H, W / 2, 4, 0, 0);
    run(bar, 240, () => [0, 0]);
    expect(Math.abs(bar.ang)).toBeLessThan(1e-6);

    const corner = plateAt(W, H, 10, 10, 0, 0);
    run(corner, 600, () => [0, 0]);
    // The center settles straight under the pin: the card turns until the
    // vector from the pin to its center points down.
    const cx = corner.rx * Math.cos(corner.ang) - corner.ry * Math.sin(corner.ang);
    expect(Math.abs(cx)).toBeLessThan(2);
    expect(Math.abs(corner.ang)).toBeGreaterThan(0.5);
    expect(plateResting(corner)).toBe(true);
  });

  it('swings with the hand even when held level, and settles faster once the hand stops than while it keeps moving', () => {
    const moving = plateAt(W, H, W / 2, 4, 0, 0);
    // A quick sideways move, then the hand keeps going at a steady pace.
    run(moving, 10, (i) => [i * 20, 0]);
    expect(Math.abs(moving.ang)).toBeGreaterThan(0.01);
    const stopped = { ...moving };
    run(moving, 30, (i) => [200 + i * 8, 0]);
    run(stopped, 30, () => [200, 0]);
    expect(Math.abs(stopped.w)).toBeLessThan(Math.abs(moving.w));
  });

  it('stays level and still under reduced motion however the hand moves', () => {
    const p = plateAt(W, H, 10, 10, 0, 0);
    run(p, 60, (i) => [i * 30, Math.sin(i) * 40], true);
    expect(p.ang).toBe(0);
    expect(p.w).toBe(0);
  });

  it('takes a frame after a stall as one short step', () => {
    const a = plateAt(W, H, 10, 10, 0, 0);
    const b = plateAt(W, H, 10, 10, 0, 0);
    stepPlate(a, 0, 0, 2, false);
    stepPlate(b, 0, 0, 0.034, false);
    expect(a.ang).toBeCloseTo(b.ang, 12);
  });
});
