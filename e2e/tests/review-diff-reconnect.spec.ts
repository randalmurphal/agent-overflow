// A review diff's image rows across a transport reconnect, under a slow
// first read and while the diff is still streaming, through the shipped
// UI and the real backend. The server drops every files handle with the
// connection that opened it (stores/reviewDiffFiles.ts).
// - A pull request (immutable revisions): after the page's WebSocket is
//   dropped and comes back, the next image read (the SVG's `Rendered`
//   view, which nothing read before) first opens the diff once more for a
//   new handle, and every read names it and shows its sides.
// - A workspace diff (the working tree): after the reconnect, with an
//   image changed on disk, the image row nothing read before offers
//   `Reload diff`; no image read is made and the diff is not reopened.
// - A slow read: the image read's answer is held for 5 seconds (the
//   server's pin wait; the backend's pin hook is not on the harness wire,
//   so the hold is the answer's), and the row keeps the height reserved
//   for it while it waits and once its sides arrive.
// - A diff larger than one read (2 MiB): its image row, published before
//   the rest of the patch is read, shows its image while the later reads
//   are held.

import { writeFile } from 'node:fs/promises';
import path from 'node:path';
import { randomBytes } from 'node:crypto';
import type { Page, WebSocketRoute } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { expectEveryForgeCallHandled, solidPNG } from './forge-helpers.js';
import { openReview, reach, seed } from './review-diff-reading-flow.js';

// Generated bindings send method ids (internal/transport/methods_gen.go).
const OPEN_PR_DIFF = 4075510992;
const OPEN_WORKSPACE_DIFF = 3558532873;
const READ_REVIEW_DIFF = 2473150982;
const GET_REVIEW_DIFF_IMAGE = 3506270873;
const GET_REVIEW_DIFF_FILE_SIZES = 2795814304;

interface FileRead { method: 'image' | 'sizes'; handle: string; error?: string }

interface RPCFrame {
  type?: string;
  id: string;
  methodId?: number;
  params: unknown[];
  result?: { filesHandle?: string };
  error?: { code?: string };
}

function parse(message: string | Buffer): RPCFrame | null {
  try {
    return JSON.parse(String(message)) as RPCFrame;
  } catch {
    return null;
  }
}

/** The page's socket, proxied: every diff open's handle and every file
 * read's handle and refusal code, in order, and the current connection.
 * `holdImageMs` delays each image read's answer; `holdReads` holds every
 * ReadReviewDiff answer until `releaseReads` runs. */
async function watchSocket(page: Page, opts: { holdImageMs?: number; holdReads?: boolean } = {}) {
  let releaseReads!: () => void;
  const readsHeld = new Promise<void>((resolve) => { releaseReads = resolve; });
  const seen = {
    socket: null as { page: WebSocketRoute; server: WebSocketRoute } | null,
    handles: [] as string[],
    reads: [] as FileRead[],
    patchReads: 0,
    releaseReads,
  };
  await page.routeWebSocket(/\/ws(?:\?|$)/, (route) => {
    const server = route.connectToServer();
    seen.socket = { page: route, server };
    const opens = new Set<string>();
    const reads = new Map<string, FileRead>();
    const images = new Set<string>();
    const patches = new Set<string>();
    route.onMessage((message) => {
      const frame = parse(message);
      if (frame?.type === 'rpc') {
        if (frame.methodId === OPEN_PR_DIFF || frame.methodId === OPEN_WORKSPACE_DIFF) opens.add(frame.id);
        if (frame.methodId === READ_REVIEW_DIFF) {
          patches.add(frame.id);
          seen.patchReads += 1;
        }
        if (frame.methodId === GET_REVIEW_DIFF_IMAGE || frame.methodId === GET_REVIEW_DIFF_FILE_SIZES) {
          const read: FileRead = { method: frame.methodId === GET_REVIEW_DIFF_IMAGE ? 'image' : 'sizes', handle: String(frame.params[1]) };
          seen.reads.push(read);
          reads.set(frame.id, read);
          if (read.method === 'image') images.add(frame.id);
        }
      }
      server.send(message);
    });
    server.onMessage((message) => {
      const frame = parse(message);
      if (frame?.type === 'rpc' && opens.has(frame.id)) seen.handles.push(String(frame.result?.filesHandle ?? ''));
      const read = frame?.type === 'rpc' ? reads.get(frame.id) : undefined;
      if (read && frame?.error) read.error = String(frame.error.code);
      if (opts.holdImageMs && frame?.type === 'rpc' && images.has(frame.id)) {
        setTimeout(() => route.send(message), opts.holdImageMs);
        return;
      }
      if (opts.holdReads && frame?.type === 'rpc' && patches.has(frame.id)) {
        void readsHeld.then(() => route.send(message));
        return;
      }
      route.send(message);
    });
  });
  return seen;
}

