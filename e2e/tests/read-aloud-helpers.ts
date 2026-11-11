// Read-aloud through the shipped UI against a scripted `speechSynthesis`:
// headless Chromium has no voices, so the stub offers one and records what
// the app asks it to say.

import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, type SeedResult } from './fixtures.js';
import { writeOpenAIKey } from './dictation-helpers.js';

/** What the stub recorded for one utterance the app spoke. */
export interface SpokenUtterance {
  text: string;
  rate: number;
  volume: number;
  voice: string;
}

declare global {
  interface Window {
    __aoSpeech: {
      utterances: SpokenUtterance[];
      queued: number;
      endCurrent(): boolean;
    };
  }
}

/**
 * Replaces `speechSynthesis` before the app loads. With `voices` the device
 * offers one fake voice (announced through `voiceschanged`); without, none.
 * `speak()` records each utterance's text, rate, volume and voice,
 * `cancel()` reports every queued utterance as interrupted, and the test
 * ends the utterance being spoken through `window.__aoSpeech.endCurrent()`.
 */
export async function installSpeechStub(page: Page, options: { voices?: boolean; enabled?: boolean } = {}): Promise<void> {
  await page.addInitScript(({ voices, enabled }) => {
    type Utterance = {
      text: string;
      voice: { voiceURI: string } | null;
      rate: number;
      volume: number;
      onend: ((event: unknown) => void) | null;
      onerror: ((event: { error: string }) => void) | null;
    };
    class FakeUtterance implements Utterance {
      text: string;
      voice: { voiceURI: string } | null = null;
      rate = 1;
      volume = 1;
      onend: ((event: unknown) => void) | null = null;
      onerror: ((event: { error: string }) => void) | null = null;
      constructor(text: string) {
        this.text = text;
      }
    }
    const voiceList = voices
      ? [{ voiceURI: 'fake-voice', name: 'Fake Voice', lang: 'en-US', default: true, localService: true }]
      : [];
    const queue: Utterance[] = [];
    const record = { utterances: [] as { text: string; rate: number; volume: number; voice: string }[], queued: 0, endCurrent: () => false };
    const synth = Object.assign(new EventTarget(), {
      getVoices: () => voiceList,
      speak: (utterance: Utterance) => {
        queue.push(utterance);
        record.queued = queue.length;
        record.utterances.push({
          text: utterance.text,
          rate: utterance.rate,
          volume: utterance.volume,
          voice: utterance.voice?.voiceURI ?? '',
        });
      },
      cancel: () => {
        const dropped = queue.splice(0);
        record.queued = 0;
        for (const utterance of dropped) utterance.onerror?.({ error: 'interrupted' });
      },
      pause: () => undefined,
      resume: () => undefined,
      get speaking() {
        return queue.length > 0;
      },
      get pending() {
        return queue.length > 1;
      },
      get paused() {
        return false;
      },
    });
    record.endCurrent = () => {
      const current = queue.shift();
      record.queued = queue.length;
      if (!current) return false;
      current.onend?.({});
      return true;
    };
    Object.defineProperty(window, 'speechSynthesis', { value: synth, configurable: true });
    Object.defineProperty(window, 'SpeechSynthesisUtterance', { value: FakeUtterance, configurable: true });
    Object.defineProperty(window, '__aoSpeech', { value: record, configurable: true });
    // The app subscribes at boot; the voices arrive the way a browser's do.
    setTimeout(() => synth.dispatchEvent(new Event('voiceschanged')), 0);
    if (enabled) {
      localStorage.setItem('agent-overflow:frontend:readAloud', JSON.stringify({ enabled: true, voiceId: '', rate: 1, volume: 1 }));
    }
  }, { voices: options.voices ?? true, enabled: options.enabled ?? false });
}

export const FIRST_REPLY = 'First one. Second one.\n\n```js\nskipped();\n```\n\nThird one.';
export const SECOND_REPLY = 'Other reply. Another sentence.';

/** A thread with two finished replies, the first with a code block. */
export async function seedReplies(harness: HarnessApp): Promise<void> {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'read-aloud-app',
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
      threads: [{
        title: 'Read this',
        turns: [
          { userText: 'First?', items: [{ kind: 'assistant_text', summary: FIRST_REPLY }] },
          { userText: 'Second?', items: [{ kind: 'assistant_text', summary: SECOND_REPLY }] },
        ],
      }],
    }],
  });
}

