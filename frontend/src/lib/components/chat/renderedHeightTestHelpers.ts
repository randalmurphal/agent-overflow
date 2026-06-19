export const FAKE_OUTER_HEIGHT = 1500;
export const FAKE_INNER_HEIGHT = 1418;
export const FAKE_SOURCE_SWAP_HEIGHT = 1234;
export const FAKE_STALE_RETRY_HEIGHT = 999;
export const FAKE_RENDERED_HEIGHT = FAKE_INNER_HEIGHT;

let restorePerTestAnimationFrame: (() => void) | undefined;
let restorePerTestOffsetHeight: (() => void) | undefined;

function restorePropertyDescriptor(
  target: object,
  property: string,
  descriptor: PropertyDescriptor | undefined,
): void {
  if (descriptor) {
    Object.defineProperty(target, property, descriptor);
    return;
  }
  delete (target as Record<string, unknown>)[property];
}

export function installRenderedHeightGeometryStubs(): () => void {
  const originalOffsetHeight = Object.getOwnPropertyDescriptor(
    HTMLElement.prototype,
    'offsetHeight',
  );
  Object.defineProperty(HTMLElement.prototype, 'offsetHeight', {
    configurable: true,
    get(this: HTMLElement) {
      if (
        this.hasAttribute?.('data-streamdown-block-math') ||
        this.hasAttribute?.('data-streamdown-mermaid')
      ) {
        return FAKE_INNER_HEIGHT;
      }
      if (
        this.classList?.contains('math-host-with-fallback') ||
        this.classList?.contains('mermaid-host-with-fallback')
      ) {
        return FAKE_OUTER_HEIGHT;
      }
      return 0;
    },
  });
  return () => {
    restorePropertyDescriptor(
      HTMLElement.prototype,
      'offsetHeight',
      originalOffsetHeight,
    );
  };
}

export function resetRenderedHeightTestOverrides(): void {
  restorePerTestAnimationFrame?.();
  restorePerTestAnimationFrame = undefined;
  restorePerTestOffsetHeight?.();
  restorePerTestOffsetHeight = undefined;
}

export function overrideOffsetHeight(getter: (this: HTMLElement) => number): void {
  restorePerTestOffsetHeight?.();
  const previous = Object.getOwnPropertyDescriptor(
    HTMLElement.prototype,
    'offsetHeight',
  );
  Object.defineProperty(HTMLElement.prototype, 'offsetHeight', {
    configurable: true,
    get: getter,
  });
  restorePerTestOffsetHeight = () => {
    restorePropertyDescriptor(HTMLElement.prototype, 'offsetHeight', previous);
  };
}

export function overrideAnimationFrameWithTimeout(): void {
  restorePerTestAnimationFrame?.();
  const previousRequest = Object.getOwnPropertyDescriptor(
    globalThis,
    'requestAnimationFrame',
  );
  const previousCancel = Object.getOwnPropertyDescriptor(
    globalThis,
    'cancelAnimationFrame',
  );
  let nextFrameId = 1;
  const timersByFrameId = new Map<number, ReturnType<typeof setTimeout>>();

  Object.defineProperty(globalThis, 'requestAnimationFrame', {
    configurable: true,
    value(callback: FrameRequestCallback): number {
      const frameId = nextFrameId;
      nextFrameId += 1;
      const timer = setTimeout(() => {
        timersByFrameId.delete(frameId);
        callback(performance.now());
      }, 0);
      timersByFrameId.set(frameId, timer);
      return frameId;
    },
  });
  Object.defineProperty(globalThis, 'cancelAnimationFrame', {
    configurable: true,
    value(frameId: number): void {
      const timer = timersByFrameId.get(frameId);
      if (!timer) return;
      clearTimeout(timer);
      timersByFrameId.delete(frameId);
    },
  });

  restorePerTestAnimationFrame = () => {
    for (const timer of timersByFrameId.values()) {
      clearTimeout(timer);
    }
    timersByFrameId.clear();
    restorePropertyDescriptor(globalThis, 'requestAnimationFrame', previousRequest);
    restorePropertyDescriptor(globalThis, 'cancelAnimationFrame', previousCancel);
  };
}

export function overrideAnimationFrameWithManualFlush(): { flushAll: () => void } {
  restorePerTestAnimationFrame?.();
  const previousRequest = Object.getOwnPropertyDescriptor(
    globalThis,
    'requestAnimationFrame',
  );
  const previousCancel = Object.getOwnPropertyDescriptor(
    globalThis,
    'cancelAnimationFrame',
  );
  let nextFrameId = 1;
  const callbacksByFrameId = new Map<number, FrameRequestCallback>();

  Object.defineProperty(globalThis, 'requestAnimationFrame', {
    configurable: true,
    value(callback: FrameRequestCallback): number {
      const frameId = nextFrameId;
      nextFrameId += 1;
      callbacksByFrameId.set(frameId, callback);
      return frameId;
    },
  });
  Object.defineProperty(globalThis, 'cancelAnimationFrame', {
    configurable: true,
    value(frameId: number): void {
      callbacksByFrameId.delete(frameId);
    },
  });

  restorePerTestAnimationFrame = () => {
    callbacksByFrameId.clear();
    restorePropertyDescriptor(globalThis, 'requestAnimationFrame', previousRequest);
    restorePropertyDescriptor(globalThis, 'cancelAnimationFrame', previousCancel);
  };

  return {
    flushAll(): void {
      const callbacks = [...callbacksByFrameId.values()];
      callbacksByFrameId.clear();
      for (const callback of callbacks) {
        callback(performance.now());
      }
    },
  };
}

export function insertRenderedMermaidSvg(wrapper: HTMLElement): void {
  const inner =
    wrapper.querySelector<HTMLElement>('[data-streamdown-mermaid]') ??
    (() => {
      const el = document.createElement('div');
      el.setAttribute('data-streamdown-mermaid', '1');
      wrapper.appendChild(el);
      return el;
    })();
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('data-mermaid-svg', '1');
  const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  svg.appendChild(path);
  inner.appendChild(svg);
}
