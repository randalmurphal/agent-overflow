// Phone toolbar density, current picker summaries, and picker handoff.
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
    const rollup = page.getByTestId('composer-pickers-rollup');
    const menu = page.getByRole('menu', { name: 'Composer options', exact: true });
    for (const width of [412, 360, 320]) {
      await page.setViewportSize({ width, height: 850 });
      await rollup.tap();
      const effort = await page.getByTestId('composer-effort-trigger').getAttribute('aria-label');
      const access = await page.getByTestId('composer-access-toggle').getAttribute('aria-label');
      await expect(menu.getByRole('menuitem', { name: /^Effort…/ })).toContainText(effort!.replace('Effort: ', ''));
      await expect(menu.getByRole('menuitem', { name: /^Access…/ })).toContainText(access!.replace('Runtime Access Mode: ', ''));
      const enabled = await page.getByTestId('composer-mcp-trigger').getAttribute('data-enabled-count');
      await expect(menu.getByRole('menuitem', { name: /^MCP servers…/ })).toContainText(`${enabled} enabled`);
      const box = (await menu.boundingBox())!;
      expect(box.x).toBeGreaterThanOrEqual(0);
      expect(box.x + box.width).toBeLessThanOrEqual(width);
      for (const text of await menu.locator('span').all()) {
        expect(await text.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
      }
      await page.getByTestId('chat-header-title').tap();
    }
    await rollup.tap();
    await menu.getByRole('menuitem', { name: /^Effort…/ }).tap();
    await expect(page.getByTestId('composer-effort-trigger')).toHaveAttribute('aria-expanded', 'true');
    await page.getByRole('menuitem', { name: 'Low', exact: true }).tap();
    await rollup.tap();
    await expect(menu.getByRole('menuitem', { name: /^Effort…/ })).toContainText('Low');
  });
}
