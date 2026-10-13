import { describe, expect, it } from "bun:test";
import { chmodSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  ChecksUnavailable,
  ForgeCliReader,
  WatcherQueryError,
  orderStack,
  parseForgeCheck,
  parseForgeCommits,
  parseForgePullRequest,
  parseForgeRepository,
  parseForgeThreads,
  resolveChecks,
  resolveContext,
} from "./github.ts";
import {
  fakeReader,
  failedCheck,
  passingCheck,
  pendingCheck,
} from "./fakes.test-helper.ts";
import { parsePrNumber } from "./types.ts";

const context = {
  owner: "owner",
  repo: "repo",
  number: parsePrNumber(42),
};

describe("checks fallback chain", () => {
  it("uses a non-empty fast-path result without a rollup query", async () => {
    const reader = fakeReader({
      fastPath: { kind: "checks", checks: [passingCheck("fast")] },
    });
    const read = await resolveChecks(reader, context);
    expect(read.source).toBe("forge-checks");
    expect(read.checks.map((check) => check.name)).toEqual(["fast"]);
    expect(reader.calls).toEqual(["checksFastPath"]);
  });

  it("paginates GraphQL when the fast path is unusable", async () => {
    const reader = fakeReader({
      fastPath: { kind: "unusable", exitCode: 8, stderr: "" },
      rollupPages: [
        { checks: [passingCheck("first")], endCursor: "next" },
        { checks: [failedCheck("second")], endCursor: null },
      ],
    });
    const read = await resolveChecks(reader, context);
    expect(read.source).toBe("graphql-rollup");
    expect(read.checks.map((check) => check.name)).toEqual(["first", "second"]);
    expect(reader.calls).toEqual([
      "checksFastPath",
      "checkRollupPage:null",
      "checkRollupPage:next",
    ]);
  });

  it("falls back when valid fast-path JSON represented an empty list", async () => {
    const reader = fakeReader({
      fastPath: { kind: "checks", checks: [] },
      rollupPages: [{ checks: [pendingCheck("fallback")], endCursor: null }],
    });
    expect((await resolveChecks(reader, context)).checks[0].name).toBe(
      "fallback"
    );
    expect(reader.calls).toEqual(["checksFastPath", "checkRollupPage:null"]);
  });

  it("fails closed when both paths are empty", async () => {
    const reader = fakeReader({
      fastPath: {
        kind: "unusable",
        exitCode: 8,
        stderr: "credential cannot read checks",
      },
    });
    await expect(resolveChecks(reader, context)).rejects.toBeInstanceOf(
      ChecksUnavailable
    );
    expect(reader.calls).toEqual(["checksFastPath", "checkRollupPage:null"]);
  });
});

describe("forge check mapping", () => {
  it("maps every forge check state, failing closed on an unknown one", () => {
    const cases = [
      ["pass", "passed", "SUCCESS"],
      ["pending", "pending", "PENDING"],
      ["skipped", "skipped", "SKIPPED"],
      ["neutral", "skipped", "NEUTRAL"],
      ["cancelled", "failed", "CANCELLED"],
      ["fail", "failed", "FAILURE"],
      ["future_value", "failed", "FAILURE"],
    ] as const;
    for (const [state, kind, reportedState] of cases)
      expect(
        parseForgeCheck({ name: "ci", state, url: "https://ci/1" })
      ).toMatchObject({ kind, reportedState, link: "https://ci/1" });
  });

  it("classifies a pending Code Review Gate as the gate", () => {
    expect(
      parseForgeCheck({ name: "Code Review Gate", state: "pending", url: null })
    ).toMatchObject({ kind: "code-review-gate", link: "" });
  });
});

