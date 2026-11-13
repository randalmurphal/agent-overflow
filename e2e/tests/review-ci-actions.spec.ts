// Acting on a pull request's CI from the review pane (docs/specs/pull-requests.md,
// "CI actions"), through the shipped UI, the real backend and the fake
// forge, on GitLab and GitHub. Covered: a refused retry reads as its one
// sentence and leaves the control usable; a retry from the log view's
// header follows the new attempt, says so, shows it pending and then
// passed as the runner moves it (HarnessForgeAdvanceJob); a running
// pipeline canceled from the CI header's menu reads canceled (GitLab); a
// manual job runs from its row (GitLab); a retry from a job's row
// (GitHub); artifacts download to a file whose contents are the forge's,
// a job's archive shown on its log's saved line (GitLab) and a run's
// artifacts from the CI header's menu as a toast (GitHub). The same
// actions from the Pipelines tab are ci-actions-flow.ts.

import { randomBytes } from 'node:crypto';
import { readFile } from 'node:fs/promises';

import type { Locator, Page } from '@playwright/test';

import { expect, test } from './fixtures.js';
import {
  advanceForgeJob,
  expectEveryForgeCallHandled,
  forgeInvocations,
  openCIJobLog,
  openPullRequestReview,
  publishPullRequest,
  refuseForgeRoute,
  seedThreadProject,
} from './forge-helpers.js';
import { registerCIActionsFlow } from './ci-actions-flow.js';

const TITLE = 'CI actions thread';

function savedPath(text: string): string {
  return text.replace(/^Saved to /, '').trim();
}

/** A job's row in the open stage list, and its hover-revealed actions. */
function jobRow(page: Page, name: string): { row: Locator; actions: Locator } {
  const actions = page.locator(`[data-testid="review-ci-job-actions"][data-job="${name}"]`);
  return { row: actions.locator('..'), actions };
}

test('GitLab: a refused retry reads as its sentence, a retried job is followed to passed, a pipeline cancels, a manual job runs and artifacts download', async ({ harness, page }, testInfo) => {
  test.setTimeout(120_000);
  await page.emulateMedia({ colorScheme: 'dark' });
  const workspace = await seedThreadProject(harness, TITLE, 'ci-actions');
  const project = `ao-e2e/ci-actions-${randomBytes(4).toString('hex')}`;
  await publishPullRequest(harness, workspace, {
    forge: 'gitlab',
    project,
    pulls: [{
      number: 51,
      title: 'Act on CI',
      ci: {
        id: 9_200,
        jobs: [
          { id: 8_001, name: 'unit', stage: 'test', status: 'failed', log: 'unit failed\n' },
          { id: 8_002, name: 'deploy', stage: 'deploy', status: 'manual' },
        ],
      },
    }],
  });
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);
  const log = await openCIJobLog(page, review, 'test', 'unit');
  await expect(log).toHaveAttribute('data-job-id', '8001');
  const retry = log.getByTestId('review-ci-log-retry_job');
  await expect(retry).toBeVisible();
  // A running job has no Retry; Cancel job is GitLab's own per-job action.
  await expect(log.getByTestId('review-ci-log-cancel_job')).toHaveCount(0);

  // The forge refuses once: its sentence, and the control is usable again.
  await refuseForgeRoute(harness, { route: 'glab api job retry', status: 403, body: { message: '403 Forbidden - Job is not retryable' }, times: 1 });
  await retry.click();
  await expect(page.getByTestId('toast-message').filter({ hasText: /^Job unit of .+ can no longer be retried\. Refresh to see its current state\.$/ })).toBeVisible();
  await expect(retry).toBeEnabled();
  await expect(log).toHaveAttribute('data-job-id', '8001');

  // The retry is the forge's new pending job; the log follows it and says so.
  await retry.click();
  await expect(log).not.toHaveAttribute('data-job-id', '8001');
  await expect(log.getByTestId('review-ci-log-retried')).toBeVisible();
  await expect(log.getByTestId('review-ci-log-status')).toHaveText('pending');
  await expect(retry).toHaveCount(0);
  await expect(log.getByTestId('review-ci-log-cancel_job')).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath('ci-actions-gitlab-retried.png') });

  // Running: Cancel pipeline from the CI header's menu ends it.
  await advanceForgeJob(harness, { forge: 'gitlab', project, job: 'unit', status: 'running', log: 'unit again\n' });
  await expect(log.getByTestId('review-ci-log-status')).toHaveText('running');
  await review.getByTestId('review-ci-pipeline-actions').click();
  await expect(page.getByTestId('review-ci-pipeline-retry_failed')).toHaveCount(0);
  await page.getByTestId('review-ci-pipeline-cancel_pipeline').click();
  await expect(log.getByTestId('review-ci-log-status')).toHaveText('canceled');

  // Retried again, it is pending, then passes with an archive.
  await retry.click();
  await expect(log.getByTestId('review-ci-log-status')).toHaveText('pending');
  await advanceForgeJob(harness, { forge: 'gitlab', project, job: 'unit', status: 'success', log: 'unit passed\n', artifacts: { text: 'coverage 91%\n' } });
  await expect(log.getByTestId('review-ci-log-status')).toHaveText('success');
  await expect(log.getByTestId('review-ci-log-retried')).toBeVisible();

  // The archive downloads to a file; the log's saved line says where.
  await log.getByTestId('review-ci-log-artifacts').click();
  const saved = log.getByTestId('review-ci-log-saved');
  await expect(saved).toContainText('Saved to ');
  expect(await readFile(savedPath(await saved.innerText()), 'utf8')).toBe('coverage 91%\n');

  // A manual job runs from its row.
  await review.getByTestId('review-ci-chip').filter({ hasText: 'deploy' }).click();
  const deploy = jobRow(page, 'deploy');
  await deploy.row.hover();
  await page.screenshot({ path: testInfo.outputPath('ci-actions-gitlab-row-hover.png') });
  await deploy.actions.getByTestId('review-ci-job-play_job').click();
  await expect(deploy.row.getByTestId('review-ci-job')).toContainText('pending');
  await expect(deploy.actions.getByTestId('review-ci-job-play_job')).toHaveCount(0);

  const routes = (await forgeInvocations(harness)).map((call) => call.route);
  for (const route of ['glab api job retry', 'glab api pipeline cancel', 'glab api job play', 'glab api job artifacts']) {
    expect(routes, route).toContain(route);
  }
  await expectEveryForgeCallHandled(harness);
});

