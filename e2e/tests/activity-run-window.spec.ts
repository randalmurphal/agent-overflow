// Activity runs larger than a page: the server ships a run's mount window
// and a stub for the rest, the boundary counts the unshipped members and
// fetches them on demand, the header counts the whole run, a reload
// restores the same picture from the replica, the message rail jumps
// through a run whose members outnumber a page's rows, a live run grows
// at its tail without the pane reading the run again, a live run a jump
// left holding its head counts what it gains until the reader returns to
// its tail, and a live run a switch-back sync ships mid-run counts the
// members it gained during the sync.
import { test, expect, type SeedResult } from './fixtures.js';
import { advance, claudeScenario, emit, startMock, textLines, toolResultLine, toolUseLine, waitForGate } from './agent-visibility-helpers.js';

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

// A pane that opens on a live run larger than its window holds a stub for
// the rest. Members appended after the run's newest member change nothing
// that stub counts, so the pane extends the run itself: no members read
// per append, and the window it holds still verifies against the server.
test('a live run larger than its window grows at its tail without reading the run again', async ({ harness, page }) => {
  const SYNC_THREAD_WINDOW = 3841902986;
  const LIST_ACTIVITY_RUN_MEMBERS = 1602023272;
  let memberReads = 0;
  const syncs = new Map<string, { threadId: string; heldWindow: boolean; status?: string }>();
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const server = socket.connectToServer();
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.methodId === LIST_ACTIVITY_RUN_MEMBERS) memberReads += 1;
      if (frame.type === 'rpc' && frame.methodId === SYNC_THREAD_WINDOW) {
        syncs.set(String(frame.id), {
          threadId: String(frame.params?.[0]),
          heldWindow: Boolean(frame.params?.[1]?.haveWindow),
        });
      }
      server.send(message);
    });
    server.onMessage(message => {
      const text = String(message);
      if (text.startsWith('{"type":"rpc"')) {
        const frame = JSON.parse(text);
        const sync = syncs.get(String(frame.id));
        if (sync) sync.status = frame.result?.status ?? 'error';
      }
      socket.send(message);
    });
  });
  const batch = (start: number, count: number) => Array.from({ length: count }, (_, j) => {
    const i = start + j;
    return [toolUseLine(`message-${i}`, `tool-${i}`, 'Bash', { command: `echo live-run-${i}` }), toolResultLine(`tool-${i}`, 'done')];
  }).flat();
  await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('live-run-window', [
    emit([...textLines('lead', 'Starting a long sweep.'), ...batch(0, 60)]),
    { waitSignal: { name: 'grow' } }, emit(batch(60, 20)),
    { waitSignal: { name: 'hold' } },
  ]) });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: 'live-run-window', repo: {}, threads: [
    { title: 'Live run', provider: 'claude', turns: [{ userText: 'Earlier', items: [{ kind: 'assistant_text', summary: 'Earlier answer.' }] }] },
    { title: 'Away', turns: [{ userText: 'Other', items: [{ kind: 'assistant_text', summary: 'Away answer.' }] }] },
  ] }] });
  const threadId = seed.projects[0].threadIds[0];
  await harness.open(page);
  const openThread = (title: string) => page.getByTestId('thread-row').getByText(title, { exact: true }).click();
  await openThread('Away');
  await expect(page.getByText('Away answer.', { exact: true })).toBeVisible();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'Start', null);
  await waitForGate(harness, 'grow');

  // Opened mid-run: the page ships the run's window and a stub for the rest.
  await openThread('Live run');
  const run = page.getByTestId('activity-run').last();
  await expect(run.getByTestId('activity-run-header-counts')).toContainText('60 Bash');
  if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
  await expect(run.getByTestId('activity-run-earlier')).toContainText(`${60 - WINDOW_ROWS} earlier`);
  memberReads = 0;

  await advance(harness, mockId, 'grow');
  await waitForGate(harness, 'hold');
  await expect(run.getByTestId('activity-run-header-counts')).toContainText('80 Bash');
  await expect(run.getByTestId('command-output-row').last()).toContainText('echo live-run-79');
  await expect(run.getByTestId('activity-run-earlier')).toContainText(`${80 - WINDOW_ROWS} earlier`);
  await expect(run.getByTestId('activity-run-later')).toHaveCount(0);

  // The server re-derives the held window, stub included, and finds it
  // exact. The switch caches the window only while every run record is
  // clean, so a record the grow left awaiting a stub refresh could not
  // reopen on a fresh held window, and one refreshed before the switch
  // would have counted a members read.
  syncs.clear();
  await openThread('Away');
  await expect(page.getByText('Away answer.', { exact: true })).toBeVisible();
  await openThread('Live run');
  // Opening Away syncs its own held window too: read this thread's answer.
  await expect.poll(() => [...syncs.values()]
    .find(sync => sync.threadId === threadId && sync.status !== undefined && sync.heldWindow)?.status).toBe('fresh');
  await expect(run.getByTestId('activity-run-header-counts')).toContainText('80 Bash');
  expect(memberReads).toBe(0);

  // The counter sees members reads: the boundary mounts the loaded rows
  // above the window first, then reads the run for the older ones.
  if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
  const earlier = run.getByTestId('activity-run-earlier');
  await expect(earlier).toContainText(`${80 - WINDOW_ROWS} earlier`);
  await earlier.click();
  await expect(earlier).toContainText(`${60 - WINDOW_ROWS} earlier`);
  expect(memberReads).toBe(0);
  await earlier.click();
  await expect.poll(() => memberReads).toBe(1);
  await expect(earlier).toContainText(`${60 - WINDOW_ROWS - CHUNK_ROWS} earlier`);
});