describe("forge pull request parsing", () => {
  const view = {
    number: 42,
    state: "open",
    draft: false,
    head: "feature",
    head_sha: "head",
    base: "main",
    merge: "clean",
    review: "approved",
    merged_at: null,
  };

  it("maps the forge's merge vocabulary onto the merge states the policy reads", () => {
    const cases = [
      ["clean", "MERGEABLE", "CLEAN"],
      ["unstable", "MERGEABLE", "UNSTABLE"],
      ["conflicts", "CONFLICTING", "DIRTY"],
      ["behind", "MERGEABLE", "BEHIND"],
      ["blocked", "MERGEABLE", "BLOCKED"],
      ["draft", "MERGEABLE", "DRAFT"],
      ["checking", "UNKNOWN", "UNKNOWN"],
      ["none", "MERGEABLE", "UNKNOWN"],
    ] as const;
    for (const [merge, mergeable, mergeStateStatus] of cases)
      expect(parseForgePullRequest({ ...view, merge }, context)).toMatchObject({
        mergeable,
        mergeStateStatus,
      });
  });

  it("reads review none as no decision and the state in upper case", () => {
    const facts = parseForgePullRequest(
      {
        ...view,
        review: "none",
        state: "merged",
        merged_at: "2026-01-01T00:00:00Z",
      },
      context
    );
    expect(facts).toMatchObject({
      reviewDecision: null,
      state: "MERGED",
      mergedAt: "2026-01-01T00:00:00Z",
      headRefName: "feature",
      baseRefName: "main",
      isDraft: false,
    });
  });

  it("rejects unknown values as retryable errors carrying the raw value", () => {
    for (const bad of [
      { merge: "future_state" },
      { review: "maybe" },
      { state: "locked" },
    ]) {
      try {
        parseForgePullRequest({ ...view, ...bad }, context);
        throw new Error("expected parser to throw");
      } catch (error) {
        expect(error).toBeInstanceOf(WatcherQueryError);
        if (!(error instanceof WatcherQueryError)) throw error;
        expect(error.failure).toMatchObject({
          kind: "missing-key",
          retryable: true,
          rawValue: JSON.stringify(Object.values(bad)[0]),
        });
      }
    }
  });
});

it("reads the repository and its pull request links from forge which", () => {
  expect(
    parseForgeRepository({
      forge: "github",
      host: "github.com",
      project: "owner/repo",
    })
  ).toEqual({
    owner: "owner",
    repo: "repo",
    pullUrlBase: "https://github.com/owner/repo/pull/",
  });
  expect(
    parseForgeRepository({
      forge: "gitlab",
      host: "gitlab.example",
      project: "grp/sub/tool",
    })
  ).toEqual({
    owner: "grp/sub",
    repo: "tool",
    pullUrlBase: "https://gitlab.example/grp/sub/tool/-/merge_requests/",
  });
  expect(() =>
    parseForgeRepository({ forge: "github", host: "github.com", project: "repo" })
  ).toThrow(WatcherQueryError);
});

it("maps forge commit states onto rollup states", () => {
  expect(
    parseForgeCommits([
      { sha: "a", state: "pass" },
      { sha: "b", state: "fail" },
      { sha: "c", state: "pending" },
      { sha: "d", state: "none" },
    ])
  ).toEqual([
    { oid: "a", state: "SUCCESS" },
    { oid: "b", state: "FAILURE" },
    { oid: "c", state: "PENDING" },
    { oid: "d", state: null },
  ]);
});

it("annotates Bugbot threads with distinct review-pass counts", () => {
  const thread = (
    id: string,
    resolved: boolean,
    author: string,
    body: string
  ) => ({
    id,
    resolved,
    outdated: false,
    path: "a.ts",
    line: 1,
    comment_count: 1,
    comments: [{ id: 1, author, body, created_at: "now" }],
  });
  const threads = parseForgeThreads([
    thread("one", false, "bugbot", "RUN_ID: run-1"),
    thread("two", false, "cursor", "CURSOR_AUTOMATION_ID: run-2 severity high"),
    thread("resolved", true, "bugbot", "RUN_ID: run-3"),
  ]);
  expect(threads.map((item) => item.id)).toEqual(["one", "two"]);
  expect(threads.map((item) => item.isBugbot)).toEqual([true, true]);
  expect(threads.map((item) => item.bugbotReviewPasses)).toEqual([3, 3]);
  expect(threads[0].firstComment).toEqual({
    authorLogin: "bugbot",
    body: "RUN_ID: run-1",
    path: "a.ts",
    line: 1,
    createdAt: "now",
  });
});

// A stand-in for the forge script that answers from canned JSON and logs the
// arguments of each call.
function stubForge(answers: Record<string, unknown>): {
  forge: string;
  log: string;
} {
  const dir = mkdtempSync(join(tmpdir(), "watch-pr-forge-"));
  const log = join(dir, "calls");
  const cases = Object.entries(answers)
    .map(
      ([args, value]) =>
        `  '${args}') printf '%s\\n' '${JSON.stringify(value)}' ;;`
    )
    .join("\n");
  const forge = join(dir, "forge");
  writeFileSync(
    forge,
    `#!/bin/sh\nprintf '%s\\n' "$*" >> '${log}'\ncase "$*" in\n${cases}\n  *) echo "forge: unexpected $*" >&2; exit 1 ;;\nesac\n`
  );
  chmodSync(forge, 0o755);
  return { forge, log };
}

