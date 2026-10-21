// The Pull requests page (docs/specs/pull-requests.md, "The page" and
// "Pipelines tab"), shared by the desktop and compact projects, through the
// shipped UI, the real backend and the fake forge: a GitLab project and a
// GitHub project, each with open merge requests in several attention
// states and repository pipelines. Covered: the tools row opens the page
// in the focused pane's place; the Needs you and Waiting on others groups
// carry one state pill per row and the sidebar badge counts the first
// group; the page reads each project with its forge's fixed request set
// (GitHub one PullsSearch, GitLab the list beside its GraphQL reviewer,
// activity and approval rule reads and the viewer's events) and never a
// read per row; a GitLab row's failed pipeline arrives with its project's
// read; the filter chips and the project picker narrow the list and
// survive a reload; picking a row opens the review pinned to it beside the
// list (full width with Back on compact) with its readiness pill and Merge
// section; the viewer's own comment on a merge request it reviews reads as
// reviewed by them, and the author's reply after it as their turn again,
// from the forge's record of the viewer's activity; a project whose list
// the forge refuses fails alone with its sentence, Details and a Retry; a
// row's menu acts on its MR without opening it; Open with agent starts a
// thread in a worktree with the MR's branch checked out at its head and
// the seeded draft, and afterwards lists it on the MR and refuses a second
// worktree; the Pipelines tab lists the repositories' pipelines with their
// refs, filters by ref kind, project and status (the last two survive a
// reload), and opens a GitLab and a GitHub pipeline's stages and a job's
// log beside the list (on compact the header's Back is the only Back and
// closes the log first).

import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import path from 'node:path';
import type { Locator, Page, TestInfo } from '@playwright/test';

import { HARNESS_TIMING, launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  clearForgeRefusal,
  expectEveryForgeCallHandled,
  forgeInvocations,
  publishPullRequest,
  refuseForgeRoute,
  seedForge,
  type ForgeRepo,
} from './forge-helpers.js';
import { harnessGit } from './worktree-removal-helpers.js';

export interface PullsSurface {
  compact: boolean;
}

const VIEWER = 'ao-viewer';
const SHA = (seed: string): string => seed.repeat(40).slice(0, 40);
const PIPELINE_JOBS = (status: 'success' | 'failed' | 'running') => [
  { id: 1, name: 'build', stage: 'build', status: 'success' as const, log: 'built\n' },
  { id: 2, name: 'unit', stage: 'test', status, log: 'unit tests ran\nall green\n' },
];

function gitlabRepo(): ForgeRepo {
  const suffix = randomBytes(4).toString('hex');
  return {
    forge: 'gitlab',
    id: 4_500_000 + parseInt(suffix.slice(0, 4), 16),
    project: `ao-e2e/widgets-${suffix}`,
    merge: { pipelineMustSucceed: true },
    pulls: [
      {
        number: 101, title: 'Queue: retry a failed delivery once', author: 'sabossedgh', authorName: 'Sawmon Abossedgh',
        reviewers: [VIEWER], readiness: { requiredApprovals: 1 },
        comments: [{ author: 'alice', authorName: 'Alice Ng', body: 'One question on the backoff.', createdAt: '2026-10-18T15:00:00Z' }],
        ci: { id: 45_101, jobs: PIPELINE_JOBS('running') },
        updatedAt: '2026-10-19T12:00:00Z',
      },
      { number: 102, title: 'Draft: speed up builds', draft: true, author: VIEWER, headRef: 'builds', headSha: SHA('2'), updatedAt: '2026-10-18T12:00:00Z' },
      { number: 103, title: 'Tenable rules: crash on empty filter', author: VIEWER, headRef: 'tenable', headSha: SHA('3'), ci: { id: 45_103, jobs: PIPELINE_JOBS('failed') }, updatedAt: '2026-10-17T12:00:00Z' },
      { number: 104, title: 'Postern: retry budget for webhooks', author: 'alice', authorName: 'Alice Ng', headRef: 'postern', headSha: SHA('4'), updatedAt: '2026-10-16T12:00:00Z' },
    ],
    pipelines: [
      { id: 9_001, refKind: 'branch', ref: 'main', sha: SHA('a'), status: 'success', title: 'Merge !100', jobs: PIPELINE_JOBS('success') },
      { id: 9_002, refKind: 'tag', ref: 'v3.9.2', sha: SHA('b'), status: 'failed', title: 'release', jobs: PIPELINE_JOBS('failed') },
    ],
  };
}

