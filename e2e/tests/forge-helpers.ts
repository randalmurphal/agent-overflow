// The fake forge from a spec: seed the pull or merge requests the fake
// GitHub and GitLab answer from, publish a seeded workspace's branch as
// one, read back what the app asked the forges, and reach a PR's review
// pane the way a user does. The fixture and invocation shapes are internal/harness/forgefake's
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
  authorName?: string;
  body: string;
  createdAt?: string;
}

/** A review thread anchored to the diff; `side` defaults to right. */
export interface ForgeThread {
  id?: string;
  path: string;
  line?: number;
  startLine?: number;
  side?: 'right' | 'left' | 'file';
  resolved?: boolean;
  outdated?: boolean;
  comments: ForgeComment[];
}

/** A submitted verdict; on GitLab only APPROVED has a counterpart (an approval). */
export interface ForgeReview {
  /** GitHub databaseId; generated when omitted. */
  id?: number;
  author: string;
  authorName?: string;
  state: 'APPROVED' | 'CHANGES_REQUESTED' | 'COMMENTED';
  body?: string;
  submittedAt?: string;
  commitSha?: string;
}

export interface ForgePull {
  number: number;
  title: string;
  body?: string;
  state?: 'open' | 'closed' | 'merged';
  /** RFC 3339; a merged pull only. Defaults to the time it was seeded. */
  mergedAt?: string;
  /** GitHub's GraphQL node id; defaults to one derived from the repository and number. */
  nodeId?: string;
  draft?: boolean;
  author?: string;
  authorName?: string;
  headRef?: string;
  /** The fork the head branch lives in ("owner/repo", a GitLab path); unset is the repository itself. */
  headRepo?: string;
  /** Logins a review is requested from (GitHub reviewRequests, GitLab reviewers). */
  reviewers?: string[];
  /** RFC 3339; default 2026-01-01T00:00:00Z, updatedAt and committedAt (the head commit's time) to createdAt. */
  createdAt?: string;
  updatedAt?: string;
  committedAt?: string;
  /** GitHub's commit count; defaults to 1. */
  commits?: number;
  baseRef?: string;
  headSha?: string;
  /** GitLab's merge base. */
  baseSha?: string;
  /** The base branch's tip: GitHub baseRefOid, GitLab start_sha. */
  startSha?: string;
  /** GitLab only: the merge request's diff is not computed yet, so
   * diff_refs reads null. */
  diffPending?: boolean;
  diff?: string;
  /** PR-level conversation comments. */
  comments?: ForgeComment[];
  threads?: ForgeThread[];
  reviews?: ForgeReview[];
  /** The head pipeline (GitLab) or workflow run (GitHub Actions). */
  ci?: ForgePipeline;
  /** What the base and project require beyond reviews, CI, threads and
   * conflicts; omitted, nothing (an open pull is ready). */
  readiness?: ForgeReadiness;
  /** A merged pull's merger (defaults to its author) and merge commit. */
  mergedBy?: string;
  mergeCommitSha?: string;
  /** A closed or merged pull's close, RFC 3339, and who closed it. */
  closedAt?: string;
  closedBy?: string;
  /** GitHub timeline items or GitLab system notes besides the reviews. */
  events?: ForgeEvent[];
}

/** A pull's merge box (forgefake Readiness). */
export interface ForgeReadiness {
  /** GitHub's base protection approvals, GitLab's approvals_required. */
  requiredApprovals?: number;
  /** GitHub only: CI job names the base requires to pass. */
  requiredChecks?: string[];
  /** GitHub only: the base requires conversation resolution. */
  resolveThreads?: boolean;
  /** The head is behind a base that requires it up to date (GitLab: a ff or rebase_merge project). */
  behind?: boolean;
  /** GitLab only: user_can_approve, overriding the role's. */
  viewerCanApprove?: boolean;
  autoMerge?: { method?: 'merge' | 'squash' | 'rebase'; by?: string; byName?: string; at?: string };
}

/** One timeline item (GitHub) or system note (GitLab); kinds per forge in forgefake Event. */
export interface ForgeEvent {
  id?: number;
  kind: string;
  actor?: string;
  actorName?: string;
  at?: string;
  ref?: string;
  method?: 'merge' | 'squash' | 'rebase';
  text?: string;
}

/** A project's merge settings and the viewer's role (forgefake RepoMerge). */
export interface ForgeRepoMerge {
  /** GitHub READ..ADMIN (default WRITE); GitLab guest..owner (default developer). */
  permission?: string;
  /** GitHub: allowed methods (all by default), the viewer's default, "Allow auto-merge". */
  methods?: ('merge' | 'squash' | 'rebase')[];
  defaultMethod?: 'merge' | 'squash' | 'rebase';
  autoMergeAllowed?: boolean;
  deleteBranchOnMerge?: boolean;
  /** GitLab: merge_method, squash_option, and the merge checks the project requires. */
  mergeMethod?: 'merge' | 'rebase_merge' | 'ff';
  squashOption?: 'default_off' | 'default_on' | 'always' | 'never';
  pipelineMustSucceed?: boolean;
  discussionsMustResolve?: boolean;
}

