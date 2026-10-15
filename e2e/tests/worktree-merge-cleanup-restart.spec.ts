// The PR a thread's worktree was seen with survives a restart, and the
// worktree is still removed when it merges (thread_pull_requests,
// internal/app/app_worktree_merge_cleanup.go). The first backend tracks the
// open PR and stops; the forge then merges it while the app is closed; the
// next backend on the same data directory reads the merge on its first poll
// with no lookup of its own, and cleans up. With "Fetch remotes in the
// background" off, a merge the first backend read while a turn held the
// worktree is acted on by the next one at its first turn end, with no
// state read.
import { mkdtemp, rm } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import { expectEveryForgeCallHandled, forgeInvocations, seedForge } from './forge-helpers.js';
import { expectHandled, expectTracked, lookUpPR, mergePollTiming, notices, seedMergeFixture, stateReads, trackedRow } from './merge-cleanup-helpers.js';
import { RESULT_LINE, claudeTurnsScenario, emit, startMock, textLines, waitForGate } from './agent-visibility-helpers.js';
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

test('with background fetch off, a merge a previous run left waiting is acted on at the next turn end, with no state read', async ({ page }) => {
  test.setTimeout(120_000);
  const root = await mkdtemp(join(tmpdir(), 'ao-merge-restart-off-'));
  const env = { AO_HARNESS_TIMING: mergePollTiming('500ms') };
  let harness: HarnessApp | undefined;
  try {
    harness = await launchHarness({ dataDir: root, env });
    const fixture = await seedMergeFixture(harness, { forge: 'github', name: 'merge-restart-off', titles: ['Waiting work', 'Other work'], branch: 'restart-off-branch', number: 78 });
    const [threadId, otherId] = fixture.threadIds;
    await harness.rpc('UpdateSettings', { backgroundGitFetch: false });
    await harness.rpc('HarnessSetScenario', {
      scenario: claudeTurnsScenario('merge-restart-hold', [
        [emit(textLines('msg-working', 'Working on it.')), { waitSignal: { name: 'hold' } }, emit([RESULT_LINE])],
      ]),
    });
    await lookUpPR(harness, fixture);
    await harness.open(page);
    await expectTracked(harness, threadId, 78);
    // A turn holds the worktree busy while the merge is read.
    await startMock(harness, threadId);
    const holding = waitForGate(harness, 'hold');
    await harness.rpc('SendMessage', threadId, 'Start the long job.', []);
    await holding;
    await seedForge(harness, [{ ...fixture.repo, pulls: [{ ...fixture.repo.pulls![0], state: 'merged', mergedAt: new Date().toISOString() }] }]);
    await lookUpPR(harness, fixture);
    await expect.poll(() => trackedRow(harness!, threadId)?.merged_at ?? 0).toBeGreaterThan(0);
    expect(trackedRow(harness, threadId)?.handled_at).toBe(0);
    expect(await harness.stop()).toBe(true);
    harness = undefined;

    harness = await launchHarness({ dataDir: root, env });
    await seedForge(harness, [{ ...fixture.repo, pulls: [{ ...fixture.repo.pulls![0], state: 'merged', mergedAt: new Date().toISOString() }] }]);
    await harness.open(page);
    // Several polls' time: the setting is still off, so nothing is read.
    await new Promise((resolve) => setTimeout(resolve, 2_500));
    expect(existsSync(fixture.worktreePath)).toBe(true);
    expect(trackedRow(harness, threadId)?.handled_at).toBe(0);

    // A turn ending in another thread runs the act pass.
    await harness.rpc('HarnessSetScenario', {
      scenario: claudeTurnsScenario('merge-restart-other', [[emit(textLines('msg-other', 'Done.')), emit([RESULT_LINE])]]),
    });
    await startMock(harness, otherId);
    const completed = harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === otherId);
    await harness.rpc('SendMessage', otherId, 'Something else.', []);
    await completed;
    await expectHandled(harness, threadId, 'removed');
    expect(existsSync(fixture.worktreePath)).toBe(false);
    // No state read: the only forge request is the check, before removing,
    // that the branch has no new open PR.
    const calls = (await forgeInvocations(harness)).filter((call) => call.via === 'http' && call.forge === 'github' && call.path === 'graphql');
    expect(calls.map((call) => (call.via === 'http' ? call.operation : ''))).toEqual(['OpenPRsByHead']);
    expect(await stateReads(harness)).toBe(0);

    await openThread(page, 'Waiting work');
    await expect(notices(page, 'merged.')).toHaveText('PR #78 merged. Removed worktree restart-off-branch; this thread now runs in Base.');
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
