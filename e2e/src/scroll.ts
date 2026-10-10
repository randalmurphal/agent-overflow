import type { Locator, Page } from '@playwright/test';

/** Capture a reading position only after native wheel motion has settled. */
export async function waitForScrollSettle(scroller: Locator): Promise<void> {
  await scroller.evaluate(async (element) => {
    let previous = element.scrollTop;
    let stableFrames = 0;
    for (let frame = 0; frame < 120; frame += 1) {
      await new Promise(requestAnimationFrame);
      const current = element.scrollTop;
      stableFrames = Math.abs(current - previous) <= 0.01 ? stableFrames + 1 : 0;
      previous = current;
      if (stableFrames >= 30) return;
    }
    throw new Error('scroll gesture did not settle');
  });
}

/**
 * Wait for every web font the page has started loading, before reading a
 * position that a later read is compared against.
 *
 * The app's faces use `font-display: swap`, so rows first lay out in a
 * fallback face and change height when the real face lands. A change under
 * the idle re-pin deadband (`IDLE_REPIN_DEADBAND_PX`) leaves a bottom-pinned
 * pane where it was, so a baseline read before the swap captures a position
 * the pane never returns to. A face starts loading on first use, so call
 * this after the content that uses it is visible.
 */
export async function waitForWebFonts(page: Page): Promise<void> {
  await page.evaluate(async () => {
    await document.fonts.ready;
  });
}
