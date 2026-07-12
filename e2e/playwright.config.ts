import { defineConfig, devices } from '@playwright/test';

// The harness suite boots one real backend per worker (see
// tests/fixtures.ts); tests within a worker share it and reset state
// between tests. Workers scale horizontally — each gets its own
// process, data dir, and port — but stay conservative by default so a
// laptop run doesn't spawn a fleet of webviews.
export default defineConfig({
  testDir: './tests',
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: process.env.CI ? 1 : 2,
  retries: 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    ...devices['Desktop Chrome'],
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
});
