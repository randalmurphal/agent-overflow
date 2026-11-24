import { describe, expect, it } from 'vitest';

import { measureComposerToolbarDensity, type ComposerToolbarDensity } from './composerToolbarDensity';

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
  // `widths` is the scrollWidth each rung reports; `from` is the attribute
  // before the read, which the measurer must leave as it found it.
  it.each<{
    name: string;
    clientWidth: number;
    widths: Record<ComposerToolbarDensity, number>;
    from: ComposerToolbarDensity;
    expected: ComposerToolbarDensity;
  }>([
    { name: 'keeps full mode when full toolbar contents fit', clientWidth: 640, widths: { full: 600, compact: 600, minimal: 600, tight: 600 }, from: 'compact', expected: 'full' },
    { name: 'switches to compact when full contents overflow but icons fit', clientWidth: 520, widths: { full: 600, compact: 480, minimal: 480, tight: 480 }, from: 'full', expected: 'compact' },
    { name: 'switches to minimal when even the compact rung overflows', clientWidth: 360, widths: { full: 600, compact: 430, minimal: 300, tight: 300 }, from: 'compact', expected: 'minimal' },
    { name: 'switches to tight when the minimal rung\'s model floor overflows', clientWidth: 269, widths: { full: 600, compact: 430, minimal: 300, tight: 269 }, from: 'minimal', expected: 'tight' },
    { name: 'preserves the current mode when the toolbar has no measurable width', clientWidth: 0, widths: { full: 600, compact: 600, minimal: 600, tight: 600 }, from: 'minimal', expected: 'minimal' },
    { name: 'expands again as soon as the full content fits the width', clientWidth: 640, widths: { full: 620, compact: 320, minimal: 320, tight: 320 }, from: 'minimal', expected: 'full' },
  ])('$name', ({ clientWidth, widths, from, expected }) => {
    const el = elementWithWidths(clientWidth, (density) => widths[density as ComposerToolbarDensity]);
    el.dataset.density = from;

    expect(measureComposerToolbarDensity(el)).toBe(expected);
    expect(el.dataset.density).toBe(from);
  });

  it('leaves the attribute unset when it began unset', () => {
    const el = elementWithWidths(640, () => 600);

    expect(measureComposerToolbarDensity(el)).toBe('full');
    expect('density' in el.dataset).toBe(false);
  });
});