export interface ForgePipeline {
  /** GitLab pipeline id or GitHub run id; generated when omitted. Give one to re-seed the same pipeline. */
  id?: number;
  /** GitHub workflow name. Defaults to "CI". */
  name?: string;
  status?: ForgeJobStatus;
  /** What it ran on: a pull's defaults to a merge request pipeline of its
   * head, a repository's to a branch pipeline of main (forgefake Pipeline). */
  refKind?: 'branch' | 'tag' | 'merge_request';
  ref?: string;
  sha?: string;
  /** GitLab source or GitHub event; defaults by refKind. */
  source?: string;
  /** GitHub run display title; defaults to name. */
  title?: string;
  createdAt?: string;
  updatedAt?: string;
  jobs: ForgeJob[];
}

export type ForgeJobStatus = 'success' | 'failed' | 'running' | 'pending' | 'skipped' | 'canceled';

export interface ForgeJob {
  /** Generated when omitted. Give one to re-seed the same job. */
  id?: number;
  name: string;
  /** GitLab stage; defaults to "test". */
  stage?: string;
  status: ForgeJobStatus;
  allowFailure?: boolean;
  startedAt?: string;
  completedAt?: string;
  /** The job log, served once the job started, running included. */
  log?: string;
  /** The forge answers the log with a 404, as GitHub does until a job's log blob exists, running or completed. */
  logWithheld?: boolean;
  /** A log generated on each request instead of `log`, for a log of many
   * megabytes (forgefake SyntheticLog). */
  synthetic?: ForgeSyntheticLog;
  /** GitHub Actions steps. */
  steps?: ForgeStep[];
}

export interface ForgeSyntheticLog {
  /** Each a GitHub step (matched to the job's started steps in order) or a
   * GitLab section of `lines` body lines. */
  sections: { name: string; lines: number }[];
  /** Bytes per body line with its newline; defaults to 80. */
  lineBytes?: number;
  /** A running GitLab trace serves this many lines on its first request
   * and `growLines` more on each later one. */
  runningLines?: number;
  growLines?: number;
}

export interface ForgeStep {
  /** GitHub's step number; defaults to one past the previous step's. */
  number?: number;
  name: string;
  status: ForgeJobStatus;
  /** RFC 3339 times, answered as null when omitted. */
  startedAt?: string;
  completedAt?: string;
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
  merge?: ForgeRepoMerge;
  /** Pipelines on any ref beside the pulls' CI. */
  pipelines?: ForgePipeline[];
  deletedBranches?: string[];
}

interface ForgeInvocationCommon {
  seq: number;
  route?: string;
  unhandled?: boolean;
  /** When the fake answered, RFC 3339. */
  at: string;
}

/** A gh, glab or ssh invocation ao-mockforge forwarded. */
export interface ForgeCLIInvocation extends ForgeInvocationCommon {
  via: 'cli';
  cli: 'gh' | 'glab' | 'ssh';
  args: string[];
  cwd: string;
  stdin?: string;
  exitCode: number;
  stderr?: string;
}

/**
 * A request to the fake's HTTP mounts. `host` is the forge host the
 * request addressed (its Host header, port included) or an attachment's
 * host. `path` is relative to the forge base (`repos/o/r/pulls/1`,
 * `graphql`), or an attachment's absolute URL without its query. A
 * GraphQL request also carries its `operation` and `variables`; its
 * route is `gh graphql <operation>`.
 */
export interface ForgeHTTPInvocation extends ForgeInvocationCommon {
  via: 'http';
  forge: 'github' | 'gitlab';
  host: string;
  method: string;
  path: string;
  operation?: string;
  variables?: Record<string, unknown>;
  status: number;
  detail?: string;
}

export type ForgeInvocation = ForgeCLIInvocation | ForgeHTTPInvocation;

export async function seedForge(harness: HarnessApp, repos: ForgeRepo[]): Promise<void> {
  await harness.rpc('HarnessForgeSeed', { repos });
}

/** Take the fake forge off the network, or put it back. */
export async function setForgeOffline(harness: HarnessApp, offline: boolean): Promise<void> {
  await harness.rpc('HarnessForgeOffline', offline);
}

/**
 * Put one quota pool of the fake forge under a rate limit until `reset`
 * (unix seconds): with `remaining` 0 the pool refuses every request as the
 * forge does (route `rate limited`); otherwise it answers and reports
 * `remaining` of 5000. GitHub's pools are `core` (REST) and `graphql`,
 * GitLab's `throttle_authenticated_api`. Lifts at `reset`; HarnessReset
 * clears it.
 */
