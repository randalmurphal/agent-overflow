// The fake forge from a spec: seed the pull or merge requests `gh` and
// `glab` answer from, publish a seeded workspace's branch as one, read back
// what the app asked the CLIs, and reach a PR's review pane the way a user
// does. The fixture and invocation shapes are internal/harness/forgefake's
// (fixture.go, engine.go); its AGENTS.md lists which invocations have
// handlers.

import { execFileSync } from 'node:child_process';
import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import type { Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect } from './fixtures.js';

export interface ForgeComment {
  id?: number;
  author?: string;
  body: string;
  createdAt?: string;
}

export interface ForgePull {
  number: number;
  title: string;
  body?: string;
  state?: 'open' | 'closed' | 'merged';
  headRef?: string;
  baseRef?: string;
  headSha?: string;
  baseSha?: string;
  diff?: string;
  comments?: ForgeComment[];
}

/** A GitHub attachment by `url`, or a GitLab upload by `secret` and `filename`. */
export interface ForgeAttachment {
  url?: string;
  secret?: string;
  filename?: string;
  contentType?: string;
  base64?: string;
  text?: string;
}

export interface ForgeRepo {
  id?: number;
  forge: 'github' | 'gitlab';
  project: string;
  host?: string;
  pulls?: ForgePull[];
  attachments?: ForgeAttachment[];
}

export interface ForgeInvocation {
  seq: number;
  cli: 'gh' | 'glab' | 'ssh';
  args: string[];
  cwd: string;
  stdin?: string;
  route?: string;
  unhandled?: boolean;
  exitCode: number;
  stderr?: string;
}

export async function seedForge(harness: HarnessApp, repos: ForgeRepo[]): Promise<void> {
  await harness.rpc('HarnessForgeSeed', { repos });
}

export async function forgeInvocations(harness: HarnessApp): Promise<ForgeInvocation[]> {
  const log = await harness.rpc<{ invocations: ForgeInvocation[] }>('HarnessForgeInvocations', 0);
  return log.invocations;
}

/** Fail on any invocation the fake has no handler for, naming its argv. */
export async function expectEveryForgeCallHandled(harness: HarnessApp): Promise<void> {
  const unhandled = (await forgeInvocations(harness)).filter((call) => call.unhandled);
  expect(
    unhandled.map((call) => call.stderr),
    'the app made forge CLI calls the fake does not implement',
  ).toEqual([]);
}

const FORGE_HOST = { github: 'github.com', gitlab: 'gitlab.com' } as const;

// Where each forge publishes a pull request's head.
const PULL_HEAD_REF = {
  github: (number: number) => `refs/pull/${number}/head`,
  gitlab: (number: number) => `refs/merge-requests/${number}/head`,
} as const;

// git runs this in place of a git:// connection (`core.gitProxy`, argv
// `host port`). It reads the daemon request pkt-line, whose first field is
// `git-upload-pack <path>` and whose extra fields may ask for protocol v2,
// and answers it from the bare repository at that path under the root.
function gitProxyScript(root: string): string {
  return `#!/bin/sh
len=$(dd bs=1 count=4 2>/dev/null)
request=$(dd bs=1 count=$((0x$len - 4)) 2>/dev/null | tr '\\0' '\\n')
repo=$(printf '%s\\n' "$request" | sed -n '1s/^git-upload-pack //p')
case "$request" in *version=2*) GIT_PROTOCOL=version=2; export GIT_PROTOCOL ;; esac
exec git upload-pack --strict '${root}'"$repo"
`;
}

/**
 * Publish the seeded `workspace` as `repo`'s first pull request and seed
 * the fake forge with `repo`. The workspace gets a `feature` branch one
 * commit ahead of `main` (the fixture's default head and base refs) that
 * writes `files`, and an origin that publishes it the way the forge does. The origin URL is
 * `git://<forge host>/<project>.git`, so forge detection sees the forge's
 * own host, and the workspace's `core.gitProxy` answers every connection
 * from a bare repository under the harness data root: git opens no socket.
 * The forge is seeded before the origin is added because the app looks
 * the branch's PR up as soon as the workspace has a forge origin, and
 * caches a miss. Git runs with the harness home, never the developer's.
 */
