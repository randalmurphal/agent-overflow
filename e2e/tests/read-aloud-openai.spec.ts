// Read-aloud in OpenAI voices through the shipped UI and the real backend:
// with the computer's switch on and a key in its key file, Settings > Voice
// lists its voices; a reply read in one admits each sentence on the
// computer, fetches its WAV once from /attachments/speech/ and plays it in
// the page to its end (the fixtures fail a test on any CSP violation, so
// blob playback is allowed); pause and resume refetch the sentence it
// paused in; Preview reads a voice without changing the saved one; a
// failure shows the computer's reason; a device with no voices reads
// through the computer; with the switch off nothing is offered or fetched.
// The isolated boot answers OpenAI in-process (internal/openai/openaifake).

import type { Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  FIRST_REPLY,
  fakeSpeechMs,
  installSpeechStub,
  openVoiceSettings,
  readAloudButtons,
  readAloudSlots,
  setUpOpenAIVoices,
  spoken,
  turnOffOpenAIVoices,
  watchSpeech,
} from './read-aloud-helpers.js';

const FIRST_SENTENCES = ['First one.', 'Second one.', 'Third one.'];
const LONG_REPLY = 'This first sentence is long enough to pause while it is still being read aloud. Then a short one.';
// Read aloud drops a bracketed span as a citation, so the fake's marker is
// escaped to reach the computer as text.
const MISREAD_REPLY = 'This one is answered instead of read \\[fake: answer\\].';

async function seedReplies(harness: HarnessApp): Promise<void> {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'read-aloud-openai-app',
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
      threads: [{
        title: 'Read this in OpenAI voices',
        turns: [
          { userText: 'First?', items: [{ kind: 'assistant_text', summary: FIRST_REPLY }] },
          { userText: 'Long?', items: [{ kind: 'assistant_text', summary: LONG_REPLY }] },
          { userText: 'Misread?', items: [{ kind: 'assistant_text', summary: MISREAD_REPLY }] },
        ],
      }],
    }],
  });
}

async function openThread(harness: HarnessApp, page: Page): Promise<void> {
  await harness.open(page);
  await page.getByTestId('thread-row').click();
  await expect(page.getByTestId('assistant-message-meta')).toHaveCount(3);
}

/** Saved voice `voiceId`, chosen in Settings > Voice. */
async function chooseVoice(page: Page, voiceId: string): Promise<void> {
  await openVoiceSettings(page);
  const select = page.getByTestId('read-aloud-voice-select');
  await expect(select.locator('optgroup[label="OpenAI voices on this computer"] option')).not.toHaveCount(0);
  await select.selectOption(voiceId);
  await page.keyboard.press('Escape');
}

test.afterEach(async ({ harness }) => {
  await turnOffOpenAIVoices(harness);
});

test('a reply read in an OpenAI voice fetches each sentence once and plays it to its end', async ({ harness, page }) => {
  await installSpeechStub(page);
  await setUpOpenAIVoices(harness, 'sk-e2e-read-aloud-main');
  await seedReplies(harness);
  const speech = watchSpeech(page);
  await openThread(harness, page);
  await chooseVoice(page, 'openai:marin');
  await expect(page.getByTestId('settings-read-aloud-section')).toHaveCount(0);

  const slot = readAloudSlots(page).first();
  const started = Date.now();
  await readAloudButtons(page).first().click();
  await expect(slot).toHaveAttribute('data-phase', 'playing');
  await expect(slot).toHaveAttribute('data-phase', 'idle', { timeout: 20_000 });
  const elapsed = Date.now() - started;
  // The audio really played: the reading lasts the clips' length.
  expect(elapsed).toBeGreaterThanOrEqual(FIRST_SENTENCES.reduce((ms, s) => ms + fakeSpeechMs(s), 0) * 0.9);
  expect(speech.minted).toEqual(FIRST_SENTENCES.map((text) => ({ text, voice: 'marin' })));
  expect(speech.fetches()).toBe(3);
  // The device's own voice said nothing.
  expect(await spoken(page)).toEqual([]);
  await expect(readAloudButtons(page).first()).toHaveAttribute('aria-label', 'Read aloud');
});

test('pause and resume read on from the sentence it paused in', async ({ harness, page }) => {
  await installSpeechStub(page);
  await setUpOpenAIVoices(harness, 'sk-e2e-read-aloud-pause');
  await seedReplies(harness);
  const speech = watchSpeech(page);
  await openThread(harness, page);
  await chooseVoice(page, 'openai:cedar');

  const button = readAloudButtons(page).nth(1);
  const slot = readAloudSlots(page).nth(1);
  await button.click();
  // The first sentence is sounding once the wait is over.
  await expect(slot).toHaveAttribute('data-waiting', 'false');
  await expect(slot).toHaveAttribute('data-phase', 'playing');
  await button.click();
  await expect(slot).toHaveAttribute('data-phase', 'paused');
  await expect(button).toHaveAttribute('aria-label', /^Resume reading/);
  const before = speech.minted.length;

  await button.click();
  await expect(slot).toHaveAttribute('data-phase', 'playing');
  await expect(slot).toHaveAttribute('data-phase', 'idle', { timeout: 20_000 });
  const [first, second] = LONG_REPLY.split(/(?<=\.) /);
  expect(speech.minted.slice(before)).toEqual([{ text: first, voice: 'cedar' }, { text: second, voice: 'cedar' }]);
});

