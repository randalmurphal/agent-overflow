import { describe, expect, it } from 'vitest';
import vectors from '../../test/fixtures/findVectors.json';
import { countFindMatches, findNeedle, foldFindText, textsMatchNeedle } from './findMatch';

// The Go CI log search (internal/cilog/search_test.go) reads the same vectors.
describe('find matching', () => {
  it('meets the vectors the CI log search shares', () => {
    expect(vectors.fold.length).toBeGreaterThan(0);
    for (const [text, folded] of vectors.fold as [string, string][]) {
      expect(foldFindText(text), text).toBe(folded);
      expect(foldFindText(text)).toHaveLength(text.length);
    }
    for (const { text, query, count } of vectors.count) {
      expect(countFindMatches(foldFindText(text), findNeedle(query)), `${query} in ${text}`).toBe(count);
    }
  });

  it('folds the same whole or in pieces', () => {
    for (const text of ['ΣΑΣ', 'İX', 'a😀B', 'AbC']) {
      expect(foldFindText(text)).toBe([...text].map(foldFindText).join(''));
    }
  });

  it('a row filter matches any of its texts by the same rule', () => {
    expect(textsMatchNeedle(findNeedle(' RETRY '), [null, 'Builds', 'needs a retry'])).toBe(true);
    expect(textsMatchNeedle(findNeedle('école'), ['École'])).toBe(false);
    expect(textsMatchNeedle('', [])).toBe(true);
  });
});