// A jump to the head of a live run leaves the pane holding its first
// members while the stub counts the rest after them. Members the run gains
// past those have no place in the pane, so the run counts them, and the
// reader reaches them, and the live edge, through the "later" boundary.
test('a live run a jump left holding its head counts the members it gains until the reader returns to its tail', async ({ harness, page }) => {
  const LIST_ACTIVITY_RUN_MEMBERS = 1602023272;
  let memberReads = 0;
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const server = socket.connectToServer();
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.methodId === LIST_ACTIVITY_RUN_MEMBERS) memberReads += 1;
      server.send(message);
    });
    server.onMessage(message => socket.send(message));
  });
  const batch = (start: number, count: number) => Array.from({ length: count }, (_, j) => {
    const i = start + j;
    return [toolUseLine(`message-${i}`, `tool-${i}`, 'Bash', { command: `echo live-run-${i}` }), toolResultLine(`tool-${i}`, 'done')];
  }).flat();
  await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('live-run-jump', [
    emit([...textLines('lead', 'Starting a long sweep.'), ...batch(0, 60)]),
    { waitSignal: { name: 'grow' } }, emit(batch(60, 20)),
    { waitSignal: { name: 'edge' } }, emit(batch(80, 5)),
    { waitSignal: { name: 'hold' } },
  ]) });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: 'live-run-jump', repo: {}, threads: [
    { title: 'Live run', provider: 'claude', turns: [{ userText: 'Earlier', items: [{ kind: 'assistant_text', summary: 'Earlier answer.' }] }] },
    { title: 'Away', turns: [{ userText: 'Other', items: [{ kind: 'assistant_text', summary: 'Away answer.' }] }] },
  ] }] });
  const threadId = seed.projects[0].threadIds[0];
  await harness.open(page);
  const openThread = (title: string) => page.getByTestId('thread-row').getByText(title, { exact: true }).click();
  await openThread('Away');
  await expect(page.getByText('Away answer.', { exact: true })).toBeVisible();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'Start', null);
  await waitForGate(harness, 'grow');

  // Opened mid-run, the page ships the run's newest members; in-thread find
  // then jumps to its first, which re-centers the run on it.
  await openThread('Live run');
  const run = page.getByTestId('activity-run').last();
  await expect(run.getByTestId('activity-run-header-counts')).toContainText('60 Bash');
  await page.keyboard.press('ControlOrMeta+f');
  await expect(page.getByTestId('message-search-input')).toBeFocused();
  await page.keyboard.type('live-run-0');
  const hits = page.getByTestId('message-search-results').getByRole('button');
  await expect(hits).toHaveCount(1);
  await hits.click();
  if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
  const rows = run.getByTestId('command-output-row');
  const later = run.getByTestId('activity-run-later');
  // The run ends the pane, and the jump leaves the "Jump to bottom" chip
  // over the middle of its last row, so the reader clicks the boundary's
  // label at its left edge.
  const laterLabel = later.getByText(/\d+ later/);
  await expect(rows.first()).toContainText('echo live-run-0');
  await expect(rows).toHaveCount(WINDOW_ROWS);
  await expect(later).toContainText(`${60 - WINDOW_ROWS} later`);

  // The members the run gains are counted, not placed after the ones the
  // pane holds.
  await advance(harness, mockId, 'grow');
  await waitForGate(harness, 'edge');
  await expect(run.getByTestId('activity-run-header-counts')).toContainText('80 Bash');
  await expect(later).toContainText(`${80 - WINDOW_ROWS} later`);
  await expect(rows).toHaveCount(WINDOW_ROWS);
  await expect(rows.last()).toContainText(`echo live-run-${WINDOW_ROWS - 1}`);

  // The boundary reads the rest, and the run is whole again at its tail.
  await laterLabel.click();
  await expect(later).toContainText(`${80 - WINDOW_ROWS - CHUNK_ROWS} later`);
  await laterLabel.click();
  await expect(later).toHaveCount(0);
  await expect(rows.last()).toContainText('echo live-run-79');

  // The clicks mount the rows below the reader without moving them; the
  // pin releases when they scroll to the clip's bottom.
  const clip = run.getByTestId('activity-run-clip');
  await clip.hover();
  await page.mouse.wheel(0, 2000);
  await expect.poll(() => clip.evaluate(el => el.scrollHeight - el.clientHeight - el.scrollTop)).toBeLessThanOrEqual(1);

  // Back at the live edge, members append at the tail without a read.
  memberReads = 0;
  await advance(harness, mockId, 'edge');
  await waitForGate(harness, 'hold');
  await expect(run.getByTestId('activity-run-header-counts')).toContainText('85 Bash');
  await expect(rows.last()).toContainText('echo live-run-84');
  await expect(later).toHaveCount(0);
  expect(memberReads).toBe(0);
});

