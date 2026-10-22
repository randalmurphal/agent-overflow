// Read-aloud through the shipped UI with a scripted `speechSynthesis`
// (read-aloud-helpers.ts): the control appears on finished replies once
// Settings > Voice turns it on and the device has a voice; it reads the
// reply's sentences with code skipped; pause resumes from the start of the
// sentence and restart from the first; a second reply stops the first;
// Mod+Shift+. pauses and resumes; starting dictation pauses a reading; and
// on the desktop the controls sit in Copy's grid.

import type { Page } from '@playwright/test';

import { expect, test } from './fixtures.js';
import { setUpDictation } from './dictation-helpers.js';
import {
  endCurrentUtterance,
  expectReadAloudInCopyGrid,
  installSpeechStub,
  openRepliesThread,
  readAloudButtons,
  readAloudSlots,
  seedReplies,
  spoken,
  turnOnReadAloud,
} from './read-aloud-helpers.js';

// Dictation records from the fake device (the test that pauses a reading).
test.use({
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] },
  permissions: ['microphone'],
});

const metaRows = (page: Page) => page.getByTestId('assistant-message-meta');

async function readyToRead(harness: Parameters<typeof seedReplies>[0], page: Page): Promise<void> {
  await installSpeechStub(page);
  await seedReplies(harness);
  await openRepliesThread(harness, page);
  await turnOnReadAloud(page);
  await expect(readAloudButtons(page)).toHaveCount(2);
}

test('the control appears once read-aloud is on, and not on a device without voices', async ({ harness, page }) => {
  await installSpeechStub(page);
  await seedReplies(harness);
  await openRepliesThread(harness, page);
  await expect(page.getByTestId('assistant-message-copy-slot').getByRole('button')).toHaveCount(2);
  await expect(readAloudButtons(page)).toHaveCount(0);
  await turnOnReadAloud(page);
  await expect(readAloudButtons(page)).toHaveCount(2);
  await expect(readAloudButtons(page).first()).toHaveAttribute('aria-label', 'Read aloud');

  // A device with no voices: the preference is on but nothing can speak,
  // so there is no control, and the page says why with its controls inert.
  await page.close();
  const mute = await page.context().newPage();
  await installSpeechStub(mute, { voices: false, enabled: true });
  await openRepliesThread(harness, mute);
  await expect(mute.getByTestId('assistant-message-copy-slot').getByRole('button')).toHaveCount(2);
  await expect(readAloudButtons(mute)).toHaveCount(0);
  await mute.getByTestId('sidebar-settings-button').click();
  await mute.getByRole('tab', { name: 'Voice', exact: true }).click();
  await expect(mute.getByTestId('settings-read-aloud-section')).toHaveAttribute('data-has-voices', 'false');
  await expect(mute.getByText('This device has no voices to read with.')).toBeVisible();
  await expect(mute.getByLabel('Toggle reading replies aloud')).toBeDisabled();
  await expect(mute.getByTestId('read-aloud-try')).toBeDisabled();
  await mute.close();
});

test('reads the reply sentence by sentence, skipping its code block', async ({ harness, page }) => {
  await readyToRead(harness, page);
  const button = readAloudButtons(page).first();
  await button.click();
  await expect(readAloudSlots(page).first()).toHaveAttribute('data-phase', 'playing');
  await expect(button).toHaveAttribute('aria-label', /^Pause reading/);
  await expect.poll(() => spoken(page)).toEqual([{ text: 'First one.', rate: 1, volume: 1, voice: '' }]);
  await endCurrentUtterance(page);
  await endCurrentUtterance(page);
  await expect.poll(async () => (await spoken(page)).map((u) => u.text)).toEqual(['First one.', 'Second one.', 'Third one.']);
  await endCurrentUtterance(page);
  await expect(readAloudSlots(page).first()).toHaveAttribute('data-phase', 'idle');
  await expect(button).toHaveAttribute('aria-label', 'Read aloud');
});

