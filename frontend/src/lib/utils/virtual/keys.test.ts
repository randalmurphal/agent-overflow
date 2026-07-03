import { describe, expect, it } from 'vitest';
import { isPureHeadTailChange, keyedReorderPermutation } from './keys';

describe('isPureHeadTailChange', () => {
  it('accepts identical key sequences', () => {
    expect(isPureHeadTailChange(['a', 'b'], ['a', 'b'], 0)).toBe(true);
  });

  it('accepts a pure tail append and a pure tail trim', () => {
    expect(isPureHeadTailChange(['a', 'b'], ['a', 'b', 'c'], 0)).toBe(true);
    expect(isPureHeadTailChange(['a', 'b', 'c'], ['a', 'b'], 0)).toBe(true);
  });

  it('accepts a pure head prepend with a matching headSplice', () => {
    expect(isPureHeadTailChange(['c', 'd'], ['a', 'b', 'c', 'd'], 2)).toBe(true);
  });

  it('accepts a pure head removal with a matching negative headSplice', () => {
    expect(isPureHeadTailChange(['a', 'b', 'c', 'd'], ['c', 'd'], -2)).toBe(true);
  });

  it('rejects a headSplice that does not account for the length change', () => {
    expect(isPureHeadTailChange(['c', 'd'], ['a', 'b', 'c', 'd'], 1)).toBe(false);
  });

  it('rejects a same-length reorder (the queued-message bump shape)', () => {
    expect(isPureHeadTailChange(['stop', 'user', 'think'], ['stop', 'think', 'user'], 0)).toBe(
      false,
    );
  });

  it('rejects a mid-list insert claimed as tail growth', () => {
    expect(isPureHeadTailChange(['a', 'c'], ['a', 'b', 'c'], 0)).toBe(false);
  });

  it('rejects a head prepend whose surviving keys also moved', () => {
    expect(isPureHeadTailChange(['c', 'd'], ['a', 'b', 'd', 'c'], 2)).toBe(false);
  });
});

describe('keyedReorderPermutation', () => {
  it('maps each new index to the key’s previous index', () => {
    expect(keyedReorderPermutation(['stop', 'user', 'think'], ['stop', 'think', 'user'])).toEqual([
      0, 2, 1,
    ]);
  });

  it('maps unknown keys to -1', () => {
    expect(keyedReorderPermutation(['a', 'b'], ['a', 'new', 'b'])).toEqual([0, -1, 1]);
  });

  it('handles removals implicitly (dropped keys just do not appear)', () => {
    expect(keyedReorderPermutation(['a', 'b', 'c'], ['c', 'a'])).toEqual([2, 0]);
  });
});
