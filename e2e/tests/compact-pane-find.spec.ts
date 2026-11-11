// The pane find bar under the compact layout and the browser's Back (the
// ladder utils/stepBack.ts runs): with Settings opened over a thread whose
// find is open, Back closes Settings first and leaves the find open; the
// next Back closes the find, and the one after returns to the list.
// Desktop coverage: pane-find.spec.ts.
import { test, expect, type SeedResult } from './fixtures.js';

test('Back closes the overlay above an open find before the find', async ({ harness, page }) => {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'compact-find', repo: {}, threads: [
      { title: 'Phone find', provider: 'claude', turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'phone-needle answer' }] }] },
    ] }],
  });
  await harness.open(page);
  const screen = page.locator('html');
  await expect(screen).toHaveAttribute('data-compact-screen', 'list');
  await page.getByText('Phone find', { exact: true }).click();
  await expect(screen).toHaveAttribute('data-compact-screen', 'thread');

  await page.getByLabel('Message Input').click();
  await page.keyboard.press('ControlOrMeta+f');
  const bar = page.getByTestId('pane-find-bar');
  await expect(bar).toBeVisible();
  await page.keyboard.type('phone-needle');
  await expect(bar.getByTestId('pane-find-count')).toHaveText('1/1');

  // settings.open is not reachable from a text field.
  await page.getByTestId('chat-header-title').click();
  await page.keyboard.press('ControlOrMeta+,');
  const settings = page.getByTestId('settings-overlay');
  await expect(settings).toBeVisible();

  await page.goBack();
  await expect(settings).toHaveCount(0);
  await expect(bar).toBeVisible();
  await expect(screen).toHaveAttribute('data-compact-screen', 'thread');

  await page.goBack();
  await expect(bar).toHaveCount(0);
  await expect(screen).toHaveAttribute('data-compact-screen', 'thread');

  await page.goBack();
  await expect(screen).toHaveAttribute('data-compact-screen', 'list');
});
