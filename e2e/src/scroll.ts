import type { Locator } from '@playwright/test';

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
