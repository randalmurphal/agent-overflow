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
