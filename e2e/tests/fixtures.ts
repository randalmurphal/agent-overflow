// Shared Playwright fixtures: one harness backend per worker, reset to
// a blank slate before every test. Import { test, expect } from here
// instead of '@playwright/test'.
import { test as base, expect } from '@playwright/test';
import { launchHarness, type HarnessApp, type HarnessMockEventData } from '../src/harness.js';
import { webkitKeychainHome } from '../src/webkit-keychain.js';

interface WorkerFixtures {
  harnessWorker: HarnessApp;
}

interface TestFixtures {
  harness: HarnessApp;
  /**
   * Every uncaught error thrown by a page in a browser context the test
   * created, the `page` fixture's included, stack first.
   */
  pageErrors: string[];
  /**
   * Set through `test.use({ expectPageErrors: true })` by a spec that
   * provokes a page error on purpose. That spec asserts on `pageErrors`
   * itself; every other spec fails at teardown when a page threw.
   */
  expectPageErrors: boolean;
}

/**
 * The running test's page-error list. A browser context binds the list
 * current when it is created, so a context the test opens reports to
 * that test and a context from a `beforeAll` reports to none.
 */
let currentPageErrors: string[] | undefined;

export const test = base.extend<TestFixtures, WorkerFixtures>({
  harnessWorker: [
    async ({}, use) => {
      const app = await launchHarness();
      await use(app);
      await app.close();
    },
    { scope: 'worker' },
  ],
  // WebKit launches with a HOME of its own keychain (src/webkit-keychain.ts).
  launchOptions: [
    async ({ launchOptions, browserName }, use) => {
      if (browserName !== 'webkit') {
        await use(launchOptions);
        return;
      }
      const keychain = await webkitKeychainHome();
      try {
        await use(keychain.env ? { ...launchOptions, env: { ...keychain.env, ...launchOptions.env, HOME: keychain.env.HOME } } : launchOptions);
      } finally {
        await keychain.close();
      }
    },
    { scope: 'worker' },
  ],
  // The worker backend outlives the test, so its stderr tail is attached
  // here when the test failed, including a reset that failed.
  harness: async ({ harnessWorker }, use, testInfo) => {
    try {
      await harnessWorker.awaitNoPages();
      await harnessWorker.reset();
    } catch (error) {
      await harnessWorker.attachStderr(testInfo);
      throw error;
    }
    await use(harnessWorker);
    if (testInfo.status !== testInfo.expectedStatus) await harnessWorker.attachStderr(testInfo);
  },
  // Every test is also an uncaught-exception check. A component that throws
  // mid-render leaves stale DOM behind, which a functional assertion only
  // reports as a timeout. The browser hands every context it creates to the
  // running test's list, and each page in that context reports into it: the
  // `page` fixture's page, a second page the spec opens, and a page in a
  // context the spec creates itself. A page listener survives navigation and
  // reconnect, so nothing a page threw earlier in the test is lost.
  browser: [
    async ({ browser }, use) => {
      browser.on('context', (context) => {
        const errors = currentPageErrors;
        if (!errors) return;
        context.on('page', (page) => {
          page.on('pageerror', (error) => errors.push(error.stack ?? error.message));
        });
      });
      await use(browser);
    },
    { scope: 'worker' },
  ],
  expectPageErrors: [false, { option: true }],
  pageErrors: [
    async ({ expectPageErrors }, use) => {
      const errors: string[] = [];
      currentPageErrors = errors;
      try {
        await use(errors);
      } finally {
        currentPageErrors = undefined;
      }
      if (!expectPageErrors) {
        expect(
          errors,
          `a page threw ${errors.length} uncaught error(s); fix the cause in the app, or ` +
            'opt the spec out with test.use({ expectPageErrors: true }) if it provokes them on purpose:\n\n' +
            errors.join('\n\n'),
        ).toEqual([]);
      }
    },
    { auto: true },
  ],

  // Every test in the suite is also a Content-Security-Policy check, for
  // whatever it happens to render. The shipped policy
  // (internal/transport.CSPProduction) is derived from what the bundle was
  // observed to load, so the failure mode it has is a load nobody thought
  // of: the engine refuses it, the app quietly renders without it, and no
  // assertion in a functional test notices a missing image or an inert
  // script. This suite drives the real bundle through the real server in a
  // real engine, which makes it the one place that can see the refusal.
  //
  // Collected in Node rather than on `window` so a test that navigates more
  // than once cannot lose the violations from its earlier page — an init
  // script re-runs per navigation and would reset a page-side array.
  page: async ({ page }, use) => {
    const violations: string[] = [];
    await page.exposeFunction('__aoReportCspViolation', (violation: string) => {
      violations.push(violation);
    });
    await page.addInitScript(() => {
      document.addEventListener('securitypolicyviolation', (event) => {
        const report = `${event.effectiveDirective || event.violatedDirective} refused ${
          event.blockedURI || '(inline)'
        }${event.sourceFile ? ` from ${event.sourceFile}:${event.lineNumber}` : ''}`;
        (
          window as unknown as { __aoReportCspViolation?: (value: string) => void }
        ).__aoReportCspViolation?.(report);
      });
    });

    await use(page);

    expect(
      violations,
      'the page reported Content-Security-Policy violations: either the load is legitimate and ' +
        'internal/transport.CSPProduction has to admit it (with the reason recorded on the ' +
        'constant), or it is a load the app should not be making',
    ).toEqual([]);
  },
});

export { expect };

/**
 * Shapes of the harness events the tests await. One definition, owned by
 * the wire client: a spec reading a new report field must not have to
 * find two copies of it.
 */
export type HarnessMockEvent = HarnessMockEventData;

export interface SeedResult {
  projects: Array<{
    projectId: string;
    path: string;
    threadIds: string[];
    workItemIds: string[];
  }>;
}