describe("ForgeCliReader", () => {
  const which = { forge: "gitlab", host: "gitlab.example", project: "grp/tool" };

  it("reads a pull request, its checks, threads and commits through the forge script", async () => {
    const { forge, log } = stubForge({
      which,
      "pr view": { number: 7 },
      "pr view 7": {
        number: 7,
        state: "open",
        draft: false,
        head: "feature",
        head_sha: "abc",
        base: "main",
        merge: "blocked",
        review: "review_required",
        merged_at: null,
      },
      "pr checks 7": {
        number: 7,
        sha: "abc",
        state: "pending",
        checks: [{ name: "unit", state: "pending", url: "u" }],
      },
      "pr threads 7": [],
      "pr commits 7": [{ sha: "abc", state: "pending" }],
      "pr list --state open --limit 300": [
        { number: 7, head: "feature", base: "main" },
      ],
    });
    const reader = new ForgeCliReader(forge);
    const current = await reader.currentPr(null);
    expect(current).toEqual({
      owner: "grp",
      repo: "tool",
      number: parsePrNumber(7),
      pullUrlBase: "https://gitlab.example/grp/tool/-/merge_requests/",
    });
    expect(await reader.pullRequest(current)).toMatchObject({
      mergeStateStatus: "BLOCKED",
      reviewDecision: "REVIEW_REQUIRED",
      headRefOid: "abc",
    });
    expect((await resolveChecks(reader, current)).checks[0]).toMatchObject({
      name: "unit",
      kind: "pending",
    });
    expect(await reader.reviewThreads(current)).toEqual([]);
    expect(await reader.commitRollups(current)).toEqual([
      { oid: "abc", state: "PENDING" },
    ]);
    expect(await reader.openPullRequests(current)).toEqual([
      { number: parsePrNumber(7), headRefName: "feature", baseRefName: "main" },
    ]);
    expect((await Bun.file(log).text()).split("\n").filter(Boolean)).toEqual([
      "which",
      "pr view",
      "pr view 7",
      "pr checks 7",
      "pr threads 7",
      "pr commits 7",
      "pr list --state open --limit 300",
    ]);
  });

  it("refuses a context naming a repository other than the remote's", async () => {
    const { forge, log } = stubForge({ which });
    const reader = new ForgeCliReader(forge);
    try {
      await reader.pullRequest({ ...context, owner: "other" });
      throw new Error("expected the reader to refuse");
    } catch (error) {
      expect(error).toBeInstanceOf(WatcherQueryError);
      if (!(error instanceof WatcherQueryError)) throw error;
      expect(error.failure).toMatchObject({
        kind: "invalid-context-url",
        retryable: false,
      });
    }
    expect(await Bun.file(log).text()).toBe("which\n");
  });

  it("reports a failing forge as unusable checks and then fails closed", async () => {
    const { forge } = stubForge({ which });
    const reader = new ForgeCliReader(forge);
    const current = { owner: "grp", repo: "tool", number: context.number };
    await expect(resolveChecks(reader, current)).rejects.toBeInstanceOf(
      ChecksUnavailable
    );
  });
});

describe("context and stack discovery", () => {
  it("returns a fully explicit context without any reader call", async () => {
    const reader = fakeReader();
    expect(
      await resolveContext({
        reader,
        owner: "explicit",
        repo: "repo",
        pr: context.number,
      })
    ).toEqual({ owner: "explicit", repo: "repo", number: context.number });
    expect(reader.calls).toEqual([]);
  });

  it("uses the local origin before currentPr for an explicit number", async () => {
    const reader = fakeReader({ origin: { owner: "local", repo: "checkout" } });
    expect(
      await resolveContext({
        reader,
        owner: null,
        repo: null,
        pr: context.number,
      })
    ).toEqual({ owner: "local", repo: "checkout", number: context.number });
    expect(reader.calls).toEqual(["originRepo"]);
  });

  it("orders the connected stack bottom-to-top", () => {
    const ordered = orderStack(context, [
      {
        number: parsePrNumber(41),
        headRefName: "base-feature",
        baseRefName: "main",
      },
      {
        number: context.number,
        headRefName: "feature",
        baseRefName: "base-feature",
      },
      {
        number: parsePrNumber(43),
        headRefName: "upstack",
        baseRefName: "feature",
      },
    ]);
    expect(ordered.map((item) => Number(item.number))).toEqual([41, 42, 43]);
  });
});
