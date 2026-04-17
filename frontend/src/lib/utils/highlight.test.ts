import { describe, expect, it } from 'vitest';
import { computeHighlightSegments } from './highlight';

describe('computeHighlightSegments', () => {
  it('returns a single text segment for an empty query', () => {
    expect(computeHighlightSegments('hello world', '')).toEqual([
      { type: 'text', value: 'hello world' },
    ]);
  });

  it('returns a single text segment for a whitespace-only query', () => {
    expect(computeHighlightSegments('hello', '  \t\n')).toEqual([
      { type: 'text', value: 'hello' },
    ]);
  });

  it('returns [] for an empty input text', () => {
    expect(computeHighlightSegments('', 'x')).toEqual([]);
  });

  it('splits a single match in the middle', () => {
    expect(computeHighlightSegments('foo bar baz', 'bar')).toEqual([
      { type: 'text', value: 'foo ' },
      { type: 'match', value: 'bar' },
      { type: 'text', value: ' baz' },
    ]);
  });

  it('splits a match at the start', () => {
    expect(computeHighlightSegments('bar baz', 'bar')).toEqual([
      { type: 'match', value: 'bar' },
      { type: 'text', value: ' baz' },
    ]);
  });

  it('splits a match at the end', () => {
    expect(computeHighlightSegments('foo bar', 'bar')).toEqual([
      { type: 'text', value: 'foo ' },
      { type: 'match', value: 'bar' },
    ]);
  });

  it('handles an exact match that equals the whole text', () => {
    expect(computeHighlightSegments('bar', 'bar')).toEqual([
      { type: 'match', value: 'bar' },
    ]);
  });

  it('case-insensitive but preserves the original casing in the output', () => {
    expect(computeHighlightSegments('Hello WORLD', 'world')).toEqual([
      { type: 'text', value: 'Hello ' },
      { type: 'match', value: 'WORLD' },
    ]);
  });

  it('returns only a text segment when the query is not found', () => {
    expect(computeHighlightSegments('hello', 'xyz')).toEqual([
      { type: 'text', value: 'hello' },
    ]);
  });

  it('highlights every occurrence of a repeated match', () => {
    expect(computeHighlightSegments('abXabXab', 'ab')).toEqual([
      { type: 'match', value: 'ab' },
      { type: 'text', value: 'X' },
      { type: 'match', value: 'ab' },
      { type: 'text', value: 'X' },
      { type: 'match', value: 'ab' },
    ]);
  });

  it('does not double-highlight overlapping matches', () => {
    // "aaa" searching for "aa" matches at index 0 but advances past that
    // span, so it does NOT match again at index 1.
    expect(computeHighlightSegments('aaa', 'aa')).toEqual([
      { type: 'match', value: 'aa' },
      { type: 'text', value: 'a' },
    ]);
  });

  it('preserves unicode in both text and match segments', () => {
    expect(computeHighlightSegments('こんにちは world', '世界 world')).toEqual([
      { type: 'text', value: 'こんにちは world' },
    ]);
    expect(computeHighlightSegments('こんにちは 世界', '世界')).toEqual([
      { type: 'text', value: 'こんにちは ' },
      { type: 'match', value: '世界' },
    ]);
  });

  it('leaves the query intact when whitespace surrounds it (trimmed)', () => {
    // Users often paste queries with stray whitespace; trim keeps the
    // matching contract predictable.
    expect(computeHighlightSegments('foo bar', '  bar  ')).toEqual([
      { type: 'text', value: 'foo ' },
      { type: 'match', value: 'bar' },
    ]);
  });

  it('trims the query but internal whitespace is part of the match', () => {
    expect(computeHighlightSegments('a b c', 'b ')).toEqual([
      { type: 'text', value: 'a ' },
      { type: 'match', value: 'b' },
      { type: 'text', value: ' c' },
    ]);
  });
});
