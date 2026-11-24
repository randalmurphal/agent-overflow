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
    // Overlapping mapped matches merge: [0, 2] and [1, 3] here.
    ['İİİ', '\u0307i', [[0, 3]]],
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
  const text = (value: string) => ({ type: 'text', value });
  const match = (value: string) => ({ type: 'match', value });
  it.each([
    ['returns a single text segment for an empty query', 'hello world', '', [text('hello world')]],
    ['returns a single text segment for a whitespace-only query', 'hello', '  \t\n', [text('hello')]],
    ['returns [] for an empty input text', '', 'x', []],
    ['splits a single match in the middle', 'foo bar baz', 'bar', [text('foo '), match('bar'), text(' baz')]],
    ['splits a match at the start', 'bar baz', 'bar', [match('bar'), text(' baz')]],
    ['splits a match at the end', 'foo bar', 'bar', [text('foo '), match('bar')]],
    ['handles an exact match that equals the whole text', 'bar', 'bar', [match('bar')]],
    ['case-insensitive but preserves the original casing in the output', 'Hello WORLD', 'world', [text('Hello '), match('WORLD')]],
    ['returns only a text segment when the query is not found', 'hello', 'xyz', [text('hello')]],
    [
      'highlights every occurrence of a repeated match',
      'abXabXab',
      'ab',
      [match('ab'), text('X'), match('ab'), text('X'), match('ab')],
    ],
    // "aaa" searching for "aa" matches at index 0 but advances past that
    // span, so it does NOT match again at index 1.
    ['does not double-highlight overlapping matches', 'aaa', 'aa', [match('aa'), text('a')]],
    // Users often paste queries with stray whitespace; trim keeps the
    // matching contract predictable.
    ['leaves the query intact when whitespace surrounds it (trimmed)', 'foo bar', '  bar  ', [text('foo '), match('bar')]],
    ['trims the query but internal whitespace is part of the match', 'a b c', 'b ', [text('a '), match('b'), text(' c')]],
  ])('%s', (_name, input, query, want) => {
    expect(computeHighlightSegments(input, query)).toEqual(want);
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
});
