// The fake forge from a spec: seed the pull or merge requests the fake
// GitHub and GitLab answer from, publish a seeded workspace's branch as
// one, read back what the app asked the forges, and reach a PR's review
// pane the way a user does. The fixture and invocation shapes are internal/harness/forgefake's
// (fixture.go, engine.go); its AGENTS.md lists which invocations have
// handlers.

import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { crc32, deflateSync } from 'node:zlib';
import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, type SeedResult } from './fixtures.js';

export interface ForgeComment {
  id?: number;
  author?: string;
  authorName?: string;
  body: string;
  createdAt?: string;
  /** RFC 3339; GitHub reads it as lastEditedAt. The viewer may edit its own
   * comments and delete them, or any as a maintainer. */
  editedAt?: string;
  /** GitLab conversation comments only: the further notes of its discussion. */
  replies?: ForgeComment[];
}

/** A review thread anchored to the diff; `side` defaults to right. */
export interface ForgeThread {
  id?: string;
  path: string;
  line?: number;
  startLine?: number;
  /** A range's first line as the forge's position names it, when it lies
   * on the other half from `line` (a removed line, type `old`). */
  startPoint?: { type?: 'old' | 'new'; oldLine?: number; newLine?: number };
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
  /** The base the forge recorded for the diff (GitHub baseRefOid, GitLab
   * start_sha); defaults to baseSha. It does not follow the base branch. */
  startSha?: string;
  /** Default clean; conflicts is GitHub CONFLICTING, GitLab has_conflicts. */
  mergeable?: 'clean' | 'conflicts' | 'checking';
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
  /** GitLab: the reviewers (each one of `reviewers`) who requested changes. */
  changeRequests?: string[];
  /** GitHub: teams asked to review, "owner/slug". */
  teamReviewers?: string[];
  /** Logins of the repository's `members`. */
  assignees?: string[];
  /** Names of the repository's `labels`. */
  labels?: string[];
  /** What the viewer may do here, each derived from the role and authorship when omitted. */
  viewer?: { update?: boolean; label?: boolean; triage?: boolean; admin?: boolean };
  /** GitLab: close, reopen and merge events (resource state events). */
  stateEvents?: { id?: number; state: 'closed' | 'reopened' | 'merged'; actor?: string; at?: string }[];
  /** GitLab: the requested changes were bypassed. */
  changesOverridden?: boolean;
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
  /** GitLab: the least role that may cancel a job or pipeline (default developer). */
  cancelRole?: 'developer' | 'maintainer' | 'no_one';
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
  /** RFC 3339; defaults to the seed time. GitHub re-runs a run only within 30 days of it. */
  createdAt?: string;
  updatedAt?: string;
  /** Job ids are unique per repository; a seed that repeats one is refused. */
  jobs: ForgeJob[];
  /** GitHub only: the run's artifacts. */
  artifacts?: ForgeArtifact[];
}

/** A GitLab job's archive or a GitHub run's artifact, served as its bytes. */
export interface ForgeArtifact {
  /** GitHub's artifact id; generated when omitted. */
  id?: number;
  /** GitHub's artifact name. */
  name?: string;
  text?: string;
  base64?: string;
  /** GitHub: listed expired, its download a 410. */
  expired?: boolean;
  /** GitHub: the list's expires_at (RFC 3339); 90 days after the run by default. */
  expiresAt?: string;
}

/** `manual` is GitLab's: a job waiting for a manual start. */
export type ForgeJobStatus = 'success' | 'failed' | 'running' | 'pending' | 'skipped' | 'canceled' | 'manual';

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
  /** A GitLab row that is not a build: a bridge (trigger job) or an
   * external commit status (stage "external" by default). Neither has a log. */
  kind?: 'bridge' | 'external';
  /** An external status's absolute link. */
  targetUrl?: string;
  /** A bridge's downstream pipeline path ("/grp/child/-/pipelines/9"). */
  downstream?: string;
  /** GitLab only: the job's artifacts archive. */
  artifacts?: ForgeArtifact;
  /** Set by the fake on a job a retry replaced. */
  retried?: boolean;
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
  /** GitLab: the viewer's events here are dropped from an events page after it is selected, as GitLab drops hidden ones. */
  eventsHidden?: boolean;
  /** Who can be asked to review and be assigned; `id` is GitLab's user id, generated when omitted. */
  members?: { login: string; name?: string; id?: number }[];
  /** GitHub: an organization's teams (`organization`), and a token whose teams search is refused. */
  teams?: { slug: string; name?: string }[];
  organization?: boolean;
  teamsRefused?: boolean;
  /** The repository's labels; color "#rrggbb". */
  labels?: { name: string; color?: string; id?: string }[];
  /** GitLab: what an older server lacks. */
  serverLacks?: ('rereview' | 'overrideRequestedChanges' | 'operationMode')[];
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
  /** A REST write's JSON body. */
  body?: unknown;
  status: number;
  detail?: string;
}

