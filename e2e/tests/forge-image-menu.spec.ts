// Forge-hosted images in a pull or merge request's review pane, through the
// shipped UI, the real backend and the fake forge CLI (cmd/ao-mockforge).
//
// Each forge seeds one PR/MR whose body wraps a PNG in
// `<p align="center"><img ...></p>` (the claim hook in the sanitizer, not a
// markdown token), references an SVG as markdown, and carries a
// conversation comment with a second PNG. The review pane is reached the
// way a user reaches it: the palette's Start Thread From Pull/Merge
// Request dialog, then Toggle review pane. Covered: the images render from
// bytes the backend fetched through `gh api` / `glab api` (asserted on the
// recorded invocations), a right-click opens only the Image Actions menu,
// Copy Image puts a PNG of the right size on the clipboard (an SVG is
// rasterised), Save Image on the owner's screen writes the forge's exact
// bytes into this boot's downloads directory, a non-image attachment is a
// file chip whose click saves it the same way, and in a paired connected
// browser Save Image is a browser download with the forge's file name.
//
// The paired browser reaches the backend on loopback; `transport/scopes.ts`
// still treats a paired session as off the host, which is the download
// path. It creates its own thread from the PR because a thread with no
// items yet is not listed in another client's sidebar.

import { randomBytes, randomUUID } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import { PNG_BASE64, PNG_BYTES, PNG_HEIGHT, PNG_WIDTH } from './attachment-fixture.js';
import {
  expandReviewSection,
  expectEveryForgeCallHandled,
  forgeInvocations,
  openPullRequestReview,
  seedForge,
  type ForgeInvocation,
  type ForgeRepo,
} from './forge-helpers.js';
import { confirmOnHost, redeemOnScreen, type PairingInvite } from './offhost-helpers.js';

const SVG_WIDTH = 120;
const SVG_HEIGHT = 80;
const SVG =
  `<svg xmlns="http://www.w3.org/2000/svg" width="${SVG_WIDTH}" height="${SVG_HEIGHT}" viewBox="0 0 120 80">` +
  '<rect width="120" height="80" fill="#2a7a5a"/><circle cx="60" cy="40" r="24" fill="#f4f4f4"/></svg>';

type ImageKey = 'centered' | 'diagram' | 'comment';
type AttachmentKey = ImageKey | 'log';

const LOG_TEXT = 'build ok\n';

const ALT: Record<ImageKey, string> = {
  centered: 'Centered shot',
  diagram: 'Flow diagram',
  comment: 'Comment shot',
};

interface ForgeCase {
  repo: ForgeRepo;
  /** What the user pastes into the dialog. */
  prUrl: string;
  /** The href each attachment is written with. */
  href: Record<AttachmentKey, string>;
  /** The name the backend saves each attachment under. */
  savedName: Record<AttachmentKey, string>;
  /**
   * The name a browser download of the centered PNG gets. Chromium appends
   * the extension the served type implies when the name has none.
   */
  downloadName: string;
  /** Whether `call` is the CLI fetch of `key`'s bytes. */
  fetches: (call: ForgeInvocation, key: AttachmentKey) => boolean;
  /** Whether `call` is the PR/MR metadata read. */
  readsPull: (call: ForgeInvocation) => boolean;
}

// The build log is written as an image, the way a forge's upload widget
// writes every attachment; its bytes make it a file chip.
function pullBody(href: Record<AttachmentKey, string>): string {
  return (
    `<p align="center"><img src="${href.centered}" alt="${ALT.centered}"></p>\n\n` +
    `How the pieces connect:\n\n![${ALT.diagram}](${href.diagram})\n\n` +
    `Log: ![build log](${href.log})\n`
  );
}

function commentBody(href: Record<AttachmentKey, string>): string {
  return `Looks right to me. ![${ALT.comment}](${href.comment})`;
}

