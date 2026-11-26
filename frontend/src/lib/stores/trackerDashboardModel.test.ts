// How a dashboard's numbers read, and the change mark against the range
// before: none for a number with no earlier value, a gap said in words,
// and the tone by the metric's good direction.

import { describe, expect, it } from 'vitest';
import { changeMark, formatNum, GAP_TEXT, numValue } from './trackerDashboardModel';
import { band, labelEvery, valueScale } from '../components/issues/charts/chartScale';

describe('a number', () => {
  it('reads in its unit, as none, or as a gap', () => {
    expect(formatNum({ v: 3 }, 'estimate', 'points')).toBe('3 points');
    expect(formatNum({ v: 1 }, 'estimate', 'points')).toBe('1 point');
    expect(formatNum({ v: 2.25 }, 'days', 'points')).toBe('2.3 days');
    expect(formatNum({ v: 4 }, 'estimate', 'none')).toBe('4 tickets');
    expect(formatNum({ v: 1 }, 'tickets', 'points')).toBe('1 ticket');
    expect(formatNum({ v: 5400 }, 'estimate', 'time')).toBe('1.5h');
    expect(formatNum({}, 'tickets', 'points')).toBe('None');
    expect(formatNum({ gap: true }, 'tickets', 'points')).toBe(GAP_TEXT);
    expect(numValue({ gap: true })).toBeNull();
    expect(numValue({ v: 0 })).toBe(0);
  });
});

describe('the change mark', () => {
  it('is none for a number with no earlier value, such as one that counts now', () => {
    expect(changeMark({ v: 3 }, undefined, 'up', 'tickets', 'points', '')).toBeNull();
    expect(changeMark({ v: 3 }, null, 'up', 'tickets', 'points', '')).toBeNull();
  });

  it('is none for a number without a value', () => {
    expect(changeMark({}, { v: 2 }, 'up', 'tickets', 'points', '')).toBeNull();
  });

  it('says why when the range before is a gap', () => {
    const why = 'This computer keeps fewer than 5 finished sprints before these to compare with.';
    expect(changeMark({ v: 3 }, { gap: true }, 'up', 'tickets', 'points', why)).toEqual({ kind: 'gap', text: why });
  });

  it('is good, bad or neutral by the metric\'s good direction', () => {
    expect(changeMark({ v: 12 }, { v: 9 }, 'up', 'estimate', 'points', '')).toEqual({ kind: 'change', direction: 'up', tone: 'good', text: 'Up 3 points from 9' });
    expect(changeMark({ v: 12 }, { v: 9 }, 'down', 'estimate', 'points', '')).toMatchObject({ direction: 'up', tone: 'bad' });
    expect(changeMark({ v: 2 }, { v: 4.5 }, 'down', 'days', 'points', '')).toEqual({ kind: 'change', direction: 'down', tone: 'good', text: 'Down 2.5 days from 4.5' });
    expect(changeMark({ v: 2 }, { v: 4 }, '', 'tickets', 'points', '')).toMatchObject({ direction: 'down', tone: 'neutral' });
    expect(changeMark({ v: 4 }, { v: 4 }, 'up', 'tickets', 'points', '')).toEqual({ kind: 'change', direction: 'same', tone: 'neutral', text: 'Same as before' });
  });
});

describe('the charts\' geometry', () => {
  it('rounds the value axis to 1, 2 or 5 steps from 0', () => {
    expect(valueScale(0)).toEqual({ max: 1, ticks: [0, 1] });
    expect(valueScale(17)).toEqual({ max: 20, ticks: [0, 5, 10, 15, 20] });
    expect(valueScale(3)).toEqual({ max: 3, ticks: [0, 1, 2, 3] });
    expect(valueScale(0.7).max).toBeCloseTo(0.8);
  });

  it('lays bands along a length with the gap between them', () => {
    expect(band(0, 3, 100, 2)).toEqual({ start: 0, size: 32 });
    expect(band(2, 3, 100, 2)).toEqual({ start: 68, size: 32 });
    expect(band(0, 0, 100)).toEqual({ start: 0, size: 0 });
  });

  it('draws every label that fits and skips the rest evenly', () => {
    expect(labelEvery(10, 640)).toBe(1);
    expect(labelEvery(10, 320)).toBe(2);
    expect(labelEvery(12, 100)).toBe(12);
  });
});
