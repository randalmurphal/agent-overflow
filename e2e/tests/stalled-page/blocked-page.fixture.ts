// Run only by tests/stalled-page-capture.spec.ts through ./playwright.config.ts.
// Each test blocks its page and then waits on it until the test times out,
// as a test against a stalled app page does; the spec reads the attachments
// the fixtures' stalled-page capture leaves.
import { test } from '../fixtures.js';
import { startBlockedPageServer, type BlockedPageServer } from './blocked-page.js';

let server: BlockedPageServer;
test.beforeAll(async () => {
  server = await startBlockedPageServer();
});
test.afterAll(async () => {
  await server.close();
});

test.setTimeout(5_000);

test('a page from the page fixture blocks', async ({ page }) => {
  await page.goto(server.url);
  await page.evaluate(() => (window as unknown as { blockSoon(checks: boolean): void }).blockSoon(true));
  await page.getByRole('button').click();
});

test('a page in a context the test opens blocks', async ({ browser }) => {
  const context = await browser.newContext();
  try {
    const page = await context.newPage();
    await page.goto(server.url);
    await page.evaluate(() => (window as unknown as { blockSoon(checks: boolean): void }).blockSoon(true));
    await page.getByRole('button').click();
  } finally {
    await context.close();
  }
});

test('a page blocked where a pause cannot land', async ({ page }) => {
  await page.goto(server.url);
  await page.evaluate(() => (window as unknown as { blockSoon(checks: boolean): void }).blockSoon(false));
  await page.getByRole('button').click();
});