// A unique project per case: the fake keys repositories by forge and
// project, and parallel workers must not read each other's fixtures.
function githubCase(): ForgeCase {
  const project = `ao-e2e/forge-images-${randomBytes(4).toString('hex')}`;
  const id: Record<ImageKey, string> = { centered: randomUUID(), diagram: randomUUID(), comment: randomUUID() };
  const logName = `build-${randomBytes(4).toString('hex')}.log`;
  const href = {
    centered: `https://github.com/user-attachments/assets/${id.centered}`,
    diagram: `https://github.com/user-attachments/assets/${id.diagram}`,
    comment: `https://github.com/user-attachments/assets/${id.comment}`,
    log: `https://github.com/user-attachments/files/${Date.now()}/${logName}`,
  };
  const number = 41;
  return {
    repo: {
      forge: 'github',
      project,
      pulls: [{ number, title: 'Forge images', body: pullBody(href), comments: [{ author: 'reviewer', body: commentBody(href) }] }],
      attachments: [
        { url: href.centered, base64: PNG_BASE64 },
        { url: href.diagram, text: SVG },
        { url: href.comment, base64: PNG_BASE64 },
        { url: href.log, text: LOG_TEXT },
      ],
    },
    prUrl: `https://github.com/${project}/pull/${number}`,
    href,
    // A user-attachments asset URL carries only an opaque id, and the id is
    // the name (internal/forgeattach gitHubAttachmentName). A save adds the
    // extension the classified bytes call for (internal/app downloadFileName).
    savedName: {
      centered: `${id.centered}.png`,
      diagram: `${id.diagram}.svg`,
      comment: `${id.comment}.png`,
      log: logName,
    },
    downloadName: `${id.centered}.png`,
    fetches: (call, key) => call.cli === 'gh' && call.route === 'gh api attachment' && call.args[1] === href[key],
    readsPull: (call) =>
      call.route === 'gh pr view' && call.args.includes(project) && call.args.includes(String(number)),
  };
}

function gitlabCase(): ForgeCase {
  const project = `ao-e2e/forge-images-${randomBytes(4).toString('hex')}`;
  const secret: Record<AttachmentKey, string> = {
    centered: randomBytes(16).toString('hex'),
    diagram: randomBytes(16).toString('hex'),
    comment: randomBytes(16).toString('hex'),
    log: randomBytes(16).toString('hex'),
  };
  // Unique names: the downloads directory outlives the per-test reset.
  const tag = randomBytes(4).toString('hex');
  const savedName: Record<AttachmentKey, string> = {
    centered: `centered-${tag}.png`,
    diagram: `flow-${tag}.svg`,
    comment: `comment-${tag}.png`,
    log: `build-${tag}.log`,
  };
  const href = {
    centered: `/uploads/${secret.centered}/${savedName.centered}`,
    diagram: `/uploads/${secret.diagram}/${savedName.diagram}`,
    comment: `/uploads/${secret.comment}/${savedName.comment}`,
    log: `/uploads/${secret.log}/${savedName.log}`,
  };
  const number = 17;
  return {
    repo: {
      forge: 'gitlab',
      project,
      pulls: [{ number, title: 'Forge images', body: pullBody(href), comments: [{ author: 'reviewer', body: commentBody(href) }] }],
      attachments: [
        { secret: secret.centered, filename: savedName.centered, contentType: 'image/png', base64: PNG_BASE64 },
        { secret: secret.diagram, filename: savedName.diagram, contentType: 'image/svg+xml', text: SVG },
        { secret: secret.comment, filename: savedName.comment, contentType: 'image/png', base64: PNG_BASE64 },
        { secret: secret.log, filename: savedName.log, contentType: 'text/plain', text: LOG_TEXT },
      ],
    },
    prUrl: `https://gitlab.com/${project}/-/merge_requests/${number}`,
    href,
    savedName,
    downloadName: savedName.centered,
    fetches: (call, key) =>
      call.cli === 'glab' &&
      call.route === 'glab api upload' &&
      call.args[1] === `projects/${encodeURIComponent(project)}/uploads/${secret[key]}/${savedName[key]}`,
    readsPull: (call) =>
      call.route === 'glab api merge request' &&
      call.args[1] === `projects/${encodeURIComponent(project)}/merge_requests/${number}`,
  };
}

const forges: Array<[string, () => ForgeCase]> = [
  ['GitHub', githubCase],
  ['GitLab', gitlabCase],
];

function imageMenu(page: Page) {
  return page.getByRole('menu', { name: 'Image Actions' });
}

