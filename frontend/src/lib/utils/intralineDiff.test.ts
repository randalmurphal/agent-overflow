import { describe, expect, it } from 'vitest';
import {
  INTRALINE_MAX_LINE_LENGTH,
  intralineRanges,
  segmentLine,
} from './intralineDiff';
import type { SpanSegment } from './syntaxSpans';

describe('intralineRanges', () => {
  it('finds the changed middle and snaps it to word bounds', () => {
    const ranges = intralineRanges(
      'const value = computeBar(input);',
      'const value = computeBaz(input);',
    );
    expect(ranges).not.toBeNull();
    // Char-level trim leaves only the differing tail chars; the word
    // snap widens both sides to the full identifier.
    const del = ranges!.del;
    expect('const value = computeBar(input);'.slice(del.start, del.end)).toBe('computeBar');
    const add = ranges!.add;
    expect('const value = computeBaz(input);'.slice(add.start, add.end)).toBe('computeBaz');
  });

  it('emits an empty range on the shorter side of a pure insertion', () => {
    const ranges = intralineRanges('list(a, b)', 'list(a, b, c)');
    expect(ranges).not.toBeNull();
    expect(ranges!.del.end - ranges!.del.start).toBe(0);
    expect('list(a, b, c)'.slice(ranges!.add.start, ranges!.add.end)).toBe(', c');
  });

  it('returns null when the lines are mostly different', () => {
    expect(intralineRanges('return fetchUsers(db)', 'const x = 12')).toBeNull();
  });

  it('returns null for identical, empty, and over-length inputs', () => {
    expect(intralineRanges('same', 'same')).toBeNull();
    expect(intralineRanges('', '')).toBeNull();
    const long = 'x'.repeat(INTRALINE_MAX_LINE_LENGTH + 1);
    expect(intralineRanges(long, `${long}y`)).toBeNull();
  });

  it('never overlaps prefix and suffix on repeated content', () => {
    // "aaa" -> "aa" inside a call: naive suffix matching would overlap
    // the prefix; the word snap then widens to the whole identifier.
    const ranges = intralineRanges('foo(aaa)', 'foo(aa)');
    expect(ranges).not.toBeNull();
    expect('foo(aaa)'.slice(ranges!.del.start, ranges!.del.end)).toBe('aaa');
    // Pure char removal: the add side has no changed slice to widen.
    expect(ranges!.add.end - ranges!.add.start).toBe(0);
  });

  it('rejects a single-word rewrite (word snap covers the whole line)', () => {
    expect(intralineRanges('aaa', 'aa')).toBeNull();
  });
});

describe('segmentLine', () => {
  const spans: SpanSegment[] = [
    { text: 'const ', className: 'syntax-keyword' },
    { text: 'value', className: '' },
    { text: ' = 1;', className: 'syntax-number' },
  ];
  const text = 'const value = 1;';

  it('splits span segments at range boundaries and keeps classes', () => {
    // Range covers "value = " — spans segment 2 fully and segment 3 partly.
    const segments = segmentLine(spans, text, { start: 6, end: 14 });
    expect(segments).toEqual([
      { text: 'const ', className: 'syntax-keyword', emph: false },
      { text: 'value', className: '', emph: true },
      { text: ' = ', className: 'syntax-number', emph: true },
      { text: '1;', className: 'syntax-number', emph: false },
    ]);
    expect(segments.map((segment) => segment.text).join('')).toBe(text);
  });

  it('falls back to plain-text slicing when spans have not landed', () => {
    const segments = segmentLine(null, text, { start: 6, end: 11 });
    expect(segments).toEqual([
      { text: 'const ', className: '', emph: false },
      { text: 'value', className: '', emph: true },
      { text: ' = 1;', className: '', emph: false },
    ]);
  });

  it('handles a range covering the whole line', () => {
    const segments = segmentLine(spans, text, { start: 0, end: text.length });
    expect(segments.every((segment) => segment.emph)).toBe(true);
    expect(segments.map((segment) => segment.text).join('')).toBe(text);
  });
});
