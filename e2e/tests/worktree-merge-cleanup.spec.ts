// A thread's worktree is removed when its pull or merge request merges
// (internal/app/app_worktree_merge_cleanup.go, docs/decisions.md). The
// backend tracks the PR a thread's worktree branch was seen with OPEN and
// reads its state from the fake forge on the per-repository poll, here
// shortened to 400ms (merged-pr-poll).
//
// Covered: a merged PR of an idle, clean worktree no pane shows removes the
// worktree, moves the thread to Base with a neutral notice and keeps the
// branch, on GitHub and GitLab; a PR closed without merging changes nothing
// and stops being read; uncommitted changes keep the worktree with one
// warning notice that does not repeat, as does a removal git refuses, with
// no path in it; a worktree moved to another branch is left alone; a
// running turn delays the cleanup until it ends; a message the user sent
// after the merge, or a new open PR on the branch, leaves the worktree
// alone; a shared worktree moves both
// threads and tells both; a PR the app's create action or an agent's
// `glab mr create` opens is tracked with no pane open.
import { expect, test as base } from './fixtures.js';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import {
  RESULT_LINE,
  advance,
  claudeScenario,
  claudeTurnsScenario,
  emit,
  startMock,
  textLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';
import { expectEveryForgeCallHandled, seedForge } from './forge-helpers.js';
import {
  expectHandled,
  expectNoMoreStateReads,
  expectTracked,
  lookUpPR,
  mergePollTiming,
  mergePR,
  movePR,
  notices,
  prLabel,
  seedMergeFixture,
  stateReads,
  trackedRow,
  type Forge,
} from './merge-cleanup-helpers.js';
import { basename, errorToasts, harnessGit, listWorktrees, openThread, samePath, threadRow } from './worktree-removal-helpers.js';
import { existsSync } from 'node:fs';
import { writeFile } from 'node:fs/promises';
import path from 'node:path';

const POLL_MS = 400;

const test = base.extend<object, { harnessWorker: HarnessApp }>({
  harnessWorker: [
    async ({}, use) => {
      const app = await launchHarness({ env: { AO_HARNESS_TIMING: mergePollTiming(`${POLL_MS}ms`) } });
      await use(app);
      await app.close();
    },
    { scope: 'worker' },
  ],
});

async function expectWorktreeKept(harness: HarnessApp, threadId: string, fixture: { projectId: string; root: string; worktreePath: string }) {
  expect(existsSync(fixture.worktreePath), 'the worktree directory was removed').toBe(true);
  const listed = await listWorktrees(harness, fixture.projectId, fixture.root);
  expect(listed.some((item) => item.path === fixture.worktreePath), 'the worktree left git').toBe(true);
  expect(await samePath((await threadRow(harness, threadId)).workspacePath, fixture.worktreePath), 'the thread left its worktree').toBe(true);
}

for (const forge of ['github', 'gitlab'] as Forge[]) {
  test(`a merged ${forge} PR removes the idle worktree no pane shows, moves its thread to Base and keeps the branch`, async ({ harness, page }, testInfo) => {
    const name = `merge-clean-${forge}`;
    const fixture = await seedMergeFixture(harness, { forge, name, titles: ['Merged work', 'Elsewhere'], branch: 'fix-pr-diff-fetch', number: 90 });
    const [threadId] = fixture.threadIds;
    await lookUpPR(harness, fixture);

    // The page shows another thread; nothing watches the worktree.
    await harness.open(page);
    await openThread(page, 'Elsewhere');
    await expectTracked(harness, threadId, 90);

    await mergePR(harness, fixture);
    await expect.poll(async () => samePath((await threadRow(harness, threadId)).workspacePath, fixture.root), { timeout: 20_000 }).toBe(true);
    expect((await threadRow(harness, threadId)).worktreePath ?? '').toBe('');
    expect(existsSync(fixture.worktreePath), 'the worktree directory is still there').toBe(false);
    expect((await listWorktrees(harness, fixture.projectId, fixture.root)).map((item) => item.path)).not.toContain(fixture.worktreePath);
    // The branch outlives its worktree, locally and on the remote.
    expect(harnessGit(harness, fixture.root, 'rev-parse', '--verify', '--quiet', 'refs/heads/fix-pr-diff-fetch')).not.toBe('');
    expect(harnessGit(harness, fixture.root, 'rev-parse', '--verify', '--quiet', 'refs/remotes/origin/fix-pr-diff-fetch')).not.toBe('');
    expect((await expectHandled(harness, threadId, 'removed')).merged_at).toBeGreaterThan(0);

    // The thread says what happened, in the neutral tone, with no action.
    await openThread(page, 'Merged work');
    await expect(page.getByTestId('env-picker-trigger')).toHaveText('Base');
    const notice = notices(page, 'merged.');
    await expect(notice).toHaveText(`${prLabel(forge, 90)} merged. Removed worktree fix-pr-diff-fetch; this thread now runs in Base.`);
    await expect(notice).toHaveClass(/text-fg-subtle/);
    await expect(notice).not.toHaveClass(/text-warning/);
    await expect(notice.locator('a, button')).toHaveCount(0);
    if (forge === 'github') await notice.screenshot({ path: testInfo.outputPath('merged-notice-removed.png') });

    // Settled once: the repository is not read again.
    await expectNoMoreStateReads(harness, POLL_MS);
    await expect(notice).toHaveCount(1);
    await expectEveryForgeCallHandled(harness);
    await expect(errorToasts(page)).toHaveCount(0);
  });
}

test('unpushed commits do not keep a merged PR\'s worktree: the branch keeps them', async ({ harness, page }) => {
  const fixture = await seedMergeFixture(harness, { forge: 'github', name: 'merge-unpushed', titles: ['Unpushed work'], branch: 'unpushed-branch', number: 21 });
  const [threadId] = fixture.threadIds;
  await lookUpPR(harness, fixture);
  await harness.open(page);
  await expectTracked(harness, threadId, 21);

  await writeFile(path.join(fixture.worktreePath, 'late.txt'), 'after review\n');
  harnessGit(harness, fixture.worktreePath, 'add', 'late.txt');
  harnessGit(harness, fixture.worktreePath, 'commit', '--quiet', '-m', 'late commit');
  const head = harnessGit(harness, fixture.worktreePath, 'rev-parse', 'HEAD');
  await mergePR(harness, fixture);

  await expectHandled(harness, threadId, 'removed');
  expect(existsSync(fixture.worktreePath)).toBe(false);
  expect(harnessGit(harness, fixture.root, 'rev-parse', 'refs/heads/unpushed-branch')).toBe(head);
  await openThread(page, 'Unpushed work');
  await expect(notices(page, 'merged.')).toHaveText('PR #21 merged. Removed worktree unpushed-branch; this thread now runs in Base.');
  await expectEveryForgeCallHandled(harness);
});

test('a PR on the project root\'s branch is never tracked, and its merge removes nothing', async ({ harness, page }) => {
  const fixture = await seedMergeFixture(harness, { forge: 'github', name: 'merge-root', titles: ['Worktree thread', 'Root thread'], branch: 'wt-branch', number: 1 });
  const rootId = fixture.threadIds[1];
  // The root checks out a branch of its own with an open PR.
  harnessGit(harness, fixture.root, 'checkout', '--quiet', '-b', 'root-branch');
  harnessGit(harness, fixture.root, 'push', '--quiet', path.join(harness.bootstrap.dataRoot, 'forge-origin', `${fixture.repo.project}.git`), 'root-branch');
  harnessGit(harness, fixture.root, 'fetch', '--quiet', 'origin');
  const rootPR = { number: 2, title: 'Root work', headRef: 'root-branch', baseRef: 'main' };
  await movePR(harness, fixture, {}, [rootPR]);
  await harness.open(page);
  await harness.rpc('GetGitStatus', { projectId: fixture.projectId, workspacePath: fixture.root });
  await openThread(page, 'Root thread');
  await expect(page.getByTestId('chat-header-pr-badge')).toBeVisible({ timeout: 30_000 });

  await movePR(harness, fixture, {}, [{ ...rootPR, state: 'merged', mergedAt: new Date().toISOString() }]);
  await expectNoMoreStateReads(harness, POLL_MS);
  expect(trackedRow(harness, rootId), 'a thread in the project root was tracked').toBeUndefined();
  expect(await stateReads(harness)).toBe(0);
  expect(await samePath((await threadRow(harness, rootId)).workspacePath, fixture.root)).toBe(true);
  expect(harnessGit(harness, fixture.root, 'rev-parse', '--abbrev-ref', 'HEAD')).toBe('root-branch');
  await expect(notices(page, 'merged')).toHaveCount(0);
  await expectEveryForgeCallHandled(harness);
});

test('an old merged PR found by the branch name is never adopted', async ({ harness, page }) => {
  const fixture = await seedMergeFixture(harness, { forge: 'gitlab', name: 'merge-old', titles: ['Reused name'], branch: 'old-name', number: 8 });
  const [threadId] = fixture.threadIds;
  // The branch name carried an MR that merged long ago; none is open now.
  await movePR(harness, fixture, { state: 'merged', mergedAt: '2026-01-01T00:00:00Z' });
  await lookUpPR(harness, fixture);
  await harness.open(page);
  await openThread(page, 'Reused name');
  await expectNoMoreStateReads(harness, POLL_MS);
  expect(trackedRow(harness, threadId)).toBeUndefined();
  expect(await stateReads(harness)).toBe(0);
  await expectWorktreeKept(harness, threadId, fixture);
  await expect(notices(page, 'merged')).toHaveCount(0);
  await expectEveryForgeCallHandled(harness);
});

test('a PR closed without merging leaves the worktree and is no longer read', async ({ harness, page }) => {
  const fixture = await seedMergeFixture(harness, { forge: 'github', name: 'merge-closed', titles: ['Closed work'], branch: 'closed-branch', number: 7 });
  const [threadId] = fixture.threadIds;
  await lookUpPR(harness, fixture);
  await harness.open(page);
  await expectTracked(harness, threadId, 7);

  await movePR(harness, fixture, { state: 'closed' });
  const row = await expectHandled(harness, threadId, 'closed');
  expect(row.merged_at).toBe(0);
  await expectWorktreeKept(harness, threadId, fixture);
  await expectNoMoreStateReads(harness, POLL_MS);

  await openThread(page, 'Closed work');
  await expect(page.getByTestId('env-picker-trigger')).toHaveText(basename(fixture.worktreePath));
  await expect(notices(page, 'merged')).toHaveCount(0);
  await expectEveryForgeCallHandled(harness);
});

test('uncommitted changes keep the merged PR\'s worktree with one warning notice', async ({ harness, page }, testInfo) => {
  const fixture = await seedMergeFixture(harness, { forge: 'gitlab', name: 'merge-dirty', titles: ['Dirty work'], branch: 'dirty-branch', number: 12 });
  const [threadId] = fixture.threadIds;
  await lookUpPR(harness, fixture);
  await harness.open(page);
  await openThread(page, 'Dirty work');
  await expectTracked(harness, threadId, 12);

  // One edited tracked file and one new, untracked one.
  await writeFile(path.join(fixture.worktreePath, 'README.md'), '# edited\n');
  await writeFile(path.join(fixture.worktreePath, 'notes.txt'), 'scratch\n');
  await mergePR(harness, fixture);

  await expectHandled(harness, threadId, 'kept');
  const notice = notices(page, 'merged.');
  await expect(notice).toHaveText('MR !12 merged. Kept worktree dirty-branch: 2 uncommitted files.');
  await expect(notice).toHaveClass(/text-warning/);
  await expect(notice.locator('a, button')).toHaveCount(0);
  await notice.screenshot({ path: testInfo.outputPath('merged-notice-kept.png') });
  await expectWorktreeKept(harness, threadId, fixture);

  // Later polls neither read it again nor repeat the notice.
  await expectNoMoreStateReads(harness, POLL_MS);
  await expect(notice).toHaveCount(1);
  await expect(page.getByTestId('env-picker-trigger')).toHaveText(basename(fixture.worktreePath));
  await expectEveryForgeCallHandled(harness);
});

test('a clean worktree git refuses to remove is kept with a warning that names no path', async ({ harness, page }) => {
  const fixture = await seedMergeFixture(harness, { forge: 'github', name: 'merge-refused', titles: ['Refused work'], branch: 'refused-branch', number: 14 });
  const [threadId] = fixture.threadIds;
  await lookUpPR(harness, fixture);
  await harness.open(page);
  await openThread(page, 'Refused work');
  await expectTracked(harness, threadId, 14);

  // git refuses to remove a worktree holding a submodule without --force;
  // committed, the tree is clean.
  const origin = path.join(harness.bootstrap.dataRoot, 'forge-origin', `${fixture.repo.project}.git`);
  harnessGit(harness, fixture.worktreePath, '-c', 'protocol.file.allow=always', 'submodule', '--quiet', 'add', origin, 'vendored');
  harnessGit(harness, fixture.worktreePath, 'commit', '--quiet', '-m', 'vendor a submodule');
  expect(harnessGit(harness, fixture.worktreePath, 'status', '--porcelain')).toBe('');
  await mergePR(harness, fixture);

  await expectHandled(harness, threadId, 'kept');
  const notice = notices(page, 'merged.');
  await expect(notice).toHaveText('PR #14 merged. Kept worktree refused-branch: removing it failed.');
  await expect(notice).toHaveClass(/text-warning/);
  await expectWorktreeKept(harness, threadId, fixture);
  await expectEveryForgeCallHandled(harness);
});

test('a worktree switched to another branch is left alone when the old branch\'s PR merges', async ({ harness, page }) => {
  const fixture = await seedMergeFixture(harness, { forge: 'gitlab', name: 'merge-moved', titles: ['Moved on'], branch: 'moved-branch', number: 16 });
  const [threadId] = fixture.threadIds;
  await lookUpPR(harness, fixture);
  await harness.open(page);
  await expectTracked(harness, threadId, 16);

  harnessGit(harness, fixture.worktreePath, 'checkout', '--quiet', '-b', 'next-branch');
  await mergePR(harness, fixture);

  await expectHandled(harness, threadId, 'skipped');
  await expectWorktreeKept(harness, threadId, fixture);
  expect(harnessGit(harness, fixture.worktreePath, 'rev-parse', '--abbrev-ref', 'HEAD')).toBe('next-branch');
  await openThread(page, 'Moved on');
  await expect(notices(page, 'merged')).toHaveCount(0);
  await expectEveryForgeCallHandled(harness);
});

test('a turn running when the PR merges delays the cleanup until it ends', async ({ harness, page }) => {
  const fixture = await seedMergeFixture(harness, { forge: 'github', name: 'merge-busy', titles: ['Busy work'], branch: 'busy-branch', number: 31 });
  const [threadId] = fixture.threadIds;
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('merge-busy-hold', [
      [emit(textLines('msg-working', 'Working on it.')), { waitSignal: { name: 'hold' } }, emit([RESULT_LINE])],
    ]),
  });
  await lookUpPR(harness, fixture);
  await harness.open(page);
  await openThread(page, 'Busy work');
  await expectTracked(harness, threadId, 31);

  const mockId = await startMock(harness, threadId);
  const holding = waitForGate(harness, 'hold');
  await harness.rpc('SendMessage', threadId, 'Start the long job.', []);
  await holding;
  await expect(page.getByText('Working on it.')).toBeVisible();

  await mergePR(harness, fixture);
  await expect.poll(() => trackedRow(harness, threadId)?.merged_at ?? 0).toBeGreaterThan(0);
  // Several polls later the turn still runs and the worktree stays.
  await new Promise((resolve) => setTimeout(resolve, POLL_MS * 4));
  expect(trackedRow(harness, threadId)?.handled_at).toBe(0);
  await expectWorktreeKept(harness, threadId, fixture);

  const completed = harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await advance(harness, mockId, 'hold');
  await completed;
  await expect(page.getByTestId('env-picker-trigger')).toHaveText('Base', { timeout: 15_000 });
  await expect(notices(page, 'merged.')).toHaveText('PR #31 merged. Removed worktree busy-branch; this thread now runs in Base.');
  await expectHandled(harness, threadId, 'removed');
  expect(existsSync(fixture.worktreePath)).toBe(false);
  await expectEveryForgeCallHandled(harness);
  await expect(errorToasts(page)).toHaveCount(0);
});