/** Open the review pane on `forge`'s PR with the description and conversation expanded. */
async function openImages(page: Page, forge: ForgeCase): Promise<Record<ImageKey, Locator>> {
  const review = await openPullRequestReview(page, forge.prUrl);
  const description = await expandReviewSection(page, 'review-pr-description');
  const conversation = await expandReviewSection(page, 'review-pr-conversation');
  const images = {
    centered: description.getByRole('img', { name: ALT.centered }),
    diagram: description.getByRole('img', { name: ALT.diagram }),
    comment: conversation.getByRole('img', { name: ALT.comment }),
  };
  await expect(review).toBeVisible();
  return images;
}

async function naturalSize(image: Locator): Promise<{ width: number; height: number }> {
  return image.evaluate((img: HTMLImageElement) => ({ width: img.naturalWidth, height: img.naturalHeight }));
}

/** The PNG the clipboard holds, as its type list and decoded size. */
async function clipboardPng(page: Page): Promise<{ types: string[]; width: number; height: number }> {
  return page.evaluate(async () => {
    const items = await navigator.clipboard.read();
    const types = items.flatMap((item) => [...item.types]);
    const bitmap = await createImageBitmap(await items[0].getType('image/png'));
    const size = { width: bitmap.width, height: bitmap.height };
    bitmap.close();
    return { types, ...size };
  });
}

async function copyImage(page: Page, image: Locator): Promise<void> {
  await image.click({ button: 'right' });
  await imageMenu(page).getByRole('menuitem', { name: 'Copy Image' }).click();
  await expect(page.getByTestId('toast').filter({ hasText: 'Image copied' }).last()).toBeVisible();
  await expect(imageMenu(page)).toHaveCount(0);
}

/** Save `image` on the owner's screen and answer the path the toast names. */
async function saveImageHere(page: Page, image: Locator, name: string): Promise<string> {
  await image.click({ button: 'right' });
  await imageMenu(page).getByRole('menuitem', { name: 'Save Image' }).click();
  await expect(imageMenu(page)).toHaveCount(0);
  const toast = page.getByTestId('toast').filter({ hasText: `Saved to ` }).filter({ hasText: name });
  await expect(toast).toBeVisible();
  const text = (await toast.textContent()) ?? '';
  const saved = text.slice(text.indexOf('Saved to ') + 'Saved to '.length).trim();
  expect(path.basename(saved), `the toast names the written file: ${text}`).toBe(name);
  return saved;
}

async function seedCase(harness: HarnessApp, make: () => ForgeCase): Promise<ForgeCase> {
  const forge = make();
  await seedForge(harness, [forge.repo]);
  return forge;
}

