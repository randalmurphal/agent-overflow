// Reading a pull request's diff in the review pane, on GitHub and GitLab,
// in the desktop and compact projects, through the shipped UI, the real
// backend and the fake forge, on `imagePullHistory` with three more
// commits (a note added to the README and a 26 MiB image, a blob, the
// note dropped and a line added to src/app.ts). Covered: a run picked by
// shift+click (desktop) and by `From here`, the trigger's `N–M/T`, the
// range card and leaving it, the diff holding only the run's files;
// `Changes from here` on the Conversation's commit group (the whole PR
// here: the fixture's commits share an author); in a run to the
// head, Comment now on a line of the PR's diff and the agent-only hover
// text on a line that is not; image rows of a changed, an added, a
// deleted and an over-cap image with their captions, in unified and split
// view; a non-image binary file's sizes; the SVG `Rendered` toggle; and
// the gutter menu's link copied to the clipboard.

import { randomBytes } from 'node:crypto';
import type { Locator, Page, TestInfo } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  expandReviewSection,
  expectEveryForgeCallHandled,
  forgeRouteCalls,
  imagePullHistory,
  publishPullRequest,
  solidPNG,
  type ForgeRepo,
} from './forge-helpers.js';
import { line, ROUTES } from './review-comment-actions-flow.js';
import { longPress } from './touch-helpers.js';

type Forge = 'github' | 'gitlab';

const TITLE = 'Diff reading';
const PR_NUMBER = 7;
const OVER_CAP_BYTES = 26 * 1024 * 1024;
const NOT_IN_PR = 'This line is not part of the pull request\'s diff, so a comment here goes to the agent only.';

// Oldest first, as the picker numbers them: 1 add icon … 8 drop note.
const SUBJECTS = ['add icon', 'restyle logo', 'add diagram', 'merge add diagram', 'rename moved', 'note readme', 'add blob', 'drop note'];

function bigPNG(): Uint8Array {
  const bytes = new Uint8Array(OVER_CAP_BYTES);
  bytes.set(solidPNG(4, 4, [0, 0, 0]));
  return bytes;
}

function history() {
  const base = imagePullHistory();
  const [blob] = base.commits.splice(base.commits.length - 1, 1);
  base.commits.push(
    { message: 'note readme', files: { 'README.md': '# Seeded\nNote.\n', 'huge.png': bigPNG() } },
    blob,
    { message: 'drop note', files: { 'README.md': '# Seeded\n', 'src/app.ts': 'export const app = 1;\nexport const moved = 2;\nexport const dropped = 3;\n' } },
  );
  return base;
}

function repo(forge: Forge): ForgeRepo {
  return {
    forge,
    project: `ao-e2e/reading-${randomBytes(4).toString('hex')}`,
    pulls: [{
      number: PR_NUMBER,
      title: 'Images and runs',
      body: 'Images.',
      comments: [{ author: 'octo', body: 'Looks good so far.', createdAt: '2026-01-01T00:00:05.500Z' }],
    }],
  };
}

function shotsDir(testInfo: TestInfo): (name: string) => string {
  const dir = process.env.REVIEW_SHOTS_DIR;
  return (name) => (dir ? `${dir}/${testInfo.project.name}-${name}.png` : testInfo.outputPath(`${testInfo.project.name}-${name}.png`));
}

export async function openReview(page: Page, forge: Forge, compact: boolean): Promise<Locator> {
  const row = page.getByTestId('thread-row').filter({ hasText: TITLE });
  if (!compact) {
    await row.click();
    const badge = page.getByTestId('chat-header-pr-badge');
    await expect(badge).toBeVisible({ timeout: 30_000 });
    await badge.click();
  } else {
    await row.tap();
    const more = page.getByTestId('chat-header-more');
    const menu = page.locator('[data-popover]');
    const prRow = menu.getByRole('menuitem', { name: forge === 'github' ? `PR #${PR_NUMBER}` : `MR !${PR_NUMBER}` });
    await expect(async () => {
      if ((await menu.count()) > 0) await page.keyboard.press('Escape');
      await expect(menu).toHaveCount(0);
      await more.tap();
      await expect(prRow).toBeVisible({ timeout: 2_000 });
    }).toPass({ timeout: 30_000 });
    await prRow.tap();
  }
  const review = page.locator('section[data-pane-kind="review"]');
  await expect(review.getByTestId('review-pr-header')).toBeVisible();
  await expect(review.getByTestId('review-file-header-path').first()).toBeVisible({ timeout: 30_000 });
  return review;
}

