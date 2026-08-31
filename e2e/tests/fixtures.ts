// Shared Playwright fixtures: one harness backend per worker, reset to
// a blank slate before every test. Import { test, expect } from here
// instead of '@playwright/test'.
import { test as base, expect } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';

interface WorkerFixtures {
  harnessWorker: HarnessApp;
}

interface TestFixtures {
  harness: HarnessApp;
}

export const test = base.extend<TestFixtures, WorkerFixtures>({
  harnessWorker: [
    async ({}, use) => {
      const app = await launchHarness();
      await use(app);
      await app.close();
    },
    { scope: 'worker' },
  ],
  harness: async ({ harnessWorker }, use) => {
    await harnessWorker.reset();
    await use(harnessWorker);
  },

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

/** Shapes of the harness events the tests await. */
export interface HarnessMockEvent {
  mockId: string;
  protocol: string;
  cwd: string;
  scenario: string;
  report: {
    kind: string;
    turn?: number;
    step?: number;
    detail?: string;
    /** Set on `user_input`: the text the mock received on the wire. */
    input?: string;
    /** Set on `user_input`: Claude's session id or Codex's thread id. */
    sessionRef?: string;
  };
}

export interface SeedResult {
  projects: Array<{
    projectId: string;
    path: string;
    threadIds: string[];
    workItemIds: string[];
  }>;
}
