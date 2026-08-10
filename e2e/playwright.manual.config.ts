import { defineConfig, devices } from '@playwright/test';

// Config for the operator-run investigation drivers (`*.manual.spec.ts`),
// which the base config deliberately testIgnores. These are not gate tests:
// they replay recorded incident data, they can run for many minutes, and a
// genuine finding is the failure.
//
//   cd e2e && pnpm test:freeze-repro
export default defineConfig({
  testDir: './tests',
  testMatch: /.*\.manual\.spec\.ts$/,
  // One live turn replays hundreds of items and a wedge capture dwells on the
  // profiler afterwards; the whole replay plus capture has to fit in here.
  timeout: 45 * 60_000,
  expect: { timeout: 30_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: 'list',
  use: {
    ...devices['Desktop Chrome'],
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    // A wedged renderer answers nothing; a short action timeout would just
    // convert the interesting hang into an uninformative locator failure.
    actionTimeout: 60_000,
  },
});
