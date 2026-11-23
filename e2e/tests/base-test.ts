// The `test` every spec builds on: Playwright's, plus the stalled-page
// capture (src/page-stall.ts), and nothing a spec could assert against.
// tests/fixtures.ts extends it with the harness backend and the page-error
// and CSP checks; a spec that wants neither imports this one.
// tests/base-test-imports.spec.ts fails a spec that imports `test` from
// '@playwright/test' instead.
import { test as base, expect, type BrowserContext } from '@playwright/test';
import { STALL_CAPTURE_TIMEOUT_MS, StalledPageCapture, StalledPageRegistry } from '../src/page-stall.js';

interface TestFixtures {
  /**
   * When the test fails, attaches a `stalled page` report for each page open
   * in the worker that does not answer (src/page-stall.ts).
   */
  stalledPageCapture: StalledPageCapture;
}

interface WorkerFixtures {
  /** Every context the worker's browser creates, its pages armed as they open. */
  stalledPageRegistry: StalledPageRegistry;
}

export const test = base.extend<TestFixtures, WorkerFixtures>({
  stalledPageRegistry: [
    async ({}, use) => {
      await use(new StalledPageRegistry());
    },
    { scope: 'worker', box: true },
  ],
  // Every context, a `beforeAll`'s included, comes through here.
  browser: [
    async ({ browser, stalledPageRegistry }, use) => {
      const track = (context: BrowserContext) => stalledPageRegistry.trackContext(context);
      browser.on('context', track);
      try {
        await use(browser);
      } finally {
        browser.off('context', track);
      }
    },
    { scope: 'worker' },
  ],
  // Auto, so a test without the `context` fixture, whose contexts the test
  // closes itself, is still captured at its teardown; the `context` override
  // below captures before the fixture context closes. Both have their own
  // timeout, so after-hooks that spent the shared teardown time cannot cut a
  // capture.
  stalledPageCapture: [
    async ({ stalledPageRegistry }, use, testInfo) => {
      const capture = new StalledPageCapture(stalledPageRegistry);
      await use(capture);
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
  // Waits up to STALL_ARM_MS for the page's session to arm, then hands the
  // page over either way; the capture reports a page that was not armed as
  // such.
  page: async ({ page, stalledPageCapture }, use) => {
    await stalledPageCapture.untilArmed(page);
    await use(page);
  },
});

export { expect };
