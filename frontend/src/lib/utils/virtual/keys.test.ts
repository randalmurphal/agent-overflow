import { describe, expect, it } from 'vitest';
import {
  classifyKeyedSequenceMutation,
  keyedReorderPermutation,
} from './keys';

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

describe('classifyKeyedSequenceMutation', () => {
  it.each([
    [['a', 'b'], ['a', 'b'], 'unchanged', 0],
    [['a', 'b'], ['a', 'b', 'c'], 'tail', 0],
    [['a', 'b', 'c'], ['a', 'b'], 'tail', 0],
    [['c', 'd'], ['a', 'b', 'c', 'd'], 'head', 2],
    [['a', 'b', 'c', 'd'], ['c', 'd'], 'head', -2],
    [['a', 'c'], ['a', 'b', 'c'], 'keyed', 0],
    [['a', 'b'], ['b', 'a'], 'keyed', 0],
  ] as const)('classifies %j -> %j', (prev, next, kind, headSplice) => {
    expect(classifyKeyedSequenceMutation(prev, next)).toEqual({ kind, headSplice });
  });
});
