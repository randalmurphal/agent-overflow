import { describe, expect, it } from 'vitest';
import { describeFirstDivergence, findFirstDivergence } from './firstDivergence';

const FILLER = Array.from({ length: 40 }, (_, i) => `word${i}`).join(' ');

describe('findFirstDivergence', () => {
  it('returns null for identical values', () => {
    expect(findFirstDivergence(FILLER, FILLER)).toBeNull();
    expect(findFirstDivergence([1, { a: 2 }], [1, { a: 2 }])).toBeNull();
  });

  it('locates the first differing character deep inside filler', () => {
    const divergence = findFirstDivergence(FILLER, FILLER.replace('word30', 'wordXX'));
    expect(divergence?.index).toBe(FILLER.indexOf('word30') + 4);
    expect(divergence?.receivedWindow).toContain('word30');
    expect(divergence?.expectedWindow).toContain('wordXX');
  });

  it('reports a pure truncation through the length pair', () => {
    const divergence = findFirstDivergence(FILLER.slice(0, 26), FILLER);
    expect(divergence?.index).toBe(26);
    expect(divergence?.receivedLength).toBe(26);
    expect(divergence?.expectedLength).toBe(FILLER.length);
  });

  it('bounds the window it prints', () => {
    const long = 'x'.repeat(20_000);
    const divergence = findFirstDivergence(`${long}a${long}`, `${long}b${long}`);
    expect(divergence?.receivedWindow.length).toBeLessThanOrEqual(161);
    expect(divergence?.expectedWindow.length).toBeLessThanOrEqual(161);
  });

  it('scans serialized structures, not just strings', () => {
    const divergence = findFirstDivergence(
      { blocks: ['a', 'b', 'c'] },
      { blocks: ['a', 'B', 'c'] },
    );
    expect(divergence?.expectedWindow).toContain('"B"');
  });
});

describe('toEqualWithFirstDivergence', () => {
  it('passes on deeply equal values', () => {
    expect([{ a: 1 }, 'text']).toEqualWithFirstDivergence([{ a: 1 }, 'text']);
  });

  it('fails with the divergence window instead of both bodies', () => {
    let message = '';
    try {
      expect(FILLER).toEqualWithFirstDivergence(FILLER.replace('word30', 'wordXX'));
    } catch (error) {
      message = (error as Error).message;
    }
    expect(message).toContain('first divergence at index');
    // `|` marks the divergence point inside each window.
    expect(message).toContain('word|XX');
    expect(message).toContain('word|30');
    // The whole 40-word body must NOT be in the message.
    expect(message).not.toContain('word0 word1');
  });

  it('reports a serialization-equal but deeply unequal pair honestly', () => {
    expect(describeFirstDivergence(
      Object.assign(Object.create({ inherited: 1 }), { a: 1 }),
      { a: 1 },
    )).toBe(
      'values are deeply unequal but serialize identically '
        + '(key order, prototype, or a non-enumerable difference)',
    );
  });
});
