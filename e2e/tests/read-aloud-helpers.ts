// Read-aloud through the shipped UI against a scripted `speechSynthesis`:
// headless Chromium has no voices, so the stub offers one and records what
// the app asks it to say.

import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, type SeedResult } from './fixtures.js';

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
 * control it shows (play/pause, and restart while paused) against Copy. */
export async function expectReadAloudInCopyGrid(row: Locator): Promise<void> {
  const copy = await boxOf(row.getByRole('button', { name: /^Cop/ }));
  const controls = [row.getByTestId('assistant-message-read-aloud'), row.getByTestId('assistant-message-read-aloud-restart')];
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
