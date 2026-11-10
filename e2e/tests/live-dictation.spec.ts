// Live dictation through the shipped UI, Chromium's fake microphone and the
// real backend: the composer captures 24 kHz PCM in an AudioWorklet, streams
// it to the computer in appends, the computer relays it to OpenAI's live
// transcription with the key from its owner-only key file (the isolated boot
// answers in-process, naming the bytes it heard), and the transcript lands
// at the caret without sending. Where both sources are on, live wins and no
// recording is uploaded.

import { readFile, rm, writeFile } from 'node:fs/promises';
import path from 'node:path';

import type { Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { composerBoxes, expectCaptionInComposerGrid, openAIKeyPath, setUpDictation, writeOpenAIKey } from './dictation-helpers.js';

// The fake device plays a tone, loud enough to pass the silence check.
test.use({
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] },
  permissions: ['microphone'],
});

const TRANSCRIPT = /^Please Heard (\d+) bytes of live audio$/;

/** AppendLiveDictation's stable method id (frontend/bindings). */
const APPEND_LIVE_DICTATION = 2562809680;

/** One second of 24 kHz 16-bit PCM. */
const PCM_BYTES_PER_SECOND = 48_000;

async function seedThread(harness: HarnessApp, title: string): Promise<void> {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'live-dictation-app',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
}

async function openThread(harness: HarnessApp, page: Page, title: string): Promise<void> {
  await harness.open(page);
  await page.getByText(title).click();
  await expect(page.getByTestId('composer-root')).toBeVisible();
}

const mic = (page: Page) => page.getByTestId('composer-dictate');
const input = (page: Page) => page.getByLabel('Message Input');
const caption = (page: Page) => page.getByTestId('composer-dictation-caption');
const captionText = (page: Page) => page.getByTestId('composer-dictation-caption-text');

/** Bytes of PCM the page has sent in live appends so far, read from the
 * transport's RPC frames: each AppendLiveDictation call carries its audio
 * as base64, its third argument. */
function countLiveAudio(page: Page): () => number {
  let bytes = 0;
  page.on('websocket', (socket) => {
    socket.on('framesent', ({ payload }) => {
      if (typeof payload !== 'string' || !payload.startsWith('{')) return;
      const frame = JSON.parse(payload) as { type?: string; methodId?: number; params?: unknown[] };
      if (frame.type !== 'rpc' || frame.methodId !== APPEND_LIVE_DICTATION) return;
      bytes += Buffer.from(String(frame.params?.[2]), 'base64').length;
    });
  });
  return () => bytes;
}

/** Counts recordings uploaded for the subscription source. */
function countUploads(page: Page): () => number {
  let pieces = 0;
  page.on('request', (request) => {
    if (request.method() === 'PUT' && new URL(request.url()).pathname.startsWith('/attachments/dictation')) pieces += 1;
  });
  return () => pieces;
}

async function liveOnly(harness: HarnessApp): Promise<void> {
  await writeOpenAIKey(harness);
  await harness.rpc('UpdateSettings', { dictationSubscriptionEnabled: false, dictationAPIEnabled: true });
}

test('live dictation streams PCM as it records and inserts the transcript', async ({ harness, page }) => {
  await seedThread(harness, 'Dictate live');
  // Both sources on: live wins, and nothing is uploaded for the other.
  await setUpDictation(harness);
  await writeOpenAIKey(harness);
  await harness.rpc('UpdateSettings', { dictationAPIEnabled: true });
  // Before the page opens its socket, which is reported only once.
  const streamed = countLiveAudio(page);
  const uploads = countUploads(page);
  await openThread(harness, page, 'Dictate live');

  await input(page).fill('Please');
  await mic(page).click();
  await expect(mic(page)).toHaveAttribute('data-phase', 'recording');
  // Audio reaches the computer while recording, not at stop. The fake
  // device beeps about once a second, so two seconds hold a beep loud
  // enough for the silence check.
  await expect.poll(streamed, { timeout: 15_000 }).toBeGreaterThanOrEqual(PCM_BYTES_PER_SECOND * 2);
  await mic(page).click();

  await expect(mic(page)).toHaveAttribute('data-phase', 'idle');
  // The title carries the failure when there is one.
  await expect(mic(page)).toHaveAttribute('title', /^Dictate/);
  await expect(input(page)).toHaveValue(TRANSCRIPT);
  // The computer heard everything the page sent, the flushed tail included,
  // and nothing else: whole 16-bit samples at 24 kHz.
  const heard = Number(TRANSCRIPT.exec(await input(page).inputValue())![1]);
  expect(heard).toBe(streamed());
  expect(heard % 2).toBe(0);
  expect(uploads()).toBe(0);
  await expect(input(page)).toBeFocused();
});