test('pause resumes from the start of its sentence, and restart from the first', async ({ harness, page }) => {
  await readyToRead(harness, page);
  const button = readAloudButtons(page).first();
  const slot = readAloudSlots(page).first();
  const restart = page.getByTestId('assistant-message-read-aloud-restart');
  await button.click();
  await endCurrentUtterance(page);
  await expect.poll(async () => (await spoken(page)).length).toBe(2);

  await button.click();
  await expect(slot).toHaveAttribute('data-phase', 'paused');
  await expect(button).toHaveAttribute('aria-label', /^Resume reading/);
  await expect(restart).toBeVisible();
  await expect.poll(() => page.evaluate(() => window.__aoSpeech.queued)).toBe(0);

  await button.click();
  await expect(slot).toHaveAttribute('data-phase', 'playing');
  await expect(restart).toHaveCount(0);
  await expect.poll(async () => (await spoken(page)).at(-1)?.text).toBe('Second one.');

  await button.click();
  await expect(slot).toHaveAttribute('data-phase', 'paused');
  await restart.click();
  await expect(slot).toHaveAttribute('data-phase', 'playing');
  await expect.poll(async () => (await spoken(page)).at(-1)?.text).toBe('First one.');
});

test('reading a second reply stops the first', async ({ harness, page }) => {
  await readyToRead(harness, page);
  await readAloudButtons(page).first().click();
  await expect(readAloudSlots(page).first()).toHaveAttribute('data-phase', 'playing');
  await readAloudButtons(page).nth(1).click();
  await expect(readAloudSlots(page).nth(1)).toHaveAttribute('data-phase', 'playing');
  await expect(readAloudSlots(page).first()).toHaveAttribute('data-phase', 'idle');
  await expect(readAloudButtons(page).first()).toHaveAttribute('aria-label', 'Read aloud');
  await expect.poll(async () => (await spoken(page)).map((u) => u.text)).toEqual(['First one.', 'Other reply.']);
});

test('the chord pauses and resumes the reading', async ({ harness, page }) => {
  await readyToRead(harness, page);
  const slot = readAloudSlots(page).first();
  await readAloudButtons(page).first().click();
  await expect(slot).toHaveAttribute('data-phase', 'playing');
  await page.keyboard.press('ControlOrMeta+Shift+Period');
  await expect(slot).toHaveAttribute('data-phase', 'paused');
  await page.keyboard.press('ControlOrMeta+Shift+Period');
  await expect(slot).toHaveAttribute('data-phase', 'playing');
  await expect.poll(async () => (await spoken(page)).map((u) => u.text)).toEqual(['First one.', 'First one.']);
});

test('starting dictation pauses the reading', async ({ harness, page }) => {
  await setUpDictation(harness);
  await readyToRead(harness, page);
  const slot = readAloudSlots(page).first();
  await readAloudButtons(page).first().click();
  await expect(slot).toHaveAttribute('data-phase', 'playing');
  const mic = page.getByTestId('composer-dictate');
  await mic.click();
  await expect(mic).toHaveAttribute('data-phase', 'recording');
  await expect(slot).toHaveAttribute('data-phase', 'paused');
  await page.getByLabel('Message Input').press('Escape');
  await expect(mic).toHaveAttribute('data-phase', 'idle');
  await expect(slot).toHaveAttribute('data-phase', 'paused');
});

test('the controls sit in Copy’s grid on the desktop', async ({ harness, page }) => {
  await readyToRead(harness, page);
  const row = metaRows(page).first();
  await row.hover();
  await expectReadAloudInCopyGrid(row);
  await readAloudButtons(page).first().click();
  await expectReadAloudInCopyGrid(row);
  await readAloudButtons(page).first().click();
  await expect(readAloudSlots(page).first()).toHaveAttribute('data-phase', 'paused');
  await expectReadAloudInCopyGrid(row);
});
