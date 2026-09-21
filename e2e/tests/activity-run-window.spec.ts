// Activity runs larger than a page: the server ships a run's mount window
// and a stub for the rest, the boundary counts the unshipped members and
// fetches them on demand, the header counts the whole run, a reload
// restores the same picture from the replica, and the message rail jumps
// through a run whose members outnumber a page's rows.
import { test, expect, type SeedResult } from './fixtures.js';

const RUN_MEMBERS = 500;
// activityRunWindowRows default and ACTIVITY_RUN_CHUNK_ROWS, the two
// numbers the boundary arithmetic below is written against.
const WINDOW_ROWS = 30;
const CHUNK_ROWS = 25;
// SLICE_AROUND_ITEM_BUDGET: the row budget the pane asks for when it opens.
const TAIL_ITEM_BUDGET = 200;

interface TailPage {
  items: Array<{ kind: string; summary: string }>;
  runs: Array<{ memberCount: number; unshippedBefore: number }>;
  oldestTurnIndex: number;
}

function heavyRun(count = RUN_MEMBERS) {
  return Array.from({ length: count }, (_, i) => ({
    kind: 'tool_call',
    toolName: 'Bash',
    summary: `run call ${i}`,
  }));
}

async function expandRun(page: import('@playwright/test').Page) {
  const run = page.getByTestId('activity-run');
  await expect(run).toHaveCount(1);
  if ((await run.getAttribute('data-collapsed')) === 'true') {
    await page.getByTestId('activity-run-header').click();
  }
  await expect(run).toHaveAttribute('data-collapsed', 'false');
}

test('a run larger than its window ships its tail, counts the rest, and fetches a chunk on demand', async ({ harness, page }) => {
  let forceRevalidation = false;
  let retainedReads = 0;
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const server = socket.connectToServer();
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (forceRevalidation && frame.type === 'rpc' && frame.methodId === 3841902986) {
        // Force a real page response over the cached expanded window.
        frame.params[1] = { ...frame.params[1], haveEpoch: -1, haveRev: -1, haveWindow: null };
        server.send(JSON.stringify(frame));
      } else {
        if (forceRevalidation && frame.type === 'rpc' && frame.methodId === 162135710) retainedReads += 1;
        server.send(message);
      }
    });
    server.onMessage(message => socket.send(message));
  });

  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'run-window',
      repo: {},
      threads: [{
        title: 'Long run',
        turns: [{
          userText: 'Do a lot',
          items: [...heavyRun(), { kind: 'assistant_text', summary: 'All done' }],
        }],
      }, { title: 'Away', turns: [{ userText: 'Another thread', items: [{ kind: 'assistant_text', summary: 'Away response' }] }] }],
    }],
  });
  await harness.open(page);
  await page.getByText('Long run', { exact: true }).click();
  await expect(page.getByText('All done', { exact: true })).toBeVisible();

  // The header counts every member, shipped or not.
  await expect(page.getByTestId('activity-run-header-counts')).toContainText(`${RUN_MEMBERS} Bash`);
  await expandRun(page);

  const rows = page.getByTestId('command-output-row');
  const earlier = page.getByTestId('activity-run-earlier');
  await expect(rows).toHaveCount(WINDOW_ROWS);
  await expect(rows.last()).toContainText(`run call ${RUN_MEMBERS - 1}`);
  await expect(earlier).toContainText(`${RUN_MEMBERS - WINDOW_ROWS} earlier`);
  await expect(page.getByTestId('activity-run-later')).toHaveCount(0);

  // Nothing older is loaded, so the click is a server round trip: the
  // chunk lands above the window and the boundary counts down by it.
  await earlier.click();
  await expect(rows).toHaveCount(WINDOW_ROWS + CHUNK_ROWS);
  await expect(rows.first()).toContainText(`run call ${RUN_MEMBERS - WINDOW_ROWS - CHUNK_ROWS}`);
  await expect(earlier).toContainText(`${RUN_MEMBERS - WINDOW_ROWS - CHUNK_ROWS} earlier`);

  await page.getByTestId('thread-row').getByText('Away', { exact: true }).click();
  await expect(page.getByText('Away response', { exact: true })).toBeVisible();
  forceRevalidation = true;
  await page.getByTestId('thread-row').getByText('Long run', { exact: true }).click();
  await expect.poll(() => retainedReads).toBeGreaterThan(0);
  await expandRun(page);
  await expect(page.getByTestId('activity-run-earlier')).toContainText(`${RUN_MEMBERS - WINDOW_ROWS - CHUNK_ROWS} earlier`);
  await expect(page.getByTestId('command-output-row')).toHaveCount(WINDOW_ROWS + CHUNK_ROWS);

  // A cold reopen must still account for the whole run through its stub.
  forceRevalidation = false;
  await page.reload();
  await page.getByTestId('thread-row').getByText('Long run', { exact: true }).click();
  await expect(page.getByText('All done', { exact: true })).toBeVisible();
  await expect(page.getByTestId('activity-run-header-counts')).toContainText(`${RUN_MEMBERS} Bash`);
  await expandRun(page);
  await expect(page.getByTestId('activity-run-earlier')).toContainText(/\d+ earlier/);
  await expect(page.getByTestId('command-output-row').last()).toContainText(`run call ${RUN_MEMBERS - 1}`);
});

