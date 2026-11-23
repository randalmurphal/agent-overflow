// Config for scripts/check-stalled-page-watchdog.mjs: one browser file whose
// page blocks, under the real watchdog with a short bound.
import { configDefaults, defineConfig } from 'vitest/config';
import { playwright } from '@vitest/browser-playwright';
import { StalledPageWatchdog } from '../stalledPageWatchdog.ts';

// scripts/check-stalled-page-watchdog.mjs expects this bound in the report.
const CHECK_BOUND_MS = 8_000;
const HOLD_MS = 500;

export default defineConfig({
  plugins: [
    {
      name: 'stalled-page-hold',
      configureServer(server) {
        server.middlewares.use('/__stall/hold', (_request, response) => {
          setTimeout(() => response.end('held'), HOLD_MS);
        });
      },
    },
  ],
  test: {
    reporters: [...configDefaults.reporters, new StalledPageWatchdog({ boundMs: CHECK_BOUND_MS, checkEveryMs: 1_000 })],
    include: ['src/test/stalledPage/stall.fixture.ts'],
    testTimeout: 5_000,
    hookTimeout: 5_000,
    browser: {
      enabled: true,
      provider: playwright(),
      headless: true,
      instances: [{ browser: 'chromium' }],
      screenshotFailures: false,
    },
  },
});
