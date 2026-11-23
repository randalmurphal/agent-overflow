// The `test` every spec builds on: Playwright's, plus the stalled-page
// capture (src/page-stall.ts), and nothing a spec could assert against.
// tests/fixtures.ts extends it with the harness backend and the page-error
// and CSP checks; a spec that wants neither imports this one.
// tests/base-test-imports.spec.ts fails a spec that imports `test` from
// '@playwright/test' instead.
import { test as base, expect } from '@playwright/test';
import { STALL_CAPTURE_TIMEOUT_MS, StalledPageCapture } from '../src/page-stall.js';

interface TestFixtures {
  /**
   * Arms a debugger session on every page a test opens and, when the test
   * fails, attaches a `stalled page` report for each open page that does not
   * answer (src/page-stall.ts).
   */
  stalledPageCapture: StalledPageCapture;
}

/**
 * The running test's stalled-page capture. A browser context binds the one
 * current when it is created, so a context the test opens reports to that
 * test and a context from a `beforeAll` reports to none.
 */
let currentStalledPageCapture: StalledPageCapture | undefined;

export const test = base.extend<TestFixtures>({
  browser: [
    async ({ browser }, use) => {
      browser.on('context', (context) => currentStalledPageCapture?.trackContext(context));
      await use(browser);
    },
    { scope: 'worker' },
  ],
  // Auto, so it is current before any context the test opens. Its own
  // teardown captures for a test without the `context` fixture, whose
  // contexts the test closes itself; the `context` override below captures
  // before the fixture context closes. Both have their own timeout, so
  // after-hooks that spent the shared teardown time cannot cut a capture.
  stalledPageCapture: [
    async ({}, use, testInfo) => {
      const capture = new StalledPageCapture();
      currentStalledPageCapture = capture;
      try {
        await use(capture);
      } finally {
        currentStalledPageCapture = undefined;
      }
      await capture.captureIfFailed(testInfo);
    },
    { auto: true, box: true, timeout: STALL_CAPTURE_TIMEOUT_MS },
  ],
  context: [
    async ({ context, stalledPageCapture }, use, testInfo) => {
      await use(context);
      await stalledPageCapture.captureIfFailed(testInfo);
    },
    { scope: 'test', timeout: STALL_CAPTURE_TIMEOUT_MS },
  ],
  // The page the fixture hands over is armed, so it cannot stall before its
  // Debugger domain is enabled.
  page: async ({ page, stalledPageCapture }, use) => {
    await stalledPageCapture.untilArmed(page);
    await use(page);
  },
});

export { expect };
