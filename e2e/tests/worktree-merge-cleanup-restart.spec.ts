// The PR a thread's worktree was seen with survives a restart, and the
// worktree is still removed when it merges (thread_pull_requests,
// internal/app/app_worktree_merge_cleanup.go). The first backend tracks the
// open PR and stops; the forge then merges it while the app is closed; the
// next backend on the same data directory reads the merge on its first poll
// with no lookup of its own, and cleans up.
import { mkdtemp, rm } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import { expectEveryForgeCallHandled, forgeInvocations, seedForge } from './forge-helpers.js';
import { expectHandled, expectTracked, lookUpPR, mergePollTiming, notices, seedMergeFixture, stateReads } from './merge-cleanup-helpers.js';
import { openThread, samePath, threadRow } from './worktree-removal-helpers.js';

test('a tracked PR survives a restart and its merge still removes the worktree', async ({ page }) => {
  test.setTimeout(120_000);
  const root = await mkdtemp(join(tmpdir(), 'ao-merge-restart-'));
  const env = { AO_HARNESS_TIMING: mergePollTiming('500ms') };
  let harness: HarnessApp | undefined;
  try {
    harness = await launchHarness({ dataDir: root, env });
    const fixture = await seedMergeFixture(harness, { forge: 'github', name: 'merge-restart', titles: ['Restarted work'], branch: 'restart-branch', number: 77 });
    const [threadId] = fixture.threadIds;
    await lookUpPR(harness, fixture);
    await harness.open(page);
    await expectTracked(harness, threadId, 77);
    expect(await harness.stop()).toBe(true);
    harness = undefined;

    harness = await launchHarness({ dataDir: root, env });
    // The fake forge lives in the backend process: the new one is seeded
    // with the PR as the forge holds it now, merged.
    await seedForge(harness, [{ ...fixture.repo, pulls: [{ ...fixture.repo.pulls![0], state: 'merged', mergedAt: new Date().toISOString() }] }]);
    await harness.open(page);

    await expect.poll(async () => samePath((await threadRow(harness!, threadId)).workspacePath, fixture.root), { timeout: 30_000 }).toBe(true);
    expect(existsSync(fixture.worktreePath)).toBe(false);
    await expectHandled(harness, threadId, 'removed');
    // The new backend learned of the merge from the row it inherited: its
    // first forge call is the state read, and the only open-PR lookup is the
    // check, before removing, that the branch has no new open PR.
    const calls = (await forgeInvocations(harness)).filter((call) => call.via === 'http' && call.forge === 'github' && call.path === 'graphql');
    expect(calls.map((call) => (call.via === 'http' ? call.operation : ''))).toEqual(['PRStates', 'OpenPRsByHead']);
    expect(await stateReads(harness)).toBe(1);

    await openThread(page, 'Restarted work');
    await expect(notices(page, 'merged.')).toHaveText('PR #77 merged. Removed worktree restart-branch; this thread now runs in Base.');
    await expectEveryForgeCallHandled(harness);
  } finally {
    try {
      await page.close();
      await harness?.close();
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  }
});
