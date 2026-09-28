import { describe, expect, it } from 'vitest';
import { fitsWhole, heldHeight, rangeAround, shouldMoveHeld, type HeldLimits } from './heldRows';

const uniform = (px: number) => () => px;
const limits: HeldLimits = { span: 1000, limit: 1500, edge: 200 };

describe('fitsWhole', () => {
  it('holds a list up to the limit, and not one row past it', () => {
    expect(fitsWhole(15, uniform(100), 1500)).toBe(true);
    expect(fitsWhole(16, uniform(100), 1500)).toBe(false);
    expect(fitsWhole(0, uniform(100), 0)).toBe(true);
  });
});

describe('rangeAround', () => {
  it('holds half the span above the focus and the rest from it down', () => {
    expect(rangeAround(100, uniform(100), 50, 1000)).toEqual({ start: 45, end: 55 });
  });

  it('gives the span an end of the list leaves over to the other side', () => {
    expect(rangeAround(100, uniform(100), 2, 1000)).toEqual({ start: 0, end: 10 });
    expect(rangeAround(100, uniform(100), 99, 1000)).toEqual({ start: 90, end: 100 });
  });

  it('holds the focus row however tall, and nothing that would pass the span', () => {
    const sizes = [100, 100, 5000, 100, 100];
    expect(rangeAround(5, (index) => sizes[index], 2, 1000)).toEqual({ start: 2, end: 3 });
  });

  it('clamps a focus past either end', () => {
    expect(rangeAround(20, uniform(100), 40, 1000)).toEqual({ start: 10, end: 20 });
    expect(rangeAround(20, uniform(100), -3, 1000)).toEqual({ start: 0, end: 10 });
    expect(rangeAround(0, uniform(100), 0, 1000)).toEqual({ start: 0, end: 0 });
  });

  it('stays within the span by the sizes it is given', () => {
    const sizeAt = (index: number) => 20 + (index % 7) * 13;
    for (const focus of [0, 17, 250, 499]) {
      const range = rangeAround(500, sizeAt, focus, 1000);
      expect(range.start).toBeLessThanOrEqual(focus);
      expect(range.end).toBeGreaterThan(focus);
      expect(heldHeight(sizeAt, range.start, range.end)).toBeLessThanOrEqual(1000);
      // Nothing more fits on either side.
      const more = Math.min(
        range.start > 0 ? sizeAt(range.start - 1) : Infinity,
        range.end < 500 ? sizeAt(range.end) : Infinity,
      );
      expect(heldHeight(sizeAt, range.start, range.end) + more).toBeGreaterThan(1000);
    }
  });
});

describe('shouldMoveHeld', () => {
  const view = (offset: number, total = 1000) => ({ offset, viewport: 100, total });

  it('moves near an end that is not the list end', () => {
    expect(shouldMoveHeld({ start: 10, end: 20 }, 100, view(199), limits)).toBe(true);
    expect(shouldMoveHeld({ start: 10, end: 20 }, 100, view(200), limits)).toBe(false);
    expect(shouldMoveHeld({ start: 10, end: 20 }, 100, view(700), limits)).toBe(false);
    expect(shouldMoveHeld({ start: 10, end: 20 }, 100, view(701), limits)).toBe(true);
  });

  it('stays at an end the list shares', () => {
    expect(shouldMoveHeld({ start: 0, end: 20 }, 100, view(0), limits)).toBe(false);
    expect(shouldMoveHeld({ start: 80, end: 100 }, 100, view(900), limits)).toBe(false);
  });

  it('moves when measured rows pass the limit, unless one row is all it holds', () => {
    expect(shouldMoveHeld({ start: 0, end: 100 }, 100, view(500, 1501), limits)).toBe(true);
    expect(shouldMoveHeld({ start: 0, end: 100 }, 100, view(500, 1500), limits)).toBe(false);
    expect(shouldMoveHeld({ start: 4, end: 5 }, 100, view(500, 9000), limits)).toBe(false);
  });
});
