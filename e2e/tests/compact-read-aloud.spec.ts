// Read-aloud on the phone: the control is always shown on a finished reply
// (no hover), and it and its restart sit in Copy's grid. The preference is
// seeded on the device: the Settings switch itself is read-aloud.spec's.

import { expect, test } from './fixtures.js';
import {
  expectReadAloudInCopyGrid,
  installSpeechStub,
  openRepliesThread,
  readAloudButtons,
  readAloudSlots,
  seedReplies,
} from './read-aloud-helpers.js';

test('the controls sit in Copy’s grid on the phone', async ({ harness, page }) => {
  await installSpeechStub(page, { enabled: true });
  await seedReplies(harness);
  await openRepliesThread(harness, page);
  await expect(readAloudButtons(page)).toHaveCount(2);
  const row = page.getByTestId('assistant-message-meta').first();
  await expect(readAloudSlots(page).first().locator('..')).toHaveCSS('opacity', '1');
  await expectReadAloudInCopyGrid(row);
  await readAloudButtons(page).first().tap();
  await expect(readAloudSlots(page).first()).toHaveAttribute('data-phase', 'playing');
  await expectReadAloudInCopyGrid(row);
  await readAloudButtons(page).first().tap();
  await expect(readAloudSlots(page).first()).toHaveAttribute('data-phase', 'paused');
  await expect(page.getByTestId('assistant-message-read-aloud-restart')).toBeVisible();
  await expectReadAloudInCopyGrid(row);
});
