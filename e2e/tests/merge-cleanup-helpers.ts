// Shared setup for the merged pull request worktree cleanup specs
// (internal/app/app_worktree_merge_cleanup.go): a project whose first
// thread runs in a linked worktree on a published branch with an open pull
// or merge request, the row the backend tracks it in, and the forge state a
// merge or close moves to. Git runs as the harness's own user.

import { randomUUID } from 'node:crypto';
import { DatabaseSync } from 'node:sqlite';
import path from 'node:path';
import type { Page } from '@playwright/test';

import { harnessTiming, type HarnessApp } from '../src/harness.js';
import { expect } from './fixtures.js';
import { forgeInvocations, publishBranch, seedForge, type ForgeInvocation, type ForgePull, type ForgeRepo } from './forge-helpers.js';
import { attachWorktree, seedWorktreeProject } from './worktree-removal-helpers.js';

/** The harness timing with the cleanup's per-repository poll at `poll`. */
export const mergePollTiming = (poll: string) => harnessTiming({ 'merged-pr-poll': poll });

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
  opts: { forge: Forge; name: string; titles: string[]; branch: string; number: number; nodeId?: string },
): Promise<MergeFixture> {
  const project = await seedWorktreeProject(harness, opts.name, opts.titles, [opts.branch]);
  const bound = await attachWorktree(harness, project.threadIds[0], opts.branch);
  const repo: ForgeRepo = {
    forge: opts.forge,
    project: opts.forge === 'github' ? `acme/${opts.name}` : `grp/${opts.name}`,
    pulls: [{ number: opts.number, title: `Merge ${opts.name}`, headRef: opts.branch, baseRef: 'main', ...(opts.nodeId ? { nodeId: opts.nodeId } : {}) }],
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
  removal_failed_at: number;
  outcome: string;
}

/** The backend's tracked pull request rows, read from its database. */
export function trackedRows(harness: HarnessApp): TrackedRow[] {
  const database = new DatabaseSync(path.join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    return database
      .prepare('SELECT thread_id, number, branch, worktree_path, merged_at, handled_at, removal_failed_at, outcome FROM thread_pull_requests ORDER BY thread_id')
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

/** A GitHub open PR every cleanup poll reads, in a repository of its own. */
export interface ReadControl {
  nodeId: string;
  threadId: string;
}

const isStateRead = (call: ForgeInvocation) =>
  call.via === 'http' && (call.operation === 'PRStates' || (call.route === 'glab api merge request list' && call.path.includes('iids')));

const readsControl = (call: ForgeInvocation, control: ReadControl) =>
  call.via === 'http' && call.operation === 'PRStates' && ((call.variables?.ids as string[] | undefined) ?? []).includes(control.nodeId);

/**
 * Seeds the control: an open PR, looked up, that the cleanup tracks and then
 * reads in one request of its own on every poll, one per repository, so
 * its reads mark the polls that ran. `expectNoMoreStateReads` and
 * `stateReads` leave its reads out.
 */
export async function seedReadControl(harness: HarnessApp): Promise<ReadControl> {
  const tag = randomUUID().slice(0, 8);
  const nodeId = `PR_ao_read_control_${tag}`;
  const control = await seedMergeFixture(harness, {
    forge: 'github', name: `merge-read-control-${tag}`, titles: ['Read control'], branch: 'read-control', number: 900, nodeId,
  });
  await lookUpPR(harness, control);
  return { nodeId, threadId: control.threadIds[0] };
}

/** The state reads the backend made of the fake forge, the control's left out. */
export async function stateReads(harness: HarnessApp, control?: ReadControl): Promise<number> {
  return (await forgeInvocations(harness)).filter((call) => isStateRead(call) && !(control && readsControl(call, control))).length;
}

/**
 * Prove the cleanup no longer reads anything but the control: two polls
 * read the control, each a full interval after the last, and nothing else
 * was read meanwhile. A repository still tracked would have come due in
 * the same polls.
 */
export async function expectNoMoreStateReads(harness: HarnessApp, control: ReadControl): Promise<void> {
  // Tracked once the cleanup handles observations, which it does from the
  // first client's catalog reads on (startMergedPRCleanup).
  await expectTracked(harness, control.threadId, 900);
  const before = await stateReads(harness, control);
  const controlReads = async () => (await forgeInvocations(harness)).filter((call) => readsControl(call, control)).length;
  const polled = await controlReads();
  await expect.poll(controlReads, { timeout: 15_000, message: 'the cleanup stopped polling' }).toBeGreaterThanOrEqual(polled + 2);
  expect(await stateReads(harness, control), 'the cleanup kept reading a pull request it had settled').toBe(before);
}

/** The timeline notices of the open thread whose text mentions `text`. */
export const notices = (page: Page, text: string) => page.getByTestId('notification-row').filter({ hasText: text });
