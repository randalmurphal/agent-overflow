import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { observeScrollSurfaceContentWidth } from './scrollSurfaceWidth';

class FireableResizeObserver {
  static instances: FireableResizeObserver[] = [];

  readonly observed = new Set<Element>();

  constructor(private readonly callback: ResizeObserverCallback) {
    FireableResizeObserver.instances.push(this);
  }

  observe(target: Element): void {
    this.observed.add(target);
  }

  unobserve(target: Element): void {
    this.observed.delete(target);
  }

  disconnect(): void {
    this.observed.clear();
  }

  trigger(target: Element, height: number, width: number): void {
    if (!this.observed.has(target)) {
      throw new Error('target is not observed');
    }
    this.callback([
      {
        target,
        contentRect: { width, height },
      } as ResizeObserverEntry,
    ], this as unknown as ResizeObserver);
  }
}

function observer(): FireableResizeObserver {
  const instance = FireableResizeObserver.instances.at(-1);
  if (!instance) throw new Error('ResizeObserver was not created');
  return instance;
}

describe('observeScrollSurfaceContentWidth', () => {
  let originalResizeObserver: typeof ResizeObserver | undefined;

  beforeEach(() => {
    originalResizeObserver = globalThis.ResizeObserver;
    FireableResizeObserver.instances = [];
    globalThis.ResizeObserver = FireableResizeObserver as unknown as typeof ResizeObserver;
  });

  afterEach(() => {
    document.body.innerHTML = '';
    if (originalResizeObserver) {
      globalThis.ResizeObserver = originalResizeObserver;
    } else {
      Reflect.deleteProperty(globalThis, 'ResizeObserver');
    }
    FireableResizeObserver.instances = [];
  });

  it('reports the content-box width and never makes a synchronous layout read', () => {
    const surface = document.createElement('div');
    document.body.append(surface);
    // Border-box (getBoundingClientRect) deliberately disagrees with the
    // content-box (ResizeObserver) width by the scrollbar gutter. If the
    // observer ever consulted a layout query, the width would flip between
    // the two sources and oscillate — the feedback loop this guards against
    // (idle width-oscillation incident 2026-06-26, commit a5a5d032).
    const getRect = vi.fn(() => ({ width: 815 }) as DOMRect);
    Object.defineProperty(surface, 'getBoundingClientRect', {
      configurable: true,
      value: getRect,
    });

    const widths: number[] = [];
    const stop = observeScrollSurfaceContentWidth(surface, (width) => widths.push(width));

    // Nothing is reported synchronously, and no layout query is made.
    expect(widths).toEqual([]);
    expect(getRect).not.toHaveBeenCalled();

    observer().trigger(surface, 600, 800);

    expect(widths).toEqual([800]);
    expect(getRect).not.toHaveBeenCalled();

    stop();
  });

  it('observes the surface once and disconnects on cleanup', () => {
    const surface = document.createElement('div');
    const stop = observeScrollSurfaceContentWidth(surface, () => {});

    const ro = observer();
    expect(FireableResizeObserver.instances).toHaveLength(1); // exactly one observer created
    expect(ro.observed.has(surface)).toBe(true);

    stop();
    expect(ro.observed.size).toBe(0);
  });

  it('rounds and clamps the measured width', () => {
    const surface = document.createElement('div');
    const widths: number[] = [];
    const stop = observeScrollSurfaceContentWidth(surface, (width) => widths.push(width));

    observer().trigger(surface, 100, 784.6);
    observer().trigger(surface, 100, -5);
    observer().trigger(surface, 100, Number.NaN); // non-finite is ignored, not pushed

    expect(widths).toEqual([785, 0]);

    stop();
  });

  it('returns a no-op cleanup when ResizeObserver is unavailable', () => {
    const saved = globalThis.ResizeObserver;
    Reflect.deleteProperty(globalThis, 'ResizeObserver');
    try {
      const widths: number[] = [];
      const before = FireableResizeObserver.instances.length;
      const stop = observeScrollSurfaceContentWidth(
        document.createElement('div'),
        (width) => widths.push(width),
      );
      stop();

      expect(FireableResizeObserver.instances.length).toBe(before);
      expect(widths).toEqual([]);
    } finally {
      globalThis.ResizeObserver = saved;
    }
  });
});
