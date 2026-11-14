// Reading a reply by section through the shipped UI, in an OpenAI voice
// (the in-process fake, 40 ms of audio per character) and in a scripted
// device voice: each section is one request with a pause between them;
// the marker rule follows the section being read and lines up with its
// blocks in the gutter left of the text; next, previous, back 10 seconds
// and "read from here" move the reading; the "read from here" buttons sit
// in the right gutter and exist only on the reply being read, each shown
// only while the pointer is over its section; a device voice moves by
// section and repeats its sentence; a paragraph split for length has one
// "read from here" button; and starting, moving, pausing and
// stopping a reading changes neither the reply row's height nor the scroll
// position.

import type { Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import {
  SECOND_REPLY,
  SECTION_PAUSE_MS,
  SECTION_SENTENCES,
  SECTION_TEXTS,
  audioTime,
  endCurrentUtterance,
  expectFromButtonsInGutter,
  expectMarkerOn,
  fakeSpeechMs,
  fromOpacities,
  installSpeechStub,
  openVoiceSettings,
  replyParts,
  seedSectionReplies,
  setUpOpenAIVoices,
  spoken,
  trackAudio,
  turnOffOpenAIVoices,
  watchSpeech,
} from './read-aloud-helpers.js';

test.afterEach(async ({ harness }) => {
  await turnOffOpenAIVoices(harness);
});

async function openThread(harness: HarnessApp, page: Page, replies = 2): Promise<void> {
  await harness.open(page);
  await page.getByTestId('thread-row').click();
  await expect(page.getByTestId('assistant-message-meta')).toHaveCount(replies);
}

async function readyOpenAI(harness: HarnessApp, page: Page, key: string) {
  await installSpeechStub(page, { enabled: true });
  await trackAudio(page);
  await setUpOpenAIVoices(harness, key);
  await seedSectionReplies(harness);
  const speech = watchSpeech(page);
  await openThread(harness, page);
  await openVoiceSettings(page);
  const select = page.getByTestId('read-aloud-voice-select');
  await expect(select.locator('optgroup[label="OpenAI voices on this computer"] option')).not.toHaveCount(0);
  await select.selectOption('openai:marin');
  await page.keyboard.press('Escape');
  return speech;
}

test('an OpenAI voice reads one request per section, pauses between them, and the marker follows', async ({ harness, page }) => {
  const speech = await readyOpenAI(harness, page, 'sk-e2e-read-aloud-sections');
  const reply = replyParts(page, 'Install it first.');
  await expect(reply.layer).toHaveCount(0);

  const started = Date.now();
  await reply.control('').click();
  // The marker shows while the first clip is fetched.
  await expectMarkerOn(reply, 0);
  await expect(reply.slot).toHaveAttribute('data-waiting', 'false');
  await expectMarkerOn(reply, 1);
  await expectMarkerOn(reply, 2);
  await expect(reply.slot).toHaveAttribute('data-phase', 'idle', { timeout: 20_000 });
  const elapsed = Date.now() - started;
  await expect(reply.layer).toHaveCount(0);

  expect(speech.minted).toEqual(SECTION_TEXTS.map((text) => ({ text, voice: 'marin' })));
  expect(speech.fetches()).toBe(3);
  const audio = SECTION_TEXTS.reduce((ms, text) => ms + fakeSpeechMs(text), 0);
  expect(elapsed).toBeGreaterThanOrEqual((audio + 2 * SECTION_PAUSE_MS) * 0.9);
});

test('next, previous, back 10 seconds and read from here move an OpenAI reading', async ({ harness, page }) => {
  const speech = await readyOpenAI(harness, page, 'sk-e2e-read-aloud-sections-move');
  const reply = replyParts(page, 'Install it first.');
  await reply.control('').click();
  await expect(reply.slot).toHaveAttribute('data-waiting', 'false');
  await expect(reply.control('-back')).toHaveAttribute('aria-label', 'Back 10 seconds');

  await reply.control('-next').click();
  await expectMarkerOn(reply, 1);
  await expect(reply.slot).toHaveAttribute('data-waiting', 'false');
  await reply.control('-previous').click();
  await expectMarkerOn(reply, 0);
  await expect(reply.slot).toHaveAttribute('data-waiting', 'false');

  // Read from here, on the last section: revealed by the pointer.
  await reply.blocks.nth(5).hover();
  await expect(reply.fromSection(2)).toBeVisible();
  await reply.fromSection(2).click();
  await expectMarkerOn(reply, 2);
  await expect(reply.slot).toHaveAttribute('data-waiting', 'false');
  await expect(reply.control('-next')).toBeDisabled();

  // Back 10 seconds from more than a second in goes to the clip's start.
  // Back moves within the clip it holds, also while paused: from more
  // than a second in it goes to the clip's start, and nothing is asked
  // for again.
  await expect.poll(() => audioTime(page), { intervals: [50] }).toBeGreaterThan(1);
  await reply.control('').click();
  await expect(reply.slot).toHaveAttribute('data-phase', 'paused');
  expect(await audioTime(page)).toBeGreaterThan(1);
  const minted = speech.minted.length;
  await reply.control('-back').click();
  expect(await audioTime(page)).toBe(0);
  await expect(reply.slot).toHaveAttribute('data-phase', 'paused');
  await reply.control('').click();
  await expect(reply.slot).toHaveAttribute('data-phase', 'playing');
  await expect(reply.marker).toHaveAttribute('data-section', '2');
  await expect(reply.slot).toHaveAttribute('data-phase', 'idle', { timeout: 20_000 });
  expect(speech.minted).toHaveLength(minted);
  expect(speech.minted.every((m) => SECTION_TEXTS.includes(m.text))).toBe(true);
});

test('the read-from-here buttons exist only on the reply being read, each shown only on hover', async ({ harness, page }) => {
  await installSpeechStub(page, { enabled: true });
  await seedSectionReplies(harness);
  await openThread(harness, page);
  const reply = replyParts(page, 'Install it first.');
  const other = replyParts(page, SECOND_REPLY);

  // Idle replies: hovering adds nothing.
  await reply.blocks.nth(3).hover();
  await other.body.hover();
  await expect(page.getByTestId('assistant-message-read-aloud-sections')).toHaveCount(0);
  await expect(page.getByTestId('assistant-message-read-aloud-from')).toHaveCount(0);

  await reply.control('').click();
  await expect(reply.slot).toHaveAttribute('data-phase', 'playing');
  await expectMarkerOn(reply, 0);
  await expect(reply.from).toHaveCount(2);
  await other.body.hover();
  await expect(other.layer).toHaveCount(0);
  await expect(other.from).toHaveCount(0);
  await expect.poll(() => fromOpacities(reply)).toEqual({ 1: '0', 2: '0' });

  await reply.blocks.nth(3).hover();
  await expect.poll(() => fromOpacities(reply)).toEqual({ 1: '1', 2: '0' });
  await reply.blocks.nth(4).hover();
  await expect.poll(() => fromOpacities(reply)).toEqual({ 1: '0', 2: '1' });
  // The current section has no button and shows none.
  await reply.blocks.nth(0).hover();
  await expect.poll(() => fromOpacities(reply)).toEqual({ 1: '0', 2: '0' });
  // A button keeps itself shown while the pointer is on it.
  await reply.fromSection(1).hover();
  await expect.poll(() => fromOpacities(reply)).toEqual({ 1: '1', 2: '0' });
  await expectFromButtonsInGutter(page, reply);

  // Paused, the marker dims and the buttons stay.
  await reply.control('').click();
  await expect(reply.slot).toHaveAttribute('data-phase', 'paused');
  await expect(reply.marker).toHaveCSS('opacity', '0.45');
  await expect(reply.from).toHaveCount(2);
  await reply.fromSection(2).click();
  await expect(reply.slot).toHaveAttribute('data-phase', 'playing');
  await expectMarkerOn(reply, 2);
  await expect(reply.marker).toHaveCSS('opacity', '1');
  await expect.poll(async () => (await spoken(page)).at(-1)?.text).toBe(SECTION_SENTENCES[2][0]);

  // Reading the other reply moves everything to it.
  await other.control('').click();
  await expect(reply.layer).toHaveCount(0);
  await expect(other.marker).toHaveAttribute('data-section', '0');
  await expect(other.from).toHaveCount(0);
});

test('a device voice moves by section and repeats its sentence', async ({ harness, page }) => {
  await installSpeechStub(page, { enabled: true });
  await seedSectionReplies(harness);
  await openThread(harness, page);
  const reply = replyParts(page, 'Install it first.');
  const said = async () => (await spoken(page)).map((u) => u.text);

  await reply.control('').click();
  await expect.poll(said).toEqual(['Setup']);
  await endCurrentUtterance(page);
  await expect.poll(said).toEqual(['Setup', 'Install it first.']);
  const back = reply.control('-back');
  await expect(back).toHaveAttribute('aria-label', 'Repeat this sentence');
  await back.click();
  await expect.poll(said).toEqual(['Setup', 'Install it first.', 'Install it first.']);
  await expectMarkerOn(reply, 0);

  await reply.control('-next').click();
  await expect.poll(async () => (await said()).at(-1)).toBe('Build');
  await expectMarkerOn(reply, 1);
  await reply.control('-next').click();
  await expect.poll(async () => (await said()).at(-1)).toBe('Ship');
  await expectMarkerOn(reply, 2);
  await expect(reply.control('-next')).toBeDisabled();
  await reply.control('-previous').click();
  await expect.poll(async () => (await said()).at(-1)).toBe('Build');
  await expectMarkerOn(reply, 1);

  // Paused, a device voice is at its sentence's start: back has nothing to do.
  await reply.control('').click();
  await expect(reply.slot).toHaveAttribute('data-phase', 'paused');
  await expect(back).toBeDisabled();
});

test('a paragraph split for length has one read from here button, at its start', async ({ harness, page }) => {
  // Three blocks; the middle one is read as three sections.
  const long = `Part one starts here. ${Array.from({ length: 88 }, () => 'The word goes on here.').join(' ')}`;
  const opener = 'An opening paragraph that is long enough to be read as a section of its own, on its own.';
  await installSpeechStub(page, { enabled: true });
  await seedSectionReplies(harness, 0, `${opener}\n\n${long}\n\nA closing line.`);
  await openThread(harness, page);
  const reply = replyParts(page, 'An opening paragraph');
  const said = async () => (await spoken(page)).map((u) => u.text);
  const offered = () => reply.from.evaluateAll((els) => els.map((el) => Number((el.parentElement as HTMLElement).dataset.section)));

  await reply.control('').click();
  await expect(reply.marker).toHaveAttribute('data-section', '0');
  expect(await offered()).toEqual([1, 4]);

  // The paragraph's second part: the rule spans the paragraph, and its one
  // button reads it from its start.
  await reply.control('-next').click();
  await reply.control('-next').click();
  await expect(reply.marker).toHaveAttribute('data-section', '2');
  expect(await offered()).toEqual([0, 1, 4]);
  const rule = (await reply.marker.boundingBox())!;
  const paragraph = (await reply.blocks.nth(1).boundingBox())!;
  expect(Math.abs(rule.y - paragraph.y)).toBeLessThanOrEqual(0.5);
  expect(Math.abs(rule.height - paragraph.height)).toBeLessThanOrEqual(0.5);
  const tops = await reply.from.evaluateAll((els) => els.map((el) => el.getBoundingClientRect().top));
  expect(new Set(tops).size, 'no two buttons share a line').toBe(tops.length);

  await reply.blocks.nth(1).hover();
  await expect.poll(async () => (await fromOpacities(reply))[1]).toBe('1');
  await reply.fromSection(1).click();
  await expect.poll(async () => (await said()).at(-1)).toBe('Part one starts here.');
  await expect(reply.marker).toHaveAttribute('data-section', '1');
  expect(await offered()).toEqual([0, 4]);
});

test('starting, moving, pausing and stopping a reading keeps the row height and the scroll position', async ({ harness, page }) => {
  await installSpeechStub(page, { enabled: true });
  await seedSectionReplies(harness, 6);
  await openThread(harness, page, 8);
  const reply = replyParts(page, 'Install it first.');
  const scroller = page.getByTestId('message-timeline-scroll');
  await expect.poll(() => scroller.evaluate((el) => el.scrollHeight > el.clientHeight)).toBe(true);
  // Away from the bottom, so a height change would not be absorbed by
  // sticking to it.
  await reply.body.scrollIntoViewIfNeeded();
  await page.mouse.wheel(0, -60);
  const geometry = async () => ({
    height: (await reply.row.boundingBox())!.height,
    scrollTop: await scroller.evaluate((el) => el.scrollTop),
  });
  await expect.poll(async () => (await geometry()).scrollTop).toBeGreaterThan(0);
  let previous = await geometry();
  await expect.poll(async () => { const now = await geometry(); const same = now.scrollTop === previous.scrollTop; previous = now; return same; }).toBe(true);
  const before = await geometry();

  const steps: Array<[string, () => Promise<void>]> = [
    ['start', async () => {
      await reply.control('').click();
      await expectMarkerOn(reply, 0);
    }],
    ['next', async () => {
      await reply.control('-next').click();
      await expectMarkerOn(reply, 1);
    }],
    ['pause', async () => {
      await reply.control('').click();
      await expect(reply.slot).toHaveAttribute('data-phase', 'paused');
    }],
    ['read from here', async () => {
      await reply.fromSection(2).click();
      await expectMarkerOn(reply, 2);
    }],
    ['stop', async () => {
      await endCurrentUtterance(page);
      await endCurrentUtterance(page);
      await expect(reply.slot).toHaveAttribute('data-phase', 'idle');
      await expect(reply.layer).toHaveCount(0);
    }],
  ];
  for (const [name, step] of steps) {
    await step();
    // Two frames for a measured row height to reach the virtualizer.
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    expect(await geometry(), `after ${name}`).toEqual(before);
  }
});
