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

afterEach(() => {
  cleanup();
  resetWailsMocks();
  resetBindingMocks();
});
