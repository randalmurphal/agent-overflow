// The chat header's PR/MR badge when the forge cannot be reached, through
// the shipped UI, the real backend and the fake forge. Opening a thread
// re-asks the forge for its branch's open PR; covered, on GitHub and
// GitLab: a thread opened again while the forge fails keeps the PR it was
// known to have, with the git menu still offering to open it and the
// review pane opening on it and loading once the forge answers; and a
// failed lookup is asked again on its own, so a PR the app could not see
// appears once the forge answers, with nothing changed on disk and no
// navigation. The second case shortens the first retry of a failed lookup
// to 300ms (pr-lookup-retry).

import { randomBytes } from 'node:crypto';

import type { Page } from '@playwright/test';
import { HARNESS_TIMING, launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  expectEveryForgeCallHandled,
  forgeInvocations,
  publishPullRequest,
  setForgeOffline,
  type ForgeInvocation,
  type ForgeRepo,
} from './forge-helpers.js';

type Forge = ForgeRepo['forge'];

const TITLE = 'Badge thread';
const OTHER = 'Elsewhere thread';
const NUMBER: Record<Forge, number> = { github: 12, gitlab: 384 };
const LABELS: Record<Forge, { badge: string; open: string; create: string; noun: string }> = {
  github: { badge: '#12', open: 'Open PR', create: 'Create PR', noun: 'PR' },
  gitlab: { badge: '!384', open: 'Open MR', create: 'Create MR', noun: 'MR' },
};

/** Seeds a project whose thread's branch has an open PR on `forge`, and a
 * second project with no forge to switch to. Answers nothing; the threads
 * are found by title. */
async function seedBadgeFixture(harness: HarnessApp, forge: Forge): Promise<void> {
  const tag = randomBytes(4).toString('hex');
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `badge-${tag}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title: TITLE, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
      {
        name: `elsewhere-${tag}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Elsewhere\n' } }] },
        threads: [{ title: OTHER, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  await publishPullRequest(harness, seed.projects[0].path, {
    forge,
    project: `ao-e2e/badge-${tag}`,
    pulls: [{ number: NUMBER[forge], title: 'Keep the badge', body: 'Seen open before the outage.' }],
  });
}

async function openThread(page: Page, title: string): Promise<void> {
  await page.getByTestId('thread-row').filter({ hasText: title }).click();
  await expect(page.getByLabel('Message Input')).toBeVisible();
}

/** Whether call is the app asking the forge for a branch's open PR/MR: a
 * GitHub OpenPRsByHead query, or a GitLab merge request list by source
 * branch (its path carries the query). */
function isOpenPRLookup(call: ForgeInvocation): boolean {
  if (call.via !== 'http') return false;
  return call.forge === 'github' ? call.operation === 'OpenPRsByHead' : /\/merge_requests\?.*source_branch=/.test(call.path);
}

async function lastSeq(harness: HarnessApp): Promise<number> {
  return Math.max(0, ...(await forgeInvocations(harness)).map((call) => call.seq));
}

/** Waits until the forge refused an open-PR lookup made after `since`. */
async function expectFailedLookupSince(harness: HarnessApp, since: number): Promise<void> {
  await expect
    .poll(async () => (await forgeInvocations(harness)).filter((call) => call.seq > since && call.route === 'offline' && isOpenPRLookup(call)).length, {
      timeout: 15_000,
    })
    .toBeGreaterThan(0);
}

async function gitMenuItem(page: Page, name: RegExp) {
  await page.getByRole('button', { name: 'More git actions' }).click();
  return page.getByRole('menu', { name: 'Git actions' }).getByRole('menuitem', { name });
}

for (const forge of ['github', 'gitlab'] as const) {
  const labels = LABELS[forge];

  test(`a ${forge} thread opened again while the forge fails keeps its ${labels.noun} badge`, async ({ harness, page }) => {
    await seedBadgeFixture(harness, forge);
    await harness.open(page);
    await openThread(page, TITLE);
    const badge = page.getByTestId('chat-header-pr-badge');
    await expect(badge).toContainText(labels.badge, { timeout: 30_000 });

    // Leaving the thread releases its workspace's status stream; nothing
    // else watches it.
    await openThread(page, OTHER);
    await expect(badge).toHaveCount(0);

    await setForgeOffline(harness, true);
    const mark = await lastSeq(harness);
    await openThread(page, TITLE);
    await expect(badge).toContainText(labels.badge);
    // The re-check went to the forge and failed: the badge stays, and the
    // git menu still opens the PR rather than offering to create one.
    await expectFailedLookupSince(harness, mark);
    await expect(badge).toContainText(labels.badge);
    const open = await gitMenuItem(page, new RegExp(`^${labels.open}`));
    await expect(open).toBeEnabled();
    await page.keyboard.press('Escape');

    // The review pane opens on that PR, waiting on the forge, and loads it
    // once the forge answers.
    await badge.click();
    const review = page.locator('section[data-pane-kind="review"]');
    await expect(review.getByTestId('review-pr-update-error')).toContainText('Retrying');
    await setForgeOffline(harness, false);
    await expect(review.getByTestId('review-pr-header')).toBeVisible();
    await expect(review.getByTestId('review-file-header-path').first()).toBeVisible();
    await expect(badge).toContainText(labels.badge);
    await expectEveryForgeCallHandled(harness);
  });
}

for (const forge of ['github', 'gitlab'] as const) {
  const labels = LABELS[forge];

  test(`a ${forge} ${labels.noun} the app could not look up appears on its own once the forge answers`, async ({ page }) => {
    const harness = await launchHarness({ env: { AO_HARNESS_TIMING: `${HARNESS_TIMING},pr-lookup-retry=300ms` } });
    try {
      // Offline before the app ever asks: there is no last known PR to show.
      await setForgeOffline(harness, true);
      await seedBadgeFixture(harness, forge);
      const mark = await lastSeq(harness);
      await harness.open(page);
      await openThread(page, TITLE);
      await expectFailedLookupSince(harness, mark);
      const badge = page.getByTestId('chat-header-pr-badge');
      await expect(badge).toHaveCount(0);
      const create = await gitMenuItem(page, new RegExp(`^${labels.create}`));
      await expect(create).toContainText(`Could not check existing ${labels.noun}`);
      await page.keyboard.press('Escape');

      // The forge comes back. Nothing on disk changes and the page stays
      // on the thread; the failed lookup's re-check finds the PR. The retry
      // delay doubles per failure, so it can reach about as long as the
      // forge was offline.
      await setForgeOffline(harness, false);
      await expect(badge).toContainText(labels.badge, { timeout: 20_000 });
      await expectEveryForgeCallHandled(harness);
    } finally {
      await page.close();
      await harness.close();
    }
  });
}
