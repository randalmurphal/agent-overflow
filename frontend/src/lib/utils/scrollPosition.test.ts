import { describe, expect, it } from 'vitest';
import { isScrollPinnedToBottom } from './scrollPosition';

describe('isScrollPinnedToBottom', () => {
  it('only treats the exact bottom as pinned', () => {
    expect(isScrollPinnedToBottom(400, 1000, 600)).toBe(true);
    expect(isScrollPinnedToBottom(399.25, 1000, 600)).toBe(true);
    expect(isScrollPinnedToBottom(398, 1000, 600)).toBe(false);
  });

  it('treats non-overflowing content as pinned', () => {
    expect(isScrollPinnedToBottom(0, 500, 600)).toBe(true);
  });
});
