// A pull request review pane while the forge rate-limits the app, through
// the shipped UI, the real backend and the fake forge. Covered on GitHub:
// an exhausted pool shows the warning banner with the local time updates
// resume, the app sends that pool nothing until then, and the pane
// recovers on its own with the banner gone; a pool under the app's
// reserve shows the pause banner while the user's own action (refreshing
// a job log) still reaches the forge. The backend polls the PR every
// second here (pr-update=1s) instead of every 45s, which changes when a
// poll runs and not what it does.

import { randomBytes } from 'node:crypto';

import type { Page } from '@playwright/test';
import { HARNESS_TIMING, launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  expectEveryForgeCallHandled,
  forgeInvocations,
  openPullRequestReview,
  publishPullRequest,
  setForgeRateLimit,
  type ForgeInvocation,
} from './forge-helpers.js';

const TITLE = 'Rate limit thread';
const JOB_ID = 7_301;

async function withHarness(page: Page, run: (harness: HarnessApp) => Promise<void>): Promise<void> {
  const harness = await launchHarness({ env: { AO_HARNESS_TIMING: `${HARNESS_TIMING},pr-update=1s` } });
  try {
    await run(harness);
  } finally {
    await page.close();
    await harness.close();
  }
}

async function seedPullRequest(harness: HarnessApp): Promise<void> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `rate-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  await publishPullRequest(harness, seed.projects[0].path, {
    forge: 'github',
    project: `ao-e2e/rate-${randomBytes(4).toString('hex')}`,
    pulls: [{
      number: 23,
      title: 'Stay under the limit',
      ci: { id: 9_300, name: 'CI', jobs: [{ id: JOB_ID, name: 'build', status: 'success', log: 'build output\n' }] },
    }],
  });
}

/** A reset `seconds` from now, as the forge sends it: unix seconds. */
function resetIn(seconds: number): number {
  return Math.ceil(Date.now() / 1000) + seconds;
}

/** The pane's clock time for a reset, in the page's own locale and zone. */
async function clockTime(page: Page, reset: number): Promise<string> {
  return page.evaluate(
    (ms) => new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' }).format(ms),
    reset * 1000,
  );
}

function graphQLCalls(calls: ForgeInvocation[]): ForgeInvocation[] {
  return calls.filter((call) => call.via === 'http' && call.path === 'graphql');
}

test('an exhausted GitHub rate limit pauses the review pane until the reset, then it recovers on its own', async ({ page }) => {
  test.setTimeout(120_000);
  await withHarness(page, async (harness) => {
    await seedPullRequest(harness);
    await harness.open(page);
    // Opened once so the PR is known; closing drops the backend's poller,
    // and the reopen starts a fresh one against the limited forge.
    const review = await openPullRequestReview(page, TITLE);
    await review.getByTestId('review-close').click();
    await expect(page.locator('section[data-pane-kind="review"]')).toHaveCount(0);

    const reset = resetIn(10);
    await setForgeRateLimit(harness, { forge: 'github', pool: 'graphql', remaining: 0, reset });
    await page.getByTestId('chat-header-pr-badge').click();
    const reopened = page.locator('section[data-pane-kind="review"]');
    const banner = reopened.getByTestId('review-pr-rate-limited');
    await expect(banner).toHaveText(`GitHub rate limit reached. Updates resume at ${await clockTime(page, reset)}.`);
    await expect(reopened.getByTestId('review-pr-update-error')).toHaveCount(0);
    const refused = graphQLCalls(await forgeInvocations(harness)).filter((call) => call.route === 'rate limited');
    expect(refused.length).toBeGreaterThan(0);
    const mark = Math.max(...refused.map((call) => call.seq));

    // Recovery: the pane loads and the banner goes, after the reset plus
    // the app's stagger of up to 2s and a poll.
    await expect(reopened.getByTestId('review-pr-header')).toBeVisible({ timeout: 20_000 });
    await expect(banner).toHaveCount(0);
    await expect(reopened.getByTestId('review-pr-update-error')).toHaveCount(0);

    // Between the refusal the banner reports and the reset, the app sent
    // the limited pool nothing.
    const early = graphQLCalls(await forgeInvocations(harness)).filter(
      (call) => call.seq > mark && Date.parse(call.at) < reset * 1000,
    );
    expect(early.map((call) => `${call.route} at ${call.at}`)).toEqual([]);

    await expectEveryForgeCallHandled(harness);
  });
});

test('a GitHub rate limit under the reserve pauses polling while the user\'s own log refresh still reaches the forge', async ({ page }) => {
  test.setTimeout(120_000);
  await withHarness(page, async (harness) => {
    await seedPullRequest(harness);
    await harness.open(page);
    const review = await openPullRequestReview(page, TITLE);
    await review.getByTestId('review-ci-chip').filter({ hasText: 'CI' }).click();
    await page.getByTestId('review-ci-job').filter({ hasText: 'build' }).click();
    const log = review.getByTestId('review-ci-log');
    await expect(log.getByTestId('review-ci-log-scroll')).toContainText('build output');

    // 100 of 5000 left: under the tenth the app keeps for the user. The
    // next poll learns it, and the one after is held.
    const reset = resetIn(12);
    await setForgeRateLimit(harness, { forge: 'github', pool: 'graphql', remaining: 100, reset });
    await setForgeRateLimit(harness, { forge: 'github', pool: 'core', remaining: 100, reset });
    const banner = review.getByTestId('review-pr-rate-limited');
    await expect(banner).toHaveText(
      `GitHub rate limit nearly used up. Updates pause until ${await clockTime(page, reset)} so your own actions still go through.`,
    );

    // Refreshing CI asks the held pool itself: the user's request goes
    // through where the poll is held.
    let before = Math.max(0, ...(await forgeInvocations(harness)).map((call) => call.seq));
    await review.getByRole('button', { name: 'Refresh CI status' }).click();
    await expect
      .poll(async () =>
        (await forgeInvocations(harness))
          .filter((call) => call.seq > before && call.via === 'http' && call.operation === 'PRTick')
          .map((call) => `${call.route} ${call.via === 'http' ? call.status : 0}`),
      )
      .toEqual(['gh graphql PRTick 200']);

    // So does refreshing the open log, revalidated against the ETag the
    // log came with.
    before = Math.max(0, ...(await forgeInvocations(harness)).map((call) => call.seq));
    await log.getByRole('button', { name: 'Refresh log' }).click();
    await expect
      .poll(async () =>
        (await forgeInvocations(harness))
          .filter((call) => call.seq > before && call.route === 'gh api job logs')
          .map((call) => (call.via === 'http' ? call.status : 0)),
      )
      .toEqual([304]);
    await expect(log.getByTestId('review-ci-log-scroll')).toContainText('build output');
    await expect(log.getByTestId('review-ci-log-error')).toHaveCount(0);
    await expect(banner).toBeVisible();

    // The pause lifts at the reset.
    await expect(banner).toHaveCount(0, { timeout: 20_000 });
    await expect(review.getByTestId('review-pr-update-error')).toHaveCount(0);
    await expectEveryForgeCallHandled(harness);
  });
});
