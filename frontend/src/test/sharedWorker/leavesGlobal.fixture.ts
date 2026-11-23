// Leaves a DOM prototype member behind; its retirement fails it.
//
// Vitest sends a file's result as the file finishes: at once when nothing
// was sent in the last 100ms, otherwise up to 100ms later with whatever
// the result holds then. The last update before that is the end of the
// file's afterAll hooks. A file fixture's teardown runs after them and
// sends nothing, so one of 300ms makes this file's result go out at once,
// before its retirement runs: the retirement must send it again.
import { test } from 'vitest';

Object.defineProperty(HTMLElement.prototype, 'aoFixtureLeftover', { value: () => 0, configurable: true });

const withSlowTeardown = test.extend<{ slowTeardown: void }>({
  slowTeardown: [
    // oxlint-disable-next-line no-empty-pattern -- Vitest reads a fixture's dependencies from this pattern; it has none
    async ({}, use) => {
      await use();
      await new Promise((resolve) => setTimeout(resolve, 300));
    },
    { scope: 'file' },
  ],
});

withSlowTeardown('passes its own test', ({ slowTeardown }) => slowTeardown);
