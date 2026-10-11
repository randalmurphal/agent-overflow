// A pull request's CI keeps itself current in the review pane, through the
// shipped UI, the real backend and the fake forge. Covered: the chips
// and an open job log follow a running GitLab job without a click, the
// polling stops once every job is terminal; a running GitHub job shows its
// steps live (read only while its log is open) and says the log arrives on
// completion, a completed job whose log the forge has not published yet
// says so and keeps being asked for past the quick tries without showing
// an error, and the log lands on its own once the forge serves it. The
// harness shortens the CI cadences to 250ms and the log wait to 500ms
// (HARNESS_TIMING), so "stops polling" is a flat invocation count across
// several cadences.

import { randomBytes } from 'node:crypto';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  expectEveryForgeCallHandled,
  forgeInvocations,
  openPullRequestReview,
  publishPullRequest,
  seedForge,
  type ForgeInvocation,
  type ForgeRepo,
} from './forge-helpers.js';

const TITLE = 'CI thread';
const JOB_ID = 7_001;
// Enough lines that the log view scrolls, so following the tail is a
// scroll position and not a no-op. The view renders the log in blocks of
// 200 lines, so the count leaves the last block tall: landing at that
// block's top instead of the log's end is then a visible miss.
const LONG_LOG_LINES = 390;
// Each growth adds more than the pin slack, so a view that did not follow
// is measurably off the bottom.
const GROWTH_LINES = 30;

function longLog(upTo: number, step: number): string {
  let text = '';
  for (let line = 1; line <= upTo; line += 1) text += `line ${line}\n`;
  for (let n = 1; n <= step; n += 1) {
    for (let line = 1; line <= GROWTH_LINES; line += 1) text += `step ${n} output ${line}\n`;
    text += `step ${n} done\n`;
  }
  return text;
}

