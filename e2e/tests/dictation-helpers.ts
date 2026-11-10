// Dictation needs the computer's Codex signed in with ChatGPT and the
// Settings > Voice switch on.

import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';

import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect } from './fixtures.js';

export function codexAuthPath(harness: HarnessApp): string {
  const home = harness.bootstrap.homeDir;
  expect(home, 'the harness must isolate a provider home').toBeTruthy();
  return path.join(home!, '.codex', 'auth.json');
}

/** A Codex sign-in with ChatGPT in the harness's own provider home. */
export async function signInCodexWithChatGPT(harness: HarnessApp): Promise<void> {
  const file = codexAuthPath(harness);
  await mkdir(path.dirname(file), { recursive: true });
  await writeFile(file, JSON.stringify({
    auth_mode: 'chatgpt',
    tokens: { access_token: 'e2e-access', refresh_token: 'e2e-refresh', account_id: 'e2e-workspace', id_token: '' },
    last_refresh: new Date().toISOString(),
  }), { mode: 0o600 });
}

/** Turns on Settings > Voice's dictation switch, which is off by default,
 * and live dictation off: the worker's reset keeps host settings, and live
 * wins where both are on. */
export async function turnOnDictation(harness: HarnessApp): Promise<void> {
  await harness.rpc('UpdateSettings', { dictationSubscriptionEnabled: true, dictationAPIEnabled: false });
}

/** A computer that dictates: signed in with ChatGPT, switch on. */
export async function setUpDictation(harness: HarnessApp): Promise<void> {
  await signInCodexWithChatGPT(harness);
  await turnOnDictation(harness);
}

/** Where the computer reads its OpenAI API key (internal/openaikey). */
export function openAIKeyPath(harness: HarnessApp): string {
  return path.join(harness.bootstrap.dataDir, 'credentials', 'openai-api-key');
}

/** Puts `key` in the key file, owner-only as the computer requires. The
 * isolated boot answers live transcription in-process (openaifake), which
 * refuses only its scripted keys. */
export async function writeOpenAIKey(harness: HarnessApp, key = 'sk-e2e-live-dictation'): Promise<void> {
  const file = openAIKeyPath(harness);
  await mkdir(path.dirname(file), { recursive: true, mode: 0o700 });
  await writeFile(file, `${key}\n`, { mode: 0o600 });
}

// ---- mic geometry ----
//
// The mic is in Send's grid, not on a grid of its own (owner ruling,
// 2026-10-21). Both checks read bounding boxes and allow half a pixel.

const TOLERANCE = 0.5;

interface Box { x: number; y: number; width: number; height: number }

async function boxOf(locator: Locator): Promise<Box> {
  const box = await locator.boundingBox();
  expect(box, `${locator} must have geometry`).not.toBeNull();
  return box!;
}

const centerX = (b: Box) => b.x + b.width / 2;
const centerY = (b: Box) => b.y + b.height / 2;

function expectClose(actual: number, expected: number, what: string): void {
  // Soft, so one run reports every misaligned edge rather than the first.
  expect.soft(Math.abs(actual - expected), `${what}: ${actual} vs ${expected}`).toBeLessThanOrEqual(TOLERANCE);
}

/**
 * Phone: the mic sits in the input row in Send's column (same center x,
 * same diameter), the paperclip on its centerline, both centered on the
 * LAST text line (bottom-anchored, like Send to the toolbar), and the text
 * column ends before them.
 */
export async function expectPhoneMicAligned(page: Page): Promise<void> {
  const input = page.getByLabel('Message Input');
  const mic = await boxOf(page.getByTestId('composer-dictate'));
  const send = await boxOf(page.getByTestId('composer-send'));
  const clip = await boxOf(page.getByTestId('composer-attach'));
  const toolbar = await boxOf(page.getByTestId('composer-toolbar'));
  const inputBox = await boxOf(input);
  const text = await input.evaluate((el) => {
    const style = getComputedStyle(el);
    return { lineHeight: parseFloat(style.lineHeight), paddingBottom: parseFloat(style.paddingBottom), scrollTop: el.scrollTop };
  });
  expect(text.scrollTop, 'the input must not be scrolled for the last line to be measurable').toBe(0);
  const lastLineCenter = inputBox.y + inputBox.height - text.paddingBottom - text.lineHeight / 2;

  expect(mic.y + mic.height, 'the phone mic is in the input row, above the toolbar').toBeLessThanOrEqual(toolbar.y + TOLERANCE);
  expectClose(centerX(mic), centerX(send), 'mic center x vs Send');
  expectClose(mic.width, send.width, 'mic width vs Send');
  expectClose(mic.height, send.height, 'mic height vs Send');
  expectClose(centerY(clip), centerY(mic), 'paperclip center y vs mic');
  expectClose(centerY(mic), lastLineCenter, 'mic center y vs the last text line');
  expect(inputBox.x + inputBox.width, 'text never runs under the controls').toBeLessThanOrEqual(clip.x + TOLERANCE);
}

