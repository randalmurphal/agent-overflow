// The frontend harness bridge (§4/§5 of docs/specs/testing-harness.md)
// end to end: a real backend, a real SPA, and the request/reply loop
// running over the same WebSocket everything else does.
//
// The unit suites already cover the shapes (frontend/src/lib/harness/*.test.ts
// for the snapshot and dispatch rules, app_harness_ui_test.go for the
// waiter correlation). What only this level can prove is that the two
// halves are actually WIRED: that a bootstrap flag arms a bridge in a
// browser, that the DOM the SPA really renders yields the item ids the
// backend seeded, that geometry is non-degenerate under a real layout
// engine, and that rAF actually turns in a headless page.
import { execFile } from "node:child_process";
import * as path from "node:path";
import { promisify } from "node:util";
import type { Page } from "@playwright/test";
import type { HarnessApp } from "../src/harness.js";
import { test, expect, type SeedResult } from "./fixtures.js";

const run = promisify(execFile);

interface SnapshotRow {
  itemId: string;
  kind: string;
  role: string;
  status: string;
  streaming: boolean;
  badge: string;
  rowIndex: number;
  inViewport: boolean;
  rect: { x: number; y: number; w: number; h: number };
  textHead: string;
}

interface SnapshotPane {
  paneId: string;
  paneKind: string;
  focused: boolean;
  threadId: string;
  rect: { x: number; y: number; w: number; h: number };
  scroll: {
    top: number;
    height: number;
    client: number;
    atBottom: boolean;
  } | null;
  mountedRows: number;
  rows: SnapshotRow[];
}

interface Viewport {
  v: number;
  settled: boolean;
  sinceMutationMs: number;
  activeThreadId: string;
  domNodes: number;
  panes: SnapshotPane[];
  overlays: Array<{ name: string; kind: string }>;
}

interface PerfReport {
  runId: string;
  sampleMs: number;
  durationMs: number;
  samples: number;
  frontend: {
    v: number;
    durationMs: number;
    samples: number;
    meters: string[];
    frames: {
      frames: number;
      fps: number;
      p50Ms: number;
      maxMs: number;
      longFrames: number;
    };
    domNodes: { count: number; last: number };
  };
  /** `omitempty` on the Go side, so "no error" arrives as absent. */
  frontendError?: string;
  backend: {
    heapBytes: { count: number; min: number; max: number; mean: number };
    goroutines: { count: number; mean: number };
    processes: Array<{ pid: number; name: string; rssBytes: number }>;
  };
}

/** One seeded project with a two-item turn, opened in the UI. */
async function seedAndOpen(
  harness: HarnessApp,
  page: Page,
  title: string,
): Promise<{ threadId: string; itemIds: string[] }> {
  const seed = await harness.rpc<SeedResult>("HarnessSeed", {
    projects: [
      {
        name: "bridge-app",
        repo: {
          commits: [{ message: "init", files: { "README.md": "# Bridge\n" } }],
        },
        threads: [
          {
            title,
            turns: [
              {
                userText: "How do I sort an array in JS?",
                items: [
                  {
                    kind: "assistant_text",
                    summary:
                      "Use Array.prototype.sort with an explicit comparator.",
                  },
                ],
              },
            ],
          },
        ],
      },
    ],
  });
  const threadId = seed.projects[0]!.threadIds[0]!;
  const items = await harness.rpc<Array<{ id: string }>>("ListItems", threadId);
  await harness.open(page);
  await page.getByText(title).click();
  await expect(
    page.getByText("Use Array.prototype.sort with an explicit comparator."),
  ).toBeVisible();
  return { threadId, itemIds: items.map((item) => item.id) };
}

