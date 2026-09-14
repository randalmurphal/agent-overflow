import { beforeEach, expect, it } from 'vitest';
import {
  __resetGlideTotalsForTest,
  beginGlideRun,
  ChaseCadence,
  foldGlideChase,
  readGlideRun,
} from './glideMeter';

beforeEach(() => __resetGlideTotalsForTest());

function warm(cadence: ChaseCadence, periodMs: number, ticks = 8): void {
  for (let i = 0; i < ticks; i++) cadence.tick(periodMs, 0.2, 0, false, false);
}

it('counts delivered-frame holes and late callbacks against the measured period', () => {
  const c = new ChaseCadence();
  warm(c, 6.06);
  expect(c.periodMs).toBeCloseTo(6.06);
  expect(c.tick(12.12, 0.3, 1, true, false)).toBe(2);
  expect(c.tick(30.3, 0.3, 1, true, false)).toBe(5);
  expect(c.tick(6.06, 4.1, 1, true, false)).toBe(1);
  expect(c.droppedFrames).toBe(5);
  expect(c.maxHoleFrames).toBe(5);
  expect(c.lateTicks).toBe(1);
});

it('flags uneven write intervals and multi-quantum step jumps, not steady cadences', () => {
  const even = new ChaseCadence();
  warm(even, 8.33);
  for (let i = 0; i < 12; i++) even.tick(8.33, 0.1, i % 2 ? 1 : 0, i % 2 === 1, false);
  expect(even.unevenWrites).toBe(0);
  expect(even.stepJumps).toBe(0);

  const uneven = new ChaseCadence();
  warm(uneven, 8.33);
  const steps = [1, 0, 1, 1, 0, 0, 1, 0, 1];
  for (const step of steps) uneven.tick(8.33, 0.1, step, step > 0, false);
  expect(uneven.writes).toBe(5);
  expect(uneven.unevenWrites).toBe(3);

  const jumpy = new ChaseCadence();
  warm(jumpy, 8.33);
  for (const step of [1, 1, 3, 1, 0, 2]) jumpy.tick(8.33, 0.1, step, step > 0, false);
  expect(jumpy.stepJumps).toBe(3);
});

it('folds chases into run totals scoped by the run start', () => {
  const before = new ChaseCadence();
  warm(before, 6.06);
  before.tick(18.18, 0.1, 1, true, true);
  foldGlideChase(before);
  const start = beginGlideRun();
  const during = new ChaseCadence();
  warm(during, 6.06);
  during.tick(12.12, 0.1, 1, true, false);
  foldGlideChase(during);
  foldGlideChase(new ChaseCadence());
  expect(readGlideRun(start)).toEqual({
    chases: 1, ticks: 9, writes: 1, droppedFrames: 1, maxHoleFrames: 2,
    lateTicks: 0, unevenWrites: 0, stepJumps: 0, fallbackTicks: 0,
  });
});

it('counts a chase that is still running when the run is read', () => {
  const start = beginGlideRun();
  const running = new ChaseCadence();
  warm(running, 6.06);
  running.tick(18.18, 0.1, 1, true, false);
  running.tick(6.06, 0.1, 1, true, false);
  expect(readGlideRun(start)).toEqual({
    chases: 1, ticks: 10, writes: 2, droppedFrames: 2, maxHoleFrames: 3,
    lateTicks: 0, unevenWrites: 0, stepJumps: 0, fallbackTicks: 0,
  });
  foldGlideChase(running);
  expect(readGlideRun(start).ticks).toBe(10);
  expect(readGlideRun(start).chases).toBe(1);
  const later = beginGlideRun();
  expect(readGlideRun(later).chases).toBe(0);
  expect(readGlideRun(later).maxHoleFrames).toBe(0);
});
