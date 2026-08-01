// Shared happy-dom geometry scaffolding for the scroll controller test
// suites (index.svelte.test.ts choreography, scrollInterleavings.test.ts
// invariants). happy-dom doesn't measure layout, so tests stub
// scrollHeight / clientHeight / scrollTop on the scroll element via
// Object.defineProperty and mutate the underlying numbers to simulate
// content growth, composer height changes, viewport resizes, and native
// browser clamps. Not a vitest file — no test hooks; the suites own
// their own clocks and lifecycles.

export interface Geometry {
  scrollHeight: number;
  clientHeight: number;
  scrollTop: number;
  contentHeight: number;
}

export interface StubGeometryOptions {
  setScrollTop?: (value: number, geom: Geometry) => void;
}

export function stubGeometry(
  scrollEl: HTMLElement,
  contentEl: HTMLElement,
  geom: Geometry,
  options: StubGeometryOptions = {},
): void {
  Object.defineProperty(scrollEl, 'scrollHeight', { configurable: true, get: () => geom.scrollHeight });
  Object.defineProperty(scrollEl, 'clientHeight', { configurable: true, get: () => geom.clientHeight });
  Object.defineProperty(scrollEl, 'scrollTop', {
    configurable: true,
    get: () => geom.scrollTop,
    set: (v: number) => {
      if (options.setScrollTop) {
        options.setScrollTop(v, geom);
        return;
      }
      geom.scrollTop = Math.max(0, Math.min(v, geom.scrollHeight - geom.clientHeight));
    },
  });
  Object.defineProperty(contentEl, 'scrollHeight', { configurable: true, get: () => geom.contentHeight });
}

export class MockResizeObserver {
  static instances: MockResizeObserver[] = [];
  callback: ResizeObserverCallback;
  observed: Element[] = [];
  constructor(cb: ResizeObserverCallback) {
    this.callback = cb;
    MockResizeObserver.instances.push(this);
  }
  observe(el: Element): void {
    this.observed.push(el);
  }
  unobserve(): void {}
  disconnect(): void {
    this.observed = [];
  }
  /** Fire the callback synchronously with a single entry for the given element, height, and optional width. */
  fire(el: Element, height: number, width = 0): void {
    this.callback(
      [
        {
          target: el,
          contentRect: { height, width, top: 0, left: 0, right: width, bottom: height, x: 0, y: 0, toJSON: () => ({}) } as DOMRectReadOnly,
          borderBoxSize: [],
          contentBoxSize: [],
          devicePixelContentBoxSize: [],
        } as ResizeObserverEntry,
      ],
      this as unknown as ResizeObserver,
    );
  }
}