export type ForgeInvocation = ForgeCLIInvocation | ForgeHTTPInvocation;

export async function seedForge(harness: HarnessApp, repos: ForgeRepo[]): Promise<void> {
  await harness.rpc('HarnessForgeSeed', { repos });
}

/**
 * Move one current CI job of a seeded repository on, as its runner would
 * (HarnessForgeAdvanceJob): `job` names the newest attempt of that name,
 * `id` one job. The pipeline's status follows its jobs. Answers the job.
 */
export async function advanceForgeJob(
  harness: HarnessApp,
  advance: { forge: 'github' | 'gitlab'; project: string; job?: string; id?: number; status: ForgeJobStatus; log?: string; artifacts?: ForgeArtifact },
): Promise<ForgeJob> {
  return harness.rpc<ForgeJob>('HarnessForgeAdvanceJob', advance);
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
 * recorded under its route. With `hold` the request is recorded at once
 * and answered, or its answer lost, on `releaseForgeRoute`. HarnessReset
 * clears it.
 */
export async function refuseForgeRoute(
  harness: HarnessApp,
  refusal: { route: string; status: number; header?: Record<string, string>; body?: unknown; times?: number; hold?: boolean },
): Promise<void> {
  await harness.rpc('HarnessForgeRefuse', refusal);
}

/** End a held refusal on `route`: its requests get the refusal, or with
 * `drop` lose their answer (closed with no reply). */
export async function releaseForgeRoute(harness: HarnessApp, route: string, drop = false): Promise<void> {
  await harness.rpc('HarnessForgeRelease', { route, drop });
}

/** Clear a route's refusal. */
export async function clearForgeRefusal(harness: HarnessApp, route: string): Promise<void> {
  await harness.rpc('HarnessForgeRefuse', { route, status: 0 });
}

export async function forgeInvocations(harness: HarnessApp): Promise<ForgeInvocation[]> {
  const log = await harness.rpc<{ invocations: ForgeInvocation[] }>('HarnessForgeInvocations', 0);
  return log.invocations;
}

/** The fake's answered HTTP requests on `route` (status below 300), oldest first. */
export async function forgeRouteCalls(harness: HarnessApp, route: string): Promise<ForgeHTTPInvocation[]> {
  return (await forgeInvocations(harness))
    .filter((call): call is ForgeHTTPInvocation => call.via === 'http' && call.route === route && call.status < 300);
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

/** Seed one project with a git repository and a thread titled `title`,
 * for a pull request `publishPullRequest` publishes from it; answers its
 * path. */
export async function seedThreadProject(harness: HarnessApp, title: string, prefix = 'ci'): Promise<string> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `${prefix}-${randomBytes(4).toString('hex')}`,
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [{ title, turns: [{ userText: 'Hello.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
      },
    ],
  });
  return seed.projects[0].path;
}

/** One commit of a pull request's history (`PullHistory`). */
export interface PullCommit {
  message: string;
  body?: string;
  /** Files written before the commit: text, or bytes for a binary file. */
  files?: Record<string, string | Uint8Array>;
  /** Paths deleted. */
  remove?: string[];
  /** Renames, old path to new path (`git mv`). */
  rename?: Record<string, string>;
  /** Commit this on a side branch off the feature tip and merge it into
   * feature with a merge commit (`merge <message>`), so the PR carries both
   * the side commit and the merge. */
  merge?: boolean;
  /** The commit's author name (email `<name>@example.com`, lowercased
   * and dashed); default the harness's git identity. */
  author?: string;
}

/** A pull request's history: files committed on `main` first (the base
 * side of a delete, a change or a rename), then the feature commits, oldest
 * first. Commit dates are one second apart in order, so the PR's commit
 * list is the same on every run. */
export interface PullHistory {
  base?: Record<string, string | Uint8Array>;
  commits: PullCommit[];
}

/** A solid `width` x `height` PNG of one RGB color, for an image a diff
 * shows; different colors or sizes make different bytes. */
export function solidPNG(width: number, height: number, rgb: [number, number, number]): Uint8Array {
  const chunk = (type: string, data: Buffer) => {
    const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
    const length = Buffer.alloc(4);
    length.writeUInt32BE(data.length);
    const crc = Buffer.alloc(4);
    crc.writeUInt32BE(crc32(body));
    return Buffer.concat([length, body, crc]);
  };
  const header = Buffer.alloc(13);
  header.writeUInt32BE(width, 0);
  header.writeUInt32BE(height, 4);
  header.set([8, 2, 0, 0, 0], 8); // 8-bit RGB, no interlace
  const row = Buffer.concat([Buffer.from([0]), Buffer.from(Array.from({ length: width }, () => rgb).flat())]);
  const pixels = deflateSync(Buffer.concat(Array.from({ length: height }, () => row)));
  return new Uint8Array(Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', header),
    chunk('IDAT', pixels),
    chunk('IEND', Buffer.alloc(0)),
  ]));
}

