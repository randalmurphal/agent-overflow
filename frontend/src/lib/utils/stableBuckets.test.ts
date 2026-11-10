import { expect, it } from 'vitest';
import { reuseUnchangedBuckets } from './stableBuckets';

const a = { id: 'a' }, b = { id: 'b' }, c = { id: 'c' };

it('keeps the previous array of a bucket whose elements did not change', () => {
  const before = new Map([['one', [a, b]], ['two', [c]]]);
  const changed = { id: 'c' };
  const next = reuseUnchangedBuckets(before, new Map([['one', [a, b]], ['two', [changed]]]));
  expect(next.get('one')).toBe(before.get('one'));
  expect(next.get('two')).not.toBe(before.get('two'));
  expect(next.get('two')).toEqual([changed]);
});

it('treats a reorder, a growth, a shrink and a new key as changes', () => {
  const before = new Map([['order', [a, b]], ['grow', [a]], ['shrink', [a, b]]]);
  const next = reuseUnchangedBuckets(before, new Map([['order', [b, a]], ['grow', [a, b]], ['shrink', [a]], ['new', [c]]]));
  expect(next.get('order')).toEqual([b, a]);
  expect(next.get('grow')).toEqual([a, b]);
  expect(next.get('shrink')).toEqual([a]);
  expect(next.get('new')).toEqual([c]);
  for (const key of ['order', 'grow', 'shrink']) expect(next.get(key)).not.toBe(before.get(key));
});

it('drops a key that is gone', () => {
  const next = reuseUnchangedBuckets(new Map([['gone', [a]]]), new Map<string, { id: string }[]>());
  expect(next.has('gone')).toBe(false);
});
