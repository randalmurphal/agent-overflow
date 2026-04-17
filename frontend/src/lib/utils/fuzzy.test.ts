import { describe, expect, it } from 'vitest';
import { fuzzyFilter, fuzzyMatch } from './fuzzy';

describe('fuzzyMatch', () => {
  it('returns empty-match for empty query', () => {
    expect(fuzzyMatch('', 'thread.new')).toEqual({ score: 0, indices: [] });
  });

  it('returns null when the target does not contain the subsequence', () => {
    expect(fuzzyMatch('xz', 'hello')).toBeNull();
  });

  it('matches a substring as a high-scoring consecutive run', () => {
    const m = fuzzyMatch('new', 'thread.new')!;
    expect(m).not.toBeNull();
    // Indices point at "new".
    expect(m.indices).toEqual([7, 8, 9]);
    expect(m.score).toBeGreaterThan(0);
  });

  it('matches a subsequence with gaps', () => {
    const m = fuzzyMatch('tn', 'thread.new')!;
    expect(m.indices).toEqual([0, 7]);
  });
});

describe('fuzzyFilter', () => {
  it('returns all candidates for empty query (sorted stable)', () => {
    const items = [
      { item: 1, text: 'a' },
      { item: 2, text: 'bb' },
    ];
    const out = fuzzyFilter('', items);
    expect(out.map((o) => o.item)).toEqual([1, 2]);
  });

  it('ranks consecutive substring matches above spread-out subsequences', () => {
    const items = [
      { item: 'disc', text: 'thread.new.discussion' },
      { item: 'new', text: 'thread.new' },
      { item: 'arch', text: 'thread.archive' },
    ];
    const out = fuzzyFilter('new', items);
    expect(out.map((o) => o.item).slice(0, 2)).toEqual(['new', 'disc']);
    // 'arch' does not contain all of "new".
    expect(out.map((o) => o.item)).not.toContain('arch');
  });

  it('ties break on shorter target length', () => {
    const items = [
      { item: 'short', text: 'abc' },
      { item: 'long', text: 'abcxxx' },
    ];
    const out = fuzzyFilter('abc', items);
    expect(out.map((o) => o.item)).toEqual(['short', 'long']);
  });
});
