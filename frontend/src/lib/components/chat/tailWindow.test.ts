import { describe, expect, it } from 'vitest';
import { isMonotonicAppend, measuredLineStartOffset, newlineCutOffset } from './tailWindow';

// measuredLineStartOffset's cutting behavior needs a real layout engine
// (happy-dom reports zero-height rects, which the helper treats as "no
// geometry" and declines to cut) — the wrap-stability of its cuts is
// covered in tailClampedText.browser.test.ts. Its bail-out paths are
// engine-independent and covered here.

describe('newlineCutOffset', () => {
  const para = (n: number, fill = 'a'): string => fill.repeat(n);

  it.each<[string, string, number, number | null]>([
    ['returns null when the text has no newline', para(10_000), 0, null],
    // One newline, 100 chars from the end — cutting there would keep
    // fewer than minKeep characters.
    ['returns null when every newline sits inside the protected tail', para(5000) + '\n' + para(100), 0, null],
    // len = 7002, minKeep 2048 → limit 4954. The newline at 6001 keeps
    // only 1000; the one at 3000 qualifies.
    ['cuts just after the LAST newline that keeps at least minKeep', para(3000) + '\n' + para(3000) + '\n' + para(1000), 0, 3001],
    ['keeps exactly minKeep when a newline lands on the boundary', para(1000) + '\n' + para(2048), 0, 1001],
    // Already cut at 3001; the only later newline (6001) is inside the
    // protected tail, so there is nowhere to advance to.
    ['never returns a cut at or before the current cutOffset', para(3000) + '\n' + para(3000) + '\n' + para(1000), 3001, null],
    ['advances an existing cut when a later newline qualifies', para(3000) + '\n' + para(3000) + '\n' + para(3000), 3001, 6002],
    ['returns null when the text is shorter than minKeep', 'a\nb', 0, null],
  ])('%s', (_name, text, cutOffset, expected) => {
    expect(newlineCutOffset(text, cutOffset, 2048)).toBe(expected);
  });

  it('keeps at least minLines newlines after the cut', () => {
    // Long lines: 2048 characters cover three of them, so the char floor
    // alone cuts after the fifth line (5005) and leaves three lines for a
    // slide that may hold eighteen pending.
    const text = Array.from({ length: 8 }, () => para(1000)).join('\n') + '\n';
    expect(newlineCutOffset(text, 0, 2048)).toBe(5005);
    // Five lines kept: the cut lands after the third line.
    expect(newlineCutOffset(text, 0, 2048, 5)).toBe(3003);
    // More lines than the text holds past the cut: nowhere to cut.
    expect(newlineCutOffset(text, 0, 2048, 9)).toBeNull();
    // Short lines: the char floor is the binding one.
    const short = Array.from({ length: 3000 }, () => 'a').join('\n');
    expect(newlineCutOffset(short, 0, 2048, 21)).toBe(newlineCutOffset(short, 0, 2048));
  });
});

describe('isMonotonicAppend', () => {
  // Texts are windows of one stream: `prev` starts at `prevStart`, `next`
  // at `nextStart`, and `cutOffset` is a stream offset inside `prev`.
  const check = (
    next: string,
    prev: string,
    cutOffset = 0,
    { prevStart = 0, nextStart = 0 } = {},
  ): boolean => isMonotonicAppend(
    next,
    nextStart,
    prevStart + prev.length,
    prev.length > 0 ? prev.charCodeAt(prev.length - 1) : 0,
    cutOffset,
    cutOffset > 0 ? prev.charCodeAt(cutOffset - prevStart) : 0,
  );

  it.each([
    ['accepts a pure append', 'hello world', 'hello', true],
    ['accepts an unchanged string', 'hello', 'hello', true],
    ['accepts the empty→first-content transition', 'hello', '', true],
    ['rejects a shrink (a dropped retained tail swapping to the trimmed summary)', 'short summary', 'a much longer accumulated live tail string', false],
    ['rejects a same-length replacement that changes the final char', 'hellX', 'hello', false],
    // Documented accepted miss — the caller contract (monotonic tail)
    // is the real invariant; the sentinels only catch the transitions
    // that actually occur (see the helper's doc comment).
    ['is a heuristic: a replacement preserving both probed chars passes', 'hXllo', 'hello', true],
  ])('%s', (_name, next, prev, expected) => {
    expect(check(next, prev)).toBe(expected);
  });

  it('rejects a replacement that changes the char at the cut boundary', () => {
    const prev = 'abcdef\nghijkl';
    const next = 'abcdef\nXhijkl-and-more';
    expect(check(next, prev, 7)).toBe(false);
  });

  it('accepts an append whose source dropped text above the cut', () => {
    const stream = 'one\ntwo\nthree\nfour';
    // The clamp cut at 'three'; the source now starts at 'two'.
    expect(check(`${stream.slice(4)} five`, stream, 8, { nextStart: 4 })).toBe(true);
  });

  it('accepts an append whose source moved past the cut', () => {
    const stream = 'one\ntwo\nthree\nfour';
    // The clamp cut at 'two'; the source now starts at 'three'.
    expect(check(`${stream.slice(8)} five`, stream, 4, { nextStart: 8 })).toBe(true);
  });

  it('accepts the whole text replacing a window of it', () => {
    const stream = 'one\ntwo\nthree\nfour';
    expect(check(stream, stream.slice(8), 14, { prevStart: 8 })).toBe(true);
  });

  it('rejects text that starts at or past the previous end', () => {
    const stream = 'one\ntwo\nthree\nfour';
    expect(check(stream.slice(8), stream.slice(0, 8), 0, { nextStart: 8 })).toBe(false);
  });

  it('checks the cut and the previous end at their stream offsets', () => {
    const stream = 'one\ntwo\nthree\nfour';
    const changedEnd = `${stream.slice(4, -1)}X more`;
    expect(check(changedEnd, stream, 8, { nextStart: 4 })).toBe(false);
    const changedCut = `two\nXhree\nfour more`;
    expect(check(changedCut, stream, 8, { nextStart: 4 })).toBe(false);
  });
});

describe('measuredLineStartOffset bail-outs', () => {
  const textNode = (s: string): Text => {
    const node = document.createTextNode(s);
    document.body.appendChild(node);
    return node;
  };

  const REASONING = 'some reasonably long reasoning text';
  it.each([
    ['a node shorter than two characters', 'a', 19.5],
    ['an empty node', '', 19.5],
    ['a NaN line height', REASONING, Number.NaN],
    ['a zero line height', REASONING, 0],
    ['a negative line height', REASONING, -5],
    // happy-dom reports zero rects for everything — the helper must
    // treat that as "cannot measure", never fabricate a cut from it.
    ['no layout geometry (zero-height rects)', 'x'.repeat(10_000), 19.5],
  ])('returns null for %s', (_name, text, lineHeight) => {
    expect(measuredLineStartOffset(textNode(text), 12, lineHeight)).toBeNull();
  });
});