export async function openRepliesThread(harness: HarnessApp, page: Page): Promise<void> {
  await harness.open(page);
  await page.getByTestId('thread-row').click();
  await expect(page.getByTestId('assistant-message-meta')).toHaveCount(2);
}

/** The Settings > Voice read-aloud switch, through the UI. */
export async function turnOnReadAloud(page: Page): Promise<void> {
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tab', { name: 'Voice', exact: true }).click();
  const toggle = page.getByLabel('Toggle reading replies aloud');
  await expect(toggle).toBeEnabled();
  await toggle.click();
  await expect(toggle).toHaveAttribute('aria-checked', 'true');
  await page.keyboard.press('Escape');
}

// ---- OpenAI voices ----
//
// The isolated boot answers OpenAI in-process (internal/openai/openaifake):
// a sentence's audio is 40 ms per character of its text, and a marker in
// the text scripts a failure.

/** The fake's audio length for `text`, in milliseconds. */
export const fakeSpeechMs = (text: string): number => [...text].length * 40;

/** MintReadAloudTicket's stable method id (frontend/bindings). */
const MINT_READ_ALOUD_TICKET = 1195194428;

/** Puts `key` in the computer's key file and turns its OpenAI voices
 * switch on. Host settings outlive the per-test reset: pair with
 * `turnOffOpenAIVoices`. */
export async function setUpOpenAIVoices(harness: HarnessApp, key: string): Promise<void> {
  await writeOpenAIKey(harness, key);
  await harness.rpc('UpdateSettings', { readAloudAPIEnabled: true });
}

export async function turnOffOpenAIVoices(harness: HarnessApp): Promise<void> {
  await harness.rpc('UpdateSettings', { readAloudAPIEnabled: false });
}

/** Records every sentence the page admits to be read in an OpenAI voice,
 * from the transport's RPC frames, and every speech audio fetch. Call
 * before the page opens its socket. */
export function watchSpeech(page: Page): { minted: Array<{ text: string; voice: string }>; fetches: () => number } {
  const minted: Array<{ text: string; voice: string }> = [];
  let fetches = 0;
  page.on('websocket', (socket) => {
    socket.on('framesent', ({ payload }) => {
      if (typeof payload !== 'string' || !payload.startsWith('{')) return;
      const frame = JSON.parse(payload) as { type?: string; methodId?: number; params?: unknown[] };
      if (frame.type !== 'rpc' || frame.methodId !== MINT_READ_ALOUD_TICKET) return;
      minted.push({ text: String(frame.params?.[0]), voice: String(frame.params?.[1]) });
    });
  });
  page.on('request', (request) => {
    if (request.method() === 'GET' && new URL(request.url()).pathname.startsWith('/attachments/speech/')) fetches += 1;
  });
  return { minted, fetches: () => fetches };
}

/** Settings > Voice, with read-aloud on for this device. */
export async function openVoiceSettings(page: Page): Promise<void> {
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tab', { name: 'Voice', exact: true }).click();
  const toggle = page.getByLabel('Toggle reading replies aloud');
  await expect(toggle).toBeEnabled();
  if (await toggle.getAttribute('aria-checked') !== 'true') await toggle.click();
  await expect(toggle).toHaveAttribute('aria-checked', 'true');
}

export const readAloudButtons = (page: Page): Locator => page.getByTestId('assistant-message-read-aloud');
export const readAloudSlots = (page: Page): Locator => page.getByTestId('assistant-message-read-aloud-slot');

export function spoken(page: Page): Promise<SpokenUtterance[]> {
  return page.evaluate(() => window.__aoSpeech.utterances);
}

export async function endCurrentUtterance(page: Page): Promise<void> {
  expect(await page.evaluate(() => window.__aoSpeech.endCurrent()), 'an utterance must be speaking to end').toBe(true);
}

// ---- geometry ----
//
// The read-aloud controls are in Copy's grid: the same box, on the same
// centerline, in both layouts (owner ruling, 2026-10-21).

const TOLERANCE = 0.5;

interface Box { x: number; y: number; width: number; height: number }

async function boxOf(locator: Locator): Promise<Box> {
  const box = await locator.boundingBox();
  expect(box, `${locator} must have geometry`).not.toBeNull();
  return box!;
}

function expectClose(actual: number, expected: number, what: string): void {
  expect.soft(Math.abs(actual - expected), `${what}: ${actual} vs ${expected}`).toBeLessThanOrEqual(TOLERANCE);
}

