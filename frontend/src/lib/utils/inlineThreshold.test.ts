import { describe, expect, it } from 'vitest';
import { MAX_INLINE_BYTES, shouldLazyLoad, truncateForPreview } from './inlineThreshold';

describe('inlineThreshold', () => {
  describe('MAX_INLINE_BYTES', () => {
    it('is 2048 so the decision stays aligned with the documented guideline', () => {
      // The constant is referenced from comments/tests that assume this
      // exact value — pin it down so a drive-by change doesn't silently
      // shift the threshold for every renderer at once.
      expect(MAX_INLINE_BYTES).toBe(2048);
    });
  });

  describe('shouldLazyLoad', () => {
    it('returns false for an empty string', () => {
      expect(shouldLazyLoad('')).toBe(false);
    });

    it('returns false when preview length is under the threshold', () => {
      expect(shouldLazyLoad('a'.repeat(100))).toBe(false);
    });

    it('returns false at the exact threshold boundary (inclusive)', () => {
      expect(shouldLazyLoad('a'.repeat(MAX_INLINE_BYTES))).toBe(false);
    });

    it('returns true one character over the threshold', () => {
      expect(shouldLazyLoad('a'.repeat(MAX_INLINE_BYTES + 1))).toBe(true);
    });

    it('returns true for a large preview', () => {
      expect(shouldLazyLoad('x'.repeat(100_000))).toBe(true);
    });
  });

  describe('truncateForPreview', () => {
    it('returns the original text unchanged when it is under the threshold', () => {
      const input = 'short preview';
      expect(truncateForPreview(input)).toBe(input);
    });

    it('returns the original text unchanged at the exact boundary', () => {
      const input = 'a'.repeat(MAX_INLINE_BYTES);
      expect(truncateForPreview(input)).toBe(input);
    });

    it('truncates and appends an ellipsis when text exceeds the threshold', () => {
      const input = 'a'.repeat(MAX_INLINE_BYTES + 100);
      const result = truncateForPreview(input);
      expect(result.length).toBe(MAX_INLINE_BYTES + 1); // +1 for the ellipsis char
      expect(result.endsWith('…')).toBe(true);
      expect(result.startsWith('a'.repeat(MAX_INLINE_BYTES))).toBe(true);
    });

    it('honours a caller-provided max', () => {
      const result = truncateForPreview('abcdefghij', 4);
      expect(result).toBe('abcd…');
    });

    it('does not truncate when caller-provided max is larger than input', () => {
      const result = truncateForPreview('abc', 100);
      expect(result).toBe('abc');
    });
  });
});
