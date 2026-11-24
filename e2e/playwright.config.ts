import { defineConfig, devices } from '@playwright/test';
import { MANUAL_SPEC, PROJECT_FILES, TEST_DIR } from './test-files.ts';

// The harness suite boots one real backend per worker (see
// tests/fixtures.ts); tests within a worker share it and reset state
// between tests. Workers scale horizontally — each gets its own
// process, data dir, and port. Four workers with their app backends and
// renderers peak near 6.5 GiB of summed RSS, under the 8 GiB ceiling
// ao-harness-e2e runs the suite under. AO_E2E_WORKERS sets the count
// explicitly: CI shards the suite across runners and runs three workers
// on each (.github/workflows/ci.yml).
function workerCount(): number {
  const explicit = process.env.AO_E2E_WORKERS;
  if (explicit === undefined || explicit === '') {
    return process.env.CI ? 1 : 4;
  }
  const count = Number(explicit);
  if (!Number.isInteger(count) || count < 1) {
    throw new Error(`AO_E2E_WORKERS must be a positive integer, got ${JSON.stringify(explicit)}`);
  }
  return count;
}

export default defineConfig({
  // The files each project runs are ./test-files.ts, which the wait scan
  // reads too.
  testDir: TEST_DIR,
  testIgnore: MANUAL_SPEC,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: workerCount(),
  retries: 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    trace: { mode: 'retain-on-failure', screenshots: false },
    // Functional-flow evidence is semantic state and bounded observations.
    // Trace snapshots contain DOM state only. Pixel capture is disabled.
    screenshot: 'off',
    // The app follows the system color scheme by default and Playwright
    // emulates light unless told otherwise; dark is what the reviewer of a
    // spec's screenshot is looking at.
    colorScheme: 'dark',
  },
  projects: [
    {
      name: 'desktop',
      use: { ...devices['Desktop Chrome'] },
      ...PROJECT_FILES.desktop,
    },
    {
      name: 'compact',
      use: { ...devices['Pixel 7'] },
      ...PROJECT_FILES.compact,
    },
    {
      name: 'mobile-webkit',
      use: { ...devices['iPhone 13'] },
      ...PROJECT_FILES['mobile-webkit'],
      grepInvert: /@chromium-lifecycle/,
    },
  ],
});
