import { describe, expect, it, vi } from 'vitest';

import { measureDensity, watchDensity } from './densityLadder';

/**
 * jsdom does no layout, so scrollWidth is scripted per rung: the getter
 * reads the attribute the measurer sets, the same coupling the real CSS
 * provides (a denser rung hides content and shrinks scrollWidth).
 */
function elementWithWidths(
  clientWidth: number,
  scrollWidthFor: (density: string | undefined) => number,
): HTMLElement {
  const el = document.createElement('div');
  Object.defineProperty(el, 'clientWidth', { configurable: true, get: () => clientWidth });
  Object.defineProperty(el, 'scrollWidth', {
    configurable: true,
    get: () => scrollWidthFor(el.dataset.density),
  });
  return el;
}

const RUNGS = ['roomy', 'mid', 'dense'] as const;

describe('measureDensity', () => {
  it('picks the roomiest rung that fits', () => {
    const el = elementWithWidths(400, (d) => (d === 'roomy' ? 600 : d === 'mid' ? 390 : 200));
    expect(measureDensity(el, RUNGS)).toBe('mid');
  });

  it('falls to the densest rung when nothing fits, and restores the attribute', () => {
    const el = elementWithWidths(100, () => 600);
    el.dataset.density = 'mid';
    expect(measureDensity(el, RUNGS)).toBe('dense');
    expect(el.dataset.density).toBe('mid');
  });

  it('keeps a known rung when the element has no width, else the roomiest', () => {
    const known = elementWithWidths(0, () => 600);
    known.dataset.density = 'dense';
    expect(measureDensity(known, RUNGS)).toBe('dense');
    const unknown = elementWithWidths(0, () => 600);
    unknown.dataset.density = 'stale';
    expect(measureDensity(unknown, RUNGS)).toBe('roomy');
  });
});

describe('watchDensity', () => {
  it('measures once on attach and again when a child mounts, then disposes cleanly', async () => {
    const el = document.createElement('div');
    document.body.append(el);
    const measure = vi.fn();
    const stop = watchDensity(el, measure);
    await new Promise((resolve) => requestAnimationFrame(() => resolve(undefined)));
    expect(measure).toHaveBeenCalledTimes(1);

    el.append(document.createElement('span'));
    await new Promise((resolve) => setTimeout(resolve, 0));
    await new Promise((resolve) => requestAnimationFrame(() => resolve(undefined)));
    expect(measure).toHaveBeenCalledTimes(2);

    stop();
    el.append(document.createElement('span'));
    await new Promise((resolve) => setTimeout(resolve, 0));
    await new Promise((resolve) => requestAnimationFrame(() => resolve(undefined)));
    expect(measure).toHaveBeenCalledTimes(2);
    el.remove();
  });
});
