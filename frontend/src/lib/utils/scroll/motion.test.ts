import { expect, it } from 'vitest';
import { SpringMotion } from './motion';
import { sampleScrollPosition } from './position';

const grid = { quantum: 1, writeOffset: 0, readbackError: 0 };
const accept = (_: number, position: number) => position;

it('samples position with bounded error, including constant sub-grid speeds in both directions', () => {
  for (const quantum of [1.25, 1, 0.8, 1 / 2.625]) {
    for (const hz of [60, 144, 165, 240, 480]) for (const speed of [7, 60, 137, -7, -60, -137]) {
      let current = 1000;
      const measured = { ...grid, quantum };
      let maximumError = 0;
      for (let i = 1; i <= hz * 2; i++) {
        const modeled = 1000 + speed * i / hz;
        current = sampleScrollPosition(modeled, current, speed > 0 ? 2000 : 0, measured);
        maximumError = Math.max(maximumError, Math.abs(current - modeled));
      }
      expect(maximumError).toBeLessThanOrEqual(quantum / 2 + 1e-6);
    }
  }
});

it('does not select an exact endpoint merely because rounded readback is near it', () => {
  const fractionalGrid = { ...grid, quantum: 2 / 3 };
  expect(sampleScrollPosition(2.4, 8 / 3, 3, fractionalGrid)).toBe(8 / 3);
  expect(sampleScrollPosition(2.8, 8 / 3, 3, fractionalGrid)).toBe(3);
});

it('does not reverse motion to align an existing position to a newly measured grid', () => {
  const changed = { ...grid, quantum: 0.8 };
  expect(sampleScrollPosition(1.1, 1, 60, changed)).toBe(1);
  expect(sampleScrollPosition(1.3, 1, 60, changed)).toBe(1.6);
  expect(sampleScrollPosition(1.5, 1.55, 0, changed)).toBe(1.55);
  expect(sampleScrollPosition(1.1, 1.55, 0, changed)).toBe(0.8);
});

it('reset removes velocity, fractional progress, and retarget history', () => {
  const motion = new SpringMotion();
  let current = 0;
  for (let i = 0; i < 60; i++) current = motion.step(current, 80, 0.25, grid, accept);
  motion.step(current, 160, 0.25, grid, accept);
  motion.reset();
  motion.reset();
  const fresh = new SpringMotion();
  let actual = 0;
  let expected = 0;
  for (let i = 0; i < 120; i++) {
    actual = motion.step(actual, 64, 0.25, grid, accept);
    expected = fresh.step(expected, 64, 0.25, grid, accept);
    expect(actual).toBe(expected);
    expect(motion.velocity).toBe(fresh.velocity);
  }
});

it('opting out of parked carry clears it permanently until new motion', () => {
  const motion = new SpringMotion();
  let current = 0;
  for (let i = 0; i < 30; i++) current = motion.step(current, 600, 1, grid, accept);
  motion.park(1, true);
  expect(motion.velocity).toBeGreaterThan(0);
  motion.park(1, false);
  motion.park(1, true);
  expect(motion.velocity).toBe(0);
  const fresh = new SpringMotion();
  expect(motion.step(current, 600, 0.25, grid, accept)).toBe(fresh.step(current, 600, 0.25, grid, accept));
});

it('rebases a refused write before the next attempt instead of banking motion debt', () => {
  const motion = new SpringMotion();
  for (let i = 0; i < 90; i++) expect(motion.step(0, 600, 1, grid, () => 0)).toBe(0);
  const resumed = motion.step(0, 600, 0.25, grid, accept);
  expect(resumed).toBeGreaterThan(0);
  expect(resumed).toBeLessThanOrEqual(8);
});

it('zero elapsed time and rejected inputs do not advance or contaminate motion', () => {
  const motion = new SpringMotion();
  const fresh = new SpringMotion();
  expect(motion.step(0, 60, 0, grid, () => { throw new Error('unexpected write'); })).toBe(0);
  for (const frames of [-1, Infinity, NaN]) {
    expect(() => motion.step(0, 60, frames, grid, accept)).toThrow(RangeError);
    expect(() => motion.park(frames, false)).toThrow(RangeError);
    expect(() => motion.park(frames, true)).toThrow(RangeError);
    expect(() => motion.decay(frames)).toThrow(RangeError);
  }
  for (const invalid of [{ ...grid, quantum: 0 }, { ...grid, writeOffset: NaN }, { ...grid, readbackError: -1 }]) {
    expect(() => motion.step(0, 60, 1, invalid, accept)).toThrow(RangeError);
  }
  expect(motion.step(0, 60, 1, grid, accept)).toBe(fresh.step(0, 60, 1, grid, accept));
  expect(motion.velocity).toBe(fresh.velocity);
});

it('requests an exact crossed endpoint even when the remaining write is below readback precision', () => {
  const precise = { ...grid, quantum: 1e-8 };
  for (const direction of [-1, 1]) {
    const current = 2110 - direction * 0.0001;
    expect(sampleScrollPosition(2110 + direction * 0.1, current, 2110, precise)).toBe(2110);
  }
});

it('lands across initial distances without reversing or stopping short of the target', () => {
  for (const hz of [60, 144, 165, 240, 480]) {
    for (const distance of [3, 6, 20, 60, 100, 115, 300, 600, 645, 1200, 2110, 4000]) {
      const motion = new SpringMotion();
      const precise = { ...grid, quantum: 1e-8 };
      let current = 0;
      let terminalSpeed = 0;
      let previousSpeed = 0;
      for (let tick = 0; tick < hz * 6 && current !== distance; tick++) {
        const before = current;
        previousSpeed = motion.velocity;
        current = motion.step(current, distance, 60 / hz, precise, accept);
        expect(current).toBeGreaterThanOrEqual(before);
        expect(current).toBeLessThanOrEqual(distance);
        if (current < distance) expect(motion.velocity).toBeGreaterThan(0);
        else terminalSpeed = previousSpeed;
      }
      expect(current, `${hz}Hz, ${distance}px`).toBe(distance);
      expect(terminalSpeed).toBeLessThan(1);
    }
  }
});

it.each([60, 144, 165, 240, 480])('mirrors the same acceleration history for upward and downward streams at %iHz', (hz) => {
  const up = new SpringMotion();
  const down = new SpringMotion();
  const precise = { ...grid, quantum: 1e-8 };
  let upPosition = 0;
  let downPosition = 4000;
  let target = 20;
  let nextGrowth = 1000 / 7.5;
  for (let tick = 1; tick <= hz * 4; tick++) {
    const time = tick * 1000 / hz;
    if (time >= nextGrowth - 1e-6) {
      target += 20;
      nextGrowth += 1000 / 7.5;
    }
    upPosition = up.step(upPosition, target, 60 / hz, precise, accept);
    downPosition = down.step(downPosition, 4000 - target, 60 / hz, precise, accept);
    expect(up.velocity).toBeCloseTo(-down.velocity, 6);
    expect(upPosition + downPosition).toBeCloseTo(4000, 6);
  }
});