/**
 * Desktop: the mic is in the toolbar immediately left of Send, Send's
 * diameter and vertical center, and the gap Send keeps to its neighbors.
 * The input row has no mic, and its paperclip sits in Send's column.
 */
export async function expectDesktopMicAligned(page: Page): Promise<void> {
  const micLocator = page.getByTestId('composer-dictate');
  await expect(micLocator).toHaveCount(1);
  const mic = await boxOf(micLocator);
  const send = await boxOf(page.getByTestId('composer-send'));
  const clip = await boxOf(page.getByTestId('composer-attach'));
  const neighbors = await micLocator.evaluate((el) => {
    const toolbar = el.closest('[data-composer-toolbar]');
    const visible = [...(el.parentElement?.children ?? [])].filter((c) => (c as HTMLElement).offsetParent !== null);
    const index = visible.indexOf(el);
    const rect = (node: Element | undefined) => node?.getBoundingClientRect() ?? null;
    return {
      inToolbar: toolbar !== null,
      previous: rect(visible[index - 1]),
      next: visible[index + 1]?.getAttribute('data-testid') ?? null,
    };
  });
  expect(neighbors.inToolbar, 'the desktop mic is in the toolbar').toBe(true);
  expect(neighbors.next, 'the mic is immediately left of Send').toBe('composer-send');
  expectClose(mic.width, send.width, 'mic width vs Send');
  expectClose(mic.height, send.height, 'mic height vs Send');
  expectClose(centerY(mic), centerY(send), 'mic center y vs Send');
  const gapToSend = send.x - (mic.x + mic.width);
  expect(neighbors.previous, 'the mic has a neighbor on its left to measure the gap against').not.toBeNull();
  const gapFromPrevious = mic.x - neighbors.previous!.right;
  expectClose(gapFromPrevious, gapToSend, 'the gap before the mic vs the gap before Send');
  expectClose(centerX(clip), centerX(send), 'paperclip center x vs Send');
}

// ---- live caption geometry ----

/** The composer's boxes a caption must leave alone. */
export interface ComposerBoxes {
  input: Box;
  toolbar: Box;
  send: Box;
  mic: Box;
}

export async function composerBoxes(page: Page): Promise<ComposerBoxes> {
  return {
    input: await boxOf(page.getByLabel('Message Input')),
    toolbar: await boxOf(page.getByTestId('composer-toolbar')),
    send: await boxOf(page.getByTestId('composer-send')),
    mic: await boxOf(page.getByTestId('composer-dictate')),
  };
}

/**
 * The live caption floats above the card in the card's own box and the
 * input's text column, and the composer's controls sit exactly where they
 * did before it appeared.
 */
export async function expectCaptionInComposerGrid(page: Page, before: ComposerBoxes): Promise<void> {
  const caption = await boxOf(page.getByTestId('composer-dictation-caption'));
  const text = await boxOf(page.getByTestId('composer-dictation-caption-text'));
  const card = await boxOf(page.getByTestId('composer-root'));
  const input = page.getByLabel('Message Input');
  const inputBox = await boxOf(input);
  const textStart = inputBox.x + await input.evaluate((el) => parseFloat(getComputedStyle(el).paddingLeft));
  const dotWidth = await page.getByTestId('composer-dictation-caption-text').evaluate((el) => {
    const dot = el.previousElementSibling as HTMLElement;
    return dot.getBoundingClientRect().width + parseFloat(getComputedStyle(el.parentElement!).columnGap);
  });
  expectClose(caption.x, card.x, 'caption left vs the card');
  expectClose(caption.x + caption.width, card.x + card.width, 'caption right vs the card');
  expect(caption.y + caption.height, 'the caption ends above the card').toBeLessThanOrEqual(card.y + TOLERANCE);
  expectClose(text.x - dotWidth, textStart, 'caption text column vs the input text column');
  const lineHeight = await page.getByTestId('composer-dictation-caption-text').evaluate((el) => parseFloat(getComputedStyle(el).lineHeight));
  expect(caption.height, 'the caption shows at most two lines').toBeLessThanOrEqual(2 * lineHeight + 12 + 2 + TOLERANCE);
  const after = await composerBoxes(page);
  for (const key of ['input', 'toolbar', 'send', 'mic'] as const) {
    for (const edge of ['x', 'y', 'width', 'height'] as const) {
      expectClose(after[key][edge], before[key][edge], `${key} ${edge} with the caption shown`);
    }
  }
}
