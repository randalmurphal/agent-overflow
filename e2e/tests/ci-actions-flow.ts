// CI actions from the Pull requests page's Pipelines tab, which renders
// the review pane's CI view over a repository pipeline, shared by the
// desktop (review-ci-actions.spec.ts) and compact
// (compact-review-ci-actions.spec.ts) projects, against the fake forge's
// GitLab and GitHub projects of pulls-page-flow.ts. Covered: a failed
// GitLab job retried from its row reads pending, then passed as the
// runner moves it; a job row's actions show without hover on compact
// (on desktop they are revealed by it); a running GitHub run canceled
// from the CI header's menu reads canceled.

import type { Page } from '@playwright/test';

import { expect, test } from './fixtures.js';
import { advanceForgeJob, expectEveryForgeCallHandled, forgeInvocations } from './forge-helpers.js';
import { launchPullsHarness, openPage, seedProjects } from './pulls-page-flow.js';

export interface CIActionsSurface {
  compact: boolean;
}

async function opacity(page: Page, testId: string, job: string): Promise<string> {
  return page.locator(`[data-testid="${testId}"][data-job="${job}"]`).evaluate((el) => getComputedStyle(el).opacity);
}

export function registerCIActionsFlow({ compact }: CIActionsSurface): void {
  test('the Pipelines tab retries a failed job from its row and cancels a running run from its menu', async ({ page }, testInfo) => {
    test.setTimeout(120_000);
    const harness = await launchPullsHarness();
    try {
      const { gitlab } = await seedProjects(harness);
      await harness.open(page);
      await page.emulateMedia({ colorScheme: 'dark' });
      const pane = await openPage(page);
      await page.getByRole('radio', { name: 'Pipelines' }).click();
      const rows = pane.getByTestId('pipeline-row');
      const view = page.getByTestId('pipeline-view');

      // GitLab's failed tag pipeline: its failed unit job retries from its row.
      await rows.filter({ hasText: '#9002' }).getByRole('button').first().click();
      await expect(view.getByTestId('pipeline-view-status')).toHaveText('failed');
      await view.getByTestId('review-ci-chip').filter({ hasText: 'test' }).click();
      const actions = page.locator('[data-testid="review-ci-job-actions"][data-job="unit"]');
      const row = actions.locator('..');
      if (compact) {
        // A touch screen has no hover: the row's actions are always shown.
        expect(await opacity(page, 'review-ci-job-actions', 'unit')).toBe('1');
      } else {
        await page.mouse.move(0, 0);
        expect(await opacity(page, 'review-ci-job-actions', 'unit')).toBe('0');
        await row.hover();
        await expect.poll(() => opacity(page, 'review-ci-job-actions', 'unit')).toBe('1');
      }
      await page.screenshot({ path: testInfo.outputPath('pipelines-ci-row-actions.png') });
      await actions.getByTestId('review-ci-job-retry_job').click();
      await expect(row.getByTestId('review-ci-job')).toContainText('pending');
      await expect(view.getByTestId('pipeline-view-status')).not.toHaveText('failed');
      await advanceForgeJob(harness, { forge: 'gitlab', project: gitlab.project, job: 'unit', status: 'success', log: 'unit passed\n' });
      await expect(row.getByTestId('review-ci-job')).toContainText('success');
      await expect(view.getByTestId('pipeline-view-status')).toHaveText('success');
      if (compact) {
        await page.keyboard.press('Escape');
        await page.getByTestId('pulls-back').click();
        await expect(view).toBeHidden();
      }

      // GitHub's running run cancels from the CI header's menu.
      await rows.filter({ hasText: '#7001' }).getByRole('button').first().click();
      await expect(view).toHaveAttribute('data-pipeline-key', /:7001$/);
      await expect(view.getByTestId('pipeline-view-status')).toHaveText('running');
      await view.getByTestId('review-ci-pipeline-actions').click();
      await expect(page.getByTestId('review-ci-pipeline-retry_failed')).toHaveCount(0);
      await page.getByTestId('review-ci-pipeline-cancel_pipeline').click();
      await expect(view.getByTestId('pipeline-view-status')).toHaveText('canceled');
      await expect(view.getByTestId('review-ci-pipeline-actions')).not.toHaveAttribute('data-pending', 'true');

      const routes = (await forgeInvocations(harness)).map((call) => call.route);
      expect(routes).toContain('glab api job retry');
      expect(routes).toContain('gh api run cancel');
      await expectEveryForgeCallHandled(harness);
    } finally {
      await harness.close();
    }
  });
}
