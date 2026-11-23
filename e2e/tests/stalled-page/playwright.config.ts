// The nested run tests/stalled-page-capture.spec.ts starts. Its tests fail on
// purpose; nothing else runs this config.
import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: /blocked-page\.fixture\.ts$/,
  // Also the after-hooks slot after a test times out; one fixture test
  // spends nearly all of it in an afterEach.
  timeout: 12_000,
  workers: 1,
  retries: 0,
  reporter: [['json']],
  // The trace setting of ../../playwright.config.ts, which CI runs.
  use: { trace: { mode: 'retain-on-failure', screenshots: false }, screenshot: 'off' },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    { name: 'webkit', use: { ...devices['iPhone 13'] } },
  ],
});
