import { expect, test, type Page } from '@playwright/test';
import { writeFile } from 'node:fs/promises';
import { join, relative, isAbsolute } from 'node:path';
import { launchHarness } from '../src/harness.js';
import { confirmOnHost, mintInvite, nonLoopbackIPv4, redeemOnScreen } from './offhost-helpers.js';
import { PNG_BYTES } from './attachment-fixture.js';

/**
 * A conforming notification cue: the 44-byte canonical header plus silence.
 * Built here rather than committed so the fixture states the exact shape
 * internal/soundlib/wav.go accepts, and a change to that shape breaks this
 * test rather than being papered over by a stale binary.
 */
function canonicalCue(sampleBytes: number): Buffer {
  const out = Buffer.alloc(44 + sampleBytes);
  out.write('RIFF', 0, 'ascii');
  out.writeUInt32LE(out.length - 8, 4);
  out.write('WAVE', 8, 'ascii');
  out.write('fmt ', 12, 'ascii');
  out.writeUInt32LE(16, 16);
  out.writeUInt16LE(1, 20); // PCM
  out.writeUInt16LE(1, 22); // mono
  out.writeUInt32LE(44100, 24);
  out.writeUInt32LE(88200, 28); // byte rate
  out.writeUInt16LE(2, 32); // block align
  out.writeUInt16LE(16, 34); // bits per sample
  out.write('data', 36, 'ascii');
  out.writeUInt32LE(sampleBytes, 40);
  return out;
}

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

      // Cues are NOT copied to the device the way themes and spinners are:
      // the library belongs to the backend, and the paired screen offers
      // exactly what that host holds.
      const sounds = await harness.rpc<{ dir: string }>('GetSoundFiles');
      await writeFile(join(sounds.dir, 'desk-bell.wav'), canonicalCue(4410));
      await settingsPage(page, 'Notifications');
      const cue = page.getByTestId('settings-sound-cue-turn-complete');
      await expect(cue.locator('optgroup[label="Custom"] option[value="custom:desk-bell"]')).toHaveText('desk-bell');
      await expect(page.getByTestId('settings-sound-warnings')).toHaveCount(0);
      await cue.selectOption('custom:desk-bell');

      // A file that is not a cue is refused by the same validator on every
      // listing, and says so once, by name, instead of being played.
      await writeFile(join(sounds.dir, 'broken.wav'), Buffer.from('not a wav at all'));
      const warnings = page.getByTestId('settings-sound-warnings');
      await expect(warnings.locator('li')).toHaveCount(1);
      await expect(warnings).toContainText('broken.wav');
      await expect(cue).toHaveValue('custom:desk-bell');
      expect(errors).toEqual([]);
    } finally {
      await page.goto('about:blank');
      await harness.close();
    }
  });
}
