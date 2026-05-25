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

  it('removes root font-size for the default (13)', () => {
    applyFontScale(13);
    expect(getRootFontSize()).toBe('');
  });

  it('sets root font-size for a larger value', () => {
    applyFontScale(16);
    const value = getRootFontSize();
    expect(value).toMatch(/^19\.69\d*px$/);
  });

  it('sets root font-size for a smaller value', () => {
    applyFontScale(10);
    const value = getRootFontSize();
    expect(value).toMatch(/^12\.30\d*px$/);
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