test('the live mic follows the switch and the key file', async ({ harness, page }) => {
  await seedThread(harness, 'Key file');
  await harness.rpc('UpdateSettings', { dictationSubscriptionEnabled: false, dictationAPIEnabled: true });
  await rm(openAIKeyPath(harness), { force: true });
  await openThread(harness, page, 'Key file');
  await expect(page.getByTestId('composer-send')).toBeVisible();
  await expect(mic(page)).toHaveCount(0);

  // The key is added outside the app; coming back to the window finds it.
  await writeOpenAIKey(harness);
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await expect(mic(page)).toBeVisible();

  await harness.rpc('UpdateSettings', { dictationAPIEnabled: false });
  await expect(mic(page)).toHaveCount(0);
});

test('switching live off mid-recording stops the stream even with the other source on', async ({ harness, page }) => {
  await seedThread(harness, 'Switch off live');
  await setUpDictation(harness);
  await writeOpenAIKey(harness);
  await harness.rpc('UpdateSettings', { dictationAPIEnabled: true });
  const streamed = countLiveAudio(page);
  const uploads = countUploads(page);
  await openThread(harness, page, 'Switch off live');

  await mic(page).click();
  await expect(mic(page)).toHaveAttribute('data-phase', 'recording');
  await expect.poll(streamed, { timeout: 15_000 }).toBeGreaterThan(0);
  await harness.rpc('UpdateSettings', { dictationAPIEnabled: false });

  // The recording ends with no error to show, whether the switch's event or
  // the computer's refusal of the next piece reaches the page first, and
  // the mic stays for the other source.
  await expect(mic(page)).toHaveAttribute('data-phase', 'idle');
  await expect(mic(page)).not.toHaveAttribute('data-error', 'true');
  const stopped = streamed();
  // A second of capture would add 48 KB if anything still streamed.
  await page.waitForTimeout(1000);
  expect(streamed()).toBe(stopped);
  expect(uploads()).toBe(0);
  await expect(input(page)).toHaveValue('');
  await expect(mic(page)).toBeVisible();
});

test('a recording the computer ended for an unannounced switch-off stops without an error', async ({ harness, page }) => {
  await seedThread(harness, 'Hand edit');
  await liveOnly(harness);
  const streamed = countLiveAudio(page);
  await openThread(harness, page, 'Hand edit');

  await mic(page).click();
  await expect(mic(page)).toHaveAttribute('data-phase', 'recording');
  await expect.poll(streamed, { timeout: 15_000 }).toBeGreaterThan(0);
  // Off by hand in the host settings file: no write, so no event reaches
  // the page and only the computer's refusal of the next piece, with its
  // wire code, can stop the recording.
  const file = path.join(harness.bootstrap.dataDir, 'settings.json');
  const settings = JSON.parse(await readFile(file, 'utf8')) as Record<string, unknown>;
  delete settings.dictationAPIEnabled;
  await writeFile(file, JSON.stringify(settings));

  await expect(mic(page)).toHaveAttribute('data-phase', 'idle');
  await expect(mic(page)).not.toHaveAttribute('data-error', 'true');
  await expect(input(page)).toHaveValue('');
});

test('a refused key ends the recording with the refusal', async ({ harness, page }) => {
  await seedThread(harness, 'Refused key');
  await liveOnly(harness);
  await openThread(harness, page, 'Refused key');
  await expect(mic(page)).toBeVisible();
  await writeOpenAIKey(harness, 'sk-fake-refused-key');

  await mic(page).click();
  // The connect fails while recording; the recording ends on its own.
  await expect(mic(page)).toHaveAttribute('data-error', 'true');
  await expect(mic(page)).toHaveAttribute('data-phase', 'idle');
  const title = await mic(page).getAttribute('title');
  expect(title).toMatch(/refused/i);
  expect(title).not.toContain('sk-fake');
  await expect(input(page)).toHaveValue('');
});

