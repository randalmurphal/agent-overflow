// A CI job log far past what one response or the view could hold, through
// the shipped UI, the real backend and the fake forge. Covered: a GitHub
// log of more than 16 MB downloads whole into the backend's cache and its
// first line is readable, with no tail cut; the view reads lines only for
// the rows it mounts, shows a placeholder of the right height while a
// block's read is in flight, and holds at most CI_LOG_BLOCK_CACHE_MAX
// blocks however far the reader jumps; Save to file writes the whole log;
// a section within the inline size goes to the chat as its text and a
// larger one as a file the backend saved with every line of it.

import { randomBytes } from 'node:crypto';
import { readFile, stat } from 'node:fs/promises';

import type { Locator } from '@playwright/test';

import { expect, test, type SeedResult } from './fixtures.js';
import {
  expectEveryForgeCallHandled,
  interceptCILogReads,
  openPullRequestReview,
  publishPullRequest,
  type ForgeStep,
} from './forge-helpers.js';

const TITLE = 'Large CI log thread';
// CI_LOG_BLOCK_CACHE_MAX in frontend/src/lib/stores/ciLogBlocks.svelte.ts.
const BLOCK_CACHE_MAX = 24;
const LINE_BYTES = 80;
// The first step stops 9 lines short of the first 200-line block's end,
// so the second step's first row is those 9 lines and its next row is a
// block of its own.
const SETUP_LINES = 190;
// 215,000 lines of 80 bytes: 17.2 MB, over the old 16 MB download cap.
const TEST_LINES = 215_000;
const COMPLETE_LINES = 20;

const STEPS: ForgeStep[] = [
  { number: 1, name: 'Set up job', status: 'success', startedAt: '2026-01-01T00:00:00Z', completedAt: '2026-01-01T00:00:05Z' },
  { number: 2, name: 'Run make test', status: 'success', startedAt: '2026-01-01T00:00:05Z', completedAt: '2026-01-01T00:10:00Z' },
  { number: 3, name: 'Complete job', status: 'success', startedAt: '2026-01-01T00:10:00Z', completedAt: '2026-01-01T00:10:01Z' },
];

function section(log: Locator, key: string): Locator {
  return log.locator(`[data-testid="review-ci-section"][data-key="${key}"]`);
}

/** Placeholders that intersect the log's viewport. */
async function placeholdersInView(scroll: Locator): Promise<number> {
  return scroll.evaluate((el) => {
    const view = el.getBoundingClientRect();
    return [...el.querySelectorAll('[data-testid="review-ci-log-block-placeholder"]')].filter((holder) => {
      const rect = holder.getBoundingClientRect();
      return rect.bottom > view.top && rect.top < view.bottom;
    }).length;
  });
}

async function cachedBlocks(log: Locator): Promise<number> {
  return Number(await log.getAttribute('data-cached-blocks'));
}

function lineCount(text: string): number {
  let count = 0;
  for (let at = text.indexOf('\n'); at >= 0; at = text.indexOf('\n', at + 1)) count += 1;
  return count;
}