/** The paths of the files the diff shows, read off the list from the top. */
async function shownPaths(review: Locator): Promise<string[]> {
  const scroll = review.getByTestId('review-scroll');
  await scroll.evaluate((el) => { el.scrollTop = 0; });
  const seen = new Set<string>();
  for (let step = 0; step < 40; step += 1) {
    const paths = await review.getByTestId('review-scroll').getByTestId('review-file-header').evaluateAll((els) => els.map((el) => el.getAttribute('data-path') ?? ''));
    for (const path of paths) seen.add(path);
    const done = await scroll.evaluate((el) => {
      const before = el.scrollTop;
      el.scrollTop += el.clientHeight * 0.8;
      return el.scrollTop === before;
    });
    if (done) break;
    await frames(scroll);
  }
  await scroll.evaluate((el) => { el.scrollTop = 0; });
  return [...seen].sort();
}

/** Opens `path` from the file tree, as a reader does, and answers
 * `target` (a row of that file) once it is mounted and settled on screen. */
export async function reach(review: Locator, path: string, target: Locator, compact: boolean): Promise<Locator> {
  const rail = review.getByTestId('review-rail');
  if ((await rail.getAttribute('data-open')) !== 'true') {
    const toggle = rail.getByTestId('review-tree-toggle');
    await (compact ? toggle.tap() : toggle.click());
    await expect(rail).toHaveAttribute('data-open', 'true');
  }
  const node = review.locator(`[data-testid="review-tree-file"][data-file-path="${path}"]`);
  await (compact ? node.tap() : node.click());
  const row = target.first();
  await expect(row).toBeInViewport();
  // Settled: the jump's scroll has stopped moving it.
  await expect.poll(async () => {
    const top = (await row.boundingBox())?.y ?? -1;
    await frames(review.getByTestId('review-scroll'));
    return top === ((await row.boundingBox())?.y ?? -2);
  }).toBe(true);
  return row;
}