test("a viewport query answers with the rows the backend seeded", async ({
  harness,
  page,
}) => {
  const { threadId, itemIds } = await seedAndOpen(
    harness,
    page,
    "Bridge viewport",
  );

  const snapshot = await harness.rpc<Viewport>("HarnessUIQuery", {
    v: 1,
    kind: "viewport",
  });
  expect(snapshot.v).toBe(1);
  expect(snapshot.activeThreadId).toBe(threadId);
  expect(snapshot.domNodes).toBeGreaterThan(50);

  const pane = snapshot.panes.find(
    (candidate) => candidate.threadId === threadId,
  );
  expect(pane, "the seeded thread must be mounted in a pane").toBeDefined();
  // The layout item's kind, straight off [data-pane-kind]; a chat pane is
  // spelled 'thread' there (panes/PaneHost.svelte).
  expect(pane!.paneKind).toBe("thread");
  // A real layout engine, so the pane and its scroller have real boxes.
  expect(pane!.rect.w).toBeGreaterThan(100);
  expect(pane!.scroll).not.toBeNull();
  expect(pane!.scroll!.client).toBeGreaterThan(0);

  // The snapshot's ids are the store's ids: this is the property that
  // makes the snapshot a substitute for a screenshot rather than a second
  // rendering of one.
  expect(pane!.rows.map((row) => row.itemId)).toEqual(itemIds);
  const [userRow, assistantRow] = pane!.rows;
  expect(userRow).toMatchObject({
    kind: "user_text",
    role: "user",
    streaming: false,
  });
  expect(userRow!.textHead).toContain("How do I sort an array in JS?");
  expect(assistantRow).toMatchObject({
    kind: "assistant_text",
    role: "assistant",
  });
  expect(assistantRow!.textHead).toContain("Array.prototype.sort");
  expect(assistantRow!.rect.h).toBeGreaterThan(0);
  expect(assistantRow!.inViewport).toBe(true);

  // `settled` is a poll, not a wait: a finished render stops mutating and
  // the flag flips on the next query. Playwright's expect.poll re-drives
  // the RPC, which is what the flag is designed for.
  await expect
    .poll(
      async () =>
        (
          await harness.rpc<Viewport>("HarnessUIQuery", {
            v: 1,
            kind: "viewport",
          })
        ).settled,
      { message: "the page must settle once the render finishes" },
    )
    .toBe(true);
});

test("an element query measures the real DOM and reports misses as misses", async ({
  harness,
  page,
}) => {
  await seedAndOpen(harness, page, "Bridge element");

  const scroller = await harness.rpc<{
    count: number;
    first: {
      tag: string;
      rect: { w: number; h: number };
      visible: boolean;
      role: string;
    };
  }>("HarnessUIQuery", {
    v: 1,
    kind: "element",
    selector: '[data-testid="message-timeline-scroll"]',
  });
  expect(scroller.count).toBe(1);
  expect(scroller.first.tag).toBe("div");
  expect(scroller.first.visible).toBe(true);
  expect(scroller.first.role).toBe("log");
  expect(scroller.first.rect.h).toBeGreaterThan(0);

  const missing = await harness.rpc<{ count: number; first: null }>(
    "HarnessUIQuery",
    {
      v: 1,
      kind: "element",
      selector: ".no-such-thing",
    },
  );
  expect(missing.count).toBe(0);
  expect(missing.first).toBeNull();

  await expect(
    harness.rpc("HarnessUIQuery", { v: 1, kind: "element", selector: "[[" }),
  ).rejects.toThrow(/invalid selector/);
});

