import { expect, test, type Page } from '@playwright/test';
import { writeFile } from 'node:fs/promises';
import { join, relative, isAbsolute } from 'node:path';
import { launchHarness } from '../src/harness.js';
import { confirmOnHost, mintInvite, nonLoopbackIPv4, redeemOnScreen } from './offhost-helpers.js';
import { PNG_BYTES } from './attachment-fixture.js';

async function settingsPage(page: Page, label: string): Promise<void> {
  if (!(await page.getByTestId('settings-overlay').count())) await page.getByRole('button', { name: 'Settings', exact: true }).click();
  const back = page.getByRole('button', { name: 'All settings', exact: true });
  if (await back.isVisible()) await back.click();
  await page.getByRole('tab', { name: label, exact: true }).click();
}

export function frontendAssetsFlow(): void {
  test('a paired frontend keeps custom appearance files and explicitly copies updates without changing its selections', async ({ page }) => {
    test.skip(nonLoopbackIPv4() === null, 'requires a LAN interface for a paired browser');
    test.setTimeout(60_000);
    page.setDefaultTimeout(10_000);
    const harness = await launchHarness();
    const errors: string[] = [];
    page.on('pageerror', (error) => errors.push(error.message));
    try {
      const themes = await harness.rpc<{ dir: string }>('GetThemeFiles');
      const spinners = await harness.rpc<{ dir: string }>('GetSpinnerFiles');
      for (const dir of [themes.dir, spinners.dir]) {
        const within = relative(harness.bootstrap.dataDir, dir);
        expect(isAbsolute(within) || within.startsWith('..'), 'fixture must own its appearance files').toBe(false);
      }
      const theme = (name: string) => JSON.stringify({ name, dark: { colors: { accent: '#88c0d0' } } });
      await writeFile(join(themes.dir, 'travel.json'), theme('Travel'));
      await harness.rpc('SetNetworkSettings', { bindAll: true });
      const invite = await mintInvite(harness, 'full');
      await confirmOnHost(harness, await redeemOnScreen(page, invite, 'Appearance phone'));
      await expect(page.getByRole('button', { name: 'Settings', exact: true })).toBeVisible();
      await settingsPage(page, 'Theme');
      const uiTheme = page.getByTestId('settings-ui-theme');
      await expect(uiTheme.locator('option[value="travel"]')).toHaveText('Travel ⏾');
      await page.getByTestId('settings-theme-mode').selectOption('dark');
      await uiTheme.selectOption('travel');

      await writeFile(join(themes.dir, 'travel.json'), theme('Travel revised'));
      await writeFile(join(themes.dir, 'desk.json'), theme('Desk'));
      await page.reload();
      await settingsPage(page, 'Theme');
      await expect(uiTheme).toHaveValue('travel');
      await expect(uiTheme.locator('option[value="travel"]')).toHaveText('Travel ⏾');
      await expect(uiTheme.locator('option[value="desk"]')).toHaveCount(0);
      await page.locator('[data-settings-field="theme.copy-files"]').getByRole('button', { name: 'Copy', exact: true }).click();
      await expect(uiTheme.locator('option[value="travel"]')).toHaveText('Travel revised ⏾');
      await expect(uiTheme.locator('option[value="desk"]')).toHaveCount(1);
      await expect(uiTheme).toHaveValue('travel');
      await expect(page.getByTestId('settings-theme-mode')).toHaveValue('dark');
      await page.screenshot({ path: `/tmp/ao-frontend-assets-${test.info().project.name}.png` });

      await settingsPage(page, 'Working indicator');
      const animations = page.getByRole('switch', { name: 'Toggle animated spinner' });
      await animations.click();
      await writeFile(join(spinners.dir, 'travel.png'), PNG_BYTES);
      await writeFile(join(spinners.dir, 'travel.json'), JSON.stringify({ frames: 4, frameMs: 100, label: 'Travel sprite' }));
      await page.locator('[data-settings-field="spinner.copy-files"]').getByRole('button', { name: 'Copy', exact: true }).click();
      await expect(page.getByTestId('settings-spinner-pool')).toContainText('Travel sprite (custom)');
      await expect(page.getByTestId('settings-spinner-warnings')).toHaveCount(0);
      await page.reload();
      await settingsPage(page, 'Working indicator');
      await expect(page.getByTestId('settings-spinner-pool')).toContainText('Travel sprite (custom)');
      expect(errors).toEqual([]);
    } finally {
      await page.goto('about:blank');
      await harness.close();
    }
  });
}
