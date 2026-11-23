import { describe, expect, it } from 'vitest';
import { SEEDS, seededFraction, seededRandom } from './seededRandom';

const DRAWS = 256;

describe('seededRandom', () => {
  it('repeats its sequence for a seed', () => {
    const a = seededRandom(5);
    const b = seededRandom(5);
    const drawsA = Array.from({ length: DRAWS }, () => a(1000));
    expect(Array.from({ length: DRAWS }, () => b(1000))).toEqual(drawsA);
  });

  it('draws every value of a small range without a period', () => {
    const next = seededRandom(1);
    const bits = Array.from({ length: DRAWS }, () => next(2));
    const alternates = bits.every((bit, i) => i === 0 || bit !== bits[i - 1]);
    expect(alternates).toBe(false);
    const sixes = Array.from({ length: DRAWS }, () => next(6));
    expect(new Set(sixes)).toEqual(new Set([0, 1, 2, 3, 4, 5]));
  });

  it('starts neighboring seeds on unrelated draws', () => {
    const first = Array.from({ length: SEEDS }, (_, i) => seededRandom(i + 1)(10));
    expect(new Set(first).size).toBeGreaterThanOrEqual(5);
  });

  it('stays inside the range it is given', () => {
    const next = seededRandom(9);
    for (const n of [1, 2, 3, 7, 1000, 2 ** 32]) {
      for (let i = 0; i < DRAWS; i += 1) {
        const value = next(n);
        expect(Number.isInteger(value) && value >= 0 && value < n).toBe(true);
      }
    }
  });
});

describe('seededFraction', () => {
  it('draws fractions in [0, 1)', () => {
    const next = seededFraction(3);
    const values = Array.from({ length: DRAWS }, next);
    expect(values.every((value) => value >= 0 && value < 1)).toBe(true);
    expect(Math.min(...values)).toBeLessThan(0.1);
    expect(Math.max(...values)).toBeGreaterThan(0.9);
  });
});