test("globals answer present, unavailable, or refused", async ({
  harness,
  page,
}) => {
  await seedAndOpen(harness, page, "Bridge globals");

  // Always installed (main.ts), and async by construction.
  const memory = await harness.rpc<{
    name: string;
    value: Record<string, unknown>;
  }>("HarnessUIQuery", { v: 1, kind: "globals", name: "__aoMemoryReport" });
  expect(memory.name).toBe("__aoMemoryReport");
  expect(memory.value).toBeTruthy();

  // Also always installed, and this is the build that has to prove it: a
  // bench and a profile end their measurement window on this probe, and a
  // harness binary ships with UI_TRACE unset. If it were gated the way the
  // diagnostics globals below are, every window would silently fall back
  // to ending at turn completion.
  const drain = await harness.rpc<{
    name: string;
    unavailable?: true;
    value?: { panes: number; draining: number; smoothers: number; boundaries: number };
  }>("HarnessUIQuery", { v: 1, kind: "globals", name: "__aoRevealDrain" });
  expect(drain.unavailable).toBeUndefined();
  expect(drain.value).toBeTruthy();
  // One pane is open (seedAndOpen), and it is not streaming.
  expect(drain.value!.panes).toBeGreaterThan(0);
  expect(drain.value!.draining).toBe(0);
  expect(drain.value!.smoothers).toBe(0);

  // `make harness` builds with UI_TRACE unset (UI_TRACE ?= $(DEBUG)), so
  // the trace api is genuinely absent here. That is an ANSWER, not a
  // fault — the caller has to be able to tell it from a bad name.
  const trace = await harness.rpc<{ unavailable?: true }>("HarnessUIQuery", {
    v: 1,
    kind: "globals",
    name: "uiTrace.recent",
    args: [5],
  });
  expect(trace.unavailable).toBe(true);

  await expect(
    harness.rpc("HarnessUIQuery", {
      v: 1,
      kind: "globals",
      name: "localStorage",
    }),
  ).rejects.toThrow(/unknown global/);
});

test("a perf run streams samples and stops with a two-sided report", async ({
  harness,
  page,
}) => {
  await seedAndOpen(harness, page, "Bridge perf");

  const status = await harness.rpc<{
    active: boolean;
    runId: string;
    sampleMs: number;
  }>("HarnessPerfStart", { sampleMs: 250, longFrameMs: 50 });
  expect(status.active).toBe(true);
  expect(status.sampleMs).toBe(250);

  // rAF only turns while the page renders, so give it something to do
  // rather than measuring an idle tab and asserting on the result.
  const scroller = page.getByTestId("message-timeline-scroll");
  interface PerfFrame {
    seq: number;
    runId: string;
    frontendError?: string;
    frontend?: { v: number; domNodes: number };
  }
  const frames: PerfFrame[] = [];
  for (let i = 0; i < 2; i += 1) {
    await scroller.hover();
    await page.mouse.wheel(0, i % 2 === 0 ? 200 : -200);
    frames.push(
      await harness.waitForEvent<PerfFrame>(
        "harness:perf",
        (ev) => ev.runId === status.runId,
      ),
    );
  }
  expect(frames.map((frame) => frame.seq)).toEqual([1, 2]);
  // `frontendError` is omitempty, so "the bridge answered" is its absence.
  expect(
    frames[0]!.frontendError,
    "the bridge must answer every collect tick",
  ).toBeUndefined();
  expect(frames[0]!.frontend?.v).toBe(1);

  const report = await harness.rpc<PerfReport>("HarnessPerfStop");
  expect(report.runId).toBe(status.runId);
  expect(report.samples).toBeGreaterThanOrEqual(2);
  expect(report.frontendError).toBeUndefined();

  // Frontend half: real frames, plausible cadence.
  expect(report.frontend.v).toBe(1);
  expect(report.frontend.meters).toContain("frames");
  expect(report.frontend.frames.frames).toBeGreaterThan(0);
  expect(report.frontend.frames.fps).toBeGreaterThan(0);
  expect(report.frontend.frames.fps).toBeLessThan(1000);
  expect(report.frontend.frames.maxMs).toBeGreaterThan(0);
  expect(report.frontend.domNodes.last).toBeGreaterThan(50);

  // Backend half: a Go process always has a heap and goroutines.
  expect(report.backend.heapBytes.count).toBe(report.samples);
  expect(report.backend.heapBytes.min).toBeGreaterThan(0);
  expect(report.backend.heapBytes.max).toBeGreaterThanOrEqual(
    report.backend.heapBytes.min,
  );
  expect(report.backend.goroutines.mean).toBeGreaterThan(0);

  // Stopping twice is an error, not an empty report.
  await expect(harness.rpc("HarnessPerfStop")).rejects.toThrow(/no perf run/);
});

