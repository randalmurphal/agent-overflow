// Read-aloud in OpenAI voices through the shipped UI and the real backend:
// with the computer's switch on and a key in its key file, Settings > Voice
// lists its voices; a reply read in one admits each section on the
// computer (short paragraphs packed into one), fetches its WAV once from
// /attachments/speech/ and plays it in the page to its end, with a pause
// between sections (the fixtures fail a test on any CSP violation, so blob
// playback is allowed); pause holds the clip and resume continues it
// mid-clip without asking again; Preview reads a voice without changing the saved one; a
// failure shows the computer's reason; switching the computer's switch off
// ends a reading, sounding or still waiting on OpenAI, without an error,
// also when it is switched straight back on; a
// device with no voices reads through the computer; with the switch off
// nothing is offered or fetched.
// The isolated boot answers OpenAI in-process (internal/openai/openaifake).

import type { Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  FIRST_REPLY,
  SECTION_PAUSE_MS,
  audioTime,
  fakeSpeechMs,
  installSpeechStub,
  openVoiceSettings,
  readAloudButtons,
  readAloudSlots,
  setUpOpenAIVoices,
  spoken,
  trackAudio,
  turnOffOpenAIVoices,
  watchSpeech,
} from './read-aloud-helpers.js';

// FIRST_REPLY's sections: its first paragraph, then (after the code block) its last.
const FIRST_SECTIONS = ['First one. Second one.', 'Third one.'];
const LONG_REPLY = 'This first sentence is long enough to pause while it is still being read aloud. Then a short one.';
// Read aloud drops a bracketed span as a citation, so the fake's marker is
// escaped to reach the computer as text.
const MISREAD_REPLY = 'This one is answered instead of read \\[fake: answer\\].';
const STALLED_REPLY = 'OpenAI never answers this one \\[fake: stall\\].';
const REPLIES = 4;

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
          { userText: 'Stalled?', items: [{ kind: 'assistant_text', summary: STALLED_REPLY }] },
        ],
      }],
    }],
  });
}

