import { describe, expect, it, beforeAll } from 'vitest';
import { initSyntaxClassNames, spanSegments, type EncodedLine } from './syntaxSpans';

const line = (r?: number[]): EncodedLine => ({ r }) as EncodedLine;

beforeAll(() => {
  initSyntaxClassNames(['none', 'keyword', 'string', 'string-special', 'comment']);
});

describe('spanSegments', () => {
  it('partitions text exactly by byte runs', () => {
    const text = 'def route():';
    const segments = spanSegments(text, line([3, 1, 1, 0, 5, 2, 3, 0]));
    expect(segments.map((s) => s.text).join('')).toBe(text);
    expect(segments).toEqual([
      { text: 'def', className: 'syntax-keyword' },
      { text: ' ', className: '' },
      { text: 'route', className: 'syntax-string' },
      { text: '():', className: '' },
    ]);
  });

  it('returns a single plain segment for missing or empty runs', () => {
    expect(spanSegments('plain text', line())).toEqual([{ text: 'plain text', className: '' }]);
    expect(spanSegments('plain text', null)).toEqual([{ text: 'plain text', className: '' }]);
    expect(spanSegments('plain text', line([]))).toEqual([{ text: 'plain text', className: '' }]);
  });

  it('returns no segments for empty text', () => {
    expect(spanSegments('', line([3, 1]))).toEqual([]);
    expect(spanSegments('', null)).toEqual([]);
  });

  it('converts UTF-8 byte lengths to UTF-16 slices', () => {
    // 'é' = 2 bytes, '€' = 3 bytes, '𝄞' = 4 bytes (2 UTF-16 units).
    const text = 'é€𝄞x';
    const segments = spanSegments(text, line([2, 1, 3, 2, 4, 4, 1, 0]));
    expect(segments).toEqual([
      { text: 'é', className: 'syntax-keyword' },
      { text: '€', className: 'syntax-string' },
      { text: '𝄞', className: 'syntax-comment' },
      { text: 'x', className: '' },
    ]);
    expect(segments.map((s) => s.text).join('')).toBe(text);
  });

  it('emits a trailing plain segment when runs undercover the text', () => {
    const text = 'abcdef';
    const segments = spanSegments(text, line([2, 1]));
    expect(segments).toEqual([
      { text: 'ab', className: 'syntax-keyword' },
      { text: 'cdef', className: '' },
    ]);
  });

  it('clamps runs that overcover the text without emitting empty segments', () => {
    const text = 'ab';
    const segments = spanSegments(text, line([5, 1, 3, 2]));
    expect(segments).toEqual([{ text: 'ab', className: 'syntax-keyword' }]);
    expect(segments.map((s) => s.text).join('')).toBe(text);
  });

  it('maps unknown class ids to plain', () => {
    const segments = spanSegments('xy', line([2, 99]));
    expect(segments).toEqual([{ text: 'xy', className: '' }]);
  });
});
