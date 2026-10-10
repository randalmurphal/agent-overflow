import { defineConfig, devices } from '@playwright/test';

// The harness suite boots one real backend per worker (see
// tests/fixtures.ts); tests within a worker share it and reset state
// between tests. Workers scale horizontally — each gets its own
// process, data dir, and port — but stay conservative by default so a
// laptop run doesn't spawn a fleet of webviews. AO_E2E_WORKERS sets the
// count explicitly: CI shards the suite across runners and runs two
// workers on each (.github/workflows/ci.yml).
function workerCount(): number {
  const explicit = process.env.AO_E2E_WORKERS;
  if (explicit === undefined || explicit === '') {
    return process.env.CI ? 1 : 2;
  }
  const count = Number(explicit);
  if (!Number.isInteger(count) || count < 1) {
    throw new Error(`AO_E2E_WORKERS must be a positive integer, got ${JSON.stringify(explicit)}`);
  }
  return count;
}

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
  workers: workerCount(),
  retries: 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    trace: { mode: 'retain-on-failure', screenshots: false },
    // Functional-flow evidence is semantic state and bounded observations.
    // Trace snapshots contain DOM state only. Pixel capture is disabled.
    screenshot: 'off',
  },
  // Compact is a layout mode of the one app (frontend/AGENTS.md § Compact),
  // so it is a second PROJECT over the same harness, not a second suite:
  // the `compact-*` specs run under a phone descriptor (touch, coarse
  // pointer, a 412px layout viewport), and everything else under a desktop
  // one. A surface is done only when both projects pass.
  projects: [
    {
      name: 'desktop',
      use: { ...devices['Desktop Chrome'] },
      testIgnore: [/.*\.manual\.spec\.ts$/, /compact-.*\.spec\.ts$/],
    },
    {
      name: 'compact',
      use: { ...devices['Pixel 7'] },
      testMatch: /compact-.*\.spec\.ts$/,
    },
    {
      name: 'mobile-webkit',
      use: { ...devices['iPhone 13'] },
      testMatch: /compact-browser-lock\.spec\.ts$/,
      grepInvert: /@chromium-lifecycle/,
    },
  ],
});
