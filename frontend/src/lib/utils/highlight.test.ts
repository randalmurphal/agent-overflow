import { describe, expect, it } from 'vitest';
import vectors from '../../test/fixtures/searchVectors.json';
import { lowerCaseHighlightSegments, searchHighlightSegments, type HighlightSegment } from './highlight';

// Where each match segment sits in the text, in UTF-16 units.
function ranges(segments: readonly HighlightSegment[]): Array<[number, number]> {
  const out: Array<[number, number]> = [];
  let at = 0;
  for (const segment of segments) {
    if (segment.type === 'match') out.push([at, at + segment.value.length]);
    at += segment.value.length;
  }
  return out;
}

describe('searchHighlightSegments', () => {
  // The same vectors hold the store's SQL (search_test.go).
  it.each(vectors.ranges)('marks what the search SQL matched: $query in $text', ({ text, query, ranges: want }) => {
    const segments = searchHighlightSegments(text, query);
    expect(segments.map((segment) => segment.value).join('')).toBe(text);
    expect(ranges(segments)).toEqual(want);
  });
});

describe('lowerCaseHighlightSegments', () => {
  it.each([
    // `İ` lower cases to two code units; the match after it keeps its place.
    ['İx', 'x', [[1, 2]]],
    ['İstanbul', 'i̇stanbul', [[0, 8]]],
    // A match inside a character's lower case covers the whole character.
    ['İx', 'i', [[0, 1]]],
    ['İİ', '\u0307i', [[0, 2]]],
    ['ÉCOLE école', 'École', [[0, 5], [6, 11]]],
    ['ΣΊΣΥΦΟΣ', 'σίσυφος', [[0, 7]]],
    ['a😀B', '😀b', [[1, 4]]],
  ] as const)('marks %j for %j as the picker filters it', (text, query, want) => {
    expect(text.toLowerCase().includes(query.trim().toLowerCase())).toBe(true);
    const segments = lowerCaseHighlightSegments(text, query);
    expect(segments.map((segment) => segment.value).join('')).toBe(text);
    expect(ranges(segments)).toEqual(want);
  });
});

describe.each([
  ['searchHighlightSegments', searchHighlightSegments],
  ['lowerCaseHighlightSegments', lowerCaseHighlightSegments],
])('%s', (_name, computeHighlightSegments) => {
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
