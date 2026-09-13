import { expect, it } from 'vitest';
import { createFrameCadence, createFrameStep } from './cadence';

it.each([24, 30, 60, 120, 165, 220, 240, 360, 480])('tracks a %sHz display', (hz) => {
  const sample = createFrameCadence();
  for (let i = 0; i < 40; i++) expect(sample(1000 / hz)).toBeCloseTo(1000 / hz, 8);
});

it('ignores isolated dropped frames and suspension, then learns a sustained slower display', () => {
  const sample = createFrameCadence();
  sample(1000 / 60);
  for (let i = 0; i < 10; i++) {
    expect(sample(1000 / 30)).toBeCloseTo(1000 / 60);
    sample(1000 / 60);
  }
  expect(sample(1000)).toBeCloseTo(1000 / 60);
  sample(1000 / 30);
  sample(1000 / 30);
  expect(sample(1000 / 30)).toBeCloseTo(1000 / 30);
  for (let i = 0; i < 80; i++) sample(1000 / 480);
  expect(sample(1000 / 480)).toBeCloseTo(1000 / 480, 3);
});

it('steps by the frame timestamp while its clock tracks delivery, including dropped frames', () => {
  const step = createFrameStep();
  const jitter = [0, 2.4, -2.4, 0.6, -0.9, 1.5];
  for (let i = 0; i < 16; i++) expect(step(6.06 + jitter[i % jitter.length], 6.06)).toBe(6.06);
  expect(step(9.0, 6.06)).toBe(6.06);
  expect(step(3.2, 6.06)).toBe(6.06);
  expect(step(13.1, 12.12)).toBe(12.12);
  expect(step(70, 66.7)).toBe(66.7);
  expect(step(6.1, 6.06)).toBe(6.06);
});

it('follows a refresh-rate change without evidence of a foreign clock', () => {
  const step = createFrameStep();
  for (let i = 0; i < 16; i++) step(16.67, 16.67);
  for (let i = 0; i < 40; i++) expect(step(4.17, 4.17)).toBe(4.17);
  for (let i = 0; i < 40; i++) expect(step(8.33, 8.33)).toBe(8.33);
});

it('holds whole delivered frames under a foreign timestamp clock and releases after an isolated glitch', () => {
  // 144Hz timestamps delivered at 165Hz: a repeat every eighth frame.
  const slower = createFrameStep();
  for (let i = 0; i < 16; i++) slower(6.06, i % 8 === 7 ? 0 : 6.94);
  expect(slower(6.06, 0)).toBe(6.06);
  expect(slower(6.4, 6.94)).toBe(6.06);
  expect(slower(12.5, 13.88)).toBeCloseTo(12.12);
  // 165Hz timestamps delivered at 144Hz: two timestamp frames in one delivery frame.
  const faster = createFrameStep();
  for (let i = 0; i < 16; i++) faster(6.94, i % 8 === 7 ? 12.12 : 6.06);
  expect(faster(6.94, 12.12)).toBe(6.94);
  expect(faster(6.94, 6.06)).toBe(6.94);
  // One glitch holds eight ticks, then the timestamp is trusted again.
  const glitch = createFrameStep();
  for (let i = 0; i < 16; i++) glitch(6.06, 6.06);
  expect(glitch(6.2, 0)).toBe(6.06);
  for (let i = 0; i < 7; i++) expect(glitch(6.4, 6.1)).toBe(6.06);
  expect(glitch(6.4, 6.1)).toBe(6.1);
});

it('falls back to elapsed time before the clocks are known', () => {
  const step = createFrameStep();
  expect(step(7, 0)).toBe(7);
  expect(step(7, 6)).toBe(6);
  expect(() => step(Number.NaN, 1)).toThrow(RangeError);
});
