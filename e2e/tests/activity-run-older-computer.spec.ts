// Activity runs of a paired computer's thread group by that computer's own
// rule, read from its hello. A computer standing in for v0.0.19
// (diagenv.HarnessOlderComputer) does not advertise
// activity-runs.agent-message-members.v1 and serves the absorbed rule
// whatever a request asks: an agent message after prose stands alone and
// the run starts at the tool after it, and one after prose following a
// run is no run at all. A current computer groups the same histories by
// the member rule. This computer's window opens each history there, seeded
// and as it arrives live, and again after a reload, on desktop and compact
// layouts: the rows group by the owning computer's rule, the history
// requests carry agentMessageMembers only to the computer that advertises
// it, the window settles within a bounded number of page and members
// calls, and no error toast is raised. A computer restarted as the older
// build and back while its thread is open here regroups that thread each
// time.
// Specs: docs/architecture/timeline-window-pages.md,
// docs/architecture/activity-runs.md.
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { devices, type Page } from '@playwright/test';
import { test, expect } from './base-test.js';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { pairOwnDevice } from './headless-pairing-helpers.js';
import { startTogether } from './launch-helpers.js';
import { instrument, methodNameById, type Surfaced } from './offhost-helpers.js';
import { awaitTurnCompleted, setScenario, threadToolsScenario } from './thread-tools-helpers.js';

test.describe.configure({ mode: 'serial' });

let home: HarnessApp;
let older: HarnessApp;
let current: HarnessApp;
const pairedIds: Array<[HarnessApp, string]> = [];

test.beforeAll(async () => {
  [home, older, current] = await startTogether(
    launchHarness(),
    launchHarness({ env: { AO_HARNESS_OLDER_COMPUTER: '1' } }),
    launchHarness(),
  );
  pairedIds.push([older, await pairOwnDevice(home, older, 'Old computer')]);
  pairedIds.push([current, await pairOwnDevice(home, current, 'New computer')]);
  // Expanded, so every member of a run is a row on screen.
  for (const app of [home, older, current]) await app.rpc('UpdateSettings', { activityRunDefault: 'expanded' });
});

test.afterAll(async () => {
  try {
    for (const [, id] of pairedIds) await home?.rpc('RemoveBackend', id, true);
  } finally {
    await older?.close();
    await current?.close();
    await home?.close();
  }
});

/** The history requests whose answers group a thread's rows. */
const PAGE_METHODS = new Set(['ListThreadSliceAround', 'ListItemsBeforeCursor', 'ListItemsAfterCursor', 'SyncThreadWindow']);
const MEMBERS_METHOD = 'ListActivityRunMembers';

interface ThreadWire {
  pages: number;
  members: number;
  /** History requests that carried `agentMessageMembers`. */
  withField: number;
  lastAt: number;
}

/** Counts, per thread, the history requests the page sends on any socket. */
function recordHistoryRequests(page: Page): Map<string, ThreadWire> {
  const byThread = new Map<string, ThreadWire>();
  page.on('websocket', (ws) => {
    ws.on('framesent', (frame) => {
      let parsed: { type?: string; method?: string; methodId?: number; params?: unknown[] };
      try {
        parsed = JSON.parse(String(frame.payload)) as typeof parsed;
      } catch {
        return;
      }
      if (parsed.type !== 'rpc') return;
      const name = parsed.method ?? (parsed.methodId ? methodNameById(parsed.methodId) : '');
      if (!PAGE_METHODS.has(name) && name !== MEMBERS_METHOD) return;
      const threadId = typeof parsed.params?.[0] === 'string' ? parsed.params[0] : '';
      const wire = byThread.get(threadId) ?? { pages: 0, members: 0, withField: 0, lastAt: 0 };
      if (name === MEMBERS_METHOD) wire.members += 1;
      else wire.pages += 1;
      if (JSON.stringify(parsed.params).includes('"agentMessageMembers"')) wire.withField += 1;
      wire.lastAt = Date.now();
      byThread.set(threadId, wire);
    });
  });
  return byThread;
}

/**
 * The open thread's rows in timeline order: `u` the person's message, `m`
 * an agent message, `p` prose, `t` a tool call, with each run's rows in
 * brackets.
 */
