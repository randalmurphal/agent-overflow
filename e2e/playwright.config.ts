import { defineConfig, devices } from '@playwright/test';

// The harness suite boots one real backend per worker (see
// tests/fixtures.ts); tests within a worker share it and reset state
// between tests. Workers scale horizontally — each gets its own
// process, data dir, and port — but stay conservative by default so a
// laptop run doesn't spawn a fleet of webviews.
export default defineConfig({
  testDir: './tests',
  // Operator-run investigation drivers (`*.manual.spec.ts`) are never part of
  // the gate: they need locally generated, gitignored fixtures and they leave
  // deliberately extreme state behind. `playwright.manual.config.ts` is how you
  // run one — same rule as the frontend's `*.manual.ts` vitest drivers.
  testIgnore: /.*\.manual\.spec\.ts$/,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: process.env.CI ? 1 : 2,
  retries: 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    ...devices['Desktop Chrome'],
    trace: { mode: 'retain-on-failure', screenshots: false },
    // Functional-flow evidence is semantic state and bounded observations.
    // Trace snapshots contain DOM state only. Pixel capture is disabled.
    screenshot: 'off',
  },
});