for (const [name, make] of forges) {
  test.describe(`${name} review pane`, () => {
    test('renders the body and comment images from bytes fetched through the forge CLI', async ({
      harness,
      page,
    }) => {
      const forge = await seedCase(harness, make);
      await harness.open(page);
      const images = await openImages(page, forge);

      await expect.poll(() => naturalSize(images.centered)).toEqual({ width: PNG_WIDTH, height: PNG_HEIGHT });
      await expect.poll(() => naturalSize(images.diagram)).toEqual({ width: SVG_WIDTH, height: SVG_HEIGHT });
      await expect.poll(() => naturalSize(images.comment)).toEqual({ width: PNG_WIDTH, height: PNG_HEIGHT });
      // The HTML-wrapped picture keeps its wrapper's layout.
      await expect(page.locator('p[align="center"]').getByRole('img', { name: ALT.centered })).toBeVisible();

      const calls = await forgeInvocations(harness);
      expect(calls.some(forge.readsPull), 'the pull request was read through the fake CLI').toBe(true);
      for (const key of ['centered', 'diagram', 'comment'] as const) {
        expect(
          calls.some((call) => forge.fetches(call, key) && call.exitCode === 0),
          `${key} was fetched through the fake CLI`,
        ).toBe(true);
      }
      await expectEveryForgeCallHandled(harness);
    });

    test('a right-click opens only the image menu, Copy is a PNG of the right size, Save writes the bytes', async ({
      harness,
      page,
    }) => {
      const forge = await seedCase(harness, make);
      await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], {
        origin: new URL(harness.url).origin,
      });
      await harness.open(page);
      const images = await openImages(page, forge);
      await expect.poll(() => naturalSize(images.diagram)).toEqual({ width: SVG_WIDTH, height: SVG_HEIGHT });

      // Only the image menu: the HTML-wrapped image sits in markdown that
      // also delegates link and text menus.
      for (const key of ['centered', 'diagram', 'comment'] as const) {
        await images[key].click({ button: 'right' });
        await expect(imageMenu(page)).toBeVisible();
        await expect(page.getByRole('menu')).toHaveCount(1);
        await expect(imageMenu(page).getByRole('menuitem')).toHaveText(['Copy Image', 'Save Image']);
        await page.keyboard.press('Escape');
        await expect(imageMenu(page)).toHaveCount(0);
      }

      await copyImage(page, images.centered);
      expect(await clipboardPng(page)).toEqual({ types: ['image/png'], width: PNG_WIDTH, height: PNG_HEIGHT });

      // An SVG is rasterised at its intrinsic size times the copy scale
      // (pngClipboard.ts rasteriseSvg: devicePixelRatio clamped to 2..4).
      await copyImage(page, images.diagram);
      const scale = await page.evaluate(() => Math.max(2, Math.min(4, window.devicePixelRatio || 1)));
      expect(await clipboardPng(page)).toEqual({
        types: ['image/png'],
        width: SVG_WIDTH * scale,
        height: SVG_HEIGHT * scale,
      });

      const downloads = path.join(harness.bootstrap.dataDir, 'downloads');
      const png = await saveImageHere(page, images.centered, forge.savedName.centered);
      expect(path.dirname(png)).toBe(downloads);
      expect(await readFile(png)).toEqual(PNG_BYTES);
      const svg = await saveImageHere(page, images.diagram, forge.savedName.diagram);
      expect(path.dirname(svg)).toBe(downloads);
      expect(await readFile(svg, 'utf8')).toBe(SVG);
      await expectEveryForgeCallHandled(harness);
    });

    test('a file attachment is a chip that saves the file on the owner\'s screen', async ({ harness, page }) => {
      const forge = await seedCase(harness, make);
      await harness.open(page);
      await openImages(page, forge);
      const chip = page.getByTestId('review-pr-description').locator('[data-forge-attachment-file]');
      await expect(chip).toContainText(forge.savedName.log);
      await chip.click();
      const toast = page.getByTestId('toast').filter({ hasText: 'Saved to ' }).filter({ hasText: forge.savedName.log });
      await expect(toast).toBeVisible();
      const saved = path.join(harness.bootstrap.dataDir, 'downloads', forge.savedName.log);
      await expect(toast).toContainText(saved);
      expect(await readFile(saved, 'utf8')).toBe(LOG_TEXT);
      expect((await forgeInvocations(harness)).some((call) => forge.fetches(call, 'log'))).toBe(true);
      await expectEveryForgeCallHandled(harness);
    });

    test('Save Image in a connected browser downloads the forge file under its own name', async ({
      harness,
      browser,
    }) => {
      const forge = await seedCase(harness, make);

      const context = await browser.newContext();
      try {
        const device = await context.newPage();
        const invite = await harness.rpc<PairingInvite>('MintDevicePairing', 'browser', 'full');
        // Paired over loopback: no LAN bind is needed for this path.
        expect(new URL(invite.url).hostname).toBe('127.0.0.1');
        const code = await redeemOnScreen(device, invite, `${name} forge browser`);
        await confirmOnHost(harness, code);
        await expect(device.getByText('No projects yet')).toBeVisible({ timeout: 30_000 });

        const images = await openImages(device, forge);
        await expect.poll(() => naturalSize(images.centered)).toEqual({ width: PNG_WIDTH, height: PNG_HEIGHT });
        await images.centered.click({ button: 'right' });
        await expect(imageMenu(device).getByRole('menuitem')).toHaveText(['Copy Image', 'Save Image']);
        const downloaded = device.waitForEvent('download');
        await imageMenu(device).getByRole('menuitem', { name: 'Save Image' }).click();
        const download = await downloaded;
        expect(download.suggestedFilename()).toBe(forge.downloadName);
        expect(await readFile(await download.path())).toEqual(PNG_BYTES);
        // Nothing was written on the host for this device.
        await expect(device.getByTestId('toast').filter({ hasText: 'Saved' })).toHaveCount(0);
        await expectEveryForgeCallHandled(harness);
      } finally {
        await context.close();
      }
    });
  });
}
