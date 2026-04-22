import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/svelte';
import { resetWailsMocks } from './mocks/wailsio-runtime';
import { resetBindingMocks } from './mocks/bindings-app';

if (typeof globalThis.ResizeObserver === 'undefined') {
  class StubResizeObserver {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
}

// Web Animations API stub. happy-dom does not implement element.animate,
// so Svelte 5's built-in fade/scale/slide transitions throw when a
// component that opts into them mounts under test. The stub returns an
// Animation-shaped object whose `finished` promise is already resolved,
// and which synchronously fires `finish` via onfinish so Svelte's
// transition machinery completes on the next microtask (matching the
// behavior tests expected back when transitions were effectively no-ops
// because happy-dom silently swallowed the missing method).
if (typeof Element !== 'undefined' && typeof Element.prototype.animate !== 'function') {
  (Element.prototype as unknown as { animate: (...args: unknown[]) => Animation }).animate =
    function stubAnimate(): Animation {
      const handlers: { finish: Array<(e: Event) => void>; cancel: Array<(e: Event) => void> } = {
        finish: [],
        cancel: [],
      };
      const anim = {
        cancel() {
          for (const h of handlers.cancel) h(new Event('cancel'));
          if (typeof anim.oncancel === 'function') {
            anim.oncancel.call(anim as unknown as Animation, new Event('cancel'));
          }
        },
        finish() {
          for (const h of handlers.finish) h(new Event('finish'));
          if (typeof anim.onfinish === 'function') {
            anim.onfinish.call(anim as unknown as Animation, new Event('finish'));
          }
        },
        play() {},
        pause() {},
        reverse() {},
        addEventListener(type: string, cb: EventListenerOrEventListenerObject) {
          const fn = cb as (e: Event) => void;
          if (type === 'finish') handlers.finish.push(fn);
          else if (type === 'cancel') handlers.cancel.push(fn);
        },
        removeEventListener(type: string, cb: EventListenerOrEventListenerObject) {
          const fn = cb as (e: Event) => void;
          if (type === 'finish') handlers.finish = handlers.finish.filter((h) => h !== fn);
          else if (type === 'cancel') handlers.cancel = handlers.cancel.filter((h) => h !== fn);
        },
        dispatchEvent() { return true; },
        currentTime: 0,
        playState: 'finished' as AnimationPlayState,
        finished: Promise.resolve() as unknown as Animation['finished'],
        onfinish: null as ((this: Animation, ev: Event) => unknown) | null,
        oncancel: null as ((this: Animation, ev: Event) => unknown) | null,
      };
      // Fire finish on the next microtask so Svelte's transition runner
      // observes the "completed" state and tears down the DOM node in the
      // same tick a real animation would.
      queueMicrotask(() => anim.finish());
      return anim as unknown as Animation;
    };
}

if (typeof globalThis.matchMedia === 'undefined') {
  globalThis.matchMedia = (() => ({
    matches: false,
    media: '',
    onchange: null,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent() { return false; },
  })) as unknown as typeof matchMedia;
}

// happy-dom exposes `localStorage` as an empty object without the Storage
// prototype methods attached, so tests that exercise code using
// localStorage hit "setItem is not a function". Install a minimal
// in-memory Storage implementation once, and clear it between tests in
// the afterEach below so specs don't leak state at each other.
const __storage = new Map<string, string>();
const memoryStorage: Storage = {
  get length() { return __storage.size; },
  clear(): void { __storage.clear(); },
  getItem(key: string): string | null {
    return __storage.has(key) ? (__storage.get(key) as string) : null;
  },
  setItem(key: string, value: string): void { __storage.set(key, String(value)); },
  removeItem(key: string): void { __storage.delete(key); },
  key(index: number): string | null {
    return Array.from(__storage.keys())[index] ?? null;
  },
};
if (
  typeof globalThis.localStorage === 'undefined' ||
  typeof (globalThis.localStorage as Storage | undefined)?.setItem !== 'function'
) {
  Object.defineProperty(globalThis, 'localStorage', {
    value: memoryStorage,
    configurable: true,
    writable: true,
  });
}
if (typeof globalThis.window !== 'undefined') {
  // Keep window.localStorage in sync so components that reach through
  // window explicitly see the same store as globalThis.localStorage.
  Object.defineProperty(globalThis.window, 'localStorage', {
    value: memoryStorage,
    configurable: true,
    writable: true,
  });
}

afterEach(() => {
  cleanup();
  resetWailsMocks();
  resetBindingMocks();
  // Wipe the in-memory localStorage between tests so persistence-aware
  // stores don't leak state across suites.
  try {
    if (typeof localStorage !== 'undefined' && typeof localStorage.clear === 'function') {
      localStorage.clear();
    }
  } catch {
    // ignore — tests running without localStorage continue happily.
  }
});
