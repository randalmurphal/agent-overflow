// How a dashboard's numbers read, and the change mark against the range
// before: none for a number with no earlier value, a gap said in words,
// and the tone by the metric's good direction.

import { describe, expect, it } from 'vitest';
import { changeMark, formatNum, GAP_TEXT, numValue, numWords } from './trackerDashboardModel';
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

  it('says what it could not count beside it, and is not known when it counted nothing', () => {
    expect(formatNum({ v: 12, nc: 3 }, 'days', 'points')).toBe('12 days, 3 not counted');
    expect(formatNum({ nc: 2 }, 'tickets', 'points')).toBe('Not known, 2 not counted');
    expect(numWords({ nc: 2 }, 'tickets', 'points')).toEqual({ amount: 'Not known', word: '', value: 'Not known', notCounted: '2 not counted' });
    expect(numWords({ v: 1, nc: 1 }, 'tickets', 'points')).toEqual({ amount: '1', word: 'ticket', value: '1 ticket', notCounted: '1 not counted' });
    expect(numValue({ nc: 2 })).toBeNull();
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

  it('is none when either range is not known, and stands beside what was not counted', () => {
    expect(changeMark({ nc: 2 }, { v: 2 }, 'up', 'tickets', 'points', '')).toBeNull();
    expect(changeMark({ v: 3 }, { nc: 1 }, 'up', 'tickets', 'points', '')).toBeNull();
    expect(changeMark({ v: 3, nc: 1 }, { v: 2 }, 'up', 'tickets', 'points', '')).toMatchObject({ kind: 'change', direction: 'up' });
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

  it('counts its ticks whatever the top, the largest number included', () => {
    expect(valueScale(1.79e308)).toEqual({ max: 1.79e308, ticks: [0, 1.79e308] });
    expect(valueScale(Number.MAX_VALUE).ticks).toHaveLength(2);
    expect(valueScale(Number.MIN_VALUE).ticks.length).toBeLessThanOrEqual(9);
    expect(valueScale(Infinity)).toEqual({ max: 1, ticks: [0, 1] });
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
