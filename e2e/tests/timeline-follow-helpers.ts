import { expect, type Page } from '@playwright/test';

/**
 * Leaves the timeline's bottom-follow as a reader does, with an upward key
 * on the timeline itself. A wheel is no substitute: over an activity run's
 * clip it scrolls the clip and leaves the timeline following, so a row then
 * opened is carried up with the bottom it follows. A timeline with room to
 * move must move.
 */
export async function escapeTimelineFollow(page: Page): Promise<void> {
  const scroller = page.getByTestId('message-timeline-scroll');
  const before = await scroller.evaluate((el) => el.scrollTop);
  await scroller.focus();
  await page.keyboard.press('ArrowUp');
  await scroller.evaluate((el) => el.blur());
  if (before > 0) {
    await expect.poll(() => scroller.evaluate((el) => el.scrollTop), {
      message: `the timeline did not leave ${Math.round(before)} for the upward key`,
    }).toBeLessThan(before);
  }
}