test('a message the user sent after the merge keeps the worktree, with no notice', async ({ harness, page }) => {
  const fixture = await seedMergeFixture(harness, { forge: 'github', name: 'merge-active', titles: ['Active work'], branch: 'active-branch', number: 44 });
  const [threadId] = fixture.threadIds;
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('merge-active', [[emit([...textLines('msg-more', 'More work done.'), RESULT_LINE])]]),
  });
  await lookUpPR(harness, fixture);
  await harness.open(page);
  await openThread(page, 'Active work');
  await expectTracked(harness, threadId, 44);

  // The PR merged a moment before the user wrote again; the poll learns of
  // the merge only after.
  const mergedAt = new Date(Date.now() - 1_000);
  const completed = harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await harness.rpc('SendMessage', threadId, 'One more thing on this branch.', []);
  await completed;
  await mergePR(harness, fixture, mergedAt);

  const row = await expectHandled(harness, threadId, 'skipped');
  expect(row.merged_at).toBe(mergedAt.getTime());
  await expectWorktreeKept(harness, threadId, fixture);
  await expect(page.getByTestId('env-picker-trigger')).toHaveText(basename(fixture.worktreePath));
  await expect(notices(page, 'merged')).toHaveCount(0);
  await expectEveryForgeCallHandled(harness);
});

