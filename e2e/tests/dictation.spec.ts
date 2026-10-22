// Composer dictation through the shipped UI, Chromium's fake microphone and
// the real backend: the recording streams to the computer in ticketed
// pieces, the computer transcribes it with the Codex ChatGPT sign-in (the
// isolated boot answers the transcription request in-process, naming the
// bytes and type it received), and the transcript lands at the caret
// without sending.
//
// Also covered: the mic and Settings → Voice follow the off-by-default
// switch and whether Codex is signed in with ChatGPT, the chord starts it
// and Escape abandons it, a recording left running when the pane switches
// threads is dropped rather than landing in the next thread, and the mic
// sits in Send's grid.

import { rm } from 'node:fs/promises';

import type { Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { codexAuthPath, expectDesktopMicAligned, setUpDictation, signInCodexWithChatGPT } from './dictation-helpers.js';

// The fake device plays a tone, loud enough to pass the silence check.
test.use({
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] },
  permissions: ['microphone'],
});

const TRANSCRIPT = /^Please Heard \d+ bytes of audio\/webm$/;

async function seedThreads(harness: HarnessApp, titles: string[]): Promise<string[]> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'dictation-app',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: titles.map((title) => ({
          title,
          turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }],
        })),
      },
    ],
  });
  return seed.projects[0].threadIds;
}

async function openThread(harness: HarnessApp, page: Page, title: string): Promise<void> {
  await harness.open(page);
  await page.getByText(title).click();
  await expect(page.getByTestId('composer-root')).toBeVisible();
}

const mic = (page: Page) => page.getByTestId('composer-dictate');
const input = (page: Page) => page.getByLabel('Message Input');

/** Counts the recording pieces the page has uploaded so far. */
function countPieces(page: Page): () => number {
  let pieces = 0;
  page.on('request', (request) => {
    if (request.method() === 'PUT' && new URL(request.url()).pathname.startsWith('/attachments/dictation')) pieces += 1;
  });
  return () => pieces;
}

/** Records until `pieces` pieces have streamed, then stops. */
async function dictate(page: Page, uploaded: () => number, pieces = 2): Promise<void> {
  const before = uploaded();
  await mic(page).click();
  await expect(mic(page)).toHaveAttribute('data-phase', 'recording');
  await expect.poll(uploaded).toBeGreaterThanOrEqual(before + pieces);
  await mic(page).click();
}

test('dictation inserts the transcript at the caret and never sends', async ({ harness, page }) => {
  await seedThreads(harness, ['Dictate here']);
  await setUpDictation(harness);
  await openThread(harness, page, 'Dictate here');

  const uploaded = countPieces(page);
  await input(page).fill('Please');
  await expect(mic(page)).toHaveAttribute('data-phase', 'idle');
  await dictate(page, uploaded);

  await expect(input(page)).toHaveValue(TRANSCRIPT);
  await expect(mic(page)).toHaveAttribute('data-phase', 'idle');
  await expect(mic(page)).not.toHaveAttribute('data-error', 'true');
  // Every piece reached the transcription, not only the first.
  const value = await input(page).inputValue();
  expect(Number(/Heard (\d+) bytes/.exec(value)![1])).toBeGreaterThan(4000);
  // Still in the composer: dictation never sends.
  await expect(input(page)).toBeFocused();
});

test('the chord starts dictation and Escape abandons it', async ({ harness, page }) => {
  await seedThreads(harness, ['Chord here']);
  await setUpDictation(harness);
  await openThread(harness, page, 'Chord here');

  const uploaded = countPieces(page);
  await input(page).fill('Please');
  await input(page).press('ControlOrMeta+Shift+Space');
  await expect(mic(page)).toHaveAttribute('data-phase', 'recording');
  await expect.poll(uploaded).toBeGreaterThanOrEqual(1);
  await input(page).press('Escape');
  await expect(mic(page)).toHaveAttribute('data-phase', 'idle');
  await expect(input(page)).toHaveValue('Please');

  // The next recording's transcript is the only one that lands.
  await dictate(page, uploaded);
  await expect(input(page)).toHaveValue(TRANSCRIPT);
});