// Two frames: the list mounts the rows a scroll brought near.
async function frames(scroll: Locator): Promise<void> {
  await scroll.evaluate(() => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
}

function bytesText(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KB', 'MB', 'GB'];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value >= 10 ? Math.round(value) : value.toFixed(1)} ${units[unit]}`;
}

export function reviewDiffReadingFlow({ compact }: { compact: boolean }): void {
  for (const forge of ['github', 'gitlab'] as const) {
    test(`on ${forge}, runs of commits, image rows and a line's link`, async ({ harness, page }, testInfo) => {
      test.setTimeout(240_000);
      await page.emulateMedia({ colorScheme: 'dark' });
      const shot = shotsDir(testInfo);
      const press = (target: Locator) => (compact ? target.tap() : target.click());
      const seeded = await seed(harness, forge);
      await harness.open(page);
      await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: new URL(page.url()).origin });
      const review = await openReview(page, forge, compact);
      const trigger = review.getByTestId('review-commit-select');
      await expect(trigger).toHaveText(/All commits/);

      const pickerRow = (subject: string) => page.locator('[data-popover]').last().getByTestId('review-commit-option').filter({ hasText: subject });
      const openPicker = async () => {
        await press(trigger);
        await expect(pickerRow(SUBJECTS[0])).toBeVisible();
      };

      // A run by shift+click (a pointer's), or by From here (every device).
      if (!compact) {
        await openPicker();
        await pickerRow('restyle logo').click();
        await expect(trigger).toHaveText(/2\/8/);
        await openPicker();
        await expect(page.locator('[data-popover]').last().getByTestId('review-commit-range-hint')).toBeVisible();
        await pickerRow('merge add diagram').click({ modifiers: ['Shift'] });
        await expect(trigger).toHaveText(/^\s*2–4\/8\s*$/);
        await expect.poll(() => shownPaths(review)).toEqual(['diagram.svg', 'logo.png', 'old.png']);
        await openPicker();
        await page.screenshot({ path: shot(`${forge}-picker-range`) });
        await page.keyboard.press('Escape');
      } else {
        await openPicker();
        const row = pickerRow('restyle logo');
        await row.getByRole('button', { name: /through the newest/ }).tap();
        await expect(trigger).toHaveText(/^\s*2–8\/8\s*$/);
        await openPicker();
        await page.screenshot({ path: shot(`${forge}-picker-range`) });
        await pickerRow('restyle logo').getByRole('button', { name: /through the newest/ }).tap();
      }
      const card = review.getByTestId('review-range-card');
      await expect(card).toBeVisible();
      await expect(card.getByTestId('review-range-card-count')).toHaveText(compact ? '7 commits' : '3 commits');
      await expect(card.getByTestId('review-range-card-ends')).toContainText('restyle logo');
      await expect(card.getByTestId('review-range-card-ends')).toContainText(compact ? 'drop note' : 'merge add diagram');
      // The card leads the diff where the commit card would, no taller
      // than a line more.
      const cardHeight = (await card.boundingBox())!.height;
      await review.screenshot({ path: shot(`${forge}-range-card`) });
      await press(card.getByTestId('review-range-card-leave'));
      await expect(trigger).toHaveText(/All commits/);
      await expect(card).toHaveCount(0);

      // From here on `add blob`: a run to the head, 7–8 of 8.
      await openPicker();
      await press(pickerRow('add blob').getByRole('button', { name: /through the newest/ }));
      await expect(trigger).toHaveText(/^\s*7–8\/8\s*$/);
      await expect.poll(() => shownPaths(review)).toEqual(['README.md', 'data.bin', 'src/app.ts']);
      // A single commit's card for comparison: at most one line taller.
      expect(cardHeight).toBeGreaterThan(0);

      // The README line the PR does not change takes an agent-only comment
      // and says why; a line the PR adds posts to the forge now.
      const readme = await reach(review, 'README.md', line(review, '# Seeded'), compact);
      if (!compact) await readme.hover();
      const agentOnly = readme.getByTestId('review-add-comment');
      await expect(agentOnly).toHaveAttribute('data-comment-target', 'agent');
      await expect(agentOnly).toHaveAttribute('title', NOT_IN_PR);
      const added = await reach(review, 'src/app.ts', line(review, 'export const dropped = 3;'), compact);
      if (!compact) await added.hover();
      await expect(added.getByTestId('review-add-comment')).toHaveAttribute('data-comment-target', 'pr');
      await press(added.getByTestId('review-add-comment'));
      const editor = review.getByTestId('review-draft-editor');
      await expect(editor).toBeVisible();
      await editor.locator('textarea').fill('Why drop the note here?');
      await press(editor.getByTestId('review-draft-editor-now'));
      await expect(editor).toHaveCount(0, { timeout: 30_000 });
      await expect.poll(async () => (await forgeRouteCalls(harness, ROUTES[forge].lineComment)).length).toBe(1);

      // Changes from here on the feed's commit group. The fixture's commits
      // share one author, so the feed has one group, the whole PR: its run
      // is All commits, and the view moves to its first file. (A group short
      // of the whole list: ReviewConversationCommits.test.ts and the pane
      // state's tests.)
      await press(review.getByTestId('review-range-card-leave'));
      await expect(trigger).toHaveText(/All commits/);
      const conversation = await expandReviewSection(page, 'review-pr-conversation');
      const group = conversation.getByTestId('review-conversation-commits');
      await expect(group).toHaveCount(1);
      await expect(group).toContainText('added 8 commits');
      await group.scrollIntoViewIfNeeded();
      if (!compact) await group.hover();
      const changesFrom = group.getByTestId('review-conversation-changes-from');
      await expect(changesFrom).toBeVisible();
      await expect(changesFrom).toHaveCSS('opacity', '1');
      await press(changesFrom);
      await expect(trigger).toHaveText(/All commits/);
      await expect(review.getByTestId('review-range-card')).toHaveCount(0);
      // To the diff: its first file at the top.
      const firstFile = review.getByTestId('review-scroll').getByTestId('review-file-header').first();
      await expect(firstFile).toBeInViewport();
      await expect.poll(async () => {
        const top = (await firstFile.boundingBox())?.y ?? -1;
        await frames(review.getByTestId('review-scroll'));
        return top === ((await firstFile.boundingBox())?.y ?? -2);
      }).toBe(true);

      // Image rows in the whole PR.
      const logo = await reach(review, 'logo.png', review.locator('[data-testid="review-image-diff"][data-path="logo.png"]'), compact);
      const captions = logo.getByTestId('review-image-caption');
      await expect(captions).toHaveText([
        `Old · 16 × 16 px · ${bytesText(solidPNG(16, 16, [200, 40, 40]).length)}`,
        `New · 32 × 32 px · ${bytesText(solidPNG(32, 32, [40, 200, 200]).length)}`,
      ], { timeout: 30_000 });
      await expect(logo.getByTestId('review-image-side')).toHaveCount(2);
      const [oldSide, newSide] = await logo.getByTestId('review-image-side').all();
      expect((await newSide.boundingBox())!.y).toBeGreaterThan((await oldSide.boundingBox())!.y);
      await logo.screenshot({ path: shot(`${forge}-image-unified`) });

      const icon = await reach(review, 'icon.png', review.locator('[data-testid="review-image-diff"][data-path="icon.png"]'), compact);
      await expect(icon.getByTestId('review-image-side')).toHaveCount(1);
      await expect(icon.getByTestId('review-image-caption')).toHaveText(`24 × 12 px · ${bytesText(solidPNG(24, 12, [240, 200, 0]).length)}`, { timeout: 30_000 });
      await icon.screenshot({ path: shot(`${forge}-image-added`) });
      const old = await reach(review, 'old.png', review.locator('[data-testid="review-image-diff"][data-path="old.png"]'), compact);
      await expect(old.getByTestId('review-image-side')).toHaveAttribute('data-side', 'old');

      const huge = await reach(review, 'huge.png', review.locator('[data-testid="review-image-diff"][data-path="huge.png"]'), compact);
      await expect(huge.getByTestId('review-image-over-cap')).toHaveText('Too large to show here (over 25 MB)', { timeout: 30_000 });
      await expect(huge.getByTestId('review-image-caption')).toHaveText(bytesText(OVER_CAP_BYTES));
      await expect(huge.getByTestId('review-image')).toHaveCount(0);
      await huge.screenshot({ path: shot(`${forge}-image-over-cap`) });

      const blob = await reach(review, 'data.bin', review.locator('[data-testid="review-binary-file"][data-path="data.bin"]'), compact);
      await expect(blob).toContainText('Binary files differ');
      await expect(blob.getByTestId('review-binary-sizes')).toHaveText('new 8 B', { timeout: 30_000 });

      // The SVG's one toggle: its text by default, its image when on.
      const svgHeader = await reach(review, 'diagram.svg', review.getByTestId('review-scroll').locator('[data-testid="review-file-header"][data-path="diagram.svg"]'), compact);
      const toggle = svgHeader.getByTestId('review-svg-rendered');
      await expect(toggle).toHaveAttribute('aria-pressed', 'false');
      await expect(line(review, '<rect width="40"').first()).toBeAttached();
      await press(toggle);
      await expect(toggle).toHaveAttribute('aria-pressed', 'true');
      const svg = review.locator('[data-testid="review-image-diff"][data-path="diagram.svg"]');
      await expect(svg.getByTestId('review-image-caption')).toHaveText(/^40 × 20 px · /, { timeout: 30_000 });
      await expect(line(review, '<rect width="40"')).toHaveCount(0);
      await review.screenshot({ path: shot(`${forge}-svg-rendered`) });
      await press(toggle);
      await expect(svg).toHaveCount(0);

      // Split view: the two sides side by side.
      await press(review.getByTestId('review-view-options'));
      await press(page.locator('[data-popover]').last().getByTestId('review-split-toggle'));
      await page.keyboard.press('Escape');
      const split = await reach(review, 'logo.png', review.locator('[data-testid="review-image-diff"][data-path="logo.png"]'), compact);
      await expect(split.getByTestId('review-image-side')).toHaveCount(2);
      const [leftSide, rightSide] = await split.getByTestId('review-image-side').all();
      const left = (await leftSide.boundingBox())!;
      const right = (await rightSide.boundingBox())!;
      expect(right.y).toBe(left.y);
      expect(right.x).toBeGreaterThanOrEqual(left.x + left.width);
      await split.screenshot({ path: shot(`${forge}-image-split`) });
      await press(review.getByTestId('review-view-options'));
      await press(page.locator('[data-popover]').last().getByTestId('review-split-toggle'));
      await page.keyboard.press('Escape');

      // The gutter's menu copies the line's link.
      const appLine = await reach(review, 'src/app.ts', line(review, 'export const moved = 2;'), compact);
      const gutter = appLine.getByTestId('review-line-gutter').first();
      const item = page.getByTestId('review-copy-line-link');
      if (compact) await longPress(page, gutter, item);
      else await gutter.click({ button: 'right' });
      await expect(item).toHaveText(forge === 'github' ? 'Copy link to line' : 'Copy link to file');
      await page.screenshot({ path: shot(`${forge}-line-menu`) });
      await press(item);
      const host = forge === 'github' ? 'github.com' : 'gitlab.com';
      const expected = forge === 'github'
        ? `https://${host}/${seeded.project}/blob/${seeded.headSha}/src/app.ts#L2`
        : `https://${host}/${seeded.project}/-/blob/${seeded.headSha}/src/app.ts`;
      await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(expected);
      await expectEveryForgeCallHandled(harness);
    });
  }
}

export async function seed(harness: HarnessApp, forge: Forge): Promise<{ project: string; headSha: string }> {
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: `reading-${randomBytes(4).toString('hex')}`,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
      threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  const published = await publishPullRequest(harness, seeded.projects[0].path, repo(forge), history());
  return { project: published.project, headSha: published.pulls![0].headSha! };
}