test('a GitHub log over 16 MB reads from its top a block at a time, saves whole, and sends a section of any size', async ({
  harness,
  page,
}, testInfo) => {
  test.setTimeout(180_000);
  const lines = await interceptCILogReads(page);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: `ci-large-${randomBytes(4).toString('hex')}`,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
      threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  await publishPullRequest(harness, seed.projects[0].path, {
    forge: 'github',
    project: `ao-e2e/ci-${randomBytes(4).toString('hex')}`,
    pulls: [{
      number: 23,
      title: 'Large log',
      ci: {
        id: 9_300,
        name: 'CI',
        jobs: [{
          id: 7_300,
          name: 'build',
          status: 'success',
          steps: STEPS,
          synthetic: {
            lineBytes: LINE_BYTES,
            sections: [
              { name: 'Set up job', lines: SETUP_LINES },
              { name: 'Run make test', lines: TEST_LINES },
              { name: 'Complete job', lines: COMPLETE_LINES },
            ],
          },
        }],
      },
    }],
  });
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);
  await review.getByTestId('review-ci-chip').filter({ hasText: 'CI' }).click();
  await page.getByTestId('review-ci-job').filter({ hasText: 'build' }).click();
  const log = review.getByTestId('review-ci-log');
  const scroll = log.getByTestId('review-ci-log-scroll');
  const setup = section(log, 'step:1');
  const test2 = section(log, 'step:2');

  // The whole log is cached once its steps have lines; nothing is read
  // while every row is collapsed.
  await expect(log.getByTestId('review-ci-section')).toHaveCount(3);
  await expect(test2.getByTestId('review-ci-section-toggle')).toBeEnabled({ timeout: 60_000 });
  await expect(log.getByTestId('review-ci-log-downloading')).toHaveCount(0);
  await expect(log.getByTestId('review-ci-log-error')).toHaveCount(0);
  expect(lines.reads).toBe(0);

  // The top of the log is readable: the old view showed only its tail.
  await setup.getByTestId('review-ci-section-toggle').click();
  await expect(scroll).toContainText('##[group]Set up job');
  await expect(scroll).toContainText('set_up_job_0 line 1 ');
  await expect(scroll).toContainText(`set_up_job_0 line ${SETUP_LINES} `);
  await expect.poll(() => lines.reads).toBe(1);
  await setup.getByTestId('review-ci-section-toggle').click();
  await expect(scroll).not.toContainText('set_up_job_0 line 1 ');

  // The big step opens from its header. Its first row's lines are in the
  // block the first step read; the next row's block is still being read,
  // and holds its lines' height as a placeholder.
  lines.hold();
  await test2.getByTestId('review-ci-section-toggle').click();
  await expect(test2).toHaveAttribute('data-open', 'true');
  await expect(scroll).toContainText('##[group]Run make test');
  await expect(scroll).toContainText('run_make_test_1 line 8 ');
  await expect.poll(() => placeholdersInView(scroll)).toBeGreaterThan(0);
  const holder = log.getByTestId('review-ci-log-block-placeholder').first();
  expect((await holder.boundingBox())?.height).toBe(200 * 18);
  await page.screenshot({ path: testInfo.outputPath('ci-log-store-large-log-placeholders.png') });

  lines.release();
  await expect(scroll).toContainText('run_make_test_1 line 9 ');
  await expect.poll(() => placeholdersInView(scroll)).toBe(0);

  // Jumps through the 17 MB step: each lands on lines read for it, and
  // the cache never holds more than its bound.
  let maxCached = await cachedBlocks(log);
  for (const fraction of [0.15, 0.35, 0.5, 0.65, 0.85, 0.999]) {
    await scroll.evaluate((el, at) => {
      el.scrollTop = Math.floor((el.scrollHeight - el.clientHeight) * at);
    }, fraction);
    await expect.poll(() => placeholdersInView(scroll)).toBe(0);
    await expect(scroll).toContainText(/run_make_test_1 line \d{4,6} /);
    maxCached = Math.max(maxCached, await cachedBlocks(log));
    expect(await cachedBlocks(log)).toBeLessThanOrEqual(BLOCK_CACHE_MAX);
  }
  expect(maxCached).toBeGreaterThan(2);
  expect(maxCached).toBeLessThanOrEqual(BLOCK_CACHE_MAX);
  // A few viewports of lines were read, never the log.
  expect(lines.reads).toBeLessThan(6 * BLOCK_CACHE_MAX);

  // Save to file writes every line.
  await log.getByTestId('review-ci-log-save').click();
  const saved = log.getByTestId('review-ci-log-saved');
  await expect(saved).toContainText('Saved to ', { timeout: 30_000 });
  const savedPath = (await saved.innerText()).replace(/^Saved to /, '').trim();
  expect((await stat(savedPath)).size).toBeGreaterThan(16 * 1024 * 1024);
  const savedText = await readFile(savedPath, 'utf8');
  expect(lineCount(savedText)).toBe(3 + SETUP_LINES + TEST_LINES + COMPLETE_LINES);
  expect(savedText).toContain('set_up_job_0 line 1 ');
  expect(savedText).toContain(`complete_job_2 line ${COMPLETE_LINES} `);

  // The small step goes to the chat as its text.
  const composer = page.getByLabel('Message Input');
  await scroll.evaluate((el) => {
    el.scrollTop = 0;
  });
  await setup.hover();
  await setup.getByTestId('review-ci-section-send').click();
  await expect(composer).toHaveValue(/Investigate `Set up job` \(success\) in CI job `build` \(CI\) on PR #23, status: success\./);
  await expect(composer).toHaveValue(new RegExp(`set_up_job_0 line ${SETUP_LINES} `));

  // The big one as a file the backend wrote, every line of it.
  await composer.fill('');
  await test2.hover();
  await test2.getByTestId('review-ci-section-send').click();
  await expect(composer).toHaveValue(/Investigate `Run make test` \(success\) in CI job `build` \(CI\) on PR #23, status: success\.\nSection log saved at: /);
  const sectionPath = (await composer.inputValue()).split('Section log saved at: ')[1]!.trim();
  const sectionText = await readFile(sectionPath, 'utf8');
  expect(lineCount(sectionText)).toBe(1 + TEST_LINES);
  expect(sectionText).toContain('##[group]Run make test');
  expect(sectionText).toContain(`run_make_test_1 line ${TEST_LINES} `);
  expect(sectionText).not.toContain('set_up_job_0');
  expect(sectionText).not.toContain('complete_job_2');

  await expectEveryForgeCallHandled(harness);
});
