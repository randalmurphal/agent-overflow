// A pull request's CI keeps itself current in the review pane, through the
// shipped UI, the real backend and the fake forge. Covered: the open log
// shows one collapsed row per GitLab section or GitHub step, nothing
// expanded on its own; an expanded row holds its place, and a reader who
// moves to the end follows its growth while one who scrolled up keeps
// their place; the chips and an open job log follow a running GitLab job
// without a click, and the polling stops once every job is terminal; the
// saved log and a section sent to chat carry no section markers; a
// running GitHub job lists its steps live (read only while its log is
// open), waits without an error while the forge answers 404 for its log,
// shows and follows the log while the jobs API still calls the job
// running once the forge serves it, waits again while a completed job's
// final log is withheld past the quick tries, and the final log lands on
// its own once the forge serves it. The harness shortens the CI cadences
// to 250ms and the log wait to 500ms (HARNESS_TIMING), so "stops polling"
// is a flat invocation count across several cadences.

import { randomBytes } from 'node:crypto';
import { readFile } from 'node:fs/promises';

import type { Locator, Page } from '@playwright/test';

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
  type ForgeStep,
} from './forge-helpers.js';

const TITLE = 'CI thread';
const JOB_ID = 7_001;
// Enough lines that an expanded section scrolls, so following its end is
// a scroll position and not a no-op. The view renders a section's lines
// in blocks of 200, so the count leaves the last block tall: landing at
// that block's top instead of the log's end is then a visible miss.
const LONG_LOG_LINES = 390;
// Each growth adds more than the pin slack, so a view that did not follow
// is measurably off the bottom.
const GROWTH_LINES = 30;

function longLog(upTo: number, step: number, prefix = ''): string {
  let text = '';
  for (let line = 1; line <= upTo; line += 1) text += `${prefix}line ${line}\n`;
  for (let n = 1; n <= step; n += 1) {
    for (let line = 1; line <= GROWTH_LINES; line += 1) text += `${prefix}step ${n} output ${line}\n`;
    text += `${prefix}step ${n} done\n`;
  }
  return text;
}

