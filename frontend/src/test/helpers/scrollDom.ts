// Shared DOM helpers for scroll-related tests. Promoted out of inline
// duplicates in MessageTimeline.test.ts and ChatView.test.ts so the
// scroll-test surface gets a single, shared implementation.

/**
 * Build a complete DOMRect from a partial spec, defaulting all unspecified
 * dimensions to 0. Useful for `setElementRect` overrides.
 */
export function rect(partial: Partial<DOMRect>): DOMRect {
  return {
    bottom: 0,
    height: 0,
    left: 0,
    right: 0,
    top: 0,
    width: 0,
    x: 0,
    y: 0,
    toJSON: () => ({}),
    ...partial,
  };
}

/**
 * Override `getBoundingClientRect` on a single element. Multiple calls on
 * the same element replace the prior override.
 */
export function setElementRect(el: Element, partial: Partial<DOMRect>): void {
  Object.defineProperty(el, 'getBoundingClientRect', {
    configurable: true,
    value: () => rect(partial),
  });
}

/**
 * Install scrollHeight / clientHeight getters and an aligned bounding rect
 * on the given scroll container element. Pass closures (not literal numbers)
 * so the test can mutate the underlying state — the getter re-reads on
 * each access.
 */
export function setScrollGeometry(
  el: HTMLElement,
  geometry: {
    scrollHeight: () => number;
    clientHeight: () => number;
    top?: number;
  },
): void {
  Object.defineProperty(el, 'scrollHeight', {
    configurable: true,
    get: geometry.scrollHeight,
  });
  Object.defineProperty(el, 'clientHeight', {
    configurable: true,
    get: geometry.clientHeight,
  });
  setElementRect(el, {
    top: geometry.top ?? 0,
    bottom: (geometry.top ?? 0) + geometry.clientHeight(),
    height: geometry.clientHeight(),
  });
}

/** Await one animation frame. */
export async function nextFrame(): Promise<void> {
  await new Promise((resolve) => requestAnimationFrame(resolve));
}

/**
 * Replace `globalThis.ResizeObserver` with a stub that records every
 * registered callback. `trigger()` invokes them all with empty entries.
 * `restore()` puts the original (or stub from setup) back.
 *
 * Use cases: simulate row-resize callbacks landing without driving real
 * layout, or assert that a component installs the observer at all.
 */
export function installControllableResizeObserver(): {
  trigger: () => void;
  restore: () => void;
} {
  const previous = globalThis.ResizeObserver;
  const callbacks: ResizeObserverCallback[] = [];

  class StubResizeObserver {
    constructor(callback: ResizeObserverCallback) {
      callbacks.push(callback);
    }
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  }

  globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;

  return {
    trigger(): void {
      for (const callback of callbacks) {
        callback([], {} as ResizeObserver);
      }
    },
    restore(): void {
      globalThis.ResizeObserver = previous;
    },
  };
}
