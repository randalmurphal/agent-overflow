// A local image an agent wrote is painted from a display-density
// derivative: the ladder tier for its box width at this device pixel ratio
// (utils/imageTiers.ts, internal/attachment/derive.go). The <img> carries
// the file's pixel size whichever tier is painted, so widening the pane
// swaps a sharper tier into the same element and the row never reflows
// through a placeholder; narrowing keeps what is painted. A click opens the
// lightbox on the painted bytes, loads the file behind it and swaps it in,
// and the wheel zooms. Decision: docs/decisions.md, "Images in chat".
import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import type { Locator } from '@playwright/test';
import { solidPng } from './attachment-fixture.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { plainScenario, setScenario } from './thread-tools-helpers.js';

// Mirrors IMAGE_TIER_WIDTHS and DeriveWidths.
const TIERS = [320, 480, 720, 1080, 1440, 2160, 2880, 3840, 5120];
// Wide enough that every tier a 1280 or 2400 px window reaches is under two
// thirds of the file's width, which is where the backend derives rather
// than serving the file itself.
const WIDTH = 4000;
const HEIGHT = 800;
const NAME = 'panorama.png';

interface PaintedImage {
  natural: number;
  box: number;
  /** The paragraph's content width, which bounds the box. */
  container: number;
  width: string | null;
  height: string | null;
  marked: boolean;
}

function readPainted(painted: Locator): Promise<PaintedImage> {
  return painted.evaluate((element) => {
    const img = element as HTMLImageElement & { __tierMark?: true };
    const paragraph = img.closest('p');
    return {
      natural: img.complete ? img.naturalWidth : 0,
      box: img.getBoundingClientRect().width,
      container: paragraph ? paragraph.getBoundingClientRect().width : -1,
      width: img.getAttribute('width'),
      height: img.getAttribute('height'),
      marked: img.__tierMark === true,
    };
  });
}

// The harness window is 1280 wide; the narrow pass starts below it so the
// box sits under a lower tier and the default width is the upgrade.
const NARROW = { width: 960, height: 720 };
const WIDE = { width: 1280, height: 720 };

function zoomPercent(dialog: Locator): Promise<number> {
  return dialog.locator('[data-lightbox-zoom]').evaluate((element) => Number.parseInt(element.textContent ?? '', 10));
}

test('a local image paints at its box tier, upgrades in place as the window widens, and opens the file in the lightbox', async ({
  harness,
  page,
}) => {
  const title = 'Local image tiers';
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'image-tiers',
        repo: {},
        threads: [{ title, provider: 'claude', turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  const workspace = seed.projects[0].path;
  const reference = `shots/${NAME}`;
  await mkdir(path.join(workspace, 'shots'), { recursive: true });
  await writeFile(path.join(workspace, reference), solidPng(WIDTH, HEIGHT, [40, 80, 160]));
  await setScenario(
    harness,
    workspace,
    plainScenario({ name: 'image-tiers', provider: 'claude', texts: [`The panorama:\n\n![panorama](${reference})\n`] }),
  );

  await page.setViewportSize(NARROW);
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: title }).click();
  await page.getByLabel('Message Input').fill('show me');
  const completed = harness.waitForEvent('provider:turn_completed');
  await page.getByTestId('composer-send').click();
  await completed;

  const painted = page.locator('img[data-markdown-image-src]');
  const loading = page.locator('[data-streamdown-image-loading]');
  await expect.poll(async () => (await readPainted(painted)).natural).toBeGreaterThan(0);
  const narrow = await readPainted(painted);
  const narrowReport = JSON.stringify(narrow);
  // The painted bytes are a ladder tier that covers the box, not the file.
  expect(TIERS, narrowReport).toContain(narrow.natural);
  expect(narrow.natural, narrowReport).toBeGreaterThanOrEqual(Math.ceil(narrow.box));
  expect(narrow.natural, narrowReport).toBeLessThan(WIDTH);
  // The narrow window must leave room for a higher tier at the wide one.
  expect(narrow.natural, narrowReport).toBeLessThan(1080);
  // The box is the file's pixel size whatever is painted.
  expect(narrow.width).toBe(String(WIDTH));
  expect(narrow.height).toBe(String(HEIGHT));
  await painted.evaluate((element) => {
    (element as HTMLImageElement & { __tierMark?: true }).__tierMark = true;
  });

  // Widening the window grows the box past the painted tier: a sharper
  // tier lands in the same element, with no placeholder in between.
  await page.setViewportSize(WIDE);
  await expect
    .poll(async () => (await readPainted(painted)).box, 'the box grows with the window')
    .toBeGreaterThan(narrow.box);
  await expect
    .poll(async () => (await readPainted(painted)).natural, `a sharper tier lands; was ${narrowReport}`)
    .toBeGreaterThan(narrow.natural);
  const wide = await readPainted(painted);
  const wideReport = JSON.stringify(wide);
  expect(TIERS, wideReport).toContain(wide.natural);
  expect(wide.natural, wideReport).toBeGreaterThanOrEqual(Math.ceil(wide.box));
  expect(wide.natural, wideReport).toBeLessThan(WIDTH);
  expect(wide.width).toBe(String(WIDTH));
  expect(wide.height).toBe(String(HEIGHT));
  expect(wide.marked, 'the <img> element was replaced on upgrade').toBe(true);
  await expect(loading).toHaveCount(0);

  // Narrowing keeps the sharper bytes: they already cover a narrower box.
  await page.setViewportSize(NARROW);
  await expect.poll(async () => (await readPainted(painted)).box).toBeLessThan(wide.box);
  expect((await readPainted(painted)).natural).toBe(wide.natural);
  expect((await readPainted(painted)).marked).toBe(true);

  // The lightbox opens at the file's box, loads the file behind the painted
  // tier and swaps it in; the wheel zooms.
  await painted.click();
  const dialog = page.getByRole('dialog', { name: NAME });
  const picture = dialog.locator('[data-lightbox-picture]');
  await expect(picture).toHaveAttribute('width', String(WIDTH));
  await expect(picture).toHaveAttribute('height', String(HEIGHT));
  await expect(picture).toHaveAttribute('data-lightbox-original', '');
  await expect.poll(() => picture.evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth)).toBe(WIDTH);
  await expect(dialog.locator('[data-lightbox-loading]')).toHaveCount(0);
  await expect(dialog.locator('[data-lightbox-failed]')).toHaveCount(0);

  const fit = await zoomPercent(dialog);
  expect(fit).toBeGreaterThan(0);
  expect(fit).toBeLessThan(100);
  await dialog.locator('[data-lightbox-canvas]').hover({ position: { x: 500, y: 300 } });
  await page.mouse.wheel(0, -300);
  await expect.poll(() => zoomPercent(dialog)).toBeGreaterThan(fit);
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
});
