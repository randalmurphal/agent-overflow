// The nested run tests/stalled-page-capture.spec.ts starts. Its tests fail on
// purpose; nothing else runs this config.
import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: /blocked-page\.fixture\.ts$/,
  timeout: 30_000,
  workers: 1,
  retries: 0,
  reporter: [['json']],
  use: { trace: 'off', screenshot: 'off' },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    { name: 'webkit', use: { ...devices['iPhone 13'] } },
  ],
});
