// The Issues pane's Dashboards screen (docs/specs/issues.md, "Dashboards"),
// shared by the desktop and compact projects, through the shipped UI, the
// real backend and the fake Jira, whose five finished sprints, active
// sprint and histories the board syncs.
//
// Covered on both: the tab opens the board's built-in dashboard, counted
// from the board's copy once it has synced; the pace says why it is not
// compared when the copy holds too few earlier sprints, and with This
// sprint shows its change from the last sprint colored as bad news; a
// widget that counts now shows "Now" and no change mark; a bar opens its
// tickets, 100 a page, and the pages move; Team to Mine narrows every
// number to the person's tickets; a person in the table narrows the page
// with a chip that clears; a ticket's history reaches the count; a
// change made in Jira reaches the open dashboard with no Refresh; a
// chart's value shows on hover (on compact, on focus); the grid lays out
// again when the pane is resized; a listed ticket opens; a number that
// counted nothing is not known, with its tickets not counted, on the tile
// and in its drill, and a chart that draws none of them lists them in a
// "not counted" that opens the whole chart; a drill's rows carry a
// count's no value, the period a row counts in and its note beside the
// title (under it on compact); Unassigned's cell and No epic's bar open
// their tickets; while a range is counted the grid takes no click; the
// board's first read not finished is said on the screen and in a drill
// until its reconcile completes; on a kanban board with more epics and
// people than its bars and table show, More opens the rows of the epics
// and people left out, two epics tied across the cut. On desktop: four
// opens and closes of the screen leave the page's heap and DOM as they
// were.

import type { Locator, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  connectFakeJira, connectFakeJiraBoard, JIRA_SITE, openIssuesPane, press, renameJiraIssue, seedFakeJira, type ConnectedBoard, type ExtraJira,
} from './tracker-helpers.js';

export interface DashboardsSurface {
  compact: boolean;
}

const DAY = 24 * 3600_000;
const JIRA = { tracker: 'jira', site: JIRA_SITE };
/** The board's ticket listing, which the first read's walk reads with
 * the tickets' fields and its reconcile by ID alone (fields=id). */
const BOARD_ISSUES = 'GET rest/software/1.0/board/{board}/issue';
/** EvaluateTrackerDashboard's method ID on the wire. */
const EVALUATE_TRACKER_DASHBOARD = 2206254666;
const FIRST_READ = "This board's first read has not finished: these numbers count the tickets read so far.";
const iso = (offset: number) => new Date(Date.now() + offset).toISOString();
const POINTS = 'customfield_10016';

let changeId = 30000;
const status = (at: number, from: [string, string], to: [string, string]) => ({
  id: String((changeId += 1)), author: 'acc-user1', created: iso(at),
  items: [{ fieldId: 'status', field: 'status', fieldtype: 'jira', from: from[0], fromString: from[1], to: to[0], toString: to[1] }],
});
const TODO: [string, string] = ['10000', 'To Do'];
const DOING: [string, string] = ['10001', 'In Progress'];
const DONE: [string, string] = ['10002', 'Done'];

