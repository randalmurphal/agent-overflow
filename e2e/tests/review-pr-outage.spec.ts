// A pull request review pane opened while the forge cannot be reached,
// through the shipped UI, the real backend and the fake forge: the
// case of a pane restored at app start before DNS is up. Covered, on
// GitHub and GitLab: the pane opens and says it is waiting with one
// retrying banner, not an error and not "No changed files"; the backend
// keeps asking the forge; once the forge answers, the diff and title bar
// load on their own with the banner gone. And on a pane already showing
// its PR, one failed poll the retry answers shows no banner, while two in
// a row do. That case polls every 2s and retries 1.5s after a failure
// (its own pr-update timing), so the spec can bring the forge back
// between a failure and its retry.

import { randomBytes } from 'node:crypto';

import type { Page } from '@playwright/test';
import { HARNESS_TIMING, launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  expectEveryForgeCallHandled,
  forgeInvocations,
  openPullRequestReview,
  publishPullRequest,
  setForgeOffline,
  type ForgeInvocation,
  type ForgeRepo,
} from './forge-helpers.js';

const TITLE = 'Outage thread';

const REPOS: Record<ForgeRepo['forge'], () => ForgeRepo> = {
  github: () => ({
    forge: 'github',
    project: `ao-e2e/outage-${randomBytes(4).toString('hex')}`,
    pulls: [{ number: 12, title: 'Survive an outage', body: 'Opened before DNS was up.' }],
  }),
  gitlab: () => ({
    forge: 'gitlab',
    project: `ao-e2e/outage-${randomBytes(4).toString('hex')}`,
    pulls: [{ number: 384, title: 'Survive an outage', body: 'Opened before DNS was up.' }],
  }),
};

async function seedOutagePullRequest(harness: HarnessApp, forge: ForgeRepo['forge']): Promise<void> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `outage-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  await publishPullRequest(harness, seed.projects[0].path, REPOS[forge](), {
    'src/parser.ts': 'export const token = parse(input);\n',
  });
}

for (const forge of ['github', 'gitlab'] as const) {
  test(`a ${forge} review pane opened while the forge is unreachable waits, retries and loads when it answers`, async ({
    harness,
    page,
  }) => {
    await seedOutagePullRequest(harness, forge);
    await harness.open(page);
    // The pane has been opened once, so the PR is known; closing it drops
    // the backend's poller, and the next open starts a fresh one.
    const review = await openPullRequestReview(page, TITLE);
    await expect(review.getByTestId('review-file-header-path').first()).toBeVisible();
    await review.getByTestId('review-close').click();
    await expect(page.locator('section[data-pane-kind="review"]')).toHaveCount(0);

    await setForgeOffline(harness, true);
    const before = (await forgeInvocations(harness)).length;
    await page.getByTestId('chat-header-pr-badge').click();
    const reopened = page.locator('section[data-pane-kind="review"]');
    await expect(reopened.getByTestId('review-awaiting-pr')).toBeVisible();
    await expect(reopened.getByTestId('review-pr-update-error')).toContainText('Retrying');
    await expect(reopened.getByTestId('review-error')).toHaveCount(0);
    await expect(reopened.getByTestId('review-empty')).toHaveCount(0);
    await expect(reopened.getByTestId('review-pr-header')).toHaveCount(0);
    // The backend keeps asking: more than the one call the open made.
    await expect
      .poll(async () => (await forgeInvocations(harness)).slice(before).filter((call) => call.route === 'offline').length)
      .toBeGreaterThan(1);
    await expect(reopened.getByTestId('review-pr-update-error')).toHaveCount(1);

    await setForgeOffline(harness, false);
    await expect(reopened.getByTestId('review-file-header-path').first()).toBeVisible();
    await expect(reopened.getByTestId('review-pr-header')).toBeVisible();
    await expect(reopened.getByTestId('review-pr-update-error')).toHaveCount(0);
    await expect(reopened.getByTestId('review-awaiting-pr')).toHaveCount(0);
    await expect(reopened.getByTestId('review-error')).toHaveCount(0);

    await expectEveryForgeCallHandled(harness);
  });
}

/** The snapshot polls (a PRTick for the detail) after `since` that the
 * offline forge dropped, and those it answered. */
function snapshotPolls(calls: ForgeInvocation[], since: number, route: string): number {
  return calls.filter(
    (call) => call.seq > since && call.via === 'http' && call.operation === 'PRTick' &&
      call.variables?.wantDetail === true && call.route === route,
  ).length;
}

async function lastSeq(harness: HarnessApp): Promise<number> {
  return Math.max(0, ...(await forgeInvocations(harness)).map((call) => call.seq));
}

/** Records in the page whether the retrying banner ever mounted. */
async function watchForBanner(page: Page): Promise<void> {
  await page.evaluate(() => {
    const w = window as unknown as { __aoSawRetrying: boolean };
    w.__aoSawRetrying = false;
    new MutationObserver(() => {
      if (document.querySelector('[data-testid="review-pr-update-error"]')) w.__aoSawRetrying = true;
    }).observe(document.body, { childList: true, subtree: true });
  });
}

test('a pane showing its PR rides out one failed poll the retry answers, and shows the banner when the retry fails too', async ({ page }) => {
  test.setTimeout(120_000);
  const timing = HARNESS_TIMING.split(',').filter((entry) => !entry.startsWith('pr-update-retry='));
  const harness = await launchHarness({
    env: { AO_HARNESS_TIMING: [...timing, 'pr-update=2s', 'pr-update-retry=1500ms'].join(',') },
  });
  try {
    await seedOutagePullRequest(harness, 'github');
    await harness.open(page);
    const review = await openPullRequestReview(page, TITLE);
    await expect(review.getByTestId('review-file-header-path').first()).toBeVisible();
    await watchForBanner(page);

    // One poll fails; the forge is back before its retry.
    let mark = await lastSeq(harness);
    await setForgeOffline(harness, true);
    await expect
      .poll(async () => snapshotPolls(await forgeInvocations(harness), mark, 'offline'), { intervals: [50], timeout: 10_000 })
      .toBe(1);
    await setForgeOffline(harness, false);
    mark = await lastSeq(harness);
    await expect
      .poll(async () => snapshotPolls(await forgeInvocations(harness), mark, 'gh graphql PRTick'), { timeout: 10_000 })
      .toBeGreaterThan(0);
    expect(await page.evaluate(() => (window as unknown as { __aoSawRetrying: boolean }).__aoSawRetrying)).toBe(false);
    await expect(review.getByTestId('review-pr-header')).toBeVisible();

    // The poll and its retry both fail: the banner shows over the PR.
    mark = await lastSeq(harness);
    await setForgeOffline(harness, true);
    await expect(review.getByTestId('review-pr-update-error')).toContainText('Retrying', { timeout: 15_000 });
    expect(snapshotPolls(await forgeInvocations(harness), mark, 'offline')).toBeGreaterThanOrEqual(2);
    await expect(review.getByTestId('review-pr-header')).toBeVisible();

    await setForgeOffline(harness, false);
    await expect(review.getByTestId('review-pr-update-error')).toHaveCount(0, { timeout: 15_000 });
    await expectEveryForgeCallHandled(harness);
  } finally {
    await page.close();
    await harness.close();
  }
});