function timelineShape(page: Page, threadId: string): Promise<string> {
  return page.evaluate((id) => {
    const surface = document.querySelector(`[data-ui-surface="chat"][data-thread-id="${id}"]`);
    if (!surface) return '<not open>';
    const parts: string[] = [];
    let run: Element | null = null;
    let members: string[] = [];
    const closeRun = () => {
      if (run) parts.push(`[${members.join(' ')}]`);
      run = null;
      members = [];
    };
    for (const leaf of surface.querySelectorAll<HTMLElement>('[data-item-id][data-item-kind]')) {
      const kind = leaf.dataset.itemKind;
      const label =
        kind === 'tool_call' ? 't'
        : kind === 'assistant_text' ? 'p'
        : kind === 'user_text' ? (leaf.querySelector('[data-testid="agent-message-row"]') ? 'm' : 'u')
        : kind ?? '?';
      const container = leaf.closest('[data-testid="activity-run"]');
      if (container !== run) closeRun();
      if (container) {
        run = container;
        members.push(label);
      } else {
        parts.push(label);
      }
    }
    closeRun();
    return parts.join(' ');
  }, threadId);
}

const sidebarRow = (view: Page, id: string) => view.locator(`[data-sidebar-thread-id="${id}"]`);
const chatShowing = (view: Page, id: string) => view.locator(`[data-ui-surface="chat"][data-thread-id="${id}"]`);

async function openThread(view: Page, layout: Layout, id: string): Promise<void> {
  if (layout === 'compact' && (await view.locator('html').getAttribute('data-compact-screen')) === 'thread') {
    await view.getByTestId('compact-back').tap();
  }
  const row = sidebarRow(view, id);
  await expect(row).toBeVisible({ timeout: 60_000 });
  if (layout === 'compact') await row.tap();
  else await row.click();
  await expect(chatShowing(view, id)).toBeVisible();
}

/**
 * Waits for the thread's history requests to stop: none for a second.
 * A refusal loop never goes quiet, so this fails on one.
 */
async function historySettles(wire: Map<string, ThreadWire>, id: string): Promise<ThreadWire> {
  await expect
    .poll(() => {
      const seen = wire.get(id);
      return seen !== undefined && Date.now() - seen.lastAt > 1_000;
    }, { timeout: 20_000, message: `history requests for ${id} never went quiet: ${JSON.stringify(wire.get(id))}` })
    .toBe(true);
  return { ...wire.get(id)! };
}

type Layout = 'desktop' | 'compact';

interface Expected {
  computer: 'older' | 'current';
  /** Whether requests to that computer carry agentMessageMembers. */
  field: boolean;
  /** prose, agent message, tool. */
  proseMessageTool: string;
  /** tool, prose, agent message. */
  toolProseMessage: string;
  /** prose, then an agent message arriving live that runs a tool and answers. */
  liveMessageTool: string;
  /** tool, prose, then an agent message arriving live that answers. */
  liveMessage: string;
}

const CASES: Expected[] = [
  {
    computer: 'older',
    field: false,
    proseMessageTool: 'u p m [t]',
    toolProseMessage: 'u [t] p m',
    liveMessageTool: 'u p m [t] p',
    liveMessage: 'u [t] p m p',
  },
  {
    computer: 'current',
    field: true,
    proseMessageTool: 'u p [m t]',
    toolProseMessage: 'u [t] p [m]',
    liveMessageTool: 'u p [m t] p',
    liveMessage: 'u [t] p [m] p',
  },
];

/** A message another thread on the same computer sent this one. */
const agentTurn = (n: number) => ({
  userText: `Agent request ${n}.\n\n---\nAgent request from thread "Lead" (t-lead).\nIt is not waiting.`,
  userMeta: { origin: 'agent-thread', originThread: { threadId: 't-lead', title: 'Lead', kind: 'send' }, sendId: `thread-request:${n}` },
});
const toolItem = (summary: string) => ({ kind: 'tool_call', toolName: 'Bash', summary });
const proseItem = (summary: string) => ({ kind: 'assistant_text', summary });

