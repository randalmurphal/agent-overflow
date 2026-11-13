// A pull request's image diff across a transport reconnect, and under a
// slow first read, on GitHub, through the shipped UI and the real backend.
// Reconnect: the page's WebSocket is dropped with an image row on screen
// and comes back; the server drops every files handle with the connection
// that opened it, so the first image read after it (the SVG's `Rendered`
// view, which nothing read before) is refused
// `review_diff_files_gone`, the diff is opened once more for a new handle
// (stores/reviewDiffFiles.ts), and every later read names the new handle
// and shows its sides. Slow read: the image read's answer is held for 5
// seconds (the server's pin wait; the backend's pin hook is not on the
// harness wire, so the hold is the answer's), and the row keeps the
// height reserved for it while it waits and once its sides arrive.

import type { Page, WebSocketRoute } from '@playwright/test';

import { expect, test } from './fixtures.js';
import { expectEveryForgeCallHandled } from './forge-helpers.js';
import { openReview, reach, seed } from './review-diff-reading-flow.js';

// Generated bindings send method ids (internal/transport/methods_gen.go).
const OPEN_PR_DIFF = 4075510992;
const GET_REVIEW_DIFF_IMAGE = 3506270873;
const GET_REVIEW_DIFF_FILE_SIZES = 2795814304;

interface FileRead { method: 'image' | 'sizes'; handle: string; error?: string }

/** The page's socket, proxied: every diff open's handle and every file
 * read's handle and refusal code, in order, and the current connection. */
async function watchSocket(page: Page, holdImageMs = 0) {
  const seen = {
    socket: null as { page: WebSocketRoute; server: WebSocketRoute } | null,
    handles: [] as string[],
    reads: [] as FileRead[],
  };
  await page.routeWebSocket(/\/ws(?:\?|$)/, (route) => {
    const server = route.connectToServer();
    const socket = { page: route, server };
    seen.socket = socket;
    const opens = new Set<string>();
    const reads = new Map<string, FileRead>();
    const images = new Set<string>();
    route.onMessage((message) => {
      const frame = parse(message);
      if (frame?.type === 'rpc') {
        if (frame.methodId === OPEN_PR_DIFF) opens.add(frame.id);
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
      if (holdImageMs > 0 && frame?.type === 'rpc' && images.has(frame.id)) {
        setTimeout(() => route.send(message), holdImageMs);
        return;
      }
      route.send(message);
    });
  });
  return seen;
}

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

test('after a reconnect the diff is reopened once and its images load on the new handle', async ({ harness, page }) => {
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

  // The connection drops and the page reconnects on its own.
  const dropped = seen.socket!;
  await dropped.page.close({ code: 1012, reason: 'test reconnect' });
  await dropped.server.close();
  await expect.poll(() => seen.socket !== dropped).toBe(true);
  await expect(page.getByTestId('transport-status-banner')).toHaveCount(0, { timeout: 30_000 });

  // A read only the reconnected page makes: the SVG shown as an image.
  const readsBefore = seen.reads.length;
  const svgHeader = await reach(review, 'diagram.svg', review.getByTestId('review-scroll').locator('[data-testid="review-file-header"][data-path="diagram.svg"]'), false);
  await svgHeader.getByTestId('review-svg-rendered').click();
  const svg = review.locator('[data-testid="review-image-diff"][data-path="diagram.svg"]');
  await expect(svg.getByTestId('review-image')).toHaveCount(1, { timeout: 30_000 });
  await expect(svg.getByTestId('review-image-caption')).toHaveText(/^40 × 20 px · /);

  // One reopen, for a new handle; the reads on the old one were refused
  // gone, and every read on the new one answered.
  expect(seen.handles).toHaveLength(2);
  const second = seen.handles[1];
  expect(second).toBeTruthy();
  expect(second).not.toBe(first);
  const after = seen.reads.slice(readsBefore);
  const stale = after.filter((read) => read.handle === first);
  const fresh = after.filter((read) => read.handle === second);
  expect(stale.length).toBeGreaterThan(0);
  expect(stale.every((read) => read.error === 'review_diff_files_gone')).toBe(true);
  expect(fresh.length).toBeGreaterThanOrEqual(1);
  expect(fresh.every((read) => !read.error)).toBe(true);
  expect(stale.length + fresh.length).toBe(after.length);
  // The stale reads all precede the first read on the new handle.
  expect(after.findIndex((read) => read.handle === second)).toBe(stale.length);
  await expect(review.getByTestId('review-binary-reason')).toHaveCount(0);
  await expectEveryForgeCallHandled(harness);
});

test('an image row keeps its reserved height through a 5 second read', async ({ harness, page }) => {
  test.setTimeout(120_000);
  await page.emulateMedia({ colorScheme: 'dark' });
  await watchSocket(page, 5_000);
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
