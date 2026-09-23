// Forge images in a merge request's review pane under the compact layout:
// a long press on an image the MR body wraps in `<p align="center">` opens
// Copy Image / Save Image, Copy puts the PNG on the clipboard and Save
// writes the forge's bytes into this boot's downloads directory; an SVG
// saves byte for byte.
//
// The phone reaches the PR scope only from a checkout whose branch has an
// open MR: the chat header's Thread actions menu carries the MR row, and
// the command palette is not offered. So the seeded workspace gets a
// feature branch and an `origin` served by a loopback dumb-HTTP server
// (static files of a bare repository, `git update-server-info`, and the
// `refs/merge-requests/<n>/head` ref GitLab publishes), which the review
// pane's local diff fetches from. The host is admitted as self-hosted
// GitLab (`gitlabSelfHostedHosts`), and the fake forge answers `glab` for
// that host and project. Nothing leaves loopback.

import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { createReadStream } from 'node:fs';
import { mkdir, readFile, stat } from 'node:fs/promises';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import path from 'node:path';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { PNG_BASE64, PNG_BYTES, PNG_HEIGHT, PNG_WIDTH } from './attachment-fixture.js';
import { expandReviewSection, expectEveryForgeCallHandled, forgeInvocations, seedForge } from './forge-helpers.js';
import { plainScenario } from './thread-tools-helpers.js';
import { longPress } from './touch-helpers.js';

const SVG =
  '<svg xmlns="http://www.w3.org/2000/svg" width="120" height="80" viewBox="0 0 120 80">' +
  '<rect width="120" height="80" fill="#2a7a5a"/></svg>';
const MR_NUMBER = 17;
const TITLE = 'Phone forge images';

