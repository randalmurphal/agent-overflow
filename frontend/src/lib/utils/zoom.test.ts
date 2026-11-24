import { describe, expect, it, vi, beforeEach } from 'vitest';
import { applyFontScale } from './zoom';

function getRootFontSize(): string {
  return document.documentElement.style.getPropertyValue('font-size');
}

describe('applyFontScale', () => {
  beforeEach(() => {
    document.documentElement.style.removeProperty('font-size');
    // Reset module-level cache
    applyFontScale(999);
    document.documentElement.style.removeProperty('font-size');
  });

  it.each([
    ['larger', 16, /^19\.69\d*px$/],
    ['smaller', 10, /^12\.30\d*px$/],
  ])('sets root font-size for a %s value', (_size, fontSize, expected) => {
    applyFontScale(fontSize);
    expect(getRootFontSize()).toMatch(expected);
  });

  it('skips redundant DOM writes for the same value', () => {
    applyFontScale(16);
    const spy = vi.spyOn(document.documentElement.style, 'setProperty');
    applyFontScale(16);
    expect(spy).not.toHaveBeenCalled();
    spy.mockRestore();
  });

  it('removes the property when switching back to default', () => {
    applyFontScale(16);
    expect(getRootFontSize()).not.toBe('');
    applyFontScale(13);
    expect(getRootFontSize()).toBe('');
  });
});
