import { test, expect } from './fixtures.js';
import { seedAgentThread } from './agent-visibility-helpers.js';

for (const provider of ['claude', 'codex'] as const) {
  test(`${provider} keeps readable model text before rolling up other controls`, async ({ harness, page }) => {
    await seedAgentThread(harness, 'model-label', 'Model label', provider);
    await harness.open(page);
    await page.getByTestId('thread-row').click();
    const toolbar = page.getByTestId('composer-toolbar');
    const model = page.getByTestId('composer-model-menu-trigger');
    const label = model.locator(':scope > span.truncate');
    for (const width of [412, 360, 320, 900, 412]) {
      await page.setViewportSize({ width, height: 850 });
      // At the phone's normal width the model text must fit in full.
      // Tiny viewports may ellipsize, but may never erase the label.
      if (width >= 412) {
        await expect.poll(() => label.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
      } else {
        await expect.poll(() => label.evaluate((el) => el.clientWidth)).toBeGreaterThan(20);
      }
      await expect.poll(() => toolbar.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
      if (width <= 412) {
        await expect(page.getByTestId('composer-pickers-rollup')).toBeVisible();
        await expect(page.getByTestId('composer-effort-trigger')).toBeHidden();
      } else {
        await expect(page.getByTestId('composer-effort-trigger')).toBeVisible();
      }
    }
    await page.getByTestId('composer-pickers-rollup').tap();
    await page.getByRole('menuitem', { name: 'Effort…' }).tap();
    await expect(page.getByTestId('composer-effort-trigger')).toHaveAttribute('aria-expanded', 'true');
  });
}