export async function publishPullRequest(
  harness: HarnessApp,
  workspace: string,
  repo: ForgeRepo,
  files: Record<string, string> = { 'feature.md': 'Feature work.\n' },
): Promise<void> {
  const [pull, ...rest] = repo.pulls ?? [];
  if (!pull) throw new Error(`publishPullRequest: ${repo.project} has no pull request to publish`);
  const env = { ...process.env, HOME: path.join(harness.bootstrap.dataRoot, 'home'), GIT_CONFIG_NOSYSTEM: '1' };
  const git = (cwd: string, ...args: string[]) => execFileSync('git', args, { cwd, env, encoding: 'utf8' }).trim();
  const root = path.join(harness.bootstrap.dataRoot, 'forge-origin');
  const proxy = path.join(root, 'git-proxy.sh');
  const bare = path.join(root, `${repo.project}.git`);
  await mkdir(bare, { recursive: true });
  await writeFile(proxy, gitProxyScript(root), { mode: 0o755 });
  git(bare, 'init', '--bare', '--quiet');

  const baseSha = git(workspace, 'rev-parse', 'HEAD');
  git(workspace, 'checkout', '--quiet', '-b', 'feature');
  for (const [name, content] of Object.entries(files)) {
    await mkdir(path.dirname(path.join(workspace, name)), { recursive: true });
    await writeFile(path.join(workspace, name), content);
  }
  git(workspace, 'add', '--', ...Object.keys(files));
  git(workspace, 'commit', '--quiet', '-m', 'feature work');
  const headSha = git(workspace, 'rev-parse', 'HEAD');
  git(workspace, 'push', '--quiet', bare, 'main', 'feature');
  git(bare, 'update-ref', PULL_HEAD_REF[repo.forge](pull.number), headSha);

  await seedForge(harness, [{ ...repo, pulls: [{ ...pull, headRef: 'feature', baseRef: 'main', headSha, baseSha }, ...rest] }]);
  git(workspace, 'config', 'core.gitProxy', proxy);
  git(workspace, 'remote', 'add', 'origin', `git://${FORGE_HOST[repo.forge]}/${repo.project}.git`);
}

/**
 * Publish the checked-out `branch` of the seeded `workspace` to `repo`'s
 * origin with no pull request, and seed the fake forge with `repo`. The
 * origin is the forge's own URL answered from a bare repository under the
 * harness data root (see `publishPullRequest`), `main` and `branch` are
 * pushed to it, and `branch` tracks `origin/<branch>` from a fetch, so git
 * status reports an upstream on a recognised forge.
 */
export async function publishBranch(
  harness: HarnessApp,
  workspace: string,
  repo: ForgeRepo,
  branch: string,
): Promise<void> {
  const env = { ...process.env, HOME: path.join(harness.bootstrap.dataRoot, 'home'), GIT_CONFIG_NOSYSTEM: '1' };
  const git = (cwd: string, ...args: string[]) => execFileSync('git', args, { cwd, env, encoding: 'utf8' }).trim();
  const root = path.join(harness.bootstrap.dataRoot, 'forge-origin');
  const proxy = path.join(root, 'git-proxy.sh');
  const bare = path.join(root, `${repo.project}.git`);
  await mkdir(bare, { recursive: true });
  await writeFile(proxy, gitProxyScript(root), { mode: 0o755 });
  git(bare, 'init', '--bare', '--quiet');
  git(workspace, 'push', '--quiet', bare, 'main', branch);

  await seedForge(harness, [repo]);
  git(workspace, 'config', 'core.gitProxy', proxy);
  git(workspace, 'remote', 'add', 'origin', `git://${FORGE_HOST[repo.forge]}/${repo.project}.git`);
  git(workspace, 'fetch', '--quiet', 'origin');
  git(workspace, 'branch', '--quiet', `--set-upstream-to=origin/${branch}`, branch);
}

/**
 * Open the thread titled `title` and its review pane on the PR scope
 * through the chat header's PR badge, which appears once git status has
 * looked the branch's PR up. Answers the review pane section.
 */
export async function openPullRequestReview(page: Page, title: string) {
  await page.getByTestId('thread-row').filter({ hasText: title }).click();
  const badge = page.getByTestId('chat-header-pr-badge');
  await expect(badge).toBeVisible({ timeout: 30_000 });
  await badge.click();
  const review = page.locator('section[data-pane-kind="review"]');
  await expect(review.getByTestId('review-pr-header')).toBeVisible();
  return review;
}

/** Expand one collapsed review header section (`review-pr-description`, ...). */
export async function expandReviewSection(page: Page, testId: string) {
  const section = page.getByTestId(testId);
  const toggle = section.locator('button[aria-expanded]').first();
  if ((await toggle.getAttribute('aria-expanded')) !== 'true') await toggle.click();
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  return section;
}
