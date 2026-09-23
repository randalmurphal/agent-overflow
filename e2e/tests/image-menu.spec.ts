// The Copy Image / Save Image menu on a sent image attachment, through the
// shipped UI and the real backend (ImageMenuHost.svelte).
//
// Covered here because only a real engine can answer it: the PNG the
// clipboard actually holds after Copy, the menu's stacking above the
// lightbox (hit-testing, not class names), focus and Escape ownership
// between the menu and the dialog under it, the file the owner's screen
// writes through `SaveAttachment`, and the browser download a paired
// connected browser gets instead. The paired browser reaches the backend
// on loopback; `transport/scopes.ts` still treats a paired session as off
// the host, which is the connected-browser save path. The last case is the
// delegated hosts' precedence under a trusted right-click: a link opens the
// link menu, an image only the image menu. A Codex generated image, imported
// from the isolated provider home through the real triage path, carries the
// same menu.
//
// Forge images (PR/MR bodies and comments) carry the same menu in the review
// pane; forge-image-menu.spec.ts covers them against the fake forge CLI.

import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import type { Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { PNG_BASE64, PNG_BYTES, PNG_HEIGHT, PNG_WIDTH } from './attachment-fixture.js';
import { seedAgentThread } from './agent-visibility-helpers.js';
import { confirmOnHost, redeemOnScreen, type PairingInvite } from './offhost-helpers.js';
import { plainScenario } from './thread-tools-helpers.js';

async function seedThread(harness: HarnessApp, title: string): Promise<string> {
  await harness.rpc('HarnessSetScenario', {
    scenario: plainScenario({
      name: 'image-menu',
      provider: 'claude',
      texts: ['Image received.'],
      afterTurns: 'repeatLast',
    }),
  });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'image-menu-app',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [
          {
            title,
            turns: [
              {
                userText: 'Hello.',
                items: [{ kind: 'assistant_text', summary: 'Hi. See [the docs](https://example.com/docs).' }],
              },
            ],
          },
        ],
      },
    ],
  });
  return seed.projects[0].threadIds[0];
}

/** A real `DataTransfer` drop on the composer, as attachment-transfer.spec.ts. */
async function dropImage(page: Page, filename: string): Promise<void> {
  await page.getByTestId('composer-root').evaluate(
    (root, payload) => {
      const binary = atob(payload.base64);
      const bytes = new Uint8Array(binary.length);
      for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
      const transfer = new DataTransfer();
      transfer.items.add(new File([bytes], payload.filename, { type: 'image/png' }));
      root.dispatchEvent(new DragEvent('drop', { dataTransfer: transfer, bubbles: true, cancelable: true }));
    },
    { filename, base64: PNG_BASE64 },
  );
}

/** Send one message carrying the image, and wait for the turn to settle. */
async function sendImage(harness: HarnessApp, page: Page, title: string, filename: string): Promise<void> {
  await page.getByText(title, { exact: true }).click();
  await dropImage(page, filename);
  await expect(page.getByTestId('attachment-thumb')).toBeVisible();
  await page.getByLabel('Message Input').fill('Here it is. [Image #1]');
  const completed = harness.waitForEvent('provider:turn_completed');
  await page.getByTestId('composer-send').click();
  await completed;
}

function sentImage(page: Page, filename: string) {
  return page.getByTestId('user-message-attachments').getByLabel(`Preview ${filename}`);
}

function imageMenu(page: Page) {
  return page.getByRole('menu', { name: 'Image Actions' });
}

/** Whether the topmost element at `locator`'s centre is inside `locator`. */
async function receivesHitsAtCentre(page: Page, locator: ReturnType<Page['locator']>): Promise<boolean> {
  const box = await locator.boundingBox();
  if (!box) return false;
  const handle = await locator.elementHandle();
  return page.evaluate(
    ({ x, y, el }) => {
      const hit = document.elementFromPoint(x, y);
      return hit !== null && el !== null && el.contains(hit);
    },
    { x: box.x + box.width / 2, y: box.y + box.height / 2, el: handle },
  );
}