test('a new open PR on the merged PR\'s branch keeps the worktree and is tracked instead', async ({ harness, page }) => {
  const fixture = await seedMergeFixture(harness, { forge: 'gitlab', name: 'merge-reused', titles: ['Reused branch'], branch: 'reused-branch', number: 5 });
  const [threadId] = fixture.threadIds;
  await lookUpPR(harness, fixture);
  await harness.open(page);
  await openThread(page, 'Reused branch');
  await expectTracked(harness, threadId, 5);

  // The branch got a follow-up MR before the first one's merge was seen.
  await movePR(harness, fixture, { state: 'merged', mergedAt: new Date().toISOString() }, [
    { number: 6, title: 'Follow-up', headRef: 'reused-branch', baseRef: 'main' },
  ]);
  await expectTracked(harness, threadId, 6);
  await expectWorktreeKept(harness, threadId, fixture);
  await expect(notices(page, 'merged')).toHaveCount(0);
  await expectEveryForgeCallHandled(harness);
});

test('a merged PR\'s shared worktree moves both threads to Base and tells both', async ({ harness, page }) => {
  const fixture = await seedMergeFixture(harness, { forge: 'github', name: 'merge-shared', titles: ['First sharer', 'Second sharer'], branch: 'shared-branch', number: 18 });
  const [firstId, secondId] = fixture.threadIds;
  await harness.rpc('UpdateThreadWorkspace', secondId, fixture.worktreePath);
  expect(await samePath((await threadRow(harness, secondId)).worktreePath, fixture.worktreePath), 'the second thread did not join the worktree').toBe(true);
  await lookUpPR(harness, fixture);
  await harness.open(page);
  await expectTracked(harness, firstId, 18);
  await expectTracked(harness, secondId, 18);

  await mergePR(harness, fixture);
  for (const id of [firstId, secondId]) {
    await expect.poll(async () => samePath((await threadRow(harness, id)).workspacePath, fixture.root), { timeout: 20_000 }).toBe(true);
    await expectHandled(harness, id, 'removed');
  }
  expect(existsSync(fixture.worktreePath)).toBe(false);
  for (const title of ['First sharer', 'Second sharer']) {
    await openThread(page, title);
    await expect(page.getByTestId('env-picker-trigger')).toHaveText('Base');
    await expect(notices(page, 'merged.')).toHaveText('PR #18 merged. Removed worktree shared-branch; this thread now runs in Base.');
  }
  expect(await stateReads(harness)).toBeGreaterThan(0);
  await expectEveryForgeCallHandled(harness);
  await expect(errorToasts(page)).toHaveCount(0);
});

