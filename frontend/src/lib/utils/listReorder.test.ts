import { describe, expect, it } from 'vitest';
import { displacement, dropIndex, moveItem } from './listReorder';

describe('listReorder', () => {
  it('moves one item to a new index', () => {
    expect(moveItem(['a', 'b', 'c', 'd'], 0, 2)).toEqual(['b', 'c', 'a', 'd']);
    expect(moveItem(['a', 'b', 'c', 'd'], 3, 1)).toEqual(['a', 'd', 'b', 'c']);
    expect(moveItem(['a', 'b'], 1, 1)).toEqual(['a', 'b']);
  });

  // Cards 40px tall with 10px gaps: middles 20, 70, 120, 170.
  const middles = [20, 70, 120, 170];

  it('lands the held item past every middle it crossed', () => {
    expect(dropIndex(middles, 0, 20)).toBe(0);
    expect(dropIndex(middles, 0, 69)).toBe(0);
    expect(dropIndex(middles, 0, 71)).toBe(1);
    expect(dropIndex(middles, 0, 400)).toBe(3);
    expect(dropIndex(middles, 3, 119)).toBe(2);
    expect(dropIndex(middles, 3, -50)).toBe(0);
  });

  it('opens the landing slot by moving only the items between', () => {
    // Held from 0 over 2: items 1 and 2 move up a slot.
    expect([0, 1, 2, 3].map((i) => displacement(i, 0, 2, 50))).toEqual([0, -50, -50, 0]);
    // Held from 3 over 1: items 1 and 2 move down a slot.
    expect([0, 1, 2, 3].map((i) => displacement(i, 3, 1, 50))).toEqual([0, 50, 50, 0]);
    expect([0, 1, 2, 3].map((i) => displacement(i, 2, 2, 50))).toEqual([0, 0, 0, 0]);
  });
});