test('the caption shows what is heard above the composer while recording, and goes with the recording', async ({ harness, page }) => {
  await seedThread(harness, 'Caption here');
  await liveOnly(harness);
  const streamed = countLiveAudio(page);
  await openThread(harness, page, 'Caption here');
  await input(page).fill('First line\nsecond line');
  await expect(caption(page)).toHaveCount(0);
  const before = await composerBoxes(page);

  await mic(page).click();
  await expect(mic(page)).toHaveAttribute('data-phase', 'recording');
  // The computer answers each append with the text heard so far; the fake
  // says "Hearing" once it has a beep and a dot per piece after.
  await expect(captionText(page)).toContainText('Hearing', { timeout: 15_000 });
  await expectCaptionInComposerGrid(page, before);
  // Enough audio for a transcript, as the first test waits for.
  await expect.poll(streamed, { timeout: 15_000 }).toBeGreaterThanOrEqual(PCM_BYTES_PER_SECOND * 2);
  await mic(page).click();
  await expect(mic(page)).toHaveAttribute('data-phase', 'idle');
  await expect(caption(page)).toHaveCount(0);
  await expect(input(page)).toHaveValue(/Heard \d+ bytes of live audio/);
});

test('Settings > Voice offers the live source with its key file and fields', async ({ harness, page }) => {
  await seedThread(harness, 'Live settings');
  await liveOnly(harness);
  await harness.open(page);
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tab', { name: 'Voice', exact: true }).click();
  const liveSwitch = page.getByRole('switch', { name: 'Toggle live dictation with an OpenAI API key' });
  const liveStatus = page.getByTestId('voice-dictation-live-status');
  await expect(liveSwitch).toHaveAttribute('aria-checked', 'true');
  await expect(liveStatus).toHaveText('Ready');
  // The ChatGPT row reads its own source, not the live one.
  await expect(page.getByTestId('voice-dictation-status')).toHaveText('Off');
  await expect(page.getByTestId('voice-dictation-live-key-path')).toHaveText(openAIKeyPath(harness));
  await expect(page.getByTestId('voice-dictation-live-key-line')).toHaveText('Key present.');
  await expect(page.getByTestId('voice-dictation-live-key-edit')).toBeVisible();
  await expect(page.getByTestId('voice-dictation-live-disclosure')).toContainText('abuse monitoring');

  // The fields save through the computer, which refuses what it cannot use
  // and says why.
  await page.getByTestId('voice-dictation-live-delay').selectOption('low');
  await expect.poll(async () => (await harness.rpc<{ dictationAPIDelay: string }>('GetSettings')).dictationAPIDelay).toBe('low');
  await page.getByTestId('voice-dictation-live-languages').fill('en, not-a-language');
  await page.getByTestId('voice-dictation-live-languages').press('Tab');
  const refusal = page.getByTestId('toast').filter({ hasText: 'Failed to save setting' });
  await expect(refusal).toBeVisible();
  await refusal.getByTestId('error-details-toggle').click();
  await expect(refusal.getByTestId('error-details')).toContainText('is not a language code');
  // Refused as a whole: nothing from that write was kept (an empty list is omitted).
  expect((await harness.rpc<{ dictationAPILanguages?: string[] }>('GetSettings')).dictationAPILanguages ?? []).toEqual([]);
  await page.getByTestId('voice-dictation-live-languages').fill('en, fr');
  await page.getByTestId('voice-dictation-live-languages').press('Tab');
  await expect.poll(async () => (await harness.rpc<{ dictationAPILanguages: string[] }>('GetSettings')).dictationAPILanguages).toEqual(['en', 'fr']);

  // The key is removed outside the app; coming back to the window sees it.
  await rm(openAIKeyPath(harness), { force: true });
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await expect(page.getByTestId('voice-dictation-live-key-line')).toHaveText('No key file yet.');
  await expect(liveStatus).toHaveText('No key file yet.');

  await liveSwitch.click();
  await expect(liveStatus).toHaveText('Off');
  await expect(page.getByTestId('voice-dictation-live-delay')).toHaveCount(0);
});
