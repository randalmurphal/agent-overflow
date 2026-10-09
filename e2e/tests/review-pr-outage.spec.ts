// A pull request review pane opened while the forge cannot be reached,
// through the shipped UI, the real backend and the fake forge CLI: the
// case of a pane restored at app start before DNS is up. Covered, on
// GitHub and GitLab: the pane opens and says it is waiting with one
// retrying banner, not an error and not "No changed files"; the backend
// keeps asking the forge; once the forge answers, the diff and title bar
// load on their own with the banner gone.

import { randomBytes } from 'node:crypto';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  expectEveryForgeCallHandled,
  forgeInvocations,
  openPullRequestReview,
  publishPullRequest,
  setForgeOffline,
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