test("HarnessReset disarms an active perf run", async ({ harness, page }) => {
  await seedAndOpen(harness, page, "Bridge perf reset");
  await harness.rpc("HarnessPerfStart", { sampleMs: 250 });
  expect(
    await harness.rpc<{ active: boolean }>("HarnessPerfStatus"),
  ).toMatchObject({
    active: true,
  });

  // The fixture's own per-test reset would do this too; driving it here is
  // what names the invariant. A run left armed would keep sampling into a
  // wiped database and keep the page's meters running past the reload.
  await harness.reset();
  expect(
    await harness.rpc<{ active: boolean }>("HarnessPerfStatus"),
  ).toMatchObject({
    active: false,
  });
});

// One test for both halves of the no-bridge story, because the timeout it
// depends on is the real 10s one.
test("a query with no page attached times out, and its late reply is dropped", async ({
  harness,
}) => {
  // Deliberately no page.goto: nothing is subscribed to harness:ui-query
  // except this client, which observes the directive without answering it.
  const pending = harness.rpc("HarnessUIQuery", { v: 1, kind: "viewport" });
  const directive = await harness.waitForEvent<{ id: string; pageId: string; spec: unknown }>(
    "harness:ui-query",
  );
  expect(directive.id).toMatch(/^uq-\d+$/);

  await expect(pending).rejects.toThrow(
    /no frontend attached or harness bridge inactive/,
  );

  // The waiter is gone, so these replies name ids nobody is holding. Both
  // must be accepted and dropped — the replying bridge did nothing wrong,
  // and erroring would turn a lost race into a red test in the frontend.
  // A refusal surfaces as a rejected RPC, which fails this test.
  await harness.rpc("HarnessUIQueryReply", directive.pageId, directive.id, { v: 1, panes: [] });
  await harness.rpc("HarnessUIQueryReply", directive.pageId, "uq-never-issued", {});
});

// The Go structs in cmd/ao-harness/ui_diff.go mirror
// frontend/src/lib/harness/snapshot.ts BY HAND. Nothing in either language
// checks that they still agree, and the failure mode is silent: a renamed
// TS field decodes to its Go zero value, so `ui diff` renders "nothing
// moved" about a page that moved — the one thing the command exists to
// catch. The unit suite cannot see this (it feeds the Go shapes to
// themselves), and neither can any assertion on `-o json`, which prints
// the server's own bytes rather than what Go made of them.
//
// So: run the real CLI against this page and read its TEXT rendering,
// which is drawn field by field from the decoded structs. A rename turns
// the ids blank, the rects into 0x0@0, and the viewport column into a
// dash, and every assertion below fails at once.
function harnessBinary(): string {
  const repoRoot = path.resolve(import.meta.dirname, "..", "..");
  const backend =
    process.env.AO_HARNESS_BIN ?? path.join(repoRoot, "bin", "agent-overflow");
  return path.join(path.dirname(backend), "ao-harness");
}

interface RenderedRow {
  rowIndex: number;
  itemId: string;
  kind: string;
  role: string;
  state: string;
  view: string;
  rect: { w: number; h: number; y: number };
}