test('Copy Image puts the full-size picture on the clipboard as PNG', async ({ harness, page }) => {
  const filename = 'copy-me.png';
  await seedThread(harness, 'Image copy');
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], {
    origin: new URL(harness.url).origin,
  });
  await harness.open(page);
  await sendImage(harness, page, 'Image copy', filename);

  const tile = sentImage(page, filename);
  await tile.click({ button: 'right' });
  await expect(imageMenu(page)).toBeVisible();
  await expect(imageMenu(page).getByRole('menuitem')).toHaveText(['Copy Image', 'Save Image']);

  await imageMenu(page).getByRole('menuitem', { name: 'Copy Image' }).click();
  await expect(page.getByTestId('toast').filter({ hasText: 'Image copied' })).toBeVisible();
  await expect(imageMenu(page)).toHaveCount(0);

  // The clipboard holds a PNG at the ORIGINAL dimensions: 300x200 is past
  // the 256px thumbnail cap, so the tile's thumbnail could not produce it.
  const held = await page.evaluate(async () => {
    const items = await navigator.clipboard.read();
    const types = items.flatMap((item) => [...item.types]);
    const png = await items[0].getType('image/png');
    const bitmap = await createImageBitmap(png);
    const size = { width: bitmap.width, height: bitmap.height };
    bitmap.close();
    return { types, ...size };
  });
  expect(held).toEqual({ types: ['image/png'], width: PNG_WIDTH, height: PNG_HEIGHT });
});

