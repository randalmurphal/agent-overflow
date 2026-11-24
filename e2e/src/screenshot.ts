// The suite's one screenshot switch. A screenshot is evidence for a person
// reading a run, never an assertion (docs/specs/testing-harness.md: evidence
// is text), so a run takes none unless AO_E2E_SCREENSHOTS=1. Every capture
// goes through `screenshot` or `attachScreenshot`; the steps a capture helper
// takes before its shot (placing a row, checking it is on screen and still)
// run either way. A spec that reads a screenshot's pixels as its assertion
// calls Playwright's own `screenshot`, which this switch does not touch.
import type { Locator, LocatorScreenshotOptions, Page, PageScreenshotOptions, TestInfo } from '@playwright/test';

/** Whether this run takes screenshots: AO_E2E_SCREENSHOTS=1. */
export const screenshotsOn = process.env.AO_E2E_SCREENSHOTS === '1';

/** Takes `target`'s screenshot when screenshots are on; otherwise does nothing. */
export async function screenshot(target: Page, options: PageScreenshotOptions): Promise<void>;
export async function screenshot(target: Locator | Page, options: LocatorScreenshotOptions): Promise<void>;
export async function screenshot(target: Locator | Page, options: PageScreenshotOptions & LocatorScreenshotOptions): Promise<void> {
  if (!screenshotsOn) return;
  await target.screenshot(options);
}

/** Attaches `target`'s screenshot to the test as `name` when screenshots are on. */
export async function attachScreenshot(testInfo: TestInfo, name: string, target: Locator | Page): Promise<void> {
  if (!screenshotsOn) return;
  await testInfo.attach(name, { body: await target.screenshot(), contentType: 'image/png' });
}