/** Parses the row table `ui snapshot` prints (cmd_ui.go renderViewport). */
function parseSnapshotRows(stdout: string): RenderedRow[] {
  const lines = stdout.split("\n");
  const header = lines.findIndex(
    (line) => /\bITEM\b/.test(line) && /\bKIND\b/.test(line),
  );
  expect(header, `no row table in:\n${stdout}`).toBeGreaterThanOrEqual(0);

  const rows: RenderedRow[] = [];
  for (const line of lines.slice(header + 1)) {
    if (line.trim() === "") break;
    // tabwriter pads with spaces, so two or more separate columns. TEXT is
    // last and may contain single spaces, which is why the split is capped.
    const cells = line.trim().split(/\s{2,}/);
    if (cells.length < 7) continue;
    const [index, itemId, kind, role, state, view, rect] = cells as [
      string,
      string,
      string,
      string,
      string,
      string,
      string,
    ];
    const geometry = /^(\d+)x(\d+)@(-?\d+)$/.exec(rect);
    expect(
      geometry,
      `unparseable RECT cell ${JSON.stringify(rect)} in:\n${line}`,
    ).not.toBeNull();
    rows.push({
      rowIndex: Number(index),
      itemId,
      kind,
      role,
      state,
      view,
      rect: {
        w: Number(geometry![1]),
        h: Number(geometry![2]),
        y: Number(geometry![3]),
      },
    });
  }
  return rows;
}

test("the ao-harness CLI decodes the bridge snapshot, field for field", async ({
  harness,
  page,
}) => {
  const { threadId, itemIds } = await seedAndOpen(
    harness,
    page,
    "Bridge CLI decode",
  );

  // The bridge's own answer, to compare the CLI's decode against. Two
  // independent readings of the same page: if they disagree, the hand-kept
  // Go mirror has drifted from snapshot.ts.
  const snapshot = await harness.rpc<Viewport>("HarnessUIQuery", {
    v: 1,
    kind: "viewport",
  });
  const bridgePane = snapshot.panes.find(
    (candidate) => candidate.threadId === threadId,
  );
  expect(bridgePane, "the seeded thread must be mounted").toBeDefined();

  const { stdout } = await run(
    harnessBinary(),
    ["--instance", harness.bootstrap.dataRoot, "ui", "snapshot"],
    { timeout: 60_000 },
  );

  // The header line is drawn from the top-level fields.
  expect(stdout).toContain(`thread ${threadId}`);
  expect(stdout).toMatch(/dom=\d+/);
  expect(stdout).not.toMatch(/\bdom=0\b/);
  expect(stdout).toContain(`pane ${bridgePane!.paneId}`);
  expect(stdout).toContain(`mounted=${bridgePane!.mountedRows}`);
  // `scroll` is a nullable object on the TS side; a pane that has one must
  // print its numbers rather than the line vanishing.
  expect(stdout).toMatch(/scroll top=-?\d+ height=\d+ client=\d+/);

  const rows = parseSnapshotRows(stdout);
  expect(rows.map((row) => row.itemId)).toHaveLength(itemIds.length);

  rows.forEach((row, i) => {
    // itemId: non-empty, and the store's own id. The CLI truncates long
    // ids for the column, so the assertion is on the prefix.
    expect(row.itemId, "itemId decoded empty").not.toBe("");
    expect(itemIds[i]!.startsWith(row.itemId.replace(/…$/, ""))).toBe(true);
    // rowIndex: present and in document order, not all zero.
    expect(row.rowIndex).toBe(i);
    // rect: a real layout engine measured this, so w and h are positive.
    expect(
      row.rect.w,
      `row ${row.itemId} decoded a zero-width rect`,
    ).toBeGreaterThan(0);
    expect(
      row.rect.h,
      `row ${row.itemId} decoded a zero-height rect`,
    ).toBeGreaterThan(0);
    // kind/role/state: enumerated strings, never blank.
    expect(row.kind).not.toBe("");
    expect(row.role).not.toBe("");
    expect(row.state).not.toBe("");
    // inViewport: a decoded BOOLEAN, printed as one of two tokens. A
    // renamed field would read false and print the dash for every row.
    expect(["vis", "-"]).toContain(row.view);
  });

  // Both seeded rows are on screen, so the boolean's true branch is the one
  // exercised: `-` everywhere would pass the token check above on its own.
  expect(rows.every((row) => row.view === "vis")).toBe(true);
  expect(rows.map((row) => row.kind)).toEqual(
    bridgePane!.rows.map((row) => row.kind),
  );
  expect(rows.map((row) => row.role)).toEqual(
    bridgePane!.rows.map((row) => row.role),
  );
});