async function openThread(harness: HarnessApp, page: Page): Promise<void> {
  await harness.open(page);
  await page.getByTestId('thread-row').click();
  await expect(page.getByTestId('assistant-message-meta')).toHaveCount(REPLIES);
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

test('a reply read in an OpenAI voice fetches each section once and plays it to its end', async ({ harness, page }) => {
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
  // The audio really played: the reading lasts the clips' length and the
  // pause between them.
  const audio = FIRST_SECTIONS.reduce((ms, s) => ms + fakeSpeechMs(s), 0);
  expect(elapsed).toBeGreaterThanOrEqual((audio + SECTION_PAUSE_MS) * 0.9);
  expect(speech.minted).toEqual(FIRST_SECTIONS.map((text) => ({ text, voice: 'marin' })));
  expect(speech.fetches()).toBe(2);
  // The device's own voice said nothing.
  expect(await spoken(page)).toEqual([]);
  await expect(readAloudButtons(page).first()).toHaveAttribute('aria-label', 'Read aloud');
});

test('pause holds the clip and resume continues it mid-clip without asking again', async ({ harness, page }) => {
  await installSpeechStub(page);
  await trackAudio(page);
  await setUpOpenAIVoices(harness, 'sk-e2e-read-aloud-pause');
  await seedReplies(harness);
  const speech = watchSpeech(page);
  await openThread(harness, page);
  await chooseVoice(page, 'openai:cedar');

  const button = readAloudButtons(page).nth(1);
  const slot = readAloudSlots(page).nth(1);
  await button.click();
  // The section is sounding once the wait is over.
  await expect(slot).toHaveAttribute('data-waiting', 'false');
  await expect.poll(() => audioTime(page)).toBeGreaterThan(0.5);
  await button.click();
  await expect(slot).toHaveAttribute('data-phase', 'paused');
  await expect(button).toHaveAttribute('aria-label', /^Resume reading/);
  const pausedAt = await audioTime(page);
  expect(speech.minted).toEqual([{ text: LONG_REPLY, voice: 'cedar' }]);
  const fetches = speech.fetches();

  await button.click();
  await expect(slot).toHaveAttribute('data-phase', 'playing');
  await expect(slot).toHaveAttribute('data-waiting', 'false');
  // It goes on from where it paused, not from the clip's start.
  expect(await audioTime(page)).toBeGreaterThanOrEqual(pausedAt);
  await expect.poll(() => audioTime(page)).toBeGreaterThan(pausedAt + 0.2);
  await expect(slot).toHaveAttribute('data-phase', 'idle', { timeout: 20_000 });
  expect(speech.minted).toEqual([{ text: LONG_REPLY, voice: 'cedar' }]);
  expect(speech.fetches()).toBe(fetches);
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
  await expect(cedar).toHaveAttribute('aria-pressed', 'true');
  await expect(cedar).toHaveAccessibleName('Stop the Cedar preview');
  // The sample plays to its end and the control returns.
  await expect(cedar).toHaveAttribute('aria-pressed', 'false', { timeout: 20_000 });
  await expect(cedar).toHaveAccessibleName('Preview Cedar');
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

test('switching the computer’s OpenAI voices off ends a reading without an error', async ({ harness, page }) => {
  await installSpeechStub(page);
  await setUpOpenAIVoices(harness, 'sk-e2e-read-aloud-revoked');
  await seedReplies(harness);
  const speech = watchSpeech(page);
  await openThread(harness, page);
  await chooseVoice(page, 'openai:marin');

  // While a section is sounding.
  const sounding = readAloudSlots(page).nth(1);
  await readAloudButtons(page).nth(1).click();
  await expect(sounding).toHaveAttribute('data-waiting', 'false');
  await expect(sounding).toHaveAttribute('data-phase', 'playing');
  await turnOffOpenAIVoices(harness);
  await expect(sounding).toHaveAttribute('data-phase', 'idle');

  // While OpenAI has yet to answer the first section.
  await setUpOpenAIVoices(harness, 'sk-e2e-read-aloud-revoked');
  await openVoiceSettings(page);
  await expect(page.getByTestId('voice-read-aloud-api-status')).toHaveText('Ready');
  await page.keyboard.press('Escape');
  const waiting = readAloudSlots(page).nth(3);
  const minted = speech.minted.length;
  await readAloudButtons(page).nth(3).click();
  await expect(waiting).toHaveAttribute('data-waiting', 'true');
  await expect.poll(() => speech.minted.length).toBe(minted + 1);
  // Off and straight back on. Whether the page hears of the switch first
  // or the refused fetch lands first, the reading ends without an error.
  await turnOffOpenAIVoices(harness);
  await setUpOpenAIVoices(harness, 'sk-e2e-read-aloud-revoked');
  await expect(waiting).toHaveAttribute('data-phase', 'idle');
  await expect(page.getByTestId('toast-message')).toHaveCount(0);

  // The voice is offered again, and a reading started now is read.
  await openVoiceSettings(page);
  await expect(page.getByTestId('voice-read-aloud-api-status')).toHaveText('Ready');
  await page.keyboard.press('Escape');
  const again = readAloudSlots(page).first();
  await readAloudButtons(page).first().click();
  await expect(again).toHaveAttribute('data-phase', 'playing');
  await expect(again).toHaveAttribute('data-phase', 'idle', { timeout: 20_000 });
  expect(speech.minted.slice(minted + 1)).toEqual(FIRST_SECTIONS.map((text) => ({ text, voice: 'marin' })));
  await expect(page.getByTestId('toast-message')).toHaveCount(0);

  // With the switch off the saved OpenAI voice is not offered, so the
  // device reads.
  await turnOffOpenAIVoices(harness);
  await openVoiceSettings(page);
  await expect(page.getByTestId('voice-read-aloud-api-status')).toHaveText('Off');
  await page.keyboard.press('Escape');
  await readAloudButtons(page).first().click();
  await expect.poll(async () => (await spoken(page)).map((u) => u.text)).toEqual(['First one.']);
  expect(speech.minted).toHaveLength(minted + 1 + FIRST_SECTIONS.length);
});

test('a device with no voices reads through the computer’s OpenAI voice', async ({ harness, page }) => {
  await installSpeechStub(page, { voices: false, enabled: true });
  await setUpOpenAIVoices(harness, 'sk-e2e-read-aloud-mute');
  await seedReplies(harness);
  const speech = watchSpeech(page);
  // Straight to the thread: the timeline asks the computer.
  await openThread(harness, page);
  await expect(readAloudButtons(page)).toHaveCount(REPLIES);
  await readAloudButtons(page).first().click();
  await expect(readAloudSlots(page).first()).toHaveAttribute('data-phase', 'idle', { timeout: 20_000 });
  expect(speech.minted.map((m) => m.text)).toEqual(FIRST_SECTIONS);
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
  await expect(mute.getByTestId('assistant-message-copy-slot').getByRole('button')).toHaveCount(REPLIES);
  await expect(readAloudButtons(mute)).toHaveCount(0);
  await mute.close();
});