// The page is open but never shows these threads, and nothing reads their
// git status: the create's own lookup is what tracks the new PR.
test('a PR the app\'s create action opens is tracked from that create', async ({ harness, page }) => {
  const fixture = await seedMergeFixture(harness, { forge: 'github', name: 'merge-app-create', titles: ['App create'], branch: 'app-create-branch', number: 1 });
  const [threadId] = fixture.threadIds;
  await seedForge(harness, [{ ...fixture.repo, pulls: [] }]);
  await harness.open(page);
  const result = await harness.rpc<{ prUrl?: string; error?: string }>(
    'GitCreatePR', { projectId: fixture.projectId, workspacePath: fixture.worktreePath }, 'Created here', '', false,
  );
  expect(result.error ?? '').toBe('');
  const number = Number(result.prUrl?.split('/').pop());
  expect(number).toBeGreaterThan(0);
  await expectTracked(harness, threadId, number);
  await expectEveryForgeCallHandled(harness);
});

test('a MR an agent\'s glab mr create opens is tracked when the command settles', async ({ harness, page }) => {
  const fixture = await seedMergeFixture(harness, { forge: 'gitlab', name: 'merge-agent-create', titles: ['Agent create'], branch: 'agent-create-branch', number: 4 });
  const [threadId] = fixture.threadIds;
  await seedForge(harness, [{ ...fixture.repo, pulls: [] }]);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('merge-agent-create', [
      { emit: { lines: [toolUseLine('msg-1', 'toolu_create', 'Bash', { command: 'glab mr create --title "Agent work" --yes' })] } },
      { waitSignal: { name: 'creating' } },
      { emit: { lines: [toolResultLine('toolu_create', 'created'), ...textLines('msg-2', 'Opened it.'), RESULT_LINE] } },
    ]),
  });
  await harness.open(page);
  const mockId = await startMock(harness, threadId);
  const creating = waitForGate(harness, 'creating');
  const completed = harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);
  await harness.rpc('SendMessage', threadId, 'open an MR', []);
  await creating;
  // The MR exists on the forge from here on, as the command creates it.
  await movePR(harness, fixture, {});
  await advance(harness, mockId, 'creating');
  await completed;
  await expectTracked(harness, threadId, 4);
  await expectEveryForgeCallHandled(harness);
});
