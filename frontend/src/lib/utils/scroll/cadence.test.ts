import { expect, it } from 'vitest';
import { createFrameCadence } from './cadence';

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
