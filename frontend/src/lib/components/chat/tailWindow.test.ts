import { describe, expect, it } from 'vitest';
import { isMonotonicAppend, measuredLineStartOffset, newlineCutOffset } from './tailWindow';

// measuredLineStartOffset's cutting behavior needs a real layout engine
// (happy-dom reports zero-height rects, which the helper treats as "no
// geometry" and declines to cut) — the wrap-stability of its cuts is
// covered in tailClampedText.browser.test.ts. Its bail-out paths are
// engine-independent and covered here.

describe('newlineCutOffset', () => {
  const para = (n: number, fill = 'a'): string => fill.repeat(n);

  it('returns null when the text has no newline', () => {
    expect(newlineCutOffset(para(10_000), 0, 2048)).toBeNull();
  });

  it('returns null when every newline sits inside the protected tail', () => {
    // One newline, 100 chars from the end — cutting there would keep
    // fewer than minKeep characters.
    const text = para(5000) + '\n' + para(100);
    expect(newlineCutOffset(text, 0, 2048)).toBeNull();
  });

  it('cuts just after the LAST newline that keeps at least minKeep', () => {
    const text = para(3000) + '\n' + para(3000) + '\n' + para(1000);
    // len = 7002, minKeep 2048 → limit 4954. The newline at 6001 keeps
    // only 1000; the one at 3000 qualifies.
    expect(newlineCutOffset(text, 0, 2048)).toBe(3001);
  });

  it('keeps exactly minKeep when a newline lands on the boundary', () => {
    const text = para(1000) + '\n' + para(2048);
    expect(newlineCutOffset(text, 0, 2048)).toBe(1001);
  });

  it('never returns a cut at or before the current cutOffset', () => {
    const text = para(3000) + '\n' + para(3000) + '\n' + para(1000);
    // Already cut at 3001; the only later newline (6001) is inside the
    // protected tail, so there is nowhere to advance to.
    expect(newlineCutOffset(text, 3001, 2048)).toBeNull();
  });

  it('advances an existing cut when a later newline qualifies', () => {
    const text = para(3000) + '\n' + para(3000) + '\n' + para(3000);
    expect(newlineCutOffset(text, 3001, 2048)).toBe(6002);
  });

  it('returns null when the text is shorter than minKeep', () => {
    expect(newlineCutOffset('a\nb', 0, 2048)).toBeNull();
  });
});

describe('isMonotonicAppend', () => {
  const sentinels = (text: string, cutOffset = 0) => ({
    prevLen: text.length,
    prevLastCharCode: text.length > 0 ? text.charCodeAt(text.length - 1) : 0,
    cutOffset,
    cutFirstCharCode: cutOffset > 0 ? text.charCodeAt(cutOffset) : 0,
  });

  const check = (next: string, prev: string, cutOffset = 0): boolean => {
    const s = sentinels(prev, cutOffset);
    return isMonotonicAppend(next, s.prevLen, s.prevLastCharCode, s.cutOffset, s.cutFirstCharCode);
  };

  it('accepts a pure append', () => {
    expect(check('hello world', 'hello')).toBe(true);
  });

  it('accepts an unchanged string', () => {
    expect(check('hello', 'hello')).toBe(true);
  });

  it('accepts the empty→first-content transition', () => {
    expect(check('hello', '')).toBe(true);
  });

  it('rejects a shrink (a dropped retained tail swapping to the trimmed summary)', () => {
    expect(check('short summary', 'a much longer accumulated live tail string')).toBe(false);
  });

  it('rejects a same-length replacement that changes the final char', () => {
    expect(check('hellX', 'hello')).toBe(false);
  });

  it('rejects a replacement that changes the char at the cut boundary', () => {
    const prev = 'abcdef\nghijkl';
    const next = 'abcdef\nXhijkl-and-more';
    expect(check(next, prev, 7)).toBe(false);
  });

  it('is a heuristic: a replacement preserving both probed chars passes', () => {
    // Documented accepted miss — the caller contract (monotonic tail)
    // is the real invariant; the sentinels only catch the transitions
    // that actually occur (see the helper's doc comment).
    expect(check('hXllo', 'hello')).toBe(true);
  });
});

describe('measuredLineStartOffset bail-outs', () => {
  const textNode = (s: string): Text => {
    const node = document.createTextNode(s);
    document.body.appendChild(node);
    return node;
  };

  it('returns null for nodes shorter than two characters', () => {
    expect(measuredLineStartOffset(textNode('a'), 12, 19.5)).toBeNull();
    expect(measuredLineStartOffset(textNode(''), 12, 19.5)).toBeNull();
  });

  it('returns null for a non-finite or non-positive line height', () => {
    const node = textNode('some reasonably long reasoning text');
    expect(measuredLineStartOffset(node, 12, Number.NaN)).toBeNull();
    expect(measuredLineStartOffset(node, 12, 0)).toBeNull();
    expect(measuredLineStartOffset(node, 12, -5)).toBeNull();
  });

  it('returns null without layout geometry (zero-height rects)', () => {
    // happy-dom reports zero rects for everything — the helper must
    // treat that as "cannot measure", never fabricate a cut from it.
    expect(measuredLineStartOffset(textNode('x'.repeat(10_000)), 12, 19.5)).toBeNull();
  });
});