function githubRepo(): ForgeRepo {
  const suffix = randomBytes(4).toString('hex');
  return {
    forge: 'github',
    project: `ao-e2e/gadgets-${suffix}`,
    merge: { autoMergeAllowed: true },
    pulls: [
      {
        number: 201, title: 'Rework the review pane header', author: VIEWER,
        readiness: { requiredApprovals: 1 },
        reviews: [{ author: 'rmurphy', authorName: 'Randy Murphy', state: 'APPROVED', submittedAt: '2026-10-19T09:00:00Z' }],
        ci: { id: 46_201, jobs: PIPELINE_JOBS('success') },
        updatedAt: '2026-10-19T10:00:00Z',
      },
      { number: 202, title: 'Trim transcript retention', author: 'bob', authorName: 'Bob Lee', reviewers: [VIEWER], headRef: 'retention', headSha: SHA('5'), updatedAt: '2026-10-18T10:00:00Z' },
    ],
    pipelines: [
      { id: 7_001, name: 'CI', refKind: 'branch', ref: 'main', sha: SHA('c'), status: 'running', title: 'CI', jobs: PIPELINE_JOBS('running') },
    ],
  };
}

interface Seeded {
  gitlab: ForgeRepo;
  github: ForgeRepo;
  gitlabPath: string;
}

async function seedProjects(harness: HarnessApp): Promise<Seeded> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'widgets', repo: { commits: [{ message: 'init', files: { 'README.md': '# Widgets\n' } }] }, threads: [] },
      { name: 'gadgets', repo: { commits: [{ message: 'init', files: { 'README.md': '# Gadgets\n' } }] }, threads: [] },
    ],
  });
  const gitlab = await publishPullRequest(harness, seed.projects[0].path, gitlabRepo(), {
    'src/queue/deliver.ts': 'export async function deliver(): Promise<void> {\n  await post();\n}\n',
  });
  const github = await publishPullRequest(harness, seed.projects[1].path, githubRepo(), {
    'src/review/header.ts': 'export const header = 1;\n',
  });
  // publishPullRequest leaves the checkout on the PR's branch; a project
  // root sits on main, which is what lets a thread check the branch out.
  const env = { ...process.env, HOME: path.join(harness.bootstrap.dataRoot, 'home'), GIT_CONFIG_NOSYSTEM: '1' };
  for (const project of seed.projects) {
    execFileSync('git', ['checkout', '--quiet', 'main'], { cwd: project.path, env, encoding: 'utf8' });
  }
  return { gitlab, github, gitlabPath: seed.projects[0].path };
}

export function launchPullsHarness(): Promise<HarnessApp> {
  // The list pump's product cadence is 45 s; the activity and refusal
  // steps wait for a tick.
  return launchHarness({ env: { AO_HARNESS_TIMING: [...HARNESS_TIMING.split(','), 'pr-update=1s'].join(',') } });
}

function row(page: Page, number: number): Locator {
  // The row key ends in the PR number; the numbers here are distinct.
  return page.locator(`[data-testid="pull-row"][data-pr-key$=":${number}"]`);
}

function rowState(page: Page, number: number): Locator {
  return row(page, number).getByTestId('pull-state');
}

/** Picks a filter from the toolbar's filter menu. */
async function pickFilter(page: Page, pane: Locator, value: 'needs_you' | 'mine' | 'reviewing' | 'all'): Promise<void> {
  await pane.getByTestId('pulls-filter').click();
  await page.getByTestId(`pulls-filter-${value}`).click();
  await expect(pane.getByTestId('pulls-filter')).toHaveAttribute('data-filter', value);
}

async function useDarkTheme(page: Page): Promise<void> {
  await page.emulateMedia({ colorScheme: 'dark' });
  await expect(page.locator('html')).toHaveClass(/\bdark\b/);
}

