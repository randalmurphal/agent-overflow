// A pull request diff larger than any single read and taller than a
// browser can lay out, through the shipped UI, the real backend and the
// fake forge CLI. The PR adds 20 files of 40,000 lines each: about 13 MB of
// diff, past the 10 MiB a whole-output read once refused, and about 16M px
// of rows once expanded, past the review list's held-range limit
// (frontend/src/lib/utils/virtual/heldRows.ts). Covered: the pane opens the
// whole diff with every file counted, expanding every file holds a range
// of the rows rather than all of them, and the file tree reaches the last
// line of the last file and then the first line of the first file, both
// outside the range held before the jump. The last line holds text past
// ASCII, which arrives packed (ReadReviewDiffPacked) and shows as written.
//
// A workspace diff larger than one read, with text past ASCII: a draft
// comment is saved under the source key of the patch, which the page takes
// from the chunks' keys, and it equals the key of the text the backend
// served.

import { randomBytes } from 'node:crypto';
import { writeFile } from 'node:fs/promises';
import path from 'node:path';
import type { Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { expectEveryForgeCallHandled, openPullRequestReview, publishPullRequest } from './forge-helpers.js';

const TITLE = 'Large diff thread';
const FILES = 20;
const LINES = 40_000;
const HELD_LIMIT_PX = 12_000_000;

const fileName = (file: number) => `f${String(file).padStart(2, '0')}`;
// Fixed-width numbers: no line's text contains another's, so a substring
// match finds exactly one line (a line renders after its +/- marker).
const lineText = (file: number, line: number) =>
  `${fileName(file)} line ${String(line).padStart(6, '0')}${file === FILES - 1 && line === LINES ? WIDE_TAIL : ''}`;
// Two-, three- and four-byte UTF-8 on the patch's last line.
const WIDE_TAIL = ' naïve — 日本語 🎉';

// Generated bindings send method ids (internal/transport/methods_gen.go).
const OPEN_WORKSPACE_DIFF = 3558532873;
const READ_REVIEW_DIFF_PACKED = 4058838878;
// A pane opened from a thread saves its drafts under the thread.
const CREATE_DIFF_REVIEW_COMMENT = 565306783;

// A chunk is packed exactly when it carries wide-line marks.
interface ChunkAnswer { data: string; wide?: number[]; key?: { hash: number; units: number } }
interface RPCFrame {
  type?: string;
  id: string;
  methodId?: number;
  params?: unknown[];
  result?: ChunkAnswer & { chunk?: ChunkAnswer };
}

/** The page's socket, proxied: the patch text its diff chunks carried, how
 * many of them arrived packed, the key the last one carried as a source
 * key, and the source key of the draft the page saved. */
async function watchDiff(page: Page) {
  const seen = { text: '', packedChunks: 0, lastChunkKey: '', draftKey: '' };
  await page.routeWebSocket(/\/ws(?:\?|$)/, (route) => {
    const server = route.connectToServer();
    const chunkCalls = new Map<string, number>();
    route.onMessage((message) => {
      const frame = JSON.parse(String(message)) as RPCFrame;
      if (frame.type === 'rpc' && frame.methodId !== undefined) {
        if (frame.methodId === OPEN_WORKSPACE_DIFF || frame.methodId === READ_REVIEW_DIFF_PACKED) chunkCalls.set(frame.id, frame.methodId);
        if (frame.methodId === CREATE_DIFF_REVIEW_COMMENT) seen.draftKey = String((frame.params?.[1] as { sourceKey?: string }).sourceKey);
      }
      server.send(message);
    });
    server.onMessage((message) => {
      const frame = JSON.parse(String(message)) as RPCFrame;
      const method = frame.type === 'rpc' ? chunkCalls.get(frame.id) : undefined;
      if (method !== undefined && frame.result) {
        const chunk = method === OPEN_WORKSPACE_DIFF ? frame.result.chunk! : frame.result;
        // An open starts the patch over. Packed data is the text's UTF-8,
        // one character per byte.
        if (method === OPEN_WORKSPACE_DIFF) seen.text = '';
        const packed = (chunk.wide?.length ?? 0) > 0;
        seen.text += packed ? Buffer.from(chunk.data, 'latin1').toString('utf8') : chunk.data;
        if (packed) seen.packedChunks += 1;
        seen.lastChunkKey = chunk.key ? `fnv1a:${chunk.key.hash.toString(16).padStart(8, '0')}:${chunk.key.units}` : '';
      }
      route.send(message);
    });
  });
  return seen;
}

/** frontend/src/lib/utils/diffSourceKey.ts: FNV-1a over UTF-16 units. */
function sourceKeyOf(text: string): string {
  let hash = 0x811c9dc5;
  for (let index = 0; index < text.length; index += 1) hash = Math.imul(hash ^ text.charCodeAt(index), 0x01000193) >>> 0;
  return `fnv1a:${hash.toString(16).padStart(8, '0')}:${text.length}`;
}

function largeFiles(): Record<string, string> {
  const files: Record<string, string> = {};
  for (let file = 0; file < FILES; file += 1) {
    const lines = Array.from({ length: LINES }, (_, index) => lineText(file, index + 1));
    files[`big/${fileName(file)}.txt`] = `${lines.join('\n')}\n`;
  }
  return files;
}

async function seedLargePullRequest(harness: HarnessApp): Promise<void> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `large-diff-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  const files = largeFiles();
  const bytes = Object.values(files).reduce((sum, content) => sum + content.length, 0);
  expect(bytes, 'the added lines alone pass 10 MiB').toBeGreaterThan(10 * 1024 * 1024);
  await publishPullRequest(
    harness,
    seed.projects[0].path,
    { forge: 'github', project: `ao-e2e/large-diff-${randomBytes(4).toString('hex')}`, pulls: [{ number: 7, title: 'Large diff' }] },
    files,
  );
}

test('a pull request diff past 10 MiB and past the browser height limit opens and reaches every file', async ({
  harness,
  page,
}) => {
  test.setTimeout(180_000);
  await seedLargePullRequest(harness);
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);

  await expect(review.getByTestId('review-diff-stats')).toHaveText(`${FILES} files +${FILES * LINES}`, { timeout: 60_000 });
  await expect(review.getByTestId('review-error')).toHaveCount(0);
  // Files over 5000 rows start collapsed.
  const expandAll = review.getByTestId('review-collapse-all-toggle');
  await expect(expandAll).toHaveAccessibleName('Expand all files');
  await expandAll.click();
  const scroll = review.getByTestId('review-scroll');
  await expect(review.getByText(lineText(0, 1))).toBeInViewport();
  expect(await scroll.evaluate((el) => el.scrollHeight)).toBeLessThanOrEqual(HELD_LIMIT_PX);

  const tree = review.getByTestId('review-tree-toggle');
  if ((await tree.getAttribute('aria-pressed')) !== 'true') await tree.click();

  const last = FILES - 1;
  await review.locator(`[data-testid="review-tree-file"][data-file-path="big/${fileName(last)}.txt"]`).click();
  await expect(review.getByText(lineText(last, 1))).toBeInViewport();
  // Moves of the held range can follow the reader to the end; scroll
  // until the last line is on screen.
  const lastLine = review.getByText(lineText(last, LINES));
  await expect
    .poll(async () => {
      await scroll.evaluate((el) => {
        el.scrollTop = el.scrollHeight;
      });
      return (await lastLine.count()) > 0 && (await lastLine.isVisible());
    })
    .toBe(true);
  await expect(lastLine).toBeInViewport();
  expect(await scroll.evaluate((el) => el.scrollHeight)).toBeLessThanOrEqual(HELD_LIMIT_PX);

  await review.locator(`[data-testid="review-tree-file"][data-file-path="big/${fileName(0)}.txt"]`).click();
  await expect(review.getByText(lineText(0, 1))).toBeInViewport();
  await expect(review.getByTestId('review-error')).toHaveCount(0);
  await expectEveryForgeCallHandled(harness);
});

test('a workspace diff larger than one read saves a draft under the key of the text the backend served', async ({ harness, page }) => {
  test.setTimeout(120_000);
  const title = 'Keyed diff thread';
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: `keyed-diff-${randomBytes(4).toString('hex')}`,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
      threads: [{ title, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  // 3 MiB of added text, more than one 2 MiB read, with characters past
  // ASCII in the part read after the open's chunk.
  const big = Array.from({ length: 60_000 }, (_, index) => `${String(index).padStart(8, '0')} ${'x'.repeat(40)}${index % 5000 === 4999 ? WIDE_TAIL : ''}`).join('\n');
  expect(big.length).toBeGreaterThan(2 * 1024 * 1024);
  const root = seeded.projects[0].path;
  await writeFile(path.join(root, 'a-notes.txt'), 'first small line\nsecond small line\n');
  await writeFile(path.join(root, 'big.txt'), `${big}\n`);

  const seen = await watchDiff(page);
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: title }).click();
  const toggle = page.getByTestId('review-toggle');
  await expect(toggle).toBeVisible({ timeout: 30_000 });
  await toggle.click();
  const review = page.locator('section[data-pane-kind="review"]');
  await expect(review.getByTestId('review-diff-stats')).toContainText('2 files', { timeout: 60_000 });

  const row = review.locator('[data-testid="review-line-block"] > div').filter({ hasText: 'first small line' });
  await row.hover();
  await row.getByTestId('review-add-comment').click();
  const editor = review.getByTestId('review-draft-editor');
  await editor.locator('textarea').fill('Keyed by the whole patch.');
  await editor.getByTestId('review-draft-editor-add').click();
  await expect(review.getByTestId('review-send-strip')).toContainText('1 draft');

  expect(seen.packedChunks).toBeGreaterThan(0);
  expect(seen.text).toContain(WIDE_TAIL);
  expect(seen.text.length).toBeGreaterThan(big.length);
  expect(seen.lastChunkKey).toBe(sourceKeyOf(seen.text));
  expect(seen.draftKey).toBe(seen.lastChunkKey);
  await expect(review.getByTestId('review-error')).toHaveCount(0);
});
