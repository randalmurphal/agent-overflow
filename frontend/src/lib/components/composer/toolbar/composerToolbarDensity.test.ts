import { describe, expect, it } from 'vitest';

import { measureComposerToolbarCompact } from './composerToolbarDensity';

function elementWithWidths(clientWidth: number, scrollWidth: number): HTMLElement {
  const el = document.createElement('div');
  Object.defineProperty(el, 'clientWidth', {
    configurable: true,
    get: () => clientWidth,
  });
  Object.defineProperty(el, 'scrollWidth', {
    configurable: true,
    get: () => scrollWidth,
  });
  return el;
}

describe('measureComposerToolbarCompact', () => {
  it('keeps full mode when full toolbar contents fit', () => {
    const el = elementWithWidths(640, 600);
    el.dataset.compact = 'true';

    expect(measureComposerToolbarCompact(el)).toBe(false);
    expect(el.dataset.compact).toBe('true');
  });

  it('switches to compact mode when full toolbar contents overflow', () => {
    const el = elementWithWidths(520, 600);
    el.dataset.compact = 'false';

    expect(measureComposerToolbarCompact(el)).toBe(true);
    expect(el.dataset.compact).toBe('false');
  });

  it('preserves the current mode when the toolbar has no measurable width', () => {
    const el = elementWithWidths(0, 600);
    el.dataset.compact = 'true';

    expect(measureComposerToolbarCompact(el)).toBe(true);
    expect(el.dataset.compact).toBe('true');
  });

  it('measures full-label width while the toolbar is currently compact', () => {
    const el = document.createElement('div');
    el.dataset.compact = 'true';
    Object.defineProperty(el, 'clientWidth', {
      configurable: true,
      get: () => 520,
    });
    Object.defineProperty(el, 'scrollWidth', {
      configurable: true,
      get: () => (el.dataset.compact === 'true' ? 320 : 640),
    });

    expect(measureComposerToolbarCompact(el)).toBe(true);
    expect(el.dataset.compact).toBe('true');
  });
});