interface SeededProject {
  projectId: string;
  path: string;
  threadIds: string[];
}

async function seedThreads(app: HarnessApp, tag: string): Promise<{
  seeded: SeededProject;
  liveTool: SeededProject;
  liveText: SeededProject;
  senders: SeededProject;
}> {
  const result = await app.rpc<{ projects: SeededProject[] }>('HarnessSeed', {
    projects: [
      {
        name: `${tag}-seeded`,
        repo: {},
        threads: [
          {
            title: `${tag} prose message tool`,
            provider: 'claude',
            turns: [
              { userText: 'Look around.', items: [proseItem('Looked around.')] },
              { ...agentTurn(1), items: [toolItem('ls')] },
            ],
          },
          {
            title: `${tag} tool prose message`,
            provider: 'claude',
            turns: [
              { userText: 'List it.', items: [toolItem('ls'), proseItem('Listed it.')] },
              { ...agentTurn(2), items: [] },
            ],
          },
        ],
      },
      {
        name: `${tag}-live-tool`,
        repo: {},
        threads: [{ title: `${tag} live message tool`, provider: 'claude', turns: [{ userText: 'Look around.', items: [proseItem('Looked around.')] }] }],
      },
      {
        name: `${tag}-live-text`,
        repo: {},
        threads: [{ title: `${tag} live message`, provider: 'claude', turns: [{ userText: 'List it.', items: [toolItem('ls'), proseItem('Listed it.')] }] }],
      },
      {
        name: `${tag}-senders`,
        repo: {},
        threads: [{ title: `${tag} sender`, provider: 'claude' }],
      },
    ],
  });
  const [seeded, liveTool, liveText, senders] = result.projects;
  return { seeded, liveTool, liveText, senders };
}

/** Has the sender thread `thread_send` each target, one per turn. */
async function scriptLiveDelivery(app: HarnessApp, threads: Awaited<ReturnType<typeof seedThreads>>, tag: string): Promise<void> {
  const targets = [threads.liveTool.threadIds[0], threads.liveText.threadIds[0]];
  await setScenario(app, threads.senders.path, threadToolsScenario({
    name: `${tag}-sender`,
    provider: 'claude',
    turns: targets.map((threadId) => ({
      steps: [{ call: { tool: 'thread_send', args: { thread_id: threadId, message: 'Check the build.', wait_seconds: 0 } } }],
      text: 'Sent.',
    })),
    afterTurns: 'silent',
  }));
  await setScenario(app, threads.liveTool.path, threadToolsScenario({
    name: `${tag}-live-tool`,
    provider: 'claude',
    turns: [{ steps: [{ call: { tool: 'thread_options', args: {} } }], text: 'Checked the build.' }],
    afterTurns: 'silent',
  }));
  await setScenario(app, threads.liveText.path, threadToolsScenario({
    name: `${tag}-live-text`,
    provider: 'claude',
    turns: [{ text: 'Checked the build.' }],
    afterTurns: 'silent',
  }));
}

/** Asserts the open thread's shape, that it settled, and what its requests carried. */
async function expectSettledShape(
  view: Page,
  wire: Map<string, ThreadWire>,
  id: string,
  shape: string,
  expected: Expected,
): Promise<void> {
  await expect.poll(() => timelineShape(view, id), { timeout: 30_000 }).toBe(shape);
  const seen = await historySettles(wire, id);
  // Each open reads one page, and these runs ship whole, so no members
  // read is due; one more of each allows the bounded recovery of a refused
  // read. A reload loop runs past both.
  expect(seen.pages, JSON.stringify(seen)).toBeLessThanOrEqual(2);
  expect(seen.members, JSON.stringify(seen)).toBeLessThanOrEqual(1);
  expect(seen.withField, JSON.stringify(seen)).toBe(expected.field ? seen.pages + seen.members : 0);
  expect(await timelineShape(view, id)).toBe(shape);
}

async function expectNoErrorToast(view: Page, surfaced: Surfaced): Promise<void> {
  await expect(view.getByTestId('toast')).toHaveCount(0);
  expect(surfaced.errorToasts).toEqual([]);
}

