// Shared setup for the merged pull request worktree cleanup specs
// (internal/app/app_worktree_merge_cleanup.go): a project whose first
// thread runs in a linked worktree on a published branch with an open pull
// or merge request, the row the backend tracks it in, and the forge state a
// merge or close moves to. Git runs as the harness's own user.

import { DatabaseSync } from 'node:sqlite';
import path from 'node:path';
import type { Page } from '@playwright/test';

import { HARNESS_TIMING, type HarnessApp } from '../src/harness.js';
import { expect } from './fixtures.js';
import { forgeInvocations, publishBranch, seedForge, type ForgePull, type ForgeRepo } from './forge-helpers.js';
import { attachWorktree, seedWorktreeProject } from './worktree-removal-helpers.js';

/** The harness timing with the cleanup's per-repository poll at `poll`. */
export const mergePollTiming = (poll: string) => `${HARNESS_TIMING},merged-pr-poll=${poll}`;

export type Forge = 'github' | 'gitlab';

export interface MergeFixture {
  forge: Forge;
  projectId: string;
  root: string;
  /** In title order; the first runs in the worktree. */
  threadIds: string[];
  worktreePath: string;
  branch: string;
  number: number;
  repo: ForgeRepo;
}

/** "PR #90" or "MR !12", as the notices name it. */
export const prLabel = (forge: Forge, number: number) => (forge === 'gitlab' ? `MR !${number}` : `PR #${number}`);

/**
 * A project with one thread per title, the first bound to a worktree the app
 * cut on `branch`, and `branch` published to the forge's origin with one open
 * pull request numbered `number`.
 */
export async function seedMergeFixture(
  harness: HarnessApp,
  opts: { forge: Forge; name: string; titles: string[]; branch: string; number: number },
): Promise<MergeFixture> {
  const project = await seedWorktreeProject(harness, opts.name, opts.titles, [opts.branch]);
  const bound = await attachWorktree(harness, project.threadIds[0], opts.branch);
  const repo: ForgeRepo = {
    forge: opts.forge,
    project: opts.forge === 'github' ? `acme/${opts.name}` : `grp/${opts.name}`,
    pulls: [{ number: opts.number, title: `Merge ${opts.name}`, headRef: opts.branch, baseRef: 'main' }],
  };
  await publishBranch(harness, bound.worktreePath!, repo, opts.branch);
  return {
    forge: opts.forge,
    projectId: project.projectId,
    root: project.root,
    threadIds: project.threadIds,
    worktreePath: bound.worktreePath!,
    branch: opts.branch,
    number: opts.number,
    repo,
  };
}

/**
 * Read the worktree's git status the way a pane does after a git action.
 * Its open-PR lookup asks the forge, and that answer is what the backend
 * tracks the PR from, or learns it went away from.
 */
export async function lookUpPR(harness: HarnessApp, fixture: MergeFixture): Promise<void> {
  await harness.rpc('GetGitStatus', { projectId: fixture.projectId, workspacePath: fixture.worktreePath });
}

export interface TrackedRow {
  thread_id: string;
  number: number;
  branch: string;
  worktree_path: string;
  merged_at: number;
  handled_at: number;
  outcome: string;
}

/** The backend's tracked pull request rows, read from its database. */
export function trackedRows(harness: HarnessApp): TrackedRow[] {
  const database = new DatabaseSync(path.join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    return database
      .prepare('SELECT thread_id, number, branch, worktree_path, merged_at, handled_at, outcome FROM thread_pull_requests ORDER BY thread_id')
      .all() as unknown as TrackedRow[];
  } finally {
    database.close();
  }
}

export function trackedRow(harness: HarnessApp, threadId: string): TrackedRow | undefined {
  return trackedRows(harness).find((row) => row.thread_id === threadId);
}

/** Wait until the backend tracks `number` for `threadId`, not yet handled. */
export async function expectTracked(harness: HarnessApp, threadId: string, number: number): Promise<void> {
  await expect
    .poll(() => {
      const row = trackedRow(harness, threadId);
      return row ? `${row.number}:${row.handled_at}` : 'none';
    }, { message: `thread ${threadId} never tracked PR ${number}` })
    .toBe(`${number}:0`);
}

/** Wait until the tracked row of `threadId` is handled, and answer it. */
export async function expectHandled(harness: HarnessApp, threadId: string, outcome: string): Promise<TrackedRow> {
  await expect
    .poll(() => trackedRow(harness, threadId)?.outcome ?? 'none', { message: `thread ${threadId} never handled its PR`, timeout: 20_000 })
    .toBe(outcome);
  return trackedRow(harness, threadId)!;
}

/** Reseed the fixture's repository with its pull request moved to `pull`, and `extra` pulls beside it. */
export async function movePR(harness: HarnessApp, fixture: MergeFixture, pull: Partial<ForgePull>, extra: ForgePull[] = []): Promise<void> {
  const [first] = fixture.repo.pulls!;
  await seedForge(harness, [{ ...fixture.repo, pulls: [{ ...first, ...pull }, ...extra] }]);
}

/** Merge the fixture's pull request on the forge at `at`. */
export const mergePR = (harness: HarnessApp, fixture: MergeFixture, at = new Date()) =>
  movePR(harness, fixture, { state: 'merged', mergedAt: at.toISOString() });

/** The batched state reads the backend made of the fake forge. */
export async function stateReads(harness: HarnessApp): Promise<number> {
  return (await forgeInvocations(harness)).filter(
    (call) => call.via === 'http' && (call.operation === 'PRStates' || (call.route === 'glab api merge request list' && call.path.includes('iids'))),
  ).length;
}

/**
 * Prove no read is made for a while: the cleanup polls every `pollMs`, so
 * four polls' time with no new read means it stopped asking. An absence has
 * no event to wait for.
 */
export async function expectNoMoreStateReads(harness: HarnessApp, pollMs: number): Promise<void> {
  const before = await stateReads(harness);
  await new Promise((resolve) => setTimeout(resolve, pollMs * 4 + 500));
  expect(await stateReads(harness), 'the cleanup kept reading a pull request it had settled').toBe(before);
}

/** The timeline notices of the open thread whose text mentions `text`. */
export const notices = (page: Page, text: string) => page.getByTestId('notification-row').filter({ hasText: text });