test('Preview reads a voice through the computer without changing the saved voice', async ({ harness, page }) => {
  await installSpeechStub(page);
  await setUpOpenAIVoices(harness, 'sk-e2e-read-aloud-preview');
  const speech = watchSpeech(page);
  await harness.open(page);
  await openVoiceSettings(page);
  const select = page.getByTestId('read-aloud-voice-select');
  await expect(select).toHaveValue('');
  await expect(page.getByTestId('voice-read-aloud-api-status')).toHaveText('Ready');

  const previews = page.getByTestId('read-aloud-openai-preview');
  const cedar = previews.and(page.locator('[data-voice="cedar"]'));
  await cedar.click();
  await expect(cedar).toHaveText('Stop');
  // The sample plays to its end and the control returns.
  await expect(cedar).toHaveText('Preview', { timeout: 20_000 });
  expect(speech.minted).toEqual([{ text: 'This is how replies will sound when they are read aloud.', voice: 'cedar' }]);
  expect(speech.fetches()).toBe(1);
  await expect(select).toHaveValue('');
  expect(await page.evaluate(() => JSON.parse(localStorage.getItem('agent-overflow:frontend:readAloud') ?? '{}').voiceId)).toBe('');
  expect(await spoken(page)).toEqual([]);

  // Try it reads the saved voice: this device's.
  await page.getByTestId('read-aloud-try').click();
  await expect.poll(async () => (await spoken(page)).length).toBe(1);
  expect(speech.minted).toHaveLength(1);
});

test('a reading OpenAI cannot do stops with the computer’s reason', async ({ harness, page }) => {
  await installSpeechStub(page);
  await setUpOpenAIVoices(harness, 'sk-e2e-read-aloud-misread');
  await seedReplies(harness);
  const speech = watchSpeech(page);
  await openThread(harness, page);
  await chooseVoice(page, 'openai:marin');

  const slot = readAloudSlots(page).nth(2);
  await readAloudButtons(page).nth(2).click();
  await expect(page.getByTestId('toast-message')).toHaveText('Read aloud stopped: OpenAI did not read the text as written', { timeout: 20_000 });
  await expect(slot).toHaveAttribute('data-phase', 'idle');
  expect(speech.minted).toEqual([{ text: 'This one is answered instead of read [fake: answer].', voice: 'marin' }]);
});

test('a device with no voices reads through the computer’s OpenAI voice', async ({ harness, page }) => {
  await installSpeechStub(page, { voices: false, enabled: true });
  await setUpOpenAIVoices(harness, 'sk-e2e-read-aloud-mute');
  await seedReplies(harness);
  const speech = watchSpeech(page);
  // Straight to the thread: the timeline asks the computer.
  await openThread(harness, page);
  await expect(readAloudButtons(page)).toHaveCount(3);
  await readAloudButtons(page).first().click();
  await expect(readAloudSlots(page).first()).toHaveAttribute('data-phase', 'idle', { timeout: 20_000 });
  expect(speech.minted.map((m) => m.text)).toEqual(FIRST_SENTENCES);
});

test('with the switch off no OpenAI voice is offered and nothing is fetched', async ({ harness, page }) => {
  await installSpeechStub(page);
  await setUpOpenAIVoices(harness, 'sk-e2e-read-aloud-off');
  await turnOffOpenAIVoices(harness);
  await seedReplies(harness);
  const speech = watchSpeech(page);
  await openThread(harness, page);
  await openVoiceSettings(page);
  await expect(page.getByTestId('voice-read-aloud-api-status')).toHaveText('Off');
  await expect(page.getByTestId('read-aloud-voice-select').locator('optgroup')).toHaveCount(0);
  await expect(page.getByTestId('read-aloud-openai-previews')).toHaveCount(0);
  await page.keyboard.press('Escape');

  await readAloudButtons(page).first().click();
  await expect.poll(async () => (await spoken(page)).map((u) => u.text)).toEqual(['First one.']);
  expect(speech.minted).toEqual([]);
  expect(speech.fetches()).toBe(0);

  // A device without voices offers no control at all.
  await page.close();
  const mute = await page.context().newPage();
  await installSpeechStub(mute, { voices: false, enabled: true });
  await openThread(harness, mute);
  await expect(mute.getByTestId('assistant-message-copy-slot').getByRole('button')).toHaveCount(3);
  await expect(readAloudButtons(mute)).toHaveCount(0);
  await mute.close();
});
