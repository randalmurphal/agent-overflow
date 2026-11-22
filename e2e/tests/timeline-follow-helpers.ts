import { expect, type Locator } from '@playwright/test';

/**
 * The timeline scroller that holds `element`. The thread and an open agent
 * pane each mount their own, so a row's scroll and follow state is its
 * nearest one.
 */
export function timelineOf(element: Locator): Locator {
  return element.locator('xpath=ancestor::*[@data-testid="message-timeline-scroll"][1]');
}

/**
 * Leaves a timeline's bottom-follow as a reader does, with an upward key
 * on the timeline itself. A wheel is no substitute: over an activity run's
 * clip it scrolls the clip and leaves the timeline following, so a row then
 * opened is carried up with the bottom it follows. A timeline with room to
 * move must move.
 */
export async function escapeTimelineFollow(timeline: Locator): Promise<void> {
  const before = await timeline.evaluate((el) => el.scrollTop);
  await timeline.focus();
  await timeline.page().keyboard.press('ArrowUp');
  await timeline.evaluate((el) => (el as HTMLElement).blur());
  if (before > 0) {
    await expect.poll(() => timeline.evaluate((el) => el.scrollTop), {
      message: `the timeline did not leave ${Math.round(before)} for the upward key`,
    }).toBeLessThan(before);
  }
}