async function seedProject(harness: HarnessApp): Promise<string> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `ci-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  return seed.projects[0].path;
}

function countRoute(calls: ForgeInvocation[], route: string): number {
  return calls.filter((call) => call.route === route).length;
}

/** The count of `route` calls must not move across several CI cadences. */
async function expectRouteIdle(harness: HarnessApp, route: string): Promise<void> {
  // Let a poll already in flight land first.
  await new Promise((resolve) => setTimeout(resolve, 400));
  const before = countRoute(await forgeInvocations(harness), route);
  await new Promise((resolve) => setTimeout(resolve, 1_200));
  expect(countRoute(await forgeInvocations(harness), route), `${route} kept being called`).toBe(before);
}

/** Re-seed the repository with its CI moved to `patch`. */
async function moveCI(harness: HarnessApp, repo: ForgeRepo, patch: Partial<NonNullable<ForgeRepo['pulls']>[number]['ci']>): Promise<ForgeRepo> {
  const pull = repo.pulls![0]!;
  const next: ForgeRepo = { ...repo, pulls: [{ ...pull, ci: { ...pull.ci!, ...patch } }] };
  await seedForge(harness, [next]);
  return next;
}

/** How far the log view's scroll position is from its bottom, in pixels. */
async function distanceFromBottom(scroll: import('@playwright/test').Locator): Promise<number> {
  return scroll.evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight);
}

async function openJobLog(page: import('@playwright/test').Page, review: import('@playwright/test').Locator, stage: string, job: string) {
  await review.getByTestId('review-ci-chip').filter({ hasText: stage }).click();
  await page.getByTestId('review-ci-job').filter({ hasText: job }).click();
  const log = review.getByTestId('review-ci-log');
  await expect(log).toBeVisible();
  return log;
}

test('a running GitLab job follows its trace into the open log and the chips, then everything goes quiet once it completes', async ({
  harness,
  page,
}) => {
  const workspace = await seedProject(harness);
  let repo = await publishPullRequest(harness, workspace, {
    forge: 'gitlab',
    project: `ao-e2e/ci-${randomBytes(4).toString('hex')}`,
    pulls: [{
      number: 41,
      title: 'Follow the trace',
      ci: { id: 9_100, jobs: [{ id: JOB_ID, name: 'unit', stage: 'test', status: 'running', log: longLog(LONG_LOG_LINES, 1) }] },
    }],
  });
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);

  const chip = review.getByTestId('review-ci-chip').filter({ hasText: 'test' });
  await expect(chip).toBeVisible();
  await expect(chip).toHaveAttribute('title', /1 running/);

  // A log opens at its tail: that is where a failure is.
  const log = await openJobLog(page, review, 'test', 'unit');
  const scroll = log.getByTestId('review-ci-log-scroll');
  await expect(scroll).toContainText('step 1 done');
  await expect(log.getByTestId('review-ci-log-pending')).toHaveCount(0);
  // The view is bounded by the pane, so the tail is a scroll position.
  await expect.poll(() => scroll.evaluate((el) => el.scrollHeight - el.clientHeight)).toBeGreaterThan(200);
  await expect.poll(() => distanceFromBottom(scroll)).toBeLessThanOrEqual(24);

  // The trace grows: the open log follows it with no click, and a reader
  // at the tail stays at the tail.
  repo = await moveCI(harness, repo, { jobs: [{ id: JOB_ID, name: 'unit', stage: 'test', status: 'running', log: longLog(LONG_LOG_LINES, 2) }] });
  await expect(scroll).toContainText('step 2 done');
  await expect.poll(() => distanceFromBottom(scroll)).toBeLessThanOrEqual(24);
  expect(countRoute(await forgeInvocations(harness), 'glab api job trace')).toBeGreaterThan(1);

  // A reader who scrolled up keeps their place when more arrives. The
  // scroll is a wheel gesture: a bare scrollTop write carries no intent,
  // and the follow would re-pin over it.
  await scroll.hover();
  await page.mouse.wheel(0, -100_000);
  await expect.poll(() => scroll.evaluate((el) => el.scrollTop)).toBe(0);
  const tracesBefore = countRoute(await forgeInvocations(harness), 'glab api job trace');
  repo = await moveCI(harness, repo, { jobs: [{ id: JOB_ID, name: 'unit', stage: 'test', status: 'running', log: longLog(LONG_LOG_LINES, 3) }] });
  // The backend has read the grown trace; its frame follows within the
  // cadence. Several cadences later the position must have held.
  await expect.poll(async () => countRoute(await forgeInvocations(harness), 'glab api job trace')).toBeGreaterThan(tracesBefore);
  await new Promise((resolve) => setTimeout(resolve, 800));
  expect(await scroll.evaluate((el) => el.scrollTop)).toBe(0);
  await expect(scroll).toContainText('line 1');

  // The job completes: the final log and the status land on their own.
  await moveCI(harness, repo, { jobs: [{ id: JOB_ID, name: 'unit', stage: 'test', status: 'success', log: longLog(LONG_LOG_LINES, 3) + 'job finished\n' }] });
  await expect(log).toContainText('success');
  // The escaped reader was left alone; wheeling back down re-sticks.
  expect(await scroll.evaluate((el) => el.scrollTop)).toBe(0);
  await page.mouse.wheel(0, 100_000);
  await expect(scroll).toContainText('job finished');
  await expect.poll(() => distanceFromBottom(scroll)).toBeLessThanOrEqual(24);
  await expect(log).toContainText('success');
  await expect(chip).toHaveAttribute('title', /1 success/);

  // Terminal everywhere: neither the trace nor the pipeline is polled again.
  await expectRouteIdle(harness, 'glab api job trace');
  await expectRouteIdle(harness, 'glab api pipeline jobs');

  await expectEveryForgeCallHandled(harness);
});

test('a running GitHub job shows its steps live, says the log arrives on completion, waits out an unpublished log, and the log lands when it does', async ({
  harness,
  page,
}) => {
  const workspace = await seedProject(harness);
  const repo = await publishPullRequest(harness, workspace, {
    forge: 'github',
    project: `ao-e2e/ci-${randomBytes(4).toString('hex')}`,
    pulls: [{
      number: 17,
      title: 'Wait for the log',
      ci: {
        id: 9_200,
        name: 'CI',
        jobs: [{
          id: JOB_ID,
          name: 'build',
          status: 'running',
          log: 'build output\n',
          steps: [{ name: 'Set up job', status: 'success' }, { name: 'Build', status: 'running' }],
        }],
      },
    }],
  });
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);

  // The rollup lists the jobs: no REST call until a log needs steps.
  await expect(review.getByTestId('review-ci-chip').filter({ hasText: 'CI' })).toHaveAttribute('title', /1 running/);
  expect(countRoute(await forgeInvocations(harness), 'gh api run jobs')).toBe(0);

  const log = await openJobLog(page, review, 'CI', 'build');
  const pending = log.getByTestId('review-ci-log-pending');
  await expect(pending).toContainText('The log is available when the job completes.');
  await expect(log.getByTestId('review-ci-steps')).toContainText('Build');
  await expect(log.getByTestId('review-ci-log-scroll')).toHaveCount(0);
  // The Actions log endpoint 404s for a running job, so the app does not ask.
  expect(countRoute(await forgeInvocations(harness), 'gh api job logs')).toBe(0);
  // The steps are polled while the job runs.
  await expect.poll(async () => countRoute(await forgeInvocations(harness), 'gh api run jobs')).toBeGreaterThan(1);
  // The CI cadence reads the rollup alone: its PRTick asks for the checks
  // and neither the detail nor the threads.
  const ciOnlyTicks = async () =>
    (await forgeInvocations(harness)).filter(
      (call) =>
        call.via === 'http' &&
        call.route === 'gh graphql PRTick' &&
        call.variables?.wantChecks === true &&
        call.variables?.wantDetail === false &&
        call.variables?.wantThreads === false,
    ).length;
  await expect.poll(ciOnlyTicks).toBeGreaterThan(0);

  // The job completes, but the forge has not published its log yet: a
  // wait, not an error.
  const failedJob = {
    id: JOB_ID,
    name: 'build',
    status: 'failed' as const,
    log: 'build output\nerror: tests failed\n',
    steps: [{ name: 'Set up job', status: 'success' as const }, { name: 'Build', status: 'failed' as const }],
  };
  const logFetches = async () => countRoute(await forgeInvocations(harness), 'gh api job logs');
  const withheld = await moveCI(harness, repo, { jobs: [{ ...failedJob, logWithheld: true }] });
  await expect(pending).toContainText('The job finished; the forge has not published its log yet.');
  await expect(log.getByTestId('review-ci-log-error')).toHaveCount(0);
  let asked = await logFetches();
  await expect.poll(logFetches).toBeGreaterThan(asked);
  asked = await logFetches();
  await expect.poll(logFetches).toBeGreaterThan(asked);
  // Past the quick tries (six at 250ms) it keeps asking at the wait
  // cadence, still without an error.
  await new Promise((resolve) => setTimeout(resolve, 2_000));
  asked = await logFetches();
  await expect.poll(logFetches).toBeGreaterThan(asked);
  await expect(log.getByTestId('review-ci-log-error')).toHaveCount(0);
  await expect(pending).toContainText('The job finished; the forge has not published its log yet.');
  await expect(log.getByTestId('review-ci-log-scroll')).toHaveCount(0);

  // The forge publishes the log: it lands on its own.
  await moveCI(harness, withheld, { jobs: [failedJob] });
  await expect(log.getByTestId('review-ci-log-scroll')).toContainText('error: tests failed');
  await expect(pending).toHaveCount(0);
  await expect(log).toContainText('failed');

  await expectRouteIdle(harness, 'gh api job logs');
  await expectRouteIdle(harness, 'gh api run jobs');

  await expectEveryForgeCallHandled(harness);
});