/** `row` is one assistant message's meta row. Checks every read-aloud
 * control it shows (play/pause, previous, back and next while reading,
 * restart while paused) against Copy. */
export async function expectReadAloudInCopyGrid(row: Locator): Promise<void> {
  const copy = await boxOf(row.getByRole('button', { name: /^Cop/ }));
  const controls = ['', '-previous', '-back', '-next', '-restart'].map((id) => row.getByTestId(`assistant-message-read-aloud${id}`));
  let checked = 0;
  for (const control of controls) {
    if ((await control.count()) === 0) continue;
    checked += 1;
    const box = await boxOf(control);
    const id = await control.getAttribute('data-testid');
    expectClose(box.width, copy.width, `${id} width vs Copy`);
    expectClose(box.height, copy.height, `${id} height vs Copy`);
    expectClose(box.y + box.height / 2, copy.y + copy.height / 2, `${id} center y vs Copy`);
  }
  expect(checked, 'the row shows a read-aloud control').toBeGreaterThan(0);
}

// ---- sections ----
//
// A reply is read in sections (frontend/src/lib/utils/speakableText.ts): a
// heading starts one, short paragraphs are packed into one, and a code block
// ends one. The reading reply shows a rule beside its current section's
// blocks and a "read from here" button beside every other section.

/** Three sections, each a heading and a short paragraph: blocks 0-1, 2-3, 4-5. */
export const SECTIONS_REPLY = [
  '## Setup', '', 'Install it first.', '',
  '## Build', '', 'Then build it.', '',
  '## Ship', '', 'Ship it once the checks are green and the notes are written.',
].join('\n');
/** What an OpenAI voice is asked to read for each section of SECTIONS_REPLY. */
export const SECTION_TEXTS = [
  'Setup\n\nInstall it first.',
  'Build\n\nThen build it.',
  'Ship\n\nShip it once the checks are green and the notes are written.',
];
/** The blocks each section of SECTIONS_REPLY covers. */
export const SECTION_BLOCKS = [[0, 1], [2, 3], [4, 5]] as const;
/** What a device voice says for each section of SECTIONS_REPLY. */
export const SECTION_SENTENCES = [
  ['Setup', 'Install it first.'],
  ['Build', 'Then build it.'],
  ['Ship', 'Ship it once the checks are green and the notes are written.'],
];
/** The pause between two OpenAI sections at normal speed (native/clipSpeech.ts). */
export const SECTION_PAUSE_MS = 850;

/** A thread with `filler` short turns, then `reply`, then SECOND_REPLY. */
export async function seedSectionReplies(harness: HarnessApp, filler = 0, reply = SECTIONS_REPLY): Promise<void> {
  const fillers = Array.from({ length: filler }, (_, i) => ({
    userText: `Filler ${i}?`,
    items: [{ kind: 'assistant_text', summary: `Filler answer ${i}.\n\nWith a second paragraph to take up room.` }],
  }));
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'read-aloud-sections-app',
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
      threads: [{
        title: 'Read this by section',
        turns: [
          ...fillers,
          { userText: 'Sections?', items: [{ kind: 'assistant_text', summary: reply }] },
          { userText: 'Second?', items: [{ kind: 'assistant_text', summary: SECOND_REPLY }] },
        ],
      }],
    }],
  });
}

/** One reply's parts, found by its body's text. */
export function replyParts(page: Page, text: string) {
  const body = page.getByTestId('assistant-message-body').filter({ hasText: text });
  const message = page.locator('[data-item-kind="assistant_text"]').filter({ has: body });
  return {
    body,
    message,
    row: page.locator('[data-virtual-row]').filter({ has: body }),
    meta: message.getByTestId('assistant-message-meta'),
    blocks: body.locator('.markdown-body > .md-committed > *'),
    marker: message.getByTestId('assistant-message-read-aloud-marker'),
    layer: message.getByTestId('assistant-message-read-aloud-sections'),
    from: message.getByTestId('assistant-message-read-aloud-from'),
    fromSection: (section: number) => message.locator(`[data-section="${section}"]`).getByTestId('assistant-message-read-aloud-from'),
    control: (id: '' | '-previous' | '-back' | '-next' | '-restart') => message.getByTestId(`assistant-message-read-aloud${id}`),
    slot: message.getByTestId('assistant-message-read-aloud-slot'),
  };
}

export type ReplyParts = ReturnType<typeof replyParts>;