/** A diagram SVG: text, so its diff has lines, and an image. */
export const DIAGRAM_SVG = '<svg xmlns="http://www.w3.org/2000/svg" width="40" height="20">\n  <rect width="40" height="20" fill="#3a7"/>\n</svg>\n';

/**
 * A pull request history with images and a merge, for the commit range
 * and image diff views. Base (on main): `logo.png` 16x16, `old.png`,
 * `moved.png`. Feature commits, oldest first, each listed by the PR as its
 * subject:
 *
 * 1. `add icon` adds `icon.png` 24x12 and `src/app.ts`.
 * 2. `restyle logo` changes `logo.png` to 32x32 and deletes `old.png`.
 * 3. `add diagram` on a side branch adds `diagram.svg`, and
 *    `merge add diagram` merges it (the PR lists both).
 * 4. `rename moved` renames `moved.png` to `img/moved.png` unchanged and
 *    adds a line to `src/app.ts`.
 * 5. `add blob` adds `data.bin`, a binary file that is not an image.
 */
/** The author of `imagePullHistory`'s later commits, so the PR's
 * Conversation has two commit groups. */
export const SECOND_AUTHOR = 'Sam Lee';

export function imagePullHistory(): PullHistory {
  return {
    base: {
      'logo.png': solidPNG(16, 16, [200, 40, 40]),
      'old.png': solidPNG(8, 8, [40, 40, 200]),
      'moved.png': solidPNG(10, 10, [40, 160, 40]),
    },
    commits: [
      { message: 'add icon', files: { 'icon.png': solidPNG(24, 12, [240, 200, 0]), 'src/app.ts': 'export const app = 1;\n' } },
      { message: 'restyle logo', files: { 'logo.png': solidPNG(32, 32, [40, 200, 200]) }, remove: ['old.png'] },
      { message: 'add diagram', files: { 'diagram.svg': DIAGRAM_SVG }, merge: true },
      { message: 'rename moved', author: SECOND_AUTHOR, rename: { 'moved.png': 'img/moved.png' }, files: { 'src/app.ts': 'export const app = 1;\nexport const moved = 2;\n' } },
      { message: 'add blob', author: SECOND_AUTHOR, files: { 'data.bin': new Uint8Array([0, 1, 2, 3, 0, 255, 254, 0]) } },
    ],
  };
}

type GitEnv = Record<string, string | undefined>;

/** The single feature commit: `files` added on a `feature` branch off
 * `main`. Answers the base commit. */
async function commitFeatureFiles(workspace: string, env: GitEnv, files: Record<string, string>): Promise<string> {
  const git = (...args: string[]) => execFileSync('git', args, { cwd: workspace, env, encoding: 'utf8' }).trim();
  const baseSha = git('rev-parse', 'HEAD');
  git('checkout', '--quiet', '-b', 'feature');
  for (const [name, content] of Object.entries(files)) {
    await mkdir(path.dirname(path.join(workspace, name)), { recursive: true });
    await writeFile(path.join(workspace, name), content);
  }
  git('add', '--', ...Object.keys(files));
  git('commit', '--quiet', '-m', 'feature work', '-m', 'Adds the mailer and its tests.');
  return baseSha;
}

/** `history` committed: its base files on `main`, then its commits on a
 * `feature` branch, one second apart. Answers the base commit. */