// A switch back into a live run reads the window before the members the run
// gains while that read is in flight, and the pane admits them at the tail
// of the run its cached window holds. A sync anchored inside the run ships
// the run centered on the anchor: the members gained are past the members
// the page did not ship, so they are counted after the page's span, not
// placed next to it.
test('a live run a switch-back sync ships mid-run counts the members it gained during the sync', async ({ harness, page }) => {
  const SYNC_THREAD_WINDOW = 3841902986;
  let threadId = '';
  let anchorItemId = '';
  let heldSyncId = '';
  let heldAnswer = '';
  let sawLastMember = false;
  let sendToPage: (text: string) => void = () => {};
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const server = socket.connectToServer();
    sendToPage = text => socket.send(text);
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (anchorItemId && frame.type === 'rpc' && frame.methodId === SYNC_THREAD_WINDOW
        && String(frame.params?.[0]) === threadId) {
        // The anchor a reader scrolled into the run leaves, and a page
        // rather than a fresh answer over the cached window.
        frame.params[1] = { ...frame.params[1], anchorItemId, haveEpoch: -1, haveRev: -1, haveWindow: null };
        heldSyncId = String(frame.id);
        anchorItemId = '';
        server.send(JSON.stringify(frame));
        return;
      }
      server.send(message);
    });
    server.onMessage(message => {
      const text = String(message);
      if (heldSyncId && text.startsWith('{"type":"rpc"') && String(JSON.parse(text).id) === heldSyncId) {
        heldAnswer = text;
        return;
      }
      if (text.includes('echo live-run-79')) sawLastMember = true;
      socket.send(message);
    });
  });
  const batch = (start: number, count: number) => Array.from({ length: count }, (_, j) => {
    const i = start + j;
    return [toolUseLine(`message-${i}`, `tool-${i}`, 'Bash', { command: `echo live-run-${i}` }), toolResultLine(`tool-${i}`, 'done')];
  }).flat();
  await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('live-run-sync', [
    emit([...textLines('lead', 'Starting a long sweep.'), ...batch(0, 60)]),
    { waitSignal: { name: 'grow' } }, emit(batch(60, 20)),
    { waitSignal: { name: 'hold' } },
  ]) });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: 'live-run-sync', repo: {}, threads: [
    { title: 'Live run', provider: 'claude', turns: [{ userText: 'Earlier', items: [{ kind: 'assistant_text', summary: 'Earlier answer.' }] }] },
    { title: 'Away', turns: [{ userText: 'Other', items: [{ kind: 'assistant_text', summary: 'Away answer.' }] }] },
  ] }] });
  threadId = seed.projects[0].threadIds[0];
  await harness.open(page);
  const openThread = (title: string) => page.getByTestId('thread-row').getByText(title, { exact: true }).click();
  await openThread('Away');
  await expect(page.getByText('Away answer.', { exact: true })).toBeVisible();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'Start', null);
  await waitForGate(harness, 'grow');

  // Opened mid-run, the page ships the run's newest members, and the pane
  // holds the run through its newest member.
  await openThread('Live run');
  const run = page.getByTestId('activity-run').last();
  await expect(run.getByTestId('activity-run-header-counts')).toContainText('60 Bash');
  const tail = await harness.rpc<{ items: Array<{ id: string; kind: string }> }>(
    'ListThreadSliceAround', threadId, '', TAIL_ITEM_BUDGET, {},
  );
  const member = tail.items.find(item => item.kind === 'tool_call' && /live-run-40\b/.test(JSON.stringify(item)));
  expect(member).toBeDefined();

  // Switch away and back. The sync is anchored on member 40 and read before
  // the run grows; its answer is held until the pane admitted the growth.
  await openThread('Away');
  await expect(page.getByText('Away answer.', { exact: true })).toBeVisible();
  anchorItemId = member!.id;
  await openThread('Live run');
  await expect.poll(() => heldAnswer).not.toBe('');
  await advance(harness, mockId, 'grow');
  await waitForGate(harness, 'hold');
  await expect.poll(() => sawLastMember).toBe(true);
  // The item stream flushes within a frame or 50ms of the last event.
  await page.waitForTimeout(300);
  heldSyncId = '';
  sendToPage(heldAnswer);

  // The page ships members 25..54. The members gained are counted after
  // them, with the five the page did not ship.
  if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
  const rows = run.getByTestId('command-output-row');
  const earlier = run.getByTestId('activity-run-earlier');
  const later = run.getByTestId('activity-run-later');
  await expect(run.getByTestId('activity-run-header-counts')).toContainText('80 Bash');
  await expect(later).toContainText('25 later');
  await expect(earlier).toContainText('25 earlier');
  await expect(rows).toHaveCount(WINDOW_ROWS);
  await expect(rows.last()).toContainText('echo live-run-54');

  // The boundary reads the rest of the run, the members the page did not
  // ship included.
  await later.click();
  await expect(later).toHaveCount(0);
  await expect(rows).toHaveCount(WINDOW_ROWS + CHUNK_ROWS);
  await expect(rows.last()).toContainText('echo live-run-79');
  await expect(page.getByText('Failed to refresh activity')).toHaveCount(0);
  await expect(page.getByText('Failed to load activity')).toHaveCount(0);
});