/** The marker is on `section` and spans its blocks' real boxes, in the 8px
 * gutter left of the text. */
export async function expectMarkerOn(parts: ReplyParts, section: number): Promise<void> {
  await expect(parts.marker).toHaveAttribute('data-section', String(section));
  const [first, last] = SECTION_BLOCKS[section];
  const rule = await boxOf(parts.marker);
  const top = await boxOf(parts.blocks.nth(first));
  const bottom = await boxOf(parts.blocks.nth(last));
  const body = await boxOf(parts.body);
  expectClose(rule.y, top.y, `marker top vs block ${first}`);
  expectClose(rule.y + rule.height, bottom.y + bottom.height, `marker bottom vs block ${last}`);
  expect(rule.x + rule.width, 'the marker ends before the text').toBeLessThanOrEqual(body.x);
  expect(rule.x, 'the marker is in the 8px gutter, right of the nav rail strip').toBeGreaterThanOrEqual(body.x - 8);
}

/** Every "read from here" button sits in the row's right gutter, clear of
 * the text and of the overlay scrollbar's 6px strip, beside the first line
 * of its section. */
export async function expectFromButtonsInGutter(page: Page, parts: ReplyParts): Promise<void> {
  const body = await boxOf(parts.body);
  const scroller = await boxOf(page.getByTestId('message-timeline-scroll'));
  const count = await parts.from.count();
  expect(count).toBeGreaterThan(0);
  for (let i = 0; i < count; i += 1) {
    const button = parts.from.nth(i);
    const section = Number(await button.locator('..').getAttribute('data-section'));
    const box = await boxOf(button);
    expect(box.x, `button ${section} starts right of the text`).toBeGreaterThanOrEqual(body.x + body.width - TOLERANCE);
    expect(box.x + box.width, `button ${section} ends inside the 32px gutter`).toBeLessThanOrEqual(body.x + body.width + 32);
    expect(box.x + box.width, `button ${section} is clear of the scrollbar strip`).toBeLessThanOrEqual(scroller.x + scroller.width - 6);
    const block = await boxOf(parts.blocks.nth(SECTION_BLOCKS[section][0]));
    const center = box.y + box.height / 2;
    expect(center, `button ${section} is beside its first line`).toBeGreaterThan(block.y);
    expect(center).toBeLessThan(block.y + Math.min(block.height, 40));
  }
}

/** The meta row's controls are on one line, inside the row and the viewport. */
export async function expectClusterFits(page: Page, meta: Locator): Promise<void> {
  const row = await boxOf(meta);
  const viewport = page.viewportSize()!;
  const overflow = await meta.evaluate((el) => el.scrollWidth - el.clientWidth);
  expect(overflow, 'the meta row does not overflow').toBeLessThanOrEqual(1);
  const buttons = meta.getByRole('button');
  const count = await buttons.count();
  const first = await boxOf(buttons.first());
  for (let i = 0; i < count; i += 1) {
    const box = await boxOf(buttons.nth(i));
    expectClose(box.y + box.height / 2, first.y + first.height / 2, `button ${i} on the first button's line`);
    expect(box.x + box.width, `button ${i} inside the row`).toBeLessThanOrEqual(row.x + row.width + TOLERANCE);
    expect(box.x + box.width, `button ${i} inside the viewport`).toBeLessThanOrEqual(viewport.width);
  }
}

/** The opacity of each "read from here" button's reveal, by section. */
export function fromOpacities(parts: ReplyParts): Promise<Record<number, string>> {
  return parts.message.locator('[data-section]:has([data-testid="assistant-message-read-aloud-from"])').evaluateAll((els) =>
    Object.fromEntries(els.map((el) => [Number((el as HTMLElement).dataset.section), getComputedStyle(el).opacity])));
}

/** Keeps the audio element the page last played as `window.__aoAudio`, so
 * a spec can read where a clip is. Call before the page loads. */
export async function trackAudio(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const play = HTMLMediaElement.prototype.play;
    HTMLMediaElement.prototype.play = function (this: HTMLMediaElement) {
      (window as unknown as { __aoAudio?: HTMLMediaElement }).__aoAudio = this;
      return play.call(this);
    };
  });
}

/** Where the tracked audio element is in its clip, in seconds; -1 before any play. */
export function audioTime(page: Page): Promise<number> {
  return page.evaluate(() => (window as unknown as { __aoAudio?: HTMLMediaElement }).__aoAudio?.currentTime ?? -1);
}
