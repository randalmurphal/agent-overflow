// A pull request review pane kept open across a thread switch, through
// the shipped UI, the real backend and the fake forge CLI. Leaving the
// thread and coming back remounts the pane before the workspace's git
// status (where the PR comes from) has been observed again. Covered: the
// pane comes back on the PR and reloads its diff, and at no point in
// between does it show an error banner or "No changed files".

import { randomBytes } from 'node:crypto';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { expectEveryForgeCallHandled, openPullRequestReview, publishPullRequest } from './forge-helpers.js';

const PR_TITLE = 'Switch away thread';
const OTHER_TITLE = 'Elsewhere thread';

async function seedTwoThreads(harness: HarnessApp): Promise<void> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `switch-pr-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title: PR_TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
      {
        name: `switch-other-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Other\n' } }] },
        threads: [{ title: OTHER_TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  await publishPullRequest(
    harness,
    seed.projects[0].path,
    {
      forge: 'github',
      project: `ao-e2e/switch-${randomBytes(4).toString('hex')}`,
      pulls: [{ number: 12, title: 'Survive a thread switch', body: 'Still here.' }],
    },
    { 'src/parser.ts': 'export const token = parse(input);\n' },
  );
}

declare global {
  interface Window {
    __aoReviewTransientNodes?: string[];
    __aoMark?: (label: string) => void;
  }
}

test('a review pane kept across a thread switch comes back on its PR without flashing an error', async ({ harness, page }) => {
  await seedTwoThreads(harness);
  await harness.open(page);
  const review = await openPullRequestReview(page, PR_TITLE);
  await expect(review.getByTestId('review-file-header-path').first()).toBeVisible();

  // Record every error banner or empty state the pane mounts from here
  // on; a brief one is a defect even when the diff shows afterwards.
  await page.evaluate(() => {
    window.__aoReviewTransientNodes = [];
    window.__aoMark = (label) => { window.__aoReviewTransientNodes!.push(`mark:${label}`); };
    const watched = ['review-error', 'review-empty', 'review-pr-update-error'];
    const observer = new MutationObserver((mutations) => {
      for (const mutation of mutations) {
        for (const node of mutation.addedNodes) {
          if (!(node instanceof HTMLElement)) continue;
          for (const testid of watched) {
            if (node.dataset.testid === testid || node.querySelector(`[data-testid="${testid}"]`)) {
              const el = node.dataset.testid === testid ? node : node.querySelector<HTMLElement>(`[data-testid="${testid}"]`)!;
              window.__aoReviewTransientNodes!.push(`${testid}: ${el.textContent?.trim().slice(0, 120)}`);
            }
          }
        }
      }
    });
    observer.observe(document.body, { childList: true, subtree: true });
  });

  await page.evaluate(() => window.__aoMark!('to-other'));
  await page.getByTestId('thread-row').filter({ hasText: OTHER_TITLE }).click();
  await expect(page.getByTestId('chat-header-pr-badge')).toHaveCount(0);
  await page.evaluate(() => window.__aoMark!('back-to-pr'));
  await page.getByTestId('thread-row').filter({ hasText: PR_TITLE }).click();

  const back = page.locator('section[data-pane-kind="review"]');
  await expect(back.getByTestId('review-pr-header')).toBeVisible();
  await expect(back.getByTestId('review-file-header-path').first()).toBeVisible();
  // Only the click markers: nothing transient mounted between them.
  expect(await page.evaluate(() => window.__aoReviewTransientNodes)).toEqual(['mark:to-other', 'mark:back-to-pr']);

  await expectEveryForgeCallHandled(harness);
});