// A raw GitLab trace as the runner writes it: each section marker erased
// by a carriage return, its header after it on the same line.
const T0 = 1_760_000_000;
function gitlabTrace(steps: number, finished = false): string {
  let text = 'Running with gitlab-runner 17.0.0\n';
  text += `section_start:${T0}:prepare_executor\r\x1b[0K\x1b[36;1mPreparing the "docker" executor\x1b[0;m\n`;
  text += 'Using docker image alpine:3.20\n';
  text += `\x1b[0Ksection_end:${T0 + 4}:prepare_executor\r\x1b[0K\n`;
  text += `section_start:${T0 + 4}:step_script\r\x1b[0K\x1b[36;1mExecuting "step_script" stage of the job script\x1b[0;m\n`;
  text += longLog(LONG_LOG_LINES, steps);
  if (finished) {
    text += 'job finished\n';
    text += `\x1b[0Ksection_end:${T0 + 70}:step_script\r\x1b[0K\n`;
    text += '\x1b[32;1mJob succeeded\x1b[0;m\n';
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
async function distanceFromBottom(scroll: Locator): Promise<number> {
  return scroll.evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight);
}

async function openJobLog(page: Page, review: Locator, stage: string, job: string) {
  await review.getByTestId('review-ci-chip').filter({ hasText: stage }).click();
  await page.getByTestId('review-ci-job').filter({ hasText: job }).click();
  const log = review.getByTestId('review-ci-log');
  await expect(log).toBeVisible();
  return log;
}

function section(log: Locator, key: string): Locator {
  return log.locator(`[data-testid="review-ci-section"][data-key="${key}"]`);
}

test('a running GitLab job shows its sections, follows its trace into an open section and the chips, then everything goes quiet once it completes', async ({
  harness,
  page,
}, testInfo) => {
  const workspace = await seedProject(harness);
  let repo = await publishPullRequest(harness, workspace, {
    forge: 'gitlab',
    project: `ao-e2e/ci-${randomBytes(4).toString('hex')}`,
    pulls: [{
      number: 41,
      title: 'Follow the trace',
      ci: { id: 9_100, jobs: [{ id: JOB_ID, name: 'unit', stage: 'test', status: 'running', log: gitlabTrace(1) }] },
    }],
  });
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);

  const chip = review.getByTestId('review-ci-chip').filter({ hasText: 'test' });
  await expect(chip).toBeVisible();
  await expect(chip).toHaveAttribute('title', /1 running/);

  // One row per section, plus the runner's line before them; none open.
  const log = await openJobLog(page, review, 'test', 'unit');
  const scroll = log.getByTestId('review-ci-log-scroll');
  const prepare = section(log, `section:prepare_executor:${T0}`);
  const script = section(log, `section:step_script:${T0 + 4}`);
  await expect(log.getByTestId('review-ci-section')).toHaveCount(3);
  await expect(prepare).toHaveAttribute('data-status', 'done');
  await expect(prepare).toContainText('Preparing the "docker" executor');
  await expect(prepare).toContainText('4s');
  await expect(script).toHaveAttribute('data-status', 'running');
  await expect(script).toContainText('Executing "step_script" stage of the job script');
  // The header line, the 390 lines and step 1's 31.
  await expect(script.getByTestId('review-ci-section-lines')).toHaveText('422 lines');
  for (const row of await log.getByTestId('review-ci-section').all()) await expect(row).toHaveAttribute('data-open', 'false');
  await expect(log.getByTestId('review-ci-log-pending')).toHaveCount(0);
  await expect(scroll).not.toContainText('step 1 done');
  await expect(scroll).not.toContainText('section_');

  // Expanding the running section shows it from its header: the row
  // holds its place rather than following to the end.
  await script.getByTestId('review-ci-section-toggle').click();
  await expect(script).toHaveAttribute('data-open', 'true');
  await expect(scroll).toContainText('line 1\n');
  await expect.poll(() => scroll.evaluate((el) => el.scrollHeight - el.clientHeight)).toBeGreaterThan(200);
  expect(await scroll.evaluate((el) => el.scrollTop)).toBe(0);
  await page.screenshot({ path: testInfo.outputPath('ci-log-sections-gitlab-running.png') });

  // A reader who moves to the end follows the trace as it grows, with no
  // click.
  await scroll.hover();
  await page.mouse.wheel(0, 100_000);
  await expect(scroll).toContainText('step 1 done');
  await expect.poll(() => distanceFromBottom(scroll)).toBeLessThanOrEqual(24);
  repo = await moveCI(harness, repo, { jobs: [{ id: JOB_ID, name: 'unit', stage: 'test', status: 'running', log: gitlabTrace(2) }] });
  await expect(scroll).toContainText('step 2 done');
  await expect.poll(() => distanceFromBottom(scroll)).toBeLessThanOrEqual(24);
  expect(countRoute(await forgeInvocations(harness), 'glab api job trace')).toBeGreaterThan(1);

  // A reader who scrolled up keeps their place when more arrives. The
  // scroll is a wheel gesture: a bare scrollTop write carries no intent,
  // and the follow would re-pin over it.
  await page.mouse.wheel(0, -100_000);
  await expect.poll(() => scroll.evaluate((el) => el.scrollTop)).toBe(0);
  const tracesBefore = countRoute(await forgeInvocations(harness), 'glab api job trace');
  repo = await moveCI(harness, repo, { jobs: [{ id: JOB_ID, name: 'unit', stage: 'test', status: 'running', log: gitlabTrace(3) }] });
  // The backend has read the grown trace; its frame follows within the
  // cadence. Several cadences later the position must have held.
  await expect.poll(async () => countRoute(await forgeInvocations(harness), 'glab api job trace')).toBeGreaterThan(tracesBefore);
  await new Promise((resolve) => setTimeout(resolve, 800));
  expect(await scroll.evaluate((el) => el.scrollTop)).toBe(0);
  await expect(scroll).toContainText('Preparing the "docker" executor');
  // The row counted the lines as they came.
  await expect(script.getByTestId('review-ci-section-lines')).toHaveText('484 lines');

  // The job completes: the final trace and the status land on their own.
  await moveCI(harness, repo, { jobs: [{ id: JOB_ID, name: 'unit', stage: 'test', status: 'success', log: gitlabTrace(3, true) }] });
  await expect(log).toContainText('success');
  await expect(script).toHaveAttribute('data-status', 'done');
  await expect(script).toContainText('1m 6s');
  // The escaped reader was left alone; wheeling back down re-sticks.
  expect(await scroll.evaluate((el) => el.scrollTop)).toBe(0);
  await page.mouse.wheel(0, 100_000);
  await expect(scroll).toContainText('job finished');
  await expect.poll(() => distanceFromBottom(scroll)).toBeLessThanOrEqual(24);
  await expect(section(log, `output:section:step_script:${T0 + 4}`)).toContainText('Job succeeded');
  await expect(chip).toHaveAttribute('title', /1 success/);

  // Terminal everywhere: neither the trace nor the pipeline is polled again.
  await expectRouteIdle(harness, 'glab api job trace');
  await expectRouteIdle(harness, 'glab api pipeline jobs');

  // The saved log is the trace as text, without its section markers.
  await log.getByTestId('review-ci-log-save').click();
  const saved = log.getByTestId('review-ci-log-saved');
  await expect(saved).toContainText('Saved to ');
  const savedText = await readFile((await saved.innerText()).replace(/^Saved to /, '').trim(), 'utf8');
  expect(savedText).toContain('Using docker image alpine:3.20\n');
  expect(savedText).toContain('step 3 done\njob finished\n');
  expect(savedText).not.toContain('section_');

  // One section goes to the chat as text, without markers either.
  await scroll.hover();
  await page.mouse.wheel(0, -100_000);
  await prepare.hover();
  await prepare.getByTestId('review-ci-section-send').click();
  const composer = page.getByLabel('Message Input');
  await expect(composer).toHaveValue(/Investigate `Preparing the "docker" executor` in CI job `unit` \(test\) on PR #41, status: success\./);
  await expect(composer).toHaveValue(/Using docker image alpine:3\.20/);
  expect(await composer.inputValue()).not.toContain('section_');

  await expectEveryForgeCallHandled(harness);
});

// A GitHub job's log as the forge serves it: every line carries its time.
function githubLog(steps: number, failed = false): string {
  let text = '2026-01-01T00:00:00.1000000Z Current runner version: \'2.337.0\'\n';
  text += '2026-01-01T00:00:00.9000000Z Complete job name: build\n';
  text += '2026-01-01T00:00:01.1000000Z ##[group]Run actions/checkout@v4\n';
  text += '2026-01-01T00:00:02.0000000Z Syncing repository: ao-e2e/ci\n';
  text += '2026-01-01T00:00:03.1000000Z ##[group]Run make test\n';
  text += longLog(LONG_LOG_LINES, steps, '2026-01-01T00:00:04.0000000Z ');
  if (failed) {
    text += '2026-01-01T00:01:02.0000000Z error: tests failed\n';
    text += '2026-01-01T00:01:02.1000000Z ##[error]Process completed with exit code 2.\n';
    text += '2026-01-01T00:01:02.2000000Z Post job cleanup.\n';
    text += '2026-01-01T00:01:02.3000000Z Cleaning up orphan processes\n';
  }
  return text;
}

const RUNNING_STEPS: ForgeStep[] = [
  { number: 1, name: 'Set up job', status: 'success', startedAt: '2026-01-01T00:00:00Z', completedAt: '2026-01-01T00:00:01Z' },
  { number: 2, name: 'Run actions/checkout@v4', status: 'success', startedAt: '2026-01-01T00:00:01Z', completedAt: '2026-01-01T00:00:03Z' },
  { number: 3, name: 'Run make test', status: 'running', startedAt: '2026-01-01T00:00:03Z' },
  { number: 4, name: 'Run make lint', status: 'pending' },
  { number: 7, name: 'Post Run actions/checkout@v4', status: 'pending' },
  { number: 8, name: 'Complete job', status: 'pending' },
];

// The failed step and the steps after it start in the same second.
const FAILED_STEPS: ForgeStep[] = [
  RUNNING_STEPS[0]!,
  RUNNING_STEPS[1]!,
  { ...RUNNING_STEPS[2]!, status: 'failed', completedAt: '2026-01-01T00:01:02Z' },
  { number: 4, name: 'Run make lint', status: 'skipped', startedAt: '2026-01-01T00:01:02Z', completedAt: '2026-01-01T00:01:02Z' },
  { number: 7, name: 'Post Run actions/checkout@v4', status: 'success', startedAt: '2026-01-01T00:01:02Z', completedAt: '2026-01-01T00:01:02Z' },
  { number: 8, name: 'Complete job', status: 'success', startedAt: '2026-01-01T00:01:02Z', completedAt: '2026-01-01T00:01:02Z' },
];

test('a running GitHub job lists its steps, waits out a log the forge has not published, follows its log live once served, and lands the final log', async ({
  harness,
  page,
}, testInfo) => {
  const workspace = await seedProject(harness);
  const runningJob = { id: JOB_ID, name: 'build', status: 'running' as const, steps: RUNNING_STEPS };
  let repo = await publishPullRequest(harness, workspace, {
    forge: 'github',
    project: `ao-e2e/ci-${randomBytes(4).toString('hex')}`,
    pulls: [{
      number: 17,
      title: 'Follow the log',
      // GitHub answers 404 for a running job until its log blob exists.
      ci: { id: 9_200, name: 'CI', jobs: [{ ...runningJob, log: githubLog(1), logWithheld: true }] },
    }],
  });
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);

  // The rollup lists the jobs: no REST call until a log needs steps.
  await expect(review.getByTestId('review-ci-chip').filter({ hasText: 'CI' })).toHaveAttribute('title', /1 running/);
  expect(countRoute(await forgeInvocations(harness), 'gh api run jobs')).toBe(0);

  // Every step is listed while the log is withheld, the ones not reached
  // as pending, and none has lines to open yet.
  const log = await openJobLog(page, review, 'CI', 'build');
  const pending = log.getByTestId('review-ci-log-pending');
  const scroll = log.getByTestId('review-ci-log-scroll');
  await expect(pending).toHaveText('The log is available when the job completes.');
  await expect(log.getByTestId('review-ci-section')).toHaveCount(6);
  await expect(section(log, 'step:3')).toHaveAttribute('data-status', 'running');
  await expect(section(log, 'step:8')).toHaveAttribute('data-status', 'pending');
  for (const toggle of await log.getByTestId('review-ci-section-toggle').all()) await expect(toggle).toBeDisabled();
  // The 404 is a wait: asked again while the job runs, past the quick
  // tries a completed job gets, never an error.
  const logFetches = async () => countRoute(await forgeInvocations(harness), 'gh api job logs');
  await expect.poll(logFetches).toBeGreaterThan(8);
  await expect(log.getByTestId('review-ci-log-error')).toHaveCount(0);
  await expect(pending).toBeVisible();
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

  // The blob exists while the jobs API still calls the job running: the
  // log splits into its steps, and the running one counts its lines.
  const step3 = section(log, 'step:3');
  repo = await moveCI(harness, repo, { jobs: [{ ...runningJob, log: githubLog(1) }] });
  await expect(pending).toHaveCount(0);
  await expect(step3.getByTestId('review-ci-section-lines')).toHaveText('422 lines');
  await expect(log).toContainText('running');
  await expect(scroll).not.toContainText('step 1 done');
  await step3.getByTestId('review-ci-section-toggle').click();
  await expect(scroll).toContainText('##[group]Run make test');
  await expect(scroll).not.toContainText('Syncing repository');

  // At the end it follows growth with no click, before the job completes.
  await scroll.hover();
  await page.mouse.wheel(0, 100_000);
  await expect(scroll).toContainText('step 1 done');
  await expect.poll(() => distanceFromBottom(scroll)).toBeLessThanOrEqual(24);
  repo = await moveCI(harness, repo, { jobs: [{ ...runningJob, log: githubLog(2) }] });
  await expect(scroll).toContainText('step 2 done');
  await expect.poll(() => distanceFromBottom(scroll)).toBeLessThanOrEqual(24);
  // An unchanged log is revalidated by its ETag, not read again.
  await expect.poll(async () =>
    (await forgeInvocations(harness)).filter((call) => call.via === 'http' && call.route === 'gh api job logs' && call.status === 304).length,
  ).toBeGreaterThan(0);

  // The job completes, and the forge holds the final log back for a
  // while: a wait over the text already shown, not an error.
  const failedJob = { id: JOB_ID, name: 'build', status: 'failed' as const, log: githubLog(2, true), steps: FAILED_STEPS };
  const withheld = await moveCI(harness, repo, { jobs: [{ ...failedJob, logWithheld: true }] });
  await expect(pending).toHaveText('The job finished; the forge has not published its log yet.');
  await expect(log.getByTestId('review-ci-log-error')).toHaveCount(0);
  await expect(scroll).toContainText('step 2 done');
  let asked = await logFetches();
  await expect.poll(logFetches).toBeGreaterThan(asked);
  // Past the quick tries (six at 250ms) it keeps asking at the wait
  // cadence, still without an error.
  await new Promise((resolve) => setTimeout(resolve, 2_000));
  asked = await logFetches();
  await expect.poll(logFetches).toBeGreaterThan(asked);
  await expect(log.getByTestId('review-ci-log-error')).toHaveCount(0);

  // The forge publishes the final log: it lands on its own, the failed
  // step keeps its lines from the steps after it, and the skipped step is
  // not listed.
  await moveCI(harness, withheld, { jobs: [failedJob] });
  await expect(scroll).toContainText('##[error]Process completed with exit code 2.');
  await expect(pending).toHaveCount(0);
  await expect(log).toContainText('failed');
  await expect(section(log, 'step:7')).toHaveAttribute('data-status', 'success');
  await expect(section(log, 'step:8')).toHaveAttribute('data-status', 'success');
  await expect(scroll).not.toContainText('Post job cleanup.');
  await page.screenshot({ path: testInfo.outputPath('ci-log-sections-github-failed-end.png') });
  await page.mouse.wheel(0, -100_000);
  await expect.poll(() => scroll.evaluate((el) => el.scrollTop)).toBe(0);
  await expect(step3).toHaveAttribute('data-status', 'failed');
  await expect(step3).toContainText('59s');
  await expect(section(log, 'step:4')).toHaveCount(0);
  await page.screenshot({ path: testInfo.outputPath('ci-log-sections-github-failed-top.png') });

  await expectRouteIdle(harness, 'gh api job logs');
  await expectRouteIdle(harness, 'gh api run jobs');

  await expectEveryForgeCallHandled(harness);
});