// Five finished sprints, A to E, two weeks each, E completed two days
// ago when Sprint 1 (the seed's) started. A to D each finished one ticket
// of 4 points, User 1's; E finished 105 of 1 point, the even ones User
// 1's (53) and the odd User 2's (52). So the pace over the last 5 is
// (16 + 105) / 5 = 24.2 points, Mine's (16 + 53) / 5 = 13.8 and User
// 2's 52 / 5 = 10.4. Sprint 1 holds the seed's WEB-1 and WEB-2 (no
// estimate, nobody's), WEB-20 (User 2, 3 points, in progress for 6
// days: stuck), WEB-21 (User 1, 2 points, done yesterday) and WEB-22
// (User 1, 5 points, to do): This sprint's pace is 2 points, down 103
// from Sprint E's 105.
const SPRINT_E = 15;
const DASHBOARDS: ExtraJira = {
  projects: [],
  boards: [],
  estimation: POINTS,
  assignable: ['acc-user1', 'acc-user2'],
  users: [{ accountId: 'acc-user2', displayName: 'User 2', email: 'user2@example.com', timeZone: 'UTC' }],
  fields: [
    { id: POINTS, name: 'Story Points', schema: { type: 'number', custom: 'com.atlassian.jira.plugin.system.customfieldtypes:float', customId: 10016 } },
  ],
  sprints: ['A', 'B', 'C', 'D', 'E'].map((name, i) => {
    const end = -(2 + 14 * (4 - i)) * DAY;
    return { id: 11 + i, name: `Sprint ${name}`, state: 'closed' as const, start: iso(end - 14 * DAY), end: iso(end), complete: iso(end) };
  }),
  issues: [
    ...[11, 12, 13, 14].map((sprint) => ({
      id: String(10400 + sprint), key: `WEB-${400 + sprint}`, project: 'WEB', type: '10002', status: '10002',
      summary: `Done in sprint ${sprint}`, sprints: [sprint], assignee: 'acc-user1', created: iso(-80 * DAY), values: { [POINTS]: 4 },
    })),
    ...Array.from({ length: 105 }, (_, i) => ({
      id: String(10200 + i), key: `WEB-${200 + i}`, project: 'WEB', type: '10002', status: '10002',
      summary: `Finished in Sprint E, ${i + 1}`, sprints: [SPRINT_E], assignee: i % 2 === 0 ? 'acc-user1' : 'acc-user2',
      created: iso(-15 * DAY), values: { [POINTS]: 1 },
    })),
    {
      id: '10120', key: 'WEB-20', project: 'WEB', type: '10002', status: '10001', summary: 'Stuck in progress', sprints: [1],
      assignee: 'acc-user2', created: iso(-10 * DAY), updated: iso(-6 * DAY), values: { [POINTS]: 3 }, changelog: [status(-6 * DAY, TODO, DOING)],
    },
    {
      id: '10121', key: 'WEB-21', project: 'WEB', type: '10002', status: '10002', summary: 'Done yesterday', sprints: [1],
      assignee: 'acc-user1', created: iso(-10 * DAY), updated: iso(-1 * DAY), values: { [POINTS]: 2 },
      changelog: [status(-5 * DAY, TODO, DOING), status(-1 * DAY, DOING, DONE)],
    },
    {
      id: '10122', key: 'WEB-22', project: 'WEB', type: '10002', status: '10000', summary: 'Waiting for User 1', sprints: [1],
      assignee: 'acc-user1', created: iso(-10 * DAY), values: { [POINTS]: 5 },
    },
  ],
};

// A kanban board (the weeks dashboard, counting tickets) with 13 epics
// of 9, 8, 7, 6, 5, 5, 4, 4, 3, 3, 2, 2 and 1 open children, and 52
// people beside User 1, User 10 to User 61, the 59 children assigned in
// turn: User 10 to User 16 carry 2, the rest 1. No epic holds the 13
// epics and WEB-1 to WEB-3 (16), as Unassigned does. The bars show 12 of
// 14 buckets: WEB-610 and WEB-611 both hold 2, and WEB-610 is shown by
// its label; More opens WEB-611's and WEB-612's 3 children, WEB-756 to
// WEB-758. The table shows 50 of 53 people: More opens User 59's, User
// 60's and User 61's, WEB-749 to WEB-751.
const KANBAN_BOARD = '2';
const EPIC_CHILDREN = [9, 8, 7, 6, 5, 5, 4, 4, 3, 3, 2, 2, 1];
const KANBAN: ExtraJira = {
  projects: [],
  boards: [{ id: 2, name: 'WEB kanban', type: 'kanban', project: 'WEB', columns: [
    { name: 'To Do', statuses: ['10000'] }, { name: 'In Progress', statuses: ['10001'] }, { name: 'Done', statuses: ['10002'] },
  ] }],
  users: Array.from({ length: 52 }, (_, i) => ({ accountId: `acc-p${10 + i}`, displayName: `User ${10 + i}`, timeZone: 'UTC' })),
  issues: [
    ...EPIC_CHILDREN.map((_, j) => ({ id: String(10600 + j), key: `WEB-${600 + j}`, project: 'WEB', type: '10000', status: '10000', summary: `Epic ${j + 1}` })),
    ...EPIC_CHILDREN.flatMap((n, j) => Array.from({ length: n }, () => j)).map((j, k) => ({
      id: String(10700 + k), key: `WEB-${700 + k}`, project: 'WEB', type: '10002', status: '10000', summary: `Child ${k + 1}`,
      parent: String(10600 + j), assignee: `acc-p${10 + (k % 52)}`,
    })),
  ],
};

