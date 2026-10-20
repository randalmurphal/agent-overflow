// The review pane's chrome after the header rework, shared by the desktop
// and compact projects, through the shipped UI, the real backend and the
// fake forge. The merge request has a named author, an approval, a nine-stage pipeline, test files beside their sources and
// an open thread the forge calls outdated. Covered: the title bar is four
// lines (title; author and state; base ← head and verdicts; CI with its
// refresh wrapping among the chips) and the toolbar carries neither the
// state nor the refs; whitespace, wrap and split are checkbox rows of the
// toolbar's view-options menu and split takes effect; the file-tree
// toggle lives in the rail, which closes to a strip holding the toggle
// and on compact opens over the diff at full width and closes on a pick
// or on Back, with the pane still on screen;
// "Hide tests" in the tree's filter menu drops test files from the tree
// and, applied to the diff, from the diff's counts; the conversation
// refresh sits on the Conversation header and re-reads the threads; an
// outdated open thread stays unresolved, counted and resolvable with the
// outdated marker on its card.
import { randomBytes } from 'node:crypto';
import type { Locator, Page, TestInfo } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  expectEveryForgeCallHandled,
  expandReviewSection,
  publishPullRequest,
  type ForgeRepo,
} from './forge-helpers.js';

export interface LayoutSurface {
  /** The compact project: taps, the rail over the diff. */
  compact: boolean;
}

const TITLE = 'Layout thread';
const MR_NUMBER = 4323;

const STAGES = ['pre-build', 'test', 'unit-tests', 'report', 'build-images', 'tag-images', 'container-scan', 'build_packages', 'cleanup'];

function queueSource(): string {
  const lines = [
    "import { renderTemplate } from './template';",
    "import type { Action } from './action';",
    '',
    'export async function sendEmail(action: Action): Promise<void> {',
    '  const body = renderTemplate(action.template, action.vars);',
    '  await deliver(action.to, body);',
    '}',
    '',
  ];
  let step = 1;
  while (lines.length < 120) {
    lines.push(`async function deliver${step === 1 ? '' : step}(to: string, body: string): Promise<void> {`, '  await post(to, body);', '}', '');
    step += 1;
  }
  return `${lines.join('\n')}\n`;
}

const FILES: Record<string, string> = {
  'src/queue/send.ts': queueSource(),
  'src/queue/send.test.ts': "import { sendEmail } from './send';\n\ntest('sends', async () => {\n  await sendEmail(action());\n});\n",
  'src/queue/template.ts': 'export function renderTemplate(name: string, vars: Record<string, string>): string {\n  return name + JSON.stringify(vars);\n}\n',
  'internal/mail/mail.go': 'package mail\n\nfunc Send(to, body string) error {\n\treturn nil\n}\n',
  'internal/mail/mail_test.go': 'package mail\n\nimport "testing"\n\nfunc TestSend(t *testing.T) {\n\tif err := Send("a", "b"); err != nil {\n\t\tt.Fatal(err)\n\t}\n}\n',
};

function repo(): ForgeRepo {
  return {
    forge: 'gitlab',
    project: `ao-e2e/layout-${randomBytes(4).toString('hex')}`,
    pulls: [
      {
        number: MR_NUMBER,
        title: 'Action Queue: send emails built from an r3 email template',
        body: 'Sends the queued action as an email rendered from the template.',
        author: 'sabossedgh',
        authorName: 'Sawmon Abossedgh',
        threads: [
          {
            id: 'T_open',
            path: 'src/queue/send.ts',
            line: 5,
            comments: [{ author: 'rmurphy', authorName: 'Randy Murphy', body: 'Escape the template vars before rendering.', createdAt: '2026-10-18T15:00:00Z' }],
          },
          {
            id: 'T_resolved',
            path: 'src/queue/template.ts',
            line: 2,
            resolved: true,
            comments: [
              { author: 'rmurphy', authorName: 'Randy Murphy', body: 'Name this `renderEmail`?', createdAt: '2026-10-17T15:00:00Z' },
              { author: 'sabossedgh', authorName: 'Sawmon Abossedgh', body: 'Kept, it renders any template.', createdAt: '2026-10-17T16:00:00Z' },
            ],
          },
          {
            id: 'T_outdated',
            path: 'src/queue/send.ts',
            line: 6,
            outdated: true,
            comments: [{ author: 'rmurphy', authorName: 'Randy Murphy', body: 'Deliver should retry once on a 5xx.', createdAt: '2026-10-16T15:00:00Z' }],
          },
        ],
        reviews: [{ author: 'rmurphy', authorName: 'Randy Murphy', state: 'APPROVED', submittedAt: '2026-10-18T16:00:00Z' }],
        ci: {
          id: 7_700,
          jobs: STAGES.map((stage, index) => ({ id: 7_701 + index, name: stage, stage, status: 'success' as const, log: `${stage} ok\n` })),
        },
      },
    ],
  };
}

