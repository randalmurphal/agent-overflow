// The lightbox under the compact layout tops out at the widest ladder tier
// (utils/imageTiers.ts#fullSizeMaxWidth): a phone's webview decoding a
// file past the derivative pixel cap can take the page down, so the
// sharper image it loads behind the painted tier is the 5120 tier, never
// the file itself. The timeline still paints the tier the phone's box
// needs. Decision: docs/decisions.md, "Images in chat".
import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { solidPng } from './attachment-fixture.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { plainScenario, setScenario } from './thread-tools-helpers.js';

// Past the derivative pixel cap (16 MP), so the backend derives even at
// the top tier instead of answering with the file.
const WIDTH = 6000;
const HEIGHT = 3000;
const TOP_TIER = 5120;
const NAME = 'poster.png';

test('the lightbox loads the top tier, not the file, behind the tier the phone painted', async ({ harness, page }) => {
  const title = 'Phone poster';
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'compact-image-lightbox',
        repo: {},
        threads: [{ title, provider: 'claude', turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  const workspace = seed.projects[0].path;
  const reference = `shots/${NAME}`;
  await mkdir(path.join(workspace, 'shots'), { recursive: true });
  await writeFile(path.join(workspace, reference), solidPng(WIDTH, HEIGHT, [160, 80, 40]));
  await setScenario(
    harness,
    workspace,
    plainScenario({ name: 'compact-image-lightbox', provider: 'claude', texts: [`The poster:\n\n![poster](${reference})\n`] }),
  );

  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: title }).tap();
  await page.getByLabel('Message Input').fill('show me');
  const completed = harness.waitForEvent('provider:turn_completed');
  await page.getByTestId('composer-send').tap();
  await completed;

  // The timeline paints the tier the phone's box needs at its pixel ratio.
  const painted = page.locator('img[data-markdown-image-src]');
  await expect.poll(() => painted.evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth)).toBeGreaterThan(0);
  const tier = await painted.evaluate((img: HTMLImageElement) => img.naturalWidth);
  expect(tier).toBeLessThan(TOP_TIER);
  await expect(painted).toHaveAttribute('width', String(WIDTH));

  await painted.tap();
  const dialog = page.getByRole('dialog', { name: NAME });
  const picture = dialog.locator('[data-lightbox-picture]');
  await expect(picture).toHaveAttribute('width', String(WIDTH));
  await expect(picture).toHaveAttribute('data-lightbox-original', '');
  await expect.poll(() => picture.evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth)).toBe(TOP_TIER);
  await expect(dialog.locator('[data-lightbox-failed]')).toHaveCount(0);
});