/** Serves `root` as static files on loopback: git's dumb HTTP protocol needs nothing more. */
async function serveStatic(root: string): Promise<Server> {
  const server = createServer((req, res) => {
    const rel = decodeURIComponent(new URL(req.url ?? '/', 'http://127.0.0.1').pathname);
    const file = path.join(root, rel);
    if (!file.startsWith(root + path.sep)) {
      res.writeHead(404).end();
      return;
    }
    stat(file).then(
      (info) => {
        if (!info.isFile()) {
          res.writeHead(404).end();
          return;
        }
        res.writeHead(200, { 'Content-Type': 'application/octet-stream', 'Content-Length': info.size });
        createReadStream(file).pipe(res);
      },
      () => res.writeHead(404).end(),
    );
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  return server;
}

interface Checkout {
  project: string;
  headSha: string;
  baseSha: string;
}

/**
 * Give the seeded workspace a feature branch and a loopback origin that
 * publishes it as merge request MR_NUMBER. Git runs with the harness home,
 * never the developer's.
 */
async function publishMergeRequest(harness: HarnessApp, workspace: string, port: number, originRoot: string) {
  const env = { ...process.env, HOME: path.join(harness.bootstrap.dataRoot, 'home'), GIT_CONFIG_NOSYSTEM: '1' };
  const git = (cwd: string, ...args: string[]) =>
    execFileSync('git', args, { cwd, env, encoding: 'utf8' }).trim();
  const project = `ao-e2e/phone-forge-${randomBytes(4).toString('hex')}`;
  const bare = path.join(originRoot, `${project}.git`);
  await mkdir(bare, { recursive: true });
  git(bare, 'init', '--bare', '--quiet');

  const baseSha = git(workspace, 'rev-parse', 'HEAD');
  git(workspace, 'checkout', '--quiet', '-b', 'feature');
  execFileSync('git', ['commit', '--quiet', '--allow-empty', '-m', 'feature work'], { cwd: workspace, env });
  const headSha = git(workspace, 'rev-parse', 'HEAD');
  git(workspace, 'push', '--quiet', bare, 'main', 'feature');
  git(bare, 'update-ref', `refs/merge-requests/${MR_NUMBER}/head`, headSha);
  git(bare, 'update-server-info');
  git(workspace, 'remote', 'add', 'origin', `http://127.0.0.1:${port}/${project}.git`);
  return { project, headSha, baseSha } satisfies Checkout;
}

test('a long press on a forge image in the review pane opens its menu, and Copy and Save work', async ({
  harness,
  page,
}) => {
  const originRoot = path.join(harness.bootstrap.dataRoot, `forge-origin-${randomBytes(4).toString('hex')}`);
  await mkdir(originRoot, { recursive: true });
  const server = await serveStatic(originRoot);
  try {
    await harness.rpc('HarnessSetScenario', {
      scenario: plainScenario({ name: 'compact-forge', provider: 'claude', texts: ['Hi.'], afterTurns: 'repeatLast' }),
    });
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        {
          name: 'phone-forge-app',
          repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
          threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
        },
      ],
    });
    const port = (server.address() as AddressInfo).port;
    const checkout = await publishMergeRequest(harness, seed.projects[0].path, port, originRoot);
    await harness.rpc('UpdateSettings', { gitlabSelfHostedHosts: ['127.0.0.1'] });

    const secret = { png: randomBytes(16).toString('hex'), svg: randomBytes(16).toString('hex') };
    // Unique names: the downloads directory outlives the per-test reset.
    const tag = randomBytes(4).toString('hex');
    const name = { png: `phone-${tag}.png`, svg: `phone-flow-${tag}.svg` };
    await seedForge(harness, [
      {
        forge: 'gitlab',
        project: checkout.project,
        host: '127.0.0.1',
        pulls: [
          {
            number: MR_NUMBER,
            title: 'Phone images',
            headRef: 'feature',
            baseRef: 'main',
            headSha: checkout.headSha,
            baseSha: checkout.baseSha,
            body:
              `<p align="center"><img src="/uploads/${secret.png}/${name.png}" alt="Centered shot"></p>\n\n` +
              `![Flow diagram](/uploads/${secret.svg}/${name.svg})\n`,
          },
        ],
        attachments: [
          { secret: secret.png, filename: name.png, contentType: 'image/png', base64: PNG_BASE64 },
          { secret: secret.svg, filename: name.svg, contentType: 'image/svg+xml', text: SVG },
        ],
      },
    ]);

    await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], {
      origin: new URL(harness.url).origin,
    });
    await harness.open(page);
    await page.getByTestId('thread-row').filter({ hasText: TITLE }).tap();

    // The MR row appears once git status has looked the branch's MR up.
    const more = page.getByTestId('chat-header-more');
    const menu = page.locator('[data-popover]');
    const mrRow = menu.getByRole('menuitem', { name: `MR !${MR_NUMBER}` });
    await expect(async () => {
      // A menu opened before the lookup landed is closed and reopened.
      if ((await menu.count()) > 0) await page.keyboard.press('Escape');
      await expect(menu).toHaveCount(0);
      await more.tap();
      await expect(mrRow).toBeVisible({ timeout: 2_000 });
    }).toPass({ timeout: 30_000 });
    await mrRow.tap();
    const review = page.locator('section[data-pane-kind="review"]');
    await expect(review.getByTestId('review-pr-header')).toBeVisible();

    const description = await expandReviewSection(page, 'review-pr-description');
    const centered = description.getByRole('img', { name: 'Centered shot' });
    const diagram = description.getByRole('img', { name: 'Flow diagram' });
    await expect.poll(() => centered.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBe(PNG_WIDTH);
    await expect.poll(() => diagram.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBe(120);

    const imageMenu = page.getByRole('menu', { name: 'Image Actions' });
    await longPress(page, centered, imageMenu);
    await expect(imageMenu.getByRole('menuitem')).toHaveText(['Copy Image', 'Save Image']);
    await expect(page.getByRole('menu')).toHaveCount(1);
    await imageMenu.getByRole('menuitem', { name: 'Copy Image' }).tap();
    await expect(page.getByTestId('toast').filter({ hasText: 'Image copied' })).toBeVisible();
    const held = await page.evaluate(async () => {
      const items = await navigator.clipboard.read();
      const bitmap = await createImageBitmap(await items[0].getType('image/png'));
      const size = { width: bitmap.width, height: bitmap.height };
      bitmap.close();
      return size;
    });
    expect(held).toEqual({ width: PNG_WIDTH, height: PNG_HEIGHT });

    const downloads = path.join(harness.bootstrap.dataDir, 'downloads');
    await longPress(page, centered, imageMenu);
    await imageMenu.getByRole('menuitem', { name: 'Save Image' }).tap();
    await expect(page.getByTestId('toast').filter({ hasText: path.join(downloads, name.png) })).toBeVisible();
    expect(await readFile(path.join(downloads, name.png))).toEqual(PNG_BYTES);

    await longPress(page, diagram, imageMenu);
    await imageMenu.getByRole('menuitem', { name: 'Save Image' }).tap();
    await expect(page.getByTestId('toast').filter({ hasText: path.join(downloads, name.svg) })).toBeVisible();
    expect(await readFile(path.join(downloads, name.svg), 'utf8')).toBe(SVG);

    const calls = await forgeInvocations(harness);
    expect(calls.some((call) => call.route === 'glab api merge request list')).toBe(true);
    expect(calls.some((call) => call.route === 'glab api upload' && call.args[1].endsWith(`/uploads/${secret.png}/${name.png}`))).toBe(true);
    await expectEveryForgeCallHandled(harness);
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});
