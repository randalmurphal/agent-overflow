// The image attachment menu under the compact layout: a long press on a
// sent image opens Copy Image / Save Image at the image without opening the
// preview, and a long press inside the open lightbox raises the menu over it
// and a tap outside dismisses only the menu. The press is raw CDP touch, so
// the app's own detector (utils/longPressContextMenu.ts) raises the
// `contextmenu`, as it does on a device.

import { expect, test, type SeedResult } from './fixtures.js';
import { PNG_BASE64, PNG_WIDTH } from './attachment-fixture.js';
import { plainScenario } from './thread-tools-helpers.js';
import { longPress } from './touch-helpers.js';

const FILENAME = 'phone-shot.png';

test.beforeEach(async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: plainScenario({
      name: 'compact-image-menu',
      provider: 'claude',
      texts: ['Image received.'],
      afterTurns: 'repeatLast',
    }),
  });
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'compact-image-app',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [
          {
            title: 'Phone image',
            turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }],
          },
        ],
      },
    ],
  });
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Phone image' }).tap();
  await page.getByTestId('composer-root').evaluate(
    (root, payload) => {
      const binary = atob(payload.base64);
      const bytes = new Uint8Array(binary.length);
      for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
      const transfer = new DataTransfer();
      transfer.items.add(new File([bytes], payload.filename, { type: 'image/png' }));
      root.dispatchEvent(new DragEvent('drop', { dataTransfer: transfer, bubbles: true, cancelable: true }));
    },
    { filename: FILENAME, base64: PNG_BASE64 },
  );
  await expect(page.getByTestId('attachment-thumb')).toBeVisible();
  await page.getByLabel('Message Input').fill('From the phone. [Image #1]');
  const completed = harness.waitForEvent('provider:turn_completed');
  await page.getByTestId('composer-send').tap();
  await completed;
});

test('a long press on a sent image opens its menu and not the preview', async ({ page }) => {
  const tile = page.getByTestId('user-message-attachments').getByLabel(`Preview ${FILENAME}`);
  const menu = page.getByRole('menu', { name: 'Image Actions' });
  await longPress(page, tile, menu);
  await expect(menu.getByRole('menuitem')).toHaveText(['Copy Image', 'Save Image']);
  // The release's compatibility click did not open the lightbox under it.
  await expect(page.getByRole('dialog', { name: FILENAME })).toHaveCount(0);
  await expect(menu).toBeVisible();
});

test('inside the lightbox, a tap outside the menu dismisses only the menu', async ({ page }) => {
  await page.getByTestId('user-message-attachments').getByLabel(`Preview ${FILENAME}`).tap();
  const dialog = page.getByRole('dialog', { name: FILENAME });
  const picture = dialog.getByRole('img', { name: FILENAME });
  await expect.poll(() => picture.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBe(PNG_WIDTH);

  const menu = page.getByRole('menu', { name: 'Image Actions' });
  await longPress(page, picture, menu);
  await expect(dialog).toBeVisible();

  // A tap on the lightbox's backdrop, well away from the menu.
  await page.touchscreen.tap(8, 8);
  await expect(menu).toHaveCount(0);
  await expect(dialog).toBeVisible();

  // With no menu up, the same tap closes the lightbox as it always has.
  await page.touchscreen.tap(8, 8);
  await expect(dialog).toHaveCount(0);
});
