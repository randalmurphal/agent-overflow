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
import type { Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import { test, expect, type SeedResult } from './fixtures.js';

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
  scroll: { top: number; height: number; client: number; atBottom: boolean } | null;
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
    frames: { frames: number; fps: number; p50Ms: number; maxMs: number; longFrames: number };
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
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'bridge-app',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Bridge\n' } }] },
        threads: [
          {
            title,
            turns: [
              {
                userText: 'How do I sort an array in JS?',
                items: [
                  {
                    kind: 'assistant_text',
                    summary: 'Use Array.prototype.sort with an explicit comparator.',
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
  const items = await harness.rpc<Array<{ id: string }>>('ListItems', threadId);
  await page.goto(harness.url);
  await page.getByText(title).click();
  await expect(page.getByText('Use Array.prototype.sort with an explicit comparator.')).toBeVisible();
  return { threadId, itemIds: items.map((item) => item.id) };
}

test('a viewport query answers with the rows the backend seeded', async ({ harness, page }) => {
  const { threadId, itemIds } = await seedAndOpen(harness, page, 'Bridge viewport');

  const snapshot = await harness.rpc<Viewport>('HarnessUIQuery', { v: 1, kind: 'viewport' });
  expect(snapshot.v).toBe(1);
  expect(snapshot.activeThreadId).toBe(threadId);
  expect(snapshot.domNodes).toBeGreaterThan(50);

  const pane = snapshot.panes.find((candidate) => candidate.threadId === threadId);
  expect(pane, 'the seeded thread must be mounted in a pane').toBeDefined();
  // The layout item's kind, straight off [data-pane-kind]; a chat pane is
  // spelled 'thread' there (panes/PaneHost.svelte).
  expect(pane!.paneKind).toBe('thread');
  // A real layout engine, so the pane and its scroller have real boxes.
  expect(pane!.rect.w).toBeGreaterThan(100);
  expect(pane!.scroll).not.toBeNull();
  expect(pane!.scroll!.client).toBeGreaterThan(0);

  // The snapshot's ids are the store's ids: this is the property that
  // makes the snapshot a substitute for a screenshot rather than a second
  // rendering of one.
  expect(pane!.rows.map((row) => row.itemId)).toEqual(itemIds);
  const [userRow, assistantRow] = pane!.rows;
  expect(userRow).toMatchObject({ kind: 'user_text', role: 'user', streaming: false });
  expect(userRow!.textHead).toContain('How do I sort an array in JS?');
  expect(assistantRow).toMatchObject({ kind: 'assistant_text', role: 'assistant' });
  expect(assistantRow!.textHead).toContain('Array.prototype.sort');
  expect(assistantRow!.rect.h).toBeGreaterThan(0);
  expect(assistantRow!.inViewport).toBe(true);

  // `settled` is a poll, not a wait: a finished render stops mutating and
  // the flag flips on the next query. Playwright's expect.poll re-drives
  // the RPC, which is what the flag is designed for.
  await expect
    .poll(
      async () => (await harness.rpc<Viewport>('HarnessUIQuery', { v: 1, kind: 'viewport' })).settled,
      { message: 'the page must settle once the render finishes' },
    )
    .toBe(true);
});

test('an element query measures the real DOM and reports misses as misses', async ({
  harness,
  page,
}) => {
  await seedAndOpen(harness, page, 'Bridge element');

  const scroller = await harness.rpc<{
    count: number;
    first: { tag: string; rect: { w: number; h: number }; visible: boolean; role: string };
  }>('HarnessUIQuery', {
    v: 1,
    kind: 'element',
    selector: '[data-testid="message-timeline-scroll"]',
  });
  expect(scroller.count).toBe(1);
  expect(scroller.first.tag).toBe('div');
  expect(scroller.first.visible).toBe(true);
  expect(scroller.first.role).toBe('log');
  expect(scroller.first.rect.h).toBeGreaterThan(0);

  const missing = await harness.rpc<{ count: number; first: null }>('HarnessUIQuery', {
    v: 1,
    kind: 'element',
    selector: '.no-such-thing',
  });
  expect(missing.count).toBe(0);
  expect(missing.first).toBeNull();

  await expect(
    harness.rpc('HarnessUIQuery', { v: 1, kind: 'element', selector: '[[' }),
  ).rejects.toThrow(/invalid selector/);
});

test('globals answer present, unavailable, or refused', async ({ harness, page }) => {
  await seedAndOpen(harness, page, 'Bridge globals');

  // Always installed (main.ts), and async by construction.
  const memory = await harness.rpc<{ name: string; value: Record<string, unknown> }>(
    'HarnessUIQuery',
    { v: 1, kind: 'globals', name: '__aoMemoryReport' },
  );
  expect(memory.name).toBe('__aoMemoryReport');
  expect(memory.value).toBeTruthy();

  // `make harness` builds with UI_TRACE unset (UI_TRACE ?= $(DEBUG)), so
  // the trace api is genuinely absent here. That is an ANSWER, not a
  // fault — the caller has to be able to tell it from a bad name.
  const trace = await harness.rpc<{ unavailable?: true }>('HarnessUIQuery', {
    v: 1,
    kind: 'globals',
    name: 'uiTrace.recent',
    args: [5],
  });
  expect(trace.unavailable).toBe(true);

  await expect(
    harness.rpc('HarnessUIQuery', { v: 1, kind: 'globals', name: 'localStorage' }),
  ).rejects.toThrow(/unknown global/);
});

test('a perf run streams samples and stops with a two-sided report', async ({ harness, page }) => {
  await seedAndOpen(harness, page, 'Bridge perf');

  const status = await harness.rpc<{ active: boolean; runId: string; sampleMs: number }>(
    'HarnessPerfStart',
    { sampleMs: 250, longFrameMs: 50 },
  );
  expect(status.active).toBe(true);
  expect(status.sampleMs).toBe(250);

  // rAF only turns while the page renders, so give it something to do
  // rather than measuring an idle tab and asserting on the result.
  const scroller = page.getByTestId('message-timeline-scroll');
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
      await harness.waitForEvent<PerfFrame>('harness:perf', (ev) => ev.runId === status.runId),
    );
  }
  expect(frames.map((frame) => frame.seq)).toEqual([1, 2]);
  // `frontendError` is omitempty, so "the bridge answered" is its absence.
  expect(frames[0]!.frontendError, 'the bridge must answer every collect tick').toBeUndefined();
  expect(frames[0]!.frontend?.v).toBe(1);

  const report = await harness.rpc<PerfReport>('HarnessPerfStop');
  expect(report.runId).toBe(status.runId);
  expect(report.samples).toBeGreaterThanOrEqual(2);
  expect(report.frontendError).toBeUndefined();

  // Frontend half: real frames, plausible cadence.
  expect(report.frontend.v).toBe(1);
  expect(report.frontend.meters).toContain('frames');
  expect(report.frontend.frames.frames).toBeGreaterThan(0);
  expect(report.frontend.frames.fps).toBeGreaterThan(0);
  expect(report.frontend.frames.fps).toBeLessThan(1000);
  expect(report.frontend.frames.maxMs).toBeGreaterThan(0);
  expect(report.frontend.domNodes.last).toBeGreaterThan(50);

  // Backend half: a Go process always has a heap and goroutines.
  expect(report.backend.heapBytes.count).toBe(report.samples);
  expect(report.backend.heapBytes.min).toBeGreaterThan(0);
  expect(report.backend.heapBytes.max).toBeGreaterThanOrEqual(report.backend.heapBytes.min);
  expect(report.backend.goroutines.mean).toBeGreaterThan(0);

  // Stopping twice is an error, not an empty report.
  await expect(harness.rpc('HarnessPerfStop')).rejects.toThrow(/no perf run/);
});

test('HarnessReset disarms an active perf run', async ({ harness, page }) => {
  await seedAndOpen(harness, page, 'Bridge perf reset');
  await harness.rpc('HarnessPerfStart', { sampleMs: 250 });
  expect(await harness.rpc<{ active: boolean }>('HarnessPerfStatus')).toMatchObject({
    active: true,
  });

  // The fixture's own per-test reset would do this too; driving it here is
  // what names the invariant. A run left armed would keep sampling into a
  // wiped database and keep the page's meters running past the reload.
  await harness.reset();
  expect(await harness.rpc<{ active: boolean }>('HarnessPerfStatus')).toMatchObject({
    active: false,
  });
});

// One test for both halves of the no-bridge story, because the timeout it
// depends on is the real 10s one.
test('a query with no page attached times out, and its late reply is dropped', async ({
  harness,
}) => {
  // Deliberately no page.goto: nothing is subscribed to harness:ui-query
  // except this client, which observes the directive without answering it.
  const pending = harness.rpc('HarnessUIQuery', { v: 1, kind: 'viewport' });
  const directive = await harness.waitForEvent<{ id: string; spec: unknown }>('harness:ui-query');
  expect(directive.id).toMatch(/^uq-\d+$/);

  await expect(pending).rejects.toThrow(/no frontend attached or harness bridge inactive/);

  // The waiter is gone, so these replies name ids nobody is holding. Both
  // must be accepted and dropped — the replying bridge did nothing wrong,
  // and erroring would turn a lost race into a red test in the frontend.
  // A refusal surfaces as a rejected RPC, which fails this test.
  await harness.rpc('HarnessUIQueryReply', directive.id, { v: 1, panes: [] });
  await harness.rpc('HarnessUIQueryReply', 'uq-never-issued', {});
});
