import { describe, expect, it } from 'vitest';

import { measureComposerToolbarDensity } from './composerToolbarDensity';

/**
 * jsdom does no layout, so scrollWidth is scripted per density rung: the
 * getter reads the attribute the measurer sets, the same coupling the
 * real CSS provides (a denser rung hides content and shrinks scrollWidth).
 */
function elementWithWidths(
  clientWidth: number,
  scrollWidthFor: (density: string | undefined) => number,
): HTMLElement {
  const el = document.createElement('div');
  Object.defineProperty(el, 'clientWidth', {
    configurable: true,
    get: () => clientWidth,
  });
  Object.defineProperty(el, 'scrollWidth', {
    configurable: true,
    get: () => scrollWidthFor(el.dataset.density),
  });
  return el;
}

describe('measureComposerToolbarDensity', () => {
  it('keeps full mode when full toolbar contents fit', () => {
    const el = elementWithWidths(640, () => 600);
    el.dataset.density = 'compact';

    expect(measureComposerToolbarDensity(el)).toBe('full');
    expect(el.dataset.density).toBe('compact');
  });

  it('switches to compact when full contents overflow but icons fit', () => {
    const el = elementWithWidths(520, (density) => (density === 'full' ? 600 : 480));
    el.dataset.density = 'full';

    expect(measureComposerToolbarDensity(el)).toBe('compact');
    expect(el.dataset.density).toBe('full');
  });

  it('switches to minimal when even the compact rung overflows', () => {
    const el = elementWithWidths(360, (density) => (density === 'full' ? 600 : 430));
    el.dataset.density = 'compact';

    expect(measureComposerToolbarDensity(el)).toBe('minimal');
    expect(el.dataset.density).toBe('compact');
  });

  it('preserves the current mode when the toolbar has no measurable width', () => {
    const el = elementWithWidths(0, () => 600);
    el.dataset.density = 'minimal';

    expect(measureComposerToolbarDensity(el)).toBe('minimal');
    expect(el.dataset.density).toBe('minimal');
  });

  it('expands again as soon as the full content fits the width', () => {
    const el = elementWithWidths(640, (density) => (density === 'full' ? 620 : 320));
    el.dataset.density = 'minimal';

    expect(measureComposerToolbarDensity(el)).toBe('full');
    expect(el.dataset.density).toBe('minimal');
  });

  it('leaves the attribute unset when it began unset', () => {
    const el = elementWithWidths(640, () => 600);

    expect(measureComposerToolbarDensity(el)).toBe('full');
    expect('density' in el.dataset).toBe(false);
  });
});
