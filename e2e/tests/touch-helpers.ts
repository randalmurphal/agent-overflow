import type { Locator, Page } from '@playwright/test';

import { expect } from './fixtures.js';

/**
 * A held touch, the way a device produces one: raw touch events through
 * CDP, so the app's own long-press detector (utils/longPressContextMenu.ts)
 * is what turns it into a menu. Playwright's `tap` is a tap, and no engine
 * under emulation raises `contextmenu` for a hold on its own. The touch is
 * released once `opens` is visible.
 */
export async function longPress(page: Page, target: Locator, opens: Locator): Promise<void> {
  // A raw touch has none of the actionability waits Playwright's `tap`
  // makes. The list renders during the initial sync, while the transport
  // banner still occupies a row above it in compact layout; a box measured
  // then is stale by the time the banner leaves.
  await expect(page.getByTestId('transport-status-banner')).toHaveCount(0);
  await target.scrollIntoViewIfNeeded();
  const box = await target.boundingBox();
  if (!box) throw new Error('long-press target is not visible');
  const x = box.x + box.width / 2;
  const y = box.y + box.height / 2;
  const cdp = await page.context().newCDPSession(page);
  try {
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y }] });
    // Hold until the app has answered the press, so the release cannot race
    // the detector's timer.
    await expect(opens).toBeVisible();
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  } finally {
    await cdp.detach();
  }
}
