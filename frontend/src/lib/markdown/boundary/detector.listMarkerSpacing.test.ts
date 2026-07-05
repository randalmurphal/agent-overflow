import { describe, expect, it } from 'vitest';
import { isListItemStart } from './detector';
import { splitAtBoundary } from './split';

// Regression: a line that merely STARTS with a list-marker character but
// has no following whitespace (`**bold**`, `*emphasis*`, `-dash`, `1.5x`)
// must NOT be treated as a list item. Misdetecting it opens a phantom
// list that never commits a stable boundary — assistant prose leads
// almost every section with `**…**`, so the streaming committed prefix
// stayed empty for the whole message (nothing revealed until completion
// with streaming disabled; full-message re-parse every tick with it on).
describe('isListItemStart — marker must be followed by whitespace', () => {
  it('rejects emphasis / literal markers with no trailing space', () => {
    expect(isListItemStart('**On the ports:** background app')).toBeNull();
    expect(isListItemStart('*emphasis* then prose')).toBeNull();
    expect(isListItemStart('-dash-joined-word')).toBeNull();
    expect(isListItemStart('+plus')).toBeNull();
    // Ordered-marker false positive: "1.5x" is a number, not "1." + item.
    expect(isListItemStart('1.5x faster than before')).toBeNull();
    // A bare marker at end of line is not a list item either.
    expect(isListItemStart('*')).toBeNull();
    expect(isListItemStart('-')).toBeNull();
  });

  it('still accepts real list items (marker + whitespace)', () => {
    expect(isListItemStart('- item')).toEqual({ ordered: false, indent: 0 });
    expect(isListItemStart('* item')).toEqual({ ordered: false, indent: 0 });
    expect(isListItemStart('+ item')).toEqual({ ordered: false, indent: 0 });
    expect(isListItemStart('1. item')).toEqual({ ordered: true, indent: 0 });
    expect(isListItemStart('2) item')).toEqual({ ordered: true, indent: 0 });
    // Indented continuation / empty item: marker followed by whitespace.
    expect(isListItemStart('    - nested')).toEqual({ ordered: false, indent: 4 });
    expect(isListItemStart('- ')).toEqual({ ordered: false, indent: 0 });
  });
});

describe('splitAtBoundary — bold-led paragraphs commit boundaries', () => {
  it('commits earlier sections of a `**header**`-led message', () => {
    const text =
      '**On the ports:** first section body.\n\n' +
      '**On the fans:** second section body.\n\n' +
      '**On the fps:** third section still streaming';
    const split = splitAtBoundary(text);
    // The first two sections are committed; the trailing (unterminated)
    // one stays in the volatile tail as one block of lookahead.
    expect(split.prefix).toBe(
      '**On the ports:** first section body.\n\n' +
        '**On the fans:** second section body.\n\n',
    );
    expect(split.tail).toBe('**On the fps:** third section still streaming');
    expect(split.prefix + split.tail).toBe(text);
  });
});