// The tail page's oldest unit is the run itself: the walk takes whole units
// until the row budget is met, and here the prose after the run leaves
// room for exactly one more unit. The first user message is older than the
// run's first member and is not in the window. Jumping to it must not ask
// the run for it: the run's stub counts unshipped members before its window,
// but the message is not one of them.
test('jump to first from a window that opens on a run does not ask the run for the message', async ({ harness, page }) => {
  const turns = [
    {
      userText: 'Rail question 0',
      items: [...heavyRun(), { kind: 'assistant_text', summary: 'Rail answer 0' }],
    },
    ...Array.from({ length: 90 }, (_, i) => ({
      userText: `Rail question ${i + 1}`,
      items: [{ kind: 'assistant_text', summary: `Rail answer ${i + 1}` }],
    })),
  ];
  const seeded = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'run-head', repo: {}, threads: [{ title: 'Run at the head', turns }] }],
  });
  // The page the pane opens on: 181 prose rows after the run leave room for
  // one more whole unit, so the run is the page's oldest unit and the
  // message that started its turn is the first row not in it.
  const tail = await harness.rpc<TailPage>(
    'ListThreadSliceAround', seeded.projects[0].threadIds[0], '', TAIL_ITEM_BUDGET, {},
  );
  expect(tail.runs.map((run) => run.memberCount)).toEqual([RUN_MEMBERS]);
  expect(tail.runs[0].unshippedBefore).toBe(RUN_MEMBERS - WINDOW_ROWS);
  expect(tail.oldestTurnIndex).toBe(0);
  expect(tail.items.some((item) => item.summary === 'Rail question 0')).toBe(false);

  await harness.open(page);
  await page.getByText('Run at the head', { exact: true }).click();

  const rail = page.getByTestId('message-nav-rail');
  const scroller = page.getByTestId('message-timeline-scroll');
  await expect(scroller.getByText('Rail question 90', { exact: true })).toBeInViewport();
  await expect(scroller.getByText('Rail question 0', { exact: true })).toHaveCount(0);

  for (let trip = 0; trip < 2; trip++) {
    await rail.getByRole('button', { name: 'Jump to first message', exact: true }).click();
    await expect(scroller.getByText('Rail question 0', { exact: true })).toBeInViewport();
    await expect(page.getByText('Failed to load activity')).toHaveCount(0);
    await expect(page.getByText('Activity moved while it was loading')).toHaveCount(0);
    await rail.getByRole('button', { name: 'Jump to latest message', exact: true }).click();
    await expect(scroller.getByText('Rail question 90', { exact: true })).toBeInViewport();
    await expect(page.getByText('Failed to load activity')).toHaveCount(0);
  }
});

test('the message rail jumps through a run whose members outnumber a page', async ({ harness, page }) => {
  const turns = [
    { userText: 'Rail question 0', items: heavyRun() },
    ...Array.from({ length: 40 }, (_, i) => ({
      userText: `Rail question ${i + 1}`,
      items: [{ kind: 'assistant_text', summary: `Rail answer ${i + 1}\n\n${'A detailed answer. '.repeat(40)}` }],
    })),
  ];
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'run-rail', repo: {}, threads: [{ title: 'Run head', turns }] }],
  });
  await harness.open(page);
  await page.getByText('Run head', { exact: true }).click();

  const rail = page.getByTestId('message-nav-rail');
  const scroller = page.getByTestId('message-timeline-scroll');
  await expect(scroller.getByText('Rail question 40', { exact: true })).toBeInViewport();

  for (let trip = 0; trip < 2; trip++) {
    await rail.getByRole('button', { name: 'Jump to first message', exact: true }).click();
    await expect(scroller.getByText('Rail question 0', { exact: true })).toBeInViewport();
    await expect(page.getByText('Message is no longer in this thread')).toHaveCount(0);
    // The run below the first message is the whole run, not the members
    // that fit a page.
    await expect(page.getByTestId('activity-run-header-counts').first()).toContainText(`${RUN_MEMBERS} Bash`);
    await rail.getByRole('button', { name: 'Jump to latest message', exact: true }).click();
    await expect(scroller.getByText('Rail question 40', { exact: true })).toBeInViewport();
  }
});

test('a run window starting with a notification stays wholly inside its collapsed run', async ({ harness, page }) => {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'notification-run-window', repo: {}, threads: [{
      title: 'Notification at the page boundary',
      turns: [{ userText: 'Work in the background', items: [
        ...heavyRun(50),
        { kind: 'notification', toolName: 'Agent', summary: 'Completed agent report at the page boundary' },
        ...heavyRun(29),
        { kind: 'assistant_text', summary: 'The parent continued.' },
      ] }],
    }] }],
  });
  await harness.open(page);
  await page.getByText('Notification at the page boundary', { exact: true }).click();
  const run = page.getByTestId('activity-run');
  await expect(run).toHaveCount(1);
  await expect(run).toHaveAttribute('data-collapsed', 'true');
  await expect(page.getByText('Completed agent report at the page boundary', { exact: true })).toHaveCount(0);
  await expandRun(page);
  await expect(run.getByText('Completed agent report at the page boundary', { exact: true })).toHaveCount(1);
  await expect(page.getByTestId('activity-run-earlier')).toContainText('50 earlier');
  await page.getByTestId('activity-run-earlier').click();
  await expect(page.getByTestId('activity-run-earlier')).toContainText('25 earlier');
  await expect(page.getByText('Failed to refresh activity')).toHaveCount(0);
});
