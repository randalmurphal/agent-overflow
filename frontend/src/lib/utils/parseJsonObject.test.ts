import { beforeEach, describe, expect, it } from 'vitest';
import { __resetParseJsonObjectCacheForTest, parseJsonObject } from './parseJsonObject';

describe('parseJsonObject', () => {
  beforeEach(() => {
    __resetParseJsonObjectCacheForTest();
  });

  it('returns null for undefined / null / empty input', () => {
    expect(parseJsonObject(undefined)).toBeNull();
    expect(parseJsonObject(null)).toBeNull();
    expect(parseJsonObject('')).toBeNull();
  });

  it('parses a valid JSON object', () => {
    expect(parseJsonObject('{"a":1,"b":"x"}')).toEqual({ a: 1, b: 'x' });
  });

  it('returns null for an array root (Array.isArray guard)', () => {
    // Arrays satisfy `typeof === 'object'` but the helper's contract
    // is "Record-shaped object or null" — array roots must not leak
    // through as records.
    expect(parseJsonObject('[]')).toBeNull();
    expect(parseJsonObject('[1, 2, 3]')).toBeNull();
  });

  it('returns null for primitive JSON roots', () => {
    expect(parseJsonObject('null')).toBeNull();
    expect(parseJsonObject('"hello"')).toBeNull();
    expect(parseJsonObject('42')).toBeNull();
    expect(parseJsonObject('true')).toBeNull();
  });

  it('returns null when JSON.parse throws on garbage input', () => {
    expect(parseJsonObject('not json')).toBeNull();
    expect(parseJsonObject('{')).toBeNull();
    expect(parseJsonObject('{"a":')).toBeNull();
  });

  it('returns the same object reference for repeated calls with the same source string', () => {
    // The transcript hot path reads parseJsonObject(item.meta) on every
    // render. Returning a fresh object each time would propagate a new
    // reference through every $derived that reads from the parsed
    // object, defeating Svelte 5's signal-equality skip optimisations.
    const source = '{"a":1,"b":"x"}';
    const first = parseJsonObject(source);
    const second = parseJsonObject(source);
    expect(first).not.toBeNull();
    expect(first).toBe(second);
  });

  it('caches the null result when JSON.parse fails so repeated bad input does not re-throw', () => {
    const first = parseJsonObject('garbage');
    const second = parseJsonObject('garbage');
    expect(first).toBeNull();
    expect(second).toBeNull();
  });

  it('serves distinct sources independently', () => {
    const a = parseJsonObject('{"x":1}');
    const b = parseJsonObject('{"y":2}');
    expect(a).toEqual({ x: 1 });
    expect(b).toEqual({ y: 2 });
    expect(a).not.toBe(b);
  });
});
