import { describe, expect, it } from 'vitest';
import { uniqueEachKeys } from './uniqueEachKeys';

const label = (v: { label: string }): string => v.label;

describe('uniqueEachKeys', () => {
  it('returns the natural keys untouched when they are already unique', () => {
    const items = [{ label: 'a' }, { label: 'b' }, { label: 'c' }];
    expect(uniqueEachKeys(items, label)).toEqual(['a', 'b', 'c']);
  });

  it('handles the empty and single-item lists', () => {
    expect(uniqueEachKeys([], label)).toEqual([]);
    expect(uniqueEachKeys([{ label: 'only' }], label)).toEqual(['only']);
  });

  it('suffixes the later occurrences of a repeated key', () => {
    const items = [{ label: 'Yes' }, { label: 'No' }, { label: 'Yes' }, { label: 'Yes' }];
    expect(uniqueEachKeys(items, label)).toEqual(['Yes', 'No', 'Yes#2', 'Yes#3']);
  });

  it('keeps one key per row, so no row is dropped', () => {
    const items = [{ label: 'x' }, { label: 'x' }, { label: 'x' }];
    const keys = uniqueEachKeys(items, label);
    expect(keys).toHaveLength(items.length);
    expect(new Set(keys).size).toBe(items.length);
  });

  // A producer that already emits the repair shape must not collide with it.
  it('skips past a suffix a natural key already occupies', () => {
    const items = [{ label: 'a' }, { label: 'a#2' }, { label: 'a' }];
    const keys = uniqueEachKeys(items, label);
    expect(keys).toEqual(['a', 'a#2', 'a#3']);
    expect(new Set(keys).size).toBe(items.length);
  });

  it('never returns a duplicate for any input', () => {
    const items = ['a', '', 'a', 'a#2', '', 'b', 'a'].map((v) => ({ label: v }));
    const keys = uniqueEachKeys(items, label);
    expect(new Set(keys).size).toBe(items.length);
  });
});