function widget(screen: Locator, id: string): Locator {
  return screen.locator(`[data-testid="dashboard-widget"][data-widget="${id}"]`);
}

/** Switches the pane to Dashboards: its tab, or the folded picker. */
async function openDashboards(page: Page, pane: Locator, compact: boolean): Promise<Locator> {
  const picker = pane.getByTestId('issues-tabs-picker');
  if (await picker.isVisible()) {
    await press(picker, compact);
    await press(page.getByTestId('issues-tab-option').filter({ hasText: 'Dashboards' }), compact);
  } else {
    await press(pane.locator('[data-testid="issues-tab"][data-screen="dashboards"]'), compact);
  }
  const screen = pane.getByTestId('dashboard-screen');
  await expect(screen).toBeVisible({ timeout: 15_000 });
  return screen;
}

async function pickRange(page: Page, screen: Locator, id: string, compact: boolean): Promise<void> {
  await press(screen.getByTestId('dashboard-range'), compact);
  await press(page.getByTestId(`dashboard-range-${id}`), compact);
  await expect(screen.getByTestId('dashboard-counting')).toHaveCount(0, { timeout: 15_000 });
}

async function spans(screen: Locator): Promise<Record<string, number>> {
  return screen.getByTestId('dashboard-cell-span').evaluateAll((els) => Object.fromEntries(els.map((el) => [
    el.querySelector<HTMLElement>('[data-testid="dashboard-widget"]')?.dataset.widget ?? '',
    Number((el as HTMLElement).dataset.span),
  ])));
}

async function openBoardDashboards(harness: HarnessApp, page: Page, name: string, compact: boolean): Promise<{ board: ConnectedBoard; pane: Locator; screen: Locator }> {
  const board = await connectFakeJiraBoard(harness, name, DASHBOARDS);
  await harness.open(page);
  const pane = await openIssuesPane(page, compact);
  await expect(pane.locator('[data-testid="issues-card"][data-issue="10101"]')).toBeVisible({ timeout: 30_000 });
  const screen = await openDashboards(page, pane, compact);
  // The board's copy has synced its sprints and histories: the pace is counted over all five sprints.
  await expect(widget(screen, 'pace').getByTestId('dashboard-number-value')).toHaveText('24.2', { timeout: 60_000 });
  await expect(widget(screen, 'stuck').getByTestId('dashboard-number-value')).toHaveText('1', { timeout: 60_000 });
  return { board, pane, screen };
}

