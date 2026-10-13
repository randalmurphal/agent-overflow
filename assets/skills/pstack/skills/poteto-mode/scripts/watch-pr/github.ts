import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import type * as T from "./types.ts";
import { nonEmpty, parsePrNumber } from "./types.ts";
interface CommandResult {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}
export class WatcherQueryError extends Error {
  readonly failure: T.QueryFailure;
  constructor(failure: T.QueryFailure) {
    super(failure.detail);
    this.name = "WatcherQueryError";
    this.failure = failure;
  }
}
export class ChecksUnavailable extends WatcherQueryError {
  constructor(detail: string) {
    super({ kind: "checks-unavailable", retryable: true, detail });
    this.name = "ChecksUnavailable";
  }
}
const firstLine = (value: string): string =>
  value.trim().split(/\r?\n/, 1)[0]?.slice(0, 240) ?? "";
function run(argv: readonly [string, ...string[]]): Promise<CommandResult> {
  return new Promise((resolve, reject) => {
    const child = spawn(argv[0], argv.slice(1), {
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.setEncoding("utf8");
    child.stderr.setEncoding("utf8");
    child.stdout.on("data", (chunk: string) => {
      stdout += chunk;
    });
    child.stderr.on("data", (chunk: string) => {
      stderr += chunk;
    });
    child.on("error", reject);
    child.on("close", (code) => resolve({ code: code ?? -1, stdout, stderr }));
  });
}
function parseJson(text: string, label: string): unknown {
  try {
    return JSON.parse(text);
  } catch (error) {
    throw new WatcherQueryError({
      kind: "json-parse",
      retryable: true,
      detail: `${label}: ${error instanceof Error ? error.message : String(error)}`,
    });
  }
}
async function runJson(argv: readonly [string, ...string[]]): Promise<unknown> {
  const result = await run(argv);
  if (result.code !== 0)
    throw new WatcherQueryError({
      kind: "command-exit",
      retryable: true,
      code: result.code,
      detail:
        firstLine(result.stderr) || `${argv.join(" ")} exited ${result.code}`,
    });
  return parseJson(result.stdout, argv.join(" "));
}
function raw(value: unknown): string {
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}
function missing(path: string, value?: unknown): never {
  throw new WatcherQueryError({
    kind: "missing-key",
    retryable: true,
    detail:
      value === undefined
        ? `missing ${path}`
        : `invalid ${path}: ${raw(value)}`,
    ...(value === undefined ? {} : { rawValue: raw(value) }),
  });
}
function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
function record(value: unknown, path: string): Record<string, unknown> {
  if (!isRecord(value)) missing(path, value);
  return value;
}
function list(value: unknown, path: string): readonly unknown[] {
  if (!Array.isArray(value)) missing(path, value);
  return value;
}
function string(value: unknown, path: string): string {
  if (typeof value !== "string") missing(path, value);
  return value;
}
const optionalString = (value: unknown, path: string): string | null =>
  value === null ? null : string(value, path);
// Every read goes through pstack's forge script, which picks gh or glab from
// the checkout's remote and prints the same JSON on both forges.
const FORGE = fileURLToPath(new URL("../forge", import.meta.url));
const MERGE_STATES: Readonly<Record<string, T.MergeStateStatus>> = {
  clean: "CLEAN",
  unstable: "UNSTABLE",
  conflicts: "DIRTY",
  behind: "BEHIND",
  blocked: "BLOCKED",
  draft: "DRAFT",
  checking: "UNKNOWN",
  none: "UNKNOWN",
};
const REVIEW_DECISIONS: Readonly<Record<string, T.ReviewDecision>> = {
  approved: "APPROVED",
  changes_requested: "CHANGES_REQUESTED",
  review_required: "REVIEW_REQUIRED",
  none: null,
};
const PR_STATES = { open: "OPEN", closed: "CLOSED", merged: "MERGED" } as const;
const COMMIT_STATES: Readonly<Record<string, T.RollupState | null>> = {
  pass: "SUCCESS",
  fail: "FAILURE",
  pending: "PENDING",
  none: null,
};
function mapped<V>(
  value: unknown,
  table: Readonly<Record<string, V>>,
  path: string
): V {
  if (typeof value === "string" && Object.hasOwn(table, value))
    return table[value] as V;
  return missing(path, value);
}
function integer(value: unknown, path: string): number {
  if (!Number.isInteger(value)) missing(path, value);
  return Number(value);
}
// The repository the forge script resolves from the remote: GitLab nests
// groups, so the owner is everything before the last segment.
export function parseForgeRepository(value: unknown): T.Repository {
  const object = record(value, "forge which");
  const forge = string(object.forge, "forge which.forge");
  const host = string(object.host, "forge which.host");
  const project = string(object.project, "forge which.project");
  const slash = project.lastIndexOf("/");
  if (slash <= 0 || slash === project.length - 1)
    missing("forge which.project", project);
  const base = `https://${host}/${project}`;
  return {
    owner: project.slice(0, slash),
    repo: project.slice(slash + 1),
    pullUrlBase:
      forge === "gitlab" ? `${base}/-/merge_requests/` : `${base}/pull/`,
  };
}
export function parseForgePullRequest(
  value: unknown,
  context: T.PrContext
): T.PullRequestFacts {
  const object = record(value, "pull request");
  if (typeof object.draft !== "boolean")
    missing("pull request.draft", object.draft);
  const merge = mapped(object.merge, MERGE_STATES, "pull request.merge");
  return {
    context,
    mergeable:
      object.merge === "conflicts"
        ? "CONFLICTING"
        : object.merge === "checking"
          ? "UNKNOWN"
          : "MERGEABLE",
    mergeStateStatus: merge,
    reviewDecision: mapped(
      object.review,
      REVIEW_DECISIONS,
      "pull request.review"
    ),
    headRefOid: optionalString(object.head_sha, "pull request.head_sha"),
    headRefName: string(object.head, "pull request.head"),
    baseRefName: string(object.base, "pull request.base"),
    state: mapped(object.state, PR_STATES, "pull request.state"),
    mergedAt: optionalString(object.merged_at, "pull request.merged_at"),
    isDraft: object.draft,
  };
}
// The owner-approval gate is excluded from pending everywhere, so the rule has
// one home. Classifying it as pending makes the watcher wait on a human.
function pendingOrGate(
  details: {
    readonly name: string;
    readonly description: string;
    readonly link: string;
    readonly workflow: string;
  },
  reportedState: string
): T.Check {
  return details.name === "Code Review Gate"
    ? {
        ...details,
        kind: "code-review-gate",
        name: "Code Review Gate",
        reportedState,
      }
    : { ...details, kind: "pending", reportedState };
}
export function parseForgeCheck(value: unknown): T.Check {
  const object = record(value, "check");
  const details = {
    name: string(object.name, "check.name"),
    description: "",
    link: typeof object.url === "string" ? object.url : "",
    workflow: "",
  };
  const state = string(object.state, "check.state");
  switch (state) {
    case "pass":
      return { ...details, kind: "passed", reportedState: "SUCCESS" };
    case "pending":
      return pendingOrGate(details, "PENDING");
    case "skipped":
    case "neutral":
      return { ...details, kind: "skipped", reportedState: state.toUpperCase() };
    case "cancelled":
      return { ...details, kind: "failed", reportedState: "CANCELLED" };
    default:
      return { ...details, kind: "failed", reportedState: "FAILURE" };
  }
}
function isBugbot(comment: T.ReviewComment | null): boolean {
  if (comment === null) return false;
  const author = (comment.authorLogin ?? "").toLowerCase();
  const body = comment.body.toLowerCase();
  return (
    author.includes("bugbot") ||
    (author === "cursor" &&
      [
        "bugbot",
        "cursor_automation_id",
        "agentic security review",
        "description start",
        "severity",
      ].some((token) => body.includes(token)))
  );
}
function passKey(comment: T.ReviewComment | null): string | null {
  if (comment === null) return null;
  for (const pattern of [
    /RUN_ID:\s*([a-zA-Z0-9_.:-]+)/,
    /CURSOR_AUTOMATION_ID:\s*([a-zA-Z0-9_.:-]+)/,
  ]) {
    const match = pattern.exec(comment.body);
    if (match?.[1]) return match[1];
  }
  return null;
}
export function parseForgeThreads(value: unknown): readonly T.ReviewThread[] {
  const threads = list(value, "threads").map((item, index) => {
    const path = `threads[${index}]`;
    const thread = record(item, path);
    if (typeof thread.resolved !== "boolean")
      missing(`${path}.resolved`, thread.resolved);
    const comments = list(thread.comments, `${path}.comments`);
    const first =
      comments.length === 0 ? null : record(comments[0], `${path}.comments[0]`);
    return {
      id: string(thread.id, `${path}.id`),
      resolved: thread.resolved,
      firstComment:
        first === null
          ? null
          : {
              authorLogin: optionalString(first.author, `${path}.comments[0].author`),
              body: string(first.body, `${path}.comments[0].body`),
              path: optionalString(thread.path, `${path}.path`),
              line:
                thread.line === null ? null : integer(thread.line, `${path}.line`),
              createdAt: string(first.created_at, `${path}.comments[0].created_at`),
            },
    };
  });
  const keys = new Set<string>();
  let keyless = false;
  for (const thread of threads) {
    if (!isBugbot(thread.firstComment)) continue;
    const key = passKey(thread.firstComment);
    if (key === null) keyless = true;
    else keys.add(key);
  }
  const passes = keys.size > 0 ? keys.size : keyless ? 1 : 0;
  return threads
    .filter((thread) => !thread.resolved)
    .map(({ id, firstComment }) => ({
      id,
      firstComment,
      isBugbot: isBugbot(firstComment),
      bugbotReviewPasses: passes,
    }));
}
export function parseForgeCommits(value: unknown): readonly T.CommitRollup[] {
  return list(value, "commits").map((item, index) => {
    const commit = record(item, `commits[${index}]`);
    return {
      oid: string(commit.sha, `commits[${index}].sha`),
      state: mapped(commit.state, COMMIT_STATES, `commits[${index}].state`),
    };
  });
}

export class ForgeCliReader implements T.GitHubReader {
  private repository: Promise<T.Repository> | null = null;
  constructor(private readonly forge: string = FORGE) {}
  private forgeJson(...args: string[]): Promise<unknown> {
    return runJson([this.forge, ...args]);
  }
  private origin(): Promise<T.Repository> {
    this.repository ??= this.forgeJson("which").then(parseForgeRepository);
    return this.repository;
  }
  // The forge script reads the checkout's own remote, so a context naming
  // another repository cannot be answered.
  private async check(context: T.PrContext): Promise<void> {
    const origin = await this.origin();
    if (origin.owner !== context.owner || origin.repo !== context.repo)
      throw new WatcherQueryError({
        kind: "invalid-context-url",
        retryable: false,
        rawValue: `${context.owner}/${context.repo}`,
        detail: `watch-pr reads the checkout's remote (${origin.owner}/${origin.repo}), not ${context.owner}/${context.repo}`,
      });
  }
  async originRepo(): Promise<T.Repository | null> {
    try {
      return await this.origin();
    } catch (error) {
      if (!(error instanceof WatcherQueryError)) throw error;
      this.repository = null;
      return null;
    }
  }
  async currentPr(pr: T.PrNumber | null): Promise<T.PrContext> {
    const origin = await this.origin();
    const args = ["pr", "view"];
    if (pr !== null) args.push(String(pr));
    const object = record(await this.forgeJson(...args), "current PR");
    return {
      ...origin,
      number: pr ?? parsePrNumber(object.number, "current PR.number"),
    };
  }
  async pullRequest(context: T.PrContext): Promise<T.PullRequestFacts> {
    await this.check(context);
    return parseForgePullRequest(
      await this.forgeJson("pr", "view", String(context.number)),
      context
    );
  }
  async openPullRequests(
    repository: T.Repository
  ): Promise<readonly T.OpenPullRequest[]> {
    await this.check({ ...repository, number: parsePrNumber(1) });
    const value = await this.forgeJson("pr", "list", "--state", "open", "--limit", "300");
    return list(value, "open PRs").map((item, index) => {
      const object = record(item, `open PRs[${index}]`);
      return {
        number: parsePrNumber(object.number, `open PRs[${index}].number`),
        headRefName: string(object.head, `open PRs[${index}].head`),
        baseRefName: string(object.base, `open PRs[${index}].base`),
      };
    });
  }
  async checksFastPath(context: T.PrContext): Promise<T.ChecksFastPath> {
    await this.check(context);
    const result = await run([this.forge, "pr", "checks", String(context.number)]);
    if (result.code === 0 && result.stdout.trim()) {
      try {
        const object = record(parseJson(result.stdout, "forge pr checks"), "checks");
        return {
          kind: "checks",
          checks: list(object.checks, "checks.checks").map(parseForgeCheck),
        };
      } catch (error) {
        if (!(error instanceof WatcherQueryError)) throw error;
      }
    }
    return { kind: "unusable", exitCode: result.code, stderr: result.stderr };
  }
  // forge pr checks already reads every page of the head commit's checks, so
  // there is no second source to fall back to.
  async checkRollupPage(): Promise<T.RollupPage> {
    return { checks: [], endCursor: null };
  }
  async reviewThreads(
    context: T.PrContext
  ): Promise<readonly T.ReviewThread[]> {
    await this.check(context);
    return parseForgeThreads(
      await this.forgeJson("pr", "threads", String(context.number))
    );
  }
  async commitRollups(
    context: T.PrContext
  ): Promise<readonly T.CommitRollup[]> {
    await this.check(context);
    return parseForgeCommits(
      await this.forgeJson("pr", "commits", String(context.number))
    );
  }
}

export async function resolveChecks(
  reader: T.GitHubReader,
  context: T.PrContext
): Promise<T.CheckRead> {
  const fast = await reader.checksFastPath(context);
  const direct = fast.kind === "checks" ? nonEmpty(fast.checks) : null;
  if (direct !== null) return { source: "forge-checks", checks: direct };
  const checks: T.Check[] = [];
  let after: string | null = null;
  do {
    const page = await reader.checkRollupPage(context, after);
    checks.push(...page.checks);
    after = page.endCursor;
  } while (after !== null);
  const fallback = nonEmpty(checks);
  if (fallback !== null) return { source: "graphql-rollup", checks: fallback };
  const suffix =
    fast.kind === "unusable"
      ? `fast path exit=${fast.exitCode}; GraphQL rollup was empty${firstLine(fast.stderr) ? `; ${firstLine(fast.stderr)}` : ""}`
      : "fast path and GraphQL rollup were empty";
  throw new ChecksUnavailable(`could not read PR checks: ${suffix}`);
}
export async function resolveContext(args: {
  readonly reader: T.GitHubReader;
  readonly owner: string | null;
  readonly repo: string | null;
  readonly pr: T.PrNumber | null;
}): Promise<T.PrContext> {
  if (args.pr !== null && args.owner !== null && args.repo !== null)
    return { owner: args.owner, repo: args.repo, number: args.pr };
  if (args.pr !== null) {
    const origin = await args.reader.originRepo();
    if (origin !== null)
      return {
        ...origin,
        owner: args.owner ?? origin.owner,
        repo: args.repo ?? origin.repo,
        number: args.pr,
      };
  }
  const inferred = await args.reader.currentPr(args.pr);
  return {
    ...inferred,
    owner: args.owner ?? inferred.owner,
    repo: args.repo ?? inferred.repo,
    number: args.pr ?? inferred.number,
  };
}
export function orderStack(
  context: T.PrContext,
  open: readonly T.OpenPullRequest[]
): T.NonEmpty<T.PrContext> {
  const byNumber = new Map(open.map((pr) => [pr.number, pr]));
  const byHead = new Map(open.map((pr) => [pr.headRefName, pr]));
  const children = new Map<string, T.OpenPullRequest[]>();
  for (const pr of open)
    children.set(pr.baseRefName, [...(children.get(pr.baseRefName) ?? []), pr]);
  for (const values of children.values())
    values.sort((a, b) => a.number - b.number);
  const start = byNumber.get(context.number);
  if (start === undefined) return [context];
  const down: T.OpenPullRequest[] = [];
  let current = start;
  while (byHead.has(current.baseRefName)) {
    const parent = byHead.get(current.baseRefName);
    if (parent === undefined) break;
    down.push(parent);
    current = parent;
  }
  const seen = new Set<T.PrNumber>([
    ...down.map((pr) => pr.number),
    start.number,
  ]);
  const up: T.OpenPullRequest[] = [];
  const visit = (parent: T.OpenPullRequest): void => {
    for (const child of children.get(parent.headRefName) ?? []) {
      if (seen.has(child.number)) continue;
      seen.add(child.number);
      up.push(child);
      visit(child);
    }
  };
  visit(start);
  return (
    nonEmpty(
      [...down.reverse(), start, ...up].map((pr) => ({
        ...context,
        number: pr.number,
      }))
    ) ?? [context]
  );
}
export async function discoverStack(
  reader: T.GitHubReader,
  context: T.PrContext
): Promise<T.NonEmpty<T.PrContext>> {
  return orderStack(context, await reader.openPullRequests(context));
}