test('a recording does not follow the pane to another thread', async ({ harness, page }) => {
  await seedThreads(harness, ['First thread', 'Second thread']);
  await setUpDictation(harness);
  await openThread(harness, page, 'First thread');
  const uploaded = countPieces(page);

  await mic(page).click();
  await expect(mic(page)).toHaveAttribute('data-phase', 'recording');
  await expect.poll(uploaded).toBeGreaterThanOrEqual(1);
  await page.getByText('Second thread').click();
  await expect(mic(page)).toHaveAttribute('data-phase', 'idle');

  // Dictating here lands this thread's transcript and nothing else.
  await input(page).fill('Please');
  await dictate(page, uploaded);
  await expect(input(page)).toHaveValue(TRANSCRIPT);
  await page.getByText('First thread').click();
  await expect(input(page)).toHaveValue('');
});

const voiceSettings = async (page: Page) => {
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tab', { name: 'Voice', exact: true }).click();
};
const voiceStatus = (page: Page) => page.getByTestId('voice-dictation-status');
const voiceSwitch = (page: Page) => page.getByRole('switch', { name: 'Toggle dictation with ChatGPT' });

test('the mic shows only while dictation is on and Codex is signed in with ChatGPT', async ({ harness, page }) => {
  await seedThreads(harness, ['No mic here']);
  await rm(codexAuthPath(harness), { force: true });
  // The worker's reset keeps host settings, and earlier tests turned the
  // switch on; its default is pinned by the settings defaults test.
  await harness.rpc('UpdateSettings', { dictationSubscriptionEnabled: false, dictationAPIEnabled: false });
  await openThread(harness, page, 'No mic here');
  await expect(page.getByTestId('composer-send')).toBeVisible();
  await expect(mic(page)).toHaveCount(0);

  await voiceSettings(page);
  await expect(voiceSwitch(page)).toHaveAttribute('aria-checked', 'false');
  await expect(voiceStatus(page)).toHaveText('Off');
  await voiceSwitch(page).click();
  await expect(voiceStatus(page)).toHaveText('Codex is not signed in with ChatGPT');
  await page.keyboard.press('Escape');
  await expect(mic(page)).toHaveCount(0);

  await signInCodexWithChatGPT(harness);
  await page.reload();
  await expect(mic(page)).toBeVisible();
  await voiceSettings(page);
  await expect(voiceStatus(page)).toHaveText('Ready');
  await expect(page.getByTestId('voice-dictation-disclosure')).toContainText('up to 30 days');

  // Turning it off takes the mic away from the open composer.
  await voiceSwitch(page).click();
  await expect(voiceStatus(page)).toHaveText('Off');
  await page.keyboard.press('Escape');
  await expect(mic(page)).toHaveCount(0);
});

test('the desktop mic is in the toolbar, in Send\'s grid, at wide and narrow widths', async ({ harness, page }) => {
  await seedThreads(harness, ['Aligned here']);
  await setUpDictation(harness);
  await openThread(harness, page, 'Aligned here');
  const toolbar = page.getByTestId('composer-toolbar');
  const label = page.getByTestId('composer-model-menu-trigger').locator(':scope > span.truncate');
  for (const width of [1280, 700]) {
    await page.setViewportSize({ width, height: 800 });
    // A pane keeps its minimum width, so a narrow window alone scrolls the
    // strip rather than narrowing the composer; folding the sidebar gives
    // the pane the whole 700px and the toolbar a width it must ladder for.
    if (width === 700) await page.getByTestId('sidebar-collapse-toggle').click();
    await expect.poll(() => label.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
    await expect.poll(() => toolbar.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
    await expectDesktopMicAligned(page);
    await input(page).fill('First line of a longer message that wraps and keeps going\nsecond line\nthird line\nfourth line');
    await expectDesktopMicAligned(page);
    await input(page).fill('');
  }
});