async function reconnect(page: Page, seen: Awaited<ReturnType<typeof watchSocket>>): Promise<void> {
  const dropped = seen.socket!;
  await dropped.page.close({ code: 1012, reason: 'test reconnect' });
  await dropped.server.close();
  await expect.poll(() => seen.socket !== dropped).toBe(true);
  await expect(page.getByTestId('transport-status-banner')).toHaveCount(0, { timeout: 30_000 });
}

const WORKSPACE_TITLE = 'Workspace images';

/** A project whose working tree holds untracked files: answers its root. */
async function seedWorkspace(harness: HarnessApp, files: Record<string, string | Uint8Array>): Promise<string> {
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: `workspace-images-${randomBytes(4).toString('hex')}`,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
      threads: [{ title: WORKSPACE_TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  const root = seeded.projects[0].path;
  for (const [name, data] of Object.entries(files)) await writeFile(path.join(root, name), data);
  return root;
}

/** The thread's review pane on its workspace's changes. */
async function openWorkspaceReview(page: Page) {
  await page.getByTestId('thread-row').filter({ hasText: WORKSPACE_TITLE }).click();
  const toggle = page.getByTestId('review-toggle');
  await expect(toggle).toBeVisible({ timeout: 30_000 });
  await toggle.click();
  const review = page.locator('section[data-pane-kind="review"]');
  await expect(review.getByTestId('review-file-header').first()).toBeVisible({ timeout: 30_000 });
  return review;
}

test('after a reconnect a pull request diff is reopened once and its images load on the new handle', async ({ harness, page }) => {
  test.setTimeout(120_000);
  await page.emulateMedia({ colorScheme: 'dark' });
  const seen = await watchSocket(page);
  await seed(harness, 'github');
  await harness.open(page);
  const review = await openReview(page, 'github', false);
  const logo = await reach(review, 'logo.png', review.locator('[data-testid="review-image-diff"][data-path="logo.png"]'), false);
  await expect(logo.getByTestId('review-image')).toHaveCount(2, { timeout: 30_000 });
  expect(seen.handles).toHaveLength(1);
  const [first] = seen.handles;
  expect(first).toBeTruthy();
  expect(seen.reads.length).toBeGreaterThan(0);
  expect(seen.reads.every((read) => read.handle === first && !read.error)).toBe(true);

  await reconnect(page, seen);

  // A read only the reconnected page makes: the SVG shown as an image.
  const readsBefore = seen.reads.length;
  const svgHeader = await reach(review, 'diagram.svg', review.getByTestId('review-scroll').locator('[data-testid="review-file-header"][data-path="diagram.svg"]'), false);
  await svgHeader.getByTestId('review-svg-rendered').click();
  const svg = review.locator('[data-testid="review-image-diff"][data-path="diagram.svg"]');
  await expect(svg.getByTestId('review-image')).toHaveCount(1, { timeout: 30_000 });
  await expect(svg.getByTestId('review-image-caption')).toHaveText(/^40 × 20 px · /);

  // One reopen, for a new handle, before any read: every read after the
  // reconnect names the new handle and answered.
  expect(seen.handles).toHaveLength(2);
  const second = seen.handles[1];
  expect(second).toBeTruthy();
  expect(second).not.toBe(first);
  const after = seen.reads.slice(readsBefore);
  expect(after.length).toBeGreaterThan(0);
  expect(after.every((read) => read.handle === second && !read.error)).toBe(true);
  await expect(review.getByTestId('review-binary-reason')).toHaveCount(0);
  await expectEveryForgeCallHandled(harness);
});

test('after a reconnect a workspace diff\'s unread image offers Reload diff and reads nothing', async ({ harness, page }) => {
  test.setTimeout(120_000);
  await page.emulateMedia({ colorScheme: 'dark' });
  const seen = await watchSocket(page);
  const filler = Array.from({ length: 3000 }, (_, index) => `line ${index + 1}`).join('\n');
  const root = await seedWorkspace(harness, {
    'a.png': solidPNG(16, 16, [200, 40, 40]),
    'm.txt': `${filler}\n`,
    'z.png': solidPNG(12, 12, [40, 40, 200]),
  });
  await harness.open(page);
  const review = await openWorkspaceReview(page);
  const top = review.locator('[data-testid="review-image-diff"][data-path="a.png"]');
  await expect(top.getByTestId('review-image')).toHaveCount(1, { timeout: 30_000 });
  expect(seen.handles).toHaveLength(1);
  // z.png sits below 3000 lines: nothing has read it.
  await expect(review.locator('[data-path="z.png"]')).toHaveCount(0);

  await reconnect(page, seen);
  await writeFile(path.join(root, 'z.png'), solidPNG(12, 12, [40, 200, 40]));
  const imageReads = seen.reads.filter((read) => read.method === 'image').length;

  const binary = await reach(review, 'z.png', review.locator('[data-testid="review-binary-file"][data-path="z.png"]'), false);
  await expect(binary.getByTestId('review-binary-reason')).toHaveText("This diff's files are no longer held; reload the diff.");
  await expect(binary.getByTestId('review-binary-reload')).toBeVisible();
  expect(seen.reads.filter((read) => read.method === 'image')).toHaveLength(imageReads);
  expect(seen.handles).toHaveLength(1);

  // Reload diff reads the tree as it is now, under a new open.
  await binary.getByTestId('review-binary-reload').click();
  await expect.poll(() => seen.handles.length, { timeout: 30_000 }).toBe(2);
  const reloaded = await reach(review, 'z.png', review.locator('[data-testid="review-image-diff"][data-path="z.png"]'), false);
  await expect(reloaded.getByTestId('review-image')).toHaveCount(1, { timeout: 30_000 });
});

test('an image row keeps its reserved height through a 5 second read', async ({ harness, page }) => {
  test.setTimeout(120_000);
  await page.emulateMedia({ colorScheme: 'dark' });
  await watchSocket(page, { holdImageMs: 5_000 });
  await seed(harness, 'github');
  await harness.open(page);
  const review = await openReview(page, 'github', false);
  const row = await reach(review, 'logo.png', review.locator('[data-testid="review-image-diff"][data-path="logo.png"]'), false);
  const next = review.getByTestId('review-scroll').locator('[data-testid="review-file-header"][data-path="notes.txt"]');
  const waiting = (await row.boundingBox())!;
  expect(waiting.height).toBeGreaterThan(0);
  await expect(row.getByTestId('review-image')).toHaveCount(0);
  const nextWaiting = (await next.boundingBox())?.y;
  await expect(row.getByTestId('review-image')).toHaveCount(2, { timeout: 30_000 });
  const shown = (await row.boundingBox())!;
  expect(shown.height).toBe(waiting.height);
  expect(shown.y).toBe(waiting.y);
  if (nextWaiting !== undefined) expect((await next.boundingBox())?.y).toBe(nextWaiting);
  await expectEveryForgeCallHandled(harness);
});

test('an image row published while a multi-read diff streams shows its image before the rest is read', async ({ harness, page }) => {
  test.setTimeout(120_000);
  await page.emulateMedia({ colorScheme: 'dark' });
  const seen = await watchSocket(page, { holdReads: true });
  // 3 MiB of added text after the image: more than one 2 MiB read.
  const big = Array.from({ length: 60_000 }, (_, index) => `${String(index).padStart(8, '0')} ${'x'.repeat(40)}`).join('\n');
  expect(big.length).toBeGreaterThan(2 * 1024 * 1024);
  await seedWorkspace(harness, { 'a.png': solidPNG(16, 16, [200, 40, 40]), 'big.txt': `${big}\n` });
  await harness.open(page);
  const review = await openWorkspaceReview(page);
  const image = review.locator('[data-testid="review-image-diff"][data-path="a.png"]');
  await expect(image.getByTestId('review-image')).toHaveCount(1, { timeout: 30_000 });
  // The rest of the patch is still held: big.txt is not complete.
  const bigHeader = review.getByTestId('review-scroll').locator('[data-testid="review-file-header"][data-path="big.txt"]');
  expect(seen.patchReads).toBeGreaterThan(0);
  await expect(bigHeader).toHaveCount(0);
  seen.releaseReads();
  await expect(bigHeader).toHaveCount(1, { timeout: 60_000 });
});
