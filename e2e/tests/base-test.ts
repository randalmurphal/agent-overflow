// The `test` every spec builds on: Playwright's, plus the gate's rules and
// the stalled-page capture (src/page-stall.ts), and nothing a spec could
// assert against. The gate's rules: a page sleep throws, and an event watch
// ends with the test that made it. tests/fixtures.ts extends it with the
// harness backend and the page-error and CSP checks; a spec that launches its
// own harnesses imports this one. The wait scan (src/wait-scan.ts) fails any
// other suite file that reaches Playwright's own `test`, so this file hands
// the harness the running test's TestInfo.
import { test as base, expect, type BrowserContext, type Page } from '@playwright/test';
import { installTestInfoReader, openEventWatchScope } from '../src/harness.js';
import { STALL_CAPTURE_TIMEOUT_MS, StalledPageCapture, StalledPageRegistry } from '../src/page-stall.js';

installTestInfoReader(() => base.info());

/** Makes `page.waitForTimeout` throw with the reason and the alternatives. */
function banPageSleep(page: Page): void {
  page.waitForTimeout = async (ms: number) => {
    throw new Error(
      `page.waitForTimeout(${ms}) is a sleep, which the gate does not run: give the product timer a knob ` +
        '(harnessTiming), advance the page clock (page.clock), poll the condition (expect.poll), or end an ' +
        'absence at a barrier (HarnessApp.watchEvents). See e2e/AGENTS.md "Writing specs".',
    );
  };
}

interface TestFixtures {
  /** Ends every HarnessApp.watchEvents watch the test made, at its end. */
  eventWatches: void;
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
  // A watch counts events for the test that made it; one the test never
  // asserted on, or abandoned at a failure, ends here rather than counting
  // into the next test.
  eventWatches: [
    async ({}, use) => {
      const end = openEventWatchScope();
      try {
        await use();
      } finally {
        end();
      }
    },
    { auto: true, box: true },
  ],
  // Every context, a `beforeAll`'s included, comes through here. No page
  // sleeps: `waitForTimeout` on any page throws with the way to wait instead.
  // The one timed hold the gate allows is src/pointer.ts pressToDragText,
  // which needs no page.
  browser: [
    async ({ browser, stalledPageRegistry }, use) => {
      const track = (context: BrowserContext) => {
        stalledPageRegistry.trackContext(context);
        context.on('page', banPageSleep);
      };
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