test('GitHub: a refused retry reads as its sentence, a run\'s artifacts download from the CI header, and a retry from a job\'s row passes', async ({ harness, page }) => {
  test.setTimeout(120_000);
  const workspace = await seedThreadProject(harness, TITLE, 'ci-actions');
  const project = `ao-e2e/ci-actions-${randomBytes(4).toString('hex')}`;
  await publishPullRequest(harness, workspace, {
    forge: 'github',
    project,
    pulls: [{
      number: 61,
      title: 'Act on CI',
      ci: {
        id: 46_300,
        name: 'CI',
        jobs: [
          { id: 9_101, name: 'unit', status: 'failed', log: 'unit failed\n' },
          { id: 9_102, name: 'lint', status: 'success', log: 'lint ok\n' },
        ],
        artifacts: [{ name: 'coverage', text: 'github coverage\n' }],
      },
    }],
  });
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);

  // The run's artifacts, from the CI header's menu, as a toast.
  await review.getByTestId('review-ci-pipeline-actions').click();
  await expect(page.getByTestId('review-ci-pipeline-retry_failed')).toBeVisible();
  // GitHub cancels runs, not jobs, and a completed run has nothing to cancel.
  await expect(page.getByTestId('review-ci-pipeline-cancel_pipeline')).toHaveCount(0);
  await page.getByTestId('review-ci-pipeline-artifacts').click();
  const toast = page.getByTestId('toast-message').filter({ hasText: /^Saved to / });
  await expect(toast).toBeVisible();
  expect(await readFile(savedPath(await toast.innerText()), 'utf8')).toBe('github coverage\n');

  // A failed job's row offers Retry; a passed one offers nothing.
  await review.getByTestId('review-ci-chip').filter({ hasText: 'CI' }).click();
  const unit = jobRow(page, 'unit');
  const lint = jobRow(page, 'lint');
  await expect(lint.actions.getByRole('button')).toHaveCount(0);
  await expect(unit.actions.getByTestId('review-ci-job-cancel_job')).toHaveCount(0);
  await refuseForgeRoute(harness, { route: 'gh api job rerun', status: 403, body: { message: 'This job cannot be rerun.' }, times: 1 });
  await unit.row.hover();
  await unit.actions.getByTestId('review-ci-job-retry_job').click();
  await expect(page.getByTestId('toast-message').filter({ hasText: /can no longer be retried\. Refresh to see its current state\.$/ })).toBeVisible();

  await unit.row.hover();
  await unit.actions.getByTestId('review-ci-job-retry_job').click();
  await expect(unit.row.getByTestId('review-ci-job')).toContainText('pending');
  await expect(unit.actions.getByTestId('review-ci-job-retry_job')).toHaveCount(0);
  await advanceForgeJob(harness, { forge: 'github', project, job: 'unit', status: 'success', log: 'unit passed\n' });
  await expect(unit.row.getByTestId('review-ci-job')).toContainText('success');

  const routes = (await forgeInvocations(harness)).map((call) => call.route);
  for (const route of ['gh api job rerun', 'gh api run artifacts', 'gh api artifact zip']) {
    expect(routes, route).toContain(route);
  }
  await expectEveryForgeCallHandled(harness);
});

registerCIActionsFlow({ compact: false });