async function commitPullHistory(workspace: string, env: GitEnv, history: PullHistory): Promise<string> {
  const git = (...args: string[]) => execFileSync('git', args, { cwd: workspace, env, encoding: 'utf8' }).trim();
  let second = 0;
  const dated = (args: string[], author?: string) => {
    second += 1;
    const date = `2026-01-01T00:00:${String(second).padStart(2, '0')}Z`;
    const identity = author ? { GIT_AUTHOR_NAME: author, GIT_AUTHOR_EMAIL: `${author.toLowerCase().replace(/\s+/g, '-')}@example.com` } : {};
    execFileSync('git', args, { cwd: workspace, env: { ...env, ...identity, GIT_AUTHOR_DATE: date, GIT_COMMITTER_DATE: date } });
  };
  const write = async (files: Record<string, string | Uint8Array>) => {
    for (const [name, data] of Object.entries(files)) {
      await mkdir(path.dirname(path.join(workspace, name)), { recursive: true });
      await writeFile(path.join(workspace, name), data);
    }
    if (Object.keys(files).length) git('add', '--', ...Object.keys(files));
  };
  const apply = async (step: PullCommit) => {
    await write(step.files ?? {});
    for (const [from, to] of Object.entries(step.rename ?? {})) {
      await mkdir(path.dirname(path.join(workspace, to)), { recursive: true });
      git('mv', '--', from, to);
    }
    if (step.remove?.length) git('rm', '--quiet', '--', ...step.remove);
    dated(['commit', '--quiet', '--allow-empty', '-m', step.message, ...(step.body ? ['-m', step.body] : [])], step.author);
  };
  if (history.base) {
    await write(history.base);
    dated(['commit', '--quiet', '-m', 'base files']);
  }
  const baseSha = git('rev-parse', 'HEAD');
  git('checkout', '--quiet', '-b', 'feature');
  for (const [index, step] of history.commits.entries()) {
    if (!step.merge) {
      await apply(step);
      continue;
    }
    const side = `side-${index}`;
    git('checkout', '--quiet', '-b', side);
    await apply(step);
    git('checkout', '--quiet', 'feature');
    dated(['merge', '--quiet', '--no-ff', '-m', `merge ${step.message}`, side]);
    git('branch', '--quiet', '-D', side);
  }
  return baseSha;
}

/**
 * Publish the seeded `workspace` as `repo`'s first pull request and seed
 * the fake forge with `repo`. The workspace gets a `feature` branch off
 * `main` (the fixture's default head and base refs): one commit that writes
 * `content` when it is a file map, or `content.commits` of a `PullHistory`.
 * An origin publishes it the way the forge does. The origin URL is
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
  content: Record<string, string> | PullHistory = { 'feature.md': 'Feature work.\n' },
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

  const baseSha = 'commits' in content && Array.isArray(content.commits)
    ? await commitPullHistory(workspace, env, content as PullHistory)
    : await commitFeatureFiles(workspace, env, content as Record<string, string>);
  const headSha = git(workspace, 'rev-parse', 'HEAD');
  git(workspace, 'push', '--quiet', bare, 'main', 'feature');
  git(bare, 'update-ref', PULL_HEAD_REF[repo.forge](pull.number), headSha);

  // The remote-tracking refs a push to origin leaves: the app asks the
  // forge for a branch's PR only once origin has the branch. Written
  // before the origin exists, for the same reason the forge is seeded
  // first.
  git(workspace, 'update-ref', 'refs/remotes/origin/main', baseSha);
  git(workspace, 'update-ref', 'refs/remotes/origin/feature', headSha);

  // A GitLab position names lines by the merge request's diff, which the
  // fake answers from the pull's `diff`: the pushed change, unless the spec
  // seeds its own. A GitHub pull needs none for its comments.
  const diff = pull.diff ?? (repo.forge === 'gitlab' ? git(workspace, 'diff', '-M', '--no-color', baseSha, headSha) + '\n' : undefined);
  const seeded: ForgeRepo = {
    ...repo,
    pulls: [{ ...pull, headRef: 'feature', baseRef: 'main', headSha, baseSha, startSha: baseSha, ...(diff === undefined ? {} : { diff }) }, ...rest],
  };
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

/** Open a stage chip's list and a job's log from it; answers the log view. */
export async function openCIJobLog(page: Page, surface: Locator, stage: string, job: string): Promise<Locator> {
  await surface.getByTestId('review-ci-chip').filter({ hasText: stage }).click();
  await page.getByTestId('review-ci-job').filter({ hasText: job }).click();
  const log = surface.getByTestId('review-ci-log');
  await expect(log).toBeVisible();
  return log;
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
