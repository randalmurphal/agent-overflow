// Run only by tests/stalled-page-capture.spec.ts through ./playwright.config.ts.
// Each test blocks one or more pages and fails, as a test against a stalled
// app page does; the spec reads the attachments the fixtures' stalled-page
// capture leaves.
import type { BrowserContext, Page } from '@playwright/test';
import { expect, test } from '../fixtures.js';
import { startBlockedPageServer, type BlockedPageServer } from './blocked-page.js';

let server: BlockedPageServer;
test.beforeAll(async () => {
  server = await startBlockedPageServer();
});
test.afterAll(async () => {
  await server.close();
});

test.setTimeout(5_000);

async function block(page: Page, checks: boolean): Promise<void> {
  await page.goto(server.url);
  await page.evaluate((withChecks) => (window as unknown as { blockSoon(c: boolean): void }).blockSoon(withChecks), checks);
}

test('a page from the page fixture blocks', async ({ page }) => {
  await block(page, true);
  await page.getByRole('button').click();
});

test('a page in a context the test opens blocks', async ({ browser }) => {
  const context = await browser.newContext();
  try {
    const page = await context.newPage();
    await block(page, true);
    await page.getByRole('button').click();
  } finally {
    await context.close();
  }
});

test('a page blocked where a pause cannot land', async ({ page }) => {
  await block(page, false);
  await page.getByRole('button').click();
});

test('two pages block in one test', async ({ page, context }) => {
  const second = await context.newPage();
  await block(page, true);
  await block(second, true);
  await second.getByRole('button').click();
});

test('a test that passes until its page-error check fails', async ({ page, browser, pageErrors }) => {
  await page.goto(server.url);
  // Left open: the capture must still see it when the teardown fails.
  const context = await browser.newContext();
  const other = await context.newPage();
  await other.goto(server.url);
  await other.evaluate(() => {
    setTimeout(() => {
      throw new Error('thrown on purpose');
    }, 0);
    (window as unknown as { blockSoon(c: boolean): void }).blockSoon(true);
  });
  await expect.poll(() => pageErrors.length).toBe(1);
});

test.describe('a page a beforeAll opened', () => {
  let shared: { context: BrowserContext; page: Page };
  test.beforeAll(async ({ browser }) => {
    const context = await browser.newContext();
    shared = { context, page: await context.newPage() };
  });
  test.afterAll(async () => {
    await shared.context.close();
  });

  test('a page a beforeAll opened blocks', async () => {
    await block(shared.page, true);
    await shared.page.getByRole('button').click();
  });
});

test.describe('an afterEach that spends the shared teardown time', () => {
  // The after-hooks slot is the 12 s project timeout. This hook outlives it,
  // so a fixture without its own timeout gets no teardown at all.
  test.afterEach(async () => {
    await new Promise((resolve) => setTimeout(resolve, 13_000));
  });

  test('a page blocks before the afterEach runs', async ({ page }) => {
    await block(page, true);
    await page.getByRole('button').click();
  });
});