for (const layout of ['desktop', 'compact'] as const) {
  for (const expected of CASES) {
    test(`${layout}: a thread on the ${expected.computer} computer groups by that computer's rule`, async ({ browser }) => {
      test.setTimeout(240_000);
      const app = expected.computer === 'older' ? older : current;
      const tag = `${layout}-${expected.computer}`;
      const threads = await seedThreads(app, tag);
      await scriptLiveDelivery(app, threads, tag);
      const context = await browser.newContext(layout === 'compact' ? { ...devices['Pixel 7'] } : { viewport: { width: 1400, height: 1000 } });
      try {
        const view = await context.newPage();
        view.setDefaultTimeout(30_000);
        const surfaced = await instrument(view);
        const wire = recordHistoryRequests(view);
        await home.open(view);

        // Seeded history, opened and then reloaded.
        const [proseMessageTool, toolProseMessage] = threads.seeded.threadIds;
        for (const [id, shape] of [[proseMessageTool, expected.proseMessageTool], [toolProseMessage, expected.toolProseMessage]] as const) {
          await openThread(view, layout, id);
          await expectSettledShape(view, wire, id, shape, expected);
          wire.clear();
          await view.reload();
          await openThread(view, layout, id);
          await expectSettledShape(view, wire, id, shape, expected);
          await expectNoErrorToast(view, surfaced);
        }

        // An agent message arriving live in an open thread, then reloaded.
        const sender = threads.senders.threadIds[0];
        await app.rpc('StartSession', sender);
        for (const [project, shape] of [[threads.liveTool, expected.liveMessageTool], [threads.liveText, expected.liveMessage]] as const) {
          const id = project.threadIds[0];
          await openThread(view, layout, id);
          await historySettles(wire, id);
          const done = awaitTurnCompleted(app, id);
          await app.rpc('SendMessage', sender, `send to ${id}`, null);
          await done;
          await expectSettledShape(view, wire, id, shape, expected);
          wire.clear();
          await view.reload();
          await openThread(view, layout, id);
          await expectSettledShape(view, wire, id, shape, expected);
          await expectNoErrorToast(view, surfaced);
        }
      } finally {
        await context.close();
      }
    });
  }
}

test('desktop: a computer restarted across the capability regroups the open thread', async ({ browser }) => {
  test.setTimeout(300_000);
  const root = await mkdtemp(join(tmpdir(), 'ao-run-rule-restart-'));
  const dataDir = join(root, 'state');
  let restarted = await launchHarness({ dataDir });
  let pairedId = '';
  const context = await browser.newContext({ viewport: { width: 1400, height: 1000 } });
  try {
    pairedId = await pairOwnDevice(home, restarted, 'Restarted computer');
    await restarted.rpc('UpdateSettings', { activityRunDefault: 'expanded' });
    const threads = await seedThreads(restarted, 'restart');
    const id = threads.seeded.threadIds[0];
    const view = await context.newPage();
    view.setDefaultTimeout(30_000);
    const surfaced = await instrument(view);
    const wire = recordHistoryRequests(view);
    await home.open(view);
    await openThread(view, 'desktop', id);
    const [older, newer] = CASES;
    await expectSettledShape(view, wire, id, newer.proseMessageTool, newer);

    // The same computer comes back as a build before the capability, then
    // as this one, while its thread stays open here.
    for (const expected of [older, newer]) {
      expect(await restarted.stop()).toBe(true);
      wire.clear();
      restarted = await launchHarness({ dataDir, env: expected === older ? { AO_HARNESS_OLDER_COMPUTER: '1' } : {} });
      await home.rpc('RepairBackendAddress', pairedId, `https://127.0.0.1:${restarted.bootstrap.port}`);
      await expect.poll(() => timelineShape(view, id), { timeout: 60_000 }).toBe(expected.proseMessageTool);
      await expectSettledShape(view, wire, id, expected.proseMessageTool, expected);
    }
    await expectNoErrorToast(view, surfaced);
  } finally {
    await context.close();
    if (pairedId) await home.rpc('RemoveBackend', pairedId, true);
    await restarted.close();
    await rm(root, { recursive: true, force: true });
  }
});
