// Shared setup for the worktree-removal specs: a seeded project whose
// threads are bound to linked worktrees cut by the app itself, reads of the
// rows and worktrees the backend holds, and git run as the harness's own
// user (never the developer's configuration).

import { execFileSync } from 'node:child_process';
import { realpath } from 'node:fs/promises';
import path from 'node:path';
import type { Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, type SeedResult } from './fixtures.js';

export interface ThreadRow {
  id: string;
  title: string;
  projectId: string;
  workspacePath: string;
  worktreePath?: string;
  branch?: string;
}

export interface WorktreeListItem {
  path: string;
  branch: string;
  missing?: boolean;
}

export interface WorktreeProject {
  projectId: string;
  root: string;
  /** Thread ids in the order the titles were given. */
  threadIds: string[];
}

/**
 * A project with one seeded thread per title, each with a completed turn so
 * the sidebar lists it, and one extra unchecked-out branch per name.
 */
export async function seedWorktreeProject(
  harness: HarnessApp,
  name: string,
  titles: string[],
  branches: string[],
): Promise<WorktreeProject> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name,
        repo: {
          commits: [{ message: 'init', files: { 'README.md': '# seed\n', 'src/app.txt': 'one\ntwo\nthree\n' } }],
          branches,
        },
        threads: titles.map((title) => ({
          title,
          provider: 'claude',
          turns: [{ userText: `open ${title}`, items: [{ kind: 'assistant_text', summary: 'Ready.' }] }],
        })),
      },
    ],
  });
  const project = seed.projects[0];
  return { projectId: project.projectId, root: project.path, threadIds: project.threadIds };
}

/** Cut a worktree on `branch` through the app and bind `threadId` to it. */
export async function attachWorktree(harness: HarnessApp, threadId: string, branch: string): Promise<ThreadRow> {
  const row = await harness.rpc<ThreadRow>('AttachThreadWorktree', threadId, branch);
  expect(row.worktreePath, `AttachThreadWorktree bound no worktree for ${branch}`).toBeTruthy();
  return row;
}

export const threadRows = (harness: HarnessApp) => harness.rpc<ThreadRow[]>('HarnessListThreadRows');

export async function threadRow(harness: HarnessApp, threadId: string): Promise<ThreadRow> {
  const row = (await threadRows(harness)).find((candidate) => candidate.id === threadId);
  if (!row) throw new Error(`thread ${threadId} has no row`);
  return row;
}

export async function listWorktrees(harness: HarnessApp, projectId: string, root: string): Promise<WorktreeListItem[]> {
  return (await harness.rpc<WorktreeListItem[] | null>('GitListWorktrees', { projectId, workspacePath: root })) ?? [];
}

export async function samePath(a: string | undefined, b: string): Promise<boolean> {
  if (!a) return false;
  const canonical = async (p: string) => {
    try {
      return await realpath(p);
    } catch {
      return path.resolve(p);
    }
  };
  return (await canonical(a)) === (await canonical(b));
}

export const basename = (p: string) => p.split('/').filter(Boolean).pop() ?? p;

/** Runs git with the harness home, so no developer configuration applies. */
export function harnessGit(harness: HarnessApp, cwd: string, ...args: string[]): string {
  const env = { ...process.env, HOME: path.join(harness.bootstrap.dataRoot, 'home'), GIT_CONFIG_NOSYSTEM: '1' };
  // stderr is piped so an expected failure carries it on the error, not the log.
  return execFileSync('git', args, { cwd, env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
}

/** Open the thread titled `title` in the page's focused pane. */
export async function openThread(page: Page, title: string): Promise<void> {
  await page.getByTestId('thread-row').filter({ hasText: title }).click();
  await expect(page.getByTestId('composer-workspace-strip')).toBeVisible();
}

/** Error toasts only: the class is the toast's one type discriminator. */
export const errorToasts = (page: Page) => page.locator('[role="alert"].text-error');
