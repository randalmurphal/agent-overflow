// Reordering saved accounts by touch, under the phone descriptor: a real
// touch drag on a card's grip (raw CDP touch events, the way a device
// produces them) moves the card, the backend keeps the order, and the drag
// does not scroll the settings page out from under the finger.
//
// Owns its backend: it adopts provider accounts, which is process-wide state
// no other spec should inherit.

import { expect, test } from '@playwright/test';

import { launchHarness } from '../src/harness.js';
import {
  cardOrder,
  claudeAccountId,
  claudeAccounts,
  claudeAccountsSection,
  completeClaudeSignIn,
  openClaudeAccounts,
  seedNativeClaudeLogin,
} from './provider-account-helpers.js';

const ACTIVE_EMAIL = 'active@agent-overflow.test';
const SECOND_EMAIL = 'second@agent-overflow.test';

test('a touch drag on a grip reorders the saved accounts', async ({ page }) => {
  const harness = await launchHarness();
  try {
    await seedNativeClaudeLogin(harness, ACTIVE_EMAIL, Date.now() + 20 * 24 * 3600_000);
    await harness.open(page);
    await openClaudeAccounts(page);
    const section = claudeAccountsSection(page);
    await section.getByRole('button', { name: 'Log in to another account' }).click();
    await completeClaudeSignIn(page, SECOND_EMAIL);
    const activeId = await claudeAccountId(harness, ACTIVE_EMAIL);
    const secondId = await claudeAccountId(harness, SECOND_EMAIL);
    await expect.poll(() => cardOrder(page)).toEqual([activeId, secondId]);

    await section.evaluate((node) => node.scrollIntoView({ block: 'center' }));
    const grip = section.getByTestId(`provider-account-reorder-${secondId}`);
    const target = section.getByTestId(`provider-account-${activeId}`);
    const from = (await grip.boundingBox())!;
    const over = (await target.boundingBox())!;
    // The compact grip is a finger-sized target.
    expect(from.height).toBeGreaterThanOrEqual(36);
    const scroller = () => section.evaluate((node) => {
      let el = node.parentElement;
      while (el && !['auto', 'scroll'].includes(getComputedStyle(el).overflowY)) el = el.parentElement;
      return el?.scrollTop ?? 0;
    });
    const scrollBefore = await scroller();

    const x = from.x + from.width / 2;
    const fromY = from.y + from.height / 2;
    const toY = over.y + over.height / 4;
    const cdp = await page.context().newCDPSession(page);
    try {
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y: fromY }] });
      const steps = 12;
      for (let i = 1; i <= steps; i++) {
        const y = fromY + ((toY - fromY) * i) / steps;
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y }] });
      }
      await expect.poll(async () => target.evaluate((card) => getComputedStyle(card).transform)).not.toBe('none');
      expect(await scroller(), 'the drag must not scroll the page').toBe(scrollBefore);
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    } finally {
      await cdp.detach();
    }

    await expect.poll(() => cardOrder(page)).toEqual([secondId, activeId]);
    await expect.poll(async () => (await claudeAccounts(harness)).map((account) => account.id)).toEqual([secondId, activeId]);
    expect((await claudeAccounts(harness)).find((account) => account.active)?.id).toBe(activeId);
  } finally {
    await harness.close();
  }
});
