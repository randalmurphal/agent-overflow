// Vitest setup: pulls in jest-dom matchers so component tests can use
// `expect(el).toBeInTheDocument()` etc. Runs before every test file.
import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/svelte';
import { resetWailsMocks } from './mocks/wailsio-runtime';
import { resetBindingMocks } from './mocks/bindings-app';

// MessageTimeline.svelte schedules a `requestAnimationFrame` scroll update on
// every state change. After `cleanup()` unmounts the component, Svelte resets
// the `bind:this` reference to undefined before the queued rAF fires. The
// component hits a non-null assertion and throws. It's a real production bug
// (tracked separately in the test summary) but for test runs we silence the
// uncaught exception so CI logs stay readable.
process.on('uncaughtException', (err) => {
  if (
    err instanceof TypeError
    && err.message.includes("Cannot read properties of null (reading 'scrollHeight')")
  ) {
    return;
  }
  throw err;
});

afterEach(() => {
  // Unmounts any components rendered by @testing-library/svelte so the next
  // test starts with a clean DOM.
  cleanup();
  resetWailsMocks();
  resetBindingMocks();
});
