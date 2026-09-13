import { afterEach, describe, expect, it } from 'vitest';
import { viewportSize } from './viewportSize';

const originalVisual = Object.getOwnPropertyDescriptor(window, 'visualViewport');

function setVisualViewport(value: { width: number; height: number } | undefined): void {
  Object.defineProperty(window, 'visualViewport', { value, configurable: true });
}

afterEach(() => {
  if (originalVisual) Object.defineProperty(window, 'visualViewport', originalVisual);
  else delete (window as { visualViewport?: unknown }).visualViewport;
});

describe('viewportSize', () => {
  it('reads the visual viewport when the browser reports one', () => {
    setVisualViewport({ width: 390, height: 420 });
    expect(viewportSize()).toEqual({ width: 390, height: 420 });
  });

  it('falls back to the layout viewport when there is no visual viewport', () => {
    setVisualViewport(undefined);
    expect(viewportSize()).toEqual({ width: window.innerWidth, height: window.innerHeight });
  });
});