async function seedMergeRequest(harness: HarnessApp): Promise<void> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `layout-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  await publishPullRequest(harness, seed.projects[0].path, repo(), FILES);
}

/** The review pane on the MR, opened the way each layout offers it. */
async function openReview(page: Page, compact: boolean): Promise<Locator> {
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
    const prRow = menu.getByRole('menuitem', { name: `MR !${MR_NUMBER}` });
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
  await expect(review.getByTestId('review-file-header-path').first()).toBeVisible();
  await expect.poll(() => review.evaluate((el) => {
    const r = el.getBoundingClientRect();
    return r.left >= 0 && r.right <= window.innerWidth ? `${r.left}` : 'moving';
  })).not.toBe('moving');
  return review;
}

export function reviewLayoutFlow({ compact }: LayoutSurface): void {
  test('the title bar reads one fact per line, the view settings fold into a menu, the tree toggle lives in the rail and tests can be hidden', async ({ harness, page }, testInfo) => {
    await seedMergeRequest(harness);
    await harness.open(page);
    const review = await openReview(page, compact);
    const press = (target: Locator) => (compact ? target.tap() : target.click());
    const shot = (name: string) => page.screenshot({ path: testInfo.outputPath(`${testInfo.project.name}-${name}.png`) });
    const paneShot = async (name: string) => {
      const box = (await review.boundingBox())!;
      await page.screenshot({ path: testInfo.outputPath(`${testInfo.project.name}-${name}.png`), clip: box });
    };

    // The title bar: four lines, each saying one thing.
    const header = review.getByTestId('review-pr-header');
    await expect(header).toContainText('Action Queue: send emails');
    const authorLine = header.getByTestId('review-pr-author-line');
    await expect(authorLine.getByTestId('review-pr-author')).toContainText('Sawmon Abossedgh');
    await expect(authorLine.getByTestId('review-pr-state')).toHaveText('open');
    const refsLine = header.getByTestId('review-pr-refs-line');
    await expect(refsLine.getByTestId('review-pr-refs')).toHaveAttribute('title', 'main ← feature');
    await expect(refsLine).toContainText('approved');
    const ciLine = header.getByTestId('review-pr-ci-line');
    await expect(ciLine.getByTestId('review-ci-chip')).toHaveCount(STAGES.length, { timeout: 30_000 });
    // The refresh is an item of the chip row, so it wraps with the chips.
    await expect(ciLine.getByTestId('review-ci-chips').getByTestId('review-ci-refresh')).toBeVisible();
    const lines = [authorLine, refsLine, ciLine];
    const tops = await Promise.all(lines.map(async (line) => (await line.boundingBox())!.y));
    expect(tops[0]).toBeLessThan(tops[1]);
    expect(tops[1]).toBeLessThan(tops[2]);
    // The toolbar keeps the diff's counts and nothing of the PR's identity.
    const toolbar = review.getByTestId('review-scope-select').locator('..');
    await expect(toolbar.getByTestId('review-diff-stats')).toContainText('5 files');
    await expect(toolbar).not.toContainText('main ← feature');
    await expect(toolbar).not.toContainText('open');
    await paneShot('pane');

    // The view settings are checkbox rows of one menu; split takes effect.
    await press(review.getByTestId('review-view-options'));
    const menu = page.locator('[data-popover]').last();
    await expect(menu.getByTestId('review-ignore-whitespace-toggle')).toHaveAttribute('aria-checked', 'false');
    await expect(menu.getByTestId('review-wrap-toggle')).toHaveAttribute('aria-checked', /^(true|false)$/);
    await expect(menu.getByTestId('review-split-toggle')).toHaveAttribute('aria-checked', 'false');
    await shot('view-options');
    await press(menu.getByTestId('review-split-toggle'));
    await expect(review.getByTestId('review-line-block').first().locator('.w-1\\/2').first()).toBeVisible();
    await press(review.getByTestId('review-view-options'));
    await expect(page.locator('[data-popover]').last().getByTestId('review-split-toggle')).toHaveAttribute('aria-checked', 'true');
    await press(page.locator('[data-popover]').last().getByTestId('review-split-toggle'));
    await expect(review.getByTestId('review-line-block').first().locator('.w-1\\/2')).toHaveCount(0);

    // The tree toggle lives in the rail: closed, the rail is a strip.
    const rail = review.getByTestId('review-rail');
    // A pane under 700px (the phone, the desktop companion beside a chat)
    // first opens with the tree hidden: the strip's toggle shows it.
    if ((await rail.getAttribute('data-open')) === 'false') {
      await expect(rail.getByTestId('review-tree-toggle')).toHaveAttribute('aria-pressed', 'false');
      await press(rail.getByTestId('review-tree-toggle'));
    }
    await expect(rail).toHaveAttribute('data-open', 'true');
    await expect(rail.getByTestId('review-file-tree')).toBeVisible();
    const toggle = rail.getByTestId('review-tree-toggle');
    await expect(toggle).toHaveAttribute('aria-pressed', 'true');
    if (compact) {
      // Over the diff at the pane's full width; a pick closes it.
      const paneBox = (await review.boundingBox())!;
      const railBox = (await rail.boundingBox())!;
      // The pane's own border is the only width the rail does not take.
      expect(railBox.width).toBeGreaterThanOrEqual(paneBox.width - 4);
      expect(railBox.x).toBeLessThanOrEqual(paneBox.x + 2);
      await paneShot('rail-open');
      // Back closes the tree, the topmost layer, and nothing more: the
      // review pane stays on screen.
      await page.goBack();
      await expect(rail).toHaveAttribute('data-open', 'false');
      await expect(review.getByTestId('review-pr-header')).toBeVisible();
      await press(rail.getByTestId('review-tree-toggle'));
      await expect(rail).toHaveAttribute('data-open', 'true');
      await press(rail.getByTestId('review-tree-file').filter({ hasText: 'mail.go' }));
      await expect(rail).toHaveAttribute('data-open', 'false');
      await expect(review.getByTestId('review-file-header-path').filter({ hasText: 'mail.go' }).first()).toBeInViewport();
      await paneShot('rail-closed');
      await press(rail.getByTestId('review-tree-toggle'));
      await expect(rail).toHaveAttribute('data-open', 'true');
    } else {
      await press(toggle);
      await expect(rail).toHaveAttribute('data-open', 'false');
      await expect(rail.getByTestId('review-file-tree')).toHaveCount(0);
      const strip = rail.getByTestId('review-tree-toggle');
      await expect(strip).toHaveAttribute('aria-pressed', 'false');
      expect((await rail.boundingBox())!.width).toBeLessThan(48);
      await paneShot('rail-closed');
      await press(strip);
      await expect(rail).toHaveAttribute('data-open', 'true');
    }

    // "Hide tests" drops the two test files from the tree; applied to the
    // diff, from its counts too.
    await expect(rail.getByTestId('review-tree-file')).toHaveCount(5);
    await press(rail.getByTestId('review-tree-ext-trigger'));
    const filterMenu = page.locator('[data-popover]').last();
    await expect(filterMenu.getByTestId('review-tree-hide-tests')).toContainText('2');
    await shot('filter-menu');
    // The filter menu stays open across picks (several chips at once).
    await press(filterMenu.getByTestId('review-tree-hide-tests'));
    await expect(rail.getByTestId('review-tree-file')).toHaveCount(3);
    await expect(rail.getByTestId('review-tree-file').filter({ hasText: 'send.test.ts' })).toHaveCount(0);
    await expect(rail.getByTestId('review-tree-file').filter({ hasText: 'mail_test.go' })).toHaveCount(0);
    await press(filterMenu.getByTestId('review-tree-filter-diff'));
    await expect(toolbar.getByTestId('review-diff-stats')).toContainText('3 files');
    await expect(review.getByTestId('review-file-header-path').filter({ hasText: 'mail_test.go' })).toHaveCount(0);
    await paneShot('tests-hidden');
    // Clear filters is the pick that closes it.
    await press(filterMenu.getByText('Clear filters'));
    await expect(filterMenu).toHaveCount(0);
    await expect(toolbar.getByTestId('review-diff-stats')).toContainText('5 files');
    if (compact) await press(rail.getByTestId('review-tree-toggle'));

    // The outdated open thread: unresolved, counted, resolvable, marked.
    const conversation = await expandReviewSection(page, 'review-pr-conversation');
    await expect(conversation.getByTestId('review-conversation-open-count')).toContainText('2 unresolved');
    const outdated = review.locator('[data-testid="review-conversation-thread"][data-thread-id="T_outdated"]');
    await expect(outdated).toHaveAttribute('data-state', 'unresolved');
    await expect(outdated).toHaveAttribute('data-outdated', 'true');
    await expect(outdated.getByTestId('review-thread-outdated')).toHaveText('outdated');
    await expect(outdated.getByTestId('review-thread-resolve')).toHaveText('Resolve');
    await expect(review.locator('[data-testid="review-conversation-thread"][data-thread-id="T_open"]')).not.toHaveAttribute('data-outdated', 'true');
    await outdated.scrollIntoViewIfNeeded();
    await paneShot('outdated');

    // The conversation refresh is on the Conversation header and re-reads
    // the threads without touching the diff.
    const refresh = conversation.getByTestId('review-refresh-comments');
    await expect(refresh).toBeVisible();
    const topPath = await review.getByTestId('review-file-header-path').first().textContent();
    await press(refresh);
    await expect(refresh).toBeEnabled();
    await expect(conversation.getByTestId('review-conversation-open-count')).toContainText('2 unresolved');
    expect(await review.getByTestId('review-file-header-path').first().textContent()).toBe(topPath);
    await expect(toolbar.getByTestId('review-refresh-comments')).toHaveCount(0);

    await expectEveryForgeCallHandled(harness);
  });
}