async function shoot(target: Page | Locator, testInfo: TestInfo, name: string): Promise<void> {
  await target.screenshot({ path: testInfo.outputPath(`${name}.png`) });
}

async function openPage(page: Page): Promise<Locator> {
  await page.getByTestId('sidebar-tool-pulls').click();
  const pane = page.getByTestId('pulls-pane');
  await expect(pane).toBeVisible();
  return pane;
}

export function registerPullsPageFlow({ compact }: PullsSurface): void {
  test('the page groups merge requests by what they need, remembers its filters, and opens one beside the list', async ({ page }, testInfo) => {
    test.setTimeout(120_000);
    const harness = await launchPullsHarness();
    try {
      const seeded = await seedProjects(harness);
      await harness.open(page);
      await useDarkTheme(page);
      const pane = await openPage(page);

      // The page opens on Needs you: every row of that group with one
      // state pill. !103's failed pipeline is not on GitLab's list; it
      // arrives with the project's GraphQL read of the user's own rows.
      const needsYou = pane.getByTestId('pulls-group-needs_you');
      await expect(needsYou).toBeVisible();
      await expect(pane.getByTestId('pulls-filter')).toHaveAttribute('data-filter', 'needs_you');
      await expect(rowState(page, 101)).toHaveText('Review requested');
      await expect(rowState(page, 202)).toHaveText('Review requested');
      await expect(rowState(page, 201)).toHaveText('Ready to merge');
      await expect(rowState(page, 103)).toHaveText('Pipeline failed', { timeout: 20_000 });
      await expect(needsYou).toContainText('4');
      // What the page asked the forges for: each project's fixed request
      // set, and no read of one PR before a review opens.
      await expect(async () => {
        expect(await pullRequestReads(harness)).toEqual({
          lists: [`gh graphql PullsSearch ${seeded.github.project}`, `glab api merge request list ${seeded.gitlab.project}`],
          details: [
            'glab api user events',
            ...['PullsAuthored', 'PullsAuthoredActivity', 'PullsAuthoredApprovals', 'PullsReviewing', 'PullsReviewingActivity', 'PullsReviewingApprovals']
              .map((op) => `glab graphql ${op} ${seeded.gitlab.project}`),
          ],
          perPR: [],
        });
      }).toPass({ timeout: 20_000 });
      await expect(pane.getByTestId('pull-row')).toHaveCount(4);
      await pickFilter(page, pane, 'all');
      await expect(rowState(page, 102)).toHaveText('Draft');
      await expect(rowState(page, 104)).toHaveText('Open');
      await expect(pane.getByTestId('pulls-group-waiting')).toContainText('2');
      await expect(page.getByTestId('sidebar-tool-pulls-badge')).toHaveText('4');
      // The needs-you count on the filter button reads the same.
      await expect(pane.getByTestId('pulls-filter-needs-you-count')).toHaveText('4');
      // Approvals and the CI dot come with each project's read.
      await expect(row(page, 101).getByTestId('pull-approvals')).toHaveText('0/1 approvals');
      await expect(row(page, 201).getByTestId('pull-approvals')).toHaveText('1/1 approvals');
      await shoot(compact ? page : pane, testInfo, 'pulls-list-dark');

      // Filters narrow the list and are remembered across a reload.
      await pickFilter(page, pane, 'mine');
      await expect(pane.getByTestId('pull-row')).toHaveCount(3);
      await expect(row(page, 101)).toHaveCount(0);
      await pickFilter(page, pane, 'all');
      await expect(pane.getByTestId('pull-row')).toHaveCount(6);
      await pane.getByTestId('pulls-projects-picker').click();
      await page.getByTestId('pulls-project-' + (await projectId(harness, 'gadgets'))).click();
      await page.keyboard.press('Escape');
      await expect(pane.getByTestId('pull-row')).toHaveCount(4);
      await expect(row(page, 201)).toHaveCount(0);
      await page.reload();
      // Compact starts on the list screen; the tools row reveals the pane.
      if (compact) await page.getByTestId('sidebar-tool-pulls').click();
      await expect(page.getByTestId('pulls-pane')).toBeVisible();
      await expect(page.getByTestId('pulls-filter')).toHaveAttribute('data-filter', 'all');
      await expect(page.getByTestId('pull-row')).toHaveCount(4);
      await page.getByTestId('pulls-projects-picker').click();
      await page.getByTestId('pulls-projects-all').click();
      await page.keyboard.press('Escape');
      await expect(page.getByTestId('pull-row')).toHaveCount(6);

      // A row opens the review pinned to it: the readiness pill, the Merge
      // section, no scope selector and no close (Back on compact).
      await row(page, 101).getByRole('button').first().click();
      const review = page.getByTestId('review-pane');
      await expect(review).toBeVisible();
      await expect(review.getByTestId('review-pr-header')).toContainText('Queue: retry a failed delivery once');
      await expect(review.getByTestId('review-pr-readiness')).toContainText('Blocked');
      await expect(review.getByTestId('review-pr-merge')).toBeVisible();
      await expect(review.getByTestId('review-scope-select')).toHaveCount(0);
      await expect(row(page, 101)).toHaveAttribute('data-selected', 'true');
      if (compact) {
        await expect(page.getByTestId('pulls-list-column')).toBeHidden();
        await shoot(page, testInfo, 'pulls-review-compact-dark');
        await page.getByTestId('pulls-back').click();
        await expect(page.getByTestId('pulls-list-column')).toBeVisible();
        await expect(review).toBeHidden();
        await row(page, 101).getByRole('button').first().click();
        await expect(review).toBeVisible();
      } else {
        await shoot(page, testInfo, 'pulls-page-dark');
      }

      // The viewer's own comment on !101, which it reviews, is its review:
      // the row waits on others, reviewed by the viewer, with GitLab's touch
      // of the merge request just after the note not read as a reply.
      if (compact) await page.getByTestId('pulls-back').click();
      const [first, ...rest] = seeded.gitlab.pulls!;
      const reviewed = {
        ...first,
        comments: [...(first.comments ?? []), { author: VIEWER, body: 'Why one retry and not three?', createdAt: '2026-10-20T15:00:00Z' }],
        updatedAt: '2026-10-20T15:00:00.400Z',
      };
      await seedForge(harness, [{ ...seeded.gitlab, pulls: [reviewed, ...rest] }]);
      await expect(rowState(page, 101)).toHaveText('Reviewed by you', { timeout: 20_000 });
      await expect(page.getByTestId('sidebar-tool-pulls-badge')).toHaveText('3');
      // The author answers a minute later: the viewer's turn again.
      await seedForge(harness, [{
        ...seeded.gitlab,
        pulls: [{ ...reviewed, comments: [...reviewed.comments, { author: 'sabossedgh', body: 'Three would hide a dead endpoint.', createdAt: '2026-10-20T15:01:00Z' }], updatedAt: '2026-10-20T15:01:00Z' }, ...rest],
      }]);
      await expect(rowState(page, 101)).toHaveText('Author replied', { timeout: 20_000 });
      await expect(page.getByTestId('sidebar-tool-pulls-badge')).toHaveText('4');

      await expectEveryForgeCallHandled(harness);
    } finally {
      await page.close();
      await harness.close();
    }
  });

  test('a project the forge refuses fails alone, and Open with agent starts a thread on the branch', async ({ page }, testInfo) => {
    test.setTimeout(120_000);
    const harness = await launchPullsHarness();
    try {
      const seeded = await seedProjects(harness);
      await harness.open(page);
      await useDarkTheme(page);
      const pane = await openPage(page);
      await expect(rowState(page, 101)).toHaveText('Review requested');
      await pickFilter(page, pane, 'all');
      await expect(pane.getByTestId('pull-row')).toHaveCount(6);

      // GitLab refuses its list with a token lacking the scope: the widgets
      // project reads the sentence under its own heading; gadgets still lists.
      await refuseForgeRoute(harness, {
        route: 'glab api merge request list', status: 403,
        body: { error: 'insufficient_scope', error_description: 'The request requires higher privileges than provided by the access token.', scope: 'api' },
      });
      await pane.getByTestId('pulls-refresh').click();
      const failed = pane.getByTestId('pulls-project-error');
      await expect(failed).toBeVisible({ timeout: 20_000 });
      await expect(failed).toContainText('api');
      await expect(failed).toHaveAttribute('data-error-kind', 'forbidden');
      // Details: the correlation id and the forge's own answer behind the sentence.
      await failed.getByTestId('error-details-toggle').click();
      await expect(failed.getByTestId('error-details')).toContainText(/ref \S+/);
      await expect(failed.getByTestId('error-details')).toContainText('HTTP 403');
      await expect(pane.getByTestId('pulls-project-group')).toContainText('widgets');
      await expect(row(page, 201)).toBeVisible();
      await shoot(compact ? page : pane, testInfo, 'pulls-project-error-dark');
      await clearForgeRefusal(harness, 'glab api merge request list');
      await pane.getByTestId('pulls-refresh').click();
      await expect(failed).toHaveCount(0, { timeout: 20_000 });
      await expect(row(page, 101)).toBeVisible();

      // A row's menu is about its own MR and leaves it unopened: the list,
      // and the trigger the menu hangs from, stay on screen (on compact too).
      const rowMenu = row(page, 201).getByTestId('pull-row-menu');
      await rowMenu.click();
      const rowMenuPopover = page.locator('[data-popover]');
      await expect(rowMenuPopover.getByTestId('open-with-agent-new-thread')).toContainText('New thread on');
      await expect(row(page, 201)).not.toHaveAttribute('data-selected', 'true');
      await expect(page.getByTestId('pulls-list-column')).toBeVisible();
      await expect(rowMenu).toBeVisible();
      const anchorBox = (await rowMenu.boundingBox())!;
      const menuBox = (await rowMenuPopover.boundingBox())!;
      expect(Math.abs(menuBox.y - (anchorBox.y + anchorBox.height))).toBeLessThan(24);
      await page.keyboard.press('Escape');
      await expect(rowMenuPopover).toHaveCount(0);

      // Open with agent on !101: a thread in a linked worktree on its
      // branch, with the seeded draft, takes the page's slot.
      await row(page, 101).getByRole('button').first().click();
      await expect(page.getByTestId('review-pane')).toBeVisible();
      await page.getByTestId('pulls-open-with-agent').click();
      const menu = page.locator('[data-popover]');
      await expect(menu.getByTestId('open-with-agent-no-threads')).toBeVisible();
      await expect(menu.getByTestId('open-with-agent-new-thread')).toContainText('New thread on feature');
      await shoot(page, testInfo, 'pulls-open-with-agent-dark');
      await menu.getByTestId('open-with-agent-new-thread').click();
      const input = page.getByRole('textbox', { name: 'Message Input', exact: true });
      await expect(input).toHaveValue(/^Reviewing !101: Queue: retry a failed delivery once\n/, { timeout: 30_000 });
      await expect(page.getByTestId('pulls-pane')).toHaveCount(0);
      // The thread's linked worktree has the MR's branch checked out at its head.
      const threads = await harness.rpc<Array<{ projectId: string; worktreePath?: string }>>('HarnessListThreadRows');
      const worktrees = threads.filter((thread) => thread.worktreePath).map((thread) => thread.worktreePath!);
      expect(worktrees).toHaveLength(1);
      expect(worktrees[0]).not.toBe(seeded.gitlabPath);
      expect(harnessGit(harness, worktrees[0], 'rev-parse', '--abbrev-ref', 'HEAD')).toBe('feature');
      expect(harnessGit(harness, worktrees[0], 'rev-parse', 'HEAD')).toBe(seeded.gitlab.pulls![0].headSha);
      // The thread's branch is the MR's: git status finds the MR, which
      // the header shows as the badge, or under compact as a menu row.
      if (compact) {
        const more = page.getByTestId('chat-header-more');
        const menu = page.locator('[data-popover]');
        const prRow = menu.getByRole('menuitem', { name: 'MR !101' });
        await expect(async () => {
          if ((await menu.count()) > 0) await page.keyboard.press('Escape');
          await expect(menu).toHaveCount(0);
          await more.tap();
          await expect(prRow).toBeVisible({ timeout: 2_000 });
        }).toPass({ timeout: 30_000 });
        await page.keyboard.press('Escape');
        await expect(menu).toHaveCount(0);
      } else {
        await expect(page.getByTestId('chat-header-pr-badge')).toBeVisible({ timeout: 30_000 });
      }

      // Back on the page, the MR lists the thread and refuses a second worktree.
      if (compact) await page.getByTestId('compact-back').click();
      await page.getByTestId('sidebar-tool-pulls').click();
      await expect(page.getByTestId('pulls-pane')).toBeVisible();
      await row(page, 101).getByRole('button').first().click();
      await page.getByTestId('pulls-open-with-agent').click();
      await expect(menu.getByTestId('open-with-agent-thread')).toHaveCount(1);
      await expect(menu.getByTestId('open-with-agent-new-thread')).toHaveAttribute('aria-disabled', 'true');
      await page.keyboard.press('Escape');

      await expectEveryForgeCallHandled(harness);
    } finally {
      await page.close();
      await harness.close();
    }
  });

  test('the Pipelines tab lists repository pipelines by ref and opens one with its logs', async ({ page }, testInfo) => {
    test.setTimeout(120_000);
    const harness = await launchPullsHarness();
    try {
      await seedProjects(harness);
      await harness.open(page);
      await useDarkTheme(page);
      const pane = await openPage(page);
      await page.getByRole('radio', { name: 'Pipelines' }).click();
      await expect(pane).toHaveAttribute('data-tab', 'pipelines');
      // Two repository pipelines and two merge request pipelines on GitLab,
      // one workflow run and one pull request run on GitHub.
      const rows = pane.getByTestId('pipeline-row');
      await expect(rows).toHaveCount(6);
      const tagRow = rows.filter({ hasText: '#9002' });
      await expect(tagRow).toContainText('v3.9.2');
      await expect(rows.filter({ hasText: '#7001' })).toContainText('CI');
      // Stage dots arrive for rows on screen.
      await expect(tagRow.getByTestId('pipeline-stages').locator('span')).toHaveCount(2);
      await shoot(compact ? page : pane, testInfo, 'pipelines-list-dark');

      await pane.getByTestId('pipelines-ref-picker').click();
      await page.getByTestId('pipelines-ref-tag').click();
      await expect(rows).toHaveCount(1);
      await pane.getByTestId('pipelines-ref-picker').click();
      await page.getByTestId('pipelines-ref-any').click();
      await expect(rows).toHaveCount(6);

      // The project and status filters narrow the list and survive a
      // reload: widgets' failed tag and merge request pipelines.
      const gadgets = await projectId(harness, 'gadgets');
      const widgets = await projectId(harness, 'widgets');
      await pane.getByTestId(`pipelines-project-${gadgets}`).click();
      await pane.getByTestId('pipelines-status-picker').click();
      await page.getByTestId('pipelines-status-failed').click();
      await expect(rows).toHaveCount(2);
      await expect(rows.filter({ hasText: '#9002' })).toHaveCount(1);
      await expect(rows.filter({ hasText: '#45103' })).toHaveCount(1);
      await page.reload();
      if (compact) await page.getByTestId('sidebar-tool-pulls').click();
      await expect(pane).toHaveAttribute('data-tab', 'pipelines');
      await expect(pane.getByTestId('pipelines-status-picker')).toContainText('Failed');
      await expect(pane.getByTestId(`pipelines-project-${gadgets}`)).toHaveAttribute('aria-pressed', 'false');
      await expect(pane.getByTestId(`pipelines-project-${widgets}`)).toHaveAttribute('aria-pressed', 'true');
      await expect(rows).toHaveCount(2);
      await expect(rows.filter({ hasText: '#9002' })).toHaveCount(1);
      await pane.getByTestId(`pipelines-project-${gadgets}`).click();
      await pane.getByTestId('pipelines-status-picker').click();
      await page.getByTestId('pipelines-status-all').click();
      await expect(rows).toHaveCount(6);

      // A row opens its stages; a job opens its log.
      await tagRow.getByRole('button').first().click();
      const view = page.getByTestId('pipeline-view');
      await expect(view).toBeVisible();
      await expect(view.getByTestId('pipeline-view-status')).toHaveText('failed');
      await view.getByTestId('review-ci-chip').filter({ hasText: 'test' }).click();
      await page.getByTestId('review-ci-job').filter({ hasText: 'unit' }).click();
      // A log opens with its sections collapsed; the job section holds the lines.
      const log = view.getByTestId('review-ci-log');
      await log.locator('[data-testid="review-ci-section"][data-key="job"]').getByTestId('review-ci-section-toggle').click();
      await expect(log).toContainText('all green');
      await shoot(compact ? page : view, testInfo, 'pipeline-view-dark');
      if (compact) {
        // One Back: the page header's, which closes the log, then the pipeline.
        await expect(page.getByRole('button', { name: 'Back', exact: true })).toHaveCount(1);
        await page.getByTestId('pulls-back').click();
        await expect(log).toHaveCount(0);
        await expect(view.getByTestId('pipeline-view-empty')).toBeVisible();
        await page.getByTestId('pulls-back').click();
        await expect(view).toBeHidden();
        await expect(rows).toHaveCount(6);
      }

      // A GitHub run opens the same way: #201's run, its unit job's log.
      await rows.filter({ hasText: '#46201' }).getByRole('button').first().click();
      await expect(view).toHaveAttribute('data-pipeline-key', /:46201$/);
      await expect(view.getByTestId('pipeline-view-status')).toHaveText('success');
      await view.getByTestId('review-ci-chip').filter({ hasText: 'CI' }).click();
      await page.getByTestId('review-ci-job').filter({ hasText: 'unit' }).click();
      await log.locator('[data-testid="review-ci-section"][data-key="job"]').getByTestId('review-ci-section-toggle').click();
      await expect(log).toContainText('all green');
      if (compact) {
        await expect(page.getByRole('button', { name: 'Back', exact: true })).toHaveCount(1);
        await page.getByTestId('pulls-back').click();
        await expect(log).toHaveCount(0);
        await page.getByTestId('pulls-back').click();
        await expect(view).toBeHidden();
      }

      await expectEveryForgeCallHandled(harness);
    } finally {
      await page.close();
      await harness.close();
    }
  });
}

