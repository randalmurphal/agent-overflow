import { describe, expect, it } from 'vitest';

import { alignRevealed, suffixPrefixOverlap } from './textOverlap';

describe('suffixPrefixOverlap', () => {
  // The definition, checked by trying every length.
  const bruteForce = (existing: string, text: string): number => {
    for (let k = Math.min(existing.length, text.length); k > 0; k--) {
      if (existing.endsWith(text.slice(0, k))) return k;
    }
    return 0;
  };

  it('matches the longest prefix the existing text ends with', () => {
    expect(suffixPrefixOverlap('hello wor', 'lo world')).toBe(6);
    expect(suffixPrefixOverlap('abc', 'def')).toBe(0);
    expect(suffixPrefixOverlap('aaaa', 'aaaaaa')).toBe(4);
    expect(suffixPrefixOverlap('abab', 'ababab')).toBe(4);
    expect(suffixPrefixOverlap('xabaab', 'abaabx')).toBe(5);
  });

  it('agrees with the definition on random small-alphabet strings', () => {
    let seed = 7;
    const random = (n: number) => {
      seed = (seed * 1103515245 + 12345) & 0x7fffffff;
      return seed % n;
    };
    const word = () => Array.from({ length: random(12) }, () => 'ab\n'[random(3)]).join('');
    for (let i = 0; i < 5000; i++) {
      const existing = word();
      const text = word();
      expect(suffixPrefixOverlap(existing, text), JSON.stringify([existing, text]))
        .toBe(bruteForce(existing, text));
    }
  });

  it('stays linear when the overlap is far shorter than the texts', () => {
    // Trying each length from the longest compares ~100k characters for
    // each of 100k lengths before reaching the match.
    const existing = 'a'.repeat(200_000);
    const text = `${'a'.repeat(100_000)}b${'a'.repeat(99_999)}`;
    const started = performance.now();
    expect(suffixPrefixOverlap(existing, text)).toBe(100_000);
    expect(performance.now() - started).toBeLessThan(1_000);
  });
});

describe('alignRevealed', () => {
  it('appends nothing when existing already starts with revealed (snapshot ahead)', () => {
    // Routine mid-stream-expand: the flushed snapshot leads the smoother reveal.
    expect(alignRevealed('The quick brown fox ', 'The quick ')).toEqual({ offset: 0, suffix: '' });
  });

  it('appends nothing for a contained reveal that the end of existing does not overlap', () => {
    // "The quick brown" ends with no prefix of "The quick", so the overlap
    // scan alone would re-append the whole reveal. Containment finds it.
    expect(suffixPrefixOverlap('The quick brown', 'The quick')).toBe(0);
    expect(alignRevealed('The quick brown', 'The quick')).toEqual({ offset: 0, suffix: '' });
  });

  it('appends nothing when existing equals revealed', () => {
    expect(alignRevealed('same text', 'same text')).toEqual({ offset: 0, suffix: '' });
  });

  it('appends the continuation tail when revealed extends existing (snapshot behind)', () => {
    expect(alignRevealed('The quick ', 'The quick brown')).toEqual({ offset: 0, suffix: 'brown' });
  });

  it('appends the non-overlapping tail for a streamed continuation', () => {
    expect(alignRevealed('hello wor', 'lo world')).toEqual({ offset: 3, suffix: 'ld' });
  });

  it('appends the whole revealed text when there is no shared content', () => {
    expect(alignRevealed('full payload before ', 'live tail'))
      .toEqual({ offset: 'full payload before '.length, suffix: 'live tail' });
  });

  it('places a reconnect interior window already contained in the snapshot', () => {
    // On reconnect the smoother reseeds from the bounded tail, so its revealed
    // slice ('gamma delta') is an interior substring of the flushed snapshot,
    // not a prefix. startsWith misses it; containment catches it; nothing is
    // re-appended. Without this the snapshot's interior gets duplicated.
    expect(alignRevealed('alpha beta gamma delta epsilon', 'gamma delta'))
      .toEqual({ offset: 11, suffix: '' });
  });

  it('appends only the new tail when an interior reveal overtakes the snapshot', () => {
    // The reveal starts inside the flush ('gamma ' is a suffix of existing) but
    // extends past it with genuinely-new text ('delta'). Containment is false,
    // so the overlap scan trims the in-flush overlap and appends exactly the new
    // bytes — the new tail is never dropped.
    expect(alignRevealed('alpha beta gamma ', 'gamma delta')).toEqual({ offset: 11, suffix: 'delta' });
  });

  it('appends everything when existing is empty', () => {
    expect(alignRevealed('', 'first reveal')).toEqual({ offset: 0, suffix: 'first reveal' });
  });

  it('appends nothing when revealed is empty', () => {
    expect(alignRevealed('anything', '')).toEqual({ offset: 8, suffix: '' });
  });
});
