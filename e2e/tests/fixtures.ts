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
  };
}

export interface SeedResult {
  projects: Array<{ projectId: string; path: string; threadIds: string[] }>;
}