/**
 * The page's forge reads so far, as sorted `<route> <target>` strings: the
 * list reads by repository, the other requests of a project read by
 * project (the events feed is the viewer's, once per host), and any read
 * of one PR by its number.
 */
async function pullRequestReads(harness: HarnessApp): Promise<{ lists: string[]; details: string[]; perPR: string[] }> {
  const lists = new Set<string>();
  const details = new Set<string>();
  const perPR = new Set<string>();
  const project = (path: string) => decodeURIComponent(path.replace(/^projects\/([^/?]+).*$/, '$1'));
  for (const call of await forgeInvocations(harness)) {
    if (call.via !== 'http' || !call.route) continue;
    if (call.route === 'gh graphql PullsSearch') {
      lists.add(`${call.route} ${String(call.variables?.query ?? '').replace(/^.*repo:(\S+).*$/, '$1')}`);
    } else if (call.route === 'glab api merge request list') {
      lists.add(`${call.route} ${project(call.path)}`);
    } else if (call.route.startsWith('glab graphql Pulls')) {
      details.add(`${call.route} ${String(call.variables?.fullPath)}`);
    } else if (call.route === 'glab api user events') {
      details.add(call.route);
    } else if (['gh graphql PRTick', 'glab api merge request', 'glab api approvals', 'glab api discussions'].includes(call.route)) {
      perPR.add(`${call.route} ${String(call.variables?.number ?? call.path.replace(/^projects\/[^/]+\/merge_requests\/(\d+).*$/, '$1'))}`);
    }
  }
  return { lists: [...lists].sort(), details: [...details].sort(), perPR: [...perPR].sort() };
}

async function projectId(harness: HarnessApp, name: string): Promise<string> {
  const rows = await harness.rpc<Array<{ project: { id: string; name: string } }>>('ListProjects');
  const row = rows.find((entry) => entry.project.name === name);
  if (!row) throw new Error(`project ${name} is not listed`);
  return row.project.id;
}
