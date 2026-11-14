// Read-aloud on the phone: the control is always shown on a finished reply
// (no hover), and it, the section controls while reading and the restart
// while paused sit in Copy's grid on one line inside the screen. The
// preference is seeded on the device: the Settings switch itself is
// read-aloud.spec's. While a reply is read every "read from here" button
// shows beside its section in the right gutter, clear of the text, and the
// marker is on the section being read. A phone with no voices reads
// through the computer's OpenAI voice, one request per section, to the
// clip's end, and Settings > Voice with OpenAI voices fits the screen.

import { expect, test } from './fixtures.js';
import { layoutFaults } from './settings-layout-helpers.js';
import {
  SECOND_REPLY,
  expectClusterFits,
  expectFromButtonsInGutter,
  expectMarkerOn,
  expectReadAloudInCopyGrid,
  fakeSpeechMs,
  fromOpacities,
  installSpeechStub,
  openRepliesThread,
  readAloudButtons,
  readAloudSlots,
  replyParts,
  seedReplies,
  seedSectionReplies,
  setUpOpenAIVoices,
  turnOffOpenAIVoices,
  watchSpeech,
} from './read-aloud-helpers.js';

test.afterEach(async ({ harness }) => {
  await turnOffOpenAIVoices(harness);
});

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
  await expect(row.getByTestId('assistant-message-read-aloud-next')).toBeVisible();
  await expectReadAloudInCopyGrid(row);
  await expectClusterFits(page, row);
  await readAloudButtons(page).first().tap();
  await expect(readAloudSlots(page).first()).toHaveAttribute('data-phase', 'paused');
  await expect(page.getByTestId('assistant-message-read-aloud-restart')).toBeVisible();
  await expectReadAloudInCopyGrid(row);
  await expectClusterFits(page, row);
});

test('every read-from-here button shows on the phone while a reply is read', async ({ harness, page }) => {
  await installSpeechStub(page, { enabled: true });
  await seedSectionReplies(harness);
  await openRepliesThread(harness, page);
  const reply = replyParts(page, 'Install it first.');
  await expect(page.getByTestId('assistant-message-read-aloud-from')).toHaveCount(0);
  await reply.control('').tap();
  await expectMarkerOn(reply, 0);
  await expect(reply.from).toHaveCount(2);
  await expect.poll(() => fromOpacities(reply)).toEqual({ 1: '1', 2: '1' });
  await expectFromButtonsInGutter(page, reply);
  await expect(replyParts(page, SECOND_REPLY).from).toHaveCount(0);

  await reply.fromSection(2).tap();
  await expectMarkerOn(reply, 2);
  await expect.poll(() => fromOpacities(reply)).toEqual({ 0: '1', 1: '1' });
});

test('a phone with no voices reads through the computer’s OpenAI voice to the end', async ({ harness, page }) => {
  await installSpeechStub(page, { voices: false, enabled: true });
  await setUpOpenAIVoices(harness, 'sk-e2e-read-aloud-phone');
  await seedReplies(harness);
  const speech = watchSpeech(page);
  await openRepliesThread(harness, page);
  await expect(readAloudButtons(page)).toHaveCount(2);
  const slot = readAloudSlots(page).nth(1);
  const started = Date.now();
  await readAloudButtons(page).nth(1).tap();
  await expect(slot).toHaveAttribute('data-phase', 'playing');
  await expect(slot).toHaveAttribute('data-phase', 'idle', { timeout: 20_000 });
  // One short paragraph is one section.
  expect(Date.now() - started).toBeGreaterThanOrEqual(fakeSpeechMs(SECOND_REPLY) * 0.9);
  expect(speech.minted.map((m) => m.text)).toEqual([SECOND_REPLY]);
  expect(speech.fetches()).toBe(1);
});

test('Settings > Voice with OpenAI voices fits the phone screen', async ({ harness, page }) => {
  await installSpeechStub(page, { enabled: true });
  await setUpOpenAIVoices(harness, 'sk-e2e-read-aloud-phone-settings');
  await harness.open(page);
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tablist', { name: 'Settings Sections' }).getByRole('tab', { name: 'Voice', exact: true }).click();
  await expect(page.getByTestId('voice-read-aloud-api-status')).toHaveText('Ready');
  const previews = page.getByTestId('read-aloud-openai-preview');
  await expect(previews).not.toHaveCount(0);
  await previews.last().scrollIntoViewIfNeeded();
  await expect(previews.last()).toBeInViewport();
  expect(await layoutFaults(page, 'Voice')).toEqual([]);
});