test('the menu opens over the lightbox, and neither Escape nor a row closes the lightbox', async ({
  harness,
  page,
}) => {
  const filename = 'lightbox-save.png';
  await seedThread(harness, 'Image lightbox');
  await harness.open(page);
  await sendImage(harness, page, 'Image lightbox', filename);

  await sentImage(page, filename).click();
  const dialog = page.getByRole('dialog', { name: filename });
  const picture = dialog.getByRole('img', { name: filename });
  await expect.poll(() => picture.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBe(PNG_WIDTH);

  await picture.click({ button: 'right' });
  const menu = imageMenu(page);
  await expect(menu).toBeVisible();
  // Stacking is proven by hit-testing: each row is the topmost element at
  // its own centre, with the full-viewport lightbox under it.
  for (const label of ['Copy Image', 'Save Image']) {
    expect(await receivesHitsAtCentre(page, menu.getByRole('menuitem', { name: label }))).toBe(true);
  }

  // Escape is the menu's, and the lightbox stays.
  await expect(menu.getByRole('menuitem', { name: 'Copy Image' })).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(menu).toHaveCount(0);
  await expect(dialog).toBeVisible();

  // A press on the backdrop dismisses a menu raised over the lightbox,
  // not the lightbox.
  await picture.click({ button: 'right' });
  await expect(menu).toBeVisible();
  await page.mouse.click(5, 5);
  await expect(menu).toHaveCount(0);
  await expect(dialog).toBeVisible();

  // Save on the owner's screen: the backend writes the original bytes into
  // this isolated boot's downloads directory, and the lightbox stays.
  await picture.click({ button: 'right' });
  await menu.getByRole('menuitem', { name: 'Save Image' }).click();
  const toast = page.getByTestId('toast').filter({ hasText: 'Saved to ' });
  await expect(toast).toBeVisible();
  await expect(dialog).toBeVisible();
  const saved = ((await toast.textContent()) ?? '').match(/Saved to (\S+lightbox-save\.png)/)?.[1];
  expect(saved, 'the toast names the written path').toBeTruthy();
  expect(path.dirname(saved!)).toBe(path.join(harness.bootstrap.dataDir, 'downloads'));
  expect(await readFile(saved!)).toEqual(PNG_BYTES);

  // With the menu gone, Escape reaches the lightbox again.
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
});

test('Save Image in a connected browser downloads the original under its own name', async ({
  harness,
  page,
  browser,
}) => {
  const filename = 'download-me.png';
  await seedThread(harness, 'Image download');
  await harness.open(page);
  await sendImage(harness, page, 'Image download', filename);

  const context = await browser.newContext();
  try {
    const device = await context.newPage();
    const invite = await harness.rpc<PairingInvite>('MintDevicePairing', 'browser', 'full');
    // Paired over loopback: no LAN bind is needed for this path.
    expect(new URL(invite.url).hostname).toBe('127.0.0.1');
    const code = await redeemOnScreen(device, invite, 'Image browser');
    await confirmOnHost(harness, code);

    await device.getByText('Image download', { exact: true }).click({ timeout: 30_000 });
    await sentImage(device, filename).click({ button: 'right' });
    const downloaded = device.waitForEvent('download');
    await imageMenu(device).getByRole('menuitem', { name: 'Save Image' }).click();
    const download = await downloaded;
    expect(download.suggestedFilename()).toBe(filename);
    const file = await download.path();
    expect(await readFile(file)).toEqual(PNG_BYTES);
    // Nothing was written on the host for this device.
    await expect(device.getByTestId('toast').filter({ hasText: 'Saved' })).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test('a real right-click opens the link menu on a link and only the image menu on an image', async ({
  harness,
  page,
}) => {
  const filename = 'beside-a-link.png';
  await seedThread(harness, 'Image and link');
  await harness.open(page);
  await sendImage(harness, page, 'Image and link', filename);
  const allMenus = page.getByRole('menu');

  await page.getByRole('link', { name: 'the docs' }).click({ button: 'right' });
  await expect(page.getByRole('menu', { name: 'Link Actions' })).toBeVisible();
  await expect(allMenus).toHaveCount(1);
  await page.keyboard.press('Escape');
  await expect(allMenus).toHaveCount(0);

  await sentImage(page, filename).click({ button: 'right' });
  await expect(imageMenu(page)).toBeVisible();
  await expect(allMenus).toHaveCount(1);
});

test('a Codex generated image opens the image menu and copies its original bytes', async ({ harness, page }) => {
  const filename = 'generated-menu.png';
  // Codex writes the picture under its own home; the import reads only from
  // there (app_provider_home.go), and this boot's home is `<dataRoot>/home`.
  const dir = path.join(harness.bootstrap.dataRoot, 'home', '.codex', 'generated_images');
  await mkdir(dir, { recursive: true });
  const savedPath = path.join(dir, filename);
  await writeFile(savedPath, PNG_BYTES);

  const rpc = (method: string, params: object) => JSON.stringify({ jsonrpc: '2.0', method, params });
  const scoped = (method: string, fields: object) =>
    rpc(method, { threadId: '${THREAD_ID}', turnId: '${TURN_ID}', ...fields });
  await harness.rpc('HarnessSetScenario', {
    scenario: {
      version: 1,
      name: 'image-menu-generated',
      provider: 'codex',
      afterTurns: 'silent',
      turns: [
        {
          steps: [
            {
              emit: {
                lines: [
                  rpc('turn/started', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}' } }),
                  scoped('item/completed', {
                    item: { id: 'img-menu-1', type: 'imageGeneration', status: 'completed', savedPath, revisedPrompt: 'A small test card' },
                  }),
                  scoped('item/completed', { item: { id: 'answer', type: 'agentMessage', text: 'Here is the picture.' } }),
                  rpc('turn/completed', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}', status: 'completed' } }),
                ],
              },
            },
          ],
        },
      ],
    },
  });
  const title = 'Generated image menu';
  const threadId = await seedAgentThread(harness, 'image-menu-generated', title, 'codex');
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], {
    origin: new URL(harness.url).origin,
  });
  await harness.open(page);
  await page.getByText(title, { exact: true }).click();
  const completed = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'draw a card', null);
  await completed;

  const tile = page.getByTestId('generated-image-attachments').getByLabel(`Preview ${filename}`);
  await tile.click({ button: 'right' });
  await expect(imageMenu(page)).toBeVisible();
  await expect(imageMenu(page).getByRole('menuitem')).toHaveText(['Copy Image', 'Save Image']);
  await expect(page.getByRole('menu')).toHaveCount(1);

  await imageMenu(page).getByRole('menuitem', { name: 'Copy Image' }).click();
  await expect(page.getByTestId('toast').filter({ hasText: 'Image copied' })).toBeVisible();
  const held = await page.evaluate(async () => {
    const items = await navigator.clipboard.read();
    const png = await items[0].getType('image/png');
    const bitmap = await createImageBitmap(png);
    const size = { width: bitmap.width, height: bitmap.height };
    bitmap.close();
    return size;
  });
  expect(held).toEqual({ width: PNG_WIDTH, height: PNG_HEIGHT });
});