export function registerDashboardsFlow({ compact }: DashboardsSurface): void {
  const name = (s: string) => `${compact ? 'compact-' : ''}issues-dashboards-${s}`;

  test('Dashboards counts the board, compares a range with the one before, and narrows to a person', async ({ harness, page }) => {
    test.setTimeout(180_000);
    const { board, pane, screen } = await openBoardDashboards(harness, page, name('count'), compact);
    await expect(screen.getByTestId('dashboard-title')).toHaveText('Overview');
    await expect(screen.getByTestId('dashboard-range')).toHaveText('Last 5 sprints');
    await expect(screen.getByTestId('dashboard-periods')).toHaveText('Sprint A to Sprint E');

    // The pace has no five sprints before these to compare with, and says so.
    const pace = widget(screen, 'pace');
    await expect(pace.getByTestId('dashboard-change-gap')).toHaveText('Not compared: This computer keeps fewer than 5 finished sprints before these to compare with.');
    await expect(pace.getByTestId('dashboard-change')).toHaveCount(0);
    // A number that counts now has no change mark.
    const stuck = widget(screen, 'stuck');
    await expect(stuck.getByTestId('dashboard-now')).toBeVisible();
    await expect(stuck.getByTestId('dashboard-change')).toHaveCount(0);
    await expect(stuck.getByTestId('dashboard-change-gap')).toHaveCount(0);
    await expect(widget(screen, 'stuck_list').getByTestId('dashboard-list-row')).toHaveText([/WEB-20\s*Stuck in progress/]);
    await expect(widget(screen, 'finished').getByTestId('dashboard-bar')).toHaveCount(5);

    // This sprint against the last: down 103 points, bad news for a pace.
    await pickRange(page, screen, 'this_sprint', compact);
    await expect(pace.getByTestId('dashboard-number-value')).toHaveText('2');
    // WEB-21's history reached the copy: its start to done is counted.
    await expect(widget(screen, 'start_to_done').getByTestId('dashboard-number-value')).not.toHaveText('None', { timeout: 30_000 });
    await expect(pace.getByTestId('dashboard-change')).toHaveText('Down 103 points from 105, the last sprint');
    await expect(pace.getByTestId('dashboard-change')).toHaveAttribute('data-tone', 'bad');
    await pickRange(page, screen, 'sprints_5', compact);
    await expect(pace.getByTestId('dashboard-number-value')).toHaveText('24.2');

    // Team to Mine: every number is User 1's.
    const whose = screen.getByRole('radiogroup', { name: 'Whose work' });
    await press(whose.getByRole('radio', { name: 'Mine' }), compact);
    await expect(pace.getByTestId('dashboard-number-value')).toHaveText('13.8', { timeout: 15_000 });
    await expect(stuck.getByTestId('dashboard-number-value')).toHaveText('0');
    const carrying = widget(screen, 'carrying');
    await expect(carrying.getByTestId('dashboard-table-label')).toHaveText(['User 1']);
    await press(whose.getByRole('radio', { name: 'Team' }), compact);
    await expect(pace.getByTestId('dashboard-number-value')).toHaveText('24.2', { timeout: 15_000 });
    await expect(carrying.getByTestId('dashboard-table-label')).toHaveText(['User 1', 'User 2']);

    // A person in the table narrows the page, and the chip clears it.
    await press(carrying.getByTestId('dashboard-table-label').filter({ hasText: 'User 2' }), compact);
    const chip = screen.getByTestId('dashboard-chip');
    await expect(chip).toHaveText('User 2');
    await expect(pace.getByTestId('dashboard-number-value')).toHaveText('10.4', { timeout: 15_000 });
    await expect(stuck.getByTestId('dashboard-number-value')).toHaveText('1');
    await press(chip.getByTestId('dashboard-chip-clear'), compact);
    await expect(chip).toHaveCount(0);
    await expect(pace.getByTestId('dashboard-number-value')).toHaveText('24.2', { timeout: 15_000 });

    // A change in Jira reaches the open dashboard with no Refresh: the
    // board keeps syncing under it, and its change counts again.
    await renameJiraIssue(board, 'WEB-20', 'Stuck in progress, renamed in Jira');
    await expect(widget(screen, 'stuck_list').getByTestId('dashboard-list-row')).toHaveText([/WEB-20\s*Stuck in progress, renamed in Jira/], { timeout: 30_000 });

    // A listed ticket opens.
    await press(widget(screen, 'stuck_list').getByTestId('dashboard-list-row'), compact);
    await expect(pane.getByTestId('ticket-screen')).toHaveAttribute('data-issue', '10120', { timeout: 15_000 });
  });

  // The heap is Chromium's, read the same on either layout: desktop only.
  if (!compact) {
    test('Dashboards gives back its heap and DOM when the screen closes', async ({ harness, page, context }) => {
      test.setTimeout(180_000);
      const { pane } = await openBoardDashboards(harness, page, name('memory'), compact);
      const cdp = await context.newCDPSession(page);
      // What the page holds: the heap snapshot's objects, less the code V8
      // compiles as functions grow hot, and the pane strip's DOM.
      const measure = async () => {
        await cdp.send('HeapProfiler.collectGarbage');
        const chunks: string[] = [];
        const onChunk = (e: { chunk: string }) => { chunks.push(e.chunk); };
        cdp.on('HeapProfiler.addHeapSnapshotChunk', onChunk);
        await cdp.send('HeapProfiler.takeHeapSnapshot', { reportProgress: false });
        cdp.off('HeapProfiler.addHeapSnapshotChunk', onChunk);
        const snap = JSON.parse(chunks.join('')) as { snapshot: { meta: { node_fields: string[]; node_types: [string[]] } }; nodes: number[] };
        const fields = snap.snapshot.meta.node_fields;
        const typeAt = fields.indexOf('type');
        const sizeAt = fields.indexOf('self_size');
        const code = snap.snapshot.meta.node_types[0].indexOf('code');
        let held = 0;
        for (let i = 0; i < snap.nodes.length; i += fields.length) if (snap.nodes[i + typeAt] !== code) held += snap.nodes[i + sizeAt];
        const nodes = await page.evaluate(() => document.querySelector('[data-testid="pane-strip"]')!.getElementsByTagName('*').length);
        return { heldKB: Math.round(held / 1024), nodes };
      };
      const toBoard = async () => {
        await pane.locator('[data-testid="issues-tab"][data-screen="board"]').click();
        await expect(pane.getByTestId('dashboard-screen')).toHaveCount(0);
        await expect(pane.locator('[data-testid="issues-card"][data-issue="10101"]')).toBeVisible();
      };
      const openAndClose = async () => {
        const screen = await openDashboards(page, pane, false);
        await expect(widget(screen, 'pace').getByTestId('dashboard-number-value')).toHaveText('24.2', { timeout: 15_000 });
        await expect(screen.getByTestId('dashboard-counting')).toHaveCount(0);
        const open = await measure();
        await toBoard();
        return open;
      };
      // The screen's code loads and each widget draws once; then four
      // opens and closes.
      await toBoard();
      await openAndClose();
      const before = await measure();
      const opens = [];
      for (let i = 0; i < 4; i += 1) opens.push(await openAndClose());
      const after = await measure();
      const cost = `before ${JSON.stringify(before)}; each open ${JSON.stringify(opens)}; after four closed ${JSON.stringify(after)}`;
      test.info().annotations.push({ type: 'dashboards screen cost', description: cost });
      console.log(`dashboards screen cost: ${cost}`);
      expect(after.nodes).toBe(before.nodes);
      for (const open of opens) expect(open.nodes).toBeGreaterThan(before.nodes);
      // Four screens come and go and the page holds what it held before,
      // give or take less than half of what one open screen holds.
      const oneScreen = Math.min(...opens.map((open) => open.heldKB - before.heldKB));
      expect(after.heldKB - before.heldKB).toBeLessThan(oneScreen / 2);
      await cdp.detach();
    });
  }

  test('Dashboards opens a bar\'s tickets by the page, shows a chart\'s values, and lays out again with the pane', async ({ harness, page }) => {
    test.setTimeout(180_000);
    const { screen } = await openBoardDashboards(harness, page, name('drill'), compact);
    const finished = widget(screen, 'finished');
    const sprintE = finished.locator(`[data-testid="dashboard-bar"][data-bucket="sprint:${SPRINT_E}"]`);

    // A bar's value on hover, or on focus where there is no hover.
    if (compact) await sprintE.focus();
    else await sprintE.hover();
    await expect(finished.getByTestId('dashboard-hover')).toHaveText('Sprint E, 105 points');

    // The bar's tickets, 100 a page.
    await press(sprintE, compact);
    const drill = page.getByTestId('dashboard-drill');
    await expect(drill).toBeVisible();
    await expect(page.getByRole('dialog', { name: 'Finished per sprint, Sprint E' })).toBeVisible();
    await expect(drill.getByTestId('dashboard-drill-value')).toHaveText('105 points');
    await expect(drill.getByTestId('dashboard-drill-count')).toHaveText('105 tickets');
    await expect(drill.getByTestId('dashboard-drill-row')).toHaveCount(100);
    await expect(drill.getByTestId('dashboard-drill-page')).toHaveText('Page 1 of 2');
    await press(drill.getByTestId('dashboard-drill-next'), compact);
    await expect(drill.getByTestId('dashboard-drill-page')).toHaveText('Page 2 of 2');
    await expect(drill.getByTestId('dashboard-drill-row')).toHaveCount(5);
    await press(drill.getByTestId('dashboard-drill-prev'), compact);
    await expect(drill.getByTestId('dashboard-drill-row')).toHaveCount(100);
    await page.keyboard.press('Escape');
    await expect(drill).toHaveCount(0);

    // The grid lays out to the pane's width.
    const viewport = page.viewportSize()!;
    if (compact) {
      expect(await spans(screen)).toMatchObject({ pace: 6, stuck: 6, finished: 12, carrying: 12, stuck_list: 12 });
      const before = await finished.locator('svg').first().getAttribute('width');
      await page.setViewportSize({ width: viewport.width + 60, height: viewport.height });
      await expect.poll(async () => finished.locator('svg').first().getAttribute('width')).not.toBe(before);
    } else {
      await page.setViewportSize({ width: 1800, height: 1000 });
      await expect.poll(async () => (await spans(screen)).pace).toBe(4);
      expect(await spans(screen)).toMatchObject({ finished: 8, carrying: 4, stuck_list: 6, open_by_epic: 6 });
      await page.setViewportSize({ width: 1000, height: 1000 });
      await expect.poll(async () => (await spans(screen)).pace).not.toBe(4);
      const narrower = await spans(screen);
      expect(narrower.finished).toBe(12);
      expect([6, 8, 12]).toContain(narrower.pace);
    }
  });

  test('Dashboards lists what it did not count, opens the rows of Unassigned and No epic, and takes no click while it counts', async ({ harness, page }) => {
    test.setTimeout(180_000);
    // Every count of This sprint waits while `holding`, as on a slow computer.
    let holding = false;
    const held: Array<() => void> = [];
    await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
      const server = socket.connectToServer();
      socket.onMessage((message) => {
        const frame = JSON.parse(String(message));
        if (holding && frame.type === 'rpc' && frame.methodId === EVALUATE_TRACKER_DASHBOARD && frame.params?.[1]?.range === 'this_sprint') {
          held.push(() => server.send(message));
          return;
        }
        server.send(message);
      });
      server.onMessage((message) => socket.send(message));
    });
    const { screen } = await openBoardDashboards(harness, page, name('not-counted'), compact);
    const drill = page.getByTestId('dashboard-drill');
    const closeDrill = async () => {
      await page.keyboard.press('Escape');
      await expect(drill).toHaveCount(0);
    };

    // No ticket the range finished shows when it finished, so start to
    // done is not known, in the same words on the tile and in its drill.
    const startToDone = widget(screen, 'start_to_done');
    await expect(startToDone.getByTestId('dashboard-number-value')).toHaveText('Not known');
    await expect(startToDone.getByTestId('dashboard-number-not-counted')).toHaveText('105 not counted');
    await expect(startToDone.getByTestId('dashboard-change')).toHaveCount(0);
    await press(startToDone.getByTestId('dashboard-number'), compact);
    await expect(drill.getByTestId('dashboard-drill-value')).toHaveText('Not known');
    await expect(drill.getByTestId('dashboard-drill-value-not-counted')).toHaveText('105 not counted');
    await expect(drill.getByTestId('dashboard-drill-not-counted').first()).toHaveText('Not counted: when it finished is not known');
    await closeDrill();
    // The line draws none of them; its "not counted" opens the whole line.
    const line = widget(screen, 'start_to_done_line');
    await expect(line.getByTestId('dashboard-not-counted')).toHaveText('105 not counted');
    await press(line.getByTestId('dashboard-not-counted'), compact);
    await expect(page.getByRole('dialog', { name: 'Days from start to done, by sprint, last 5 sprints' })).toBeVisible();
    await expect(drill.getByTestId('dashboard-drill-value-not-counted')).toHaveText('105 not counted');
    await closeDrill();

    // A count's rows carry no value; the note sits beside the title, and
    // under it on compact. WEB-20's history has reached the copy once the
    // list says when it moved to In Progress.
    const STUCK_NOTE = 'in In Progress for 6 days';
    await expect(widget(screen, 'stuck_list').getByTestId('dashboard-list-note')).toHaveText(STUCK_NOTE, { timeout: 30_000 });
    await press(widget(screen, 'stuck').getByTestId('dashboard-number'), compact);
    const stuckRow = drill.getByTestId('dashboard-drill-row');
    await expect(stuckRow).toHaveCount(1);
    await expect(stuckRow).toContainText('WEB-20');
    await expect(stuckRow.getByTestId('dashboard-drill-note')).toHaveText(STUCK_NOTE);
    await expect(drill.getByTestId('dashboard-drill-row-value')).toHaveCount(0);
    const title = (await stuckRow.getByTestId('dashboard-drill-title').boundingBox())!;
    const note = (await stuckRow.getByTestId('dashboard-drill-note').boundingBox())!;
    if (compact) expect(note.y).toBeGreaterThanOrEqual(title.y + title.height - 1);
    else expect(note.y).toBeLessThan(title.y + title.height);
    await closeDrill();

    // The pace's rows each name the sprint they count in, with their points.
    await press(widget(screen, 'pace').getByTestId('dashboard-number'), compact);
    await expect(drill.getByTestId('dashboard-drill-count')).toHaveText('109 tickets');
    await expect(drill.getByTestId('dashboard-drill-period')).toHaveCount(100);
    await expect(drill.getByTestId('dashboard-drill-row').filter({ hasText: 'WEB-411' }).getByTestId('dashboard-drill-period')).toHaveText('Sprint A');
    await expect(drill.getByTestId('dashboard-drill-row').filter({ hasText: 'WEB-411' }).getByTestId('dashboard-drill-row-value')).toHaveText('4 points');
    await closeDrill();

    // Unassigned's Doing cell lists the unassigned ticket in progress.
    const unassigned = widget(screen, 'carrying').locator('[data-testid="dashboard-table-row"][data-bucket=""]');
    await press(unassigned.getByTestId('dashboard-cell').nth(1), compact);
    await expect(drill.getByTestId('dashboard-drill-row')).toHaveText([/WEB-1\s*First story/]);
    await closeDrill();

    // No epic's bar lists the open tickets in no epic: on a scrum board
    // every one, since the board's copy keeps no epic.
    const noEpic = widget(screen, 'open_by_epic').locator('[data-testid="dashboard-bar"][data-bucket=""]');
    await press(noEpic, compact);
    await expect(drill.getByTestId('dashboard-drill-row').first()).toBeVisible();
    const noEpicKeys = await drill.getByTestId('dashboard-drill-row').locator('span.font-mono').allTextContents();
    expect(noEpicKeys.sort()).toEqual(['WEB-1', 'WEB-2', 'WEB-20', 'WEB-22']);
    await closeDrill();

    // While This sprint is counted, the grid shows the last 5 sprints'
    // numbers and takes no click: a click on Sprint E's bar opens nothing.
    holding = true;
    await press(screen.getByTestId('dashboard-range'), compact);
    await press(page.getByTestId('dashboard-range-this_sprint'), compact);
    await expect.poll(() => held.length).toBe(1);
    await expect(screen.getByTestId('dashboard-counting')).toBeVisible();
    await expect(screen.getByTestId('dashboard-grid')).toHaveAttribute('inert', '');
    const bar = (await widget(screen, 'finished').locator(`[data-testid="dashboard-bar"][data-bucket="sprint:${SPRINT_E}"]`).boundingBox())!;
    if (compact) await page.touchscreen.tap(bar.x + bar.width / 2, bar.y + bar.height / 2);
    else await page.mouse.click(bar.x + bar.width / 2, bar.y + bar.height / 2);
    // A drill opens on the click, before its rows are read: once This
    // sprint is counted, none has.
    holding = false;
    for (const send of held.splice(0)) send();
    await expect(screen.getByTestId('dashboard-counting')).toHaveCount(0, { timeout: 15_000 });
    await expect(widget(screen, 'pace').getByTestId('dashboard-number-value')).toHaveText('2');
    await expect(drill).toHaveCount(0);
    await expect(screen.getByTestId('dashboard-grid')).not.toHaveAttribute('inert', '');
  });

  test('Dashboards opens the epics and people a chart and a table left out from their More', async ({ harness, page }) => {
    test.setTimeout(180_000);
    await seedFakeJira(harness, KANBAN);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: name('more'), repo: {}, threads: [] }] });
    await connectFakeJira(harness, seed.projects[0].projectId, KANBAN_BOARD);
    await harness.open(page);
    const pane = await openIssuesPane(page, compact);
    const screen = await openDashboards(page, pane, compact);
    const drill = page.getByTestId('dashboard-drill');
    const keys = async () => {
      await expect(drill.getByTestId('dashboard-drill-row').first()).toBeVisible();
      return (await drill.getByTestId('dashboard-drill-row').locator('span.font-mono').allTextContents()).sort();
    };

    const epics = widget(screen, 'open_by_epic');
    await expect(epics.getByTestId('dashboard-bar')).toHaveCount(12, { timeout: 60_000 });
    await expect(epics.locator('[data-testid="dashboard-bar"][data-bucket="10610"]')).toHaveCount(1);
    await expect(epics.locator('[data-testid="dashboard-bar"][data-bucket="10611"]')).toHaveCount(0);
    await press(epics.getByTestId('dashboard-more'), compact);
    await expect(page.getByRole('dialog', { name: 'Open work by epic, 2 more epics' })).toBeVisible();
    await expect(drill.getByTestId('dashboard-drill-count')).toHaveText('3 tickets');
    expect(await keys()).toEqual(['WEB-756', 'WEB-757', 'WEB-758']);
    await page.keyboard.press('Escape');
    await expect(drill).toHaveCount(0);

    const carrying = widget(screen, 'carrying');
    await expect(carrying.getByTestId('dashboard-table-row')).toHaveCount(50);
    await expect(carrying.getByTestId('dashboard-more')).toHaveText('3 more people');
    await press(carrying.getByTestId('dashboard-more'), compact);
    await expect(page.getByRole('dialog', { name: 'Who is carrying what, 3 more people' })).toBeVisible();
    expect(await keys()).toEqual(['WEB-749', 'WEB-750', 'WEB-751']);
  });

  test('Dashboards says the board\'s first read has not finished, on the screen and in a drill, until it has', async ({ harness, page }) => {
    test.setTimeout(180_000);
    await seedFakeJira(harness, DASHBOARDS);
    // The first read's reconcile, which lists the board's tickets by ID
    // alone, fails, and its retry waits; the walk, which lists them with
    // their fields, reads. The board shows once its first sync ends.
    const reconcile = { origin: JIRA, route: BOARD_ISSUES, query: { fields: 'id' } };
    await harness.rpc('HarnessTrackerFault', { ...reconcile, kind: 'refuse', times: 1, status: 500, body: { errorMessages: ['Internal server error'], errors: {} } });
    await harness.rpc('HarnessTrackerFault', { ...reconcile, kind: 'hold' });
    const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: name('first-read'), repo: {}, threads: [] }] });
    await connectFakeJira(harness, seed.projects[0].projectId);
    await harness.open(page);
    const pane = await openIssuesPane(page, compact);
    const screen = await openDashboards(page, pane, compact);
    const reading = screen.getByTestId('dashboard-reading');
    await expect(reading).toHaveText(FIRST_READ, { timeout: 30_000 });
    await press(widget(screen, 'pace').getByTestId('dashboard-number'), compact);
    const drill = page.getByTestId('dashboard-drill');
    await expect(drill.getByTestId('dashboard-drill-reading')).toHaveText(FIRST_READ);
    await page.keyboard.press('Escape');
    await expect(drill).toHaveCount(0);

    await harness.rpc('HarnessTrackerRelease', { origin: JIRA, route: BOARD_ISSUES });
    await expect(widget(screen, 'pace').getByTestId('dashboard-number-value')).toHaveText('24.2', { timeout: 60_000 });
    await expect(reading).toHaveCount(0, { timeout: 30_000 });
  });
}
