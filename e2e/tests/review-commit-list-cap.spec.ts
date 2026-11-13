// A pull request with more commits than the backend lists (it lists the
// newest 300 and counts the rest), on GitHub, through the shipped UI, the
// real backend and the fake forge. Covered: the commit picker numbers the
// commits from the PR's first (the newest is 302/302), says once in its
// menu that only the newest 300 of 302 are listed, keeps `From here` on
// the oldest listed commit, and a run from it to the head is a run of
// 300 commits (3–302/302), not All commits.

import { randomBytes } from 'node:crypto';

import { expect, test, type SeedResult } from './fixtures.js';
import { expectEveryForgeCallHandled, openPullRequestReview, publishPullRequest, type PullCommit } from './forge-helpers.js';

const TITLE = 'Long history';
const COMMITS = 302;

test('a pull request past the commit list cap is numbered from its first commit and keeps From here on the oldest listed', async ({ harness, page }) => {
  test.setTimeout(180_000);
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: `long-history-${randomBytes(4).toString('hex')}`,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
      threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }],
  });
  const commits: PullCommit[] = Array.from({ length: COMMITS }, (_, index) => ({
    message: `step ${String(index + 1).padStart(3, '0')}`,
    ...(index === COMMITS - 1 ? { files: { 'notes.txt': 'done\n' } } : {}),
  }));
  await publishPullRequest(
    harness,
    seeded.projects[0].path,
    { forge: 'github', project: `ao-e2e/long-history-${randomBytes(4).toString('hex')}`, pulls: [{ number: 7, title: 'Long history' }] },
    { commits },
  );
  await harness.open(page);
  const review = await openPullRequestReview(page, TITLE);
  const trigger = review.getByTestId('review-commit-select');
  await expect(trigger).toHaveText(/All commits/, { timeout: 30_000 });
  const width = (await trigger.boundingBox())!.width;

  await trigger.click();
  const menu = page.locator('[data-popover]').last();
  await expect(menu.getByTestId('review-commit-option')).toHaveCount(300);
  await expect(menu.getByTestId('review-commit-position')).toHaveText('302 commits');
  await expect(menu.getByTestId('review-commit-list-note')).toHaveText('Only the newest 300 of 302 commits are listed.');
  const oldestListed = menu.getByTestId('review-commit-option').filter({ hasText: 'step 003' });
  await oldestListed.scrollIntoViewIfNeeded();
  await oldestListed.hover();
  await oldestListed.getByRole('button', { name: /through the newest/ }).click();

  await expect(trigger).toHaveText(/^\s*3–302\/302\s*$/, { timeout: 30_000 });
  const card = review.getByTestId('review-range-card');
  await expect(card.getByTestId('review-range-card-count')).toHaveText('300 commits');
  await expect(card).toContainText('3–302 of 302 · to the head');
  // The trigger kept its width.
  expect((await trigger.boundingBox())!.width).toBe(width);
  await expectEveryForgeCallHandled(harness);
});