export async function setForgeRateLimit(
  harness: HarnessApp,
  limit: { forge: 'github' | 'gitlab'; pool: string; remaining: number; reset: number },
): Promise<void> {
  await harness.rpc('HarnessForgeRateLimit', limit);
}

/**
 * Make one fake forge route answer `status`, `header` and the JSON `body`
 * in place of its handler, `times` requests or until cleared (0): 401,
 * 403, 405, 409 or 422, or 200 with an `errors` body on a GraphQL
 * operation. `route` is a REST route's name (`glab api approve`) or
 * `gh graphql <operation>`. Nothing in the fixture changes; the request is
 * recorded under its route. HarnessReset clears it.
 */
export async function refuseForgeRoute(
  harness: HarnessApp,
  refusal: { route: string; status: number; header?: Record<string, string>; body?: unknown; times?: number },
): Promise<void> {
  await harness.rpc('HarnessForgeRefuse', refusal);
}

/** Clear a route's refusal. */
export async function clearForgeRefusal(harness: HarnessApp, route: string): Promise<void> {
  await harness.rpc('HarnessForgeRefuse', { route, status: 0 });
}

export async function forgeInvocations(harness: HarnessApp): Promise<ForgeInvocation[]> {
  const log = await harness.rpc<{ invocations: ForgeInvocation[] }>('HarnessForgeInvocations', 0);
  return log.invocations;
}

/** Fail on any invocation the fake has no handler for, naming the call. */
export async function expectEveryForgeCallHandled(harness: HarnessApp): Promise<void> {
  const unhandled = (await forgeInvocations(harness)).filter((call) => call.unhandled);
  expect(
    unhandled.map((call) => (call.via === 'cli' ? call.stderr : call.detail)),
    'the app made forge calls the fake does not implement',
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
 * The forge is seeded and the remote-tracking refs written before the
 * origin is added because the app looks the branch's PR up as soon as the
 * workspace has a forge origin, and caches a miss. Git runs with the harness home, never the developer's.
 */
export async function publishPullRequest(
  harness: HarnessApp,
  workspace: string,
  repo: ForgeRepo,
  files: Record<string, string> = { 'feature.md': 'Feature work.\n' },
): Promise<ForgeRepo> {
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
  git(workspace, 'commit', '--quiet', '-m', 'feature work', '-m', 'Adds the mailer and its tests.');
  const headSha = git(workspace, 'rev-parse', 'HEAD');
  git(workspace, 'push', '--quiet', bare, 'main', 'feature');
  git(bare, 'update-ref', PULL_HEAD_REF[repo.forge](pull.number), headSha);

  // The remote-tracking refs a push to origin leaves: the app asks the
  // forge for a branch's PR only once origin has the branch. Written
  // before the origin exists, for the same reason the forge is seeded
  // first.
  git(workspace, 'update-ref', 'refs/remotes/origin/main', baseSha);
  git(workspace, 'update-ref', 'refs/remotes/origin/feature', headSha);

  const seeded: ForgeRepo = { ...repo, pulls: [{ ...pull, headRef: 'feature', baseRef: 'main', headSha, baseSha, startSha: baseSha }, ...rest] };
  await seedForge(harness, [seeded]);
  git(workspace, 'config', 'core.gitProxy', proxy);
  git(workspace, 'remote', 'add', 'origin', `git://${FORGE_HOST[repo.forge]}/${repo.project}.git`);
  // Returned so a spec can re-seed the same repository with the forge
  // state moved (a CI job finishing, its log growing): seedForge replaces
  // the repository under the same forge and project.
  return seeded;
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

// GetPRCILogLines, as frontend/src/lib/transport/methodRoutes.ts lists it.
const GET_CI_LOG_LINES_METHOD_ID = 1_435_724_188;

/** Counts the page's CI log line reads (GetPRCILogLines) and holds them
 * while `hold()` is in effect. Call before the page opens. */
export async function interceptCILogReads(page: Page) {
  const state = { hold: false, reads: 0, held: [] as (() => void)[] };
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message)) as { type?: string; methodId?: number };
      if (frame.type === 'rpc' && frame.methodId === GET_CI_LOG_LINES_METHOD_ID) {
        state.reads += 1;
        if (state.hold) {
          state.held.push(() => server.send(message));
          return;
        }
      }
      server.send(message);
    });
    server.onMessage((message) => socket.send(message));
  });
  return {
    get reads() {
      return state.reads;
    },
    hold() {
      state.hold = true;
    },
    release() {
      state.hold = false;
      for (const send of state.held.splice(0)) send();
    },
  };
}
