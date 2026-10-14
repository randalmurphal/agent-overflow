// A merged PR's worktree is cleaned up as soon as an open-PR lookup of the
// watched worktree stops finding the PR, without waiting for the
// per-repository poll (internal/app/app_worktree_merge_cleanup.go). The
// poll is set to ten minutes here, so only that transition can act: the
// thread is open on screen, its pane's git status tracks the PR, the PR
// merges, and the next lookup (a refresh after a git action) answers "no
// open PR". One state read follows, and the worktree goes.
import { expect, test as base } from './fixtures.js';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expectEveryForgeCallHandled } from './forge-helpers.js';
import { expectHandled, expectTracked, lookUpPR, mergePollTiming, mergePR, notices, prLabel, seedMergeFixture, stateReads, type Forge } from './merge-cleanup-helpers.js';
import { basename, errorToasts, openThread, samePath, threadRow } from './worktree-removal-helpers.js';
import { existsSync } from 'node:fs';

const test = base.extend<object, { harnessWorker: HarnessApp }>({
  harnessWorker: [
    async ({}, use) => {
      const app = await launchHarness({ env: { AO_HARNESS_TIMING: mergePollTiming('10m') } });
      await use(app);
      await app.close();
    },
    { scope: 'worker' },
  ],
});

for (const forge of ['github', 'gitlab'] as Forge[]) {
  test(`a ${forge} PR that merges while its thread is on screen is cleaned up at the next lookup`, async ({ harness, page }) => {
    const fixture = await seedMergeFixture(harness, { forge, name: `merge-watched-${forge}`, titles: ['Watched work'], branch: 'watched-branch', number: 3 });
    const [threadId] = fixture.threadIds;
    await harness.open(page);
    // Opening the thread subscribes its git status, whose lookup finds the PR.
    await openThread(page, 'Watched work');
    await expect(page.getByTestId('chat-header-pr-badge')).toBeVisible({ timeout: 30_000 });
    await expect(page.getByTestId('env-picker-trigger')).toHaveText(basename(fixture.worktreePath));
    await expectTracked(harness, threadId, 3);
    expect(await stateReads(harness), 'the ten-minute poll read the forge').toBe(0);

    await mergePR(harness, fixture);
    await lookUpPR(harness, fixture);

    await expect(page.getByTestId('env-picker-trigger')).toHaveText('Base', { timeout: 10_000 });
    await expect(notices(page, 'merged.')).toHaveText(`${prLabel(forge, 3)} merged. Removed worktree watched-branch; this thread now runs in Base.`);
    expect(await samePath((await threadRow(harness, threadId)).workspacePath, fixture.root)).toBe(true);
    expect(existsSync(fixture.worktreePath)).toBe(false);
    await expectHandled(harness, threadId, 'removed');
    expect(await stateReads(harness), 'the transition reads the PR once').toBe(1);
    await expectEveryForgeCallHandled(harness);
    await expect(errorToasts(page)).toHaveCount(0);
  });
}
