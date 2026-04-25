import { describe, expect, it } from 'vitest';
import { parseJsonObject } from './parseJsonObject';

describe('parseJsonObject', () => {
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
});
